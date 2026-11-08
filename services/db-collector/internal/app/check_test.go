package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckConfig(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.yaml")
	writeTestConfig(t, valid, testCollector{id: "sql-a", env: "prod", interval: "30s"})
	version, collectors, err := CheckConfig(valid)
	if err != nil || version == "" || collectors != 1 {
		t.Fatalf("CheckConfig(valid) = %q, %d, %v; want a version and 1 collector", version, collectors, err)
	}

	// A probe the catalog does not know passes the shared loader but not the
	// collector's catalog check.
	content, err := os.ReadFile(valid)
	if err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(dir, "unknown-probe.yaml")
	if err := os.WriteFile(unknown, []byte(strings.Replace(string(content), "- name: waits", "- name: no_such_probe", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CheckConfig(unknown); err == nil || !strings.Contains(err.Error(), "unknown-probe.yaml") {
		t.Fatalf("CheckConfig(unknown probe) = %v, want an error naming the file", err)
	}
	if _, _, err := CheckConfig(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("CheckConfig(missing) passed")
	}
}
