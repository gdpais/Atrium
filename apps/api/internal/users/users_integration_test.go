//go:build integration

package users_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/audit/audittest"
	"heartbeat/apps/api/internal/password"
	"heartbeat/apps/api/internal/testenv"
	"heartbeat/apps/api/internal/users"
)

const secret = "correct horse battery staple, admin"

var actor = audit.CLIActor("api users create-admin", "api-7d9f-xk2p")

func hasher() *password.Hasher { return password.NewHasher(password.DefaultParams, 1) }

// outbox returns the records waiting in the outbox, oldest first.
func outbox(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), "select record::text from audit_outbox order by seq")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var records []string
	for rows.Next() {
		var record string
		if err := rows.Scan(&record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return records
}

func count(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// CreateAdmin writes the user, its password hash, the admin role and one
// audit record of it, together; the record shows the user without the hash.
func TestCreateAdmin(t *testing.T) {
	t.Parallel()
	_, _, pool := testenv.MigratedPostgres(t)
	ctx := context.Background()
	created, err := users.CreateAdmin(ctx, pool, hasher(), users.NewAdmin{Email: " Admin@Example.COM ", Password: secret}, actor, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if created.Email != "admin@example.com" {
		t.Fatalf("email %q, want it trimmed and in lower case", created.Email)
	}
	var name, hash, status string
	err = pool.QueryRow(ctx, "select display_name, password_hash, status from users where id = $1", created.UserID).Scan(&name, &hash, &status)
	if err != nil {
		t.Fatal(err)
	}
	if name != "admin" || status != "active" || !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Fatalf("user: name %q, status %q, hash %q", name, status, hash)
	}
	if n := count(t, pool, "select count(*) from user_roles ur join roles r on r.id = ur.role_id where ur.user_id = $1 and r.name = 'admin'", created.UserID); n != 1 {
		t.Fatalf("the user has the admin role %d times, want once", n)
	}

	records := outbox(t, pool)
	if len(records) != 1 {
		t.Fatalf("%d audit records, want 1", len(records))
	}
	audittest.Validate(t, []byte(records[0]))
	audittest.NoSecrets(t, []byte(records[0]), secret, hash)
	var record struct {
		ID         string      `json:"id"`
		RequestID  string      `json:"request_id"`
		Actor      audit.Actor `json:"actor"`
		Action     string      `json:"action"`
		EntityType string      `json:"entity_type"`
		EntityID   string      `json:"entity_id"`
		Before     any         `json:"before"`
		After      users.User  `json:"after"`
	}
	if err := json.Unmarshal([]byte(records[0]), &record); err != nil {
		t.Fatal(err)
	}
	want := users.User{ID: created.UserID, Email: "admin@example.com", DisplayName: "admin", Status: "active", Roles: []string{"admin"}}
	if record.ID != created.AuditRecord.ID || record.RequestID != "req-1" || record.Actor != actor ||
		record.Action != "create" || record.EntityType != "user" || record.EntityID != created.UserID || record.Before != nil ||
		record.After.ID != want.ID || record.After.Email != want.Email || record.After.DisplayName != want.DisplayName ||
		record.After.Status != want.Status || strings.Join(record.After.Roles, ",") != "admin" {
		t.Fatalf("record %s, want the creation of %+v", records[0], want)
	}
}

// An email that belongs to a user already, in any case, is refused, and
// nothing is written.
func TestCreateAdminRefusesAnExistingEmail(t *testing.T) {
	t.Parallel()
	_, _, pool := testenv.MigratedPostgres(t)
	ctx := context.Background()
	if _, err := users.CreateAdmin(ctx, pool, hasher(), users.NewAdmin{Email: "admin@example.com", Password: secret}, actor, "req-1"); err != nil {
		t.Fatal(err)
	}
	_, err := users.CreateAdmin(ctx, pool, hasher(), users.NewAdmin{Email: "ADMIN@example.com", Password: secret + "!"}, actor, "req-2")
	if !errors.Is(err, users.ErrEmailTaken) {
		t.Fatalf("second CreateAdmin = %v, want ErrEmailTaken", err)
	}
	if n := count(t, pool, "select count(*) from users"); n != 1 {
		t.Fatalf("%d users, want 1", n)
	}
	if n := len(outbox(t, pool)); n != 1 {
		t.Fatalf("%d audit records, want only the first creation's", n)
	}

	// The database enforces it too, for a row another writer stored in
	// mixed case.
	if _, err := pool.Exec(ctx, "insert into users (email, display_name) values ('Ops@Example.com', 'ops')"); err != nil {
		t.Fatal(err)
	}
	_, err = users.CreateAdmin(ctx, pool, hasher(), users.NewAdmin{Email: "ops@example.com", Password: secret}, actor, "req-3")
	if !errors.Is(err, users.ErrEmailTaken) {
		t.Fatalf("CreateAdmin over a mixed-case row = %v, want ErrEmailTaken", err)
	}
}

// The user and its record are one transaction: when the record cannot be
// written, the user is not created either.
func TestCreateAdminWritesNothingWithoutItsRecord(t *testing.T) {
	t.Parallel()
	_, _, pool := testenv.MigratedPostgres(t)
	_, err := users.CreateAdmin(context.Background(), pool, hasher(), users.NewAdmin{Email: "admin@example.com", Password: secret}, audit.Actor{}, "req-1")
	if err == nil {
		t.Fatal("CreateAdmin succeeded with an actor the record cannot hold")
	}
	if n := count(t, pool, "select count(*) from users"); n != 0 {
		t.Fatalf("%d users created without an audit record", n)
	}
	if n := count(t, pool, "select count(*) from user_roles"); n != 0 {
		t.Fatalf("%d roles granted without an audit record", n)
	}
}
