package audit_test

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/audit/audittest"
	"heartbeat/apps/api/internal/store"
)

// recorder is a store.DBTX that keeps the arguments of each Exec.
type recorder struct {
	execs [][]any
	err   error
}

func (r *recorder) Exec(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
	r.execs = append(r.execs, args)
	return pgconn.NewCommandTag("INSERT 0 1"), r.err
}

func (r *recorder) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("not used")
}

func (r *recorder) QueryRow(context.Context, string, ...any) pgx.Row { return nil }

type user struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

func validChange() audit.Change {
	return audit.Change{
		RequestID:  "4b7f3c1e-2a7d-4f43-9a51-0d6c1f5d2e10",
		Actor:      audit.CLIActor("api users create-admin", "api-7d9f-xk2p"),
		Action:     audit.ActionCreate,
		EntityType: audit.EntityUser,
		EntityID:   "0c5b2a3e-8f6d-4f1a-9a2b-3c4d5e6f7a8b",
		After:      user{ID: "0c5b2a3e-8f6d-4f1a-9a2b-3c4d5e6f7a8b", Email: "admin@example.com"},
	}
}

// Write inserts the record it returns, under the record's id, and the
// record is valid against the schema.
func TestWriteInsertsAValidRecord(t *testing.T) {
	db := &recorder{}
	before := time.Now()
	record, err := audit.Write(context.Background(), store.New(db), validChange())
	if err != nil {
		t.Fatal(err)
	}
	if len(db.execs) != 1 || len(db.execs[0]) != 2 {
		t.Fatalf("execs = %v, want one insert of id and record", db.execs)
	}
	id, ok := db.execs[0][0].(pgtype.UUID)
	if !ok || id.String() != record.ID {
		t.Fatalf("inserted id %v, want the record's %s", db.execs[0][0], record.ID)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(record.ID) {
		t.Fatalf("id %s is not a version 4 UUID", record.ID)
	}
	encoded := db.execs[0][1].([]byte)
	audittest.Validate(t, encoded)

	var stored map[string]any
	if err := json.Unmarshal(encoded, &stored); err != nil {
		t.Fatal(err)
	}
	if stored["id"] != record.ID || stored["before"] != nil || stored["entity_type"] != "user" || stored["action"] != "create" {
		t.Fatalf("stored record %v", stored)
	}
	if after := stored["after"].(map[string]any); after["email"] != "admin@example.com" {
		t.Fatalf("after = %v", after)
	}
	if record.Time.Before(before.Add(-time.Second)) || record.Time.Location() != time.UTC {
		t.Fatalf("time %v: want now, in UTC", record.Time)
	}
	if _, ok := stored["config_version"]; ok {
		t.Fatal("config_version is written though the change has none")
	}
}

// Each record gets an id of its own.
func TestWriteGivesEachRecordANewID(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		record, err := audit.Write(context.Background(), store.New(&recorder{}), validChange())
		if err != nil {
			t.Fatal(err)
		}
		if seen[record.ID] {
			t.Fatalf("id %s given twice", record.ID)
		}
		seen[record.ID] = true
	}
}

// A user actor and a config version are valid too.
func TestWriteRecordsAUserActorAndAConfigVersion(t *testing.T) {
	db := &recorder{}
	change := validChange()
	change.Actor = audit.UserActor("9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d", "operator@example.com")
	change.Before = map[string]any{"status": "active"}
	change.ConfigVersion = "sha256:4f2a"
	if _, err := audit.Write(context.Background(), store.New(db), change); err != nil {
		t.Fatal(err)
	}
	encoded := db.execs[0][1].([]byte)
	audittest.Validate(t, encoded)
	if !strings.Contains(string(encoded), `"config_version":"sha256:4f2a"`) {
		t.Fatalf("record lacks the config version: %s", encoded)
	}
}

// The inventory file, applied by the API, is an actor too: by the file and
// the host, with the file's revision as the config version.
func TestWriteRecordsTheInventoryFileAsActor(t *testing.T) {
	db := &recorder{}
	change := validChange()
	change.Actor = audit.ConfigActor("/etc/heartbeat/inventory/inventory.yaml", "api-7d9f-xk2p")
	change.ConfigVersion = "sha256:9c1e"
	if _, err := audit.Write(context.Background(), store.New(db), change); err != nil {
		t.Fatal(err)
	}
	encoded := db.execs[0][1].([]byte)
	audittest.Validate(t, encoded)
	if !strings.Contains(string(encoded), `"actor":{"type":"config","host":"api-7d9f-xk2p","file":"/etc/heartbeat/inventory/inventory.yaml"}`) {
		t.Fatalf("record's actor: %s", encoded)
	}
}

// A change the schema could not describe fails the change, and nothing is
// written.
func TestWriteRejectsChangesItCannotRecord(t *testing.T) {
	for name, edit := range map[string]func(*audit.Change){
		"no request ID":      func(c *audit.Change) { c.RequestID = "" },
		"unknown entity":     func(c *audit.Change) { c.EntityType = "widget" },
		"no entity ID":       func(c *audit.Change) { c.EntityID = "" },
		"no action":          func(c *audit.Change) { c.Action = "" },
		"action not a name":  func(c *audit.Change) { c.Action = "Create User" },
		"neither state":      func(c *audit.Change) { c.After = nil },
		"nil pointer state":  func(c *audit.Change) { c.After = (*user)(nil) },
		"state not object":   func(c *audit.Change) { c.After = []string{"admin"} },
		"no actor":           func(c *audit.Change) { c.Actor = audit.Actor{} },
		"user without email": func(c *audit.Change) { c.Actor = audit.UserActor("9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d", "") },
		"user ID not a UUID": func(c *audit.Change) { c.Actor = audit.UserActor("42", "operator@example.com") },
		"cli without command": func(c *audit.Change) {
			c.Actor = audit.CLIActor("", "host")
		},
		"config without file": func(c *audit.Change) { c.Actor = audit.ConfigActor("", "host") },
		"config with command": func(c *audit.Change) {
			c.Actor = audit.Actor{Type: audit.ActorConfig, File: "inventory.yaml", Command: "x"}
		},
		"cli with file": func(c *audit.Change) {
			c.Actor = audit.Actor{Type: audit.ActorCLI, Command: "x", File: "inventory.yaml"}
		},
		"mixed actor": func(c *audit.Change) {
			c.Actor = audit.Actor{Type: audit.ActorCLI, Command: "x", Email: "a@example.com"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			db := &recorder{}
			change := validChange()
			edit(&change)
			if _, err := audit.Write(context.Background(), store.New(db), change); err == nil {
				t.Fatal("Write accepted the change")
			}
			if len(db.execs) != 0 {
				t.Fatalf("Write inserted %d rows for a change it rejected", len(db.execs))
			}
		})
	}
}

// A failed insert fails Write, so the change's transaction rolls back.
func TestWriteReturnsTheInsertError(t *testing.T) {
	db := &recorder{err: errors.New("connection reset")}
	if _, err := audit.Write(context.Background(), store.New(db), validChange()); err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("Write = %v, want the insert's error", err)
	}
}
