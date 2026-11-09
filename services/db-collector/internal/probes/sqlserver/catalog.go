// Package sqlserver defines the built-in SQL Server probe catalog used by the
// db-collector service.
//
// A [Catalog] maps probe names to [Probe] definitions.  Each [Probe] bundles
// a default SQL query template with the set of [Metric] descriptors that
// describe how to decode the result set into Prometheus samples.
//
// The [DefaultCatalog] function returns a pre-populated catalog with the
// following built-in probes:
//
//   - waits          – cumulative wait time from sys.dm_os_wait_stats (counter)
//   - blocking       – current blocked-request count from sys.dm_exec_requests
//   - sessions       – session counts by status from sys.dm_exec_sessions
//   - memory_pressure – total server memory from sys.dm_os_performance_counters
//   - storage        – database file sizes from sys.master_files
//   - throughput     – cumulative batch requests and transactions from
//     sys.dm_os_performance_counters (counters)
//   - cpu            – SQL Server and other-process CPU utilisation from the
//     scheduler monitor ring buffer in sys.dm_os_ring_buffers (ratios)
//   - buffer_cache   – page life expectancy and buffer cache hit ratio from
//     sys.dm_os_performance_counters
//   - file_io        – cumulative reads, writes, bytes and I/O stall time per
//     database file from sys.dm_io_virtual_file_stats (counters)
//
// Metric names follow Prometheus conventions: base units (seconds, bytes),
// and a _total suffix on counters, which only counters carry.
package sqlserver

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	atriumconfig "atrium/internal/config"
	collectorexport "atrium/services/db-collector/internal/export"
)

// Probe describes a named SQL Server probe: the SQL to execute and the metrics
// to extract from the result set.
type Probe struct {
	// Name is the unique key used to look up this probe in a [Catalog].
	Name string
	// Category groups related probes and determines whether evidence records
	// are produced (e.g. "blocking", "sessions").
	Category string
	// QueryTemplate is the default SQL query.  Individual collector
	// configurations may override this per probe; an override must return
	// the same columns, in the same units.
	QueryTemplate string
	// Metrics lists the descriptors that decode columns from the query result
	// set into labelled Prometheus samples.  Every row is decoded by every
	// descriptor, so one row can carry several metrics in separate columns.
	Metrics []Metric
}

// Metric describes how to extract a single Prometheus metric from one column
// of a probe's SQL result set.
type Metric struct {
	// Name is the fully-qualified Prometheus metric name
	// (e.g. "atrium_sqlserver_sessions").
	Name string
	// Help is the human-readable description registered with Prometheus.
	Help string
	// Type is how the metric is exposed.  The zero value is a gauge.  Use
	// [collectorexport.Counter] for values SQL Server accumulates since
	// startup (DMV totals, cumulative performance counters such as cntr_type
	// 272696576): they are exported as SQL Server reports them, and rate() or
	// increase() handle the reset when SQL Server restarts or its statistics
	// are cleared.  Ratios belong in the query and are gauges.
	Type collectorexport.MetricType
	// ValueColumn is the result-set column whose value becomes the sample
	// value.  Rows where the column is missing or NULL produce no sample.
	ValueColumn string
	// Scale multiplies the column value to convert it to the metric's base
	// unit, e.g. 0.001 for milliseconds to seconds or 1024 for KB to bytes.
	// Zero means 1.
	Scale float64
	// LabelColumns are additional result-set columns whose values become
	// Prometheus label values (column name becomes the label key).  Choose
	// columns with a bounded set of values: every distinct value is a series.
	LabelColumns []string
}

// Convert returns raw in the metric's base unit by applying [Metric.Scale].
func (m Metric) Convert(raw float64) float64 {
	if m.Scale == 0 {
		return raw
	}
	return raw * m.Scale
}

// benignWaitTypes are background and idle waits that say nothing about
// workload performance: system tasks sleeping, queues waiting for work, and
// timers.  The list follows the exclusion list of Paul Randal's widely used
// wait statistics query (SQLskills, "Wait statistics, or please tell me where
// it hurts"), which tracks new SQL Server versions; update it from there.
var benignWaitTypes = []string{
	"BROKER_EVENTHANDLER", "BROKER_RECEIVE_WAITFOR", "BROKER_TASK_STOP",
	"BROKER_TO_FLUSH", "BROKER_TRANSMITTER", "CHECKPOINT_QUEUE", "CHKPT",
	"CLR_AUTO_EVENT", "CLR_MANUAL_EVENT", "CLR_SEMAPHORE", "CXCONSUMER",
	"DBMIRROR_DBM_EVENT", "DBMIRROR_EVENTS_QUEUE", "DBMIRROR_WORKER_QUEUE",
	"DBMIRRORING_CMD", "DIRTY_PAGE_POLL", "DISPATCHER_QUEUE_SEMAPHORE",
	"EXECSYNC", "FSAGENT", "FT_IFTS_SCHEDULER_IDLE_WAIT", "FT_IFTSHC_MUTEX",
	"HADR_CLUSAPI_CALL", "HADR_FILESTREAM_IOMGR_IOCOMPLETION",
	"HADR_LOGCAPTURE_WAIT", "HADR_NOTIFICATION_DEQUEUE", "HADR_TIMER_TASK",
	"HADR_WORK_QUEUE", "KSOURCE_WAKEUP", "LAZYWRITER_SLEEP", "LOGMGR_QUEUE",
	"MEMORY_ALLOCATION_EXT", "ONDEMAND_TASK_QUEUE",
	"PARALLEL_REDO_DRAIN_WORKER", "PARALLEL_REDO_LOG_CACHE",
	"PARALLEL_REDO_TRAN_LIST", "PARALLEL_REDO_WORKER_SYNC",
	"PARALLEL_REDO_WORKER_WAIT_WORK", "PREEMPTIVE_OS_FLUSHFILEBUFFERS",
	"PREEMPTIVE_XE_GETTARGETSTATE", "PVS_PREALLOCATE",
	"PWAIT_ALL_COMPONENTS_INITIALIZED", "PWAIT_DIRECTLOGCONSUMER_GETNEXT",
	"PWAIT_EXTENSIBILITY_CLEANUP_TASK", "QDS_PERSIST_TASK_MAIN_LOOP_SLEEP",
	"QDS_ASYNC_QUEUE", "QDS_CLEANUP_STALE_QUERIES_TASK_MAIN_LOOP_SLEEP",
	"QDS_SHUTDOWN_QUEUE", "REDO_THREAD_PENDING_WORK",
	"REQUEST_FOR_DEADLOCK_SEARCH", "RESOURCE_QUEUE", "SERVER_IDLE_CHECK",
	"SLEEP_BPOOL_FLUSH", "SLEEP_DBSTARTUP", "SLEEP_DCOMSTARTUP",
	"SLEEP_MASTERDBREADY", "SLEEP_MASTERMDREADY", "SLEEP_MASTERUPGRADED",
	"SLEEP_MSDBSTARTUP", "SLEEP_SYSTEMTASK", "SLEEP_TASK",
	"SLEEP_TEMPDBSTARTUP", "SNI_HTTP_ACCEPT", "SOS_WORK_DISPATCHER",
	"SP_SERVER_DIAGNOSTICS_SLEEP", "SQLTRACE_BUFFER_FLUSH",
	"SQLTRACE_INCREMENTAL_FLUSH_SLEEP", "SQLTRACE_WAIT_ENTRIES",
	"VDI_CLIENT_OTHER", "WAIT_FOR_RESULTS", "WAITFOR", "WAITFOR_TASKSHUTDOWN",
	"WAIT_XTP_RECOVERY", "WAIT_XTP_HOST_WAIT", "WAIT_XTP_OFFLINE_CKPT_NEW_LOG",
	"WAIT_XTP_CKPT_CLOSE", "XE_DISPATCHER_JOIN", "XE_DISPATCHER_WAIT",
	"XE_TIMER_EVENT",
}

// sqlStringList renders values as a comma-separated list of N'...' literals
// for an IN clause.  Values are compile-time constants; quotes are doubled
// anyway so the rendering is always valid SQL.
func sqlStringList(values []string) string {
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = "N'" + strings.ReplaceAll(value, "'", "''") + "'"
	}
	return strings.Join(quoted, ", ")
}

// fileIOColumn is one counter of the file_io probe: metric name, help, value
// column and unit scale.
type fileIOColumn struct {
	name, help, column string
	scale              float64
}

// fileIOMetrics returns counter descriptors labelled like the storage probe,
// one per column of the file_io result set.
func fileIOMetrics(columns []fileIOColumn) []Metric {
	metrics := make([]Metric, len(columns))
	for i, c := range columns {
		metrics[i] = Metric{
			Name:         c.name,
			Help:         c.help,
			Type:         collectorexport.Counter,
			ValueColumn:  c.column,
			Scale:        c.scale,
			LabelColumns: []string{"database_name", "file_name", "file_type"},
		}
	}
	return metrics
}

// Catalog is an immutable, name-indexed collection of [Probe] definitions.
type Catalog struct {
	byName map[string]Probe
}

// DefaultCatalog returns a [Catalog] pre-populated with all built-in SQL
// Server probes.  See the package documentation for the full list.
func DefaultCatalog() Catalog {
	probes := []Probe{
		{
			Name:     "waits",
			Category: "waits",
			// Benign idle waits (see benignWaitTypes) are excluded: they grow
			// by about a second per second on an idle server and would
			// dominate top-N wait panels.  A read of the DMV takes no locks.
			QueryTemplate: "SELECT wait_type, wait_time_ms FROM sys.dm_os_wait_stats WHERE wait_time_ms > 0 AND wait_type NOT IN (" + sqlStringList(benignWaitTypes) + ")",
			Metrics: []Metric{{
				Name:         "atrium_sqlserver_wait_seconds_total",
				Help:         "Cumulative SQL Server wait time in seconds by wait type since SQL Server started or wait statistics were cleared.",
				Type:         collectorexport.Counter,
				ValueColumn:  "wait_time_ms",
				Scale:        0.001,
				LabelColumns: []string{"wait_type"},
			}},
		},
		{
			Name:          "blocking",
			Category:      "blocking",
			QueryTemplate: "SELECT blocking_session_id, COUNT(*) AS blocked_count FROM sys.dm_exec_requests WHERE blocking_session_id <> 0 GROUP BY blocking_session_id",
			Metrics: []Metric{{
				Name:         "atrium_sqlserver_blocked_requests",
				Help:         "Current blocked SQL Server request count by blocking session.",
				ValueColumn:  "blocked_count",
				LabelColumns: []string{"blocking_session_id"},
			}},
		},
		{
			Name:          "sessions",
			Category:      "sessions",
			QueryTemplate: "SELECT status, COUNT(*) AS session_count FROM sys.dm_exec_sessions GROUP BY status",
			Metrics: []Metric{{
				Name:         "atrium_sqlserver_sessions",
				Help:         "Current SQL Server session count by status.",
				ValueColumn:  "session_count",
				LabelColumns: []string{"status"},
			}},
		},
		{
			Name:          "memory_pressure",
			Category:      "memory_pressure",
			QueryTemplate: "SELECT cntr_value AS total_server_memory_kb FROM sys.dm_os_performance_counters WHERE counter_name = 'Total Server Memory (KB)'",
			Metrics: []Metric{{
				Name:        "atrium_sqlserver_total_server_memory_bytes",
				Help:        "Memory SQL Server has committed (Total Server Memory) in bytes.",
				ValueColumn: "total_server_memory_kb",
				Scale:       1024,
			}},
		},
		{
			Name:     "storage",
			Category: "storage",
			// One row per database file: file_name is the logical name, unique
			// within a database, so every file gets its own series.  size is
			// in 8 KB pages.  DB_NAME() is NULL for a database the login
			// cannot see (no VIEW ANY DATABASE) or one dropped mid-query;
			// the database_id fallback keeps the label set and keeps two such
			// databases with the same logical file names (restored copies)
			// apart.  file_io uses the same expression.
			QueryTemplate: "SELECT COALESCE(DB_NAME(database_id), CONCAT(N'database_id:', database_id)) AS database_name, name AS file_name, type_desc AS file_type, size AS size_pages FROM sys.master_files",
			Metrics: []Metric{{
				Name:         "atrium_sqlserver_database_file_size_bytes",
				Help:         "SQL Server database file size in bytes, one series per file.",
				ValueColumn:  "size_pages",
				Scale:        8192,
				LabelColumns: []string{"database_name", "file_name", "file_type"},
			}},
		},
		{
			Name:     "throughput",
			Category: "throughput",
			// Both counters are cumulative despite their /sec names (cntr_type
			// 272696576), so they are exported as counters and read with
			// rate().  The query pivots them into one row with one column per
			// metric.  Transactions/sec has one row per database plus _Total;
			// only the server-wide _Total is kept.  MAX over no rows is NULL,
			// so a missing counter exports no series rather than 0.
			QueryTemplate: "SELECT " +
				"MAX(CASE WHEN counter_name = 'Batch Requests/sec' THEN cntr_value END) AS batch_requests, " +
				"MAX(CASE WHEN counter_name = 'Transactions/sec' AND instance_name = '_Total' THEN cntr_value END) AS transactions " +
				"FROM sys.dm_os_performance_counters WHERE counter_name IN ('Batch Requests/sec', 'Transactions/sec')",
			Metrics: []Metric{
				{
					Name:        "atrium_sqlserver_batch_requests_total",
					Help:        "Cumulative SQL Server batch requests (Batch Requests/sec counter) since SQL Server started.",
					Type:        collectorexport.Counter,
					ValueColumn: "batch_requests",
				},
				{
					Name:        "atrium_sqlserver_transactions_total",
					Help:        "Cumulative SQL Server transactions across all databases (Transactions/sec counter, _Total) since SQL Server started.",
					Type:        collectorexport.Counter,
					ValueColumn: "transactions",
				},
			},
		},
		{
			Name:     "cpu",
			Category: "cpu",
			// The scheduler monitor writes one SystemHealth record a minute
			// with the CPU split over the last minute, in percent; the ring
			// buffer keeps the last 256 (about four hours).  Only the newest
			// record is converted to XML: TOP (1) picks the raw row before
			// the CROSS APPLY casts it.  Other processes are what is neither
			// SQL Server nor idle, floored at 0 because the two values are
			// sampled separately and can add up to more than 100.  SQL Server
			// on Linux reports SystemIdle as 0 whatever the load, so other
			// processes are NULL (no sample) there, as they are when either
			// value is missing from the record.  The platform comes from
			// @@VERSION ("... on Windows Server 2019 ..." or "... on Linux
			// (Ubuntu ...)"), which needs no permission and, unlike
			// sys.dm_os_host_info (SQL Server 2017+), exists on every
			// version.  The XML value() method needs QUOTED_IDENTIFIER ON,
			// which the driver's ODBC login options set.  Before the first
			// record, about a minute after startup, the query returns no row
			// and the probe exports nothing.  It also returns no row when the
			// newest record is 3 minutes old or more (its timestamp and
			// sys.dm_os_sys_info.ms_ticks count milliseconds since startup),
			// so a ring buffer that stops being written cannot re-export an
			// old value forever.
			QueryTemplate: "SELECT v.sql_process_percent, " +
				"CASE WHEN @@VERSION LIKE N'% on Windows%' " +
				"AND v.system_idle_percent IS NOT NULL AND v.sql_process_percent IS NOT NULL THEN " +
				"CASE WHEN 100 - v.system_idle_percent - v.sql_process_percent > 0 " +
				"THEN 100 - v.system_idle_percent - v.sql_process_percent ELSE 0 END END AS other_process_percent " +
				"FROM (SELECT TOP (1) timestamp, record FROM sys.dm_os_ring_buffers " +
				"WHERE ring_buffer_type = N'RING_BUFFER_SCHEDULER_MONITOR' AND record LIKE N'%<SystemHealth>%' " +
				"ORDER BY timestamp DESC) AS latest " +
				"CROSS JOIN sys.dm_os_sys_info AS si " +
				"CROSS APPLY (SELECT CONVERT(xml, latest.record) AS doc) AS r " +
				"CROSS APPLY (SELECT " +
				"r.doc.value('(/Record/SchedulerMonitorEvent/SystemHealth/ProcessUtilization)[1]', 'int') AS sql_process_percent, " +
				"r.doc.value('(/Record/SchedulerMonitorEvent/SystemHealth/SystemIdle)[1]', 'int') AS system_idle_percent) AS v " +
				"WHERE si.ms_ticks - latest.timestamp < 180000",
			Metrics: []Metric{
				{
					Name:        "atrium_sqlserver_cpu_sql_process_ratio",
					Help:        "Share of the host's CPU used by the SQL Server process over the last scheduler monitor minute (0-1).",
					ValueColumn: "sql_process_percent",
					Scale:       0.01,
				},
				{
					Name:        "atrium_sqlserver_cpu_other_process_ratio",
					Help:        "Share of the host's CPU used by processes other than SQL Server over the last scheduler monitor minute (0-1). Windows only.",
					ValueColumn: "other_process_percent",
					Scale:       0.01,
				},
			},
		},
		{
			Name:     "buffer_cache",
			Category: "buffer_cache",
			// Buffer Manager counters.  object_name is SQLServer:Buffer Manager
			// on a default instance and MSSQL$NAME:Buffer Manager on a named
			// one, padded with spaces (nchar), hence RTRIM and the suffix
			// match; Buffer Node (per NUMA node) does not match.  The hit
			// ratio is a raw fraction over its base counter, divided here;
			// NULLIF turns a zero base into NULL, which exports no sample.
			QueryTemplate: "SELECT " +
				"MAX(CASE WHEN counter_name = N'Page life expectancy' THEN cntr_value END) AS page_life_expectancy_seconds, " +
				"CAST(MAX(CASE WHEN counter_name = N'Buffer cache hit ratio' THEN cntr_value END) AS float) " +
				"/ NULLIF(MAX(CASE WHEN counter_name = N'Buffer cache hit ratio base' THEN cntr_value END), 0) AS buffer_cache_hit_ratio " +
				"FROM sys.dm_os_performance_counters " +
				"WHERE RTRIM(object_name) LIKE N'%:Buffer Manager' " +
				"AND counter_name IN (N'Page life expectancy', N'Buffer cache hit ratio', N'Buffer cache hit ratio base')",
			Metrics: []Metric{
				{
					Name:        "atrium_sqlserver_page_life_expectancy_seconds",
					Help:        "Seconds a page is expected to stay in the SQL Server buffer pool without references (Buffer Manager Page life expectancy).",
					ValueColumn: "page_life_expectancy_seconds",
				},
				{
					Name:        "atrium_sqlserver_buffer_cache_hit_ratio",
					Help:        "Share of page requests served from the SQL Server buffer pool without a disk read (Buffer Manager Buffer cache hit ratio, 0-1).",
					ValueColumn: "buffer_cache_hit_ratio",
				},
			},
		},
		{
			Name:     "file_io",
			Category: "file_io",
			// One row per database file, with the storage probe's labels.
			// The values accumulate from the moment the database came online
			// (SQL Server start, or the database being brought online), so
			// they are counters.  The join to sys.master_files names the file
			// and drops the hidden resource database, which has no row there.
			// database_name falls back to the id like the storage probe's.
			QueryTemplate: "SELECT COALESCE(DB_NAME(vfs.database_id), CONCAT(N'database_id:', vfs.database_id)) AS database_name, mf.name AS file_name, mf.type_desc AS file_type, " +
				"vfs.num_of_reads, vfs.num_of_writes, vfs.num_of_bytes_read, vfs.num_of_bytes_written, " +
				"vfs.io_stall_read_ms, vfs.io_stall_write_ms " +
				"FROM sys.dm_io_virtual_file_stats(NULL, NULL) AS vfs " +
				"JOIN sys.master_files AS mf ON mf.database_id = vfs.database_id AND mf.file_id = vfs.file_id",
			Metrics: fileIOMetrics([]fileIOColumn{
				{"atrium_sqlserver_database_file_reads_total", "Cumulative read operations on a SQL Server database file.", "num_of_reads", 0},
				{"atrium_sqlserver_database_file_writes_total", "Cumulative write operations on a SQL Server database file.", "num_of_writes", 0},
				{"atrium_sqlserver_database_file_read_bytes_total", "Cumulative bytes read from a SQL Server database file.", "num_of_bytes_read", 0},
				{"atrium_sqlserver_database_file_written_bytes_total", "Cumulative bytes written to a SQL Server database file.", "num_of_bytes_written", 0},
				{"atrium_sqlserver_database_file_read_stall_seconds_total", "Cumulative seconds SQL Server waited for reads from a database file to complete.", "io_stall_read_ms", 0.001},
				{"atrium_sqlserver_database_file_write_stall_seconds_total", "Cumulative seconds SQL Server waited for writes to a database file to complete.", "io_stall_write_ms", 0.001},
			}),
		},
	}
	catalog := Catalog{byName: map[string]Probe{}}
	for _, probe := range probes {
		catalog.byName[probe.Name] = probe
	}
	return catalog
}

// Get returns the [Probe] registered under name and true, or the zero value
// and false if no probe with that name exists.
func (c Catalog) Get(name string) (Probe, bool) {
	probe, ok := c.byName[name]
	return probe, ok
}

// Names returns the names of every probe in the catalog, sorted.
func (c Catalog) Names() []string {
	return slices.Sorted(maps.Keys(c.byName))
}

// Validate rejects a sqlserver collector, enabled or not, that names a probe
// the catalog does not have, in its default probes or a target's own.  A
// query_template override replaces the query of a probe; it cannot add one,
// because the catalog also holds the probe's metric descriptors.  The
// db-collector registers it with [atriumconfig.WithValidator], so a typo
// is a rejected config instead of a target that fails every cycle.
func (c Catalog) Validate(cfg atriumconfig.RuntimeConfig) error {
	for _, collector := range cfg.Collectors {
		if collector.Kind != atriumconfig.KindSQLServer {
			continue
		}
		if err := c.validateProbes("collector "+collector.ID, collector.Probes); err != nil {
			return err
		}
		for _, target := range collector.Targets {
			if err := c.validateProbes(fmt.Sprintf("collector %s target %s", collector.ID, target.Name), target.Probes); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateProbes checks the probes of one scope against the catalog.
func (c Catalog) validateProbes(scope string, probes []atriumconfig.ProbeRuntimeConfig) error {
	for _, probe := range probes {
		if _, ok := c.Get(probe.Name); !ok {
			return fmt.Errorf("%s: unknown SQL Server probe %q; probes: %s", scope, probe.Name, strings.Join(c.Names(), ", "))
		}
	}
	return nil
}
