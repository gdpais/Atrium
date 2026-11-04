// Package collectors reads what the DB collector runs, through its
// authenticated, redacted GET /admin/config, read-only (ADR 0013 §1): which
// environments its outsystems_logs collectors extract, for the API to say
// whether a telemetry source is observed (ADR 0017 §4), and what its
// sqlserver collectors run, for the API to say whether a probe assignment is
// applied (ADR 0014 §4).
//
// The collector's desired state stays in YAML (ADR 0002): the API only
// reads what the collector reports, and a collector it cannot reach makes a
// source's observation unknown, never a change fail.
package collectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Defaults for a Client.
const (
	DefaultTimeout = 2 * time.Second
	DefaultTTL     = 15 * time.Second
	// maxBody bounds the config read: a few hundred collectors at most.
	maxBody = 8 << 20
)

// Collector kinds the API reads.
const (
	// KindOutSystemsLogs extracts OutSystems 11 logs (ADR 0008).
	KindOutSystemsLogs = "outsystems_logs"
	// KindSQLServer runs probes against SQL Server targets.
	KindSQLServer = "sqlserver"
)

// ErrNotConfigured is a Client without the collector's URL.
var ErrNotConfigured = errors.New("the API is not configured to read the collector (HEARTBEAT_API_COLLECTOR_URL)")

// Coverage is which environments the enabled outsystems_logs collectors
// extract, as the collector reported it.
type Coverage struct {
	// CheckedAt is when the collector answered.
	CheckedAt time.Time
	// ByEnvironment maps an environment slug to the IDs of the collectors
	// whose logs carry it as deployment.environment.name, sorted.
	ByEnvironment map[string][]string
}

// Client reads the collector's admin config. It is safe for concurrent use;
// an answer, or a failure, is kept for TTL, so that lists and wallboards
// make one request at most every TTL.
type Client struct {
	// URL is the collector's base URL, such as http://db-collector:8082.
	URL string
	// Token is the collector's admin token, sent as a bearer token.
	Token string
	// HTTP sends the requests; nil is a client with DefaultTimeout.
	HTTP *http.Client
	// TTL is how long an answer is kept; 0 is DefaultTTL.
	TTL time.Duration

	mu       sync.Mutex
	cached   adminConfig
	err      error
	cachedAt time.Time
	now      func() time.Time
}

// SQLServerCollectors is what the collector's sqlserver collectors run, as it
// reported it.
type SQLServerCollectors struct {
	// CheckedAt is when the collector answered.
	CheckedAt time.Time
	// ByID maps a collector's ID to what it runs.
	ByID map[string]SQLServerCollector
}

// SQLServerCollector is one sqlserver collector the collector runs.
type SQLServerCollector struct {
	Enabled        bool
	ScrapeInterval time.Duration
	// Targets are the targets it scrapes, those target_names selects.
	Targets []Target
}

// Target is a target of a sqlserver collector, its probes resolved: its own,
// or else the collector's.
type Target struct {
	Name            string
	EnvironmentSlug string
	Host            string
	Port            int
	DatabaseName    string
	Probes          []Probe
}

// Probe is a probe a target runs.
type Probe struct {
	Name          string
	QueryTemplate string
	TimeoutMS     int
	MaxRows       int
}

// adminConfig is the part of GET /admin/config the API reads. The
// collector's runtime config has no JSON tags, so its keys are Go names.
type adminConfig struct {
	Config struct {
		Collectors []struct {
			ID             string
			Kind           string
			Enabled        bool
			TargetNames    []string
			ScrapeInterval time.Duration
			Targets        []struct {
				Name            string
				EnvironmentSlug string
				Host            string
				Port            int
				DatabaseName    string
				Probes          []Probe
			}
		}
	} `json:"config"`
}

// OutSystemsLogs returns which environments the collector's enabled
// outsystems_logs collectors extract, from its last answer if that is
// younger than TTL.
func (c *Client) OutSystemsLogs(ctx context.Context) (Coverage, error) {
	doc, at, err := c.config(ctx)
	if err != nil {
		return Coverage{}, err
	}
	return coverage(doc, at), nil
}

// SQLServer returns what the collector's sqlserver collectors run, from its
// last answer if that is younger than TTL.
func (c *Client) SQLServer(ctx context.Context) (SQLServerCollectors, error) {
	doc, at, err := c.config(ctx)
	if err != nil {
		return SQLServerCollectors{}, err
	}
	out := SQLServerCollectors{CheckedAt: at, ByID: map[string]SQLServerCollector{}}
	for _, collector := range doc.Config.Collectors {
		if collector.Kind != KindSQLServer {
			continue
		}
		running := SQLServerCollector{Enabled: collector.Enabled, ScrapeInterval: collector.ScrapeInterval}
		for _, target := range collector.Targets {
			if len(collector.TargetNames) > 0 && !slices.Contains(collector.TargetNames, target.Name) {
				continue
			}
			running.Targets = append(running.Targets, Target{
				Name: target.Name, EnvironmentSlug: target.EnvironmentSlug, Host: target.Host,
				Port: target.Port, DatabaseName: target.DatabaseName, Probes: target.Probes,
			})
		}
		out.ByID[collector.ID] = running
	}
	return out, nil
}

// config returns the collector's admin config and when it answered, from
// its last answer if that is younger than TTL.
func (c *Client) config(ctx context.Context) (adminConfig, time.Time, error) {
	if c == nil || c.URL == "" {
		return adminConfig{}, time.Time{}, ErrNotConfigured
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	ttl := c.TTL
	if ttl == 0 {
		ttl = DefaultTTL
	}
	if !c.cachedAt.IsZero() && now().Sub(c.cachedAt) < ttl {
		return c.cached, c.cachedAt, c.err
	}
	// The answer is shared and kept, so it must not depend on this caller:
	// a client that gives up does not cancel the request, and its failure
	// is not kept for everyone. The client's timeout still bounds it.
	c.cached, c.err = c.fetch(context.WithoutCancel(ctx))
	c.cachedAt = now()
	return c.cached, c.cachedAt, c.err
}

func (c *Client) fetch(ctx context.Context) (adminConfig, error) {
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: DefaultTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.URL, "/")+"/admin/config", nil)
	if err != nil {
		return adminConfig{}, fmt.Errorf("the collector's URL: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return adminConfig{}, errors.New("the collector did not answer")
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return adminConfig{}, fmt.Errorf("the collector refused the API's admin token (%d)", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return adminConfig{}, fmt.Errorf("the collector answered %d", resp.StatusCode)
	}
	var doc adminConfig
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&doc); err != nil {
		return adminConfig{}, errors.New("the collector's answer is not its admin config")
	}
	return doc, nil
}

// coverage applies the collector's own rule: an enabled outsystems_logs
// collector extracts each of its targets that target_names selects (all
// when it is empty), and stamps the target's environment, which defaults to
// the collector's, as deployment.environment.name.
func coverage(doc adminConfig, at time.Time) Coverage {
	out := Coverage{CheckedAt: at, ByEnvironment: map[string][]string{}}
	for _, collector := range doc.Config.Collectors {
		if !collector.Enabled || collector.Kind != KindOutSystemsLogs {
			continue
		}
		for _, target := range collector.Targets {
			if len(collector.TargetNames) > 0 && !slices.Contains(collector.TargetNames, target.Name) {
				continue
			}
			if env := target.EnvironmentSlug; env != "" && !slices.Contains(out.ByEnvironment[env], collector.ID) {
				out.ByEnvironment[env] = append(out.ByEnvironment[env], collector.ID)
			}
		}
	}
	for env := range out.ByEnvironment {
		slices.Sort(out.ByEnvironment[env])
	}
	return out
}
