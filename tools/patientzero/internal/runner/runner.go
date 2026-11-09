// Package runner runs PatientZero's fault scenarios, one at a time: it
// applies each step's fault for the step's duration, evaluates the
// scenario's expected signals over their windows, counts the traffic, and
// writes everything to the run's journal. The admin endpoints in admin.go
// start, watch and stop runs.
package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"atrium/tools/patientzero/internal/journal"
	"atrium/tools/patientzero/internal/scenario"
	"atrium/tools/patientzero/internal/signal"
	"atrium/tools/patientzero/internal/traffic"
)

// Defaults for Config.
const (
	DefaultKeepJournals    = 20
	DefaultSummaryInterval = 15 * time.Second
	DefaultPollInterval    = 10 * time.Second
	// faultStopTimeout bounds a fault's Stop, which runs with a fresh
	// context after the run's own has ended: longer than a cluster fault
	// waits for its host agent.
	faultStopTimeout = DefaultAgentTimeout + 30*time.Second
	// evalTimeout bounds one signal evaluation.
	evalTimeout = 15 * time.Second
)

// Errors Start returns.
var (
	ErrUnknown     = errors.New("no such scenario")
	ErrBusy        = errors.New("a scenario is already running")
	ErrNotRunnable = errors.New("this runner cannot run the scenario")
	ErrNotRunning  = errors.New("no scenario is running")
)

// Config configures a Runner.
type Config struct {
	Scenarios []*scenario.Scenario
	Registry  *scenario.Registry
	// Scopes are the fault scopes this runner applies; scenarios with a
	// fault of another scope are listed but not runnable.
	Scopes  []scenario.Scope
	Sources signal.Sources
	Traffic *traffic.Counter
	// JournalDir holds the run journals; the newest KeepJournals are kept.
	JournalDir   string
	KeepJournals int
	// SummaryInterval is how often traffic is summarized in the journal.
	SummaryInterval time.Duration
	// PollInterval is how often a signal is evaluated within its window.
	PollInterval time.Duration
	// Host names this runner in run_start, such as the pod name.
	Host string
	// Delegate hands cluster faults to the host agent of a run started with
	// one (delegate.go); nil runs no cluster faults.
	Delegate *Delegate
	Now      func() time.Time
	Log      *slog.Logger
}

// Runner runs scenarios one at a time.
type Runner struct {
	cfg    Config
	base   context.Context
	byName map[string]*scenario.Scenario

	mu      sync.Mutex
	current *run
	last    *run
	done    chan struct{}
}

// New returns a runner. Runs stop when ctx ends.
func New(ctx context.Context, cfg Config) *Runner {
	if cfg.KeepJournals <= 0 {
		cfg.KeepJournals = DefaultKeepJournals
	}
	if cfg.SummaryInterval <= 0 {
		cfg.SummaryInterval = DefaultSummaryInterval
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Traffic == nil {
		cfg.Traffic = &traffic.Counter{}
	}
	r := &Runner{cfg: cfg, base: ctx, byName: map[string]*scenario.Scenario{}}
	for _, s := range cfg.Scenarios {
		r.byName[s.Name] = s
	}
	return r
}

// Listing describes a scenario for the admin endpoint.
type Listing struct {
	Name               string   `json:"name"`
	Description        string   `json:"description"`
	Load               string   `json:"load"`
	Faults             []string `json:"faults"`
	Steps              []string `json:"steps"`
	Signals            []string `json:"signals"`
	StepsSeconds       float64  `json:"steps_seconds"`
	MaxDurationSeconds float64  `json:"max_duration_seconds"`
	Runnable           bool     `json:"runnable"`
	Reason             string   `json:"reason,omitempty"`
	// Agent is true when the scenario has cluster faults: it runs only with
	// a host agent attached to apply them.
	Agent bool `json:"agent"`
}

// FaultTypeListing describes a registered fault type.
type FaultTypeListing struct {
	Name        string `json:"name"`
	Scope       string `json:"scope"`
	Description string `json:"description"`
	Runnable    bool   `json:"runnable"`
}

// Scenarios lists the scenarios and whether this runner can run them.
func (r *Runner) Scenarios() []Listing {
	out := make([]Listing, 0, len(r.cfg.Scenarios))
	for _, s := range r.cfg.Scenarios {
		l := Listing{
			Name: s.Name, Description: strings.TrimSpace(s.Description), Load: s.Load,
			Faults:             nonNil(s.FaultTypes()),
			StepsSeconds:       s.StepsDuration().Seconds(),
			MaxDurationSeconds: s.MaxDuration().Seconds(),
		}
		for _, st := range s.Steps {
			l.Steps = append(l.Steps, st.Name)
		}
		for _, sig := range s.Signals {
			l.Signals = append(l.Signals, sig.Name)
		}
		l.Signals = nonNil(l.Signals)
		l.Agent = r.needsAgent(s)
		if err := r.runnable(s, l.Agent); err != nil {
			l.Reason = err.Error()
		} else {
			l.Runnable = true
		}
		out = append(out, l)
	}
	return out
}

// FaultTypes lists the registered fault types.
func (r *Runner) FaultTypes() []FaultTypeListing {
	var out []FaultTypeListing
	for _, t := range r.cfg.Registry.Types() {
		out = append(out, FaultTypeListing{Name: t.Name, Scope: string(t.Scope), Description: t.Description,
			Runnable: slices.Contains(r.cfg.Scopes, t.Scope)})
	}
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// needsAgent reports whether s has a cluster fault.
func (r *Runner) needsAgent(s *scenario.Scenario) bool {
	for _, st := range s.Steps {
		if st.Fault == nil {
			continue
		}
		if t, ok := r.cfg.Registry.Type(st.Fault.Type); ok && t.Scope == scenario.ScopeCluster {
			return true
		}
	}
	return false
}

// runnable reports why this runner cannot run s, with or without a host
// agent attached, or nil.
func (r *Runner) runnable(s *scenario.Scenario, agent bool) error {
	for _, st := range s.Steps {
		if st.Fault == nil {
			continue
		}
		t, ok := r.cfg.Registry.Type(st.Fault.Type)
		if !ok {
			return fmt.Errorf("fault type %q is not registered", st.Fault.Type)
		}
		if !slices.Contains(r.cfg.Scopes, t.Scope) || (t.Scope == scenario.ScopeCluster && r.cfg.Delegate == nil) {
			return fmt.Errorf("fault type %q acts on the %s; this runner applies only %s faults", t.Name, t.Scope, joinScopes(r.cfg.Scopes))
		}
		if t.Scope == scenario.ScopeCluster && !agent {
			return fmt.Errorf("fault type %q acts on the cluster and needs a host agent to apply it: start the run with make patientzero-scenario", t.Name)
		}
	}
	for _, sig := range s.Signals {
		if r.cfg.Sources.URLFor(sig.Kind()) == "" {
			return fmt.Errorf("signal %q needs a %s URL, and none is configured", sig.Name, sig.Kind())
		}
		if sig.Kind() == scenario.KindDelivery && (r.cfg.Sources.Ledger == nil || r.cfg.Sources.Application == "") {
			return fmt.Errorf("signal %q is a delivery check, which needs the ledger of committed rows", sig.Name)
		}
	}
	return nil
}

func joinScopes(scopes []scenario.Scope) string {
	s := make([]string, len(scopes))
	for i, v := range scopes {
		s[i] = string(v)
	}
	return strings.Join(s, ", ")
}

// Run states.
const (
	StateRunning = "running"
	StatePassed  = "passed"
	StateFailed  = "failed"
	StateStopped = "stopped"
	StateError   = "error"
)

// Check results.
const (
	ResultPending = "pending"
	ResultPass    = "pass"
	ResultFail    = "fail"
	ResultSkipped = "skipped"
)

// Status is a run's state for the admin endpoint.
type Status struct {
	Run      string        `json:"run"`
	Scenario string        `json:"scenario"`
	State    string        `json:"state"`
	Started  time.Time     `json:"started"`
	Ended    *time.Time    `json:"ended,omitempty"`
	Step     string        `json:"step,omitempty"`
	Steps    []string      `json:"steps"`
	Checks   []CheckStatus `json:"checks"`
	Error    string        `json:"error,omitempty"`
	Journal  string        `json:"journal"`
	// FaultRequest is the cluster fault the host agent has to apply or
	// remove now, if any.
	FaultRequest *FaultRequest `json:"fault_request,omitempty"`
}

// CheckStatus is one signal's check so far.
type CheckStatus struct {
	Signal   string    `json:"signal"`
	Result   string    `json:"result"`
	Attempts int       `json:"attempts"`
	Values   []float64 `json:"values,omitempty"`
	Error    string    `json:"error,omitempty"`
}

type run struct {
	id       string
	scenario *scenario.Scenario
	cancel   context.CancelFunc
	stopped  bool

	mu     sync.Mutex
	status Status
}

func (u *run) snapshot() Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	s := u.status
	s.Checks = slices.Clone(u.status.Checks)
	s.Steps = slices.Clone(u.status.Steps)
	if u.status.Ended != nil {
		e := *u.status.Ended
		s.Ended = &e
	}
	return s
}

func (u *run) update(f func(*Status)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	f(&u.status)
}

// newRunID is the UTC start time, the scenario name and four random hex
// digits: unique, and sortable by age.
func newRunID(now time.Time, name string) string {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return now.UTC().Format("20060102T150405Z") + "-" + name + "-" + hex.EncodeToString(b[:])
}

// Start starts the scenario called name and returns its status. It fails
// with ErrBusy while another run is active.
func (r *Runner) Start(name string) (Status, error) { return r.StartWith(name, false) }

// StartWith starts the scenario called name; agent says a host agent is
// attached to apply its cluster faults (delegate.go).
func (r *Runner) StartWith(name string, agent bool) (Status, error) {
	s, ok := r.byName[name]
	if !ok {
		return Status{}, ErrUnknown
	}
	if err := r.runnable(s, agent); err != nil {
		return Status{}, fmt.Errorf("%w: %v", ErrNotRunnable, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current != nil {
		return r.current.snapshot(), ErrBusy
	}
	if err := r.base.Err(); err != nil {
		return Status{}, err
	}
	// Every fault is built before anything starts, so bad parameters fail
	// the request instead of half a run.
	faults := make([]scenario.Fault, len(s.Steps))
	for i, st := range s.Steps {
		if st.Fault == nil {
			continue
		}
		f, err := r.cfg.Registry.New(*st.Fault)
		if err != nil {
			return Status{}, fmt.Errorf("%w: steps[%d]: %v", ErrNotRunnable, i, err)
		}
		faults[i] = f
	}
	start := r.cfg.Now()
	id := newRunID(start, s.Name)
	j, err := journal.Create(r.cfg.JournalDir, id)
	if err != nil {
		return Status{}, err
	}
	if err := journal.Prune(r.cfg.JournalDir, r.cfg.KeepJournals, id); err != nil {
		r.cfg.Log.Warn("pruning old journals failed", "err", err)
	}
	ctx, cancel := context.WithCancel(r.base)
	u := &run{id: id, scenario: s, cancel: cancel}
	u.status = Status{Run: id, Scenario: s.Name, State: StateRunning, Started: start.UTC(),
		Steps: []string{}, Checks: []CheckStatus{}, Journal: "/admin/runs/" + id + "/journal"}
	for _, st := range s.Steps {
		u.status.Steps = append(u.status.Steps, st.Name)
	}
	for _, sig := range s.Signals {
		u.status.Checks = append(u.status.Checks, CheckStatus{Signal: sig.Name, Result: ResultPending})
	}
	r.current = u
	r.done = make(chan struct{})
	done := r.done
	if agent && r.cfg.Delegate != nil {
		r.cfg.Delegate.attach()
	}
	go func() {
		defer close(done)
		r.execute(ctx, u, j, faults, start)
		r.cfg.Delegate.end()
		cancel()
		r.mu.Lock()
		r.current, r.last = nil, u
		r.mu.Unlock()
	}()
	return u.snapshot(), nil
}

// Stop stops the active run: its fault is removed, pending checks are
// skipped, and the journal ends with run_end state stopped. It returns once
// the run has ended.
func (r *Runner) Stop() (Status, error) {
	r.mu.Lock()
	u, done := r.current, r.done
	if u == nil {
		r.mu.Unlock()
		return Status{}, ErrNotRunning
	}
	u.stopped = true
	u.cancel()
	r.mu.Unlock()
	<-done
	return u.snapshot(), nil
}

// Status is the active run's status, or the last run's; false if there was
// none.
func (r *Runner) Status() (Status, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.current != nil:
		st := r.current.snapshot()
		st.FaultRequest = r.cfg.Delegate.Pending()
		return st, true
	case r.last != nil:
		return r.last.snapshot(), true
	}
	return Status{}, false
}

// Wait waits for the active run, if any, to end.
func (r *Runner) Wait() {
	r.mu.Lock()
	done := r.done
	r.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (r *Runner) isStopped(u *run) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return u.stopped
}
