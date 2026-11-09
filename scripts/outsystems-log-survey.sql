-- Survey of an OutSystems 11 logging database for Atrium (TODO 3.3).
--
-- Read-only. Run it with a login that has SELECT on the oslog_* objects (the
-- extractor's login is enough) against the logging database, for example:
--   sqlcmd -S <server> -d <logging db> -U <login> -i scripts/outsystems-log-survey.sql -o survey.txt -y 0 -w 4000
-- Each section answers one open question of
-- docs/architecture/application-observability.md#open-questions, and
-- section 2 supplies the DDL of the fixtures under
-- services/db-collector/internal/collectors/outsystems/testdata/oslog/.
-- The output holds real rows (section 3): keep it out of git.
-- Sections 5 to 7 read the current and the previous week's views; on a busy
-- database run them off-peak. Each section is its own batch, so an error in
-- one does not stop the others.

SET NOCOUNT ON;

PRINT '== 1. Clock: which time zone is Instant in? (log_timezone) ==';
-- The newest Instant of each log lies a few seconds behind one of these
-- clocks: GETDATE() means the servers'' local zone, GETUTCDATE() means UTC.
SELECT SYSDATETIMEOFFSET() AS db_now_with_offset, GETDATE() AS db_local_now, GETUTCDATE() AS utc_now;
SELECT 'general' AS log_type, MAX(Instant) AS newest_instant FROM dbo.oslog_General
UNION ALL SELECT 'error', MAX(Instant) FROM dbo.oslog_Error
UNION ALL SELECT 'screen', MAX(Instant) FROM dbo.oslog_Screen
UNION ALL SELECT 'integration', MAX(Instant) FROM dbo.oslog_Integration
UNION ALL SELECT 'extension', MAX(Instant) FROM dbo.oslog_Extension
UNION ALL SELECT 'cyclic_job', MAX(Instant) FROM dbo.oslog_Cyclic_Job
UNION ALL SELECT 'mobile_request', MAX(Instant) FROM dbo.oslog_mobile_request
UNION ALL SELECT 'request_event', MAX(INSTANT) FROM dbo.oslog_RequestEvent;
GO

PRINT '== 2. DDL of the log tables (one cycle table per log type) and views ==';
-- Paste into testdata/oslog/<log type>.sql: the columns of one cycle table
-- (its _0 copy stands for all ten), its indexes, and the two view
-- definitions, each wrapped in EXEC (N'...').
SELECT c.TABLE_NAME, c.ORDINAL_POSITION, c.COLUMN_NAME, c.DATA_TYPE, c.CHARACTER_MAXIMUM_LENGTH, c.NUMERIC_PRECISION, c.NUMERIC_SCALE, c.IS_NULLABLE, c.COLUMN_DEFAULT
FROM INFORMATION_SCHEMA.COLUMNS AS c
WHERE c.TABLE_SCHEMA = 'dbo' AND c.TABLE_NAME LIKE 'oslog[_]%[_]0'
ORDER BY c.TABLE_NAME, c.ORDINAL_POSITION;
SELECT t.name AS table_name, i.name AS index_name, i.type_desc, i.is_primary_key, i.is_unique,
       STRING_AGG(col.name, ', ') WITHIN GROUP (ORDER BY ic.key_ordinal) AS key_columns
FROM sys.tables AS t
JOIN sys.indexes AS i ON i.object_id = t.object_id AND i.index_id > 0
JOIN sys.index_columns AS ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id AND ic.is_included_column = 0
JOIN sys.columns AS col ON col.object_id = ic.object_id AND col.column_id = ic.column_id
WHERE t.name LIKE 'oslog[_]%[_]0'
GROUP BY t.name, i.name, i.type_desc, i.is_primary_key, i.is_unique
ORDER BY t.name, i.name;
SELECT v.name AS view_name, OBJECT_DEFINITION(v.object_id) AS definition
FROM sys.views AS v
WHERE v.name LIKE 'oslog%'
ORDER BY v.name;
GO

PRINT '== 3. Rows for the fixtures: one request across log types, around the last rotation ==';
-- Pick a Request_Key that appears in several log types in the previous
-- week, and rows from the last minutes before and the first minutes after
-- the rotation (Friday 23:45). They show what real rows look like; the
-- fixtures' rows stay synthetic, modelled on them, since the repository is
-- public.
-- ORDER BY cannot add up column aliases, hence the derived table.
SELECT TOP (20) k.*
FROM (
    SELECT e.Request_key AS Request_Key, COUNT(*) AS error_rows,
           (SELECT COUNT(*) FROM dbo.oslog_Screen_Previous AS s WHERE s.Request_Key = e.Request_key) AS screen_rows,
           (SELECT COUNT(*) FROM dbo.oslog_mobile_request_Previous AS m WHERE m.Request_Key = e.Request_key) AS mobile_request_rows,
           (SELECT COUNT(*) FROM dbo.oslog_Integration_Previous AS i WHERE i.Request_Key = e.Request_key) AS integration_rows,
           (SELECT COUNT(*) FROM dbo.oslog_Extension_Previous AS x WHERE x.Request_Key = e.Request_key) AS extension_rows,
           (SELECT COUNT(*) FROM dbo.oslog_General_Previous AS g WHERE g.Request_Key = e.Request_key) AS general_rows
    FROM dbo.oslog_Error_Previous AS e
    WHERE e.Request_key IS NOT NULL AND e.Request_key <> ''
    GROUP BY e.Request_key
) AS k
ORDER BY k.screen_rows + k.mobile_request_rows + k.integration_rows + k.extension_rows + k.general_rows DESC;
SELECT TOP (5) * FROM dbo.oslog_Error_Previous ORDER BY Instant DESC;
SELECT TOP (5) * FROM dbo.oslog_Error ORDER BY Instant ASC;
GO

PRINT '== 4. Retention and rotation: cycle tables in use, rows per table ==';
SELECT t.name AS table_name, SUM(p.rows) AS row_count
FROM sys.tables AS t
JOIN sys.partitions AS p ON p.object_id = t.object_id AND p.index_id IN (0, 1)
WHERE t.name LIKE 'oslog%'
GROUP BY t.name
ORDER BY t.name;
SELECT TOP (1) Cycle AS current_cycle FROM dbo.oslog_General;
GO

PRINT '== 5. Duration units: seconds or milliseconds per log type? ==';
-- Compare magnitudes: a log that stores seconds shows small integers and
-- many zeros; milliseconds show hundreds and thousands.
SELECT 'screen' AS log_type, COUNT(*) AS rows_7d, MIN(Duration) AS min_duration, AVG(CAST(Duration AS float)) AS avg_duration, MAX(Duration) AS max_duration,
       SUM(CASE WHEN Duration = 0 THEN 1 ELSE 0 END) AS zero_durations
FROM (SELECT Duration FROM dbo.oslog_Screen UNION ALL SELECT Duration FROM dbo.oslog_Screen_Previous) AS d
UNION ALL SELECT 'integration', COUNT(*), MIN(Duration), AVG(CAST(Duration AS float)), MAX(Duration), SUM(CASE WHEN Duration = 0 THEN 1 ELSE 0 END)
FROM (SELECT Duration FROM dbo.oslog_Integration UNION ALL SELECT Duration FROM dbo.oslog_Integration_Previous) AS d
UNION ALL SELECT 'extension', COUNT(*), MIN(Duration), AVG(CAST(Duration AS float)), MAX(Duration), SUM(CASE WHEN Duration = 0 THEN 1 ELSE 0 END)
FROM (SELECT Duration FROM dbo.oslog_Extension UNION ALL SELECT Duration FROM dbo.oslog_Extension_Previous) AS d
UNION ALL SELECT 'cyclic_job', COUNT(*), MIN(Duration), AVG(CAST(Duration AS float)), MAX(Duration), SUM(CASE WHEN Duration = 0 THEN 1 ELSE 0 END)
FROM (SELECT Duration FROM dbo.oslog_Cyclic_Job UNION ALL SELECT Duration FROM dbo.oslog_Cyclic_Job_Previous) AS d
UNION ALL SELECT 'mobile_request', COUNT(*), MIN(Duration), AVG(CAST(Duration AS float)), MAX(Duration), SUM(CASE WHEN Duration = 0 THEN 1 ELSE 0 END)
FROM (SELECT Duration FROM dbo.oslog_mobile_request UNION ALL SELECT Duration FROM dbo.oslog_mobile_request_Previous) AS d;
-- The longest screen request and timer set the screen and cyclic_job lags
-- (probes[].lag): the lag must exceed them.
GO

PRINT '== 6. Rows per minute at peak, per log type, over the current and previous week (batch_limit, Loki sizing) ==';
SELECT TOP (5) 'general' AS log_type, DATEADD(MINUTE, DATEDIFF(MINUTE, 0, Instant), 0) AS minute, COUNT(*) AS rows_in_minute
FROM (SELECT Instant FROM dbo.oslog_General UNION ALL SELECT Instant FROM dbo.oslog_General_Previous) AS r
GROUP BY DATEADD(MINUTE, DATEDIFF(MINUTE, 0, Instant), 0) ORDER BY rows_in_minute DESC;
SELECT TOP (5) 'error' AS log_type, DATEADD(MINUTE, DATEDIFF(MINUTE, 0, Instant), 0) AS minute, COUNT(*) AS rows_in_minute
FROM (SELECT Instant FROM dbo.oslog_Error UNION ALL SELECT Instant FROM dbo.oslog_Error_Previous) AS r
GROUP BY DATEADD(MINUTE, DATEDIFF(MINUTE, 0, Instant), 0) ORDER BY rows_in_minute DESC;
SELECT TOP (5) 'screen' AS log_type, DATEADD(MINUTE, DATEDIFF(MINUTE, 0, Instant), 0) AS minute, COUNT(*) AS rows_in_minute
FROM (SELECT Instant FROM dbo.oslog_Screen UNION ALL SELECT Instant FROM dbo.oslog_Screen_Previous) AS r
GROUP BY DATEADD(MINUTE, DATEDIFF(MINUTE, 0, Instant), 0) ORDER BY rows_in_minute DESC;
SELECT TOP (5) 'integration' AS log_type, DATEADD(MINUTE, DATEDIFF(MINUTE, 0, Instant), 0) AS minute, COUNT(*) AS rows_in_minute
FROM (SELECT Instant FROM dbo.oslog_Integration UNION ALL SELECT Instant FROM dbo.oslog_Integration_Previous) AS r
GROUP BY DATEADD(MINUTE, DATEDIFF(MINUTE, 0, Instant), 0) ORDER BY rows_in_minute DESC;
SELECT TOP (5) 'extension' AS log_type, DATEADD(MINUTE, DATEDIFF(MINUTE, 0, Instant), 0) AS minute, COUNT(*) AS rows_in_minute
FROM (SELECT Instant FROM dbo.oslog_Extension UNION ALL SELECT Instant FROM dbo.oslog_Extension_Previous) AS r
GROUP BY DATEADD(MINUTE, DATEDIFF(MINUTE, 0, Instant), 0) ORDER BY rows_in_minute DESC;
SELECT TOP (5) 'cyclic_job' AS log_type, DATEADD(MINUTE, DATEDIFF(MINUTE, 0, Instant), 0) AS minute, COUNT(*) AS rows_in_minute
FROM (SELECT Instant FROM dbo.oslog_Cyclic_Job UNION ALL SELECT Instant FROM dbo.oslog_Cyclic_Job_Previous) AS r
GROUP BY DATEADD(MINUTE, DATEDIFF(MINUTE, 0, Instant), 0) ORDER BY rows_in_minute DESC;
SELECT TOP (5) 'mobile_request' AS log_type, DATEADD(MINUTE, DATEDIFF(MINUTE, 0, Instant), 0) AS minute, COUNT(*) AS rows_in_minute
FROM (SELECT Instant FROM dbo.oslog_mobile_request UNION ALL SELECT Instant FROM dbo.oslog_mobile_request_Previous) AS r
GROUP BY DATEADD(MINUTE, DATEDIFF(MINUTE, 0, Instant), 0) ORDER BY rows_in_minute DESC;
SELECT TOP (5) 'request_event' AS log_type, DATEADD(MINUTE, DATEDIFF(MINUTE, 0, INSTANT), 0) AS minute, COUNT(*) AS rows_in_minute
FROM (SELECT INSTANT FROM dbo.oslog_RequestEvent UNION ALL SELECT INSTANT FROM dbo.oslog_RequestEvent_Previous) AS r
GROUP BY DATEADD(MINUTE, DATEDIFF(MINUTE, 0, INSTANT), 0) ORDER BY rows_in_minute DESC;
GO

PRINT '== 7. Entry-point coverage: requests seen in other logs without a screen or mobile request row (24 h before the newest row) ==';
-- A request that logged an error, integration or extension but no screen or
-- mobile request row has no root span in the request trace design. The
-- window ends at the newest general row, not now, so that a stale copy of
-- the database still answers.
DECLARE @since datetime = (SELECT DATEADD(HOUR, -24, MAX(Instant)) FROM dbo.oslog_General);
WITH entry_points AS (
    SELECT Request_Key FROM dbo.oslog_Screen WHERE Instant > @since
    UNION SELECT Request_Key FROM dbo.oslog_mobile_request WHERE Instant > @since
    UNION SELECT Request_Key FROM dbo.oslog_Cyclic_Job WHERE Instant > @since
), other AS (
    SELECT 'error' AS log_type, Request_key AS Request_Key FROM dbo.oslog_Error WHERE Instant > @since
    UNION ALL SELECT 'integration', Request_Key FROM dbo.oslog_Integration WHERE Instant > @since
    UNION ALL SELECT 'extension', Request_Key FROM dbo.oslog_Extension WHERE Instant > @since
    UNION ALL SELECT 'general', Request_Key FROM dbo.oslog_General WHERE Instant > @since
)
SELECT o.log_type,
       COUNT(DISTINCT o.Request_Key) AS requests,
       COUNT(DISTINCT CASE WHEN o.Request_Key IS NULL OR o.Request_Key = '' THEN NULL WHEN ep.Request_Key IS NULL THEN o.Request_Key END) AS without_entry_point,
       SUM(CASE WHEN o.Request_Key IS NULL OR o.Request_Key = '' THEN 1 ELSE 0 END) AS rows_without_key
FROM other AS o
LEFT JOIN entry_points AS ep ON ep.Request_Key = o.Request_Key
GROUP BY o.log_type
ORDER BY o.log_type;
GO
