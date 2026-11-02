//go:build integration

package audit_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/audit/audittest"
	"heartbeat/apps/api/internal/password"
	"heartbeat/apps/api/internal/testenv"
	"heartbeat/apps/api/internal/users"
)

// auditedChange makes one audited change through the code that makes it in
// production.
type auditedChange struct {
	name string
	// entity is the entity type its records are about.
	entity string
	// change makes the change and returns every secret it used or wrote:
	// passwords, password hashes, session IDs, tokens.
	change func(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (secrets []string)
}

// auditedChanges changes every audited entity (ADR 0016 §6). A pull
// request that audits a new entity, or a new change of one, adds it here;
// TestAuditRecordsHoldNoSecrets fails while an entity type in
// audit.EntityTypes has no change listed.
var auditedChanges = []auditedChange{
	{
		name:   "api users create-admin",
		entity: audit.EntityUser,
		change: func(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []string {
			const secret = "correct horse battery staple, admin"
			created, err := users.CreateAdmin(ctx, pool, password.NewHasher(password.DefaultParams, 1),
				users.NewAdmin{Email: "admin@example.com", Password: secret},
				audit.CLIActor("api users create-admin", "api-7d9f-xk2p"), "req-create-admin")
			if err != nil {
				t.Fatal(err)
			}
			var hash string
			if err := pool.QueryRow(ctx, "select password_hash from users where id = $1", created.UserID).Scan(&hash); err != nil {
				t.Fatal(err)
			}
			return []string{secret, hash}
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
	ctx := context.Background()
	covered := map[string]bool{}
	for _, c := range auditedChanges {
		t.Run(c.name, func(t *testing.T) {
			var last int64
			if err := pool.QueryRow(ctx, "select coalesce(max(seq), 0) from audit_outbox").Scan(&last); err != nil {
				t.Fatal(err)
			}
			secrets := c.change(t, ctx, pool)
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
