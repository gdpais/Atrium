package probecatalog

import (
	"slices"
	"testing"

	"heartbeat/services/db-collector/internal/probes/sqlserver"
)

// Builtins lists every probe of the SQL Server catalog once, as the catalog
// has it, so the API's seed test compares against the whole catalog.
func TestBuiltinsListTheSQLServerCatalog(t *testing.T) {
	catalog := sqlserver.DefaultCatalog()
	builtins := Builtins()
	var names []string
	for _, probe := range builtins {
		want, ok := catalog.Get(probe.Name)
		if !ok {
			t.Fatalf("Builtins() lists %q, which the catalog does not have", probe.Name)
		}
		if probe.Engine != "sqlserver" || probe.Category != want.Category {
			t.Errorf("Builtins() lists %+v, want engine sqlserver and category %q", probe, want.Category)
		}
		names = append(names, probe.Name)
	}
	if !slices.Equal(names, catalog.Names()) {
		t.Errorf("Builtins() names = %v, want the catalog's, sorted: %v", names, catalog.Names())
	}
}
