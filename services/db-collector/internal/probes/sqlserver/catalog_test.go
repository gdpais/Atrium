package sqlserver

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	atriumconfig "atrium/internal/config"
	collectorexport "atrium/services/db-collector/internal/export"
)

// Every built-in query passes the check every query_template override must
// pass, so the two cannot drift apart.
func TestCatalogQueriesAreOneReadOnlySelect(t *testing.T) {
	for _, name := range DefaultCatalog().Names() {
		probe, _ := DefaultCatalog().Get(name)
		if err := atriumconfig.ValidateReadOnlySelect(probe.QueryTemplate); err != nil {
			t.Errorf("probe %s must be one read-only SELECT: it %v", name, err)
		}
	}
}

// A misspelled probe name is a rejected config, not a target that fails
// every cycle (usability test finding F4).
func TestValidateRejectsUnknownProbes(t *testing.T) {
	catalog := DefaultCatalog()
	collector := func(kind string, enabled bool, collectorProbe, targetProbe string) atriumconfig.CollectorRuntimeConfig {
		c := atriumconfig.CollectorRuntimeConfig{ID: "c", Kind: kind, Enabled: enabled}
		if collectorProbe != "" {
			c.Probes = []atriumconfig.ProbeRuntimeConfig{{Name: collectorProbe}}
		}
		c.Targets = []atriumconfig.TargetRuntimeConfig{{Name: "t", Probes: []atriumconfig.ProbeRuntimeConfig{{Name: targetProbe}}}}
		return c
	}
	const probes = "probes: blocking, buffer_cache, cpu, file_io, memory_pressure, sessions, storage, throughput, waits"
	tests := []struct {
		name      string
		collector atriumconfig.CollectorRuntimeConfig
		wantErr   string
	}{
		{"known probes", collector(atriumconfig.KindSQLServer, true, "waits", "file_io"), ""},
		{"unknown on the target", collector(atriumconfig.KindSQLServer, true, "", "wiats"), `collector c target t: unknown SQL Server probe "wiats"; ` + probes},
		{"unknown on the collector", collector(atriumconfig.KindSQLServer, true, "wiats", "waits"), `collector c: unknown SQL Server probe "wiats"; ` + probes},
		{"disabled collectors too", collector(atriumconfig.KindSQLServer, false, "", "wiats"), `unknown SQL Server probe "wiats"`},
		{"log types are not SQL Server probes", collector(atriumconfig.KindSQLServer, true, "", "error"), `unknown SQL Server probe "error"`},
		{"outsystems_logs collectors are not checked here", collector(atriumconfig.KindOutSystemsLogs, true, "error", "error"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := catalog.Validate(atriumconfig.RuntimeConfig{Collectors: []atriumconfig.CollectorRuntimeConfig{tt.collector}})
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestCatalogContainsRequiredProbes(t *testing.T) {
	catalog := DefaultCatalog()
	required := []string{"waits", "blocking", "sessions", "memory_pressure", "storage", "throughput", "cpu", "buffer_cache", "file_io"}
	for _, name := range required {
		probe, ok := catalog.Get(name)
		if !ok {
			t.Fatalf("missing required probe %q", name)
		}
		if probe.QueryTemplate == "" {
			t.Fatalf("probe %q has empty query template", name)
		}
		if len(probe.Metrics) == 0 {
			t.Fatalf("probe %q has no metric names", name)
		}
		for _, metric := range probe.Metrics {
			if metric.Name == "" {
				t.Fatalf("probe %q has metric with empty name", name)
			}
			if metric.ValueColumn == "" {
				t.Fatalf("probe %q metric %q has empty value column", name, metric.Name)
			}
		}
	}
}

// Renaming an exported metric breaks rules and dashboards, so names are
// checked against Prometheus conventions once, here: base units only, and
// _total on counters and nowhere else.
func TestCatalogMetricsFollowPrometheusConventions(t *testing.T) {
	validName := regexp.MustCompile(`^atrium_sqlserver_[a-z0-9_]+[a-z0-9]$`)
	nonBaseUnit := regexp.MustCompile(`_(ms|milliseconds|kb|mb|gb|kilobytes|megabytes|minutes|hours|percent)(_|$)`)
	seen := map[string]bool{}
	for _, probe := range DefaultCatalog().byName {
		for _, metric := range probe.Metrics {
			if seen[metric.Name] {
				t.Errorf("%s: emitted by more than one descriptor", metric.Name)
			}
			seen[metric.Name] = true
			if !validName.MatchString(metric.Name) {
				t.Errorf("%s: not a atrium_sqlserver_ snake_case name", metric.Name)
			}
			if nonBaseUnit.MatchString(metric.Name) {
				t.Errorf("%s: use base units (seconds, bytes, ratio) and Scale", metric.Name)
			}
			isTotal := strings.HasSuffix(metric.Name, "_total")
			switch metric.Type {
			case collectorexport.Counter:
				if !isTotal {
					t.Errorf("%s: counters must end in _total", metric.Name)
				}
			case collectorexport.Gauge:
				if isTotal {
					t.Errorf("%s: only counters end in _total", metric.Name)
				}
			default:
				t.Errorf("%s: unknown type %v", metric.Name, metric.Type)
			}
			if metric.Scale < 0 {
				t.Errorf("%s: negative scale %v", metric.Name, metric.Scale)
			}
			if metric.Help == "" {
				t.Errorf("%s: empty help", metric.Name)
			}
		}
	}
}

func TestMetricConvertAppliesScale(t *testing.T) {
	tests := []struct {
		name  string
		scale float64
		raw   float64
		want  float64
	}{
		{"zero scale is identity", 0, 42, 42},
		{"milliseconds to seconds", 0.001, 1500, 1.5},
		{"KB to bytes", 1024, 2, 2048},
		{"8 KB pages to bytes", 8192, 3, 24576},
		{"percent to ratio", 0.01, 50, 0.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (Metric{Scale: tt.scale}).Convert(tt.raw); got != tt.want {
				t.Fatalf("Convert(%v) with scale %v = %v, want %v", tt.raw, tt.scale, got, tt.want)
			}
		})
	}
}

func TestWaitsProbeExcludesBenignWaits(t *testing.T) {
	probe, _ := DefaultCatalog().Get("waits")
	waitType := regexp.MustCompile(`^[A-Z0-9_]+$`)
	seen := map[string]bool{}
	for _, wait := range benignWaitTypes {
		if !waitType.MatchString(wait) {
			t.Errorf("benign wait %q is not a wait type name", wait)
		}
		if seen[wait] {
			t.Errorf("benign wait %q listed twice", wait)
		}
		seen[wait] = true
		if !strings.Contains(probe.QueryTemplate, "N'"+wait+"'") {
			t.Errorf("waits query does not exclude %s", wait)
		}
	}
	for _, idle := range []string{"SLEEP_TASK", "LAZYWRITER_SLEEP", "XE_TIMER_EVENT", "REQUEST_FOR_DEADLOCK_SEARCH"} {
		if !seen[idle] {
			t.Errorf("%s must be excluded", idle)
		}
	}
	if got := sqlStringList([]string{"A", "B'C"}); got != "N'A', N'B''C'" {
		t.Errorf("sqlStringList = %s", got)
	}
}

// The scheduler monitor ring buffer holds up to 256 records; converting each
// to XML would cost far more than the one value read.  The query must pick
// the newest raw record first and convert only that one: no ORDER BY sorts
// on an XML value, the TOP (1) derived table converts nothing, and the only
// XML conversion comes after that table closes.
func TestCPUProbeConvertsOnlyTheLatestRecord(t *testing.T) {
	probe, _ := DefaultCatalog().Get("cpu")
	query := strings.ToLower(probe.QueryTemplate)
	usesXML := func(s string) bool { return strings.Contains(s, "xml") || strings.Contains(s, ".value(") }

	for rest := query; ; {
		i := strings.Index(rest, "order by")
		if i < 0 {
			break
		}
		rest = rest[i+len("order by"):]
		clause := rest
		if end := strings.Index(clause, ")"); end >= 0 {
			clause = clause[:end]
		}
		if usesXML(clause) {
			t.Errorf("cpu query orders by an XML value (%q), which converts every record", clause)
		}
	}

	open := strings.Index(query, "(select top (1)")
	if open < 0 {
		t.Fatalf("cpu query must pick the newest record in a (SELECT TOP (1) ...) derived table: %s", query)
	}
	end := closingParen(query, open)
	if end < 0 {
		t.Fatalf("unbalanced parentheses in cpu query: %s", query)
	}
	if derived := query[open : end+1]; usesXML(derived) {
		t.Errorf("TOP (1) derived table must select the raw record, not XML: %s", derived)
	}
	if strings.Count(query, "convert(xml") != 1 || strings.Contains(query, "as xml") {
		t.Errorf("cpu query must convert to XML exactly once, with CONVERT(xml: %s", query)
	}
	if convert := strings.Index(query, "convert(xml"); convert < end {
		t.Errorf("CONVERT(xml must come after the TOP (1) derived table closes: %s", query)
	}
}

// closingParen returns the index of the parenthesis that closes the one at
// open, skipping string literals, or -1.
func closingParen(s string, open int) int {
	depth, quoted := 0, false
	for i := open; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\'':
			quoted = !quoted
		case quoted:
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// The scheduler monitor writes a record a minute.  If it stops, the newest
// record must age out instead of being exported forever: the query keeps it
// only while it is younger than 3 minutes on the server's millisecond clock.
func TestCPUProbeSkipsStaleRecords(t *testing.T) {
	probe, _ := DefaultCatalog().Get("cpu")
	query := probe.QueryTemplate
	for _, part := range []string{"CROSS JOIN sys.dm_os_sys_info AS si", "WHERE si.ms_ticks - latest.timestamp < 180000"} {
		if !strings.Contains(query, part) {
			t.Errorf("cpu query lacks the staleness guard %q: %s", part, query)
		}
	}
}

// Other-process CPU is 100 - idle - SQL Server, floored at 0.  A missing value
// makes the difference NULL, which the floor's ELSE would turn into 0, so the
// guards belong in the outer CASE: no sample instead of a false 0.
func TestCPUProbeOtherProcessIsNullWithoutInputs(t *testing.T) {
	probe, _ := DefaultCatalog().Get("cpu")
	query := probe.QueryTemplate
	start, end := strings.Index(query, "CASE WHEN"), strings.Index(query, " AS other_process_percent")
	if start < 0 || end < start {
		t.Fatalf("cpu query has no CASE ... AS other_process_percent: %s", query)
	}
	expr := query[start:end]
	condition := expr[:strings.Index(expr, " THEN ")]
	for _, guard := range []string{"v.system_idle_percent IS NOT NULL", "v.sql_process_percent IS NOT NULL"} {
		if !strings.Contains(condition, guard) {
			t.Errorf("other_process_percent outer CASE condition lacks %q: %s", guard, condition)
		}
	}
	if !strings.Contains(expr, "ELSE 0 END END") {
		t.Errorf("other_process_percent must floor at 0 inside the guarded CASE and be NULL otherwise: %s", expr)
	}
}

// sys.dm_os_host_info needs SQL Server 2017 or later; on an older server the
// whole cpu batch would fail to compile and fail its target every cycle.  The
// platform comes from @@VERSION, which every version has.
func TestCPUProbeDetectsWindowsWithoutHostInfo(t *testing.T) {
	probe, _ := DefaultCatalog().Get("cpu")
	query := probe.QueryTemplate
	if strings.Contains(strings.ToLower(query), "dm_os_host_info") {
		t.Errorf("cpu query uses sys.dm_os_host_info (SQL Server 2017+): %s", query)
	}
	if !strings.Contains(query, "@@VERSION LIKE N'% on Windows%'") {
		t.Errorf("cpu query must detect Windows from @@VERSION: %s", query)
	}
}

// File I/O and file size series of one file share their labels, so panels
// and queries can join them.
func TestFileIOProbeLabelsMatchStorage(t *testing.T) {
	catalog := DefaultCatalog()
	storage, _ := catalog.Get("storage")
	fileIO, _ := catalog.Get("file_io")
	want := storage.Metrics[0].LabelColumns
	for _, metric := range fileIO.Metrics {
		if !slices.Equal(metric.LabelColumns, want) {
			t.Errorf("%s labels %v, want the storage probe's %v", metric.Name, metric.LabelColumns, want)
		}
	}
}

// DB_NAME() is NULL for a database the login cannot see or one dropped
// mid-query.  Both file probes fall back to the database id, the same way,
// so the label is never "<nil>" and two unnamed databases never collide.
func TestFileProbesNameDatabasesTheSameWay(t *testing.T) {
	catalog := DefaultCatalog()
	pattern := regexp.MustCompile(`COALESCE\(DB_NAME\((\w+\.)?database_id\), CONCAT\(N'database_id:', (\w+\.)?database_id\)\) AS database_name`)
	for _, name := range []string{"storage", "file_io"} {
		probe, _ := catalog.Get(name)
		if !pattern.MatchString(probe.QueryTemplate) {
			t.Errorf("%s query must name databases with %s: %s", name, pattern, probe.QueryTemplate)
		}
		if strings.Contains(probe.QueryTemplate, "SELECT DB_NAME(") {
			t.Errorf("%s query uses a bare DB_NAME(), which can be NULL: %s", name, probe.QueryTemplate)
		}
	}
}
