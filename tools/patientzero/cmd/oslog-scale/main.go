// oslog-scale fills a test logging database with a production-sized
// OutSystems 11 log and measures the collector's queries against it
// (internal/scale). make patientzero-scale runs it against the sandbox SQL
// Server.
//
// Usage:
//
//	oslog-scale [flags] generate   create the database and fill it; writes expected.json
//	oslog-scale [flags] measure    run every log query over the busiest minute and 1 h, 1 day and 7 day backlogs; writes report.md
//	oslog-scale [flags] drop       drop the database
//	oslog-scale [flags] check      compare what Loki holds for the log with expected.json; writes check.md
//	oslog-scale [flags] caught-up  exit 0 once Loki holds the log's last hour in full, 3 before
//
// PATIENTZERO_SCALE_SQLSERVER is a sqlserver:// URL for a login that may
// create databases (the sandbox's sa), without a database; it is read from
// the environment so the password stays off the command line. check and
// caught-up need no SQL Server, only Loki (-loki) and the collector that
// read the log (-environment, -collected-from, -lookback).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	_ "github.com/microsoft/go-mssqldb"

	"heartbeat/tools/patientzero/internal/logschema"
	"heartbeat/tools/patientzero/internal/scale"
)

// errBehind is caught-up's answer while the collector is still catching up.
var errBehind = errors.New("Loki does not hold the log's last hour in full yet")

// caughtUpTimeout bounds one caught-up pass, which took at most 10 seconds
// on CI on 2026-11-02. A port-forward that stalls would otherwise hold the
// pass, and with it SCALE_WAIT's loop, until the step's timeout, and the
// check would never run.
const caughtUpTimeout = 2 * time.Minute

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log, os.Args[1:]); err != nil {
		if errors.Is(err, errBehind) {
			os.Exit(3)
		}
		log.Error("oslog-scale failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("oslog-scale", flag.ContinueOnError)
	database := fs.String("database", scale.DatabasePrefix, "test logging database, created and dropped by this tool (its name starts with "+scale.DatabasePrefix+")")
	factor := fs.Float64("scale", 1, "volume, users and sessions relative to the surveyed installation's week")
	seed := fs.Uint64("seed", 1, "seed of the generated log")
	endFlag := fs.String("end", "", "last instant generated, RFC 3339 (default now, UTC)")
	schemaDir := fs.String("schema-dir", "../../services/db-collector/internal/collectors/outsystems/testdata/oslog", "the oslog fixtures the logging schema is built from")
	queriesDir := fs.String("queries-dir", "../../services/db-collector/internal/probes/outsystems/queries", "the collector's log queries")
	reader := fs.String("reader-role", "heartbeat_oslog_reader", "role granted SELECT on the log views")
	readerLogin := fs.String("reader-login", "", "login added to the reader role, such as the Heartbeat collector's")
	limit := fs.Int("limit", 5000, "rows per query run (the collector's batch_limit)")
	out := fs.String("out", "../../.tmp/patientzero-scale", "directory for expected.json, report.md and check.md")
	lokiURL := fs.String("loki", "http://127.0.0.1:3100", "check: Loki's HTTP API")
	environment := fs.String("environment", "", "check: deployment.environment.name of the collector that read the log")
	collectedFrom := fs.String("collected-from", "", "check: when that collector started, RFC 3339")
	lookback := fs.Duration("lookback", 48*time.Hour, "check: that collector's initial_lookback")
	dashboards := fs.String("dashboards", "../../infra/helm/heartbeat/files/grafana/dashboards", "check: the chart's Grafana dashboards")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("want one action: generate, measure, drop, check or caught-up")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch fs.Arg(0) {
	case "check":
		return check(ctx, log, *out, *dashboards, scale.Loki{URL: *lokiURL}, *environment, *collectedFrom, *lookback)
	case "caught-up":
		return caughtUp(ctx, log, *out, scale.Loki{URL: *lokiURL}, *environment, *collectedFrom, *lookback)
	}
	server := os.Getenv("PATIENTZERO_SCALE_SQLSERVER")
	if server == "" {
		return errors.New("PATIENTZERO_SCALE_SQLSERVER is not set")
	}
	master, err := scale.Open(server, "master")
	if err != nil {
		return err
	}
	defer master.Close()
	db, err := scale.Open(server, *database)
	if err != nil {
		return err
	}
	defer db.Close()

	switch fs.Arg(0) {
	case "drop":
		if err := scale.Drop(ctx, master, *database); err != nil {
			return err
		}
		log.Info("dropped", "database", *database)
		return nil
	case "generate":
		end := time.Now().UTC()
		if *endFlag != "" {
			if end, err = time.Parse(time.RFC3339, *endFlag); err != nil {
				return fmt.Errorf("-end: %w", err)
			}
		}
		types, err := logschema.Load(*schemaDir)
		if err != nil {
			return err
		}
		if err := scale.Create(ctx, master, *database); err != nil {
			return err
		}
		if err := scale.Schema(ctx, db, types, *reader, *readerLogin); err != nil {
			return err
		}
		profile := scale.Surveyed().Scaled(*factor)
		log.Info("generating", "database", *database, "scale", *factor, "end", end.Format(time.RFC3339),
			"screen_rows_per_week", profile.Screen, "users", profile.Users, "sessions", profile.Sessions)
		started := time.Now()
		sink := scale.NewBulkSink(ctx, db, types)
		exp, err := scale.Generate(scale.Options{Profile: profile, Seed: *seed, End: end}, sink, func(day time.Time) {
			var rows int
			for _, n := range sink.Rows {
				rows += n
			}
			log.Info("generated", "day", day.Format(time.DateOnly), "rows", rows, "elapsed", time.Since(started).Round(time.Second))
		})
		if err != nil {
			return err
		}
		if err := writeJSON(filepath.Join(*out, "expected.json"), exp); err != nil {
			return err
		}
		log.Info("done", "database", *database, "elapsed", time.Since(started).Round(time.Second),
			"expected", filepath.Join(*out, "expected.json"))
		return nil
	case "measure":
		types, err := logschema.Load(*schemaDir)
		if err != nil {
			return err
		}
		views := map[string]string{}
		for _, t := range types {
			views[t.Name] = t.View
		}
		windows, err := scale.Windows(ctx, db)
		if err != nil {
			return err
		}
		sizes, err := scale.Sizes(ctx, db, views)
		if err != nil {
			return err
		}
		log.Info("measuring", "database", *database, "windows", len(windows))
		results, err := scale.Measure(ctx, db, *queriesDir, windows, *limit)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(*out, 0o755); err != nil {
			return err
		}
		path := filepath.Join(*out, "report.md")
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := scale.Report(f, *database, sizes, windows, results, *limit); err != nil {
			return err
		}
		if err := scale.Report(os.Stdout, *database, sizes, windows, results, *limit); err != nil {
			return err
		}
		log.Info("report written", "path", path)
		return f.Close()
	default:
		return fmt.Errorf("unknown action %q: want generate, measure, drop, check or caught-up", fs.Arg(0))
	}
}

// collected reads expected.json and where the collector that read the log
// started, for check and caught-up.
func collected(out, environment, collectedFrom string, lookback time.Duration) (*scale.Expected, scale.CheckOptions, error) {
	if environment == "" || collectedFrom == "" {
		return nil, scale.CheckOptions{}, errors.New("check and caught-up need -environment and -collected-from")
	}
	started, err := time.Parse(time.RFC3339, collectedFrom)
	if err != nil {
		return nil, scale.CheckOptions{}, fmt.Errorf("-collected-from: %w", err)
	}
	b, err := os.ReadFile(filepath.Join(out, "expected.json"))
	if err != nil {
		return nil, scale.CheckOptions{}, err
	}
	var exp scale.Expected
	if err := json.Unmarshal(b, &exp); err != nil {
		return nil, scale.CheckOptions{}, err
	}
	return &exp, scale.CheckOptions{Environment: environment, From: started.Add(-lookback)}, nil
}

func caughtUp(ctx context.Context, log *slog.Logger, out string, loki scale.Loki, environment, collectedFrom string, lookback time.Duration) error {
	exp, opts, err := collected(out, environment, collectedFrom, lookback)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, caughtUpTimeout)
	defer cancel()
	p, err := scale.CaughtUp(ctx, loki, exp, opts)
	if err != nil {
		return err
	}
	log.Info("in Loki", "environment", environment, "hours_in_full", p.Complete, "hours", p.Hours, "last_hour_in_full", p.Done)
	if !p.Done {
		return errBehind
	}
	return nil
}

func check(ctx context.Context, log *slog.Logger, out, dashboards string, loki scale.Loki, environment, collectedFrom string, lookback time.Duration) error {
	exp, opts, err := collected(out, environment, collectedFrom, lookback)
	if err != nil {
		return err
	}
	panels, err := scale.LoadPanels(dashboards)
	if err != nil {
		return err
	}
	log.Info("checking", "environment", environment, "from", opts.From.Format(time.RFC3339), "to", exp.End.Format(time.RFC3339))
	res, err := scale.Check(ctx, loki, exp, panels, opts)
	if err != nil {
		return err
	}
	path := filepath.Join(out, "check.md")
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := scale.CheckReport(f, res); err != nil {
		return err
	}
	if err := scale.CheckReport(os.Stdout, res); err != nil {
		return err
	}
	log.Info("check written", "path", path)
	if err := f.Close(); err != nil {
		return err
	}
	if !res.Passed() {
		return errors.New("Loki does not match expected.json; see " + path)
	}
	return nil
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
