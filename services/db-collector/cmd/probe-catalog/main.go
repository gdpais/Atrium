// Package main prints the built-in SQL Server probe catalog and the OutSystems
// log types, so a developer can see and run exactly what the collector sends
// to a target.
//
// Without arguments it lists every probe with its metrics: name, type, source
// column, unit scale and label columns, then every OutSystems log type with
// its query file.  With -sql it prints one probe's or log type's batch as the
// collector sends it, session settings included, ready to pipe into sqlcmd.
// A log type's batch starts with DECLAREs for the parameters the collector
// binds (the last 15 minutes up to 1 minute ago, in UTC, at most 100 rows):
//
//	go run ./services/db-collector/cmd/probe-catalog
//	go run ./services/db-collector/cmd/probe-catalog -sql waits
//	go run ./services/db-collector/cmd/probe-catalog -sql error
//
// It is a development aid; the db-collector image does not include it.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	connector "atrium/services/db-collector/internal/connectors/sqlserver"
	catalogoutsystems "atrium/services/db-collector/internal/probes/outsystems"
	catalogsqlserver "atrium/services/db-collector/internal/probes/sqlserver"
)

// logParameters declares the parameters the collector binds to a log-type
// query, with example values, so the printed batch runs in sqlcmd.
const logParameters = `-- Parameters the collector binds; example values, edit them as needed.
DECLARE @from datetime2 = DATEADD(MINUTE, -15, SYSUTCDATETIME());
DECLARE @to datetime2 = DATEADD(MINUTE, -1, SYSUTCDATETIME());
DECLARE @limit int = 100;
`

func main() {
	probe := flag.String("sql", "", "print the batch the collector sends for this probe or OutSystems log type")
	flag.Parse()
	if err := run(os.Stdout, catalogsqlserver.DefaultCatalog(), catalogoutsystems.DefaultCatalog(), *probe); err != nil {
		fmt.Fprintln(os.Stderr, "probe-catalog:", err)
		os.Exit(2)
	}
}

func run(w io.Writer, catalog catalogsqlserver.Catalog, logs catalogoutsystems.Catalog, probe string) error {
	if probe == "" {
		if err := list(w, catalog); err != nil {
			return err
		}
		return listLogTypes(w, logs)
	}
	definition, isProbe := catalog.Get(probe)
	query, isLogType := logs.Get(probe)
	switch {
	case isProbe && isLogType:
		return fmt.Errorf("%q is both a SQL Server probe and an OutSystems log type; rename one", probe)
	case isProbe:
		_, err := fmt.Fprintln(w, connector.WithSessionSettings(definition.QueryTemplate))
		return err
	case isLogType:
		_, err := fmt.Fprintln(w, logParameters+connector.WithSessionSettings(query.SQL))
		return err
	}
	return fmt.Errorf("unknown probe %q; probes: %s; OutSystems log types: %s", probe, strings.Join(catalog.Names(), ", "), strings.Join(logs.Names(), ", "))
}

// listLogTypes writes one line per OutSystems log type.
func listLogTypes(w io.Writer, logs catalogoutsystems.Catalog) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "\nOUTSYSTEMS LOG TYPE\tQUERY")
	for _, name := range logs.Names() {
		fmt.Fprintf(tw, "%s\tservices/db-collector/internal/probes/outsystems/queries/%s.sql\n", name, name)
	}
	return tw.Flush()
}

// list writes one line per metric, grouped by probe in catalog order.
func list(w io.Writer, catalog catalogsqlserver.Catalog) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PROBE\tMETRIC\tTYPE\tCOLUMN\tSCALE\tLABELS")
	for _, name := range catalog.Names() {
		definition, _ := catalog.Get(name)
		for _, metric := range definition.Metrics {
			labels := "-"
			if len(metric.LabelColumns) > 0 {
				labels = strings.Join(metric.LabelColumns, ",")
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%g\t%s\n",
				name, metric.Name, metric.Type, metric.ValueColumn, metric.Convert(1), labels)
		}
	}
	return tw.Flush()
}
