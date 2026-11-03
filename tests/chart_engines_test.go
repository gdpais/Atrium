package tests

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ADR 0015 §6: one version per engine everywhere. The chart's bundled
// PostgreSQL and Valkey run exactly what the tests run against: the
// PostgreSQL integration fixture (infra/docker-compose.test.yml, which the
// API's tests reuse) and the API's Valkey (testenv.ValkeyImage). A Renovate
// update of one without the other fails here.
func TestBundledEnginesMatchTestFixtures(t *testing.T) {
	root := repoRoot(t)

	var values map[string]any
	readYAML(t, filepath.Join(root, "infra/helm/heartbeat/values.yaml"), &values)
	chartImage := func(key string) string {
		image, _ := dig(values, key, "image").(map[string]any)
		repository, _ := image["repository"].(string)
		tag, _ := image["tag"].(string)
		return repository + ":" + tag
	}

	var fixture struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	readYAML(t, filepath.Join(root, "infra/docker-compose.test.yml"), &fixture)
	// The fixture pins a digest as well; the chart pins the tag.
	postgres, _, _ := strings.Cut(fixture.Services["postgres"].Image, "@")
	if got := chartImage("postgresql"); got != postgres {
		t.Errorf("values.yaml postgresql.image is %s, the integration fixture runs %s", got, postgres)
	}

	testenv, err := os.ReadFile(filepath.Join(root, "apps/api/internal/testenv/testenv.go"))
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`ValkeyImage = "([^"]+)"`).FindSubmatch(testenv)
	if match == nil {
		t.Fatal("no ValkeyImage in apps/api/internal/testenv/testenv.go")
	}
	if got := chartImage("valkey"); got != string(match[1]) {
		t.Errorf("values.yaml valkey.image is %s, the API's integration tests run %s", got, match[1])
	}
}
