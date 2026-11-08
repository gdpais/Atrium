// Package config loads, validates, versions, and redacts Heartbeat integration
// runtime configuration.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"time"
	// Embeds the IANA time zone database, so log_timezone validates the same
	// way in a distroless image as on a developer machine.
	_ "time/tzdata"

	"gopkg.in/yaml.v3"
)

// Collector kinds.
const (
	// KindSQLServer collects SQL Server health metrics with the probe catalog.
	KindSQLServer = "sqlserver"
	// KindOutSystemsLogs extracts OutSystems 11 logs from the logging
	// database and sends them as OTLP logs (ADR 0008).
	KindOutSystemsLogs = "outsystems_logs"
)

// Observability platforms. Heartbeat writes telemetry only as OTLP to the
// OpenTelemetry Collector and as Prometheus exposition on /metrics (ADR 0011),
// so the platform that stores and shows it is the Collector's and the chart's
// choice. platform names it for the parts that cannot be neutral: dashboard
// and deep-link templates, and later the API's log and metric queries.
const (
	// PlatformGrafana is Grafana with Prometheus, Loki and Alertmanager, the
	// platform the chart bundles.
	PlatformGrafana = "grafana"
	// PlatformGeneric is any other platform that takes OTLP, or scrapes
	// /metrics, through the Collector. Heartbeat has no dashboards, links or
	// queries for it.
	PlatformGeneric = "generic"
	// DefaultPlatform is the platform when the config names none.
	DefaultPlatform = PlatformGrafana
)

// supportedPlatforms are the platform values the loader accepts.
var supportedPlatforms = []string{PlatformGrafana, PlatformGeneric}

// Defaults and limits of the outsystems_logs settings.
const (
	// DefaultLogLag is how far behind now an outsystems_logs read stops, so
	// rows the platform's log service commits late are not skipped.
	DefaultLogLag = 60 * time.Second
	// DefaultLogBatchLimit is the most rows one log-type query returns.
	DefaultLogBatchLimit = 5000
	// MaxLogBatchLimit is the largest batch_limit accepted.
	MaxLogBatchLimit = 50000
	// DefaultLogInitialLookback is how far back extraction starts for a log
	// type with no stored watermark.
	DefaultLogInitialLookback = 15 * time.Minute
	// DefaultLogTimezone is the time zone OutSystems Instant values are read in.
	DefaultLogTimezone = "UTC"
	// DefaultLogRecountWindow is how far behind its watermark a log type's
	// rows are counted again, to find those committed past the lag.
	DefaultLogRecountWindow = 10 * time.Minute
	// MaxLogRecountWindow is the largest recount_window accepted.
	MaxLogRecountWindow = time.Hour
	// DefaultLogMaxDerivedSeries is the most derived-metric series an
	// outsystems_logs collector holds, counted as Prometheus stores them.  A
	// proposed default, not a measured need: 29 times the 1,716 series of
	// PatientZero's production-scale profile, and about 33 MiB of the
	// collector's memory at some 690 bytes a series
	// (docs/reference/configuration.md#outsystems_logs-collectors).
	DefaultLogMaxDerivedSeries = 50000
	// MaxLogDerivedSeries is the largest max_derived_series accepted.
	MaxLogDerivedSeries = 1000000
)

// Endpoint holds the connection details for an external service integration.
// Dashboards uses the templates, logs and alerting only base_url (their query
// APIs), opentelemetry only endpoint.
type Endpoint struct {
	BaseURL            string            `yaml:"base_url"`
	Endpoint           string            `yaml:"endpoint"`
	DashboardTemplates map[string]string `yaml:"dashboard_templates"`
	DeepLinkTemplates  map[string]string `yaml:"deep_link_templates"`
}

// NotificationChannel declares outbound delivery targets without embedding
// secret values in the runtime config.
type NotificationChannel struct {
	ID            string            `yaml:"id"`
	ChannelType   string            `yaml:"channel_type"`
	TargetRef     string            `yaml:"target_ref"`
	CredentialRef string            `yaml:"credential_ref"`
	Config        map[string]string `yaml:"config"`
}

type document struct {
	Platform             string                 `yaml:"platform"`
	Dashboards           Endpoint               `yaml:"dashboards"`
	Logs                 Endpoint               `yaml:"logs"`
	Alerting             Endpoint               `yaml:"alerting"`
	OpenTelemetry        Endpoint               `yaml:"opentelemetry"`
	NotificationChannels []NotificationChannel  `yaml:"notification_channels"`
	Collectors           []collectorDocument    `yaml:"collectors"`
	CredentialRefs       map[string]string      `yaml:"credential_refs"`
	Delivery             map[string]interface{} `yaml:"delivery"`
}

type collectorDocument struct {
	ID            string                  `yaml:"id"`
	Kind          string                  `yaml:"kind"`
	Enabled       bool                    `yaml:"enabled"`
	CredentialRef string                  `yaml:"credential_ref"`
	Config        collectorConfigDocument `yaml:"config"`
}

type collectorConfigDocument struct {
	Environment    string           `yaml:"environment"`
	ScrapeInterval string           `yaml:"scrape_interval"`
	TargetNames    []string         `yaml:"target_names"`
	Targets        []targetDocument `yaml:"targets"`
	Probes         []probeDocument  `yaml:"probes"`
	// outsystems_logs settings.
	Lag              string `yaml:"lag"`
	BatchLimit       *int   `yaml:"batch_limit"`
	InitialLookback  string `yaml:"initial_lookback"`
	LogTimezone      string `yaml:"log_timezone"`
	RecountWindow    string `yaml:"recount_window"`
	MaxDerivedSeries *int   `yaml:"max_derived_series"`
}

type targetDocument struct {
	Name          string          `yaml:"name"`
	Environment   string          `yaml:"environment"`
	Host          string          `yaml:"host"`
	Port          int             `yaml:"port"`
	DatabaseName  string          `yaml:"database_name"`
	CredentialRef string          `yaml:"credential_ref"`
	Probes        []probeDocument `yaml:"probes"`
}

type probeDocument struct {
	Name          string `yaml:"name"`
	QueryTemplate string `yaml:"query_template"`
	TimeoutMS     int    `yaml:"timeout_ms"`
	MaxRows       int    `yaml:"max_rows"`
	// Lag is the outsystems_logs per-log-type lag.
	Lag string `yaml:"lag"`
}

// CollectorRuntimeConfig is the normalised, ready-to-use representation of a
// single collector declaration from the integrations file.
type CollectorRuntimeConfig struct {
	ID             string
	Kind           string
	Enabled        bool
	CredentialRef  string
	Environment    string
	TargetNames    []string
	ScrapeInterval time.Duration
	Targets        []TargetRuntimeConfig
	Probes         []ProbeRuntimeConfig
	// OutSystemsLogs holds the settings of an outsystems_logs collector; it is
	// the zero value for every other kind.
	OutSystemsLogs OutSystemsLogsConfig
}

// OutSystemsLogsConfig holds the settings of an outsystems_logs collector.
// Each cycle (every ScrapeInterval) reads every selected log type of every
// target from its watermark to now minus Lag.
type OutSystemsLogsConfig struct {
	// Lag is how far behind now a read stops.
	Lag time.Duration
	// BatchLimit is the most rows one query returns.
	BatchLimit int
	// InitialLookback is how far back a log type without a stored watermark
	// starts.
	InitialLookback time.Duration
	// LogTimezone is the IANA name of the time zone Instant is stored in.
	LogTimezone string
	// RecountWindow is how far behind its watermark each log type counts its
	// rows again, once a minute, to find rows committed past the lag; 0 turns
	// the recount off.
	RecountWindow time.Duration
	// MaxDerivedSeries is the most derived-metric series the collector
	// holds, counted as Prometheus stores them (a histogram's buckets, sum
	// and count each).  Past it, a row that needs a new series is not
	// counted in it, and the overflow counter says so; series that exist
	// keep updating.
	MaxDerivedSeries int
	// LogsEndpoint is opentelemetry.endpoint, the OTLP/HTTP base URL the logs
	// are sent to. It is copied here so a change restarts the collector.
	LogsEndpoint string
}

// TargetRuntimeConfig holds the connection parameters and probe assignments
// for a single database target.
type TargetRuntimeConfig struct {
	Name            string
	EnvironmentSlug string
	Engine          string
	Host            string
	Port            int
	DatabaseName    string
	CredentialRef   string
	Probes          []ProbeRuntimeConfig
}

// MaxProbeTimeout is the longest a single probe execution may run.  A probe's
// timeout_ms above it is rejected, and collectors cap every probe timeout at
// it.  Database connectors size their socket read deadlines from it, so a
// probe still within its timeout is never cut off by the driver.
const MaxProbeTimeout = 25 * time.Second

// Row limits of sqlserver probes.  A probe that returns more rows than its
// max_rows fails instead of being held in memory and exported in full.
const (
	// DefaultProbeMaxRows is the most rows a probe reads when it sets no
	// max_rows.  The built-in probes return at most a few hundred.
	DefaultProbeMaxRows = 10000
	// MaxProbeMaxRows is the largest max_rows accepted.
	MaxProbeMaxRows = 100000
)

// ProbeRuntimeConfig describes a single probe to execute against a target.
type ProbeRuntimeConfig struct {
	Name          string
	QueryTemplate string
	TimeoutMS     int
	// MaxRows, on a sqlserver collector, is the most rows the probe may
	// return; 0 means [DefaultProbeMaxRows].  outsystems_logs collectors
	// bound their reads with batch_limit instead.
	MaxRows int
	// Lag, on an outsystems_logs collector, replaces the collector's lag for
	// this log type; 0 means the collector's.  A log type whose Instant is
	// the start of the operation (screen, cyclic_job) is written when the
	// operation ends, so its lag must exceed the longest operation.
	Lag time.Duration
}

// RuntimeConfig is the fully parsed and validated integrations file.
type RuntimeConfig struct {
	Version string
	// Platform is the observability platform: grafana unless set.
	Platform string
	// Dashboards is the platform's UI (Grafana): base URL, dashboard and
	// deep-link templates.
	Dashboards Endpoint
	// Logs is the platform's log query API (Loki).
	Logs Endpoint
	// Alerting is the platform's alert API (Alertmanager).
	Alerting Endpoint
	// OpenTelemetry is the OTel Collector Heartbeat sends OTLP to.
	OpenTelemetry        Endpoint
	NotificationChannels []NotificationChannel
	Collectors           []CollectorRuntimeConfig
	CredentialRefs       map[string]string
}

// LoadRuntimeConfig reads, validates, normalises, and versions a candidate
// integrations YAML file. Invalid candidates return an error and no partial
// RuntimeConfig.
func LoadRuntimeConfig(path string) (RuntimeConfig, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("read runtime config: %w", err)
	}
	return ParseRuntimeConfig(content)
}

// decodeDocument decodes an integrations YAML file, refusing keys the format
// does not define: a misspelt key (scrape_intervl, max_row) would otherwise
// be dropped without a word and its setting fall back to the default, which
// is how a reviewed file comes to run with values nobody chose.  An empty
// file is an empty document.
func decodeDocument(content []byte) (document, error) {
	var doc document
	dec := yaml.NewDecoder(bytes.NewReader(content))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return document{}, err
	}
	return doc, nil
}

// ParseRuntimeConfig validates, normalises, and versions the content of a
// candidate integrations YAML file, as LoadRuntimeConfig does a file's.
func ParseRuntimeConfig(content []byte) (RuntimeConfig, error) {
	doc, err := decodeDocument(content)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("parse runtime config: %w", err)
	}
	cfg := RuntimeConfig{
		Version:              hash(content),
		Platform:             strings.TrimSpace(doc.Platform),
		Dashboards:           normalizeEndpoint(doc.Dashboards),
		Logs:                 normalizeEndpoint(doc.Logs),
		Alerting:             normalizeEndpoint(doc.Alerting),
		OpenTelemetry:        normalizeEndpoint(doc.OpenTelemetry),
		NotificationChannels: append([]NotificationChannel(nil), doc.NotificationChannels...),
		CredentialRefs:       copyStringMap(doc.CredentialRefs),
	}
	if cfg.Platform == "" {
		cfg.Platform = DefaultPlatform
	}
	for _, collector := range doc.Collectors {
		runtimeCfg, err := normalizeCollector(collector)
		if err != nil {
			return RuntimeConfig{}, err
		}
		if runtimeCfg.Kind == KindOutSystemsLogs {
			runtimeCfg.OutSystemsLogs.LogsEndpoint = cfg.OpenTelemetry.Endpoint
		}
		cfg.Collectors = append(cfg.Collectors, runtimeCfg)
	}
	if err := validate(cfg); err != nil {
		return RuntimeConfig{}, err
	}
	return cfg, nil
}

// EnabledCollectors returns a deterministically sorted slice of enabled
// collectors of any of kinds.
func (c RuntimeConfig) EnabledCollectors(kinds ...string) []CollectorRuntimeConfig {
	var out []CollectorRuntimeConfig
	for _, collector := range c.Collectors {
		if collector.Enabled && slices.Contains(kinds, collector.Kind) {
			out = append(out, collector)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// redactedValue replaces secret values in diagnostics.
const redactedValue = "<redacted>"

// Redacted returns a copy suitable for diagnostics. Secret references remain
// visible, but their concrete values are masked. Notification channel config
// values are masked too, since channel settings such as SMTP or webhook
// options can carry secrets, and userinfo (user:password@) is stripped from
// every URL-bearing field.
func (c RuntimeConfig) Redacted() RuntimeConfig {
	out := c
	out.Dashboards = redactEndpoint(c.Dashboards)
	out.Logs = redactEndpoint(c.Logs)
	out.Alerting = redactEndpoint(c.Alerting)
	out.OpenTelemetry = redactEndpoint(c.OpenTelemetry)
	out.Collectors = cloneCollectors(c.Collectors)
	out.NotificationChannels = cloneNotificationChannels(c.NotificationChannels)
	out.CredentialRefs = map[string]string{}
	for key := range c.CredentialRefs {
		out.CredentialRefs[key] = redactedValue
	}
	for i := range out.NotificationChannels {
		channel := &out.NotificationChannels[i]
		if channel.CredentialRef != "" {
			channel.CredentialRef = redactRef(channel.CredentialRef)
		}
		channel.TargetRef = redactTargetRef(channel.TargetRef)
		for key := range channel.Config {
			channel.Config[key] = redactedValue
		}
	}
	for i := range out.Collectors {
		out.Collectors[i].CredentialRef = redactRef(out.Collectors[i].CredentialRef)
		for j := range out.Collectors[i].Targets {
			out.Collectors[i].Targets[j].CredentialRef = redactRef(out.Collectors[i].Targets[j].CredentialRef)
		}
	}
	return out
}

// redactEndpoint returns a copy of in with userinfo stripped from its URLs
// and URL templates.
func redactEndpoint(in Endpoint) Endpoint {
	out := cloneEndpoint(in)
	out.BaseURL = stripUserinfo(out.BaseURL)
	out.Endpoint = stripUserinfo(out.Endpoint)
	for key, value := range out.DashboardTemplates {
		out.DashboardTemplates[key] = stripUserinfo(value)
	}
	for key, value := range out.DeepLinkTemplates {
		out.DeepLinkTemplates[key] = stripUserinfo(value)
	}
	return out
}

func cloneEndpoint(in Endpoint) Endpoint {
	in.DashboardTemplates = copyStringMap(in.DashboardTemplates)
	in.DeepLinkTemplates = copyStringMap(in.DeepLinkTemplates)
	return in
}

func cloneCollectors(in []CollectorRuntimeConfig) []CollectorRuntimeConfig {
	out := make([]CollectorRuntimeConfig, len(in))
	for i, collector := range in {
		out[i] = collector
		out[i].TargetNames = append([]string(nil), collector.TargetNames...)
		out[i].Probes = append([]ProbeRuntimeConfig(nil), collector.Probes...)
		out[i].Targets = make([]TargetRuntimeConfig, len(collector.Targets))
		for j, target := range collector.Targets {
			out[i].Targets[j] = target
			out[i].Targets[j].Probes = append([]ProbeRuntimeConfig(nil), target.Probes...)
		}
	}
	return out
}

func cloneNotificationChannels(in []NotificationChannel) []NotificationChannel {
	out := make([]NotificationChannel, len(in))
	for i, channel := range in {
		out[i] = channel
		if channel.Config != nil {
			out[i].Config = make(map[string]string, len(channel.Config))
			for key, value := range channel.Config {
				out[i].Config[key] = value
			}
		}
	}
	return out
}

func emptyEndpoint(endpoint Endpoint) bool {
	return endpoint.BaseURL == "" && endpoint.Endpoint == "" &&
		len(endpoint.DashboardTemplates) == 0 && len(endpoint.DeepLinkTemplates) == 0
}

func normalizeEndpoint(endpoint Endpoint) Endpoint {
	if endpoint.DashboardTemplates == nil {
		endpoint.DashboardTemplates = map[string]string{}
	}
	if endpoint.DeepLinkTemplates == nil {
		endpoint.DeepLinkTemplates = map[string]string{}
	}
	return endpoint
}

func normalizeCollector(doc collectorDocument) (CollectorRuntimeConfig, error) {
	cfg := CollectorRuntimeConfig{
		ID:             strings.TrimSpace(doc.ID),
		Kind:           strings.TrimSpace(doc.Kind),
		Enabled:        doc.Enabled,
		CredentialRef:  strings.TrimSpace(doc.CredentialRef),
		ScrapeInterval: 30 * time.Second,
		Environment:    strings.TrimSpace(doc.Config.Environment),
		TargetNames:    append([]string(nil), doc.Config.TargetNames...),
	}
	probes, err := normalizeProbes(doc.ID, doc.Config.Probes)
	if err != nil {
		return CollectorRuntimeConfig{}, err
	}
	cfg.Probes = probes
	if doc.Config.ScrapeInterval != "" {
		interval, err := time.ParseDuration(doc.Config.ScrapeInterval)
		if err != nil {
			return CollectorRuntimeConfig{}, fmt.Errorf("invalid scrape_interval for collector %s: %w", doc.ID, err)
		}
		cfg.ScrapeInterval = interval
	}
	logs, err := normalizeOutSystemsLogs(doc)
	if err != nil {
		return CollectorRuntimeConfig{}, err
	}
	cfg.OutSystemsLogs = logs
	for _, target := range doc.Config.Targets {
		targetProbes, err := normalizeProbes(doc.ID, target.Probes)
		if err != nil {
			return CollectorRuntimeConfig{}, err
		}
		runtimeTarget := TargetRuntimeConfig{
			Name:            strings.TrimSpace(target.Name),
			EnvironmentSlug: strings.TrimSpace(target.Environment),
			Engine:          cfg.Kind,
			Host:            strings.TrimSpace(target.Host),
			Port:            target.Port,
			DatabaseName:    strings.TrimSpace(target.DatabaseName),
			CredentialRef:   strings.TrimSpace(target.CredentialRef),
			Probes:          targetProbes,
		}
		if runtimeTarget.EnvironmentSlug == "" {
			runtimeTarget.EnvironmentSlug = cfg.Environment
		}
		if runtimeTarget.CredentialRef == "" {
			runtimeTarget.CredentialRef = cfg.CredentialRef
		}
		if len(runtimeTarget.Probes) == 0 {
			runtimeTarget.Probes = append(runtimeTarget.Probes, cfg.Probes...)
		}
		cfg.Targets = append(cfg.Targets, runtimeTarget)
	}
	return cfg, nil
}

// normalizeOutSystemsLogs parses the outsystems_logs settings of doc and
// fills in their defaults. Other kinds must not set them.
func normalizeOutSystemsLogs(doc collectorDocument) (OutSystemsLogsConfig, error) {
	c := doc.Config
	if strings.TrimSpace(doc.Kind) != KindOutSystemsLogs {
		for _, field := range []struct {
			name string
			set  bool
		}{
			{"lag", c.Lag != ""},
			{"batch_limit", c.BatchLimit != nil},
			{"initial_lookback", c.InitialLookback != ""},
			{"log_timezone", c.LogTimezone != ""},
			{"recount_window", c.RecountWindow != ""},
			{"max_derived_series", c.MaxDerivedSeries != nil},
		} {
			if field.set {
				return OutSystemsLogsConfig{}, fmt.Errorf("collector %s: %s applies only to %s collectors", doc.ID, field.name, KindOutSystemsLogs)
			}
		}
		return OutSystemsLogsConfig{}, nil
	}
	cfg := OutSystemsLogsConfig{
		Lag:              DefaultLogLag,
		BatchLimit:       DefaultLogBatchLimit,
		InitialLookback:  DefaultLogInitialLookback,
		LogTimezone:      DefaultLogTimezone,
		RecountWindow:    DefaultLogRecountWindow,
		MaxDerivedSeries: DefaultLogMaxDerivedSeries,
	}
	for _, field := range []struct {
		name  string
		value string
		out   *time.Duration
	}{
		{"lag", c.Lag, &cfg.Lag},
		{"initial_lookback", c.InitialLookback, &cfg.InitialLookback},
		{"recount_window", c.RecountWindow, &cfg.RecountWindow},
	} {
		if field.value == "" {
			continue
		}
		parsed, err := time.ParseDuration(field.value)
		if err != nil {
			return OutSystemsLogsConfig{}, fmt.Errorf("invalid %s for collector %s: %w", field.name, doc.ID, err)
		}
		*field.out = parsed
	}
	if c.BatchLimit != nil {
		cfg.BatchLimit = *c.BatchLimit
	}
	if c.MaxDerivedSeries != nil {
		cfg.MaxDerivedSeries = *c.MaxDerivedSeries
	}
	if tz := strings.TrimSpace(c.LogTimezone); tz != "" {
		cfg.LogTimezone = tz
	}
	return cfg, nil
}

// normalizeProbes parses the probes of collector collectorID.  A probe's lag
// must be a positive duration, so a parsed Lag of 0 always means "not set".
func normalizeProbes(collectorID string, docs []probeDocument) ([]ProbeRuntimeConfig, error) {
	probes := make([]ProbeRuntimeConfig, 0, len(docs))
	for _, doc := range docs {
		if strings.TrimSpace(doc.Name) == "" {
			continue
		}
		probe := ProbeRuntimeConfig{
			Name:          strings.TrimSpace(doc.Name),
			QueryTemplate: doc.QueryTemplate,
			TimeoutMS:     doc.TimeoutMS,
			MaxRows:       doc.MaxRows,
		}
		if doc.Lag != "" {
			lag, err := time.ParseDuration(doc.Lag)
			if err != nil {
				return nil, fmt.Errorf("invalid lag for collector %s probe %s: %w", collectorID, probe.Name, err)
			}
			if lag <= 0 {
				return nil, fmt.Errorf("collector %s probe %s lag must be positive", collectorID, probe.Name)
			}
			probe.Lag = lag
		}
		probes = append(probes, probe)
	}
	return probes, nil
}

func validate(cfg RuntimeConfig) error {
	if err := validateEndpointCredentials(cfg); err != nil {
		return err
	}
	if cfg.Platform != "" && !slices.Contains(supportedPlatforms, cfg.Platform) {
		return fmt.Errorf("platform %q is not supported (supported: %s)", cfg.Platform, strings.Join(supportedPlatforms, ", "))
	}
	// Heartbeat never writes to the platform directly: logs go through
	// opentelemetry.endpoint, alerts through the platform's own rules.
	if cfg.Logs.Endpoint != "" {
		return fmt.Errorf("logs.endpoint is not supported: logs are sent to opentelemetry.endpoint")
	}
	if cfg.Alerting.Endpoint != "" {
		return fmt.Errorf("alerting.endpoint is not supported: Heartbeat does not send alerts itself")
	}
	if cfg.Platform == PlatformGeneric {
		for _, section := range []struct {
			name     string
			endpoint Endpoint
		}{{"dashboards", cfg.Dashboards}, {"logs", cfg.Logs}, {"alerting", cfg.Alerting}} {
			if !emptyEndpoint(section.endpoint) {
				return fmt.Errorf("%s must be empty with platform %s: Heartbeat has no links or queries for it", section.name, PlatformGeneric)
			}
		}
	}
	if err := validateURL("dashboards.base_url", cfg.Dashboards.BaseURL, false); err != nil {
		return err
	}
	if err := validateURL("logs.base_url", cfg.Logs.BaseURL, false); err != nil {
		return err
	}
	if err := validateURL("alerting.base_url", cfg.Alerting.BaseURL, false); err != nil {
		return err
	}
	if err := validateURL("opentelemetry.endpoint", cfg.OpenTelemetry.Endpoint, false); err != nil {
		return err
	}
	for key, ref := range cfg.CredentialRefs {
		if ref != "" && !ValidSecretRef(ref) {
			return fmt.Errorf("credential_refs.%s must be a secret reference", key)
		}
	}
	ids := map[string]struct{}{}
	for _, collector := range cfg.Collectors {
		if collector.ID == "" {
			return fmt.Errorf("collector id is required")
		}
		if _, exists := ids[collector.ID]; exists {
			return fmt.Errorf("collector id %q must be unique", collector.ID)
		}
		ids[collector.ID] = struct{}{}
		if collector.Kind == "" {
			return fmt.Errorf("collector kind is required for %s", collector.ID)
		}
		if collector.ScrapeInterval <= 0 {
			return fmt.Errorf("collector %s must have positive scrape interval", collector.ID)
		}
		if collector.CredentialRef != "" && !ValidSecretRef(collector.CredentialRef) {
			return fmt.Errorf("collector %s credential_ref must be a secret reference", collector.ID)
		}
		if err := validateTargets(collector); err != nil {
			return err
		}
		if err := validateProbeQueries(collector); err != nil {
			return err
		}
		if collector.Kind == KindOutSystemsLogs {
			if err := validateOutSystemsLogs(collector, cfg.OpenTelemetry.Endpoint); err != nil {
				return err
			}
		}
	}
	if err := validateUniqueSQLServerTargets(cfg.Collectors); err != nil {
		return err
	}
	if err := validateUniqueOutSystemsLogSources(cfg.Collectors); err != nil {
		return err
	}
	notificationIDs := map[string]struct{}{}
	for _, channel := range cfg.NotificationChannels {
		if channel.ID == "" {
			return fmt.Errorf("notification channel id is required")
		}
		if _, exists := notificationIDs[channel.ID]; exists {
			return fmt.Errorf("notification channel id %q must be unique", channel.ID)
		}
		notificationIDs[channel.ID] = struct{}{}
		if channel.ChannelType == "" || channel.TargetRef == "" {
			return fmt.Errorf("notification channel %s requires channel_type and target_ref", channel.ID)
		}
		if targetRefCredentials(channel.TargetRef) {
			return fmt.Errorf("notification channel %s target_ref %s", channel.ID, embeddedCredentialsHint)
		}
		if channel.CredentialRef != "" && !ValidSecretRef(channel.CredentialRef) {
			return fmt.Errorf("notification channel %s credential_ref must be a secret reference", channel.ID)
		}
	}
	return nil
}

func validateTargets(collector CollectorRuntimeConfig) error {
	targets := map[string]struct{}{}
	for _, target := range collector.Targets {
		if target.Name == "" {
			return fmt.Errorf("collector %s target name is required", collector.ID)
		}
		if _, exists := targets[target.Name]; exists {
			return fmt.Errorf("collector %s target %q must be unique", collector.ID, target.Name)
		}
		targets[target.Name] = struct{}{}
		if target.Host == "" {
			return fmt.Errorf("collector %s target %s host is required", collector.ID, target.Name)
		}
		if !ValidHost(target.Host) {
			return fmt.Errorf("collector %s target %s host must be a host name, an IPv4 address or a bare IPv6 address, with no port, brackets, credentials or path; set the port in port", collector.ID, target.Name)
		}
		if target.Port < 1 || target.Port > 65535 {
			return fmt.Errorf("collector %s target %s port must be between 1 and 65535", collector.ID, target.Name)
		}
		if target.CredentialRef != "" && !ValidSecretRef(target.CredentialRef) {
			return fmt.Errorf("collector %s target %s credential_ref must be a secret reference", collector.ID, target.Name)
		}
		for _, probe := range target.Probes {
			if probe.TimeoutMS < 0 {
				return fmt.Errorf("collector %s target %s probe %s timeout_ms cannot be negative", collector.ID, target.Name, probe.Name)
			}
			if maxMS := MaxProbeTimeout.Milliseconds(); int64(probe.TimeoutMS) > maxMS {
				return fmt.Errorf("collector %s target %s probe %s timeout_ms %d exceeds the maximum of %d", collector.ID, target.Name, probe.Name, probe.TimeoutMS, maxMS)
			}
			if probe.Lag != 0 && collector.Kind != KindOutSystemsLogs {
				return fmt.Errorf("collector %s target %s probe %s: lag applies only to %s collectors", collector.ID, target.Name, probe.Name, KindOutSystemsLogs)
			}
		}
	}
	for _, selected := range collector.TargetNames {
		if _, exists := targets[selected]; !exists && len(targets) > 0 {
			return fmt.Errorf("collector %s target_names references unknown target %q", collector.ID, selected)
		}
	}
	return nil
}

// validateProbeQueries checks the query_template and max_rows of every probe
// of collector, its default probes and each target's own.
func validateProbeQueries(collector CollectorRuntimeConfig) error {
	check := func(scope string, probes []ProbeRuntimeConfig) error {
		for _, probe := range probes {
			if probe.QueryTemplate != "" {
				// Defense in depth: the collector's read-only login is what keeps
				// probes from writing (see ValidateReadOnlySelect).
				if err := ValidateReadOnlySelect(probe.QueryTemplate); err != nil {
					return fmt.Errorf("%s probe %s query_template must be one read-only SELECT: it %w", scope, probe.Name, err)
				}
				// The recount counts the query's rows as a derived table.
				if collector.Kind == KindOutSystemsLogs && collector.OutSystemsLogs.RecountWindow > 0 {
					if _, err := DerivedTable(probe.QueryTemplate); err != nil {
						return fmt.Errorf("%s probe %s query_template cannot be recounted: it %w; change the query_template or set recount_window: 0s", scope, probe.Name, err)
					}
				}
			}
			if probe.MaxRows == 0 {
				continue
			}
			if collector.Kind != KindSQLServer {
				return fmt.Errorf("%s probe %s: max_rows applies only to %s collectors", scope, probe.Name, KindSQLServer)
			}
			if probe.MaxRows < 0 || probe.MaxRows > MaxProbeMaxRows {
				return fmt.Errorf("%s probe %s max_rows must be between 1 and %d", scope, probe.Name, MaxProbeMaxRows)
			}
		}
		return nil
	}
	if err := check("collector "+collector.ID, collector.Probes); err != nil {
		return err
	}
	for _, target := range collector.Targets {
		if err := check(fmt.Sprintf("collector %s target %s", collector.ID, target.Name), target.Probes); err != nil {
			return err
		}
	}
	return nil
}

// validateOutSystemsLogs checks the settings of an outsystems_logs
// collector. Whether its log types exist is checked by the db-collector,
// which embeds the queries (see WithValidator).
func validateOutSystemsLogs(collector CollectorRuntimeConfig, logsEndpoint string) error {
	logs := collector.OutSystemsLogs
	if collector.Enabled && logsEndpoint == "" {
		return fmt.Errorf("collector %s: opentelemetry.endpoint is required for %s collectors", collector.ID, KindOutSystemsLogs)
	}
	if logs.Lag <= 0 {
		return fmt.Errorf("collector %s lag must be positive", collector.ID)
	}
	if logs.InitialLookback <= 0 {
		return fmt.Errorf("collector %s initial_lookback must be positive", collector.ID)
	}
	if w := logs.RecountWindow; w != 0 && (w < 2*time.Minute || w > MaxLogRecountWindow || w%time.Minute != 0) {
		return fmt.Errorf("collector %s recount_window must be 0s (off) or whole minutes from 2m to %s", collector.ID, MaxLogRecountWindow)
	}
	if logs.BatchLimit < 1 || logs.BatchLimit > MaxLogBatchLimit {
		return fmt.Errorf("collector %s batch_limit must be between 1 and %d", collector.ID, MaxLogBatchLimit)
	}
	if logs.MaxDerivedSeries < 1 || logs.MaxDerivedSeries > MaxLogDerivedSeries {
		return fmt.Errorf("collector %s max_derived_series must be between 1 and %d", collector.ID, MaxLogDerivedSeries)
	}
	if _, err := time.LoadLocation(logs.LogTimezone); err != nil || logs.LogTimezone == "Local" {
		return fmt.Errorf("collector %s log_timezone %q must be an IANA time zone name such as UTC or Europe/Lisbon", collector.ID, logs.LogTimezone)
	}
	// Each log type of a target has one watermark, so it can be listed only
	// once.
	for _, target := range collector.Targets {
		seen := map[string]bool{}
		for _, probe := range target.Probes {
			if seen[probe.Name] {
				return fmt.Errorf("collector %s target %s lists log type %q more than once", collector.ID, target.Name, probe.Name)
			}
			seen[probe.Name] = true
		}
	}
	return nil
}

// validateUniqueSQLServerTargets rejects a target, identified by environment
// and name, that more than one enabled sqlserver collector scrapes.  Probe
// series carry only environment and target labels, so two collectors would
// write the same series: counters would jump between their readings, which
// rate() reads as resets, and the database would be polled twice.
func validateUniqueSQLServerTargets(collectors []CollectorRuntimeConfig) error {
	type targetKey struct{ environment, name string }
	owners := map[targetKey]string{}
	for _, collector := range collectors {
		if !collector.Enabled || collector.Kind != KindSQLServer {
			continue
		}
		selected := map[string]bool{}
		for _, name := range collector.TargetNames {
			selected[name] = true
		}
		for _, target := range collector.Targets {
			if len(selected) > 0 && !selected[target.Name] {
				continue
			}
			key := targetKey{environment: target.EnvironmentSlug, name: target.Name}
			if owner, exists := owners[key]; exists {
				return fmt.Errorf("target %q in environment %q is scraped by collectors %s and %s; enable it in one collector only", target.Name, target.EnvironmentSlug, owner, collector.ID)
			}
			owners[key] = collector.ID
		}
	}
	return nil
}

// validateUniqueOutSystemsLogSources rejects a log type of one logging
// database, identified by host, port and database name, that more than one
// enabled outsystems_logs collector extracts.  Each keeps its own watermark,
// so every row would reach Loki twice and be counted twice.
func validateUniqueOutSystemsLogSources(collectors []CollectorRuntimeConfig) error {
	type sourceKey struct {
		host     string
		port     int
		database string
		logType  string
	}
	owners := map[sourceKey]string{}
	for _, collector := range collectors {
		if !collector.Enabled || collector.Kind != KindOutSystemsLogs {
			continue
		}
		selected := map[string]bool{}
		for _, name := range collector.TargetNames {
			selected[name] = true
		}
		for _, target := range collector.Targets {
			if len(selected) > 0 && !selected[target.Name] {
				continue
			}
			for _, probe := range target.Probes {
				key := sourceKey{
					host:     strings.ToLower(target.Host),
					port:     target.Port,
					database: strings.ToLower(target.DatabaseName),
					logType:  probe.Name,
				}
				if owner, exists := owners[key]; exists && owner != collector.ID {
					return fmt.Errorf("log type %q of database %s on %s:%d is extracted by collectors %s and %s; enable it in one collector only", probe.Name, target.DatabaseName, target.Host, target.Port, owner, collector.ID)
				}
				owners[key] = collector.ID
			}
		}
	}
	return nil
}

func validateURL(name, raw string, required bool) error {
	if raw == "" {
		if required {
			return fmt.Errorf("%s is required", name)
		}
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("%s must be an absolute URL", name)
	}
	return nil
}

// embeddedCredentialsHint ends validation errors for URLs with userinfo. The
// URL itself is never echoed, since it contains the credential.
const embeddedCredentialsHint = "must not embed credentials (user:password@); use a credential reference"

// validateEndpointCredentials rejects userinfo in every URL-bearing endpoint
// field: base_url, endpoint and the dashboard and deep-link templates of
// dashboards, logs, alerting and opentelemetry. Credentials belong in secret
// references; a URL is copied into logs, diagnostics, browser links and
// error messages.
func validateEndpointCredentials(cfg RuntimeConfig) error {
	for _, section := range []struct {
		name     string
		endpoint Endpoint
	}{
		{"dashboards", cfg.Dashboards},
		{"logs", cfg.Logs},
		{"alerting", cfg.Alerting},
		{"opentelemetry", cfg.OpenTelemetry},
	} {
		if hasUserinfo(section.endpoint.BaseURL) {
			return fmt.Errorf("%s.base_url %s", section.name, embeddedCredentialsHint)
		}
		if hasUserinfo(section.endpoint.Endpoint) {
			return fmt.Errorf("%s.endpoint %s", section.name, embeddedCredentialsHint)
		}
		for _, templates := range []struct {
			field  string
			values map[string]string
		}{
			{"dashboard_templates", section.endpoint.DashboardTemplates},
			{"deep_link_templates", section.endpoint.DeepLinkTemplates},
		} {
			for key, value := range templates.values {
				if hasUserinfo(value) {
					return fmt.Errorf("%s.%s.%s %s", section.name, templates.field, key, embeddedCredentialsHint)
				}
			}
		}
	}
	return nil
}

// specialSchemes are the WHATWG "special" schemes. Browsers parse an
// authority after them even with backslashes, one slash or none
// ("http:\\user:pass@host", "http:user:pass@host").
var specialSchemes = map[string]bool{"http": true, "https": true, "ws": true, "wss": true, "ftp": true, "file": true}

// userinfoSpan returns the byte range [start, end) of the userinfo in raw,
// including its trailing '@', and whether raw has userinfo at all.
//
// It works on the text rather than on url.Parse, so it also covers values
// url.Parse rejects (templates with ${placeholders} in the host) and the
// lenient forms browsers accept for special schemes. As in net/url, the
// authority ends at the first '/', '?' or '#' (and, for special schemes as in
// browsers, '\'), and userinfo ends at the last '@' in it.
func userinfoSpan(raw string) (start, end int, ok bool) {
	isSlash := func(c byte) bool { return c == '/' || c == '\\' }
	// Browsers strip leading C0 controls and spaces before parsing.
	i := len(raw) - len(strings.TrimLeftFunc(raw, func(r rune) bool { return r <= ' ' }))
	delimiters := "/?#"
	if colon := schemeEnd(raw[i:]); colon > 0 {
		scheme := strings.ToLower(raw[i : i+colon])
		i += colon + 1
		if specialSchemes[scheme] {
			delimiters = "/\\?#"
		} else if !strings.HasPrefix(raw[i:], "//") {
			return 0, 0, false // opaque, such as mailto:ops@example.com
		}
	} else if len(raw)-i < 2 || !isSlash(raw[i]) || !isSlash(raw[i+1]) {
		return 0, 0, false // a path, query or fragment: no authority
	}
	for i < len(raw) && isSlash(raw[i]) {
		i++
	}
	authority := raw[i:]
	if stop := strings.IndexAny(authority, delimiters); stop >= 0 {
		authority = authority[:stop]
	}
	at := strings.LastIndexByte(authority, '@')
	if at < 0 {
		return 0, 0, false
	}
	return i, i + at + 1, true
}

// schemeEnd returns the index of the ':' that ends raw's URL scheme, or -1
// when raw does not start with a scheme.
func schemeEnd(raw string) int {
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z':
		case '0' <= c && c <= '9', c == '+', c == '-', c == '.':
			if i == 0 {
				return -1
			}
		case c == ':':
			if i == 0 {
				return -1
			}
			return i
		default:
			return -1
		}
	}
	return -1
}

// hasUserinfo reports whether raw is a URL or URL template with userinfo.
func hasUserinfo(raw string) bool {
	_, _, ok := userinfoSpan(removeTabsAndNewlines(raw))
	return ok
}

// stripUserinfo removes userinfo from raw and leaves everything else,
// including template placeholders and encoding, unchanged apart from the
// tabs and newlines browsers ignore.
func stripUserinfo(raw string) string {
	raw = removeTabsAndNewlines(raw)
	start, end, ok := userinfoSpan(raw)
	if !ok {
		return raw
	}
	return raw[:start] + raw[end:]
}

// removeTabsAndNewlines drops ASCII tab, CR and LF anywhere in raw, as the
// WHATWG URL parser does before parsing, so "ht\ttp://u:p@h" is scanned as
// the browser sees it.
func removeTabsAndNewlines(raw string) string {
	if !strings.ContainsAny(raw, "\t\r\n") {
		return raw
	}
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, raw)
}

// bareUserinfoEnd returns the index just past the '@' of a scheme-less
// "user:password@host" value, or -1. Such a value is not a URL with an
// authority, but tools like curl read it as credentials for http://host.
// "mailto:" is the one opaque scheme a target_ref legitimately uses with an
// '@', so it is exempt; a bare address such as ops@example.com has no ':' and
// never matches.
func bareUserinfoEnd(raw string) int {
	raw = removeTabsAndNewlines(raw)
	head := strings.TrimLeftFunc(raw, func(r rune) bool { return r <= ' ' })
	offset := len(raw) - len(head)
	if stop := strings.IndexAny(head, "/\\?#"); stop >= 0 {
		head = head[:stop]
	}
	at := strings.LastIndexByte(head, '@')
	if at < 0 {
		return -1
	}
	user, _, hasColon := strings.Cut(head[:at], ":")
	if !hasColon || strings.EqualFold(user, "mailto") {
		return -1
	}
	return offset + at + 1
}

// targetRefCredentials reports whether a notification channel target_ref
// carries credentials, as URL userinfo or as a bare user:password@host.
func targetRefCredentials(ref string) bool {
	return hasUserinfo(ref) || bareUserinfoEnd(ref) >= 0
}

// redactTargetRef strips credentials from a notification channel target_ref.
func redactTargetRef(ref string) string {
	ref = stripUserinfo(ref) // also drops tabs and newlines
	if end := bareUserinfoEnd(ref); end >= 0 {
		return ref[end:]
	}
	return ref
}

// ValidHost reports whether host is safe to join with a port into the
// connection URL, for the collector and for the API's database targets: a DNS name or IPv4 address made of letters, digits, '.',
// '-' and '_', or a bare IPv6 literal (net.JoinHostPort adds the brackets, so
// a bracketed value would be double-bracketed). Anything else, such as
// "db:1433", "user@db", "db/instance", "db\instance", whitespace or control
// characters, makes the driver fail to parse the connection URL, and its
// parse error would quote the URL, password included.
func ValidHost(host string) bool {
	if strings.Contains(host, ":") {
		ip := net.ParseIP(host)
		return ip != nil && ip.To4() == nil
	}
	for _, c := range []byte(host) {
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '.', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// ValidSecretRef reports whether ref is a credential reference: kv/, secret/
// or env/ and a name, which the collector resolves from the key
// credentials.Key(ref) of its mounted credentials Secret. The API checks
// database targets' references with it, and more strictly (package
// inventory), so it stores none the collector would refuse.
func ValidSecretRef(ref string) bool {
	return strings.HasPrefix(ref, "kv/") || strings.HasPrefix(ref, "secret/") || strings.HasPrefix(ref, "env/")
}

func redactRef(ref string) string {
	if ref == "" {
		return ""
	}
	return ref + ":<redacted>"
}

func copyStringMap(in map[string]string) map[string]string {
	if in == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func hash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}
