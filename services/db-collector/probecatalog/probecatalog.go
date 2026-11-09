// Package probecatalog lists the db-collector's built-in probes for code
// outside the collector, which cannot import its internal packages: the API
// seeds one built-in probe definition per probe
// (db/migrations/0004_probe_definitions.up.sql), and its test fails when the
// seed and this list differ.
//
// It lists the SQL Server catalog only. The OutSystems log types are not
// probes the API assigns: outsystems_logs collectors stay in YAML
// (ADR 0017), and the API renders only sqlserver collectors (ADR 0014).
package probecatalog

import (
	"atrium/internal/config"
	"atrium/services/db-collector/internal/probes/sqlserver"
)

// Probe is one built-in probe.
type Probe struct {
	// Engine is the kind of collector that runs it, such as
	// [config.KindSQLServer].
	Engine string
	// Name selects it in a collector's probes list.
	Name string
	// Category groups related probes and decides which produce evidence.
	Category string
}

// Builtins returns every built-in probe, sorted by name.
func Builtins() []Probe {
	catalog := sqlserver.DefaultCatalog()
	names := catalog.Names()
	probes := make([]Probe, len(names))
	for i, name := range names {
		probe, _ := catalog.Get(name)
		probes[i] = Probe{Engine: config.KindSQLServer, Name: probe.Name, Category: probe.Category}
	}
	return probes
}
