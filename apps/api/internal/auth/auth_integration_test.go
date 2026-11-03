//go:build integration

package auth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	goredis "github.com/redis/go-redis/v9"

	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/audit/audittest"
	"heartbeat/apps/api/internal/auth"
	"heartbeat/apps/api/internal/password"
	"heartbeat/apps/api/internal/store"
	"heartbeat/apps/api/internal/testenv"
)

// cheap are argon2id parameters fast enough for the tests that are not
// about a login's cost.
var cheap = password.Params{Time: 1, MemoryKiB: 8 * 1024, Threads: 1, SaltBytes: 16, KeyBytes: 32}

const secret = "correct horse battery staple, wallboard"

var client = auth.Client{Address: netip.MustParseAddr("192.0.2.10"), RequestID: "req-login"}

// fixture is a login service on its own PostgreSQL and Valkey.
type fixture struct {
	pool     *pgxpool.Pool
	redis    *goredis.Client
	hasher   *password.Hasher
	sessions *auth.Sessions
	attempts *prometheus.CounterVec
	service  *auth.Service
}

func newFixture(t *testing.T, params password.Params) *fixture {
	t.Helper()
	_, _, pool := testenv.MigratedPostgres(t)
	_, client := testenv.ValkeyClient(t)
	f := &fixture{
		pool:     pool,
		redis:    client,
		hasher:   password.NewHasher(params, 2),
		sessions: &auth.Sessions{Redis: client, IdleTimeout: auth.DefaultIdleTimeout, MaxAge: auth.DefaultMaxAge},
		attempts: auth.NewAttemptsCounter(),
	}
	f.service = auth.NewService(pool, f.hasher, f.sessions, &auth.Limits{Redis: client}, f.attempts, nil)
	return f
}

// addUser creates an active user with secret's hash, by hasher, and roles,
// and returns its ID. An empty secret leaves the user without a password.
func (f *fixture) addUser(t *testing.T, hasher *password.Hasher, email, secret string, roles ...string) string {
	t.Helper()
	ctx := context.Background()
	var hash *string
	if secret != "" {
		h, err := hasher.Hash(ctx, secret)
		if err != nil {
			t.Fatal(err)
		}
		hash = &h
	}
	var id string
	err := f.pool.QueryRow(ctx, "insert into users (email, display_name, password_hash) values ($1, $1, $2) returning id::text", email, hash).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range roles {
		f.exec(t, "insert into user_roles (user_id, role_id) select $1, id from roles where name = $2", id, role)
	}
	return id
}

func (f *fixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) hash(t *testing.T, userID string) string {
	t.Helper()
	var hash string
	if err := f.pool.QueryRow(context.Background(), "select coalesce(password_hash, '') from users where id = $1", userID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	return hash
}

// records returns the outbox's records of action, oldest first.
func (f *fixture) records(t *testing.T, action string) []audit.Record {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), "select record::text from audit_outbox where record->>'action' = $1 order by seq", action)
	if err != nil {
		t.Fatal(err)
	}
	texts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	records := make([]audit.Record, len(texts))
	for i, text := range texts {
		audittest.Validate(t, []byte(text))
		if err := json.Unmarshal([]byte(text), &records[i]); err != nil {
			t.Fatal(err)
		}
	}
	return records
}

// sessionKeys returns the Redis keys of every session.
func (f *fixture) sessionKeys(t *testing.T) []string {
	t.Helper()
	keys, err := f.redis.Keys(context.Background(), "heartbeat:session:*").Result()
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

// keyOf is the Redis key of token's session: the SHA-256 of the token's
// bytes, never the token.
func keyOf(t *testing.T, token string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 32 {
		t.Fatalf("token %q: want 256 bits, base64url-encoded without padding (%v)", token, err)
	}
	sum := sha256.Sum256(raw)
	return "heartbeat:session:" + hex.EncodeToString(sum[:])
}

// null reports a record's before or after that holds no state.
func null(object json.RawMessage) bool { return object == nil || string(object) == "null" }

func field(t *testing.T, object json.RawMessage, name string) string {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal(object, &fields); err != nil {
		t.Fatalf("%s: %v", object, err)
	}
	value, _ := fields[name].(string)
	return value
}

// A login checks the password, starts a session in Redis under the hash of
// its token, records the login time, and audits it with the session's ID,
// never the token, the password or the hash.
func TestLoginStartsAnAuditedSession(t *testing.T) {
	t.Parallel()
	f := newFixture(t, cheap)
	ctx := context.Background()
	userID := f.addUser(t, f.hasher, "wall@example.com", secret, "viewer")

	principal, token, err := f.service.Login(ctx, " Wall@Example.COM ", secret, client)
	if err != nil {
		t.Fatal(err)
	}
	if principal.User.ID != userID || principal.User.Email != "wall@example.com" || !slices.Equal(principal.User.Roles, []string{"viewer"}) {
		t.Fatalf("user %+v", principal.User)
	}
	if principal.Session.UserID != userID || principal.Session.Address != "192.0.2.10" || principal.Session.ID == "" {
		t.Fatalf("session %+v", principal.Session)
	}
	if keys := f.sessionKeys(t); !slices.Equal(keys, []string{keyOf(t, token)}) {
		t.Fatalf("session keys %v, want the token's hash alone", keys)
	}
	var lastLogin *time.Time
	if err := f.pool.QueryRow(ctx, "select last_login_at from users where id = $1", userID).Scan(&lastLogin); err != nil {
		t.Fatal(err)
	}
	if lastLogin == nil || time.Since(*lastLogin) > time.Minute {
		t.Fatalf("last_login_at = %v, want now", lastLogin)
	}

	records := f.records(t, audit.ActionLogin)
	if len(records) != 1 {
		t.Fatalf("%d login records, want 1", len(records))
	}
	r := records[0]
	if r.EntityType != audit.EntitySession || r.EntityID != principal.Session.ID || r.RequestID != "req-login" ||
		r.Actor != audit.UserActor(userID, "wall@example.com") || !null(r.Before) {
		t.Fatalf("record %+v", r)
	}
	if field(t, r.After, "id") != principal.Session.ID || field(t, r.After, "address") != "192.0.2.10" || field(t, r.After, "expires_at") == "" {
		t.Fatalf("record's after %s", r.After)
	}
	encoded, _ := json.Marshal(r)
	audittest.NoSecrets(t, encoded, secret, token, f.hash(t, userID), strings.TrimPrefix(keyOf(t, token), "heartbeat:session:"))
	if got := testutil.ToFloat64(f.attempts.WithLabelValues("succeeded")); got != 1 {
		t.Fatalf("succeeded logins = %v, want 1", got)
	}

	if p, err := f.service.Authenticate(ctx, token); err != nil || p.Session.ID != principal.Session.ID {
		t.Fatalf("Authenticate(token) = %+v, %v; want the session", p, err)
	}
}

// Every refused login gets the same error, starts no session, counts
// against the limits, and is audited with the email tried and why, never
// the password.
func TestLoginFailuresAreAlikeAndAudited(t *testing.T) {
	t.Parallel()
	f := newFixture(t, cheap)
	ctx := context.Background()
	active := f.addUser(t, f.hasher, "active@example.com", secret, "viewer")
	disabled := f.addUser(t, f.hasher, "disabled@example.com", secret, "viewer")
	f.exec(t, "update users set status = 'disabled' where id = $1", disabled)
	noPassword := f.addUser(t, f.hasher, "nopassword@example.com", "", "viewer")
	unreadable := f.addUser(t, f.hasher, "unreadable@example.com", secret, "viewer")
	f.exec(t, "update users set password_hash = '$argon2id$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA' where id = $1", unreadable)

	for i, tc := range []struct {
		email, secret, userID, reason string
	}{
		{"active@example.com", secret + "!", active, auth.ReasonWrongPassword},
		{"nobody@example.com", secret, "", auth.ReasonUnknownEmail},
		{"disabled@example.com", secret, disabled, auth.ReasonDisabled},
		{"nopassword@example.com", secret, noPassword, auth.ReasonNoPassword},
		{"unreadable@example.com", secret, unreadable, auth.ReasonUnreadableHash},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			address := netip.AddrFrom4([4]byte{198, 51, 100, byte(i + 1)})
			_, token, err := f.service.Login(ctx, tc.email, tc.secret, auth.Client{Address: address, RequestID: "req-" + tc.reason})
			if !errors.Is(err, auth.ErrInvalidCredentials) || token != "" {
				t.Fatalf("Login = %q, %v; want ErrInvalidCredentials", token, err)
			}
			records := f.records(t, audit.ActionLoginFailed)
			r := records[len(records)-1]
			if r.RequestID != "req-"+tc.reason || r.Actor != audit.ClientActor(address.String()) || r.EntityType != audit.EntitySession || !null(r.Before) {
				t.Fatalf("record %+v", r)
			}
			if field(t, r.After, "email") != tc.email || field(t, r.After, "reason") != tc.reason || field(t, r.After, "user_id") != tc.userID {
				t.Fatalf("record's after %s", r.After)
			}
			encoded, _ := json.Marshal(r)
			hash := ""
			if tc.userID != "" {
				hash = f.hash(t, tc.userID)
			}
			audittest.NoSecrets(t, encoded, tc.secret, hash)
		})
	}
	if keys := f.sessionKeys(t); len(keys) != 0 {
		t.Fatalf("failed logins left sessions %v", keys)
	}
	if got := testutil.ToFloat64(f.attempts.WithLabelValues("failed")); got != 5 {
		t.Fatalf("failed logins = %v, want 5", got)
	}
	if n := len(f.records(t, audit.ActionLogin)); n != 0 {
		t.Fatalf("%d login records after failures alone", n)
	}
}

// A request that cannot be checked is refused before the limits, the
// database and the audit trail see it.
func TestLoginRefusesRequestsItCannotCheck(t *testing.T) {
	t.Parallel()
	f := newFixture(t, cheap)
	ctx := context.Background()
	for name, tc := range map[string]struct{ email, secret string }{
		"not an address":    {"wallboard", secret},
		"no password":       {"wall@example.com", ""},
		"password too long": {"wall@example.com", strings.Repeat("x", password.MaxBytes+1)},
		"not UTF-8":         {"wall@example.com", "\xff\xfe" + secret},
	} {
		t.Run(name, func(t *testing.T) {
			var invalid *auth.InvalidError
			if _, _, err := f.service.Login(ctx, tc.email, tc.secret, client); !errors.As(err, &invalid) {
				t.Fatalf("Login = %v, want an InvalidError", err)
			}
		})
	}
	if n := len(f.records(t, audit.ActionLoginFailed)); n != 0 {
		t.Fatalf("%d failed-login records for requests that were never checked", n)
	}
	if keys, _ := f.redis.Keys(ctx, "heartbeat:login-failures:*").Result(); len(keys) != 0 {
		t.Fatalf("requests that were never checked counted against the limits: %v", keys)
	}
}

// Past AccountFailures failures, even the right password is refused,
// unchecked and unaudited, until the window ends; other accounts from the
// same address still log in.
func TestLoginIsRateLimitedPerAccount(t *testing.T) {
	t.Parallel()
	f := newFixture(t, cheap)
	ctx := context.Background()
	f.addUser(t, f.hasher, "target@example.com", secret, "viewer")
	f.addUser(t, f.hasher, "other@example.com", secret, "viewer")

	for i := range auth.AccountFailures {
		// From many addresses: the account's limit spans them.
		from := auth.Client{Address: netip.AddrFrom4([4]byte{203, 0, 113, byte(i)}), RequestID: "req-guess"}
		if _, _, err := f.service.Login(ctx, "target@example.com", "guess", from); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("guess %d: %v", i+1, err)
		}
	}
	_, token, err := f.service.Login(ctx, "target@example.com", secret, client)
	var limited *auth.RateLimitedError
	if !errors.As(err, &limited) || token != "" {
		t.Fatalf("Login with the right password after %d failures = %v, want a RateLimitedError", auth.AccountFailures, err)
	}
	if limited.RetryAfter <= auth.FailureWindow-time.Minute || limited.RetryAfter > auth.FailureWindow {
		t.Fatalf("RetryAfter = %s, want about %s", limited.RetryAfter, auth.FailureWindow)
	}
	if n := len(f.records(t, audit.ActionLoginFailed)); n != auth.AccountFailures {
		t.Fatalf("%d failed-login records, want %d: a refused login is not checked", n, auth.AccountFailures)
	}
	if got := testutil.ToFloat64(f.attempts.WithLabelValues("rate_limited")); got != 1 {
		t.Fatalf("rate_limited logins = %v, want 1", got)
	}
	if _, _, err := f.service.Login(ctx, "other@example.com", secret, client); err != nil {
		t.Fatalf("another account from the same address: %v", err)
	}
}

// A successful login clears the account's failures.
func TestLoginClearsTheAccountsFailures(t *testing.T) {
	t.Parallel()
	f := newFixture(t, cheap)
	ctx := context.Background()
	f.addUser(t, f.hasher, "wall@example.com", secret, "viewer")
	for round := range 2 {
		for range auth.AccountFailures - 1 {
			if _, _, err := f.service.Login(ctx, "wall@example.com", "guess", client); !errors.Is(err, auth.ErrInvalidCredentials) {
				t.Fatalf("round %d: %v", round, err)
			}
		}
		if _, _, err := f.service.Login(ctx, "wall@example.com", secret, client); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
}

// A hash made with other parameters is replaced at the next login, and the
// new one works; a password changed since the login read the hash is kept.
func TestLoginUpgradesOldHashes(t *testing.T) {
	t.Parallel()
	f := newFixture(t, cheap)
	ctx := context.Background()
	old := password.NewHasher(password.Params{Time: 2, MemoryKiB: 4 * 1024, Threads: 1, SaltBytes: 16, KeyBytes: 32}, 1)
	userID := f.addUser(t, old, "wall@example.com", secret, "viewer")
	oldHash := f.hash(t, userID)

	if _, _, err := f.service.Login(ctx, "wall@example.com", secret, client); err != nil {
		t.Fatal(err)
	}
	newHash := f.hash(t, userID)
	if !strings.HasPrefix(newHash, "$argon2id$v=19$m=8192,t=1,p=1$") {
		t.Fatalf("hash after login %q, want the current parameters", newHash)
	}
	if _, _, err := f.service.Login(ctx, "wall@example.com", secret, client); err != nil {
		t.Fatalf("login with the upgraded hash: %v", err)
	}
	if f.hash(t, userID) != newHash {
		t.Fatal("a current hash was replaced")
	}
	for _, r := range f.records(t, audit.ActionLogin) {
		encoded, _ := json.Marshal(r)
		audittest.NoSecrets(t, encoded, secret, oldHash, newHash)
	}

	// The upgrade replaces only the hash the login checked.
	var id pgtype.UUID
	if err := id.Scan(userID); err != nil {
		t.Fatal(err)
	}
	n, err := store.New(f.pool).UpgradePasswordHash(ctx, store.UpgradePasswordHashParams{
		ID: id, OldHash: pgtype.Text{String: oldHash, Valid: true}, NewHash: pgtype.Text{String: "changed meanwhile", Valid: true},
	})
	if err != nil || n != 0 || f.hash(t, userID) != newHash {
		t.Fatalf("UpgradePasswordHash over a changed hash = %d, %v; want it kept", n, err)
	}
}

// Authenticate reads the user on every request: a role granted after the
// login applies at once, and a disabled user's session ends and is deleted.
func TestAuthenticateFollowsTheUser(t *testing.T) {
	t.Parallel()
	f := newFixture(t, cheap)
	ctx := context.Background()
	userID := f.addUser(t, f.hasher, "wall@example.com", secret, "viewer")
	_, token, err := f.service.Login(ctx, "wall@example.com", secret, client)
	if err != nil {
		t.Fatal(err)
	}

	f.exec(t, "insert into user_roles (user_id, role_id) select $1, id from roles where name = 'operator'", userID)
	p, err := f.service.Authenticate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.User.Roles, []string{"operator", "viewer"}) {
		t.Fatalf("roles %v, want the granted one too", p.User.Roles)
	}

	f.exec(t, "update users set status = 'disabled' where id = $1", userID)
	if _, err := f.service.Authenticate(ctx, token); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("Authenticate after disabling = %v, want ErrNoSession", err)
	}
	if keys := f.sessionKeys(t); len(keys) != 0 {
		t.Fatalf("the disabled user's session is still in Redis: %v", keys)
	}
	if members, _ := f.redis.SMembers(ctx, "heartbeat:user-sessions:"+userID).Result(); len(members) != 0 {
		t.Fatalf("the disabled user's session is still listed: %v", members)
	}

	for _, bad := range []string{"", "x", token + "x", strings.Repeat("A", 43)} {
		if _, err := f.service.Authenticate(ctx, bad); !errors.Is(err, auth.ErrNoSession) {
			t.Fatalf("Authenticate(%q) = %v, want ErrNoSession", bad, err)
		}
	}
}

// Logout deletes the session and audits it; if the session cannot be
// deleted, nothing is recorded.
func TestLogout(t *testing.T) {
	t.Parallel()
	f := newFixture(t, cheap)
	ctx := context.Background()
	userID := f.addUser(t, f.hasher, "wall@example.com", secret, "viewer")
	principal, token, err := f.service.Login(ctx, "wall@example.com", secret, client)
	if err != nil {
		t.Fatal(err)
	}

	// Redis gone: the logout fails and writes no record.
	down := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
	_ = down.Close()
	broken := auth.NewService(f.pool, f.hasher, &auth.Sessions{Redis: down, IdleTimeout: time.Hour, MaxAge: time.Hour}, &auth.Limits{Redis: down}, nil, nil)
	if err := broken.Logout(ctx, principal, client); err == nil {
		t.Fatal("Logout without Redis succeeded")
	}
	if n := len(f.records(t, audit.ActionLogout)); n != 0 {
		t.Fatalf("%d logout records for a logout that failed", n)
	}

	out := auth.Client{Address: client.Address, RequestID: "req-logout"}
	if err := f.service.Logout(ctx, principal, out); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Authenticate(ctx, token); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("Authenticate after logout = %v, want ErrNoSession", err)
	}
	if keys := f.sessionKeys(t); len(keys) != 0 {
		t.Fatalf("session keys after logout: %v", keys)
	}
	records := f.records(t, audit.ActionLogout)
	if len(records) != 1 {
		t.Fatalf("%d logout records, want 1", len(records))
	}
	r := records[0]
	if r.RequestID != "req-logout" || r.EntityID != principal.Session.ID || r.Actor != audit.UserActor(userID, "wall@example.com") ||
		!null(r.After) || field(t, r.Before, "id") != principal.Session.ID {
		t.Fatalf("record %+v", r)
	}
	encoded, _ := json.Marshal(r)
	audittest.NoSecrets(t, encoded, secret, token, f.hash(t, userID))
}

// commitFails is a pool whose transactions fail to commit while fail is
// set, as when PostgreSQL goes away between the last statement and the
// commit.
type commitFails struct {
	*pgxpool.Pool
	fail bool
}

func (c *commitFails) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := c.Pool.Begin(ctx)
	if err != nil || !c.fail {
		return tx, err
	}
	return failingTx{tx}, nil
}

type failingTx struct{ pgx.Tx }

func (tx failingTx) Commit(ctx context.Context) error {
	_ = tx.Tx.Rollback(ctx)
	return errors.New("commit: connection reset by peer")
}

// The session is created in Redis before the login's transaction commits;
// if the commit fails, the session is deleted again and no token is
// returned, so no session exists without its audit record.
func TestLoginDeletesTheSessionWhenTheCommitFails(t *testing.T) {
	t.Parallel()
	f := newFixture(t, cheap)
	ctx := context.Background()
	userID := f.addUser(t, f.hasher, "wall@example.com", secret, "viewer")
	db := &commitFails{Pool: f.pool, fail: true}
	service := auth.NewService(db, f.hasher, f.sessions, &auth.Limits{Redis: f.redis}, f.attempts, nil)

	principal, token, err := service.Login(ctx, "wall@example.com", secret, client)
	if err == nil || token != "" || principal.Session.ID != "" {
		t.Fatalf("Login with a failing commit = %+v, %q, %v; want an error and no session", principal, token, err)
	}
	if keys := f.sessionKeys(t); len(keys) != 0 {
		t.Fatalf("the session outlived its failed commit: %v", keys)
	}
	if members, _ := f.redis.SMembers(ctx, "heartbeat:user-sessions:"+userID).Result(); len(members) != 0 {
		t.Fatalf("the user still lists the session: %v", members)
	}
	if n := len(f.records(t, audit.ActionLogin)); n != 0 {
		t.Fatalf("%d login records after a rolled-back login", n)
	}
	var lastLogin *time.Time
	if err := f.pool.QueryRow(ctx, "select last_login_at from users where id = $1", userID).Scan(&lastLogin); err != nil || lastLogin != nil {
		t.Fatalf("last_login_at = %v (%v), want none", lastLogin, err)
	}
	if got := testutil.ToFloat64(f.attempts.WithLabelValues("succeeded")); got != 0 {
		t.Fatalf("succeeded logins = %v, want 0", got)
	}

	db.fail = false
	if _, _, err := service.Login(ctx, "wall@example.com", secret, client); err != nil {
		t.Fatalf("Login once commits work: %v", err)
	}
}

// An unknown email, a user without a password and a disabled user take as
// long to refuse as a wrong password: each checks a hash, so the time
// tells nothing about which accounts exist.
//
// A shared runner's load comes and goes, so one path timed after another
// can look several times slower (444ms against 93ms once in CI, for a
// disabled user and a wrong password, which run the same code). So the
// test runs alone, not beside its package's other tests, which hash too;
// it warms every path up, times them in turns, round after round, and
// compares their medians. A refusal that skipped the hash takes a
// hundredth of the time, far outside the factor of 2 allowed either way.
func TestFailedLoginsTakeAsLongAsAWrongPassword(t *testing.T) {
	f := newFixture(t, password.DefaultParams)
	ctx := context.Background()
	f.addUser(t, f.hasher, "active@example.com", secret, "viewer")
	disabled := f.addUser(t, f.hasher, "disabled@example.com", secret, "viewer")
	f.exec(t, "update users set status = 'disabled' where id = $1", disabled)
	f.addUser(t, f.hasher, "nopassword@example.com", "", "viewer")

	paths := []struct{ name, email, secret string }{
		{"wrong password", "active@example.com", "guess"},
		{"unknown email", "nobody@example.com", secret},
		{"no password", "nopassword@example.com", secret},
		{"disabled user", "disabled@example.com", secret},
	}
	login := func(email, secret string) time.Duration {
		t.Helper()
		start := time.Now()
		if _, _, err := f.service.Login(ctx, email, secret, client); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("%s: %v", email, err)
		}
		return time.Since(start)
	}
	// The decoy hash is made at the first login that needs it, and the
	// connections open at the first ones.
	for _, p := range paths {
		login(p.email, p.secret)
	}
	// Within the limits: 8 failures per account, 32 from the address.
	const rounds = 7
	took := make([][]time.Duration, len(paths))
	for round := range rounds {
		// Each round starts at another path, so none is always timed first.
		for i := range paths {
			j := (round + i) % len(paths)
			took[j] = append(took[j], login(paths[j].email, paths[j].secret))
		}
	}
	medians := make([]time.Duration, len(paths))
	for i := range paths {
		slices.Sort(took[i])
		medians[i] = took[i][rounds/2]
	}
	wrong := medians[0]
	for i, p := range paths {
		ratio := float64(medians[i]) / float64(wrong)
		t.Logf("%s: median %s, %.2f× a wrong password's", p.name, medians[i].Round(time.Microsecond), ratio)
		if ratio < 0.5 || ratio > 2 {
			t.Errorf("%s: refused in %s (median of %d), a wrong password in %s: want within a factor of 2", p.name, medians[i], rounds, wrong)
		}
	}
}

// Sessions live under the SHA-256 of their token, expire in Redis at the
// earlier of their idle and absolute deadlines, move the idle one at most
// once a minute, and end together per user.
func TestSessions(t *testing.T) {
	t.Parallel()
	_, client := testenv.ValkeyClient(t)
	ctx := context.Background()
	start := time.Now().Truncate(time.Second)
	now := start
	sessions := &auth.Sessions{Redis: client, IdleTimeout: time.Hour, MaxAge: 2 * time.Hour, Now: func() time.Time { return now }}
	expireTime := func(key string) time.Time {
		t.Helper()
		d, err := client.ExpireTime(ctx, key).Result()
		if err != nil {
			t.Fatal(err)
		}
		return time.Unix(int64(d/time.Second), 0)
	}

	session, token, err := sessions.Create(ctx, "user-1", "192.0.2.10")
	if err != nil {
		t.Fatal(err)
	}
	key := keyOf(t, token)
	if got := expireTime(key); !got.Equal(start.Add(time.Hour)) {
		t.Fatalf("expires in Redis at %s, want the idle deadline %s", got, start.Add(time.Hour))
	}
	keys, _ := client.Keys(ctx, "*").Result()
	for _, k := range keys {
		value, _ := client.Dump(ctx, k).Result()
		if strings.Contains(k, token) || strings.Contains(value, token) {
			t.Fatalf("Redis holds the token under %s", k)
		}
	}
	if members, _ := client.SMembers(ctx, "heartbeat:user-sessions:user-1").Result(); !slices.Equal(members, []string{strings.TrimPrefix(key, "heartbeat:session:")}) {
		t.Fatalf("user-1's sessions %v", members)
	}

	// Within a minute, the session is not written again.
	now = start.Add(59 * time.Second)
	if got, err := sessions.Lookup(ctx, token); err != nil || !got.LastSeen.Equal(session.LastSeen) {
		t.Fatalf("Lookup = %+v, %v", got, err)
	}
	// After it, the idle deadline moves.
	now = start.Add(50 * time.Minute)
	if got, err := sessions.Lookup(ctx, token); err != nil || !got.LastSeen.Equal(now) {
		t.Fatalf("Lookup = %+v, %v; want last seen now", got, err)
	}
	if got := expireTime(key); !got.Equal(now.Add(time.Hour)) {
		t.Fatalf("expires in Redis at %s, want %s", got, now.Add(time.Hour))
	}
	// Up to the absolute deadline, however active.
	now = start.Add(100 * time.Minute)
	if _, err := sessions.Lookup(ctx, token); err != nil {
		t.Fatal(err)
	}
	if got := expireTime(key); !got.Equal(start.Add(2 * time.Hour)) {
		t.Fatalf("expires in Redis at %s, want the absolute deadline %s", got, start.Add(2*time.Hour))
	}
	now = start.Add(2 * time.Hour)
	if _, err := sessions.Lookup(ctx, token); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("Lookup at the absolute deadline = %v, want ErrNoSession", err)
	}

	// Idle past the timeout.
	now = start
	idle, idleToken, err := sessions.Create(ctx, "user-1", "192.0.2.10")
	if err != nil {
		t.Fatal(err)
	}
	now = start.Add(time.Hour)
	if _, err := sessions.Lookup(ctx, idleToken); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("Lookup after the idle timeout = %v, want ErrNoSession", err)
	}

	// Delete ends one; DeleteUser ends every session of one user.
	now = start
	if err := sessions.Delete(ctx, idle); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Lookup(ctx, idleToken); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("Lookup after Delete = %v, want ErrNoSession", err)
	}
	var tokens []string
	for _, user := range []string{"user-2", "user-2", "user-3"} {
		_, token, err := sessions.Create(ctx, user, "192.0.2.10")
		if err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, token)
	}
	if err := sessions.DeleteUser(ctx, "user-2"); err != nil {
		t.Fatal(err)
	}
	for i, token := range tokens {
		_, err := sessions.Lookup(ctx, token)
		if want := i == 2; (err == nil) != want {
			t.Fatalf("session %d after DeleteUser(user-2): %v", i, err)
		}
	}
	if n, _ := client.Exists(ctx, "heartbeat:user-sessions:user-2").Result(); n != 0 {
		t.Fatal("user-2 still lists sessions")
	}
}

// Failures count per account and per address block, in a window that
// starts at the first failure; a success clears the account's, not the
// address's; Redis holds no email.
func TestLimits(t *testing.T) {
	t.Parallel()
	_, client := testenv.ValkeyClient(t)
	ctx := context.Background()
	limits := &auth.Limits{Redis: client}
	fail := func(email, address string, n int) {
		t.Helper()
		for range n {
			if err := limits.Failed(ctx, email, netip.MustParseAddr(address)); err != nil {
				t.Fatal(err)
			}
		}
	}
	wait := func(email, address string) time.Duration {
		t.Helper()
		d, err := limits.Wait(ctx, email, netip.MustParseAddr(address))
		if err != nil {
			t.Fatal(err)
		}
		return d
	}

	// Per account, across addresses.
	fail("a@example.com", "192.0.2.1", auth.AccountFailures-1)
	if d := wait("a@example.com", "192.0.2.2"); d != 0 {
		t.Fatalf("below the account limit: wait %s", d)
	}
	fail("a@example.com", "192.0.2.3", 1)
	if d := wait("a@example.com", "192.0.2.4"); d <= 0 || d > auth.FailureWindow {
		t.Fatalf("at the account limit: wait %s, want up to %s", d, auth.FailureWindow)
	}
	if err := limits.Succeeded(ctx, "a@example.com"); err != nil {
		t.Fatal(err)
	}
	if d := wait("a@example.com", "192.0.2.4"); d != 0 {
		t.Fatalf("after a success: wait %s", d)
	}

	// Per address: an IPv6 address counts as its /64, and a mapped IPv4
	// address as itself.
	for i := range auth.AddressFailures {
		address := "2001:db8::1"
		if i%2 == 1 {
			address = "2001:db8::ffff:2"
		}
		fail("user"+strings.Repeat("x", i)+"@example.com", address, 1)
	}
	if d := wait("new@example.com", "2001:db8::3"); d <= 0 {
		t.Fatal("the /64 is past the address limit, but a new address in it may log in")
	}
	if d := wait("new@example.com", "2001:db8:0:1::1"); d != 0 {
		t.Fatalf("another /64: wait %s", d)
	}
	if err := limits.Succeeded(ctx, "user@example.com"); err != nil {
		t.Fatal(err)
	}
	if d := wait("new@example.com", "2001:db8::3"); d <= 0 {
		t.Fatal("a success cleared the address's failures")
	}
	fail("b@example.com", "::ffff:198.51.100.7", auth.AddressFailures)
	if d := wait("c@example.com", "198.51.100.7"); d <= 0 {
		t.Fatal("a mapped IPv4 address is counted apart from itself")
	}

	// The window starts at the first failure: later ones do not extend it.
	fail("d@example.com", "192.0.2.9", 1)
	keys, _ := client.Keys(ctx, "heartbeat:login-failures:address:192.0.2.9").Result()
	if len(keys) != 1 {
		t.Fatalf("address keys %v", keys)
	}
	if err := client.PExpire(ctx, keys[0], 5*time.Second).Err(); err != nil {
		t.Fatal(err)
	}
	fail("d@example.com", "192.0.2.9", 1)
	if ttl, _ := client.PTTL(ctx, keys[0]).Result(); ttl > 5*time.Second {
		t.Fatalf("a later failure extended the window to %s", ttl)
	}

	all, _ := client.Keys(ctx, "*").Result()
	for _, key := range all {
		if strings.Contains(key, "@") {
			t.Fatalf("Redis key %q holds an email", key)
		}
	}
}
