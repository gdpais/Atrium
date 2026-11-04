package inventory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/collectors"
	"heartbeat/apps/api/internal/store"
)

// Runner says what the collector's sqlserver collectors run:
// *collectors.Client.
type Runner interface {
	SQLServer(ctx context.Context) (collectors.SQLServerCollectors, error)
}

var _ Runner = (*collectors.Client)(nil)

// WithRunner makes the Service able to mark probe assignments applied;
// without it, none ever is.
func WithRunner(r Runner) Option { return func(s *Service) { s.runner = r } }

// AppliedCheckInterval is how often RunAppliedCheck checks.
const AppliedCheckInterval = 30 * time.Second

// AppliedCheckTask names the applied check in its audit records.
const AppliedCheckTask = "probe assignment applied check"

// MarkAppliedProbeAssignments marks applied, and audits, every approved
// assignment the collector runs as rendered (ADR 0014 §4): its collector,
// api-<target id>-<interval>s, enabled at the interval, with the target as
// rendered and the probe with the same query, timeout and row limit. It
// returns how many it marked. A collector that cannot be asked marks none
// and returns the error. Nothing moves back: an assignment the collector
// stops running stays applied, and the collector's own metrics and
// readiness say why (ADR 0014 §7).
func (s *Service) MarkAppliedProbeAssignments(ctx context.Context, actor audit.Actor) (int, error) {
	if s.runner == nil {
		return 0, nil
	}
	rows, err := store.New(s.db).ListRenderedProbeAssignments(ctx)
	if err != nil {
		return 0, fmt.Errorf("list the approved probe assignments: %w", err)
	}
	var pending []store.ListRenderedProbeAssignmentsRow
	for _, row := range rows {
		if row.State == StateApproved {
			pending = append(pending, row)
		}
	}
	if len(pending) == 0 {
		return 0, nil
	}
	running, err := s.runner.SQLServer(ctx)
	if err != nil {
		return 0, err
	}
	marked := 0
	for _, row := range pending {
		if !runsAsRendered(running, row) {
			continue
		}
		caller := Caller{Actor: actor, RequestID: "applied-check-" + running.CheckedAt.UTC().Format(time.RFC3339Nano)}
		err := s.change(ctx, caller, audit.EntityProbeAssignment, func(q *store.Queries, record func(string, string, any, any) error) error {
			current, err := q.LockProbeAssignment(ctx, row.ID)
			if err != nil {
				return notFound(err)
			}
			// A change since the list moved it out of approved, or changed
			// what it runs and was approved again: the collector was found
			// running the assignment as listed, not as it is now. Every
			// change, approval and reset sets updated_at.
			if current.State != StateApproved || !current.UpdatedAt.Time.Equal(row.UpdatedAt.Time) {
				return nil
			}
			n, err := q.MarkProbeAssignmentApplied(ctx, row.ID)
			if err != nil || n == 0 {
				return err
			}
			before := probeAssignment(current)
			after := before
			after.State = StateApplied
			after.StateDetail = ""
			marked++
			return record(audit.ActionApply, before.ID.String(), before, after)
		})
		if err != nil && !errors.Is(err, ErrNotFound) {
			return marked, fmt.Errorf("mark probe assignment %s applied: %w", goUUID(row.ID), err)
		}
	}
	return marked, nil
}

// runsAsRendered reports whether the collector runs row's probe as the
// render holds it.
func runsAsRendered(running collectors.SQLServerCollectors, row store.ListRenderedProbeAssignmentsRow) bool {
	c, ok := running.ByID[CollectorID(goUUID(row.TargetID), int(row.IntervalSeconds))]
	if !ok || !c.Enabled || c.ScrapeInterval != time.Duration(row.IntervalSeconds)*time.Second {
		return false
	}
	want := renderedProbe(row)
	for _, t := range c.Targets {
		if t.Name != row.TargetName || t.EnvironmentSlug != row.EnvironmentSlug || t.Host != row.Host ||
			t.Port != int(row.Port) || t.DatabaseName != row.DatabaseName.String {
			continue
		}
		return slices.Contains(t.Probes, collectors.Probe{
			Name: want.Name, QueryTemplate: want.QueryTemplate, TimeoutMS: want.TimeoutMS, MaxRows: want.MaxRows,
		})
	}
	return false
}

// RunAppliedCheck runs MarkAppliedProbeAssignments every
// AppliedCheckInterval until ctx ends, logging what it marks and why it
// could not check.
func (s *Service) RunAppliedCheck(ctx context.Context, actor audit.Actor, logger *slog.Logger) {
	ticker := time.NewTicker(AppliedCheckInterval)
	defer ticker.Stop()
	for {
		n, err := s.MarkAppliedProbeAssignments(ctx, actor)
		switch {
		case err != nil && ctx.Err() == nil:
			logger.Warn("probe assignment applied check failed", "error", err)
		case n > 0:
			logger.Info("probe assignments applied", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
