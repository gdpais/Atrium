//go:build sqlserver

package outsystems

import (
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-sql/civil"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	collectorconfig "heartbeat/internal/config"
	"heartbeat/internal/otlp"
	"heartbeat/services/db-collector/internal/collectors"
	connector "heartbeat/services/db-collector/internal/connectors/sqlserver"
	collectormetadata "heartbeat/services/db-collector/internal/metadata"
	catalogoutsystems "heartbeat/services/db-collector/internal/probes/outsystems"
)

// Credential references resolved through EnvCredentialResolver (see
// scripts/sqlserver-test.sh).
const (
	// liveOSLogCredentialRef is HEARTBEAT_CREDENTIAL_SQLSERVER_TEST_OSLOG, a
	// login with no server permissions.  setupFixtures maps it to a user of
	// the fixture database with SELECT on the fixture tables only.
	liveOSLogCredentialRef = "sqlserver-test-oslog"
	// liveSACredentialRef is HEARTBEAT_CREDENTIAL_SQLSERVER_TEST_SA, used only
	// to create the fixtures and insert rows.
	liveSACredentialRef = "sqlserver-test-sa"
	// fixtureDatabase is created, and dropped first, by setupFixtures.
	fixtureDatabase = "heartbeat_oslog_test"
)

// liveServer is the disposable test server.
type liveServer struct {
	host    string
	port    int
	manager connector.Manager
}

// newLiveServer returns the test server, skipping the test outside make
// test-sqlserver.
func newLiveServer(t *testing.T) liveServer {
	t.Helper()
	addr := os.Getenv("HEARTBEAT_TEST_SQLSERVER_ADDR")
	if addr == "" {
		t.Skip("HEARTBEAT_TEST_SQLSERVER_ADDR is not set; run make test-sqlserver")
	}
	for _, env := range []string{"HEARTBEAT_CREDENTIAL_SQLSERVER_TEST_SA", "HEARTBEAT_CREDENTIAL_SQLSERVER_TEST_OSLOG"} {
		if os.Getenv(env) == "" {
			t.Fatalf("%s is not set; run make test-sqlserver", env)
		}
	}
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("HEARTBEAT_TEST_SQLSERVER_ADDR: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("HEARTBEAT_TEST_SQLSERVER_ADDR port: %v", err)
	}
	manager := connector.NewManager(connector.EnvCredentialResolver{})
	// The disposable test server uses a self-signed certificate.
	manager.TrustServerCertificate = true
	t.Cleanup(func() { _ = manager.Close() })
	return liveServer{host: host, port: port, manager: manager}
}

// target returns the server's database as a target logged in with ref.
func (s liveServer) target(database, ref string) collectormetadata.DatabaseTarget {
	return collectormetadata.DatabaseTarget{
		EnvironmentSlug: "test", Name: "logging-db", Engine: collectorconfig.KindOutSystemsLogs,
		Host: s.host, Port: s.port, DatabaseName: database, CredentialRef: ref,
	}
}

// sa opens database as sa.
func (s liveServer) sa(t *testing.T, database string) *sql.DB {
	t.Helper()
	db, cleanup, err := s.manager.Open(context.Background(), s.target(database, liveSACredentialRef))
	if err != nil {
		t.Fatalf("open %s as sa: %v", database, err)
	}
	t.Cleanup(cleanup)
	return db
}

func exec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, query, args...); err != nil {
		t.Fatalf("%s: %v", strings.SplitN(strings.TrimSpace(query), "\n", 2)[0], err)
	}
}

// setupFixtures recreates the fixture database with the tables and views of
// every log type's fixture, without rows, and gives the test login a user in
// it with SELECT on each of those tables and views and nothing else.
func setupFixtures(t *testing.T, server liveServer) {
	t.Helper()
	master := server.sa(t, "master")
	exec(t, master, fmt.Sprintf(`IF DB_ID(N'%[1]s') IS NOT NULL
BEGIN
	ALTER DATABASE [%[1]s] SET SINGLE_USER WITH ROLLBACK IMMEDIATE;
	DROP DATABASE [%[1]s];
END
CREATE DATABASE [%[1]s];`, fixtureDatabase))
	db := server.sa(t, fixtureDatabase)
	for _, logType := range catalogoutsystems.DefaultCatalog().Names() {
		ddl, err := os.ReadFile(filepath.Join(fixtureDir, logType+".sql"))
		if err != nil {
			t.Fatalf("log type %q has no fixture (see TestEveryLogTypeHasASQLServerFixture): %v", logType, err)
		}
		exec(t, db, string(ddl))
	}
	login, _, _ := strings.Cut(os.Getenv("HEARTBEAT_CREDENTIAL_SQLSERVER_TEST_OSLOG"), ":")
	exec(t, db, `DECLARE @sql nvarchar(max) = N'CREATE USER ' + QUOTENAME(@login) + N' FOR LOGIN ' + QUOTENAME(@login) + N';';
SELECT @sql += N'GRANT SELECT ON ' + QUOTENAME(SCHEMA_NAME(schema_id)) + N'.' + QUOTENAME(name) + N' TO ' + QUOTENAME(@login) + N';'
FROM sys.objects WHERE type IN ('U', 'V');
EXEC sys.sp_executesql @sql;`, sql.Named("login", login))
	// A table the login was not granted, to show the grants are all it has.
	exec(t, db, "CREATE TABLE dbo.not_granted (Id int)")
}

// loadFixtureRows runs the log type's <log type>.rows.sql and returns how
// many rows it added to the log tables: the tables with an Instant column,
// which leaves the detail tables out.
func loadFixtureRows(t *testing.T, db *sql.DB, logType string) int {
	t.Helper()
	rows, err := os.ReadFile(filepath.Join(fixtureDir, logType+".rows.sql"))
	if err != nil {
		t.Fatalf("log type %q has no fixture rows (see TestEveryLogTypeHasASQLServerFixture): %v", logType, err)
	}
	before := countLogRows(t, db)
	exec(t, db, string(rows))
	return countLogRows(t, db) - before
}

// countLogRows counts the rows of every table that has an Instant column.
func countLogRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var count int
	err := db.QueryRowContext(ctx, `DECLARE @sql nvarchar(max) = N'SELECT 0';
SELECT @sql += N' + (SELECT COUNT(*) FROM ' + QUOTENAME(SCHEMA_NAME(t.schema_id)) + N'.' + QUOTENAME(t.name) + N')'
FROM sys.tables AS t
WHERE EXISTS (SELECT 1 FROM sys.columns AS c WHERE c.object_id = t.object_id AND c.name = 'Instant');
EXEC sys.sp_executesql @sql;`).Scan(&count)
	if err != nil {
		t.Fatalf("count log rows: %v", err)
	}
	return count
}

// viewTables returns the tables a view reads: its cycle table, and for
// oslog_Error the detail table it joins too.
func viewTables(t *testing.T, db *sql.DB, view string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, `SELECT referenced_entity_name FROM sys.sql_expression_dependencies WHERE referencing_id = OBJECT_ID(@view)`,
		sql.Named("view", "dbo."+view))
	if err != nil {
		t.Fatalf("tables behind dbo.%s: %v", view, err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil || len(tables) == 0 {
		t.Fatalf("tables behind dbo.%s: %v %v", view, tables, err)
	}
	return tables
}

// cycleTable returns the cycle table behind the log type's view, for
// example oslog_Error_3 behind oslog_Error.
func cycleTable(t *testing.T, db *sql.DB, logView, view string) string {
	t.Helper()
	cycle := regexp.MustCompile(`^` + regexp.QuoteMeta(logView) + `_\d+$`)
	for _, table := range viewTables(t, db, view) {
		if cycle.MatchString(table) {
			return table
		}
	}
	t.Fatalf("no %s_<cycle> table behind dbo.%s", logView, view)
	return ""
}

// rotateLogTables does what the platform does every Friday at 23:45: the
// current week's view moves to an empty cycle table and the previous week's
// view to the table the current one used.  With two fixture tables the
// views swap: each view's definition is kept, with the other view's cycle
// in its table names (the cycle table, and the detail table it joins).
func rotateLogTables(t *testing.T, db *sql.DB, view string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	views := []string{view, view + "_Previous"}
	suffix := regexp.MustCompile(`_\d+$`)
	tables := map[string][]string{}
	cycles := map[string]string{}
	definitions := map[string]string{}
	for _, name := range views {
		tables[name] = viewTables(t, db, name)
		cycles[name] = suffix.FindString(cycleTable(t, db, view, name))
		var definition string
		if err := db.QueryRowContext(ctx, `SELECT OBJECT_DEFINITION(OBJECT_ID(@view))`, sql.Named("view", "dbo."+name)).Scan(&definition); err != nil {
			t.Fatalf("definition of dbo.%s: %v", name, err)
		}
		definitions[name] = definition
	}
	for i, name := range views {
		var pairs []string
		for _, table := range tables[name] {
			pairs = append(pairs, table, suffix.ReplaceAllLiteralString(table, cycles[views[1-i]]))
		}
		definition := strings.NewReplacer(pairs...).Replace(definitions[name])
		if !strings.HasPrefix(definition, "CREATE VIEW ") {
			t.Fatalf("definition of dbo.%s: %q", name, definition)
		}
		exec(t, db, `EXEC sys.sp_executesql @definition`, sql.Named("definition", "ALTER VIEW "+strings.TrimPrefix(definition, "CREATE VIEW ")))
	}
}

// Runs every embedded query against its fixture tables and rows, logged in
// with only SELECT on them, and checks the result against the query
// contract.  Over a window that covers every fixture row, the query must
// return each of them: the rows of the previous week's table as well as the
// current week's.
func TestLogQueriesAgainstSQLServer(t *testing.T) {
	server := newLiveServer(t)
	setupFixtures(t, server)
	sa := server.sa(t, fixtureDatabase)
	source := SQLServerSource{Manager: server.manager}
	target := server.target(fixtureDatabase, liveOSLogCredentialRef)
	catalog := catalogoutsystems.DefaultCatalog()
	from := Wall(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), time.UTC)
	to := Wall(time.Now(), time.UTC).Truncate(time.Second)
	const limit = 10000
	for _, logType := range catalog.Names() {
		t.Run(logType, func(t *testing.T) {
			want := loadFixtureRows(t, sa, logType)
			if want == 0 {
				t.Fatalf("%s.rows.sql inserted no rows into a table with an Instant column", logType)
			}
			query, _ := catalog.Get(logType)
			for _, dateTime := range []bool{false, true} {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				batch, err := source.Query(ctx, target, query.SQL, QueryParams{From: from, To: to, Limit: limit, DateTime: dateTime})
				cancel()
				if err != nil {
					t.Fatalf("query (datetime bounds %v): %v", dateTime, err)
				}
				if _, _, err := checkContract(batch, from, to, limit); err != nil {
					t.Fatal(err)
				}
				if len(batch.Rows) != want {
					t.Fatalf("query returned %d rows, want the %d rows of %s.rows.sql (the current and the previous week's tables)", len(batch.Rows), want, logType)
				}
				if logType == errorLogType {
					checkLongStack(t, sa, batch)
				}
			}
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := source.Query(ctx, target, "SELECT TOP (@limit) Id FROM dbo.not_granted WHERE @from < @to", QueryParams{From: from, To: to, Limit: 1})
	if err == nil || !strings.Contains(err.Error(), "SELECT permission was denied") {
		t.Fatalf("query of a table that was not granted: %v; want a permission error", err)
	}
}

// checkLongStack checks that the error query returns the stack trace that
// lives only in oslog_Error_Detail in full: past the cycle table's 2000
// characters, and past varchar(8000).
func checkLongStack(t *testing.T, sa *sql.DB, batch Batch) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var full string
	if err := sa.QueryRowContext(ctx, `SELECT FullStackTrace FROM dbo.oslog_Error_Detail_3`).Scan(&full); err != nil {
		t.Fatalf("detail row: %v", err)
	}
	stack := slices.IndexFunc(batch.Columns, func(c Column) bool { return c.Name == columnStack })
	var long []string
	for _, row := range batch.Rows {
		if text := optionalText(row[stack]); len(text) > 2000 {
			long = append(long, text)
		}
	}
	if len(full) <= 8000 || len(long) != 1 || long[0] != full {
		t.Fatalf("the query returned %d stacks longer than 2000 characters, want the detail table's %d-character trace", len(long), len(full))
	}
}

// A backlog after an outage: the query must read about @limit rows of each
// week's view, not every row in (@from, @to].  Both cycle tables of each log
// type get 50,000 copies of a fixture row, then the query runs for 20 rows
// over all of them, on one connection whose logical reads are counted.  A
// query that reads the whole range does at least one read per data page,
// thousands here; reading the 20 rows of each week, through the Instant
// index and a lookup each (and, for errors, the detail table), takes from a
// dozen to about 400.
func TestLogQueriesReadOnlyTheBatchAgainstSQLServer(t *testing.T) {
	server := newLiveServer(t)
	setupFixtures(t, server)
	sa := server.sa(t, fixtureDatabase)
	catalog := catalogoutsystems.DefaultCatalog()
	const copies, limit = 50000, 20
	views := regexp.MustCompile(`(?i)FROM\s+dbo\.(oslog_\w+)`)
	for _, logType := range catalog.Names() {
		t.Run(logType, func(t *testing.T) {
			loadFixtureRows(t, sa, logType)
			query, _ := catalog.Get(logType)
			current := views.FindStringSubmatch(query.SQL)[1]
			for _, view := range []string{current, current + "_Previous"} {
				copyRows(t, sa, cycleTable(t, sa, current, view), copies)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			conn, err := sa.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			sessionReads := func() int64 {
				var reads int64
				if err := conn.QueryRowContext(ctx, `SELECT logical_reads FROM sys.dm_exec_sessions WHERE session_id = @@SPID`).Scan(&reads); err != nil {
					t.Fatalf("logical reads: %v", err)
				}
				return reads
			}
			run := func() int {
				t.Helper()
				rows, err := conn.QueryContext(ctx, query.SQL,
					sql.Named("from", civil.DateTimeOf(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))),
					sql.Named("to", civil.DateTimeOf(time.Now().UTC())), sql.Named("limit", limit))
				if err != nil {
					t.Fatalf("query: %v", err)
				}
				n := 0
				for rows.Next() {
					n++
				}
				if err := rows.Close(); err != nil || rows.Err() != nil {
					t.Fatalf("query: %v %v", err, rows.Err())
				}
				return n
			}
			// The first run compiles the plan and updates the statistics the
			// copies made stale, which reads the tables; the fewest reads of
			// the next three are the plan's own.
			run()
			reads := int64(-1)
			for range 3 {
				before := sessionReads()
				if n := run(); n != limit {
					t.Fatalf("query returned %d rows, want %d", n, limit)
				}
				if r := sessionReads() - before; reads < 0 || r < reads {
					reads = r
				}
			}
			t.Logf("%d rows of %d in range, %d logical reads", limit, 2*copies, reads)
			if reads > 25*limit+200 {
				t.Fatalf("%d logical reads for %d rows: the query reads the whole (@from, @to] range; limit each week's view to @limit rows before the UNION ALL (queries/README.md)", reads, limit)
			}
		})
	}
}

// copyRows inserts n copies of one row of table, a millisecond apart from
// 2025-01-01 (before every fixture row), each with a new Id where the table
// has one.
func copyRows(t *testing.T, db *sql.DB, table string, n int) {
	t.Helper()
	exec(t, db, `DECLARE @cols nvarchar(max), @vals nvarchar(max);
SELECT @cols = STRING_AGG(CAST(QUOTENAME(name) AS nvarchar(max)), N', ') WITHIN GROUP (ORDER BY column_id),
       @vals = STRING_AGG(CAST(CASE name
           WHEN N'Id' THEN N'CONVERT(varchar(50), NEWID())'
           WHEN N'Instant' THEN N'DATEADD(millisecond, n.i, CAST(''2025-01-01'' AS datetime2(3)))'
           ELSE N'r.' + QUOTENAME(name) END AS nvarchar(max)), N', ') WITHIN GROUP (ORDER BY column_id)
FROM sys.columns WHERE object_id = OBJECT_ID(N'dbo.' + QUOTENAME(@table)) AND is_computed = 0 AND is_identity = 0;
DECLARE @sql nvarchar(max) = N'WITH n AS (SELECT TOP (@n) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) AS i FROM sys.all_columns AS a CROSS JOIN sys.all_columns AS b)
INSERT INTO dbo.' + QUOTENAME(@table) + N' (' + @cols + N') SELECT ' + @vals + N' FROM n CROSS JOIN (SELECT TOP (1) * FROM dbo.' + QUOTENAME(@table) + N') AS r;';
EXEC sys.sp_executesql @sql, N'@n int', @n;`, sql.Named("table", table), sql.Named("n", n))
}

// Computes the derived metrics from every log type's fixture rows, read
// through its query, so the column types the driver returns (bit, int,
// datetime, text error ids) are the ones the metrics code sees in
// production.
func TestDerivedMetricsAgainstSQLServer(t *testing.T) {
	server := newLiveServer(t)
	setupFixtures(t, server)
	sa := server.sa(t, fixtureDatabase)
	source := SQLServerSource{Manager: server.manager}
	target := server.target(fixtureDatabase, liveOSLogCredentialRef)
	target.EnvironmentSlug = "prod"
	catalog := catalogoutsystems.DefaultCatalog()
	from := Wall(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), time.UTC)
	to := Wall(time.Now(), time.UTC).Truncate(time.Second)
	const limit = 10000
	d := newDerivedRun(t)
	for _, logType := range catalog.Names() {
		loadFixtureRows(t, sa, logType)
		query, _ := catalog.Get(logType)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		batch, err := source.Query(ctx, target, query.SQL, QueryParams{From: from, To: to, Limit: limit})
		cancel()
		if err != nil {
			t.Fatalf("%s query: %v", logType, err)
		}
		instants, _, err := checkContract(batch, from, to, limit)
		if err != nil {
			t.Fatalf("%s: %v", logType, err)
		}
		item := collectormetadata.ScheduledProbe{CollectorID: "c", Target: target, Definition: collectormetadata.ProbeDefinition{Name: logType}}
		d.metrics.observeRows(item, batch, batch.Rows, instants)
	}

	operations := func(module, logType, operation, outcome string) map[string]string {
		return map[string]string{"application": "Shop", "module": module, "log_type": logType, "operation": operation, "outcome": outcome}
	}
	for _, tc := range []struct {
		name   string
		metric string
		labels map[string]string
		want   float64
	}{
		{"screen request", MetricOperations, operations("Shop_CS", "screen", "Checkout", "success"), 1},
		{"Reactive server call with an error id", MetricOperations, operations("Shop_RW", "mobile_request", "Pay", "error"), 1},
		{"Reactive server call", MetricOperations, operations("Shop_RW", "mobile_request", "GetOrders", "success"), 1},
		{"timer run with an error id", MetricOperations, operations("Shop_CS", "cyclic_job", "ProcessOrders", "error"), 1},
		{"exposed API", MetricOperations, operations("Shop_CS", "integration", "Orders_Get", "success"), 1},
		{"consumed integration call that failed", MetricIntegrationDuration, map[string]string{"application": "Shop", "direction": "consume", "type": "REST", "outcome": "error"}, 1},
		{"consumed integration call", MetricIntegrationDuration, map[string]string{"application": "Shop", "direction": "consume", "type": "REST", "outcome": "success"}, 1},
		{"errors by module", MetricErrors, map[string]string{"application": "Shop", "module": "Shop_CS"}, 2},
		{"slow SQL", MetricSlowEvents, map[string]string{"application": "Shop", "module": "Shop_CS", "kind": "sql"}, 1},
		{"front-end server", MetricFrontEndOperations, map[string]string{"application": "Shop", "front_end": "FE-02", "outcome": "error"}, 2},
	} {
		if got := d.value(tc.metric, tc.labels); got != tc.want {
			t.Errorf("%s: %s%v = %v, want %v", tc.name, tc.metric, tc.labels, got, tc.want)
		}
	}
	// Seconds, converted by the query, then to seconds by the collector.
	if got := d.sum(MetricOperationDuration, map[string]string{"application": "Shop", "module": "Shop_RW", "log_type": "mobile_request", "outcome": "error"}); got != 31 {
		t.Errorf("Reactive server call duration = %v s, want 31", got)
	}
	// ProcessOrders started 53 s after its schedule.
	if got := d.sum(MetricTimerDelay, map[string]string{"application": "Shop", "operation": "ProcessOrders"}); got != 53 {
		t.Errorf("timer delay = %v s, want 53", got)
	}
}

// Reads the session's lock timeout and deadlock priority from inside a log
// query, as a query_template override would, with its parameters bound: the
// collector's session settings must hold there too.
func TestLogQuerySessionSettingsAgainstSQLServer(t *testing.T) {
	server := newLiveServer(t)
	setupFixtures(t, server)
	source := SQLServerSource{Manager: server.manager}
	const query = `SELECT TOP (@limit) CAST(@to AS datetime2) AS Instant, @@LOCK_TIMEOUT AS LockTimeout,
	(SELECT deadlock_priority FROM sys.dm_exec_sessions WHERE session_id = @@SPID) AS DeadlockPriority`
	to := Wall(time.Now(), time.UTC).Truncate(time.Second)
	for run := range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		batch, err := source.Query(ctx, server.target(fixtureDatabase, liveOSLogCredentialRef), query, QueryParams{From: to.Add(-time.Minute), To: to, Limit: 1})
		cancel()
		if err != nil {
			t.Fatalf("run %d: %v", run+1, err)
		}
		row := batch.Rows[0]
		if row[1] != connector.LockTimeout.Milliseconds() || row[2] != int64(-5) {
			t.Fatalf("run %d: lock timeout %v, deadlock priority %v; want %d and -5 (LOW)", run+1, row[1], row[2], connector.LockTimeout.Milliseconds())
		}
	}
}

// otlpReceiver is a fake OTel Collector that records the Request_Key of
// every record it receives.
type otlpReceiver struct {
	mu   sync.Mutex
	keys []string
}

func (r *otlpReceiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	zr, err := gzip.NewReader(req.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	raw, _ := io.ReadAll(zr)
	var data logspb.LogsData
	if err := proto.Unmarshal(raw, &data); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rl := range data.ResourceLogs {
		for _, sl := range rl.ScopeLogs {
			for _, record := range sl.LogRecords {
				key, _ := attributes(record.Attributes)["Request_Key"].(string)
				r.keys = append(r.keys, key)
			}
		}
	}
}

// sameRows reports whether got holds each of want exactly once.  Rows that
// share an Instant come back in no particular order: the surveyed cycle
// tables are clustered on Id, not Instant.
func sameRows(got, want []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want)))
}

func (r *otlpReceiver) received() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.keys)
}

// Extracts dbo.oslog_Error through the production path (connector, the
// embedded error.sql, OTLP/HTTP, watermark file) over several cycles and a
// restart, with ties on Instant at batch boundaries, millisecond instants
// (the surveyed datetime2), and a row committed after its Instant.  Every
// row must arrive exactly once.
func TestExtractionAgainstSQLServer(t *testing.T) {
	server := newLiveServer(t)
	setupFixtures(t, server)
	sa := server.sa(t, fixtureDatabase)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// insertInto writes a row into a cycle table by name; insert into the
	// one dbo.oslog_Error reads.  The view computes Stack from the detail
	// table, so rows cannot be inserted through it.
	insertInto := func(object, key string, offset time.Duration) {
		t.Helper()
		exec(t, sa, `DECLARE @sql nvarchar(max) = N'INSERT INTO dbo.' + QUOTENAME(@object) + N' (Id, Instant, Session_Id, User_Id, Espace_Id, Tenant_Id, Message, Stack, Module_Name, Server, Espace_Name, Request_Key)
VALUES (CONVERT(varchar(50), NEWID()), @instant, '''', 7, 1, 0, @message, ''at Shop.Checkout()'', ''ShopCore'', ''FE-01'', ''ShopCore'', @key)';
EXEC sys.sp_executesql @sql, N'@instant datetime2(3), @message varchar(2000), @key varchar(36)', @instant, @message, @key;`,
			sql.Named("object", object), sql.Named("instant", base.Add(offset)), sql.Named("message", "Order failed: "+key), sql.Named("key", key))
	}
	insert := func(key string, offset time.Duration) {
		t.Helper()
		insertInto(cycleTable(t, sa, "oslog_Error", "oslog_Error"), key, offset)
	}
	ms := time.Millisecond
	insert("a", time.Minute)
	insert("b", 2*time.Minute+3*ms) // b and c share an Instant
	insert("c", 2*time.Minute+3*ms)
	insert("d", 2*time.Minute+4*ms) // the next millisecond
	insert("e", 3*time.Minute)
	insert("f", 4*time.Minute) // f and g share an Instant
	insert("g", 4*time.Minute)
	insert("h", 8*time.Minute+59*time.Second+997*ms)

	receiver := &otlpReceiver{}
	collector := httptest.NewServer(receiver)
	defer collector.Close()
	dir := t.TempDir()
	newExecutor := func(now *time.Time) *Executor {
		manager := connector.NewManager(connector.EnvCredentialResolver{})
		manager.TrustServerCertificate = true
		t.Cleanup(func() { _ = manager.Close() })
		executor := NewExecutor(SQLServerSource{Manager: manager}, NewStore(dir), otlp.NewClient(), nil, slog.New(slog.DiscardHandler))
		executor.now = func() time.Time { return *now }
		return executor
	}
	item := collectormetadata.ScheduledProbe{
		CollectorID: "oslogs",
		Target:      server.target(fixtureDatabase, liveOSLogCredentialRef),
		Definition:  collectormetadata.ProbeDefinition{Name: "error"},
		OutSystemsLogs: collectorconfig.OutSystemsLogsConfig{
			Lag: time.Minute, BatchLimit: 3, InitialLookback: 15 * time.Minute, LogTimezone: "UTC", LogsEndpoint: collector.URL,
		},
	}
	run := func(executor *Executor) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, _, err := executor.RunProbe(ctx, item); err != nil {
			t.Fatalf("RunProbe: %v", err)
		}
	}

	now := base.Add(10 * time.Minute)
	executor := newExecutor(&now)
	// Reads (11:55, 12:09] in batches of 3: a | b c | d e | f g | h.
	run(executor)
	if got := receiver.received(); !sameRows(got, []string{"a", "b", "c", "d", "e", "f", "g", "h"}) {
		t.Fatalf("first cycle delivered %v", got)
	}
	watermark, _, _ := executor.Store.Load(WatermarkKey{Collector: "oslogs", Target: "logging-db", LogType: "error"})
	if watermark.InstantType != "DATETIME2" || !watermark.Instant.Equal(base.Add(9*time.Minute)) {
		t.Fatalf("watermark %+v", watermark)
	}

	// Committed now, 30 s after its Instant: inside the lag, so not lost.
	insert("late", 9*time.Minute+30*time.Second)
	now = now.Add(time.Minute)
	run(executor)

	// A restart resumes from the watermark file.
	insert("after-restart", 10*time.Minute+30*time.Second)
	now = now.Add(time.Minute)
	restarted := newExecutor(&now)
	run(restarted)
	now = now.Add(time.Minute)
	run(restarted)

	want := []string{"a", "b", "c", "d", "e", "f", "g", "h", "late", "after-restart"}
	if got := receiver.received(); !sameRows(got, want) {
		t.Fatalf("delivered %v, want each of %v once", got, want)
	}

	// Friday 23:45: the platform rotates the log tables.  dbo.oslog_Error now
	// reads an empty cycle table and last week's table is behind
	// dbo.oslog_Error_Previous.  A row of the old week committed after the
	// rotation, within the lag, is only in the previous table; the query
	// reads both, so it arrives with the first row of the new week.
	oldTable := cycleTable(t, sa, "oslog_Error", "oslog_Error")
	rotateLogTables(t, sa, "oslog_Error")
	if got := cycleTable(t, sa, "oslog_Error", "oslog_Error_Previous"); got != oldTable {
		t.Fatalf("after the rotation dbo.oslog_Error_Previous reads %s, want %s", got, oldTable)
	}
	insertInto(oldTable, "before-rotation", 12*time.Minute+30*time.Second)
	insert("after-rotation", 13*time.Minute)
	now = now.Add(2 * time.Minute)
	run(restarted)
	want = append(want, "before-rotation", "after-rotation")
	if got := receiver.received(); !sameRows(got, want) {
		t.Fatalf("after the rotation delivered %v, want %v", got, want)
	}
}

// heldReceiver is an OTel Collector that holds every request until release
// is closed, as the chart's Collector does while Loki has not taken the
// batch before.
type heldReceiver struct {
	otlpReceiver
	release chan struct{}
}

func (r *heldReceiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	select {
	case <-r.release:
	case <-req.Context().Done():
		return
	}
	r.otlpReceiver.ServeHTTP(w, req)
}

// While a batch waits for the OTel Collector, the collector's login holds
// nothing on SQL Server: no running request, no open transaction, and no
// lock but the shared database lock every connection holds.  The rows were
// read and the connection returned to the pool before the export started.
func TestWaitingExportHoldsNothingAgainstSQLServer(t *testing.T) {
	server := newLiveServer(t)
	setupFixtures(t, server)
	sa := server.sa(t, fixtureDatabase)
	table := cycleTable(t, sa, "oslog_Error", "oslog_Error")
	exec(t, sa, `INSERT INTO dbo.`+table+` (Id, Instant, Session_Id, User_Id, Espace_Id, Tenant_Id, Message, Stack, Module_Name, Server, Espace_Name, Request_Key)
VALUES ('held-1', DATEADD(MINUTE, -5, SYSUTCDATETIME()), '', 7, 1, 0, 'Order failed', 'at Shop.Checkout()', 'ShopCore', 'FE-01', 'ShopCore', 'held-1')`)

	receiver := &heldReceiver{release: make(chan struct{})}
	collector := httptest.NewServer(receiver)
	defer collector.Close()
	manager := connector.NewManager(connector.EnvCredentialResolver{})
	manager.TrustServerCertificate = true
	t.Cleanup(func() { _ = manager.Close() })
	executor := NewExecutor(SQLServerSource{Manager: manager}, NewStore(t.TempDir()), otlp.NewClient(), nil, slog.New(slog.DiscardHandler))
	defer executor.Close(context.Background())
	item := collectormetadata.ScheduledProbe{
		CollectorID: "oslogs",
		Target:      server.target(fixtureDatabase, liveOSLogCredentialRef),
		Definition:  collectormetadata.ProbeDefinition{Name: "error"},
		OutSystemsLogs: collectorconfig.OutSystemsLogsConfig{
			Lag: time.Minute, BatchLimit: 100, InitialLookback: 15 * time.Minute, LogTimezone: "UTC", LogsEndpoint: collector.URL,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_, _, err := executor.RunProbe(ctx, item)
	cancel()
	if !errors.Is(err, collectors.ErrBlocked) {
		t.Fatalf("RunProbe: %v, want the export waiting", err)
	}

	login, _, _ := strings.Cut(os.Getenv("HEARTBEAT_CREDENTIAL_SQLSERVER_TEST_OSLOG"), ":")
	var sessions, busy, locks int
	row := sa.QueryRowContext(context.Background(), `SELECT
    (SELECT COUNT(*) FROM sys.dm_exec_sessions WHERE login_name = @login),
    (SELECT COUNT(*) FROM sys.dm_exec_sessions s WHERE s.login_name = @login
        AND (s.open_transaction_count > 0 OR EXISTS (SELECT 1 FROM sys.dm_exec_requests r WHERE r.session_id = s.session_id))),
    (SELECT COUNT(*) FROM sys.dm_tran_locks l JOIN sys.dm_exec_sessions s ON s.session_id = l.request_session_id
        WHERE s.login_name = @login AND l.resource_type <> 'DATABASE')`, sql.Named("login", login))
	if err := row.Scan(&sessions, &busy, &locks); err != nil {
		t.Fatal(err)
	}
	t.Logf("while the export waits: %d sessions of %s, %d busy, %d locks", sessions, login, busy, locks)
	if busy != 0 || locks != 0 {
		t.Fatalf("while the export waits, %s has %d busy sessions and %d locks; want none", login, busy, locks)
	}

	close(receiver.release)
	executor.Close(context.Background())
	if got := receiver.received(); !sameRows(got, []string{"held-1"}) {
		t.Fatalf("delivered %v after the Collector accepted", got)
	}
}

// The recount wraps every log type's query and counts, per minute, exactly
// the rows the query returns, with datetime and datetime2 bounds.
func TestRecountAgainstSQLServer(t *testing.T) {
	server := newLiveServer(t)
	setupFixtures(t, server)
	sa := server.sa(t, fixtureDatabase)
	source := SQLServerSource{Manager: server.manager}
	target := server.target(fixtureDatabase, liveOSLogCredentialRef)
	catalog := catalogoutsystems.DefaultCatalog()
	from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	to := Wall(time.Now(), time.UTC).Truncate(time.Minute)
	for _, logType := range catalog.Names() {
		t.Run(logType, func(t *testing.T) {
			loadFixtureRows(t, sa, logType)
			query, _ := catalog.Get(logType)
			statement, err := collectorconfig.DerivedTable(query.SQL)
			if err != nil {
				t.Fatal(err)
			}
			for _, dateTime := range []bool{false, true} {
				params := QueryParams{From: from, To: to, Limit: 10000, DateTime: dateTime}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				batch, err := source.Query(ctx, target, query.SQL, params)
				if err != nil {
					cancel()
					t.Fatalf("query: %v", err)
				}
				instants, _, err := checkContract(batch, from, to, params.Limit)
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				want := map[int64]int64{}
				for _, instant := range instants {
					want[int64(minuteEnd(instant).Sub(from)/time.Minute)]++
				}
				params.Limit = RecountLimit
				got, err := source.Recount(ctx, target, recountSQL(statement), params)
				cancel()
				if err != nil {
					t.Fatalf("recount (datetime bounds %v): %v", dateTime, err)
				}
				if len(want) == 0 || fmt.Sprint(got) != fmt.Sprint(want) {
					t.Fatalf("recount (datetime bounds %v) = %v, want the query's %d rows by minute: %v", dateTime, got, len(instants), want)
				}
			}
		})
	}
}

// The recount reads only the Instant index of the window, not the rows: both
// cycle tables of each log type get 50,000 copies of a fixture row, and the
// count of the 100,000 must take a few hundred logical reads, not one or
// more per row.
func TestRecountReadsTheInstantIndexAgainstSQLServer(t *testing.T) {
	server := newLiveServer(t)
	setupFixtures(t, server)
	sa := server.sa(t, fixtureDatabase)
	catalog := catalogoutsystems.DefaultCatalog()
	const copies = 50000
	views := regexp.MustCompile(`(?i)FROM\s+dbo\.(oslog_\w+)`)
	for _, logType := range catalog.Names() {
		t.Run(logType, func(t *testing.T) {
			loadFixtureRows(t, sa, logType)
			query, _ := catalog.Get(logType)
			current := views.FindStringSubmatch(query.SQL)[1]
			for _, view := range []string{current, current + "_Previous"} {
				copyRows(t, sa, cycleTable(t, sa, current, view), copies)
			}
			statement, err := collectorconfig.DerivedTable(query.SQL)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			conn, err := sa.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			sessionReads := func() int64 {
				var reads int64
				if err := conn.QueryRowContext(ctx, `SELECT logical_reads FROM sys.dm_exec_sessions WHERE session_id = @@SPID`).Scan(&reads); err != nil {
					t.Fatalf("logical reads: %v", err)
				}
				return reads
			}
			// The copies are a millisecond apart from 2025-01-01, before every
			// fixture row: the first two minutes after it hold them all.
			run := func() int64 {
				t.Helper()
				rows, err := conn.QueryContext(ctx, recountSQL(statement),
					sql.Named("from", civil.DateTimeOf(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))),
					sql.Named("to", civil.DateTimeOf(time.Date(2025, 1, 1, 0, 2, 0, 0, time.UTC))), sql.Named("limit", RecountLimit))
				if err != nil {
					t.Fatalf("recount: %v", err)
				}
				var total int64
				for rows.Next() {
					var minute, n int64
					if err := rows.Scan(&minute, &n); err != nil {
						t.Fatal(err)
					}
					total += n
				}
				if err := rows.Close(); err != nil || rows.Err() != nil {
					t.Fatalf("recount: %v %v", err, rows.Err())
				}
				return total
			}
			run() // compiles the plan and updates the stale statistics
			reads := int64(-1)
			for range 3 {
				before := sessionReads()
				if n := run(); n != 2*copies {
					t.Fatalf("recount counted %d rows, want %d", n, 2*copies)
				}
				if r := sessionReads() - before; reads < 0 || r < reads {
					reads = r
				}
			}
			t.Logf("%d rows counted, %d logical reads", 2*copies, reads)
			if reads > 2*copies/50 {
				t.Fatalf("%d logical reads to count %d rows: the recount reads the rows, not only the Instant index", reads, 2*copies)
			}
		})
	}
}
