// Package outsystems holds the built-in OutSystems 11 log queries that the
// db-collector's outsystems_logs collectors run against the logging database
// (ADR 0008).
//
// Every queries/*.sql file is embedded in the binary.  The file name without
// .sql is the log type: queries/error.sql is log type "error", selected in
// integrations.yaml with probes: [{name: error}].  The query contract is in
// queries/README.md.
package outsystems

import (
	"embed"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"

	atriumconfig "atrium/internal/config"
)

//go:embed queries/*.sql
var queryFiles embed.FS

// logTypeName is the form of a log type name: lower-case snake_case.
var logTypeName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Query is the built-in query of one log type.
type Query struct {
	// LogType is the file name without .sql.
	LogType string
	// SQL is the file content: one SELECT that follows the query contract.
	SQL string
}

// Catalog maps log types to their queries.
type Catalog struct {
	byName map[string]Query
}

// DefaultCatalog returns the embedded queries.  It panics when a file name is
// not a valid log type name, which a test catches before a release.
func DefaultCatalog() Catalog {
	catalog, err := newCatalog(queryFiles, "queries")
	if err != nil {
		panic(err)
	}
	return catalog
}

// newCatalog loads every .sql file of dir in fsys.
func newCatalog(fsys fs.FS, dir string) (Catalog, error) {
	files, err := fs.Glob(fsys, path.Join(dir, "*.sql"))
	if err != nil {
		return Catalog{}, err
	}
	catalog := Catalog{byName: map[string]Query{}}
	for _, file := range files {
		name := strings.TrimSuffix(path.Base(file), ".sql")
		if !logTypeName.MatchString(name) {
			return Catalog{}, fmt.Errorf("outsystems query %s: the file name must be a lower-case snake_case log type", file)
		}
		content, err := fs.ReadFile(fsys, file)
		if err != nil {
			return Catalog{}, err
		}
		catalog.byName[name] = Query{LogType: name, SQL: string(content)}
	}
	return catalog, nil
}

// Get returns the query of logType and true, or the zero value and false when
// there is none.
func (c Catalog) Get(logType string) (Query, bool) {
	query, ok := c.byName[logType]
	return query, ok
}

// Names returns every log type in the catalog, sorted.
func (c Catalog) Names() []string {
	return slices.Sorted(maps.Keys(c.byName))
}

// Validate rejects an outsystems_logs collector, enabled or not, that selects
// a log type the catalog does not have.  A query_template override replaces
// the query of a log type; it cannot add one.  The db-collector registers it
// with [atriumconfig.WithValidator].
func (c Catalog) Validate(cfg atriumconfig.RuntimeConfig) error {
	for _, collector := range cfg.Collectors {
		if collector.Kind != atriumconfig.KindOutSystemsLogs {
			continue
		}
		probes := slices.Clone(collector.Probes)
		for _, target := range collector.Targets {
			probes = append(probes, target.Probes...)
		}
		for _, probe := range probes {
			if _, ok := c.Get(probe.Name); !ok {
				return fmt.Errorf("collector %s: unknown OutSystems log type %q; log types: %s", collector.ID, probe.Name, strings.Join(c.Names(), ", "))
			}
		}
	}
	return nil
}
