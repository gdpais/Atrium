package metadata

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestRepositoryListsScheduledProbes(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	repo := NewRepository(db)
	rows := sqlmock.NewRows([]string{
		"collector_id",
		"target_id",
		"environment_slug",
		"target_name",
		"engine",
		"host",
		"port",
		"database_name",
		"credential_ref",
		"probe_assignment_id",
		"probe_definition_id",
		"probe_name",
		"probe_category",
		"query_template",
		"timeout_ms",
		"interval_seconds",
	}).AddRow(
		"sql-prod",
		"target-1",
		"prod",
		"core-db",
		"sqlserver",
		"sql.prod.local",
		1433,
		"Atrium",
		"kv/sql-prod",
		"assign-1",
		"probe-1",
		"waits",
		"waits",
		"SELECT 1",
		5000,
		30,
	)

	mock.ExpectQuery(regexp.QuoteMeta(scheduledProbesQuery)).
		WithArgs("sqlserver", "sql-prod").
		WillReturnRows(rows)

	items, err := repo.ListScheduledProbes(context.Background(), QueryFilter{
		Engine:      "sqlserver",
		CollectorID: "sql-prod",
	})
	if err != nil {
		t.Fatalf("ListScheduledProbes: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	item := items[0]
	if item.Target.Name != "core-db" {
		t.Fatalf("unexpected target name: %s", item.Target.Name)
	}
	if item.Definition.Category != "waits" {
		t.Fatalf("unexpected category: %s", item.Definition.Category)
	}
	if item.Assignment.IntervalSeconds != 30 {
		t.Fatalf("unexpected interval: %d", item.Assignment.IntervalSeconds)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}
