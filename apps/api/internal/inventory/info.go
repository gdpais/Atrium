package inventory

import (
	"context"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"heartbeat/apps/api/internal/store"
)

// Info metrics: the inventory as Grafana reads it (ADR 0021 §6). Each is
// always 1, one series per row, the row's identity and state in its labels.
const (
	// MetricEnvironmentInfo is an environment. Labels: environment (its
	// slug), type, status, managed_by.
	MetricEnvironmentInfo = "heartbeat_api_inventory_environment_info"
	// MetricApplicationInfo is an application. Labels: environment,
	// application (its name), platform, criticality, status, managed_by.
	MetricApplicationInfo = "heartbeat_api_inventory_application_info"
	// MetricSourceInfo is a telemetry source. Labels: environment,
	// application, source_type, ingest_mode, application_name (its
	// config's, as the collector and the links name it), status,
	// managed_by, observation (whether the collector reads it: observed,
	// not_observed, unknown or not_applicable).
	MetricSourceInfo = "heartbeat_api_inventory_source_info"
	// MetricDatabaseTargetInfo is a database target. Labels: environment,
	// target (its name, the collector's target label), engine, status,
	// managed_by.
	MetricDatabaseTargetInfo = "heartbeat_api_inventory_database_target_info"
	// MetricInfoTruncated is 1 when a kind has more rows than
	// MaxInfoSeries, so its info metric lists only the first ones, 0
	// otherwise. Labels: kind (environment, application, source,
	// database_target).
	MetricInfoTruncated = "heartbeat_api_inventory_info_truncated"
)

// MaxInfoSeries is the most series each info metric has: the first rows
// in the order of their labels. A scrape stays bounded whatever the
// inventory holds; an inventory is written by people, so it should not
// come near.
const MaxInfoSeries = 1000

// Kinds, the values of MetricInfoTruncated's kind label.
const (
	kindEnvironment    = "environment"
	kindApplication    = "application"
	kindSource         = "source"
	kindDatabaseTarget = "database_target"
)

// infoTimeout bounds the queries and the collector's answer a scrape
// waits for.
const infoTimeout = 2 * time.Second

var (
	environmentInfoDesc = prometheus.NewDesc(MetricEnvironmentInfo,
		"An environment of the inventory; always 1.",
		[]string{"environment", "type", "status", "managed_by"}, nil)
	applicationInfoDesc = prometheus.NewDesc(MetricApplicationInfo,
		"An application of the inventory; always 1.",
		[]string{"environment", "application", "platform", "criticality", "status", "managed_by"}, nil)
	sourceInfoDesc = prometheus.NewDesc(MetricSourceInfo,
		"A telemetry source of the inventory, and whether the collector observes it; always 1.",
		[]string{"environment", "application", "source_type", "ingest_mode", "application_name", "status", "managed_by", "observation"}, nil)
	databaseTargetInfoDesc = prometheus.NewDesc(MetricDatabaseTargetInfo,
		"A database target of the inventory; always 1.",
		[]string{"environment", "target", "engine", "status", "managed_by"}, nil)
	infoTruncatedDesc = prometheus.NewDesc(MetricInfoTruncated,
		"1 when the kind has more rows than its info metric lists.",
		[]string{"kind"}, nil)
)

// InfoCollector returns the collector of the inventory's info metrics. It
// reads PostgreSQL at each scrape, so every replica reports every row
// whichever writer changed it (an endpoint, the file, another replica),
// with no refresh to miss. Whether a source is observed comes from the
// collector's admin API, which the Service's Observer already caches.
func (s *Service) InfoCollector() prometheus.Collector { return infoCollector{s: s} }

type infoCollector struct{ s *Service }

// Describe implements [prometheus.Collector].
func (c infoCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{environmentInfoDesc, applicationInfoDesc, sourceInfoDesc, databaseTargetInfoDesc, infoTruncatedDesc} {
		ch <- d
	}
}

// Collect implements [prometheus.Collector]. When PostgreSQL does not
// answer within infoTimeout, the scrape has none of these series rather
// than a partial inventory or a failed scrape: GET /readyz reports the
// database.
func (c infoCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), infoTimeout)
	defer cancel()
	snap, err := c.s.readInfo(ctx)
	if err != nil {
		return
	}
	snap.emit(ch)
}

// infoSnapshot is one scrape's rows, each kind cut at MaxInfoSeries.
type infoSnapshot struct {
	environments []store.EnvironmentInfoRow
	applications []store.ApplicationInfoRow
	sources      []store.SourceInfoRow
	observations []string
	targets      []store.DatabaseTargetInfoRow
	truncated    map[string]bool
}

func (s *Service) readInfo(ctx context.Context) (infoSnapshot, error) {
	q := store.New(s.db)
	limit := int32(MaxInfoSeries + 1)
	snap := infoSnapshot{truncated: map[string]bool{}}
	var err error
	if snap.environments, err = q.EnvironmentInfo(ctx, limit); err != nil {
		return infoSnapshot{}, err
	}
	if snap.applications, err = q.ApplicationInfo(ctx, limit); err != nil {
		return infoSnapshot{}, err
	}
	if snap.sources, err = q.SourceInfo(ctx, limit); err != nil {
		return infoSnapshot{}, err
	}
	if snap.targets, err = q.DatabaseTargetInfo(ctx, limit); err != nil {
		return infoSnapshot{}, err
	}
	snap.environments = cut(snap.environments, kindEnvironment, snap.truncated)
	snap.applications = cut(snap.applications, kindApplication, snap.truncated)
	snap.sources = cut(snap.sources, kindSource, snap.truncated)
	snap.targets = cut(snap.targets, kindDatabaseTarget, snap.truncated)

	sources := make([]Source, len(snap.sources))
	for i, r := range snap.sources {
		sources[i] = Source{SourceType: r.SourceType, IngestMode: r.IngestMode, environmentSlug: r.EnvironmentSlug}
	}
	s.observe(ctx, sources)
	snap.observations = make([]string, len(sources))
	for i, src := range sources {
		snap.observations[i] = src.Observation.Status
	}
	return snap, nil
}

// cut keeps the first MaxInfoSeries rows, and records whether there were
// more.
func cut[T any](rows []T, kind string, truncated map[string]bool) []T {
	truncated[kind] = len(rows) > MaxInfoSeries
	return rows[:min(len(rows), MaxInfoSeries)]
}

// emit sends one series per row. Two rows with the same labels would fail
// the whole scrape, so a repeated label set is sent once; the unique
// indexes leave only sources able to repeat one. A row whose labels the
// client refuses (a value that is not UTF-8) is left out rather than
// panicking in the scrape.
func (snap infoSnapshot) emit(ch chan<- prometheus.Metric) {
	seen := map[string]bool{}
	send := func(desc *prometheus.Desc, labels ...string) {
		key := desc.String() + "\xff" + strings.Join(labels, "\xff")
		if seen[key] {
			return
		}
		seen[key] = true
		if m, err := prometheus.NewConstMetric(desc, prometheus.GaugeValue, 1, labels...); err == nil {
			ch <- m
		}
	}
	for _, r := range snap.environments {
		send(environmentInfoDesc, r.Slug, r.Type, r.Status, r.ManagedBy)
	}
	for _, r := range snap.applications {
		send(applicationInfoDesc, r.EnvironmentSlug, r.Name, r.Platform, r.Criticality, r.Status, r.ManagedBy)
	}
	for i, r := range snap.sources {
		send(sourceInfoDesc, r.EnvironmentSlug, r.Application, r.SourceType, r.IngestMode, r.ApplicationName,
			r.Status, r.ManagedBy, snap.observations[i])
	}
	for _, r := range snap.targets {
		send(databaseTargetInfoDesc, r.EnvironmentSlug, r.Name, r.Engine, r.Status, r.ManagedBy)
	}
	for _, kind := range []string{kindEnvironment, kindApplication, kindSource, kindDatabaseTarget} {
		v := 0.0
		if snap.truncated[kind] {
			v = 1
		}
		ch <- prometheus.MustNewConstMetric(infoTruncatedDesc, prometheus.GaugeValue, v, kind)
	}
}
