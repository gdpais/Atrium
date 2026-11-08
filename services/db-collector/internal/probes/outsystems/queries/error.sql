-- The error log: one row per logged exception, written from the OutSystems 11 Log data reference
-- (view oslog_Error); column names and types checked against the logging-database
-- survey [3.3].
--
-- oslog_Error is a view over the current week's cycle table and
-- oslog_Error_Previous over last week's; the platform switches them every
-- Friday at 23:45 (database time). Reading both keeps the rows logged just
-- before a rotation and lets initial_lookback reach back a week.
-- Columns that differ between the log views are returned under the standard
-- names of README.md (Standard columns).
-- Entrypoint_Name is the entry point (standard name Screen). Module_Name is
-- the free text the error was logged with, Espace_Name the module.
-- Stack is the error detail's FullStackTrace, a varchar(max) that a monitored
-- application's users can make arbitrarily long: it is cut here to one
-- character more than the 16 KiB a record keeps, so the driver never holds a
-- whole one and the collector still marks the record outsystems.truncated.
SELECT TOP (@limit)
    r.Instant,
    r.Message,
    'ERROR' AS Severity,
    r.Application_Name,
    r.Espace_Name,
    r.Module_Name,
    r.Entrypoint_Name AS Screen,
    r.Action_Name,
    r.Request_Key,
    r.Session_Id,
    r.User_Id,
    r.Username,
    r.Tenant_Id,
    r.Server,
    LEFT(r.Stack, 16385) AS Stack,
    r.EnvironmentInformation
-- Each week's view is cut to @limit rows in Instant order before the
-- union, so a backlog costs @limit index rows per view instead of every row
-- in (@from, @to]: the first @limit rows overall are among them.
FROM (
    SELECT * FROM (
        SELECT TOP (@limit) * FROM dbo.oslog_Error
        WHERE Instant > @from AND Instant <= @to
        ORDER BY Instant
    ) AS current_week
    UNION ALL
    SELECT * FROM (
        SELECT TOP (@limit) * FROM dbo.oslog_Error_Previous
        WHERE Instant > @from AND Instant <= @to
        ORDER BY Instant
    ) AS previous_week
) AS r
ORDER BY r.Instant;
