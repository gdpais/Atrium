package collectors

import "time"

// CycleResult summarises one Runner.RunOnce cycle for a collector. Pollers
// hand it to their Report callback so lifecycle supervisors can derive
// readiness and freshness without parsing errors.
type CycleResult struct {
	CollectorID string
	Started     time.Time
	Finished    time.Time
	// Targets holds one entry per scheduled target, including targets skipped
	// because they are backing off after earlier failures.
	Targets []TargetResult
}

// TargetState is the outcome of one target within a cycle.
type TargetState string

const (
	TargetOK      TargetState = "ok"
	TargetFailed  TargetState = "failed"
	TargetBackoff TargetState = "backoff"
	// TargetBlocked is a target whose probes all ran without error, but at
	// least one of them waits for its output to be accepted downstream (it
	// returned [ErrBlocked]): an outsystems_logs log type whose batch the OTel
	// Collector has not accepted yet.  It is backpressure, not a failure: the
	// target does not back off and heartbeat_collector_target_up stays 1.
	TargetBlocked TargetState = "blocked"
	// TargetDegraded is a target with probes that failed, back off on their
	// own or were skipped while its other probes succeeded: only the failing
	// probes are missing, and skipped ones keep their last series.  The
	// target does not back off, heartbeat_collector_target_up stays 1 and its
	// last success advances; Probes lists the failing probes.
	// A target is failed, and backs off as a whole, only when every probe
	// that ran failed.
	TargetDegraded TargetState = "degraded"
	// TargetSkipped is a target none of whose probes ran because the cycle
	// reached its deadline first: the targets or probes before it used the
	// whole scrape interval.  It is not a failure: the target does not back
	// off, and its series, heartbeat_collector_target_up included, keep the
	// values of the last cycle that ran it.  Its last success does not
	// advance, so its age shows how stale those series are; no alert watches
	// it, but a collector whose cycles keep running out of time fires
	// HeartbeatCollectorCyclesLate.
	TargetSkipped TargetState = "skipped"
)

// ProbeState is the outcome of one probe of a degraded target.
type ProbeState string

const (
	// ProbeFailed is a probe that failed in this cycle.
	ProbeFailed ProbeState = "failed"
	// ProbeBackoff is a probe that did not run in this cycle: it waits out
	// the backoff of its earlier failures.
	ProbeBackoff ProbeState = "backoff"
	// ProbeSkipped is a probe that did not run in this cycle because the
	// cycle reached its deadline before it started.  It does not back off and
	// keeps the series of its last run; the next cycle runs it first.
	ProbeSkipped ProbeState = "skipped"
)

// ProbeResult is a failing probe of a [TargetDegraded] target.
type ProbeResult struct {
	Probe string
	State ProbeState
	// Err is the probe's error in this cycle, or in its last failed one while
	// it backs off.  Like TargetResult.Err, it is for logs and in-process
	// decisions only.
	Err                 error
	ConsecutiveFailures int
	NextAttempt         time.Time
}

// TargetResult is the per-target outcome of one cycle.
type TargetResult struct {
	Target string
	State  TargetState
	// Err joins every probe error for the target in this cycle when State is
	// TargetFailed, and in its last failed cycle when State is TargetBackoff;
	// when State is TargetBlocked it joins the [ErrBlocked] errors of the
	// probes that wait; when State is TargetDegraded it joins the errors of
	// its failing probes and those [ErrBlocked] errors; nil when State is
	// TargetOK. It is for logs and in-process decisions only
	// and must not be served on unauthenticated endpoints.
	Err                 error
	ConsecutiveFailures int
	LastSuccess         time.Time
	// NextAttempt is set when State is TargetBackoff or TargetFailed.
	NextAttempt time.Time
	// Probes lists the failing probes of a TargetDegraded target.
	Probes []ProbeResult
}

// Healthy reports whether every target in the cycle succeeded.  A blocked
// target counts as healthy: its probes ran.  A degraded one does not.
func (c CycleResult) Healthy() bool {
	for _, target := range c.Targets {
		if target.State != TargetOK && target.State != TargetBlocked {
			return false
		}
	}
	return true
}
