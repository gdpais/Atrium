package inventory

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"atrium/internal/config"
	configschema "atrium/packages/config-schema"
)

// exampleFile is config/inventory.example.yaml, which the reference docs
// point to: it must stay valid.
func exampleFile(t *testing.T) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(here), "..", "..", "..", "..", "config", "inventory.example.yaml")
}

// kind's values declare local-dev's inventory (api.inventory), which every
// kind cluster applies: it must stay valid, and it must be the environment
// the kind collectors report.
func TestKindsInventoryIsValid(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "..", "..", "..", "infra", "helm", "values", "kind.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		API struct {
			Inventory string `yaml:"inventory"`
		} `yaml:"api"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	f, err := ParseFile([]byte(values.API.Inventory))
	if err != nil {
		t.Fatal(err)
	}
	if f.Environment.Slug != "local-dev" || len(f.Applications) != 1 || f.Applications[0].Name != "PatientZero" {
		t.Fatalf("kind's inventory = %+v, want local-dev with PatientZero", f)
	}
	// PatientZero's database target is the sandbox SQL Server, under its own
	// name: sqlserver-dev is the static collector's target in
	// sqlserver-dev.yaml. scripts/kind.sh rewrites the host for a throwaway
	// cluster's container, so it must stay this exact string.
	if len(f.DatabaseTargets) != 1 {
		t.Fatalf("kind's inventory has %d database targets, want PatientZero's", len(f.DatabaseTargets))
	}
	target := f.DatabaseTargets[0]
	if target.Name != "patientzero" || target.Host != "atrium-sqlserver-dev" || target.CredentialRef != "env/sqlserver-dev" || len(target.Probes) == 0 {
		t.Fatalf("kind's database target = %+v, want patientzero on atrium-sqlserver-dev with env/sqlserver-dev and probes", target)
	}
}

func TestTheExampleInventoryFileIsValid(t *testing.T) {
	f, err := LoadFile(exampleFile(t))
	if err != nil {
		t.Fatal(err)
	}
	if f.Environment.Slug != "prod" || len(f.Applications) != 1 || len(f.DatabaseTargets) != 1 {
		t.Fatalf("file = %+v", f)
	}
	app := f.Applications[0]
	if len(app.Components) != 2 || len(app.TelemetrySources) != 1 || app.TelemetrySources[0].Status != StatusActive {
		t.Errorf("application = %+v", app)
	}
	target := f.DatabaseTargets[0]
	if target.Engine != "sqlserver" || len(target.Probes) != 3 || target.Probes[2].On() {
		t.Errorf("target = %+v", target)
	}
}

// ParseFile fills in what the API would: defaults, trimmed names, a host
// in lower case, the default port and interval.
func TestParseFileNormalizesAsTheAPIStores(t *testing.T) {
	f, err := ParseFile([]byte(`
version: 1
environment: {slug: dev, name: "  Dev  ", type: development}
applications:
  - {name: " Shop ", platform: outsystems}
database_targets:
  - name: shop-db
    host: Shop.DB.Internal
    credential_ref: env/shop-db
    probes: [{name: cpu}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if f.Environment.Name != "Dev" || f.Environment.Status != StatusActive {
		t.Errorf("environment = %+v", f.Environment)
	}
	if a := f.Applications[0]; a.Name != "Shop" || a.Criticality != "medium" || a.Status != StatusActive {
		t.Errorf("application = %+v", a)
	}
	tg := f.DatabaseTargets[0]
	if tg.Host != "shop.db.internal" || tg.Port != 1433 || tg.Engine != "sqlserver" || tg.Status != StatusActive {
		t.Errorf("target = %+v", tg)
	}
	if p := tg.Probes[0]; p.IntervalSeconds != 30 || !p.On() {
		t.Errorf("probe = %+v", p)
	}
}

// Every problem is reported at once, each at its place in the file.
func TestParseFileReportsEveryProblemWithItsPlace(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want []string
	}{
		{"empty", ``, []string{"empty"}},
		{"two documents", "version: 1\n---\nversion: 1\n", []string{"more than one YAML document"}},
		{"a repeated key", "version: 1\nversion: 1\n", []string{"not valid YAML"}},
		{"an unquoted date", "version: 1\nenvironment: {slug: dev, name: Dev, type: test, description: 2026-11-04}\n", []string{"environment/description", "quote it"}},
		{"another version", "version: 2\nenvironment: {slug: dev, name: Dev, type: test}\n", []string{"version:"}},
		{"an unknown field", "version: 1\nenvironment: {slug: dev, name: Dev, type: test, owner: me}\n", []string{"environment:", "owner"}},
		{"a bad slug, deep", `
version: 1
environment: {slug: Dev, name: Dev, type: test}
applications:
  - name: Shop
    platform: outsystems
    components: [{name: Web, type: Web}]
`, []string{"environment.slug:", "applications[0].components[0].type:"}},
		{"a number as a label", "version: 1\nenvironment: {slug: dev, name: Dev, type: test, labels: {tier: 1}}\n", []string{"environment.labels.tier:"}},
		{"duplicates", `
version: 1
environment: {slug: dev, name: Dev, type: test}
applications:
  - name: Shop
    platform: outsystems
    components: [{name: Web, type: web}, {name: Web, type: web}]
    telemetry_sources:
      - {type: outsystems, ingest_mode: agentless, config: {application_name: Shop}}
  - name: Shop
    platform: outsystems
  - name: Other
    platform: outsystems
    telemetry_sources:
      - {type: outsystems, ingest_mode: agentless, config: {application_name: Shop}}
database_targets:
  - {name: a, host: db.internal, credential_ref: env/a}
  - {name: a, host: DB.internal, port: 1433, credential_ref: env/a}
`, []string{
			"applications[1].name: \"Shop\" is applications[0]'s too",
			"applications[0].components[1]: web component \"Web\"",
			"applications[2].telemetry_sources[0].config.application_name: \"Shop\" is applications[0]'s",
			"database_targets[1].name: \"a\" is database_targets[0]'s too",
			"database_targets[1]: the same sqlserver server, port and database",
		}},
		{"a source config its type refuses", `
version: 1
environment: {slug: dev, name: Dev, type: test}
applications:
  - name: Shop
    platform: outsystems
    telemetry_sources: [{type: outsystems, ingest_mode: agentless, config: {}}]
`, []string{"applications[0].telemetry_sources[0].config:"}},
		{"a target the collector refuses", `
version: 1
environment: {slug: dev, name: Dev, type: test}
database_targets:
  - {name: a, host: "db.internal:1433", credential_ref: env/a}
  - {name: b, host: "user@db.internal", credential_ref: env/b}
`, []string{"database_targets[0].host:", "database_targets[1].host:"}},
		{"probes", `
version: 1
environment: {slug: dev, name: Dev, type: test}
database_targets:
  - name: a
    host: db.internal
    credential_ref: env/a
    probes:
      - {name: cpu}
      - {name: cpu}
      - {name: nothing}
      - {name: custom_waits}
      - {name: waits, interval_seconds: 10, timeout_ms: 20000}
      - {name: blocking, interval_seconds: 60}
      - {name: sessions, interval_seconds: 60, enabled: false}
`, []string{
			"database_targets[0].probes[1].name: \"cpu\" is database_targets[0].probes[0] too",
			"database_targets[0].probes[2].name: \"nothing\" is no built-in sqlserver probe",
			"database_targets[0].probes[3].name: \"custom_waits\": the inventory file takes built-in probes only",
			"database_targets[0].probes[4].timeout_ms:",
			"database_targets[0].probes[5].interval_seconds: 60s, and database_targets[0].probes[0] runs every 30s",
		}},
		{"problems under an application or a target that has its own", `
version: 1
environment: {slug: dev, name: Dev, type: test}
applications:
  - name: "  "
    platform: outsystems
    components: [{name: Web, type: web}, {name: Web, type: web}]
    telemetry_sources: [{type: outsystems, ingest_mode: agentless, config: {}}]
database_targets:
  - name: a
    host: "db.internal:1433"
    credential_ref: env/a
    probes: [{name: nothing}]
`, []string{
			"applications[0].name: required",
			"applications[0].components[1]: web component \"Web\"",
			"applications[0].telemetry_sources[0].config:",
			"database_targets[0].host:",
			"database_targets[0].probes[0].name: \"nothing\" is no built-in sqlserver probe",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseFile([]byte(tc.yaml))
			var ferr *FileError
			if !errors.As(err, &ferr) {
				t.Fatalf("err = %v, want a *FileError", err)
			}
			msg := err.Error()
			for _, want := range tc.want {
				if !strings.Contains(msg, want) {
					t.Errorf("error lacks %q:\n%s", want, msg)
				}
			}
			// A disabled probe at another interval is no problem.
			if strings.Contains(msg, "probes[6]") {
				t.Errorf("a disabled probe was held to the shared interval:\n%s", msg)
			}
		})
	}
}

// The file's credential errors never quote the value: a password pasted in
// by mistake stays out of CI logs.
func TestParseFileDoesNotQuoteACredentialRef(t *testing.T) {
	_, err := ParseFile([]byte("version: 1\nenvironment: {slug: dev, name: Dev, type: test}\ndatabase_targets:\n  - {name: a, host: db.internal, credential_ref: env/hunter2-p4ss}\n  - {name: b, host: db2.internal, credential_ref: \"sa:hunter2\"}\n"))
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("err = %v, want one that does not quote the reference", err)
	}
}

func TestLoadFileRefusesAnOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inventory.yaml")
	if err := os.WriteFile(path, make([]byte, MaxFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("err = %v", err)
	}
}

// The schema states the API's patterns; they must not drift apart.
func TestTheInventorySchemaStatesTheAPIsPatterns(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(filepath.Dir(exampleFile(t)), "..", "packages", "config-schema", "src", "inventory.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Defs map[string]struct {
			Pattern    string `json:"pattern"`
			Properties map[string]struct {
				Pattern string `json:"pattern"`
				Minimum int    `json:"minimum"`
				Maximum int    `json:"maximum"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ got, want, what string }{
		{schema.Defs["slug"].Pattern, SlugPattern, "slug"},
		{schema.Defs["identifier"].Pattern, IdentifierPattern, "identifier"},
		{schema.Defs["database_target"].Properties["credential_ref"].Pattern, CredentialRefPattern, "credential_ref"},
	} {
		if c.got != c.want {
			t.Errorf("schema's %s pattern %q, the API's %q", c.what, c.got, c.want)
		}
	}
	probe := schema.Defs["probe"].Properties
	if probe["interval_seconds"].Minimum != MinIntervalSeconds || probe["interval_seconds"].Maximum != MaxIntervalSeconds ||
		probe["timeout_ms"].Maximum != MaxTimeoutMS || probe["max_rows"].Maximum != config.MaxProbeMaxRows {
		t.Errorf("schema's probe limits %+v differ from the API's", probe)
	}
}

// The schema's enums are the values the API allows: a source type added to
// packages/config-schema/src/telemetry-sources, say, must be one the file
// may name too.
func TestTheInventorySchemaStatesTheAPIsValues(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(filepath.Dir(exampleFile(t)), "..", "packages", "config-schema", "src", "inventory.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	type values struct {
		Enum []string `json:"enum"`
	}
	var schema struct {
		Defs map[string]struct {
			Enum       []string          `json:"enum"`
			Properties map[string]values `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	var modes []string
	for _, m := range ingestModes {
		modes = append(modes, m...)
	}
	for _, c := range []struct {
		what      string
		got, want []string
	}{
		{"status", schema.Defs["status"].Enum, statuses},
		{"environment type", schema.Defs["environment"].Properties["type"].Enum, environmentTypes},
		{"criticality", schema.Defs["application"].Properties["criticality"].Enum, criticalities},
		{"source type", schema.Defs["telemetry_source"].Properties["type"].Enum, configschema.SourceTypes()},
		{"ingest mode", schema.Defs["telemetry_source"].Properties["ingest_mode"].Enum, modes},
		{"engine", schema.Defs["database_target"].Properties["engine"].Enum, Engines},
	} {
		got, want := slices.Sorted(slices.Values(c.got)), slices.Compact(slices.Sorted(slices.Values(c.want)))
		if !slices.Equal(got, want) {
			t.Errorf("schema's %s values %q, the API's %q", c.what, got, want)
		}
	}
}
