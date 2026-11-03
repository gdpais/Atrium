// Package collectors reads what the DB collector runs, through its
// authenticated, redacted GET /admin/config, read-only (ADR 0013 §1): so far
// which environments its outsystems_logs collectors extract, for the API to
// say whether a telemetry source is observed (ADR 0017 §4).
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

// KindOutSystemsLogs is the collector kind that extracts OutSystems 11 logs
// (ADR 0008).
const KindOutSystemsLogs = "outsystems_logs"

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
	cached   Coverage
	err      error
	cachedAt time.Time
	now      func() time.Time
}

// adminConfig is the part of GET /admin/config the API reads. The
// collector's runtime config has no JSON tags, so its keys are Go names.
type adminConfig struct {
	Config struct {
		Collectors []struct {
			ID          string
			Kind        string
			Enabled     bool
			TargetNames []string
			Targets     []struct {
				Name            string
				EnvironmentSlug string
			}
		}
	} `json:"config"`
}

// OutSystemsLogs returns which environments the collector's enabled
// outsystems_logs collectors extract, from its last answer if that is
// younger than TTL.
func (c *Client) OutSystemsLogs(ctx context.Context) (Coverage, error) {
	if c == nil || c.URL == "" {
		return Coverage{}, ErrNotConfigured
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
		return c.cached, c.err
	}
	// The answer is shared and kept, so it must not depend on this caller:
	// a client that gives up does not cancel the request, and its failure
	// is not kept for everyone. The client's timeout still bounds it.
	c.cached, c.err = c.fetch(context.WithoutCancel(ctx), now)
	c.cachedAt = now()
	return c.cached, c.err
}

func (c *Client) fetch(ctx context.Context, now func() time.Time) (Coverage, error) {
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: DefaultTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.URL, "/")+"/admin/config", nil)
	if err != nil {
		return Coverage{}, fmt.Errorf("the collector's URL: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Coverage{}, errors.New("the collector did not answer")
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return Coverage{}, fmt.Errorf("the collector refused the API's admin token (%d)", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return Coverage{}, fmt.Errorf("the collector answered %d", resp.StatusCode)
	}
	var doc adminConfig
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&doc); err != nil {
		return Coverage{}, errors.New("the collector's answer is not its admin config")
	}
	return coverage(doc, now()), nil
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
