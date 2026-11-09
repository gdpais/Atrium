package audit

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"atrium/apps/api/internal/store"
	"atrium/internal/telemetry"
)

var tracer = telemetry.Tracer("atrium/api/audit")

// finishTimeout bounds deleting delivered rows and committing, which go on
// after a shutdown has begun: records the Collector took are not sent again.
// It bounds a rollback too.
const finishTimeout = 5 * time.Second

// Row is one record waiting in the outbox.
type Row struct {
	// Seq orders the rows: the oldest first.
	Seq int64
	// ID is the record's id.
	ID string
	// Record is the record as JSON.
	Record []byte
}

// Beginner starts transactions: a pool or a connection.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Outbox is the audit_outbox table.
type Outbox struct {
	DB Beginner
}

// Drain claims up to limit of the oldest rows that no other drainer holds
// and passes them to deliver, inside a transaction that keeps them locked.
// When deliver returns nil the rows are deleted; otherwise they are left for
// a later round, by this drainer or another. It returns how many rows it
// delivered, 0 when there were none.
//
// A round that finds rows is traced as audit.drain; one that finds none
// makes no span, so an idle outbox fills no traces.
func (o Outbox) Drain(ctx context.Context, limit int, deliver func(context.Context, []Row) error) (int, error) {
	tx, err := o.DB.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("audit outbox: begin: %w", err)
	}
	defer func() {
		finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
		defer cancel()
		_ = tx.Rollback(finish) // a no-op after Commit
	}()
	q := store.New(tx)
	claimed, err := q.ClaimAuditRecords(ctx, int32(limit))
	if err != nil {
		return 0, fmt.Errorf("audit outbox: claim: %w", err)
	}
	if len(claimed) == 0 {
		return 0, nil
	}

	ctx, span := tracer.Start(ctx, "audit.drain")
	defer span.End()
	span.SetAttributes(attribute.Int("audit.records", len(claimed)))
	rows := make([]Row, len(claimed))
	seqs := make([]int64, len(claimed))
	for i, c := range claimed {
		rows[i] = Row{Seq: c.Seq, ID: c.ID.String(), Record: c.Record}
		seqs[i] = c.Seq
	}
	if err := deliver(ctx, rows); err != nil {
		span.SetStatus(codes.Error, "delivery failed")
		return 0, err
	}

	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	if err := q.DeleteAuditRecords(finish, seqs); err != nil {
		span.SetStatus(codes.Error, "delete failed")
		return 0, fmt.Errorf("audit outbox: delete %d delivered rows: %w", len(seqs), err)
	}
	if err := tx.Commit(finish); err != nil {
		span.SetStatus(codes.Error, "commit failed")
		return 0, fmt.Errorf("audit outbox: commit the delete of %d delivered rows: %w", len(seqs), err)
	}
	return len(rows), nil
}
