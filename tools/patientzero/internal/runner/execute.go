package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"atrium/tools/patientzero/internal/journal"
	"atrium/tools/patientzero/internal/scenario"
	"atrium/tools/patientzero/internal/signal"
	"atrium/tools/patientzero/internal/traffic"
)

// The journal events' data. The field names are the journal's contract
// (scenarios/README.md, "Run journal"); renaming or removing one bumps
// journal.Version.

type runStartData struct {
	Scenario    string             `json:"scenario"`
	Description string             `json:"description"`
	Digest      string             `json:"scenario_sha256"`
	Definition  *scenario.Scenario `json:"definition"`
	Load        string             `json:"load"`
	Host        string             `json:"host"`
	Sources     map[string]string  `json:"sources"`
	Interval    string             `json:"summary_interval"`
	Poll        string             `json:"poll_interval"`
	PlannedEnd  time.Time          `json:"planned_end"`
}

type faultData struct {
	Step      string         `json:"step"`
	StepIndex int            `json:"step_index"`
	Type      string         `json:"type"`
	Scope     string         `json:"scope"`
	Params    map[string]any `json:"params"`
	// fault_end only.
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
}

type trafficData struct {
	Scope string `json:"scope"` // interval or run
	traffic.Summary
}

type checkData struct {
	Signal      string            `json:"signal"`
	Kind        string            `json:"kind"`
	Query       string            `json:"query"`
	Condition   string            `json:"condition,omitempty"`
	State       string            `json:"state,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Step        string            `json:"step"`
	From        string            `json:"from"`
	Mode        string            `json:"mode"`
	Within      string            `json:"within"`
	WindowStart time.Time         `json:"window_start"`
	WindowEnd   time.Time         `json:"window_end"`
	Result      string            `json:"result"`
	EvaluatedAt *time.Time        `json:"evaluated_at,omitempty"`
	Attempts    int               `json:"attempts"`
	Values      []float64         `json:"values"`
	Series      int               `json:"series"`
	Error       string            `json:"error,omitempty"`
	// Delivery checks only: the rows' window and each log type's counts.
	RowsFrom *time.Time             `json:"rows_from,omitempty"`
	RowsTo   *time.Time             `json:"rows_to,omitempty"`
	Delivery []signal.DeliveryCount `json:"delivery,omitempty"`
}

type runEndData struct {
	Scenario string         `json:"scenario"`
	State    string         `json:"state"`
	Error    string         `json:"error,omitempty"`
	Seconds  float64        `json:"seconds"`
	Checks   map[string]int `json:"checks"`
	Requests int64          `json:"requests"`
	Failed   int64          `json:"failed"`
	Injected int64          `json:"injected"`
	SlowSQL  int64          `json:"slow_sql"`
	Shed     int64          `json:"shed"`
	Users    int            `json:"users"`
	Sessions int            `json:"sessions"`
}

// digest identifies the scenario definition a run used.
func digest(s *scenario.Scenario) string {
	b, _ := json.Marshal(s)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// execute runs u to its end. ctx ends when the run is stopped or the
// process shuts down.
func (r *Runner) execute(ctx context.Context, u *run, j *journal.Journal, faults []scenario.Fault, start time.Time) {
	s := u.scenario
	log := r.cfg.Log.With("run", u.id, "scenario", s.Name)
	var jmu sync.Mutex
	var jerr error
	write := func(at time.Time, event string, data any) {
		if err := j.Write(at, event, data); err != nil {
			jmu.Lock()
			if jerr == nil {
				jerr = err
				log.Error("writing the run journal failed", "err", err)
			}
			jmu.Unlock()
		}
	}

	r.cfg.Traffic.Begin(start)
	write(start, journal.RunStart, runStartData{
		Scenario: s.Name, Description: s.Description, Digest: digest(s), Definition: s, Load: s.Load,
		Host: r.cfg.Host,
		Sources: map[string]string{
			"prometheus":   r.cfg.Sources.Prometheus,
			"loki":         r.cfg.Sources.Loki,
			"alertmanager": r.cfg.Sources.Alertmanager,
		},
		Interval:   r.cfg.SummaryInterval.String(),
		Poll:       r.cfg.PollInterval.String(),
		PlannedEnd: start.Add(s.MaxDuration()).UTC(),
	})
	log.Info("scenario started")

	// Traffic summaries, every interval until the run ends.
	summaries := make(chan struct{})
	summariesDone := make(chan struct{})
	go func() {
		defer close(summariesDone)
		t := time.NewTicker(r.cfg.SummaryInterval)
		defer t.Stop()
		for {
			select {
			case <-summaries:
				return
			case <-t.C:
				now := r.cfg.Now()
				write(now, journal.Traffic, trafficData{Scope: "interval", Summary: r.cfg.Traffic.Flush(now)})
			}
		}
	}()

	// Each step's start (its fault on) and end (its fault off), as they
	// happen: a window opens at its planned time, but never before the
	// fault it watches has actually started or stopped.
	began := make([]chan struct{}, len(s.Steps))
	ended := make([]chan struct{}, len(s.Steps))
	// When each step began and ended, written before its channel closes:
	// a delivery check's rows' window, read once its own window has opened.
	starts := make([]time.Time, len(s.Steps))
	ends := make([]time.Time, len(s.Steps))
	stepIndex := map[string]int{}
	for i, st := range s.Steps {
		began[i], ended[i] = make(chan struct{}), make(chan struct{})
		stepIndex[st.Name] = i
	}

	// Checks, each in its own window.
	var checks sync.WaitGroup
	results := make([]string, len(s.Signals))
	for i, sig := range s.Signals {
		checks.Add(1)
		go func() {
			defer checks.Done()
			opened := began[stepIndex[sig.Step]]
			if sig.From == scenario.FromEnd {
				opened = ended[stepIndex[sig.Step]]
			}
			var rows func() signal.Window
			if sig.Kind() == scenario.KindDelivery {
				from, to := stepIndex[sig.Delivery.FromStep], stepIndex[sig.Delivery.ToStep]
				rows = func() signal.Window { return signal.Window{From: starts[from], To: ends[to]} }
			}
			d := r.check(ctx, u, i, sig, start.Add(s.Anchor(sig)), opened, rows)
			results[i] = d.Result
			write(r.cfg.Now(), journal.Check, d)
		}()
	}

	// Steps, one after another.
	runErr := ""
	stepStart := start
	for i, st := range s.Steps {
		if ctx.Err() != nil {
			break
		}
		u.update(func(s *Status) { s.Step = st.Name })
		starts[i] = r.cfg.Now()
		end := stepStart.Add(st.Duration.D())
		if f := faults[i]; f != nil {
			fd := faultData{Step: st.Name, StepIndex: i, Type: st.Fault.Type, Params: st.Fault.Params}
			if t, ok := r.cfg.Registry.Type(st.Fault.Type); ok {
				fd.Scope = string(t.Scope)
			}
			if err := f.Start(ctx); err != nil {
				fd.Reason, fd.Error = "error", err.Error()
				write(r.cfg.Now(), journal.FaultEnd, fd)
				runErr = fmt.Sprintf("starting the %s fault of step %s: %v", st.Fault.Type, st.Name, err)
				log.Error("fault failed to start", "step", st.Name, "err", err)
				u.cancel()
				break
			}
			write(r.cfg.Now(), journal.FaultStart, fd)
			close(began[i])
			log.Info("fault started", "step", st.Name, "type", st.Fault.Type)
			stopped := sleepUntil(ctx, r.cfg.Now, end)
			stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), faultStopTimeout)
			err := f.Stop(stopCtx)
			cancel()
			fd.Reason = "completed"
			if stopped {
				fd.Reason = "stopped"
			}
			if err != nil {
				fd.Reason, fd.Error = "error", err.Error()
				runErr = fmt.Sprintf("stopping the %s fault of step %s: %v", st.Fault.Type, st.Name, err)
				log.Error("fault failed to stop", "step", st.Name, "err", err)
			}
			write(r.cfg.Now(), journal.FaultEnd, fd)
			log.Info("fault ended", "step", st.Name, "type", st.Fault.Type, "reason", fd.Reason)
			if err != nil {
				u.cancel()
				break
			}
			ends[i] = r.cfg.Now()
			close(ended[i])
		} else {
			close(began[i])
			sleepUntil(ctx, r.cfg.Now, end)
			ends[i] = r.cfg.Now()
			close(ended[i])
		}
		stepStart = end
	}
	u.update(func(s *Status) { s.Step = "" })

	checks.Wait()
	close(summaries)
	<-summariesDone

	end := r.cfg.Now()
	last, total := r.cfg.Traffic.End(end)
	write(end, journal.Traffic, trafficData{Scope: "interval", Summary: last})
	write(end, journal.Traffic, trafficData{Scope: "run", Summary: total})

	counts := map[string]int{ResultPass: 0, ResultFail: 0, ResultSkipped: 0}
	for _, res := range results {
		counts[res]++
	}
	// A stopped run is stopped, even when stopping made its fault fail to
	// start or stop (a cluster fault whose agent detached); the error is
	// kept.
	state := StatePassed
	switch {
	case r.isStopped(u) || r.base.Err() != nil:
		state = StateStopped
	case runErr != "":
		state = StateError
	case counts[ResultFail] > 0:
		state = StateFailed
	}
	jmu.Lock()
	firstJournalErr := jerr
	jmu.Unlock()
	if firstJournalErr != nil && runErr == "" {
		state, runErr = StateError, firstJournalErr.Error()
	}
	write(end, journal.RunEnd, runEndData{
		Scenario: s.Name, State: state, Error: runErr, Seconds: end.Sub(start).Seconds(), Checks: counts,
		Requests: total.Requests, Failed: total.Failed, Injected: total.Injected, SlowSQL: total.SlowSQL,
		Shed: total.Shed, Users: total.Users, Sessions: total.Sessions,
	})
	if err := j.Close(); err != nil && runErr == "" {
		state, runErr = StateError, err.Error()
	}
	u.update(func(st *Status) {
		st.State, st.Error = state, runErr
		e := end.UTC()
		st.Ended = &e
	})
	log.Info("scenario ended", "state", state, "checks_passed", counts[ResultPass], "checks_failed", counts[ResultFail])
}

// sleepUntil waits until t or until ctx ends; it reports whether ctx ended.
func sleepUntil(ctx context.Context, now func() time.Time, t time.Time) bool {
	d := t.Sub(now())
	if d <= 0 {
		return ctx.Err() != nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return false
	case <-ctx.Done():
		return true
	}
}

// check evaluates one signal over its window, every poll interval and once
// more when the window closes. The window opens at anchor, or when opened is
// closed if that is later (the step's fault actually on or off), and stays
// open for sig.Within.
func (r *Runner) check(ctx context.Context, u *run, i int, sig scenario.Signal, anchor time.Time, opened <-chan struct{}, rows func() signal.Window) checkData {
	d := checkData{
		Signal: sig.Name, Kind: sig.Kind(), Query: sig.Query(), Condition: sig.Condition, State: sig.State,
		Labels: sig.Labels, Step: sig.Step, From: sig.From, Mode: sig.Mode, Within: sig.Within.String(),
		WindowStart: anchor.UTC(), WindowEnd: anchor.Add(sig.Within.D()).UTC(), Result: ResultSkipped, Values: []float64{},
	}
	defer func() {
		u.update(func(s *Status) {
			s.Checks[i] = CheckStatus{Signal: sig.Name, Result: d.Result, Attempts: d.Attempts, Values: d.Values, Error: d.Error}
		})
	}()
	if sleepUntil(ctx, r.cfg.Now, anchor) {
		return d
	}
	select {
	case <-opened:
	case <-ctx.Done():
		return d
	}
	if now := r.cfg.Now(); now.After(anchor) {
		anchor = now
	}
	deadline := anchor.Add(sig.Within.D())
	d.WindowStart, d.WindowEnd = anchor.UTC(), deadline.UTC()
	var window signal.Window
	if rows != nil {
		window = rows()
		from, to := window.From.UTC(), window.To.UTC()
		d.RowsFrom, d.RowsTo = &from, &to
	}
	for {
		at := r.cfg.Now()
		evalCtx, cancel := context.WithTimeout(ctx, evalTimeout)
		var res signal.Result
		var err error
		if rows != nil {
			window.At = at
			res, err = r.cfg.Sources.EvaluateDelivery(evalCtx, sig, window)
		} else {
			res, err = r.cfg.Sources.Evaluate(evalCtx, sig, at)
		}
		cancel()
		if ctx.Err() != nil {
			// Stopped mid-evaluation: the run's end, not the signal's.
			d.Result = ResultSkipped
			return d
		}
		d.Attempts++
		evaluated := at.UTC()
		d.EvaluatedAt = &evaluated
		d.Values, d.Series, d.Delivery, d.Error = nonNilValues(res.Values), res.Series, res.Delivery, ""
		if err != nil {
			d.Error = err.Error()
		}
		ok := err == nil && res.Pass
		u.update(func(s *Status) {
			s.Checks[i] = CheckStatus{Signal: sig.Name, Result: ResultPending, Attempts: d.Attempts, Values: d.Values, Error: d.Error}
		})
		switch {
		case sig.Mode == scenario.ModeEventually && ok:
			d.Result = ResultPass
			return d
		case sig.Mode == scenario.ModeAlways && !ok:
			d.Result = ResultFail
			return d
		case !at.Before(deadline):
			d.Result = ResultFail
			if sig.Mode == scenario.ModeAlways {
				d.Result = ResultPass
			}
			return d
		}
		next := at.Add(r.cfg.PollInterval)
		if next.After(deadline) {
			next = deadline
		}
		if sleepUntil(ctx, r.cfg.Now, next) {
			d.Result = ResultSkipped
			return d
		}
	}
}

func nonNilValues(v []float64) []float64 {
	if v == nil {
		return []float64{}
	}
	return v
}
