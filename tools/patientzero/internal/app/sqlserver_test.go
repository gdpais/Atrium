package app

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"

	"atrium/tools/patientzero/internal/logschema"
	"atrium/tools/patientzero/internal/oslog"
	"atrium/tools/patientzero/internal/shop"
)

// queriesDir holds the queries the db-collector's outsystems_logs collector
// runs; the live test runs them unchanged against what PatientZero wrote.
const queriesDir = "../../../../services/db-collector/internal/probes/outsystems/queries"

// TestLiveSQLServer runs the entry points against a real SQL Server and reads
// their rows back with the collector's own queries. It needs
// PATIENTZERO_TEST_SQLSERVER, a sqlserver:// URL for a login that may create
// databases (the sandbox's sa), and is skipped without it. It creates two
// databases with random names and drops them at the end.
func TestLiveSQLServer(t *testing.T) {
	admin := os.Getenv("PATIENTZERO_TEST_SQLSERVER")
	if admin == "" {
		t.Skip("PATIENTZERO_TEST_SQLSERVER is not set")
	}
	ctx := context.Background()
	business := openTestDatabase(t, admin, "pz_test")
	logging := openTestDatabase(t, admin, "pz_test_log")
	types, err := logschema.Load(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 { // safe to run again
		if err := Migrate(ctx, business, logging, types, "atrium_oslog_reader"); err != nil {
			t.Fatal(err)
		}
	}
	var tables, views int
	if err := logging.QueryRow(`SELECT
		(SELECT COUNT(*) FROM sys.tables WHERE name LIKE 'oslog[_]%'),
		(SELECT COUNT(*) FROM sys.views WHERE name LIKE 'oslog[_]%')`).Scan(&tables, &views); err != nil {
		t.Fatal(err)
	}
	wantTables := 0
	for _, lt := range types {
		wantTables += logschema.Cycles
		if lt.Detail != nil {
			wantTables += logschema.Cycles
		}
	}
	if tables != wantTables || views != len(types)*2 {
		t.Fatalf("%d cycle and detail tables and %d views, want %d and %d", tables, views, wantTables, len(types)*2)
	}

	schema := oslog.NewSchema(types)
	s := &Server{
		Store:    &shop.SQLStore{DB: business},
		Logs:     &oslog.SQLWriter{DB: logging, Schema: schema},
		Schema:   schema,
		FrontEnd: "FE-PZ-1",
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	from := time.Now().UTC().Add(-time.Minute)
	for _, c := range []struct {
		method, path, body string
		status             int
	}{
		{http.MethodPost, PathPlaceOrder, `{"productId":2,"quantity":3}`, http.StatusOK},
		{http.MethodGet, PathOrderList, "", http.StatusOK},
		{http.MethodGet, PathAPIOrders, "", http.StatusOK},
		{http.MethodPost, PathPlaceOrder, `{"productId":99,"quantity":1}`, http.StatusBadRequest},
	} {
		w := do(t, s, c.method, c.path, c.body, maria)
		if w.Code != c.status {
			t.Fatalf("%s %s: status %d, want %d: %s", c.method, c.path, w.Code, c.status, w.Body)
		}
		if c.path == PathOrderList && !strings.Contains(w.Body.String(), "Stethoscope") {
			t.Fatalf("the order placed first is not on the screen: %s", w.Body)
		}
	}

	// Every log type PatientZero wrote, read with the collector's query.
	got := map[string][]map[string]any{}
	for _, logType := range []string{"screen", "mobile_request", "integration", "general", "error"} {
		got[logType] = collect(t, logging, logType, from, time.Now().UTC().Add(time.Minute))
	}
	for logType, n := range map[string]int{"screen": 1, "mobile_request": 2, "integration": 1, "general": 1, "error": 1} {
		if len(got[logType]) != n {
			t.Fatalf("%s: the collector's query returned %d rows, want %d: %v", logType, len(got[logType]), n, got[logType])
		}
	}
	for logType, rows := range got {
		for _, row := range rows {
			if row["Request_Key"] == nil || row["Application_Name"] != Application {
				t.Errorf("%s row %v", logType, row)
			}
			for _, col := range []string{"Session_Id", "Username", "Client_IP"} {
				if v, ok := row[col]; ok && v != nil && v != maria[HeaderSession] && v != "maria.santos" && v != "10.20.30.41" {
					t.Errorf("%s.%s = %v", logType, col, v)
				}
			}
		}
	}
	errRow := got["error"][0]
	if errRow["Severity"] != nil && errRow["Severity"] != "ERROR" {
		t.Errorf("error severity %v", errRow["Severity"])
	}
	var failed map[string]any
	for _, row := range got["mobile_request"] {
		if row["Request_Key"] == errRow["Request_Key"] {
			failed = row
		}
	}
	if failed == nil || failed["Severity"] != "ERROR" {
		t.Errorf("the failed server action's mobile request row is not ERROR under the error's Request_Key: %v", got["mobile_request"])
	}
	for _, col := range []string{"Session_Id", "Username", "Client_IP"} {
		if got["screen"][0][col] == nil || got["mobile_request"][0][col] == nil || got["general"][0][col] == nil {
			t.Errorf("%s missing from the screen, mobile request or general row", col)
		}
	}

	// A trace longer than the error cycle table's Stack goes into the
	// current cycle's detail table too, and the collector's query returns it
	// in full through the view. A rotation only moves the views; the next
	// error goes into the tables of the cycle the current view now reads.
	errType := schema["error"]
	trace := strings.Repeat("at PatientZero_App.ActionRecalculateTotals(HeContext heContext)\n", 150)
	for i, cycle := range []int{logschema.InitialCurrent, logschema.InitialCurrent + 1} {
		if i > 0 {
			for _, stmt := range errType.RotateStatements(cycle, cycle-1) {
				if _, err := logging.ExecContext(ctx, stmt); err != nil {
					t.Fatalf("rotation: %v", err)
				}
			}
		}
		id, key := newGUID(), newGUID()
		row := oslog.Row{Type: "error", Columns: map[string]any{
			"Id": id, "Instant": time.Now(), "Session_Id": "", "User_Id": 0, "Espace_Id": appModule.ID, "Tenant_Id": TenantID,
			"Message": "Insufficient stack to continue executing the program safely.", "Stack": trace,
			"Module_Name": appModule.Name, "Server": "FE-PZ-1", "Request_Key": key, "Application_Name": Application,
		}}
		if err := s.Logs.Write(ctx, []oslog.Row{row}); err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
		var stored, full int
		if err := logging.QueryRow(fmt.Sprintf("SELECT (SELECT DATALENGTH(Stack) FROM dbo.%s WHERE Id = @p1), (SELECT DATALENGTH(FullStackTrace) FROM dbo.%s WHERE Id = @p1)",
			errType.Table(cycle), errType.Detail.Table(cycle)), id).Scan(&stored, &full); err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
		if stored != errType.Detail.Limit || full != len(trace) {
			t.Fatalf("cycle %d: %s.Stack holds %d characters and %s.FullStackTrace %d; want %d and %d",
				cycle, errType.Table(cycle), stored, errType.Detail.Table(cycle), full, errType.Detail.Limit, len(trace))
		}
		var read []map[string]any
		for _, r := range collect(t, logging, "error", from, time.Now().UTC().Add(time.Minute)) {
			if r["Request_Key"] == key {
				read = append(read, r)
			}
		}
		if len(read) != 1 || read[0]["Stack"] != trace {
			t.Fatalf("cycle %d: the collector's query returned %d rows for the long trace, want 1 with all %d characters of it", cycle, len(read), len(trace))
		}
	}
}

// openTestDatabase creates a database with a random name and drops it when
// the test ends.
func openTestDatabase(t *testing.T, admin, prefix string) *sql.DB {
	t.Helper()
	var b [4]byte
	_, _ = rand.Read(b[:])
	name := fmt.Sprintf("%s_%x", prefix, b)
	root, err := sql.Open("sqlserver", admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	if _, err := root.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("database", name)
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlserver", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		if _, err := root.Exec("ALTER DATABASE " + name + " SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE " + name); err != nil {
			t.Errorf("dropping %s: %v", name, err)
		}
	})
	return db
}

// collect runs the collector's query for the log type, binding the bounds as
// datetime as the collector does for a datetime Instant.
func collect(t *testing.T, db *sql.DB, logType string, from, to time.Time) []map[string]any {
	t.Helper()
	query, err := os.ReadFile(filepath.Join(queriesDir, logType+".sql"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(string(query),
		sql.Named("from", mssql.DateTime1(from)), sql.Named("to", mssql.DateTime1(to)), sql.Named("limit", 100))
	if err != nil {
		t.Fatalf("%s query: %v", logType, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		row := map[string]any{}
		for i, c := range cols {
			if b, ok := vals[i].([]byte); ok {
				vals[i] = string(b)
			}
			row[c] = vals[i]
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s query: %v", logType, err)
	}
	return out
}

// TestLiveSlowSQL checks that a sql-latency fault slows the business
// statement in SQL Server itself: while the request waits, SQL Server shows
// its session running a request in the business database, and the
// slow-query row PatientZero writes is read by the collector's general
// query with the statement's time as its Duration. Same requirements as
// TestLiveSQLServer.
func TestLiveSlowSQL(t *testing.T) {
	admin := os.Getenv("PATIENTZERO_TEST_SQLSERVER")
	if admin == "" {
		t.Skip("PATIENTZERO_TEST_SQLSERVER is not set")
	}
	ctx := context.Background()
	business := openTestDatabase(t, admin, "pz_test")
	logging := openTestDatabase(t, admin, "pz_test_log")
	types, err := logschema.Load(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, business, logging, types, "atrium_oslog_reader"); err != nil {
		t.Fatal(err)
	}
	root, err := sql.Open("sqlserver", admin)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var database string
	if err := business.QueryRow("SELECT DB_NAME()").Scan(&database); err != nil {
		t.Fatal(err)
	}

	schema := oslog.NewSchema(types)
	s := &Server{
		Store:    &shop.SQLStore{DB: business},
		Logs:     &oslog.SQLWriter{DB: logging, Schema: schema},
		Schema:   schema,
		FrontEnd: "FE-PZ-1",
		SlowSQL:  DefaultSlowSQL,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	switchOn(t, s, "sql-latency", map[string]any{"entry_points": []any{EntryOrderList}, "delay": "2s"})
	from := time.Now().UTC().Add(-time.Minute)
	done := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, PathOrderList, nil)
		for k, v := range maria {
			req.Header.Set(k, v)
		}
		s.Handler().ServeHTTP(w, req)
		done <- w.Code
	}()
	// What the sessions probe counts: a session running a request, here
	// one waiting in the business database. Caught before or after its
	// WAITFOR, the request runs with no wait type (NULL), so this polls
	// until it is seen waiting.
	var status string
	var wait sql.NullString
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		err := root.QueryRow(`SELECT TOP (1) s.status, r.wait_type
			FROM sys.dm_exec_requests AS r JOIN sys.dm_exec_sessions AS s ON s.session_id = r.session_id
			WHERE r.database_id = DB_ID(@p1) AND r.session_id <> @@SPID`, database).Scan(&status, &wait)
		if err == nil && wait.String == "WAITFOR" {
			break
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if code := <-done; code != http.StatusOK {
		t.Fatalf("OrderList answered %d", code)
	}
	if status != "running" || wait.String != "WAITFOR" {
		t.Fatalf("SQL Server showed the slowed request as session %q waiting %q, want running and WAITFOR", status, wait.String)
	}

	var slow []map[string]any
	for _, row := range collect(t, logging, "general", from, time.Now().UTC().Add(time.Minute)) {
		if row["Module_Name"] == "SLOWSQL" {
			slow = append(slow, row)
		}
	}
	if len(slow) != 1 {
		t.Fatalf("the collector's general query returned %d slow-query rows, want 1", len(slow))
	}
	if d, ok := slow[0]["Duration"].(int64); !ok || d < 2000 || d > 5000 {
		t.Errorf("slow-query Duration %v (%T), want the 2 s or a little more the statement took", slow[0]["Duration"], slow[0]["Duration"])
	}
	screen := collect(t, logging, "screen", from, time.Now().UTC().Add(time.Minute))
	if len(screen) != 1 || screen[0]["Request_Key"] != slow[0]["Request_Key"] {
		t.Fatalf("screen rows %v, want one under the slow query's Request_Key", screen)
	}
}

// TestLiveRotation rotates a logging database as the rotation fault does,
// between two requests: every log type's views move one cycle on, the
// second request's rows go into the new current week, and the collector's
// queries, which read both weeks, return both requests' rows once. Same
// requirements as TestLiveSQLServer.
func TestLiveRotation(t *testing.T) {
	admin := os.Getenv("PATIENTZERO_TEST_SQLSERVER")
	if admin == "" {
		t.Skip("PATIENTZERO_TEST_SQLSERVER is not set")
	}
	ctx := context.Background()
	business := openTestDatabase(t, admin, "pz_test")
	logging := openTestDatabase(t, admin, "pz_test_log")
	types, err := logschema.Load(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, business, logging, types, "atrium_oslog_reader"); err != nil {
		t.Fatal(err)
	}
	schema := oslog.NewSchema(types)
	writer := &oslog.SQLWriter{DB: logging, Schema: schema}
	s := &Server{
		Store: &shop.SQLStore{DB: business}, Logs: writer, Schema: schema, FrontEnd: "FE-PZ-1",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	from := time.Now().UTC().Add(-time.Minute)
	// A refused order writes an error row too, so the error log, whose rows
	// go into the cycle table its view reads, rotates with the others.
	do(t, s, http.MethodPost, PathPlaceOrder, `{"productId":99,"quantity":1}`, maria)
	cycles, err := writer.Rotate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, lt := range types {
		if cycles[lt.Name] != logschema.InitialCurrent+1 {
			t.Errorf("%s: current view reads cycle %d, want %d", lt.Name, cycles[lt.Name], logschema.InitialCurrent+1)
		}
		var previous string
		if err := logging.QueryRow(`SELECT TOP (1) referenced_entity_name FROM sys.sql_expression_dependencies
			WHERE referencing_id = OBJECT_ID(@p1) AND referenced_entity_name NOT LIKE '%Detail%'`, "dbo."+lt.PreviousView()).Scan(&previous); err != nil {
			t.Fatal(err)
		}
		if previous != lt.Table(logschema.InitialCurrent) {
			t.Errorf("%s: previous view reads %s, want %s", lt.Name, previous, lt.Table(logschema.InitialCurrent))
		}
	}
	do(t, s, http.MethodPost, PathPlaceOrder, `{"productId":99,"quantity":1}`, maria)
	to := time.Now().UTC().Add(time.Minute)
	for _, logType := range []string{"mobile_request", "error"} {
		rows := collect(t, logging, logType, from, to)
		if len(rows) != 2 || rows[0]["Request_Key"] == rows[1]["Request_Key"] {
			t.Errorf("%s: the collector's query returned %d rows across the rotation, want both requests' once: %v", logType, len(rows), rows)
		}
	}
	var after int
	if err := logging.QueryRow("SELECT COUNT(*) FROM dbo." + schema["error"].Table(logschema.InitialCurrent+1)).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != 1 {
		t.Errorf("%d error rows in the new current cycle table, want the second request's", after)
	}
}
