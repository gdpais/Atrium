package httpapi

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"atrium/apps/api/internal/openapi"
)

// A Check tells whether a dependency the API needs to serve requests works,
// such as PostgreSQL or Redis. GET /readyz runs every check on each call.
type Check struct {
	// Name identifies the dependency in logs; GET /readyz never shows it.
	Name string
	// Run returns nil when the dependency works. It should return once its
	// context is done; one that does not counts as failed when the readiness
	// timeout expires, and is left to finish on its own.
	Run func(context.Context) error
}

// readiness runs the checks of GET /readyz and logs when the result changes.
type readiness struct {
	checks []Check
	// timeout bounds one evaluation, every check included.
	timeout time.Duration
	logger  *slog.Logger

	mu sync.Mutex
	// last is the previous result, "" before the first evaluation.
	last openapi.ReadinessStatus
}

// checkResult is the outcome of the check at index.
type checkResult struct {
	index int
	err   error
}

// ready runs every check concurrently and reports whether all of them
// passed within the timeout. A check still running when the timeout expires,
// or when ctx is done, counts as failed. With no checks the API is ready
// while it serves.
func (r *readiness) ready(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	// Buffered, so a check that answers after the timeout never blocks.
	results := make(chan checkResult, len(r.checks))
	for i, check := range r.checks {
		go func() { results <- checkResult{index: i, err: check.Run(ctx)} }()
	}
	errs := make([]error, len(r.checks))
	answered := make([]bool, len(r.checks))
collect:
	for range r.checks {
		select {
		case result := <-results:
			errs[result.index], answered[result.index] = result.err, true
		case <-ctx.Done():
			break collect
		}
	}
	var reasons []string
	for i, check := range r.checks {
		switch {
		case !answered[i]:
			reasons = append(reasons, fmt.Sprintf("%s: no answer: %v", check.Name, ctx.Err()))
		case errs[i] != nil:
			reasons = append(reasons, fmt.Sprintf("%s: %v", check.Name, errs[i]))
		}
	}
	r.logTransition(reasons)
	return len(reasons) == 0
}

// logTransition logs when the readiness result changes, with the failed
// checks and their errors when it becomes not ready.
func (r *readiness) logTransition(reasons []string) {
	status := openapi.Ready
	if len(reasons) > 0 {
		status = openapi.NotReady
	}
	r.mu.Lock()
	changed := r.last != status
	r.last = status
	r.mu.Unlock()
	if !changed {
		return
	}
	if status == openapi.Ready {
		r.logger.Info("readiness changed", "status", status)
		return
	}
	r.logger.Warn("readiness changed", "status", status, "reasons", reasons)
}
