-- Investigation queries for an OutSystems 11 logging database: what the SRE
-- team runs by hand against the oslog_* views, with the same vocabulary as
-- Atrium's extracted logs (standard columns of
-- services/db-collector/internal/probes/outsystems/queries/README.md).
--
-- Read-only; run in the logging database with a login that has SELECT on
-- the oslog_* views. Set the parameters, leave a filter NULL to skip it, then
-- run one section at a time (each ends with GO).
--
-- The oslog_<TYPE> views are the current week only (rotation Friday 23:45,
-- database time). For last week use oslog_<TYPE>_Previous, for older weeks
-- the cycle table oslog_<TYPE>_<n>; the detail tables use the same cycle
-- number (oslog_Error_Detail_<n> goes with oslog_Error_<n>).
-- NOLOCK: these views are written in bulk by the log service; dirty reads
-- are acceptable for an investigation and never block it. FORCESCAN is not
-- used: every query filters on Instant, which the cycle tables cluster on,
-- and a forced scan would read the whole week.

DECLARE @from        datetime      = '2026-08-24 10:10:00';  -- in the servers' time zone (see log_timezone)
DECLARE @to          datetime      = '2026-08-24 12:30:00';
DECLARE @client_ip   nvarchar(50)  = NULL;                   -- e.g. '10.232.19.133' (substring match)
DECLARE @username    nvarchar(100) = NULL;                   -- e.g. 'fm\userx'
DECLARE @espace      nvarchar(50)  = NULL;                   -- LIKE pattern, e.g. 'EF[_]%' ('_' alone matches any character)
DECLARE @screen      nvarchar(100) = NULL;
DECLARE @action      nvarchar(100) = NULL;                   -- screen action, server action, endpoint or timer
DECLARE @session_id  nvarchar(50)  = NULL;
DECLARE @request_key char(36)      = NULL;
DECLARE @server      nvarchar(50)  = NULL;                   -- front-end server, e.g. 'FE-01'
DECLARE @min_ms      int           = NULL;                   -- e.g. 3000: only requests slower than this

-- ---------------------------------------------------------------------------
-- 1. Requests: Traditional screen requests and Reactive/Mobile requests in
--    the window, one row each, with the common filters.
-- ---------------------------------------------------------------------------
SELECT 'screen' AS log_type, CONVERT(varchar(23), s.Instant, 121) AS instant,
       s.Application_Name, s.Espace_Name, s.Screen, s.Action_Name, s.Request_Key,
       s.Duration AS duration_ms, s.Username, s.User_Id, s.Executed_By AS server, s.Client_IP, s.Session_Id
FROM dbo.oslog_Screen AS s WITH (NOLOCK)
WHERE s.Instant BETWEEN @from AND @to
  AND (@client_ip  IS NULL OR s.Client_IP LIKE '%' + @client_ip + '%')
  AND (@username   IS NULL OR s.Username = @username)
  AND (@espace     IS NULL OR s.Espace_Name LIKE @espace)
  AND (@screen     IS NULL OR s.Screen = @screen)
  AND (@action     IS NULL OR s.Action_Name = @action)
  AND (@session_id IS NULL OR s.Session_Id = @session_id)
  AND (@server     IS NULL OR s.Executed_By = @server)
  AND (@min_ms     IS NULL OR s.Duration >= @min_ms)
UNION ALL
SELECT 'mobile_request', CONVERT(varchar(23), m.Instant, 121),
       m.Application_Name, m.Espace_Name, m.Screen, m.Endpoint, m.Request_Key,
       m.Duration, m.Username, m.User_Id, m.Executed_By, m.Source, m.Login_Id
FROM dbo.oslog_mobile_request AS m WITH (NOLOCK)
WHERE m.Instant BETWEEN @from AND @to
  AND (@client_ip  IS NULL OR m.Source LIKE '%' + @client_ip + '%')
  AND (@username   IS NULL OR m.Username = @username)
  AND (@espace     IS NULL OR m.Espace_Name LIKE @espace)
  AND (@screen     IS NULL OR m.Screen = @screen)
  AND (@action     IS NULL OR m.Endpoint = @action)
  AND (@session_id IS NULL OR m.Login_Id = @session_id)
  AND (@server     IS NULL OR m.Executed_By = @server)
  AND (@min_ms     IS NULL OR m.Duration >= @min_ms)
ORDER BY instant;
GO

-- ---------------------------------------------------------------------------
-- 2. Request timeline: every request in the window (filtered as in 1) with
--    what it called: integrations, extensions, slow queries and errors, one
--    row per call. Traditional and Reactive requests both appear.
-- ---------------------------------------------------------------------------
DECLARE @from        datetime      = '2026-08-24 10:10:00';
DECLARE @to          datetime      = '2026-08-24 12:30:00';
DECLARE @client_ip   nvarchar(50)  = NULL;
DECLARE @username    nvarchar(100) = NULL;
DECLARE @espace      nvarchar(50)  = NULL;
DECLARE @screen      nvarchar(100) = NULL;
DECLARE @action      nvarchar(100) = NULL;
DECLARE @session_id  nvarchar(50)  = NULL;
DECLARE @request_key char(36)      = NULL;
DECLARE @server      nvarchar(50)  = NULL;
DECLARE @min_ms      int           = NULL;

WITH requests AS (
    SELECT 'screen' AS log_type, s.Instant, s.Application_Name, s.Espace_Name, s.Screen, s.Action_Name, s.Request_Key,
           s.Duration, s.Username, s.Executed_By AS Server, s.Client_IP, s.Session_Id
    FROM dbo.oslog_Screen AS s WITH (NOLOCK)
    WHERE s.Instant BETWEEN @from AND @to
      AND (@client_ip IS NULL OR s.Client_IP LIKE '%' + @client_ip + '%')
      AND (@username IS NULL OR s.Username = @username) AND (@espace IS NULL OR s.Espace_Name LIKE @espace)
      AND (@screen IS NULL OR s.Screen = @screen) AND (@action IS NULL OR s.Action_Name = @action)
      AND (@session_id IS NULL OR s.Session_Id = @session_id) AND (@request_key IS NULL OR s.Request_Key = @request_key)
      AND (@server IS NULL OR s.Executed_By = @server) AND (@min_ms IS NULL OR s.Duration >= @min_ms)
    UNION ALL
    SELECT 'mobile_request', m.Instant, m.Application_Name, m.Espace_Name, m.Screen, m.Endpoint, m.Request_Key,
           m.Duration, m.Username, m.Executed_By, m.Source, m.Login_Id
    FROM dbo.oslog_mobile_request AS m WITH (NOLOCK)
    WHERE m.Instant BETWEEN @from AND @to
      AND (@client_ip IS NULL OR m.Source LIKE '%' + @client_ip + '%')
      AND (@username IS NULL OR m.Username = @username) AND (@espace IS NULL OR m.Espace_Name LIKE @espace)
      AND (@screen IS NULL OR m.Screen = @screen) AND (@action IS NULL OR m.Endpoint = @action)
      AND (@session_id IS NULL OR m.Login_Id = @session_id) AND (@request_key IS NULL OR m.Request_Key = @request_key)
      AND (@server IS NULL OR m.Executed_By = @server) AND (@min_ms IS NULL OR m.Duration >= @min_ms)
)
SELECT r.log_type AS request_type, CONVERT(varchar(23), r.Instant, 121) AS request_instant,
       r.Application_Name, r.Espace_Name, r.Screen, r.Action_Name, r.Request_Key, r.Duration AS request_ms,
       r.Username, r.Server, r.Client_IP, r.Session_Id,
       'request' AS call_type, NULL AS call_instant, NULL AS call_name, NULL AS call_detail, NULL AS call_ms, NULL AS call_server
FROM requests AS r
UNION ALL
SELECT r.log_type, CONVERT(varchar(23), r.Instant, 121), r.Application_Name, r.Espace_Name, r.Screen, r.Action_Name, r.Request_Key, r.Duration,
       r.Username, r.Server, r.Client_IP, r.Session_Id,
       'integration', CONVERT(varchar(23), i.Instant, 121), i.Action, i.Endpoint, i.Duration, i.Executed_By
FROM requests AS r
JOIN dbo.oslog_Integration AS i WITH (NOLOCK) ON i.Request_Key = r.Request_Key
WHERE i.Instant BETWEEN @from AND DATEADD(MINUTE, 30, @to)   -- Instant is the call's end
UNION ALL
SELECT r.log_type, CONVERT(varchar(23), r.Instant, 121), r.Application_Name, r.Espace_Name, r.Screen, r.Action_Name, r.Request_Key, r.Duration,
       r.Username, r.Server, r.Client_IP, r.Session_Id,
       'extension', CONVERT(varchar(23), e.Instant, 121), e.Extension_Name + '.' + e.Action_Name, NULL, e.Duration, e.Executed_By
FROM requests AS r
JOIN dbo.oslog_Extension AS e WITH (NOLOCK) ON e.Request_Key = r.Request_Key
WHERE e.Instant BETWEEN @from AND DATEADD(MINUTE, 30, @to)
  AND e.Duration > 0
UNION ALL
-- The platform logs slow queries to the general log with Module_Name
-- SLOWSQL and a message '<statement> took <n> ms'.
SELECT r.log_type, CONVERT(varchar(23), r.Instant, 121), r.Application_Name, r.Espace_Name, r.Screen, r.Action_Name, r.Request_Key, r.Duration,
       r.Username, r.Server, r.Client_IP, r.Session_Id,
       'slow_sql', CONVERT(varchar(23), g.Instant, 121),
       LEFT(g.Message, NULLIF(CHARINDEX(' took ', g.Message), 0) - 1),
       NULL,
       TRY_CAST(SUBSTRING(g.Message, PATINDEX('%took [0-9]%', g.Message) + 5,
                          CHARINDEX(' ms', g.Message, PATINDEX('%took [0-9]%', g.Message)) - PATINDEX('%took [0-9]%', g.Message) - 5) AS int),
       NULL
FROM requests AS r
JOIN dbo.oslog_General AS g WITH (NOLOCK) ON g.Request_Key = r.Request_Key
WHERE g.Instant BETWEEN @from AND DATEADD(MINUTE, 30, @to)
  AND g.Module_Name = 'SLOWSQL'
  AND g.Message LIKE '%took [0-9]% ms%'
UNION ALL
SELECT r.log_type, CONVERT(varchar(23), r.Instant, 121), r.Application_Name, r.Espace_Name, r.Screen, r.Action_Name, r.Request_Key, r.Duration,
       r.Username, r.Server, r.Client_IP, r.Session_Id,
       'error', CONVERT(varchar(23), x.Instant, 121), x.Message, x.Stack, NULL, x.Server
FROM requests AS r
JOIN dbo.oslog_Error AS x WITH (NOLOCK) ON x.Request_key = r.Request_Key
WHERE x.Instant BETWEEN @from AND DATEADD(MINUTE, 30, @to)
ORDER BY request_instant, r.Request_Key, call_instant;
GO

-- ---------------------------------------------------------------------------
-- 3. Everything a client or user did: screen and mobile requests, general
--    log messages and errors, by client IP, user or session. The error log
--    has no client IP, so its rows are matched through the sessions and
--    request keys the other logs saw for that client.
-- ---------------------------------------------------------------------------
DECLARE @from        datetime      = '2026-08-24 10:10:00';
DECLARE @to          datetime      = '2026-08-24 12:30:00';
DECLARE @client_ip   nvarchar(50)  = '10.232.19.133';
DECLARE @username    nvarchar(100) = NULL;
DECLARE @session_id  nvarchar(50)  = NULL;

WITH activity AS (
    SELECT 'screen' AS log_type, s.Instant, s.Application_Name, s.Espace_Name, s.Screen, s.Action_Name, s.Request_Key,
           s.Duration, s.Username, s.Executed_By AS Server, s.Client_IP, s.Session_Id, CAST(NULL AS nvarchar(2000)) AS Message
    FROM dbo.oslog_Screen AS s WITH (NOLOCK)
    WHERE s.Instant BETWEEN @from AND @to
      AND (@client_ip IS NULL OR s.Client_IP LIKE '%' + @client_ip + '%')
      AND (@username IS NULL OR s.Username = @username) AND (@session_id IS NULL OR s.Session_Id = @session_id)
    UNION ALL
    SELECT 'mobile_request', m.Instant, m.Application_Name, m.Espace_Name, m.Screen, m.Endpoint, m.Request_Key,
           m.Duration, m.Username, m.Executed_By, m.Source, m.Login_Id, NULL
    FROM dbo.oslog_mobile_request AS m WITH (NOLOCK)
    WHERE m.Instant BETWEEN @from AND @to
      AND (@client_ip IS NULL OR m.Source LIKE '%' + @client_ip + '%')
      AND (@username IS NULL OR m.Username = @username) AND (@session_id IS NULL OR m.Login_Id = @session_id)
    UNION ALL
    SELECT 'general', g.Instant, g.Application_Name, g.Espace_Name, g.Entrypoint_Name, g.Action_Name, g.Request_Key,
           NULL, g.Username, NULL, g.Client_IP, g.Session_Id, g.Message
    FROM dbo.oslog_General AS g WITH (NOLOCK)
    WHERE g.Instant BETWEEN @from AND @to
      AND (@client_ip IS NULL OR g.Client_IP LIKE '%' + @client_ip + '%')
      AND (@username IS NULL OR g.Username = @username) AND (@session_id IS NULL OR g.Session_Id = @session_id)
)
SELECT log_type, CONVERT(varchar(23), Instant, 121) AS instant, Application_Name, Espace_Name, Screen, Action_Name, Request_Key,
       Duration AS duration_ms, Username, Server, Client_IP, Session_Id, Message
FROM activity
UNION ALL
SELECT 'error', CONVERT(varchar(23), x.Instant, 121), x.Application_Name, x.Espace_Name, x.Entrypoint_Name, x.Action_Name, x.Request_key,
       NULL, x.Username, x.Server, NULL, x.Session_Id, x.Message
FROM dbo.oslog_Error AS x WITH (NOLOCK)
WHERE x.Instant BETWEEN @from AND @to
  AND (   x.Session_Id  IN (SELECT a.Session_Id  FROM activity AS a WHERE a.Session_Id  IS NOT NULL)
       OR x.Request_key IN (SELECT a.Request_Key FROM activity AS a WHERE a.Request_Key IS NOT NULL)
       OR (@username IS NOT NULL AND x.Username = @username))
ORDER BY instant DESC;
GO

-- ---------------------------------------------------------------------------
-- 4. Errors with their full stack trace, by module, user, message or stack.
-- ---------------------------------------------------------------------------
DECLARE @from         datetime      = '2026-08-24 10:10:00';
DECLARE @to           datetime      = '2026-08-24 12:30:00';
DECLARE @espace       nvarchar(50)  = NULL;   -- e.g. 'GCLI[_]GestaoClinica[_]UI'
DECLARE @username     nvarchar(100) = NULL;
DECLARE @message_like nvarchar(200) = NULL;   -- e.g. '%session fixation%'
DECLARE @stack_like   nvarchar(200) = NULL;   -- e.g. '%SNF_Login_Reactive%'

SELECT CONVERT(varchar(23), x.Instant, 121) AS instant, x.Application_Name, x.Espace_Name, x.Module_Name,
       x.Entrypoint_Name AS Screen, x.Action_Name, x.Request_key AS Request_Key, x.Username, x.User_Id, x.Session_Id, x.Server,
       x.Message, x.Stack, d.FullStackTrace, x.EnvironmentInformation
FROM dbo.oslog_Error AS x WITH (NOLOCK)
LEFT JOIN dbo.oslog_Error_Detail AS d WITH (NOLOCK) ON d.Id = x.Id
WHERE x.Instant BETWEEN @from AND @to
  AND (@espace       IS NULL OR x.Espace_Name LIKE @espace)
  AND (@username     IS NULL OR x.Username = @username)
  AND (@message_like IS NULL OR x.Message LIKE @message_like)
  AND (@stack_like   IS NULL OR x.Stack LIKE @stack_like)
ORDER BY x.Instant DESC;
GO

-- ---------------------------------------------------------------------------
-- 5. One request across every log type, by its key (the OutSystems docs'
--    own correlation, extended to all logs). Instant is the start for screen
--    and timer rows and the end for integration and mobile request rows.
-- ---------------------------------------------------------------------------
DECLARE @request_key char(36) = 'a1b2c3d4-0004-4a00-8000-000000000004';

SELECT 'screen' AS log_type, CONVERT(varchar(23), Instant, 121) AS instant, Espace_Name, Screen, Action_Name, Duration AS duration_ms, Executed_By AS server, CAST(NULL AS nvarchar(max)) AS detail
FROM dbo.oslog_Screen WITH (NOLOCK) WHERE Request_Key = @request_key
UNION ALL SELECT 'mobile_request', CONVERT(varchar(23), Instant, 121), Espace_Name, Screen, Endpoint, Duration, Executed_By, NULL
FROM dbo.oslog_mobile_request WITH (NOLOCK) WHERE Request_Key = @request_key
UNION ALL SELECT 'cyclic_job', CONVERT(varchar(23), Instant, 121), Espace_Name, NULL, Cyclic_Job_Name, Duration, Executed_By, NULL
FROM dbo.oslog_Cyclic_Job WITH (NOLOCK) WHERE Request_Key = @request_key
UNION ALL SELECT 'integration', CONVERT(varchar(23), Instant, 121), Espace_Name, NULL, Action, Duration, Executed_By, Endpoint
FROM dbo.oslog_Integration WITH (NOLOCK) WHERE Request_Key = @request_key
UNION ALL SELECT 'extension', CONVERT(varchar(23), Instant, 121), Espace_Name, NULL, Extension_Name + '.' + Action_Name, Duration, Executed_By, NULL
FROM dbo.oslog_Extension WITH (NOLOCK) WHERE Request_Key = @request_key
UNION ALL SELECT 'general', CONVERT(varchar(23), Instant, 121), Espace_Name, Entrypoint_Name, Action_Name, NULL, NULL, Module_Name + ': ' + Message
FROM dbo.oslog_General WITH (NOLOCK) WHERE Request_Key = @request_key
UNION ALL SELECT 'error', CONVERT(varchar(23), Instant, 121), Espace_Name, Entrypoint_Name, Action_Name, NULL, Server, Message
FROM dbo.oslog_Error WITH (NOLOCK) WHERE Request_key = @request_key
UNION ALL SELECT 'request_event', CONVERT(varchar(23), INSTANT, 121), MODULENAME, NULL, REQUESTEVENTNAME, NULL, NULL, EVENTDETAILS
FROM dbo.oslog_RequestEvent WITH (NOLOCK) WHERE REQUESTKEY = @request_key
ORDER BY instant;
GO
