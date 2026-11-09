package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"atrium/apps/api/internal/audit"
	"atrium/apps/api/internal/httpapi"
	"atrium/apps/api/internal/inventory"
	"atrium/apps/api/internal/redis"
	"atrium/apps/api/internal/store"
)

// testSecret is a password the logs must never show.
const testSecret = "s3cr3t-P@ssw0rd"

// testTimeouts are the timeouts of run in tests: long enough for a slow CI
// runner, short enough that a hang fails fast.
func testTimeouts() timeouts {
	return timeouts{readiness: 5 * time.Second, httpShutdown: 2 * time.Second}
}

// runFixture is run started in the background.
type runFixture struct {
	cancel context.CancelFunc
	result chan error
	url    string
}

// startRun starts run with a fresh registry and the given readiness checks;
// configure, when given, adjusts the dependencies first.
func startRun(t *testing.T, checks []httpapi.Check, configure ...func(*runDeps)) *runFixture {
	t.Helper()
	registry, metrics, err := newRegistry()
	if err != nil {
		t.Fatal(err)
	}
	addr := make(chan net.Addr, 1)
	deps := runDeps{
		cfg:            Config{ListenAddr: "127.0.0.1:0"},
		registry:       registry,
		requestMetrics: metrics,
		checks:         checks,
		timeouts:       testTimeouts(),
		onListen:       func(a net.Addr) { addr <- a },
	}
	for _, fn := range configure {
		fn(&deps)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f := &runFixture{cancel: cancel, result: make(chan error, 1)}
	go func() { f.result <- run(ctx, deps) }()
	select {
	case a := <-addr:
		f.url = "http://" + a.String()
	case err := <-f.result:
		t.Fatalf("run returned before listening: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("run did not listen")
	}
	return f
}

// stop cancels run's context and returns how long run took to return, and
// what it returned.
func (f *runFixture) stop(t *testing.T) (time.Duration, error) {
	t.Helper()
	start := time.Now()
	f.cancel()
	select {
	case err := <-f.result:
		return time.Since(start), err
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after its context was cancelled")
		return 0, nil
	}
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp.StatusCode, string(body)
}

func TestRunServesHealthReadinessAndMetrics(t *testing.T) {
	f := startRun(t, nil)
	if code, body := get(t, f.url+"/healthz"); code != http.StatusOK || body != "ok" {
		t.Fatalf("GET /healthz = %d %q", code, body)
	}
	if code, body := get(t, f.url+"/readyz"); code != http.StatusOK || strings.TrimSpace(body) != `{"status":"ready"}` {
		t.Fatalf("GET /readyz = %d %s", code, body)
	}
	code, body := get(t, f.url+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics = %d", code)
	}
	for _, want := range []string{
		"go_goroutines",
		"process_start_time_seconds",
		MetricHTTPRequests + `{code="200",method="GET",route="/healthz"} 1`,
		MetricHTTPRequests + `{code="200",method="GET",route="/readyz"} 1`,
		MetricHTTPRequestDuration + `_count{method="GET",route="/healthz"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /metrics lacks %s", want)
		}
	}
	if _, err := f.stop(t); err != nil {
		t.Fatalf("run returned %v after a clean shutdown", err)
	}
	if resp, err := http.Get(f.url + "/healthz"); err == nil {
		_ = resp.Body.Close()
		t.Fatal("http server still serving after run returned")
	}
}

func TestRunReadinessFollowsItsChecks(t *testing.T) {
	failing := httpapi.Check{Name: "postgres", Run: func(context.Context) error { return errors.New("connection refused") }}
	f := startRun(t, []httpapi.Check{failing})
	if code, body := get(t, f.url+"/readyz"); code != http.StatusServiceUnavailable || strings.TrimSpace(body) != `{"status":"not_ready"}` {
		t.Fatalf("GET /readyz = %d %s, want 503 not_ready", code, body)
	}
	// A failing dependency never fails liveness.
	if code, _ := get(t, f.url+"/healthz"); code != http.StatusOK {
		t.Fatalf("GET /healthz = %d with a failing check, want 200", code)
	}
}

// blockingCheck holds GET /readyz in flight: it reports when a request
// reaches it and returns once released.
func blockingCheck() (check httpapi.Check, reached <-chan struct{}, release func()) {
	reachedCh := make(chan struct{}, 1)
	releaseCh := make(chan struct{})
	check = httpapi.Check{Name: "slow", Run: func(context.Context) error {
		reachedCh <- struct{}{}
		<-releaseCh
		return nil
	}}
	return check, reachedCh, func() { close(releaseCh) }
}

func TestRunDrainsInFlightRequestsBeforeReturning(t *testing.T) {
	check, reached, release := blockingCheck()
	f := startRun(t, []httpapi.Check{check})

	type response struct {
		code int
		err  error
	}
	inFlight := make(chan response, 1)
	go func() {
		resp, err := http.Get(f.url + "/readyz")
		if err != nil {
			inFlight <- response{err: err}
			return
		}
		_ = resp.Body.Close()
		inFlight <- response{code: resp.StatusCode}
	}()
	<-reached

	f.cancel()
	// Shutdown waits for the request: run must not return while it is in
	// flight.
	select {
	case err := <-f.result:
		t.Fatalf("run returned %v with a request in flight", err)
	case <-time.After(200 * time.Millisecond):
	}
	// New connections are refused once shutdown starts.
	eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", strings.TrimPrefix(f.url, "http://"), time.Second)
		if err == nil {
			_ = conn.Close()
		}
		return err != nil
	}, "the listener still accepts connections during shutdown")

	release()
	if got := <-inFlight; got.err != nil || got.code != http.StatusOK {
		t.Fatalf("in-flight request = %d %v, want it answered 200", got.code, got.err)
	}
	select {
	case err := <-f.result:
		if err != nil {
			t.Fatalf("run returned %v after a clean shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after the in-flight request finished")
	}
}

// Once shutdown begins, GET /readyz answers 503 while the API keeps
// accepting connections and serving them for the shutdown delay, so its
// Service drops it before it stops listening; then it drains and returns.
func TestRunServesUnreadyForTheShutdownDelay(t *testing.T) {
	const delay = time.Second
	var logs syncBuffer
	f := startRun(t, nil, func(deps *runDeps) {
		deps.timeouts.shutdownDelay = delay
		deps.logger = slog.New(slog.NewTextHandler(&logs, nil))
	})
	if code, _ := get(t, f.url+"/readyz"); code != http.StatusOK {
		t.Fatalf("GET /readyz before shutdown = %d, want 200", code)
	}
	start := time.Now()
	f.cancel()
	// A new connection for each request: the listener must still accept.
	fresh := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	status := func(path string) int {
		resp, err := fresh.Get(f.url + path)
		if err != nil {
			t.Fatalf("GET %s during the shutdown delay: %v", path, err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	eventually(t, func() bool { return status("/readyz") == http.StatusServiceUnavailable }, "GET /readyz still answers 200 once shutdown has begun")
	if code := status("/healthz"); code != http.StatusOK {
		t.Fatalf("GET /healthz during the shutdown delay = %d, want 200", code)
	}
	select {
	case err := <-f.result:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after the shutdown delay")
	}
	if elapsed := time.Since(start); elapsed < delay {
		t.Fatalf("run returned %s after shutdown began, before the %s delay", elapsed, delay)
	}
	if !strings.Contains(logs.String(), `msg="shutdown: serving unready before draining" delay=1s`) {
		t.Fatalf("logs do not show the delay:\n%s", logs.String())
	}
}

func TestRunShutdownIsBoundedWhenARequestHangs(t *testing.T) {
	check, reached, release := blockingCheck()
	t.Cleanup(release)
	f := startRun(t, []httpapi.Check{check}, func(deps *runDeps) {
		deps.timeouts.httpShutdown = 100 * time.Millisecond
	})
	requestErr := make(chan error, 1)
	go func() {
		resp, err := http.Get(f.url + "/readyz")
		if err == nil {
			_ = resp.Body.Close()
		}
		requestErr <- err
	}()
	<-reached

	elapsed, err := f.stop(t)
	if err != nil {
		t.Fatalf("run returned %v; a drain that times out is still a shutdown", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("shutdown took %s with a 100ms drain bound", elapsed)
	}
	// The hung request's connection is closed rather than left open.
	select {
	case err := <-requestErr:
		if err == nil {
			t.Fatal("the hung request got a response after its connection was closed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the hung request's connection was left open")
	}
}

func TestRunReturnsListenError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	registry, metrics, err := newRegistry()
	if err != nil {
		t.Fatal(err)
	}
	err = run(context.Background(), runDeps{
		cfg:            Config{ListenAddr: listener.Addr().String()},
		registry:       registry,
		requestMetrics: metrics,
		timeouts:       testTimeouts(),
	})
	if err == nil {
		t.Fatal("run did not return the error of a port already in use")
	}
}

// eventually polls cond for up to 5 seconds.
func eventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// syncBuffer is a bytes.Buffer safe for a logger and a test to share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// closedAddr returns a loopback address nothing listens on.
func closedAddr(t *testing.T) *net.TCPAddr {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().(*net.TCPAddr)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// With PostgreSQL and Redis down, the API still starts and stays alive; it
// is not ready, and the log says why for each dependency. On shutdown it
// closes their connections after the HTTP server has stopped.
func TestRunWithDependenciesDown(t *testing.T) {
	var logs syncBuffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	// PostgreSQL's credential is missing; Redis has one but is not there.
	t.Setenv("ATRIUM_CREDENTIAL_SECRET_API_POSTGRES", "")
	t.Setenv("ATRIUM_CREDENTIAL_SECRET_API_REDIS", "default:"+testSecret)
	cfg := Config{
		Postgres: store.Config{
			Host: "127.0.0.1", Port: closedAddr(t).Port, Database: "atrium",
			SSLMode: "disable", CredentialRef: "secret/api-postgres",
		},
		Redis:          redis.Config{Addr: closedAddr(t).String(), CredentialRef: "secret/api-redis"},
		CredentialsDir: t.TempDir(),
	}
	registry := prometheus.NewRegistry()
	deps, err := connect(cfg, logger, registry)
	if err != nil {
		t.Fatal(err)
	}
	checks, closeDeps := deps.checks, deps.close
	// The pools answer at a scrape even before either dependency is reachable.
	for name, want := range map[string]int{
		MetricDBConnections: 3, MetricDBConnectionsMax: 1, MetricDBConnectionWaits: 1, MetricDBConnectionWaitSeconds: 1,
		MetricRedisConnections: 2, MetricRedisConnectionTimeouts: 1,
	} {
		if got, err := testutil.GatherAndCount(registry, name); err != nil || got != want {
			t.Errorf("%s has %d series (%v), want %d", name, got, err, want)
		}
	}
	// The outbox's gauges and the inventory's info metrics need the
	// database: absent while it is down, and the scrape still succeeds.
	for name, want := range map[string]int{
		audit.MetricRecordsDelivered: 1, audit.MetricOutboxRows: 0, audit.MetricOutboxOldestAge: 0,
		inventory.MetricEnvironmentInfo: 0, inventory.MetricApplicationInfo: 0, inventory.MetricSourceInfo: 0,
		inventory.MetricDatabaseTargetInfo: 0, inventory.MetricInfoTruncated: 0,
	} {
		if got, err := testutil.GatherAndCount(registry, name); err != nil || got != want {
			t.Errorf("%s has %d series (%v), want %d", name, got, err, want)
		}
	}
	if len(checks) != 2 || checks[0].Name != checkPostgreSQL || checks[1].Name != checkRedis {
		t.Fatalf("checks = %v, want postgresql and redis", checks)
	}
	if deps.drain != nil || !strings.Contains(logs.String(), "audit delivery off") {
		t.Fatalf("without an audit endpoint: drain set %v, logs:\n%s", deps.drain != nil, logs.String())
	}
	var f *runFixture
	closed := make(chan bool, 1)
	f = startRun(t, checks, func(deps *runDeps) {
		deps.logger = logger
		deps.closeDeps = func() {
			// The HTTP server is already closed.
			conn, err := net.DialTimeout("tcp", strings.TrimPrefix(f.url, "http://"), time.Second)
			if err == nil {
				_ = conn.Close()
			}
			closeDeps()
			closed <- err != nil
		}
	})

	if code, body := get(t, f.url+"/readyz"); code != http.StatusServiceUnavailable || strings.TrimSpace(body) != `{"status":"not_ready"}` {
		t.Fatalf("GET /readyz = %d %s, want 503 not_ready", code, body)
	}
	if code, _ := get(t, f.url+"/healthz"); code != http.StatusOK {
		t.Fatalf("GET /healthz = %d with both dependencies down, want 200", code)
	}
	for _, want := range []string{
		`"msg":"readiness changed"`,
		`postgresql: missing credential for ref secret/api-postgres`,
		`redis: `,
		`connection refused`,
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs lack %q:\n%s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), testSecret) {
		t.Fatal("logs contain the Redis password")
	}

	if _, err := f.stop(t); err != nil {
		t.Fatalf("run returned %v after a clean shutdown", err)
	}
	select {
	case afterHTTP := <-closed:
		if !afterHTTP {
			t.Fatal("dependencies closed while the HTTP server still accepted connections")
		}
	default:
		t.Fatal("run returned without closing the PostgreSQL and Redis connections")
	}
}

// Settings that cannot work fail Run at once instead of serving a
// readiness that can never pass.
func TestNewRunDepsRejectsInvalidSettings(t *testing.T) {
	_, err := newRunDeps(Config{
		ListenAddr: "127.0.0.1:0",
		Postgres:   store.Config{Host: "db", Port: 5432, Database: "atrium", SSLMode: "prefer", CredentialRef: "secret/api-postgres"},
		Redis:      redis.Config{Addr: "valkey:6379", CredentialRef: "secret/api-redis"},
		Logger:     slog.New(slog.DiscardHandler),
	})
	if err == nil || !strings.Contains(err.Error(), "postgresql") {
		t.Fatalf("newRunDeps = %v, want a postgresql settings error", err)
	}
	_, err = newRunDeps(Config{
		ListenAddr: "127.0.0.1:0",
		Postgres:   store.Config{Host: "db", Port: 5432, Database: "atrium", SSLMode: "disable", CredentialRef: "secret/api-postgres"},
		Redis:      redis.Config{Addr: "valkey", CredentialRef: "secret/api-redis"},
		Logger:     slog.New(slog.DiscardHandler),
	})
	if err == nil || !strings.Contains(err.Error(), "redis") {
		t.Fatalf("newRunDeps = %v, want a redis settings error", err)
	}
}
