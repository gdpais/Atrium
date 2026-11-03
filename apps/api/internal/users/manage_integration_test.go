//go:build integration

package users_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/audit/audittest"
	"heartbeat/apps/api/internal/password"
	"heartbeat/apps/api/internal/testenv"
	"heartbeat/apps/api/internal/users"
)

// cheap are argon2id parameters fast enough for these tests.
var cheap = password.Params{Time: 1, MemoryKiB: 8 * 1024, Threads: 1, SaltBytes: 16, KeyBytes: 32}

const pw = "correct horse battery staple, managed"

var admin = users.Caller{Actor: audit.UserActor("0b4e7a52-8f43-4d0e-a1b6-3c9d2e5f7a10", "root@example.com"), RequestID: "req-admin"}

// fakeSessions records whose sessions were ended; err, when set, fails it.
type fakeSessions struct {
	mu    sync.Mutex
	ended []string
	err   error
}

func (f *fakeSessions) DeleteUser(_ context.Context, userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.ended = append(f.ended, userID)
	return nil
}

type fixture struct {
	pool     *pgxpool.Pool
	sessions *fakeSessions
	service  *users.Service
	hasher   *password.Hasher
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	_, _, pool := testenv.MigratedPostgres(t)
	f := &fixture{pool: pool, sessions: &fakeSessions{}, hasher: password.NewHasher(cheap, 2)}
	f.service = users.NewService(pool, f.hasher, f.sessions)
	return f
}

func (f *fixture) create(t *testing.T, email string, roles ...string) users.User {
	t.Helper()
	u, err := f.service.Create(context.Background(), admin, users.NewUser{Email: email, Password: pw, Roles: roles})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func (f *fixture) hash(t *testing.T, id string) string {
	t.Helper()
	var hash string
	if err := f.pool.QueryRow(context.Background(), "select password_hash from users where id = $1", id).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	return hash
}

// records returns the outbox's records of action, as JSON, oldest first;
// each is valid against the schema and holds none of secrets.
func (f *fixture) records(t *testing.T, action string, secrets ...string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, text := range outbox(t, f.pool) {
		var r map[string]any
		if err := json.Unmarshal([]byte(text), &r); err != nil {
			t.Fatal(err)
		}
		if r["action"] != action {
			continue
		}
		audittest.Validate(t, []byte(text))
		audittest.NoSecrets(t, []byte(text), secrets...)
		out = append(out, r)
	}
	return out
}

func roles(r map[string]any, side string) string {
	m, _ := r[side].(map[string]any)
	list, _ := m["roles"].([]any)
	var names []string
	for _, n := range list {
		names = append(names, n.(string))
	}
	return strings.Join(names, ",")
}

// A user is created active, with their email in lower case, the hash of
// their password and their roles, in one transaction with its record.
func TestServiceCreate(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	u, err := f.service.Create(ctx, admin, users.NewUser{Email: " Alice@Example.COM ", DisplayName: "Alice", Password: pw, Roles: []string{"viewer", "operator"}})
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "alice@example.com" || u.DisplayName != "Alice" || u.Status != users.StatusActive || strings.Join(u.Roles, ",") != "operator,viewer" ||
		u.LastLoginAt != nil || u.CreatedAt.IsZero() {
		t.Fatalf("user %+v", u)
	}
	hash := f.hash(t, u.ID)
	if ok, _, err := f.hasher.Verify(ctx, pw, hash); err != nil || !ok {
		t.Fatalf("the stored hash does not check the password: %v", err)
	}
	records := f.records(t, audit.ActionCreate, pw, hash)
	if len(records) != 1 || records[0]["entity_id"] != u.ID || records[0]["request_id"] != "req-admin" || roles(records[0], "after") != "operator,viewer" {
		t.Fatalf("records %v", records)
	}

	before := count(t, f.pool, "select count(*) from users")
	for name, in := range map[string]users.NewUser{
		"email taken, other case": {Email: "ALICE@example.com", Password: pw, Roles: []string{"viewer"}},
		"unknown role":            {Email: "bob@example.com", Password: pw, Roles: []string{"viewer", "owner"}},
		"short password":          {Email: "bob@example.com", Password: "short", Roles: []string{"viewer"}},
		"no roles":                {Email: "bob@example.com", Password: pw},
		"not an address":          {Email: "bob", Password: pw, Roles: []string{"viewer"}},
	} {
		_, err := f.service.Create(ctx, admin, in)
		var invalid *users.InvalidError
		switch name {
		case "email taken, other case":
			if !errors.Is(err, users.ErrEmailTaken) {
				t.Errorf("%s: %v, want ErrEmailTaken", name, err)
			}
		case "unknown role":
			if !errors.Is(err, users.ErrUnknownRole) {
				t.Errorf("%s: %v, want ErrUnknownRole", name, err)
			}
		default:
			if !errors.As(err, &invalid) {
				t.Errorf("%s: %v, want an InvalidError", name, err)
			}
		}
	}
	if after := count(t, f.pool, "select count(*) from users"); after != before {
		t.Fatalf("refused creations left %d users", after-before)
	}
	if n := len(f.records(t, audit.ActionCreate)); n != 1 {
		t.Fatalf("%d create records, want 1", n)
	}
}

func TestServiceListAndGet(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	bob := f.create(t, "bob@example.com", "viewer")
	alice := f.create(t, "alice@example.com", "admin")
	if _, err := f.service.Update(ctx, admin, uuid.MustParse(bob.ID), users.Change{Status: ptr(users.StatusDisabled)}); err != nil {
		t.Fatal(err)
	}
	for status, want := range map[string]string{"": "alice@example.com,bob@example.com", "active": "alice@example.com", "disabled": "bob@example.com"} {
		list, err := f.service.List(ctx, status)
		if err != nil {
			t.Fatal(err)
		}
		var emails []string
		for _, u := range list {
			emails = append(emails, u.Email)
		}
		if strings.Join(emails, ",") != want {
			t.Errorf("List(%q) = %v, want %s", status, emails, want)
		}
	}
	if got, err := f.service.Get(ctx, uuid.MustParse(alice.ID)); err != nil || got.Email != "alice@example.com" || strings.Join(got.Roles, ",") != "admin" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if _, err := f.service.Get(ctx, uuid.New()); !errors.Is(err, users.ErrNotFound) {
		t.Fatalf("Get(unknown) = %v, want ErrNotFound", err)
	}
}

// An update changes what it holds and audits before and after; one that
// changes nothing audits nothing; disabling ends the user's sessions, and
// if that fails nothing changes.
func TestServiceUpdate(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	f.create(t, "root@example.com", "admin")
	bob := f.create(t, "bob@example.com", "operator")
	f.create(t, "carol@example.com", "viewer")
	id := uuid.MustParse(bob.ID)

	u, err := f.service.Update(ctx, admin, id, users.Change{Email: ptr(" Robert@Example.com"), DisplayName: ptr(" Robert ")})
	if err != nil || u.Email != "robert@example.com" || u.DisplayName != "Robert" || !u.UpdatedAt.After(bob.UpdatedAt) {
		t.Fatalf("Update = %+v, %v", u, err)
	}
	records := f.records(t, users.ActionUpdate)
	if len(records) != 1 || records[0]["before"].(map[string]any)["email"] != "bob@example.com" || records[0]["after"].(map[string]any)["email"] != "robert@example.com" {
		t.Fatalf("records %v", records)
	}
	if _, err := f.service.Update(ctx, admin, id, users.Change{Email: ptr("robert@example.com"), Status: ptr(users.StatusActive)}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.records(t, users.ActionUpdate)); n != 1 {
		t.Fatalf("a change that changed nothing was audited: %d records", n)
	}
	if _, err := f.service.Update(ctx, admin, id, users.Change{Email: ptr("CAROL@example.com")}); !errors.Is(err, users.ErrEmailTaken) {
		t.Fatalf("Update to another's email = %v, want ErrEmailTaken", err)
	}
	var invalid *users.InvalidError
	for _, c := range []users.Change{{DisplayName: ptr("  ")}, {Status: ptr("gone")}, {Email: ptr("robert")}} {
		if _, err := f.service.Update(ctx, admin, id, c); !errors.As(err, &invalid) {
			t.Fatalf("Update(%+v) = %v, want an InvalidError", c, err)
		}
	}
	if _, err := f.service.Update(ctx, admin, uuid.New(), users.Change{Status: ptr(users.StatusDisabled)}); !errors.Is(err, users.ErrNotFound) {
		t.Fatalf("Update(unknown) = %v, want ErrNotFound", err)
	}

	// Redis fails: the user stays active, unaudited.
	f.sessions.err = errors.New("redis: connection refused")
	if _, err := f.service.Update(ctx, admin, id, users.Change{Status: ptr(users.StatusDisabled)}); err == nil {
		t.Fatal("disabling while sessions cannot end succeeded")
	}
	if got, _ := f.service.Get(ctx, id); got.Status != users.StatusActive || len(f.records(t, users.ActionUpdate)) != 1 {
		t.Fatalf("a failed disabling changed the user: %+v", got)
	}
	f.sessions.err = nil
	if u, err := f.service.Update(ctx, admin, id, users.Change{Status: ptr(users.StatusDisabled)}); err != nil || u.Status != users.StatusDisabled {
		t.Fatalf("disable = %+v, %v", u, err)
	}
	if !slices.Equal(f.sessions.ended, []string{bob.ID}) {
		t.Fatalf("sessions ended for %v, want %s", f.sessions.ended, bob.ID)
	}
}

// The last active admin can be neither disabled nor stripped of admin;
// any other admin can.
func TestServiceKeepsAnActiveAdmin(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	a := f.create(t, "a@example.com", "admin")
	b := f.create(t, "b@example.com", "admin", "viewer")
	aID, bID := uuid.MustParse(a.ID), uuid.MustParse(b.ID)

	if _, err := f.service.Update(ctx, admin, bID, users.Change{Status: ptr(users.StatusDisabled)}); err != nil {
		t.Fatal(err)
	}
	// b is disabled: a is the last active admin.
	if _, err := f.service.Update(ctx, admin, aID, users.Change{Status: ptr(users.StatusDisabled)}); !errors.Is(err, users.ErrLastAdmin) {
		t.Fatalf("disable the last active admin = %v, want ErrLastAdmin", err)
	}
	if _, err := f.service.Revoke(ctx, admin, aID, "admin"); !errors.Is(err, users.ErrLastAdmin) {
		t.Fatalf("revoke admin from the last active admin = %v, want ErrLastAdmin", err)
	}
	// Other changes to the last admin go through, and b, disabled, may lose admin.
	if _, err := f.service.Update(ctx, admin, aID, users.Change{DisplayName: ptr("A")}); err != nil {
		t.Fatal(err)
	}
	if u, err := f.service.Revoke(ctx, admin, bID, "admin"); err != nil || strings.Join(u.Roles, ",") != "viewer" {
		t.Fatalf("revoke admin from a disabled admin = %+v, %v", u, err)
	}
	if _, err := f.service.Grant(ctx, admin, bID, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Update(ctx, admin, bID, users.Change{Status: ptr(users.StatusActive)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Update(ctx, admin, aID, users.Change{Status: ptr(users.StatusDisabled)}); err != nil {
		t.Fatalf("disable an admin while another is active: %v", err)
	}
	if got, _ := f.service.Get(ctx, aID); got.Status != users.StatusDisabled {
		t.Fatalf("a is %s", got.Status)
	}
}

// Disabling an admin and revoking admin wait for the admin role's lock, so
// two of them at once cannot both count the other and leave no admin.
func TestServiceSerialisesChangesThatCanLeaveNoAdmin(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	a := f.create(t, "a@example.com", "admin")
	f.create(t, "b@example.com", "admin")

	for name, change := range map[string]func() error{
		"disable": func() error {
			_, err := f.service.Update(ctx, admin, uuid.MustParse(a.ID), users.Change{Status: ptr(users.StatusDisabled)})
			return err
		},
		"revoke": func() error {
			_, err := f.service.Revoke(ctx, admin, uuid.MustParse(a.ID), "admin")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			tx, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			// As another such change takes it (LockAdminRole).
			if _, err := tx.Exec(ctx, "select id from roles where name = 'admin' for no key update"); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- change() }()
			select {
			case err := <-done:
				_ = tx.Rollback(ctx)
				t.Fatalf("the change finished (%v) while another held the admin role's lock", err)
			case <-time.After(300 * time.Millisecond):
			}
			if err := tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			// Undo, for the other subtest.
			if _, err := f.service.Update(ctx, admin, uuid.MustParse(a.ID), users.Change{Status: ptr(users.StatusActive)}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.Grant(ctx, admin, uuid.MustParse(a.ID), "admin"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Setting a password replaces the hash, ends the user's sessions and
// audits that it did, without either hash or the password; a change that
// expected another hash, or whose sessions cannot end, changes nothing.
func TestSetPassword(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	bob := f.create(t, "bob@example.com", "viewer")
	id := uuid.MustParse(bob.ID)
	old := f.hash(t, bob.ID)
	const next = "a brand new horse battery staple"

	if err := users.SetPassword(ctx, f.pool, f.hasher, f.sessions, admin, users.PasswordChange{UserID: id, Password: next, IfHash: "$argon2id$v=19$m=8192,t=1,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNo"}); !errors.Is(err, users.ErrStalePassword) {
		t.Fatalf("SetPassword over another hash = %v, want ErrStalePassword", err)
	}
	f.sessions.err = errors.New("redis: connection refused")
	if err := f.service.SetPassword(ctx, admin, id, next); err == nil {
		t.Fatal("SetPassword while sessions cannot end succeeded")
	}
	f.sessions.err = nil
	if f.hash(t, bob.ID) != old || len(f.records(t, users.ActionSetPassword)) != 0 {
		t.Fatal("a refused password change changed the hash or was audited")
	}

	if err := f.service.SetPassword(ctx, admin, id, next); err != nil {
		t.Fatal(err)
	}
	hash := f.hash(t, bob.ID)
	if ok, _, _ := f.hasher.Verify(ctx, next, hash); !ok || hash == old {
		t.Fatal("the new password does not check against the new hash")
	}
	if !slices.Equal(f.sessions.ended, []string{bob.ID}) {
		t.Fatalf("sessions ended for %v", f.sessions.ended)
	}
	records := f.records(t, users.ActionSetPassword, pw, next, old, hash)
	if len(records) != 1 || records[0]["after"].(map[string]any)["sessions_ended"] != true || records[0]["actor"].(map[string]any)["email"] != "root@example.com" {
		t.Fatalf("records %v", records)
	}
	if err := users.SetPassword(ctx, f.pool, f.hasher, f.sessions, admin, users.PasswordChange{UserID: id, Password: pw, IfHash: hash}); err != nil {
		t.Fatalf("SetPassword over the current hash: %v", err)
	}
	var invalid *users.InvalidError
	if err := f.service.SetPassword(ctx, admin, id, "short"); !errors.As(err, &invalid) {
		t.Fatalf("SetPassword(short) = %v, want an InvalidError", err)
	}
	if err := f.service.SetPassword(ctx, admin, uuid.New(), next); !errors.Is(err, users.ErrNotFound) {
		t.Fatalf("SetPassword(unknown) = %v, want ErrNotFound", err)
	}
}

// Granting a role the user has, or revoking one they lack, changes and
// audits nothing; the others are audited with the roles before and after.
func TestServiceRoles(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	f.create(t, "root@example.com", "admin")
	bob := f.create(t, "bob@example.com", "viewer")
	id := uuid.MustParse(bob.ID)

	for _, step := range []struct {
		grant  bool
		role   string
		want   string
		action string
	}{
		{true, "operator", "operator,viewer", users.ActionGrantRole},
		{true, "operator", "operator,viewer", ""},
		{false, "viewer", "operator", users.ActionRevokeRole},
		{false, "viewer", "operator", ""},
		{true, "admin", "admin,operator", users.ActionGrantRole},
		{false, "admin", "operator", users.ActionRevokeRole},
	} {
		before := len(f.records(t, users.ActionGrantRole)) + len(f.records(t, users.ActionRevokeRole))
		change := f.service.Grant
		if !step.grant {
			change = f.service.Revoke
		}
		u, err := change(ctx, admin, id, step.role)
		if err != nil || strings.Join(u.Roles, ",") != step.want {
			t.Fatalf("grant %t %s = %v, %v; want %s", step.grant, step.role, u.Roles, err, step.want)
		}
		records := append(f.records(t, users.ActionGrantRole), f.records(t, users.ActionRevokeRole)...)
		if step.action == "" {
			if len(records) != before {
				t.Fatalf("grant %t %s changed nothing but was audited", step.grant, step.role)
			}
			continue
		}
		if len(records) != before+1 {
			t.Fatalf("grant %t %s: %d records, want %d", step.grant, step.role, len(records), before+1)
		}
	}
	last := f.records(t, users.ActionRevokeRole)
	if r := last[len(last)-1]; roles(r, "before") != "admin,operator" || roles(r, "after") != "operator" {
		t.Fatalf("revoke record %v", r)
	}
	if _, err := f.service.Grant(ctx, admin, id, "owner"); !errors.Is(err, users.ErrUnknownRole) {
		t.Fatalf("Grant(owner) = %v, want ErrUnknownRole", err)
	}
	if _, err := f.service.Revoke(ctx, admin, uuid.New(), "viewer"); !errors.Is(err, users.ErrNotFound) {
		t.Fatalf("Revoke(unknown user) = %v, want ErrNotFound", err)
	}
}

func ptr[T any](v T) *T { return &v }

// Granting admin while the same user is disabled, or has admin revoked, at
// the same moment cannot deadlock. The grant holds the user's row and its
// new user_roles row needs a key-share lock on the admin role's row (the
// foreign key); the other change holds the admin role's row and waits for
// the user's. The admin role's lock must not exclude key-share locks.
func TestServiceGrantDoesNotDeadlockWithTheAdminLock(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	f.create(t, "root@example.com", "admin")
	for name, other := range map[string]func(id uuid.UUID) error{
		"disable": func(id uuid.UUID) error {
			_, err := f.service.Update(ctx, admin, id, users.Change{Status: ptr(users.StatusDisabled)})
			return err
		},
		"revoke": func(id uuid.UUID) error {
			_, err := f.service.Revoke(ctx, admin, id, "admin")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			u := f.create(t, name+"@example.com", "viewer")
			id := uuid.MustParse(u.ID)
			// Queue the grant first on the user's row, then the other change
			// behind it, holding the admin role's row; then let them go.
			hold, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := hold.Exec(ctx, "select id from users where id = $1 for update", u.ID); err != nil {
				t.Fatal(err)
			}
			grant, change := make(chan error, 1), make(chan error, 1)
			go func() {
				_, err := f.service.Grant(ctx, admin, id, "admin")
				grant <- err
			}()
			time.Sleep(200 * time.Millisecond)
			go func() { change <- other(id) }()
			time.Sleep(200 * time.Millisecond)
			if err := hold.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			for what, done := range map[string]chan error{"grant": grant, name: change} {
				select {
				case err := <-done:
					if err != nil {
						t.Errorf("%s: %v", what, err)
					}
				case <-time.After(30 * time.Second):
					t.Fatalf("%s did not finish", what)
				}
			}
		})
	}
}
