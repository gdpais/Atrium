package probecatalog

import (
	"slices"
	"strings"
	"testing"

	"atrium/services/db-collector/internal/probes/sqlserver"
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

// Custom probe definitions' names start with custom_ (the API's
// probedefs.CustomPrefix), so a built-in's may not: its seed migration would
// clash with a definition an admin made.
func TestNoBuiltinTakesTheCustomPrefix(t *testing.T) {
	for _, probe := range Builtins() {
		if strings.HasPrefix(probe.Name, "custom_") {
			t.Errorf("built-in probe %q starts with custom_, which custom probe definitions' names start with; rename it", probe.Name)
		}
	}
}
