package scale

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"atrium/tools/patientzero/internal/logschema"
)

const queriesDir = "../../../../services/db-collector/internal/probes/outsystems/queries"

// TestLiveScale fills a small scale database on a real SQL Server and
// measures the collector's queries on it. It needs
// PATIENTZERO_TEST_SQLSERVER, a sqlserver:// URL for a login that may create
// databases (the sandbox's sa), and is skipped without it.
func TestLiveScale(t *testing.T) {
	server := os.Getenv("PATIENTZERO_TEST_SQLSERVER")
	if server == "" {
		t.Skip("PATIENTZERO_TEST_SQLSERVER is not set")
	}
	ctx := context.Background()
	var b [4]byte
	_, _ = rand.Read(b[:])
	name := fmt.Sprintf("%s_test_%x", DatabasePrefix, b)
	master, err := Open(server, "master")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	db, err := Open(server, name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		if err := Drop(ctx, master, name); err != nil {
			t.Errorf("dropping %s: %v", name, err)
		}
	})
	types, err := logschema.Load(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Create(ctx, master, name); err != nil {
		t.Fatal(err)
	}
	if err := Schema(ctx, db, types, "atrium_oslog_reader", ""); err != nil {
		t.Fatal(err)
	}
	sink := NewBulkSink(ctx, db, types)
	end, _ := time.Parse(time.RFC3339, "2026-10-20T12:00:00Z")
	exp, err := Generate(Options{Profile: Surveyed().Scaled(0.002), Seed: 1, End: end}, sink, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Every generated row is in the views, the previous week's behind
	// oslog_<TYPE>_Previous.
	for _, lt := range types {
		for i, view := range []string{lt.PreviousView(), lt.View} {
			var got int
			if err := db.QueryRow("SELECT COUNT(*) FROM dbo." + view).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if want := exp.Weeks[i].Rows[lt.Name]; got != want {
				t.Errorf("dbo.%s holds %d rows, expected.json says %d", view, got, want)
			}
		}
	}
	// A long trace is cut in the cycle table and whole in the detail table,
	// and the view returns it whole.
	var cut, whole, longest int
	if err := db.QueryRow(`SELECT
    (SELECT COUNT(*) FROM dbo.oslog_Error_0 WHERE DATALENGTH(Stack) = 2000) + (SELECT COUNT(*) FROM dbo.oslog_Error_1 WHERE DATALENGTH(Stack) = 2000),
    (SELECT COUNT(*) FROM dbo.oslog_Error_Detail_0) + (SELECT COUNT(*) FROM dbo.oslog_Error_Detail_1),
    (SELECT MAX(DATALENGTH(Stack)) FROM dbo.oslog_Error_Previous)`).Scan(&cut, &whole, &longest); err != nil {
		t.Fatal(err)
	}
	if whole == 0 || cut != whole || longest <= 2000 {
		t.Errorf("%d traces cut to 2000 characters, %d in the detail tables, the longest Stack through the view %d characters", cut, whole, longest)
	}

	windows, err := Windows(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	results, err := Measure(ctx, db, queriesDir, windows, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != len(types)*len(windows) {
		t.Fatalf("%d results, want %d", len(results), len(types)*len(windows))
	}
	views := map[string]string{}
	for _, lt := range types {
		views[lt.Name] = lt.View
	}
	sizes, err := Sizes(ctx, db, views)
	if err != nil {
		t.Fatal(err)
	}
	var report strings.Builder
	if err := Report(&report, name, sizes, windows, results, 5000); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"| screen | busiest minute |", "| error | 7 day backlog |", "Slowest:"} {
		if !strings.Contains(report.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, report.String())
		}
	}
	var screenRows int64
	for _, s := range sizes {
		if s.LogType == "screen" {
			screenRows = s.Rows
		}
	}
	if want := int64(exp.Weeks[0].Rows["screen"] + exp.Weeks[1].Rows["screen"]); screenRows != want {
		t.Errorf("sizes: %d screen rows, want %d", screenRows, want)
	}
}
