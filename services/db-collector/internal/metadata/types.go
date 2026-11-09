// Package metadata defines the shared domain types that flow between the
// configuration, collection, and export layers of the db-collector service.
//
// The central value type is [ScheduledProbe], which bundles a
// [DatabaseTarget], a [ProbeDefinition], and a [ProbeAssignment] into a
// single self-contained unit of work that [collectors.ProbeExecutor] can
// execute without additional lookups.
package metadata

import (
	"time"

	collectorconfig "atrium/internal/config"
)

// QueryFilter constrains which scheduled probes are returned by
// [Repository.ListScheduledProbes].
type QueryFilter struct {
	// Engine filters probes to targets of this database engine (e.g. "sqlserver").
	Engine string
	// CollectorID scopes the query to a specific collector.
	CollectorID string
	// EnvironmentSlug, when non-empty, restricts results to targets in this
	// environment.
	EnvironmentSlug string
	// TargetNames, when non-empty, restricts results to targets with these
	// names.
	TargetNames []string
}

// DatabaseTarget holds the connection parameters for a single monitored
// database instance.
type DatabaseTarget struct {
	// ID is the opaque primary-key identifier of the target record.
	ID string
	// EnvironmentSlug categorises the target (e.g. "production", "staging").
	EnvironmentSlug string
	// Name is the human-readable identifier for the target.
	Name string
	// Engine specifies the database technology (e.g. "sqlserver").
	Engine string
	// Host is the network hostname or IP address of the database server.
	Host string
	// Port is the TCP port of the database server.
	Port int
	// DatabaseName is the logical database to connect to.
	DatabaseName string
	// CredentialRef references the secret used to authenticate to this target.
	CredentialRef string
}

// ProbeDefinition describes what a probe measures and how it measures it.
type ProbeDefinition struct {
	// ID is the opaque primary-key identifier of the definition record.
	ID string
	// Name identifies the probe type and maps to a catalog entry.
	Name string
	// Category groups related probes (e.g. "waits", "blocking", "sessions").
	Category string
	// QueryTemplate is the SQL query to execute; an empty value defers to the
	// catalog default.
	QueryTemplate string
	// TimeoutMS is the per-execution deadline in milliseconds; 0 means use the
	// default derived from the scrape interval.
	TimeoutMS int
	// MaxRows is the most rows a SQL Server probe may return; 0 means the
	// default ([config.DefaultProbeMaxRows]).
	MaxRows int
	// Lag, for an outsystems_logs log type, replaces the collector's lag
	// ([ScheduledProbe.OutSystemsLogs]); 0 means the collector's.
	Lag time.Duration
}

// ProbeAssignment links a [ProbeDefinition] to a [DatabaseTarget] and
// configures scheduling parameters.
type ProbeAssignment struct {
	// ID is the opaque primary-key identifier of the assignment record.
	ID string
	// IntervalSeconds is the target polling frequency for this assignment.
	IntervalSeconds int
}

// ScheduledProbe is a self-contained unit of work: it carries everything a
// [collectors.ProbeExecutor] needs to open a connection, execute a query, and
// decode the results.
type ScheduledProbe struct {
	// CollectorID identifies which collector owns this probe execution.
	CollectorID string
	Target      DatabaseTarget
	Definition  ProbeDefinition
	Assignment  ProbeAssignment
	// OutSystemsLogs holds the collector's log extraction settings when its
	// kind is outsystems_logs (Target.Engine), and the zero value otherwise.
	OutSystemsLogs collectorconfig.OutSystemsLogsConfig
}

// Evidence is a structured observation produced by certain probe categories
// (e.g. "blocking", "sessions") to be forwarded to an [collectors.EvidenceSink].
type Evidence struct {
	// Kind mirrors the probe category that produced this evidence.
	Kind string
	// Title is a human-readable summary of the observation.
	Title string
	// Metadata carries additional key-value context (e.g. row counts).
	Metadata map[string]string
}
