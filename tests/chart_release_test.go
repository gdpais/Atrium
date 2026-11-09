package tests

import (
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The chart's image defaults must name what the release workflow publishes:
// v0.1.0 defaulted to heartbeat/db-collector, which resolves to Docker Hub,
// so `helm install` of the published chart could never pull its own image.
// The tag defaults to appVersion, which must be the release version the
// workflow tags the images with.

// releaseImageValues maps each image the release workflow builds (its images
// matrix) to the chart values key whose image deploys it.
var releaseImageValues = map[string]string{
	"db-collector": "dbCollector",
	"api":          "api",
}

func TestChartDefaultsToReleasedArtifacts(t *testing.T) {
	root := repoRoot(t)

	var chart struct {
		Name       string `yaml:"name"`
		Version    string `yaml:"version"`
		AppVersion string `yaml:"appVersion"`
		Home       string `yaml:"home"`
	}
	readYAML(t, filepath.Join(root, "infra/helm/atrium/Chart.yaml"), &chart)
	if chart.AppVersion != chart.Version {
		t.Errorf("Chart.yaml appVersion %q differs from version %q; images are published with the release version, so the default tag would not exist", chart.AppVersion, chart.Version)
	}
	// The workflow publishes under the repository owner's namespace; the
	// chart's home names the canonical repository.
	home, err := url.Parse(chart.Home)
	if err != nil || home.Host != "github.com" {
		t.Fatalf("Chart.yaml home %q is not a github.com repository URL", chart.Home)
	}
	owner, _, _ := strings.Cut(strings.TrimPrefix(home.Path, "/"), "/")

	var workflow struct {
		Env  map[string]string `yaml:"env"`
		Jobs struct {
			Images struct {
				Env      map[string]string `yaml:"env"`
				Strategy struct {
					Matrix struct {
						Include []struct {
							Service    string `yaml:"service"`
							Dockerfile string `yaml:"dockerfile"`
						} `yaml:"include"`
					} `yaml:"matrix"`
				} `yaml:"strategy"`
			} `yaml:"images"`
		} `yaml:"jobs"`
	}
	readYAML(t, filepath.Join(root, ".github/workflows/release.yml"), &workflow)
	expand := func(s string) string {
		return strings.ReplaceAll(s, "${{ github.repository_owner }}", owner)
	}
	imagePrefix := expand(workflow.Env["IMAGE_PREFIX"])
	if imagePrefix != "ghcr.io/"+owner+"/atrium" {
		t.Errorf("release.yml IMAGE_PREFIX is %q; update this test and the chart's image defaults together", imagePrefix)
	}
	chartRef := expand(workflow.Env["CHART_REPO"]) + "/" + chart.Name
	include := workflow.Jobs.Images.Strategy.Matrix.Include
	if len(include) == 0 {
		t.Fatal("release.yml jobs.images.strategy.matrix.include is empty; the test is not reading the workflow")
	}
	var services []string
	for _, image := range include {
		services = append(services, image.Service)
		if _, err := os.Stat(filepath.Join(root, image.Dockerfile)); err != nil {
			t.Errorf("release.yml builds %s from %q: %v", image.Service, image.Dockerfile, err)
		}
	}
	for service := range releaseImageValues {
		if !slices.Contains(services, service) {
			t.Errorf("the chart deploys %s, which release.yml does not build", service)
		}
	}

	var values map[string]any
	readYAML(t, filepath.Join(root, "infra/helm/atrium/values.yaml"), &values)
	var published []string
	for _, service := range services {
		ref := strings.ReplaceAll(expand(workflow.Jobs.Images.Env["REF"]), "${{ matrix.service }}", service)
		repository := imagePrefix + "/" + service
		published = append(published, repository)
		if !strings.HasPrefix(ref, repository+":") {
			t.Errorf("release.yml pushes %s as %q, not under IMAGE_PREFIX (%s)", service, ref, repository)
		}
		key, ok := releaseImageValues[service]
		if !ok {
			t.Errorf("release.yml builds %s, which no chart image deploys; add its values key to releaseImageValues", service)
			continue
		}
		image, _ := dig(values, key, "image").(map[string]any)
		if got := image["repository"]; got != repository {
			t.Errorf("values.yaml %s.image.repository is %v, want %s (where release.yml publishes it)", key, got, repository)
		}
		for _, field := range []string{"tag", "digest"} {
			if got := image[field]; got != "" {
				t.Errorf("values.yaml %s.image.%s is %v; leave it empty so the tag defaults to appVersion", key, field, got)
			}
		}
	}

	// The install docs name the same locations, so a move breaks them here
	// rather than for a reader.
	for _, doc := range []string{"docs/guides/install-from-release.md", "infra/helm/atrium/README.md"} {
		content, err := os.ReadFile(filepath.Join(root, doc))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range append([]string{chartRef}, published...) {
			if !strings.Contains(string(content), want) {
				t.Errorf("%s does not mention %s", doc, want)
			}
		}
	}
}

func readYAML(t *testing.T, path string, out any) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(content, out); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

// dig returns the value at a path of map keys, or nil.
func dig(v any, keys ...string) any {
	for _, key := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[key]
	}
	return v
}
