package probedefs

import (
	"errors"
	"strings"
	"testing"

	"heartbeat/internal/config"
)

// valid is content every check accepts.
func valid() Content {
	return Content{Overrides: "sessions", QueryTemplate: "SELECT status, COUNT(*) AS session_count FROM sys.dm_exec_sessions GROUP BY status"}
}

func TestNormalizeAppliesDefaults(t *testing.T) {
	got, err := normalize(valid())
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultIntervalSeconds != DefaultIntervalSeconds || got.TimeoutMS != 0 {
		t.Fatalf("normalize() = %+v, want the default interval %d and no timeout", got, DefaultIntervalSeconds)
	}
	c := valid()
	c.DefaultIntervalSeconds, c.TimeoutMS, c.Description = MaxIntervalSeconds, MaxTimeoutMS, "line one\n\tline two"
	if got, err = normalize(c); err != nil || got != c {
		t.Fatalf("normalize(%+v) = %+v, %v; want it unchanged", c, got, err)
	}
}

func TestNormalizeRejects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Content)
		want   string
	}{
		{"no overrides", func(c *Content) { c.Overrides = "" }, "overrides"},
		{"overrides not a probe name", func(c *Content) { c.Overrides = "Waits" }, "overrides"},
		{"interval too short", func(c *Content) { c.DefaultIntervalSeconds = MinIntervalSeconds - 1 }, "default_interval_seconds"},
		{"interval too long", func(c *Content) { c.DefaultIntervalSeconds = MaxIntervalSeconds + 1 }, "default_interval_seconds"},
		{"negative interval", func(c *Content) { c.DefaultIntervalSeconds = -1 }, "default_interval_seconds"},
		{"negative timeout", func(c *Content) { c.TimeoutMS = -1 }, "timeout_ms"},
		{"timeout past the collector's maximum", func(c *Content) { c.TimeoutMS = MaxTimeoutMS + 1 }, "timeout_ms"},
		{"long description", func(c *Content) { c.Description = strings.Repeat("é", MaxDescriptionRunes+1) }, "description"},
		{"control character in the description", func(c *Content) { c.Description = "a\x00b" }, "description"},
		{"no query", func(c *Content) { c.QueryTemplate = "" }, "query_template must be one read-only SELECT: it is empty"},
		{"query too long", func(c *Content) { c.QueryTemplate = "SELECT 1" + strings.Repeat(" ", MaxQueryBytes) }, "query_template: want at most"},
		{"query not UTF-8", func(c *Content) { c.QueryTemplate = "SELECT '\xff'" }, "query_template: want at most"},
		{"two statements", func(c *Content) { c.QueryTemplate = "SELECT 1; DROP TABLE t" }, "query_template must be one read-only SELECT: it has more than one statement"},
		{"a write", func(c *Content) { c.QueryTemplate = "SELECT 1 INTO t" }, "query_template must be one read-only SELECT: it contains INTO"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := valid()
			tc.change(&c)
			_, err := normalize(c)
			var invalid *InvalidError
			if !errors.As(err, &invalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("normalize() = %v, want an *InvalidError containing %q", err, tc.want)
			}
		})
	}
}

// The API accepts a query exactly when the collector's loader does: both
// call config.ValidateReadOnlySelect. The queries are a sample of what it
// accepts and why it refuses, so a check of the API's own, stricter or
// looser, fails here.
func TestQueryCheckIsTheCollectors(t *testing.T) {
	for _, query := range []string{
		"SELECT 1",
		";WITH a AS (SELECT 1 AS x) SELECT x FROM a",
		"SELECT 'DROP TABLE x; EXEC xp_cmdshell ''dir''' AS s",
		"SELECT [Insert], [Drop]] me] FROM [Backup].[Delete]",
		"SELECT a.x FROM a INNER MERGE JOIN b ON a.x = b.x OPTION (MERGE JOIN)",
		"SELECT 1 /* comment */DROP TABLE x",
		"SELECT 1 SELECT 2",
		"UPDATE t SET x = 1",
		"EXEC sp_who",
		"SELECT * FROM OPENROWSET('SQLNCLI', 'Server=x;', 'SELECT 1')",
		"SELECT 1 WAITFOR DELAY '00:00:10'",
		"SELECT xp_cmdshell('dir')",
		"SELECT (1",
		"SELECT 'unterminated",
		"SELECT 1 DROP TABLE x",
	} {
		c := valid()
		c.QueryTemplate = query
		_, apiErr := normalize(c)
		collectorErr := config.ValidateReadOnlySelect(query)
		if (apiErr == nil) != (collectorErr == nil) {
			t.Errorf("query %q: the API says %v, the collector %v", query, apiErr, collectorErr)
		}
		if collectorErr != nil && apiErr != nil && !strings.HasSuffix(apiErr.Error(), collectorErr.Error()) {
			t.Errorf("query %q: the API says %q, want the collector's reason %q", query, apiErr, collectorErr)
		}
	}
}
