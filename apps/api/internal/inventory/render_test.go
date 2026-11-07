package inventory_test

import (
	"os"
	"strings"
	"testing"

	"heartbeat/apps/api/internal/inventory"
)

// The ID of a target the file creates depends only on the environment's
// slug and the target's name, so a render made before the API applies
// the file names the collector the API will render.
func TestFileTargetIDIsStable(t *testing.T) {
	a, b := inventory.FileTargetID("prod", "claims-prod"), inventory.FileTargetID("prod", "claims-prod")
	if a != b || a.Version() != 5 {
		t.Fatalf("FileTargetID = %s, %s: want one version 5 UUID", a, b)
	}
	// Pinned: a changed namespace would rename every collector a file renders.
	if got := a.String(); got != "eccd0167-745f-5072-8af1-db1200f6b30d" {
		t.Errorf("FileTargetID(prod, claims-prod) = %s", got)
	}
	for _, other := range []struct{ slug, name string }{{"prod", "claims"}, {"dev", "claims-prod"}, {"prod/claims", "prod"}} {
		if inventory.FileTargetID(other.slug, other.name) == a {
			t.Errorf("FileTargetID(%s, %s) collides with prod/claims-prod", other.slug, other.name)
		}
	}
}

func TestRenderFile(t *testing.T) {
	file, err := inventory.ParseFile([]byte(`version: 1
environment: {slug: shop, name: Shop, type: test}
database_targets:
  - name: shop-db
    host: SHOP-SQL.internal
    database_name: Shop
    credential_ref: env/shop-db
    probes:
      - {name: waits, interval_seconds: 15, timeout_ms: 5000, max_rows: 500}
      - {name: blocking, interval_seconds: 15}
      - {name: cpu, interval_seconds: 15, enabled: false}
  - name: archive-db
    host: archive-sql.internal
    port: 14330
    credential_ref: env/archive-db
    status: disabled
    probes:
      - {name: sessions}
  - name: idle-db
    host: idle-sql.internal
    credential_ref: env/idle-db
`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := inventory.RenderFile(file)
	if err != nil {
		t.Fatal(err)
	}
	id := inventory.FileTargetID("shop", "shop-db")
	// A disabled target, a disabled probe and a target with no probes
	// render nothing; probes are sorted by name; the host is normalized as
	// the API stores it.
	want := inventory.RenderHeader + `managedCollectors:
  - id: api-` + id.String() + `-15s
    kind: sqlserver
    enabled: true
    config:
      environment: shop
      scrape_interval: 15s
      targets:
        - name: shop-db
          environment: shop
          host: shop-sql.internal
          port: 1433
          database_name: Shop
          credential_ref: env/shop-db
          probes:
            - name: blocking
            - name: waits
              timeout_ms: 5000
              max_rows: 500
`
	if string(got) != want {
		t.Fatalf("RenderFile() =\n%s\nwant\n%s", got, want)
	}
	again, err := inventory.RenderFile(file)
	if err != nil || string(again) != string(got) {
		t.Fatalf("a second render differs: %v\n%s", err, again)
	}
}

func TestRenderFileWithNoProbes(t *testing.T) {
	file, err := inventory.ParseFile([]byte("version: 1\nenvironment: {slug: dev, name: Dev, type: test}\n"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := inventory.RenderFile(file)
	if err != nil || string(got) != inventory.RenderHeader+"managedCollectors: []\n" {
		t.Fatalf("RenderFile() = %q, %v", got, err)
	}
}

// The example file renders, so the guide's command works on it.
func TestRenderFileOfTheExample(t *testing.T) {
	raw, err := os.ReadFile("../../../../config/inventory.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	file, err := inventory.ParseFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	got, err := inventory.RenderFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"api-" + inventory.FileTargetID("prod", "claims-prod").String() + "-30s", "name: blocking", "name: waits"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("render lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(string(got), "name: cpu") {
		t.Errorf("render holds the disabled probe:\n%s", got)
	}
}
