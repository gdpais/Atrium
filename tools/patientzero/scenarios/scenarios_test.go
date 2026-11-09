package scenarios_test

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"

	"atrium/tools/patientzero/internal/app"
	"atrium/tools/patientzero/internal/cluster"
	"atrium/tools/patientzero/internal/inject"
	"atrium/tools/patientzero/internal/scenario"
	"atrium/tools/patientzero/scenarios"
)

// registry is the registry PatientZero's main builds.
func registry() *scenario.Registry {
	reg := scenario.NewRegistry()
	inject.Register(reg, &inject.Injector{}, app.Modules)
	cluster.Register(reg, nil)
	return reg
}

// Every scenario file in the image must load: PatientZero refuses to start
// otherwise.
func TestScenarioFilesLoad(t *testing.T) {
	list, err := scenario.LoadDir(scenarios.FS, ".", registry())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) == 0 {
		t.Fatal("no scenario files")
	}
	// A scenario is a test: it injects something and expects something.
	for _, s := range list {
		if len(s.FaultTypes()) == 0 || len(s.Signals) == 0 {
			t.Errorf("%s: needs at least one fault and one signal", s.Name)
		}
	}
}

func readSchema(t *testing.T) map[string]any {
	t.Helper()
	b, err := os.ReadFile("scenario.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("scenario.schema.json: %v", err)
	}
	return m
}

func path(t *testing.T, m map[string]any, keys ...string) any {
	t.Helper()
	var cur any = m
	for _, k := range keys {
		obj, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("schema: %v: not an object at %q", keys, k)
		}
		cur = obj[k]
	}
	return cur
}

func strings(t *testing.T, v any) []string {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("schema: %v is not a list", v)
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, e.(string))
	}
	sort.Strings(out)
	return out
}

func number(t *testing.T, v any) int {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("schema: %v is not a number", v)
	}
	return int(f)
}

// The schema is for editors; the Go validation is what PatientZero
// enforces. The lists and limits they share must agree, or the schema would
// accept a file PatientZero refuses (or the reverse).
func TestSchemaMatchesValidation(t *testing.T) {
	s := readSchema(t)
	var types []string
	for _, ft := range registry().Types() {
		types = append(types, ft.Name)
	}
	if got := strings(t, path(t, s, "$defs", "fault", "properties", "type", "enum")); !slices.Equal(got, types) {
		t.Errorf("schema fault types %v, registry %v", got, types)
	}
	entryPoints := slices.Sorted(slices.Values(app.EntryPoints))
	if got := strings(t, path(t, s, "$defs", "entryPoints", "items", "enum")); !slices.Equal(got, entryPoints) {
		t.Errorf("schema entry points %v, app.EntryPoints %v", got, entryPoints)
	}
	var modules []string
	for m := range app.Modules {
		modules = append(modules, m)
	}
	sort.Strings(modules)
	if got := strings(t, path(t, s, "$defs", "module", "enum")); !slices.Equal(got, modules) {
		t.Errorf("schema modules %v, app.Modules %v", got, modules)
	}
	logTypes := slices.Sorted(slices.Values(scenario.LogTypes))
	if got := strings(t, path(t, s, "$defs", "signal", "properties", "delivery", "properties", "log_types", "items", "enum")); !slices.Equal(got, logTypes) {
		t.Errorf("schema delivery log types %v, scenario.LogTypes %v", got, logTypes)
	}
	loads := slices.Sorted(slices.Values(scenario.LoadProfiles))
	if got := strings(t, path(t, s, "properties", "load", "enum")); !slices.Equal(got, loads) {
		t.Errorf("schema load profiles %v, scenario.LoadProfiles %v", got, loads)
	}
	if got := number(t, path(t, s, "properties", "steps", "maxItems")); got != scenario.MaxSteps {
		t.Errorf("schema steps maxItems %d, scenario.MaxSteps %d", got, scenario.MaxSteps)
	}
	if got := number(t, path(t, s, "properties", "signals", "maxItems")); got != scenario.MaxSignals {
		t.Errorf("schema signals maxItems %d, scenario.MaxSignals %d", got, scenario.MaxSignals)
	}
	// Each fault type has its parameters in the schema.
	allOf, _ := path(t, s, "$defs", "fault", "allOf").([]any)
	var described []string
	for _, a := range allOf {
		cond := path(t, a.(map[string]any), "if", "properties", "type").(map[string]any)
		if c, ok := cond["const"].(string); ok {
			described = append(described, c)
		} else {
			described = append(described, strings(t, cond["enum"])...)
			workloads := strings(t, path(t, a.(map[string]any), "then", "properties", "params", "properties", "workload", "enum"))
			if want := slices.Sorted(slices.Values(cluster.Workloads)); !slices.Equal(workloads, want) {
				t.Errorf("schema workloads %v, cluster.Workloads %v", workloads, want)
			}
		}
		if path(t, a.(map[string]any), "if", "properties", "type", "const") == "log-flood" {
			params := path(t, a.(map[string]any), "then", "properties", "params", "properties").(map[string]any)
			if rows := number(t, path(t, params, "rows", "maximum")); rows != inject.MaxFloodRows {
				t.Errorf("schema log-flood rows maximum %d, inject.MaxFloodRows %d", rows, inject.MaxFloodRows)
			}
			if bytes := number(t, path(t, params, "bytes", "maximum")); bytes != inject.MaxFloodBytes {
				t.Errorf("schema log-flood bytes maximum %d, inject.MaxFloodBytes %d", bytes, inject.MaxFloodBytes)
			}
		}
		for typ, field := range map[string]string{"errors": "message", "deploy": "exception"} {
			if path(t, a.(map[string]any), "if", "properties", "type", "const") != typ {
				continue
			}
			max := number(t, path(t, a.(map[string]any), "then", "properties", "params", "properties", field, "maxLength"))
			if max != inject.MaxMessageLen {
				t.Errorf("schema %s %s maxLength %d, inject.MaxMessageLen %d", typ, field, max, inject.MaxMessageLen)
			}
		}
	}
	sort.Strings(described)
	if !slices.Equal(described, types) {
		t.Errorf("schema describes the params of %v, registry has %v", described, types)
	}
}

// make patientzero-scenarios, and the scenarios workflow with it, runs the
// scenarios in SCENARIO_ORDER (scripts/patientzero.sh): every scenario file
// must have its place there, chosen for what it leaves behind.
func TestEveryScenarioHasAPlaceInTheOrder(t *testing.T) {
	script, err := os.ReadFile("../scripts/patientzero.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^SCENARIO_ORDER="([^"]*)"$`).FindSubmatch(script)
	if m == nil {
		t.Fatal(`no SCENARIO_ORDER="..." line in scripts/patientzero.sh`)
	}
	var order []string
	for _, name := range bytes.Fields(m[1]) {
		order = append(order, string(name))
	}
	files, err := fs.Glob(scenarios.FS, "*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, file := range files {
		names = append(names, file[:len(file)-len(".yaml")])
	}
	sorted := slices.Clone(order)
	slices.Sort(sorted)
	if !slices.Equal(sorted, names) || len(slices.Compact(sorted)) != len(order) {
		t.Fatalf("SCENARIO_ORDER = %v, want each scenario file once: %v", order, names)
	}
	if order[len(order)-1] != "late-commits" {
		t.Errorf("late-commits must run last: its late-row alert stays firing for 15 minutes")
	}
}

// Every alert the chart ships is either fired by a scenario or listed in the
// README's "Not covered" table, so the table says what the runs leave out.
// Every alert a scenario checks is one the chart ships: a check that one
// stays quiet would pass for good on a name the rules no longer have.
func TestEveryAlertIsFiredOrNotCovered(t *testing.T) {
	const rules = "../../../infra/helm/atrium/files/prometheus/rules"
	files, err := filepath.Glob(rules + "/*.yml")
	if err != nil {
		t.Fatal(err)
	}
	generated, err := filepath.Glob(rules + "/generated/*.yml")
	if err != nil {
		t.Fatal(err)
	}
	var shipped []string
	for _, file := range append(files, generated...) {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Groups []struct {
				Rules []struct {
					Alert string `yaml:"alert"`
				} `yaml:"rules"`
			} `yaml:"groups"`
		}
		if err := yaml.Unmarshal(data, &doc); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		for _, g := range doc.Groups {
			for _, r := range g.Rules {
				if r.Alert != "" {
					shipped = append(shipped, r.Alert)
				}
			}
		}
	}
	if len(shipped) == 0 {
		t.Fatalf("no alerts in %s", rules)
	}
	slices.Sort(shipped)
	shipped = slices.Compact(shipped)

	list, err := scenario.LoadDir(scenarios.FS, ".", registry())
	if err != nil {
		t.Fatal(err)
	}
	fired := map[string]bool{}
	for _, s := range list {
		for _, sig := range s.Signals {
			if sig.Kind() != scenario.KindAlert {
				continue
			}
			if !slices.Contains(shipped, sig.Alert) {
				t.Errorf("%s: checks %s, which is not an alert in %s", s.Name, sig.Alert, rules)
			}
			if sig.State == scenario.StateFiring {
				fired[sig.Alert] = true
			}
		}
	}

	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	section := regexp.MustCompile(`(?ms)^### Not covered\r?\n(.*?)^#`).FindSubmatch(readme)
	if section == nil {
		t.Fatal(`no "### Not covered" section in README.md`)
	}
	// The table's rows, after its header and separator line. A row in any
	// other form fails here, not as an alert missing from the table.
	var rows [][]byte
	for _, line := range bytes.Split(section[1], []byte("\n")) {
		if bytes.HasPrefix(line, []byte("|")) {
			rows = append(rows, bytes.TrimRight(line, "\r"))
		}
	}
	row := regexp.MustCompile("^\\| `(\\w+)` \\|")
	listed := map[string]bool{}
	for _, line := range rows[min(2, len(rows)):] {
		m := row.FindSubmatch(line)
		switch {
		case m == nil:
			t.Errorf("README's Not covered table: %q does not start with an alert name in backticks", line)
		case listed[string(m[1])]:
			t.Errorf("%s: in the README's Not covered table twice", m[1])
		default:
			listed[string(m[1])] = true
		}
	}

	for _, alert := range shipped {
		switch {
		case fired[alert] && listed[alert]:
			t.Errorf("%s: a scenario fires it; take it out of the README's Not covered table", alert)
		case !fired[alert] && !listed[alert]:
			t.Errorf("%s: no scenario fires it; add a scenario that does, or the alert to the README's Not covered table", alert)
		}
		delete(listed, alert)
	}
	for _, alert := range slices.Sorted(maps.Keys(listed)) {
		t.Errorf("%s: in the README's Not covered table but not an alert in %s", alert, rules)
	}
}
