package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scripts/ci-changes.sh decides which slow CI jobs run (ADR 0007). Running too
// little lets a broken change merge, so unknown paths must run everything.
func TestCIChangesFlags(t *testing.T) {
	const (
		none   = "db_collector=false chart=false kind_e2e=false"
		all    = "db_collector=true chart=true kind_e2e=true"
		dbOnly = "db_collector=true chart=false kind_e2e=true"
		chart  = "db_collector=false chart=true kind_e2e=true"
		e2e    = "db_collector=false chart=false kind_e2e=true"
		probes = "db_collector=true chart=false kind_e2e=false"
	)
	cases := []struct {
		name   string
		paths  []string
		runAll bool
		want   string
	}{
		{"no changes", nil, false, none},
		{"docs and markdown anywhere", []string{"docs/guides/kind-walkthrough.md", "TODO.md", "services/db-collector/README.md", "docs/architecture/diagrams/a b.svg"}, false, none},
		{"docs generator", []string{"tools/docsite/internal/site/site.go"}, false, none},
		{"release bookkeeping", []string{"CHANGELOG.md", "version.txt", ".release-please-manifest.json", "release-please-config.json"}, false, none},
		{"demo helpers", []string{"scripts/demo-helpers.sh"}, false, none},
		{"migrations", []string{"db/migrations/0002_targets.up.sql"}, false, e2e},
		{"migrations package", []string{"db/migrations/migrations.go", "db/migrations/migrations_test.go"}, false, e2e},
		{"db-collector code", []string{"services/db-collector/internal/probes/sqlserver/waits.go"}, false, dbOnly},
		{"db-collector Dockerfile", []string{"services/db-collector/Dockerfile"}, false, dbOnly},
		{"chart", []string{"infra/helm/heartbeat/values.yaml"}, false, chart},
		{"chart rules", []string{"infra/helm/heartbeat/files/prometheus/rules/heartbeat.rules.yml"}, false, chart},
		{"values profile", []string{"infra/helm/values/kind.yaml"}, false, chart},
		{"kind cluster", []string{"infra/kind/cluster.yaml", "infra/kind/e2e.yaml"}, false, e2e},
		{"docs and chart", []string{"docs/README.md", "infra/helm/heartbeat/Chart.yaml"}, false, chart},
		{"chart tests", []string{"tests/chart_render_test.go", "tests/chart_schema_test.go"}, false, "db_collector=false chart=true kind_e2e=false"},
		{"kind scripts", []string{"scripts/kind.sh", "scripts/kind-e2e.sh"}, false, e2e},
		{"probe test script", []string{"scripts/sqlserver-test.sh"}, false, probes},
		{"PatientZero", []string{"tools/patientzero/internal/app/server.go", "tools/patientzero/go.mod", "tools/patientzero/Dockerfile"}, false, probes},
		{"PatientZero script", []string{"tools/patientzero/scripts/patientzero.sh"}, false, "db_collector=true chart=true kind_e2e=false"},
		{"chart tests, PatientZero's", []string{"tests/chart_patientzero_test.go"}, false, "db_collector=false chart=true kind_e2e=false"},
		{"PatientZero chart", []string{"tools/patientzero/chart/values.yaml", "tools/patientzero/chart/templates/deployment.yaml"}, false, "db_collector=false chart=true kind_e2e=false"},
		{"PatientZero and its chart", []string{"tools/patientzero/internal/app/server.go", "tools/patientzero/chart/Chart.yaml"}, false, "db_collector=true chart=true kind_e2e=false"},
		{"PatientZero docs", []string{"tools/patientzero/README.md"}, false, none},
		{"PatientZero and docs", []string{"tools/patientzero/cmd/patientzero/main.go", "docs/README.md"}, false, probes},
		{"collector login script", []string{"scripts/sqlserver-login.sh"}, false, dbOnly},
		{"tool pins", []string{"scripts/install-tools.sh", "scripts/tools-check.sh", "scripts/chart-deps.sh"}, false, chart},
		{"telemetry contracts", []string{"packages/telemetry-contracts/src/application_event.schema.json"}, false, none},
		{"API", []string{"apps/api/cmd/api/main.go", "apps/api/openapi.yaml", "apps/api/internal/openapi/openapi.gen.go"}, false, e2e},
		{"API queries", []string{"apps/api/sqlc.yaml", "apps/api/internal/store/queries/schema.sql", "apps/api/internal/store/schema.sql.gen.go", "apps/api/internal/testenv/testenv.go"}, false, e2e},
		{"API image", []string{"apps/api/Dockerfile", "apps/api/Dockerfile.dockerignore"}, false, e2e},
		{"shared credentials package", []string{"internal/credentials/credentials.go"}, false, all},
		{"API and chart", []string{"apps/api/internal/app/app.go", "infra/helm/heartbeat/values.yaml"}, false, chart},
		{"go.mod", []string{"go.mod"}, false, all},
		{"go.sum", []string{"go.sum"}, false, all},
		{"shared config package", []string{"internal/config/config.go"}, false, all},
		{"contracts", []string{"packages/config-schema/src/integrations.schema.json"}, false, all},
		{"repository tests", []string{"tests/foundations_test.go"}, false, all},
		{"other scripts", []string{"scripts/release-verify.sh"}, false, all},
		{"CI scripts", []string{"scripts/ci-changes.sh"}, false, all},
		{"Makefile", []string{"Makefile"}, false, all},
		{"workflows", []string{".github/workflows/ci.yml"}, false, all},
		{"docker ignore", []string{".dockerignore"}, false, all},
		{"dev config", []string{"config/integrations.yaml"}, false, all},
		{"new directory", []string{"apps/web/src/main.tsx"}, false, all},
		{"new service", []string{"services/reporting/cmd/reporting/main.go"}, false, all},
		{"docs then unknown", []string{"docs/README.md", "apps/web/package.json"}, false, all},
		{"run all", nil, true, all},
		{"run all with docs", []string{"docs/README.md"}, true, all},
	}
	script := filepath.Join(repoRoot(t), "scripts/ci-changes.sh")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("sh", script)
			cmd.Env = append(os.Environ(), "CI_RUN_ALL=false")
			if tc.runAll {
				cmd.Env = append(os.Environ(), "CI_RUN_ALL=true")
			}
			cmd.Stdin = strings.NewReader(strings.Join(tc.paths, "\n") + "\n")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("ci-changes.sh failed: %v\n%s", err, out)
			}
			if got := strings.Join(strings.Fields(string(out)), " "); got != tc.want {
				t.Fatalf("flags = %q, want %q", got, tc.want)
			}
		})
	}
}

// scripts/ci-gate.sh is the only required check. GitHub counts skipped jobs
// as passed, so the gate must fail whenever a job did not do what the change
// detection asked for.
func TestCIGateVerdict(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Fatal("scripts/ci-gate.sh needs jq on PATH")
	}
	args := []string{"changes", "test", "sqlserver:db_collector", "chart:chart", "kind-e2e:kind_e2e"}
	needs := func(flags, changes, test, sqlserver, chart, e2e string) string {
		return `{
  "changes": {"result": "` + changes + `", "outputs": ` + flags + `},
  "test": {"result": "` + test + `", "outputs": {}},
  "sqlserver": {"result": "` + sqlserver + `", "outputs": {}},
  "chart": {"result": "` + chart + `", "outputs": {}},
  "kind-e2e": {"result": "` + e2e + `", "outputs": {}}
}`
	}
	allOn := `{"db_collector": "true", "chart": "true", "kind_e2e": "true"}`
	allOff := `{"db_collector": "false", "chart": "false", "kind_e2e": "false"}`
	codeOnly := `{"db_collector": "true", "chart": "false", "kind_e2e": "true"}`

	cases := []struct {
		name  string
		needs string
		args  []string
		pass  bool
	}{
		{"everything ran and passed", needs(allOn, "success", "success", "success", "success", "success"), args, true},
		{"docs only: slow jobs skipped", needs(allOff, "success", "success", "skipped", "skipped", "skipped"), args, true},
		{"code only: chart skipped, kind ran", needs(codeOnly, "success", "success", "success", "skipped", "success"), args, true},
		{"needed job skipped by mistake", needs(codeOnly, "success", "success", "success", "skipped", "skipped"), args, false},
		{"kind skipped after chart failed", needs(allOn, "success", "success", "success", "failure", "skipped"), args, false},
		{"job ran although not needed", needs(allOff, "success", "success", "success", "skipped", "skipped"), args, false},
		{"always-run job failed", needs(allOff, "success", "failure", "skipped", "skipped", "skipped"), args, false},
		{"always-run job skipped", needs(allOff, "success", "skipped", "skipped", "skipped", "skipped"), args, false},
		{"needed job failed", needs(allOn, "success", "success", "failure", "success", "success"), args, false},
		{"run cancelled", needs(allOn, "success", "cancelled", "cancelled", "success", "skipped"), args, false},
		{"change detection failed", needs(`{}`, "failure", "success", "skipped", "skipped", "skipped"), args, false},
		{"flag missing from outputs", needs(`{"db_collector": "true", "chart": "true"}`, "success", "success", "success", "success", "success"), args, false},
		{"job missing from the arguments", needs(allOn, "success", "success", "success", "success", "success"), args[:4], false},
		{"argument missing from needs", needs(allOn, "success", "success", "success", "success", "success"), append(append([]string{}, args...), "lint"), false},
	}
	script := filepath.Join(repoRoot(t), "scripts/ci-gate.sh")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("sh", append([]string{script}, tc.args...)...)
			cmd.Env = append(os.Environ(), "NEEDS="+tc.needs, "GITHUB_STEP_SUMMARY=")
			out, err := cmd.CombinedOutput()
			if tc.pass && err != nil {
				t.Fatalf("gate failed, want pass: %v\n%s", err, out)
			}
			if !tc.pass && err == nil {
				t.Fatalf("gate passed, want failure:\n%s", out)
			}
		})
	}
}
