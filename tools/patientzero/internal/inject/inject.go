// Package inject holds PatientZero's in-process faults, switched on and off
// by the scenario runner: latency and errors on its entry points, a bad
// deploy of one of its modules, and slow business SQL; and on its logging
// database: the weekly rotation, a log service that commits late, and
// verbose logging. The entry points ask the Injector before their business
// SQL, so an injected delay shows in the log rows' Duration and an injected
// failure writes an error row under the request's Request_Key, as a real
// failure would. Slow SQL is spent in SQL Server itself, inside the business
// statement's batch.
package inject

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"atrium/tools/patientzero/internal/scenario"
)

// ErrInjected marks a failure a scenario injected: errors.Is(err,
// ErrInjected) holds for every Failure.
var ErrInjected = errors.New("injected by scenario")

// Failure is a request a fault failed. Its message is the error row's
// Message.
type Failure struct{ Message string }

func (f *Failure) Error() string { return f.Message }

// Is makes every Failure an ErrInjected.
func (f *Failure) Is(target error) bool { return target == ErrInjected }

// Limits on the parameters, repeated in scenarios/scenario.schema.json.
const (
	MaxDelay       = time.Minute
	MaxMessageLen  = 200
	MaxCommitDelay = 10 * time.Minute
	MaxFloodRows   = 200
	MaxFloodBytes  = 2000
)

// DefaultFloodBytes is the length of a log-flood row's Message when its
// scenario names none.
const DefaultFloodBytes = 1000

// Rotator moves the logging database's views one cycle on, as the
// platform's weekly rotation does (oslog.SQLWriter).
type Rotator interface {
	Rotate(ctx context.Context) (map[string]int, error)
}

// DefaultException is the message of a bad deploy's failures when its
// scenario names none: .NET's NullReferenceException, the commonest
// exception in an OutSystems error log.
const DefaultException = "Object reference not set to an instance of an object."

// FirstVersion is every module's version (the error log's eSpaceVer) until
// a deploy fault publishes a new one.
const FirstVersion = 1

type kind int

const (
	kindLatency     kind = iota // delays the request before its business SQL
	kindErrors                  // fails the request before its business SQL
	kindDeploy                  // a new module version: delays and fails
	kindSlowSQL                 // delays the business SQL in SQL Server
	kindCommitDelay             // commits the log rows late
	kindFlood                   // writes extra general rows per request
)

type rule struct {
	kind        kind
	entryPoints []string // empty: every entry point
	ratio       float64  // share of the requests hit (latency, errors, slow SQL)
	delay       time.Duration
	jitter      time.Duration
	message     string  // the failure's message (errors, deploy)
	errorRatio  float64 // share of the requests a deploy fails
	rows, bytes int     // a flood's extra rows per request and their size
}

func (r *rule) applies(entryPoint string) bool {
	return len(r.entryPoints) == 0 || slices.Contains(r.entryPoints, entryPoint)
}

// Injector is the set of active faults. The zero value has none; a nil
// *Injector is valid and injects nothing.
type Injector struct {
	mu    sync.RWMutex
	rules map[*rule]struct{}
	// running is each module's version now, published the newest version
	// number each module has had; absent means FirstVersion.
	running   map[string]int
	published map[string]int
	// Float returns a number in [0, 1); math/rand/v2 when nil.
	Float func() float64
	// Sleep waits d or until ctx ends; a timer when nil.
	Sleep func(ctx context.Context, d time.Duration) error
	// Rotator rotates the logging database for the rotation fault; without
	// one that fault fails to start.
	Rotator Rotator
}

func (i *Injector) add(r *rule) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.rules == nil {
		i.rules = map[*rule]struct{}{}
	}
	i.rules[r] = struct{}{}
}

func (i *Injector) remove(r *rule) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.rules, r)
}

// Active is how many faults are switched on.
func (i *Injector) Active() int {
	if i == nil {
		return 0
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	return len(i.rules)
}

// Version is the version of module running now: FirstVersion, or the one a
// deploy fault published.
func (i *Injector) Version(module string) int {
	if i == nil {
		return FirstVersion
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	if v, ok := i.running[module]; ok {
		return v
	}
	return FirstVersion
}

// publish makes a new version of module the running one and returns the
// version it replaced.
func (i *Injector) publish(module string) (previous int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.running == nil {
		i.running, i.published = map[string]int{}, map[string]int{}
	}
	previous, newest := FirstVersion, FirstVersion
	if v, ok := i.running[module]; ok {
		previous = v
	}
	if v, ok := i.published[module]; ok {
		newest = v
	}
	i.published[module] = newest + 1
	i.running[module] = newest + 1
	return previous
}

// rollBack makes version the running one of module again.
func (i *Injector) rollBack(module string, version int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.running[module] = version
}

// Apply runs the active faults for one request to entryPoint: it waits out
// every delay that hits the request, then returns a *Failure if a fault
// fails it. A request that ends while waiting returns its context's error.
// Slow SQL is not applied here: see SQLDelay.
func (i *Injector) Apply(ctx context.Context, entryPoint string) error {
	if i == nil {
		return nil
	}
	i.mu.RLock()
	var delay time.Duration
	var failure string
	fail := func(message string) {
		if failure == "" {
			failure = message
		}
	}
	for r := range i.rules {
		if !r.applies(entryPoint) {
			continue
		}
		switch r.kind {
		case kindLatency:
			if i.float() < r.ratio {
				delay += i.jittered(r)
			}
		case kindErrors:
			if i.float() < r.ratio {
				fail(r.message)
			}
		case kindDeploy:
			delay += r.delay
			if r.errorRatio > 0 && i.float() < r.errorRatio {
				fail(r.message)
			}
		}
	}
	i.mu.RUnlock()
	if delay > 0 {
		if err := i.sleep(ctx, delay); err != nil {
			return err
		}
	}
	if failure != "" {
		return &Failure{Message: failure}
	}
	return nil
}

// SQLDelay is how much longer the business SQL of one request to
// entryPoint takes in SQL Server: the sum of the sql-latency faults that hit
// it. The entry point passes it to the Store (shop.WithDelay).
func (i *Injector) SQLDelay(entryPoint string) time.Duration {
	if i == nil {
		return 0
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	var delay time.Duration
	for r := range i.rules {
		if r.kind == kindSlowSQL && r.applies(entryPoint) && i.float() < r.ratio {
			delay += i.jittered(r)
		}
	}
	return delay
}

// CommitDelay is how long the log rows of a request that ends now wait
// before they are committed: the longest active commit-delay fault, as one
// log service queue holds every row.
func (i *Injector) CommitDelay() time.Duration {
	if i == nil {
		return 0
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	var delay time.Duration
	for r := range i.rules {
		if r.kind == kindCommitDelay && r.delay > delay {
			delay = r.delay
		}
	}
	return delay
}

// Flood is how many extra general rows a request to entryPoint logs, and
// the length of their Message: the sum of the active log-flood faults that
// hit it, the longest Message.
func (i *Injector) Flood(entryPoint string) (rows, bytes int) {
	if i == nil {
		return 0, 0
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	for r := range i.rules {
		if r.kind == kindFlood && r.applies(entryPoint) {
			rows += r.rows
			bytes = max(bytes, r.bytes)
		}
	}
	return rows, bytes
}

func (i *Injector) jittered(r *rule) time.Duration {
	d := r.delay
	if r.jitter > 0 {
		d += time.Duration(i.float() * float64(r.jitter))
	}
	return d
}

func (i *Injector) float() float64 {
	if i.Float != nil {
		return i.Float()
	}
	return rand.Float64()
}

func (i *Injector) sleep(ctx context.Context, d time.Duration) error {
	if i.Sleep != nil {
		return i.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// fault switches one rule on and off.
type fault struct {
	inj  *Injector
	rule *rule
}

func (f *fault) Start(context.Context) error { f.inj.add(f.rule); return nil }
func (f *fault) Stop(context.Context) error  { f.inj.remove(f.rule); return nil }

// deploy is a new version of a module: published when it starts, the
// previous version published again (a rollback) when it stops.
type deploy struct {
	fault
	module   string
	previous int
}

func (d *deploy) Start(ctx context.Context) error {
	d.previous = d.inj.publish(d.module)
	return d.fault.Start(ctx)
}

func (d *deploy) Stop(ctx context.Context) error {
	err := d.fault.Stop(ctx)
	d.inj.rollBack(d.module, d.previous)
	return err
}

// rotation rotates the logging database when it starts; its end changes
// nothing, as the platform's weekly switch is not undone.
type rotation struct{ inj *Injector }

func (r *rotation) Start(ctx context.Context) error {
	if r.inj.Rotator == nil {
		return errors.New("no logging database to rotate")
	}
	_, err := r.inj.Rotator.Rotate(ctx)
	return err
}

func (r *rotation) Stop(context.Context) error { return nil }

// delayParams are the parameters of the faults that delay requests.
type delayParams struct {
	EntryPoints []string `json:"entry_points"`
	Delay       string   `json:"delay"`
	Jitter      string   `json:"jitter"`
	Ratio       *float64 `json:"ratio"`
}

func (p delayParams) rule(k kind, known []string) (*rule, error) {
	r := &rule{kind: k, ratio: 1}
	var err error
	if r.entryPoints, err = checkEntryPoints(p.EntryPoints, known); err != nil {
		return nil, err
	}
	if r.delay, err = duration("delay", p.Delay, true); err != nil {
		return nil, err
	}
	if r.jitter, err = duration("jitter", p.Jitter, false); err != nil {
		return nil, err
	}
	if r.delay+r.jitter > MaxDelay {
		return nil, fmt.Errorf("delay plus jitter must be at most %s", MaxDelay)
	}
	if p.Ratio != nil {
		if r.ratio, err = ratio(*p.Ratio); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Register adds the latency, errors, deploy, sql-latency, rotation,
// commit-delay and log-flood fault types, acting on inj, to reg. modules maps each PatientZero module (eSpace) to
// its entry points, named as Atrium's operation label names them: the
// names a fault may target.
func Register(reg *scenario.Registry, inj *Injector, modules map[string][]string) {
	var entryPoints []string
	for _, eps := range modules {
		entryPoints = append(entryPoints, eps...)
	}
	sort.Strings(entryPoints)
	reg.Register(scenario.FaultType{
		Name:        "latency",
		Scope:       scenario.ScopeInProcess,
		Description: "Delays requests to PatientZero's entry points before their business SQL",
		New: func(params map[string]any) (scenario.Fault, error) {
			var p delayParams
			if err := scenario.DecodeParams(params, &p); err != nil {
				return nil, err
			}
			r, err := p.rule(kindLatency, entryPoints)
			if err != nil {
				return nil, err
			}
			return &fault{inj: inj, rule: r}, nil
		},
	})
	reg.Register(scenario.FaultType{
		Name:        "errors",
		Scope:       scenario.ScopeInProcess,
		Description: "Fails a share of the requests to PatientZero's entry points, with an error row",
		New: func(params map[string]any) (scenario.Fault, error) {
			var p struct {
				EntryPoints []string `json:"entry_points"`
				Ratio       *float64 `json:"ratio"`
				Message     string   `json:"message"`
			}
			if err := scenario.DecodeParams(params, &p); err != nil {
				return nil, err
			}
			message := strings.TrimSpace(p.Message)
			r := &rule{kind: kindErrors}
			var err error
			if r.entryPoints, err = checkEntryPoints(p.EntryPoints, entryPoints); err != nil {
				return nil, err
			}
			if p.Ratio == nil {
				return nil, errors.New("ratio is required")
			}
			if r.ratio, err = ratio(*p.Ratio); err != nil {
				return nil, err
			}
			if message == "" {
				message = "scenario fault"
			}
			if len(message) > MaxMessageLen {
				return nil, fmt.Errorf("message must be at most %d bytes", MaxMessageLen)
			}
			r.message = ErrInjected.Error() + ": " + message
			return &fault{inj: inj, rule: r}, nil
		},
	})
	reg.Register(scenario.FaultType{
		Name:        "deploy",
		Scope:       scenario.ScopeInProcess,
		Description: "Publishes a broken version of one PatientZero module, whose requests fail or slow down; the previous version is published again when it ends",
		New: func(params map[string]any) (scenario.Fault, error) {
			var p struct {
				Module     string   `json:"module"`
				ErrorRatio *float64 `json:"error_ratio"`
				Exception  string   `json:"exception"`
				Delay      string   `json:"delay"`
			}
			if err := scenario.DecodeParams(params, &p); err != nil {
				return nil, err
			}
			eps, ok := modules[p.Module]
			if !ok {
				names := make([]string, 0, len(modules))
				for m := range modules {
					names = append(names, m)
				}
				sort.Strings(names)
				return nil, fmt.Errorf("module %q is not one of %s", p.Module, strings.Join(names, ", "))
			}
			r := &rule{kind: kindDeploy, entryPoints: slices.Clone(eps), message: strings.TrimSpace(p.Exception)}
			var err error
			if p.ErrorRatio != nil {
				if r.errorRatio, err = ratio(*p.ErrorRatio); err != nil {
					return nil, fmt.Errorf("error_%w", err)
				}
			}
			if r.delay, err = duration("delay", p.Delay, false); err != nil {
				return nil, err
			}
			if r.delay > MaxDelay {
				return nil, fmt.Errorf("delay must be at most %s", MaxDelay)
			}
			if r.errorRatio == 0 && r.delay == 0 {
				return nil, errors.New("give error_ratio, delay or both: a deploy that changes nothing is no fault")
			}
			if r.message == "" {
				r.message = DefaultException
			}
			if len(r.message) > MaxMessageLen {
				return nil, fmt.Errorf("exception must be at most %d bytes", MaxMessageLen)
			}
			return &deploy{fault: fault{inj: inj, rule: r}, module: p.Module}, nil
		},
	})
	reg.Register(scenario.FaultType{
		Name:        "sql-latency",
		Scope:       scenario.ScopeInProcess,
		Description: "Makes the business SQL of PatientZero's entry points run longer in SQL Server",
		New: func(params map[string]any) (scenario.Fault, error) {
			var p delayParams
			if err := scenario.DecodeParams(params, &p); err != nil {
				return nil, err
			}
			r, err := p.rule(kindSlowSQL, entryPoints)
			if err != nil {
				return nil, err
			}
			return &fault{inj: inj, rule: r}, nil
		},
	})
	reg.Register(scenario.FaultType{
		Name:        "rotation",
		Scope:       scenario.ScopeInProcess,
		Description: "Rotates PatientZero's logging database as the platform does every Friday at 23:45: every log type's views move one cycle table on",
		New: func(params map[string]any) (scenario.Fault, error) {
			var p struct{}
			if err := scenario.DecodeParams(params, &p); err != nil {
				return nil, err
			}
			return &rotation{inj: inj}, nil
		},
	})
	reg.Register(scenario.FaultType{
		Name:        "commit-delay",
		Scope:       scenario.ScopeInProcess,
		Description: "Holds every request's log rows back for a while before they are committed, as a log service that falls behind does",
		New: func(params map[string]any) (scenario.Fault, error) {
			var p struct {
				Delay string `json:"delay"`
			}
			if err := scenario.DecodeParams(params, &p); err != nil {
				return nil, err
			}
			d, err := duration("delay", p.Delay, true)
			if err != nil {
				return nil, err
			}
			if d > MaxCommitDelay {
				return nil, fmt.Errorf("delay must be at most %s", MaxCommitDelay)
			}
			return &fault{inj: inj, rule: &rule{kind: kindCommitDelay, delay: d}}, nil
		},
	})
	reg.Register(scenario.FaultType{
		Name:        "log-flood",
		Scope:       scenario.ScopeInProcess,
		Description: "Makes PatientZero's entry points log extra general rows per request, as verbose logging left on does",
		New: func(params map[string]any) (scenario.Fault, error) {
			var p struct {
				EntryPoints []string `json:"entry_points"`
				Rows        int      `json:"rows"`
				Bytes       *int     `json:"bytes"`
			}
			if err := scenario.DecodeParams(params, &p); err != nil {
				return nil, err
			}
			r := &rule{kind: kindFlood, rows: p.Rows, bytes: DefaultFloodBytes}
			var err error
			if r.entryPoints, err = checkEntryPoints(p.EntryPoints, entryPoints); err != nil {
				return nil, err
			}
			if r.rows < 1 || r.rows > MaxFloodRows {
				return nil, fmt.Errorf("rows must be 1 to %d", MaxFloodRows)
			}
			if p.Bytes != nil {
				if *p.Bytes < 1 || *p.Bytes > MaxFloodBytes {
					return nil, fmt.Errorf("bytes must be 1 to %d", MaxFloodBytes)
				}
				r.bytes = *p.Bytes
			}
			return &fault{inj: inj, rule: r}, nil
		},
	})
}

func checkEntryPoints(got, known []string) ([]string, error) {
	for _, e := range got {
		if !slices.Contains(known, e) {
			return nil, fmt.Errorf("entry point %q is not one of %s", e, strings.Join(known, ", "))
		}
	}
	return slices.Clone(got), nil
}

func duration(name, s string, required bool) (time.Duration, error) {
	if s == "" {
		if required {
			return 0, fmt.Errorf("%s is required", name)
		}
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 || (required && d == 0) {
		return 0, fmt.Errorf("%s %q must be a positive duration such as 500ms", name, s)
	}
	return d, nil
}

func ratio(v float64) (float64, error) {
	if !(v > 0 && v <= 1) {
		return 0, fmt.Errorf("ratio %v must be more than 0 and at most 1", v)
	}
	return v, nil
}
