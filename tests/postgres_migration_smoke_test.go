//go:build integration

package tests

import (
	"strings"
	"testing"
)

func TestUpMigrationCreatesCoreTables(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	stack := newPostgresStack(t, root)
	stack.ResetDatabase(t)
	stack.ApplyMigration(t, "/migrations/0001_foundations.up.sql")

	stdout := stack.PSQL(t, stack.DBName, `
select string_agg(name, ',')
from (
  values
    (to_regclass('public.users')::text),
    (to_regclass('public.applications')::text),
    (to_regclass('public.investigations')::text),
    (to_regclass('public.report_runs')::text)
) as t(name);
`)

	if !strings.Contains(stdout, "users,applications,investigations,report_runs") {
		t.Fatalf("unexpected table check output: %s", stdout)
	}
}

func TestDownMigrationDropsTables(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	stack := newPostgresStack(t, root)
	stack.ResetDatabase(t)
	stack.ApplyMigration(t, "/migrations/0001_foundations.up.sql")
	stack.ApplyMigration(t, "/migrations/0001_foundations.down.sql")

	stdout := stack.PSQL(t, stack.DBName, `select coalesce(to_regclass('public.users')::text, '');`)
	if !strings.Contains(stdout, "\n\n") {
		t.Fatalf("expected empty to_regclass output, got: %q", stdout)
	}
}

func TestInvestigationLookupQueryPlanUsesIndex(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	stack := newPostgresStack(t, root)
	stack.ResetDatabase(t)
	stack.ApplyMigration(t, "/migrations/0001_foundations.up.sql")
	stack.PSQL(t, stack.DBName, `
insert into users (email, display_name) values ('admin@example.com', 'Admin');
insert into environments (name, slug, type) values ('Prod', 'prod', 'production');
insert into applications (environment_id, name, platform)
select id, 'Wallboard', 'outsystems' from environments where slug = 'prod';
insert into investigations (application_id, environment_id, created_by, subject_type, subject_value, time_start, time_end, query)
select a.id, e.id, u.id, 'user', 'alice@example.com', now() - interval '1 hour', now(), '{}'::jsonb
from applications a
join environments e on e.id = a.environment_id
join users u on u.email = 'admin@example.com';
`)

	stdout := stack.PSQL(t, stack.DBName, `
set enable_seqscan = off;
explain (costs off)
select *
from investigations
where environment_id = (select id from environments where slug = 'prod')
  and subject_type = 'user'
  and subject_value = 'alice@example.com'
  and time_start <= now()
  and time_end >= now() - interval '2 hours';
`)

	if !strings.Contains(stdout, "investigations_subject_lookup_idx") {
		t.Fatalf("query plan did not use expected index: %s", stdout)
	}
}

// 0002 adds the audit outbox, the password hash and the admin role, and its
// down migration removes them again, so the up migration applies twice.
func TestAuditOutboxMigrationUpAndDown(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	stack := newPostgresStack(t, root)
	stack.ResetDatabase(t)
	stack.ApplyMigration(t, "/migrations/0001_foundations.up.sql")
	present := `
select concat_ws(',',
  to_regclass('public.audit_outbox')::text,
  (select column_name from information_schema.columns where table_name = 'users' and column_name = 'password_hash'),
  (select name from roles where name = 'admin'),
  to_regclass('public.users_email_lower_uq')::text);
`
	for round := 1; round <= 2; round++ {
		stack.ApplyMigration(t, "/migrations/0002_audit_outbox.up.sql")
		if stdout := stack.PSQL(t, stack.DBName, present); !strings.Contains(stdout, "audit_outbox,password_hash,admin,users_email_lower_uq") {
			t.Fatalf("round %d, after up: %s", round, stdout)
		}
		stack.ApplyMigration(t, "/migrations/0002_audit_outbox.down.sql")
		if stdout := stack.PSQL(t, stack.DBName, present); strings.Contains(stdout, "audit_outbox") || strings.Contains(stdout, "password_hash") || strings.Contains(stdout, "admin") || strings.Contains(stdout, "users_email_lower_uq") {
			t.Fatalf("round %d, after down: %s", round, stdout)
		}
	}
}

// 0004 versions probe definitions and seeds the built-ins; its down
// migration removes them again with every custom definition, so the up
// migration applies twice.
func TestProbeDefinitionsMigrationUpAndDown(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	stack := newPostgresStack(t, root)
	stack.ResetDatabase(t)
	for _, m := range []string{"0001_foundations", "0002_audit_outbox", "0003_login_roles"} {
		stack.ApplyMigration(t, "/migrations/"+m+".up.sql")
	}
	shape := `
select concat_ws(',',
  (select data_type from information_schema.columns where table_name = 'probe_definitions' and column_name = 'version'),
  (select is_nullable from information_schema.columns where table_name = 'probe_definitions' and column_name = 'query_template'),
  (select count(*) from probe_definitions where builtin)::text,
  (select count(*) from probe_definitions where not builtin)::text);
`
	// A custom definition, its timeout left to the collector.
	custom := `
insert into probe_definitions (engine, name, version, category, default_interval_seconds, query_template, overrides)
values ('sqlserver', 'waits_lean', 2, 'waits', 30, 'SELECT 1', 'waits');
`
	for round := 1; round <= 2; round++ {
		stack.ApplyMigration(t, "/migrations/0004_probe_definitions.up.sql")
		stack.PSQL(t, stack.DBName, custom)
		if stdout := stack.PSQL(t, stack.DBName, shape); !strings.Contains(stdout, "integer,YES,9,1") {
			t.Fatalf("round %d, after up: %s", round, stdout)
		}
		stack.ApplyMigration(t, "/migrations/0004_probe_definitions.down.sql")
		after := `select concat_ws(',',
  (select data_type from information_schema.columns where table_name = 'probe_definitions' and column_name = 'version'),
  (select is_nullable from information_schema.columns where table_name = 'probe_definitions' and column_name = 'query_template'),
  coalesce((select column_name from information_schema.columns where table_name = 'probe_definitions' and column_name = 'builtin'), 'no builtin'),
  (select count(*) from probe_definitions)::text);`
		if stdout := stack.PSQL(t, stack.DBName, after); !strings.Contains(stdout, "text,NO,no builtin,0") {
			t.Fatalf("round %d, after down: %s", round, stdout)
		}
	}
}
