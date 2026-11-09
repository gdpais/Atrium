package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"atrium/apps/api/internal/inventory"
)

// errInvalidInventory is an inventory file `api inventory check` found
// problems in: it has written them, so main exits 1 without logging more.
var errInvalidInventory = errors.New("the inventory file is not valid")

// checkInventory runs `api inventory check -f FILE`: it checks an
// environment's inventory file (ADR 0021) as the API would before applying
// it, with no database or other setting, for the configuration
// repository's CI and for the people who edit the file. It writes a
// one-line summary, or every problem, to stdout.
func checkInventory(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("inventory check", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("f", "", "")
	if err := flags.Parse(args); err != nil || flags.NArg() > 0 || *path == "" {
		return errUsage
	}
	file, err := inventory.LoadFile(*path)
	var ferr *inventory.FileError
	switch {
	case errors.As(err, &ferr):
		if _, werr := fmt.Fprintf(stdout, "%s: %v\n", *path, ferr); werr != nil {
			return werr
		}
		return errInvalidInventory
	case err != nil:
		return err
	}
	var components, sources, probes int
	for _, a := range file.Applications {
		components += len(a.Components)
		sources += len(a.TelemetrySources)
	}
	for _, t := range file.DatabaseTargets {
		probes += len(t.Probes)
	}
	_, err = fmt.Fprintf(stdout, "%s: environment %s is valid: %d applications, %d components, %d telemetry sources, %d database targets, %d probes\n",
		*path, file.Environment.Slug, len(file.Applications), components, sources, len(file.DatabaseTargets), probes)
	return err
}

// renderInventory runs `api inventory render -f FILE`: it renders the
// probe assignments of an environment's inventory file into the
// managedCollectors values file (ADR 0021 §5, ADR 0014's format) with no
// database or other setting, so the pull request that changes an
// assignment carries the collectors it renders. A file that is not valid
// is reported as `inventory check` reports it, and nothing is written.
func renderInventory(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("inventory render", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("f", "", "")
	if err := flags.Parse(args); err != nil || flags.NArg() > 0 || *path == "" {
		return errUsage
	}
	file, err := inventory.LoadFile(*path)
	var ferr *inventory.FileError
	switch {
	case errors.As(err, &ferr):
		return fmt.Errorf("%s: %w: %v", *path, errInvalidInventory, ferr)
	case err != nil:
		return err
	}
	rendered, err := inventory.RenderFile(file)
	if err != nil {
		return err
	}
	_, err = stdout.Write(rendered)
	return err
}
