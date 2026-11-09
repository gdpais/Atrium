package oslog

import (
	"strings"
	"testing"
	"time"

	"atrium/tools/patientzero/internal/logschema"
)

const fixtureDir = "../../../../services/db-collector/internal/collectors/outsystems/testdata/oslog"

func schema(t *testing.T) Schema {
	t.Helper()
	types, err := logschema.Load(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	return NewSchema(types)
}

func TestCheckRejectsUnknownTypesAndColumns(t *testing.T) {
	s := schema(t)
	if err := s.Check([]Row{{Type: "screen", Columns: map[string]any{"instant": time.Now(), "Request_Key": "k"}}}); err != nil {
		t.Fatalf("valid row: %v", err)
	}
	if err := s.Check([]Row{{Type: "audit", Columns: nil}}); err == nil || !strings.Contains(err.Error(), "audit") {
		t.Fatalf("unknown log type: %v", err)
	}
	if err := s.Check([]Row{{Type: "screen", Columns: map[string]any{"Error_Id": ""}}}); err == nil || !strings.Contains(err.Error(), "oslog_Screen has no column Error_Id") {
		t.Fatalf("unknown column: %v", err)
	}
}

func TestInsertGoesThroughTheCurrentViewInUTC(t *testing.T) {
	w := &SQLWriter{Schema: schema(t)}
	at := time.Date(2026, 10, 19, 11, 0, 0, 0, time.FixedZone("WEST", 3600))
	stmts, err := w.inserts(Row{Type: "screen", Columns: map[string]any{"Instant": at, "Screen": "OrderList", "Duration": 5}}, 4)
	if err != nil || len(stmts) != 1 {
		t.Fatalf("%d statements, %v", len(stmts), err)
	}
	if want := "INSERT INTO dbo.oslog_Screen ([Duration], [Instant], [Screen]) VALUES (@p1, @p2, @p3);"; stmts[0].query != want {
		t.Fatalf("query\n got %s\nwant %s", stmts[0].query, want)
	}
	got, ok := stmts[0].args[1].(time.Time)
	if !ok || !got.Equal(at) || got.Location() != time.UTC {
		t.Fatalf("Instant bound as %#v, want the time in UTC", stmts[0].args[1])
	}
}

// The error view computes Stack from the detail table, so error rows go
// into the current cycle table, and a long trace into its detail table too.
func TestErrorRowsGoIntoTheCurrentCycleTables(t *testing.T) {
	w := &SQLWriter{Schema: schema(t)}
	short := Row{Type: "error", Columns: map[string]any{"Id": "e1", "Message": "boom", "Stack": "at A()"}}
	stmts, err := w.inserts(short, 4)
	if err != nil || len(stmts) != 1 {
		t.Fatalf("short trace: %d statements, %v", len(stmts), err)
	}
	if want := "INSERT INTO dbo.oslog_Error_4 ([Id], [Message], [Stack]) VALUES (@p1, @p2, @p3);"; stmts[0].query != want {
		t.Fatalf("query\n got %s\nwant %s", stmts[0].query, want)
	}

	trace := strings.Repeat("at ssPatientZero_App.Actions.ActionPlaceOrder(HeContext heContext)\n", 60)
	long := Row{Type: "error", Columns: map[string]any{"Id": "e2", "Message": "boom", "Stack": trace}}
	stmts, err = w.inserts(long, 7)
	if err != nil || len(stmts) != 2 {
		t.Fatalf("long trace: %d statements, %v", len(stmts), err)
	}
	if want := "INSERT INTO dbo.oslog_Error_7 ([Id], [Message], [Stack]) VALUES (@p1, @p2, @p3);"; stmts[0].query != want {
		t.Fatalf("query\n got %s\nwant %s", stmts[0].query, want)
	}
	if got := stmts[0].args[2]; got != trace[:2000] {
		t.Fatalf("cycle table Stack has %d characters, want the trace's first 2000", len(got.(string)))
	}
	if want := "INSERT INTO dbo.oslog_Error_Detail_7 ([FullStackTrace], [Id]) VALUES (@p1, @p2);"; stmts[1].query != want {
		t.Fatalf("query\n got %s\nwant %s", stmts[1].query, want)
	}
	if stmts[1].args[0] != trace || stmts[1].args[1] != "e2" {
		t.Fatalf("detail args %v", stmts[1].args)
	}
	if long.Columns["Stack"] != trace {
		t.Fatal("inserts changed the row")
	}

	delete(long.Columns, "Id")
	if _, err := w.inserts(long, 7); err == nil || !strings.Contains(err.Error(), "needs Id") {
		t.Fatalf("a long trace without an Id: %v", err)
	}
}
