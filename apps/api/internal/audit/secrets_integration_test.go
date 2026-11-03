//go:build integration

package audit_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/netip"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/audit/audittest"
	"heartbeat/apps/api/internal/auth"
	"heartbeat/apps/api/internal/inventory"
	"heartbeat/apps/api/internal/password"
	"heartbeat/apps/api/internal/probedefs"
	"heartbeat/apps/api/internal/testenv"
	"heartbeat/apps/api/internal/users"
)

// harness is what an audited change runs against: PostgreSQL, and Redis
// for the sessions.
type harness struct {
	pool  *pgxpool.Pool
	redis *goredis.Client
}

// sessions is the login service on h, with the default parameters.
func (h harness) sessions() *auth.Service {
	sessions := &auth.Sessions{Redis: h.redis, IdleTimeout: auth.DefaultIdleTimeout, MaxAge: auth.DefaultMaxAge}
	return auth.NewService(h.pool, password.NewHasher(password.DefaultParams, 1), sessions, &auth.Limits{Redis: h.redis}, nil, nil)
}

// createAdmin creates a user who logs in with secret, hashed with params,
// and returns the user's password hash.
func (h harness) createAdmin(t *testing.T, ctx context.Context, params password.Params, email, secret string) string {
	t.Helper()
	created, err := users.CreateAdmin(ctx, h.pool, password.NewHasher(params, 1),
		users.NewAdmin{Email: email, Password: secret}, audit.CLIActor("api users create-admin", "api-7d9f-xk2p"), "req-create-admin")
	if err != nil {
		t.Fatal(err)
	}
	return h.hash(t, ctx, created.UserID)
}

func (h harness) hash(t *testing.T, ctx context.Context, userID string) string {
	t.Helper()
	var hash string
	if err := h.pool.QueryRow(ctx, "select password_hash from users where id = $1", userID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	return hash
}

// login logs email in and returns the session and the secrets it holds:
// the token, as the cookie does, and the hash Redis keys the session by.
func (h harness) login(t *testing.T, ctx context.Context, email, secret string) (auth.Principal, []string) {
	t.Helper()
	principal, token, err := h.sessions().Login(ctx, email, secret, client)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return principal, []string{token, hex.EncodeToString(sum[:])}
}

var client = auth.Client{Address: netip.MustParseAddr("198.51.100.7"), RequestID: "req-session"}

// operator logs a new user in, as the inventory's changes come from one,
// and returns who they are as a change's caller and the secrets of their
// login: none may reach an inventory record through its actor.
func (h harness) operator(t *testing.T, ctx context.Context, email string) (inventory.Caller, []string) {
	t.Helper()
	secret := "correct horse battery staple, " + email
	hash := h.createAdmin(t, ctx, password.DefaultParams, email, secret)
	principal, session := h.login(t, ctx, email, secret)
	return inventory.Caller{Actor: audit.UserActor(principal.User.ID, principal.User.Email), RequestID: "req-inventory"},
		append(session, secret, hash)
}

// weak are argon2id parameters below the current ones: a login upgrades
// a hash made with them.
var weak = password.Params{Time: 1, MemoryKiB: 8 * 1024, Threads: 1, SaltBytes: 16, KeyBytes: 32}

// auditedChange makes one audited change through the code that makes it in
// production.
type auditedChange struct {
	name string
	// entity is the entity type its records are about.
	entity string
	// change makes the change and returns every secret it used or wrote:
	// passwords, password hashes, session IDs, tokens. A change that needs
	// others first, as a logout needs a login, calls begin once they are
	// made: only the records written after it are the change's.
	change func(t *testing.T, ctx context.Context, h harness, begin func()) (secrets []string)
}

// auditedChanges changes every audited entity (ADR 0016 §6). A pull
// request that audits a new entity, or a new change of one, adds it here;
// TestAuditRecordsHoldNoSecrets fails while an entity type in
// audit.EntityTypes has no change listed.
var auditedChanges = []auditedChange{
	{
		name:   "api users create-admin",
		entity: audit.EntityUser,
		change: func(t *testing.T, ctx context.Context, h harness, _ func()) []string {
			const secret = "correct horse battery staple, admin"
			hash := h.createAdmin(t, ctx, password.DefaultParams, "admin@example.com", secret)
			return []string{secret, hash}
		},
	},
	{
		name:   "login",
		entity: audit.EntitySession,
		change: func(t *testing.T, ctx context.Context, h harness, begin func()) []string {
			const secret = "correct horse battery staple, login"
			hash := h.createAdmin(t, ctx, password.DefaultParams, "login@example.com", secret)
			begin()
			_, session := h.login(t, ctx, "login@example.com", secret)
			return append(session, secret, hash)
		},
	},
	{
		name:   "login that upgrades the password hash",
		entity: audit.EntitySession,
		change: func(t *testing.T, ctx context.Context, h harness, begin func()) []string {
			const secret = "correct horse battery staple, upgrade"
			old := h.createAdmin(t, ctx, weak, "upgrade@example.com", secret)
			begin()
			principal, session := h.login(t, ctx, "upgrade@example.com", secret)
			upgraded := h.hash(t, ctx, principal.User.ID)
			if upgraded == old {
				t.Fatal("the login kept the weak hash")
			}
			return append(session, secret, old, upgraded)
		},
	},
	{
		name:   "failed login, wrong password",
		entity: audit.EntitySession,
		change: func(t *testing.T, ctx context.Context, h harness, begin func()) []string {
			const secret, wrong = "correct horse battery staple, failed", "correct horse battery staple, guessed"
			hash := h.createAdmin(t, ctx, password.DefaultParams, "failed@example.com", secret)
			begin()
			if _, _, err := h.sessions().Login(ctx, "failed@example.com", wrong, client); !errors.Is(err, auth.ErrInvalidCredentials) {
				t.Fatalf("Login = %v, want ErrInvalidCredentials", err)
			}
			return []string{wrong, secret, hash}
		},
	},
	{
		name:   "failed login, unknown email",
		entity: audit.EntitySession,
		change: func(t *testing.T, ctx context.Context, h harness, _ func()) []string {
			const secret = "correct horse battery staple, nobody"
			if _, _, err := h.sessions().Login(ctx, "nobody@example.com", secret, client); !errors.Is(err, auth.ErrInvalidCredentials) {
				t.Fatalf("Login = %v, want ErrInvalidCredentials", err)
			}
			return []string{secret}
		},
	},
	{
		name:   "logout",
		entity: audit.EntitySession,
		change: func(t *testing.T, ctx context.Context, h harness, begin func()) []string {
			const secret = "correct horse battery staple, logout"
			hash := h.createAdmin(t, ctx, password.DefaultParams, "logout@example.com", secret)
			principal, session := h.login(t, ctx, "logout@example.com", secret)
			begin()
			if err := h.sessions().Logout(ctx, principal, client); err != nil {
				t.Fatal(err)
			}
			return append(session, secret, hash)
		},
	},
	{
		name:   "environment created, changed and deleted",
		entity: audit.EntityEnvironment,
		change: func(t *testing.T, ctx context.Context, h harness, begin func()) []string {
			caller, secrets := h.operator(t, ctx, "env-operator@example.com")
			svc := inventory.New(h.pool)
			begin()
			e, err := svc.CreateEnvironment(ctx, caller, inventory.NewEnvironment{Name: "Production", Slug: "prod", Type: "production", Labels: map[string]string{"region": "eu"}})
			if err != nil {
				t.Fatal(err)
			}
			disabled := inventory.StatusDisabled
			if _, err := svc.UpdateEnvironment(ctx, caller, e.ID, inventory.EnvironmentChange{Status: &disabled}); err != nil {
				t.Fatal(err)
			}
			if err := svc.DeleteEnvironment(ctx, caller, e.ID); err != nil {
				t.Fatal(err)
			}
			return secrets
		},
	},
	{
		name:   "application created, changed and deleted",
		entity: audit.EntityApplication,
		change: func(t *testing.T, ctx context.Context, h harness, begin func()) []string {
			caller, secrets := h.operator(t, ctx, "app-operator@example.com")
			svc := inventory.New(h.pool)
			e, err := svc.CreateEnvironment(ctx, caller, inventory.NewEnvironment{Name: "Staging", Slug: "staging", Type: "staging"})
			if err != nil {
				t.Fatal(err)
			}
			begin()
			a, err := svc.CreateApplication(ctx, caller, inventory.NewApplication{EnvironmentID: e.ID, Name: "Claims", Platform: "outsystems_traditional"})
			if err != nil {
				t.Fatal(err)
			}
			critical := "critical"
			if _, err := svc.UpdateApplication(ctx, caller, a.ID, inventory.ApplicationChange{Criticality: &critical}); err != nil {
				t.Fatal(err)
			}
			if err := svc.DeleteApplication(ctx, caller, a.ID); err != nil {
				t.Fatal(err)
			}
			return secrets
		},
	},
	{
		name:   "component created, changed and deleted",
		entity: audit.EntityComponent,
		change: func(t *testing.T, ctx context.Context, h harness, begin func()) []string {
			caller, secrets := h.operator(t, ctx, "component-operator@example.com")
			svc := inventory.New(h.pool)
			e, err := svc.CreateEnvironment(ctx, caller, inventory.NewEnvironment{Name: "Test", Slug: "test", Type: "test"})
			if err != nil {
				t.Fatal(err)
			}
			a, err := svc.CreateApplication(ctx, caller, inventory.NewApplication{EnvironmentID: e.ID, Name: "Claims", Platform: "outsystems_traditional"})
			if err != nil {
				t.Fatal(err)
			}
			begin()
			c, err := svc.CreateComponent(ctx, caller, inventory.NewComponent{ApplicationID: a.ID, Name: "ClaimsWeb", ComponentType: "module"})
			if err != nil {
				t.Fatal(err)
			}
			runtime := "ClaimsWeb"
			if _, err := svc.UpdateComponent(ctx, caller, c.ID, inventory.ComponentChange{RuntimeName: &runtime}); err != nil {
				t.Fatal(err)
			}
			if err := svc.DeleteComponent(ctx, caller, c.ID); err != nil {
				t.Fatal(err)
			}
			return secrets
		},
	},
	{
		name:   "telemetry source registered, changed and deleted",
		entity: audit.EntityTelemetrySource,
		change: func(t *testing.T, ctx context.Context, h harness, begin func()) []string {
			caller, secrets := h.operator(t, ctx, "source-operator@example.com")
			svc := inventory.New(h.pool)
			e, err := svc.CreateEnvironment(ctx, caller, inventory.NewEnvironment{Name: "Sources", Slug: "sources", Type: "test"})
			if err != nil {
				t.Fatal(err)
			}
			a, err := svc.CreateApplication(ctx, caller, inventory.NewApplication{EnvironmentID: e.ID, Name: "Claims", Platform: "outsystems_traditional"})
			if err != nil {
				t.Fatal(err)
			}
			begin()
			src, err := svc.CreateSource(ctx, caller, inventory.NewSource{
				ApplicationID: a.ID, SourceType: "outsystems", IngestMode: "agentless", Config: map[string]any{"application_name": "Claims"},
			})
			if err != nil {
				t.Fatal(err)
			}
			otlp := "otlp"
			if _, err := svc.UpdateSource(ctx, caller, src.ID, inventory.SourceChange{IngestMode: &otlp}); err != nil {
				t.Fatal(err)
			}
			if err := svc.DeleteSource(ctx, caller, src.ID); err != nil {
				t.Fatal(err)
			}
			return secrets
		},
	},
	{
		name:   "database target created, changed and deleted",
		entity: audit.EntityDatabaseTarget,
		change: func(t *testing.T, ctx context.Context, h harness, begin func()) []string {
			caller, secrets := h.operator(t, ctx, "target-operator@example.com")
			svc := inventory.New(h.pool)
			e, err := svc.CreateEnvironment(ctx, caller, inventory.NewEnvironment{Name: "Finance", Slug: "finance", Type: "production"})
			if err != nil {
				t.Fatal(err)
			}
			begin()
			// The credential behind the reference: the API never sees it,
			// and a record must never hold anything like it.
			const credential = "sqlmonitor:Corr3ct-Horse-Battery"
			if _, err := svc.CreateDatabaseTarget(ctx, caller, inventory.NewDatabaseTarget{EnvironmentID: e.ID, Name: "refused", Host: "refused", CredentialRef: credential}); err == nil {
				t.Fatal("a credential was accepted as the reference")
			}
			tg, err := svc.CreateDatabaseTarget(ctx, caller, inventory.NewDatabaseTarget{EnvironmentID: e.ID, Name: "finance-prod", Host: "sql01.finance.internal", CredentialRef: "env/finance-prod"})
			if err != nil {
				t.Fatal(err)
			}
			ref := "kv/finance-prod"
			if _, err := svc.UpdateDatabaseTarget(ctx, caller, tg.ID, inventory.DatabaseTargetChange{CredentialRef: &ref}); err != nil {
				t.Fatal(err)
			}
			if err := svc.DeleteDatabaseTarget(ctx, caller, tg.ID); err != nil {
				t.Fatal(err)
			}
			return append(secrets, credential)
		},
	},
	{
		name:   "probe definition created, changed and disabled",
		entity: audit.EntityProbeDefinition,
		change: func(t *testing.T, ctx context.Context, h harness, _ func()) []string {
			s := probedefs.NewService(h.pool)
			actor := audit.UserActor("6f1c0e8a-3c55-4f0b-9d2b-4a7e8c1d2e3f", "admin@example.com")
			v1, err := s.Create(ctx, probedefs.NewDefinition{Engine: "sqlserver", Name: "custom_sessions_by_status", Overrides: "sessions",
				Content: probedefs.Content{QueryTemplate: "SELECT status, COUNT(*) AS session_count FROM sys.dm_exec_sessions GROUP BY status"},
			}, actor, "req-create-probe")
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.AddVersion(ctx, v1.ID, probedefs.Content{QueryTemplate: "SELECT status, COUNT_BIG(*) AS session_count FROM sys.dm_exec_sessions GROUP BY status"}, actor, "req-version-probe"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.SetEnabled(ctx, v1.ID, false, actor, "req-disable-probe"); err != nil {
				t.Fatal(err)
			}
			// Probe definitions hold no secrets: SQL, never credentials.
			return nil
		},
	},
}

// Secrets never enter audit records: each audited change runs against
// PostgreSQL, and every record it writes must be valid against the schema,
// be about its entity, and hold none of the change's secrets nor anything
// shaped like a password hash.
func TestAuditRecordsHoldNoSecrets(t *testing.T) {
	t.Parallel()
	_, _, pool := testenv.MigratedPostgres(t)
	_, redis := testenv.ValkeyClient(t)
	h := harness{pool: pool, redis: redis}
	ctx := context.Background()
	covered := map[string]bool{}
	for _, c := range auditedChanges {
		t.Run(c.name, func(t *testing.T) {
			var last int64
			begin := func() {
				if err := pool.QueryRow(ctx, "select coalesce(max(seq), 0) from audit_outbox").Scan(&last); err != nil {
					t.Fatal(err)
				}
			}
			begin()
			secrets := c.change(t, ctx, h, begin)
			rows, err := pool.Query(ctx, "select record::text from audit_outbox where seq > $1 order by seq", last)
			if err != nil {
				t.Fatal(err)
			}
			var records []string
			for rows.Next() {
				var record string
				if err := rows.Scan(&record); err != nil {
					t.Fatal(err)
				}
				records = append(records, record)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if len(records) == 0 {
				t.Fatal("the change wrote no audit record")
			}
			for _, record := range records {
				audittest.Validate(t, []byte(record))
				audittest.NoSecrets(t, []byte(record), secrets...)
				var head struct {
					EntityType string `json:"entity_type"`
				}
				if err := json.Unmarshal([]byte(record), &head); err != nil {
					t.Fatal(err)
				}
				if head.EntityType != c.entity {
					t.Errorf("record about %q, want %q: %s", head.EntityType, c.entity, record)
				}
			}
			covered[c.entity] = true
		})
	}
	for _, entity := range audit.EntityTypes {
		if !covered[entity] {
			t.Errorf("no audited change of entity type %q: add one to auditedChanges", entity)
		}
	}
}
