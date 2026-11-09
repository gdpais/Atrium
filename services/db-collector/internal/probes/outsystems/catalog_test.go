package outsystems

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	atriumconfig "atrium/internal/config"
)

func TestDefaultCatalogEmbedsEveryQueryFile(t *testing.T) {
	catalog := DefaultCatalog()
	names := catalog.Names()
	if !slices.Contains(names, "error") {
		t.Fatalf("catalog %v lacks the error log type", names)
	}
	for _, name := range names {
		query, ok := catalog.Get(name)
		if !ok || query.LogType != name || strings.TrimSpace(query.SQL) == "" {
			t.Errorf("log type %s: got %+v, %v", name, query, ok)
		}
	}
	if _, ok := catalog.Get("nope"); ok {
		t.Error("Get found an unknown log type")
	}
}

// Every query must follow the contract in queries/README.md as far as text
// can tell; the SQL Server tests run them.
func TestQueriesFollowTheContract(t *testing.T) {
	sessionSettings := regexp.MustCompile(`(?i)\bSET\s+(LOCK_TIMEOUT|DEADLOCK_PRIORITY|TRANSACTION)\b|\bBEGIN\s+TRAN`)
	catalog := DefaultCatalog()
	for _, name := range catalog.Names() {
		query, _ := catalog.Get(name)
		for _, want := range []string{"TOP (@limit)", "@from", "@to", "Instant", "ORDER BY"} {
			if !strings.Contains(query.SQL, want) {
				t.Errorf("%s.sql does not contain %q", name, want)
			}
		}
		if sessionSettings.MatchString(query.SQL) {
			t.Errorf("%s.sql sets session options or opens a transaction; the collector does that", name)
		}
	}
}

// Every query reads both weeks' views, each cut to @limit rows in Instant
// order before the union. A TOP only around the union makes SQL Server read
// and sort every row in (@from, @to] first, which after an outage is the
// whole backlog (README.md, Files); the SQL Server tests measure it.
func TestQueriesLimitEachWeekBeforeTheUnion(t *testing.T) {
	view := regexp.MustCompile(`(?i)FROM\s+dbo\.(oslog_\w+)`)
	limited := regexp.MustCompile(`(?is)SELECT\s+TOP\s+\(@limit\)\s+\*\s+FROM\s+dbo\.(oslog_\w+)\s+WHERE\s+Instant\s*>\s*@from\s+AND\s+Instant\s*<=\s*@to\s+ORDER\s+BY\s+Instant\s*\)`)
	catalog := DefaultCatalog()
	for _, name := range catalog.Names() {
		query, _ := catalog.Get(name)
		var views, limitedViews []string
		for _, m := range view.FindAllStringSubmatch(query.SQL, -1) {
			views = append(views, m[1])
		}
		for _, m := range limited.FindAllStringSubmatch(query.SQL, -1) {
			limitedViews = append(limitedViews, m[1])
		}
		if len(views) != 2 || !strings.EqualFold(views[1], views[0]+"_Previous") {
			t.Errorf("%s.sql reads %v, want the current week's view and its _Previous", name, views)
			continue
		}
		if !slices.Equal(limitedViews, views) {
			t.Errorf("%s.sql: %v are not each read as SELECT TOP (@limit) * FROM ... WHERE Instant > @from AND Instant <= @to ORDER BY Instant; limited: %v", name, views, limitedViews)
		}
	}
}

func TestCatalogRejectsInvalidFileNames(t *testing.T) {
	for _, name := range []string{"Error.sql", "mobile-request.sql", "1error.sql"} {
		fsys := fstest.MapFS{"queries/" + name: {Data: []byte("SELECT 1")}}
		if _, err := newCatalog(fsys, "queries"); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	fsys := fstest.MapFS{"queries/mobile_request.sql": {Data: []byte("SELECT 1")}, "queries/README.md": {Data: []byte("x")}}
	catalog, err := newCatalog(fsys, "queries")
	if err != nil || !slices.Equal(catalog.Names(), []string{"mobile_request"}) {
		t.Fatalf("got %v, %v", catalog.Names(), err)
	}
}

func TestValidateRejectsUnknownLogTypes(t *testing.T) {
	catalog := DefaultCatalog()
	collector := func(kind string, enabled bool, collectorProbe, targetProbe string) atriumconfig.CollectorRuntimeConfig {
		c := atriumconfig.CollectorRuntimeConfig{ID: "c", Kind: kind, Enabled: enabled}
		if collectorProbe != "" {
			c.Probes = []atriumconfig.ProbeRuntimeConfig{{Name: collectorProbe}}
		}
		c.Targets = []atriumconfig.TargetRuntimeConfig{{Name: "t", Probes: []atriumconfig.ProbeRuntimeConfig{{Name: targetProbe}}}}
		return c
	}
	tests := []struct {
		name      string
		collector atriumconfig.CollectorRuntimeConfig
		wantErr   bool
	}{
		{"known log type", collector(atriumconfig.KindOutSystemsLogs, true, "error", "error"), false},
		{"unknown on the target", collector(atriumconfig.KindOutSystemsLogs, true, "", "screen_typo"), true},
		{"unknown on the collector", collector(atriumconfig.KindOutSystemsLogs, true, "screen_typo", "error"), true},
		{"disabled collectors too", collector(atriumconfig.KindOutSystemsLogs, false, "", "screen_typo"), true},
		{"SQL Server probes are not log types", collector(atriumconfig.KindSQLServer, true, "waits", "waits"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := catalog.Validate(atriumconfig.RuntimeConfig{Collectors: []atriumconfig.CollectorRuntimeConfig{tt.collector}})
			if tt.wantErr != (err != nil) {
				t.Fatalf("wantErr %v, got %v", tt.wantErr, err)
			}
			if err != nil && !strings.Contains(err.Error(), `unknown OutSystems log type "screen_typo"; log types: cyclic_job, error, extension, general, integration, mobile_request, request_event, screen`) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// Every built-in query passes the check every query_template override must
// pass, so the two cannot drift apart.
func TestQueriesAreOneReadOnlySelect(t *testing.T) {
	catalog := DefaultCatalog()
	for _, name := range catalog.Names() {
		query, _ := catalog.Get(name)
		if err := atriumconfig.ValidateReadOnlySelect(query.SQL); err != nil {
			t.Errorf("%s.sql must be one read-only SELECT: it %v", name, err)
		}
	}
}
