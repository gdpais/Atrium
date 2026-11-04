//go:build integration

package main

import (
	"bytes"
	"context"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"heartbeat/apps/api/internal/inventory"
	"heartbeat/apps/api/internal/testenv"
	"heartbeat/internal/credentials"
)

// `api users create-admin` end to end: it refuses a database that is not
// migrated, creates the admin with its audit record once it is, and
// refuses the same email again.
func TestCreateAdminCommand(t *testing.T) {
	pg, cfg := testenv.Postgres(t)
	setRequired(t)
	t.Setenv(envPostgresHost, cfg.Host)
	t.Setenv(envPostgresPort, strconv.Itoa(cfg.Port))
	t.Setenv(envPostgresSSLMode, "disable")
	t.Setenv(credentials.Key(testenv.PostgresRef), pg.Credential.Username+":"+pg.Credential.Password)
	ctx := context.Background()
	createAdmin := func(stdin string) (string, error) {
		var logs bytes.Buffer
		err := execute(ctx, []string{"users", "create-admin", "--email", "admin@example.com", "--name", "First Admin"},
			slog.New(slog.NewJSONHandler(&logs, nil)), strings.NewReader(stdin), &bytes.Buffer{})
		return logs.String(), err
	}
	const secret = "correct horse battery staple, admin"

	if _, err := createAdmin(secret + "\n"); err == nil || !strings.Contains(err.Error(), "schema not migrated") {
		t.Fatalf("create-admin before migrating = %v, want schema not migrated", err)
	}
	if err := execute(ctx, []string{"migrate", "up"}, slog.New(slog.DiscardHandler), strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	logs, err := createAdmin(secret + "\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"msg":"users: admin created"`, `"email":"admin@example.com"`, `"audit_record_id":"`, `"user_id":"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %s:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, secret) || strings.Contains(logs, "$argon2id$") {
		t.Fatalf("logs contain the password or its hash:\n%s", logs)
	}
	record := pg.Exec(t, "psql", "-U", "heartbeat", "-d", "heartbeat", "-tAc",
		"select record->>'entity_type', record->'actor'->>'type', record->'actor'->>'command', record->'after'->>'display_name' from audit_outbox")
	if got := strings.TrimSpace(record); got != "user|cli|api users create-admin|First Admin" {
		t.Fatalf("audit record: %q", got)
	}

	if _, err := createAdmin(secret + "\n"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("create-admin with the same email = %v, want already exists", err)
	}

	// `api probe-assignments render` prints the render, empty with no
	// approvals, and writes nothing.
	var rendered bytes.Buffer
	if err := execute(ctx, []string{"probe-assignments", "render"}, slog.New(slog.DiscardHandler), strings.NewReader(""), &rendered); err != nil {
		t.Fatal(err)
	}
	if rendered.String() != inventory.RenderHeader+"managedCollectors: []\n" {
		t.Fatalf("probe-assignments render = %q", rendered.String())
	}
	if n := strings.TrimSpace(pg.Exec(t, "psql", "-U", "heartbeat", "-d", "heartbeat", "-tAc", "select count(*) from audit_outbox")); n != "1" {
		t.Fatalf("audit records after the render: %s, want the admin's only", n)
	}
}
