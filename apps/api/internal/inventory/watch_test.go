package inventory

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// A file larger than MaxFileBytes is invalid, not an error, and is not
// counted again until it changes; one that cannot be read is an error at
// every read. Neither reaches PostgreSQL, so the applier needs no Service.
func TestFileApplierRefusesWithoutPostgreSQL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inventory.yaml")
	applier := NewFileApplier(nil, path, "api-test", nil)
	ctx := context.Background()

	for range 2 {
		if got := applier.Check(ctx); got != ApplyError {
			t.Fatalf("missing file: %q, want error", got)
		}
	}
	if err := os.WriteFile(path, make([]byte, MaxFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := applier.Check(ctx); got != ApplyInvalid {
		t.Fatalf("oversized file: %q, want invalid", got)
	}
	if got := applier.Check(ctx); got != "" {
		t.Fatalf("same oversized file: %q, want nothing applied", got)
	}
	for result, want := range map[string]float64{ApplyError: 2, ApplyInvalid: 1, ApplyChanged: 0, ApplyUnchanged: 0} {
		if got := testutil.ToFloat64(applier.applies.WithLabelValues(result)); got != want {
			t.Errorf("%s count %v, want %v", result, got, want)
		}
	}
}

func TestNewFileApplierWithoutAPath(t *testing.T) {
	if NewFileApplier(nil, "", "api-test", nil) != nil {
		t.Fatal("an empty path returned an applier")
	}
}
