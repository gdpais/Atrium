package sqlserver

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	collectormetadata "atrium/services/db-collector/internal/metadata"
)

const testPassword = "s3cr3t-P@ssw0rd"

// fakeDriver is a database/sql driver whose connect/ping behaviour is
// controlled per test.  Each test registers its own instance under a unique
// name.  Queries answer the pool's login check with one row holding login.
type fakeDriver struct {
	// ping, when set, is called on every ping and every query with the DSN
	// host.
	ping func(ctx context.Context, host string) error
	// login holds the IS_SRVROLEMEMBER('sysadmin') and CONTROL SERVER
	// HAS_PERMS_BY_NAME results of the check query: int64(1), int64(0) or
	// nil for NULL.  Guarded by mu; see setLogin.
	login [2]driver.Value

	connectors atomic.Int64 // sql.Open calls (one per *sql.DB)
	closed     atomic.Int64 // *sql.DB closes
	pings      atomic.Int64 // pings and queries

	mu      sync.Mutex
	dsns    []string
	queries []string
}

var fakeDriverSeq atomic.Int64

func newFakeDriver(t *testing.T) (*fakeDriver, string) {
	t.Helper()
	d := &fakeDriver{login: [2]driver.Value{int64(0), int64(0)}}
	name := fmt.Sprintf("sqlserver-fake-%d", fakeDriverSeq.Add(1))
	sql.Register(name, d)
	return d, name
}

func (d *fakeDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("fake driver: use OpenConnector")
}

func (d *fakeDriver) OpenConnector(dsn string) (driver.Connector, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, errors.New("fake driver: bad dsn")
	}
	d.connectors.Add(1)
	d.mu.Lock()
	d.dsns = append(d.dsns, dsn)
	d.mu.Unlock()
	return &fakeConnector{d: d, host: u.Hostname()}, nil
}

func (d *fakeDriver) lastDSN() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.dsns) == 0 {
		return ""
	}
	return d.dsns[len(d.dsns)-1]
}

func (d *fakeDriver) setLogin(member, control driver.Value) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.login = [2]driver.Value{member, control}
}

func (d *fakeDriver) allQueries() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.queries...)
}

type fakeConnector struct {
	d    *fakeDriver
	host string
}

func (c *fakeConnector) Connect(context.Context) (driver.Conn, error) {
	return &fakeConn{c: c}, nil
}

func (c *fakeConnector) Driver() driver.Driver { return c.d }

func (c *fakeConnector) Close() error {
	c.d.closed.Add(1)
	return nil
}

type fakeConn struct{ c *fakeConnector }

func (*fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (*fakeConn) Close() error                        { return nil }
func (*fakeConn) Begin() (driver.Tx, error)           { return nil, errors.New("not supported") }

func (f *fakeConn) Ping(ctx context.Context) error {
	f.c.d.pings.Add(1)
	if f.c.d.ping != nil {
		return f.c.d.ping(ctx, f.c.host)
	}
	return nil
}

// QueryContext records query and answers it like the login check.
func (f *fakeConn) QueryContext(ctx context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	d := f.c.d
	d.mu.Lock()
	d.queries = append(d.queries, query)
	login := d.login
	d.mu.Unlock()
	if err := f.Ping(ctx); err != nil {
		return nil, err
	}
	return &fakeRows{row: login[:]}, nil
}

// fakeRows is a result set of at most one two-column row.
type fakeRows struct {
	row  []driver.Value
	done bool
}

func (*fakeRows) Columns() []string { return []string{"", ""} }
func (*fakeRows) Close() error      { return nil }

func (r *fakeRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	copy(dest, r.row)
	return nil
}

// mapResolver resolves credentials from a mutable map.
type mapResolver struct {
	mu    sync.Mutex
	creds map[string]Credential
}

func (r *mapResolver) Resolve(_ context.Context, ref string) (Credential, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.creds[ref]
	if !ok {
		return Credential{}, fmt.Errorf("missing credential for ref %s", ref)
	}
	return c, nil
}

func (r *mapResolver) set(ref string, c Credential) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.creds[ref] = c
}

func newResolver() *mapResolver {
	return &mapResolver{creds: map[string]Credential{
		"ref-a": {Username: "collector", Password: testPassword},
		"ref-b": {Username: "collector", Password: testPassword},
	}}
}

func target(name, host, ref string) collectormetadata.DatabaseTarget {
	return collectormetadata.DatabaseTarget{
		Name: name, Host: host, Port: 1433, DatabaseName: "master", CredentialRef: ref,
	}
}

func newTestManager(t *testing.T) (Manager, *fakeDriver, *mapResolver) {
	t.Helper()
	d, name := newFakeDriver(t)
	r := newResolver()
	m := NewManager(r)
	m.driverName = name
	t.Cleanup(func() { _ = m.Close() })
	return m, d, r
}

func mustOpen(t *testing.T, m Manager, tg collectormetadata.DatabaseTarget) (*sql.DB, func()) {
	t.Helper()
	db, cleanup, err := m.Open(context.Background(), tg)
	if err != nil {
		t.Fatalf("Open(%s): %v", tg.Name, err)
	}
	if db == nil || cleanup == nil {
		t.Fatalf("Open(%s) returned nil db or cleanup", tg.Name)
	}
	return db, cleanup
}

func TestOpenReusesPoolForSameTarget(t *testing.T) {
	m, d, _ := newTestManager(t)
	tg := target("a", "db-a.example", "ref-a")

	db1, cleanup1 := mustOpen(t, m, tg)
	cleanup1()
	cleanup1() // idempotent
	db2, cleanup2 := mustOpen(t, m, tg)
	defer cleanup2()

	if db1 != db2 {
		t.Fatal("expected the same *sql.DB for the same target")
	}
	if got := d.connectors.Load(); got != 1 {
		t.Fatalf("sql.Open calls = %d, want 1", got)
	}
	if got := d.pings.Load(); got != 1 {
		t.Fatalf("pings = %d, want 1 (only on pool creation)", got)
	}
	if got := d.closed.Load(); got != 0 {
		t.Fatalf("cleanup closed the shared pool (%d closes)", got)
	}
	if err := db2.PingContext(context.Background()); err != nil {
		t.Fatalf("pooled handle unusable after cleanup: %v", err)
	}
	stats := db2.Stats()
	if stats.MaxOpenConnections != defaultMaxOpenConns {
		t.Fatalf("MaxOpenConnections = %d, want %d", stats.MaxOpenConnections, defaultMaxOpenConns)
	}
}

func TestOpenCopiesShareThePool(t *testing.T) {
	m, d, _ := newTestManager(t)
	copyOfM := m
	tg := target("a", "db-a.example", "ref-a")

	db1, c1 := mustOpen(t, m, tg)
	defer c1()
	db2, c2 := mustOpen(t, copyOfM, tg)
	defer c2()
	if db1 != db2 || d.connectors.Load() != 1 {
		t.Fatal("copies of a Manager should share pools")
	}
}

func TestOpenSeparatesPoolsByCredentialAndSettings(t *testing.T) {
	m, d, r := newTestManager(t)
	tg := target("a", "db-a.example", "ref-a")

	db1, c1 := mustOpen(t, m, tg)
	defer c1()

	r.set("ref-a", Credential{Username: "collector", Password: "rotated"})
	db2, c2 := mustOpen(t, m, tg)
	defer c2()
	if db1 == db2 {
		t.Fatal("rotated credential should get a fresh pool")
	}

	other := tg
	other.CredentialRef = "ref-b"
	db3, c3 := mustOpen(t, m, other)
	defer c3()
	if db3 == db1 || db3 == db2 {
		t.Fatal("different credential ref should get a different pool")
	}

	trusting := m
	trusting.TrustServerCertificate = true
	db4, c4 := mustOpen(t, trusting, tg)
	defer c4()
	if db4 == db2 {
		t.Fatal("DSN-affecting settings set after NewManager must be part of the pool key")
	}
	if !strings.Contains(d.lastDSN(), "TrustServerCertificate=true") {
		t.Fatal("DSN should be computed from the Manager value passed to Open")
	}
	if got := d.connectors.Load(); got != 4 {
		t.Fatalf("sql.Open calls = %d, want 4", got)
	}
}

func TestOpenDoesNotCacheFailedFirstPing(t *testing.T) {
	m, d, _ := newTestManager(t)
	var fail atomic.Bool
	fail.Store(true)
	d.ping = func(context.Context, string) error {
		if fail.Load() {
			return errors.New("login failed")
		}
		return nil
	}
	tg := target("a", "db-a.example", "ref-a")

	db, cleanup, err := m.Open(context.Background(), tg)
	if err == nil || db != nil || cleanup != nil {
		t.Fatalf("expected failure with nil db/cleanup, got db=%v cleanup=%v err=%v", db, cleanup != nil, err)
	}
	if strings.Contains(err.Error(), testPassword) {
		t.Fatalf("error leaks password: %v", err)
	}
	if got := d.closed.Load(); got != 1 {
		t.Fatalf("failed handle closes = %d, want 1", got)
	}

	fail.Store(false)
	_, c := mustOpen(t, m, tg)
	defer c()
	if got := d.connectors.Load(); got != 2 {
		t.Fatalf("sql.Open calls = %d, want 2 (failure must not be cached)", got)
	}
}

func TestOpenConcurrentSameKeyCreatesOnePool(t *testing.T) {
	m, d, _ := newTestManager(t)
	d.ping = func(context.Context, string) error {
		time.Sleep(50 * time.Millisecond)
		return nil
	}
	tg := target("a", "db-a.example", "ref-a")

	const n = 50
	var wg sync.WaitGroup
	dbs := make([]*sql.DB, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			db, cleanup, err := m.Open(context.Background(), tg)
			dbs[i], errs[i] = db, err
			if err == nil {
				cleanup()
			}
		})
	}
	close(start)
	wg.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if dbs[i] != dbs[0] {
			t.Fatalf("goroutine %d got a different pool", i)
		}
	}
	if got := d.connectors.Load(); got != 1 {
		t.Fatalf("sql.Open calls = %d, want 1", got)
	}
}

func TestOpenConcurrentWaitersShareFirstPingFailure(t *testing.T) {
	m, d, _ := newTestManager(t)
	release := make(chan struct{})
	d.ping = func(context.Context, string) error {
		<-release
		return errors.New("unreachable")
	}
	tg := target("a", "db-a.example", "ref-a")

	const n = 10
	var wg sync.WaitGroup
	var failures atomic.Int64
	for range n {
		wg.Go(func() {
			if _, _, err := m.Open(context.Background(), tg); err != nil {
				failures.Add(1)
			}
		})
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if failures.Load() != n {
		t.Fatalf("failures = %d, want %d", failures.Load(), n)
	}
	if got := d.connectors.Load(); got > n {
		t.Fatalf("sql.Open calls = %d, want at most %d", got, n)
	}
}

func TestOpenSlowTargetDoesNotBlockOtherTargets(t *testing.T) {
	m, d, _ := newTestManager(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	d.ping = func(ctx context.Context, host string) error {
		if host == "slow.example" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}

	slowDone := make(chan error, 1)
	go func() {
		_, cleanup, err := m.Open(context.Background(), target("slow", "slow.example", "ref-a"))
		if err == nil {
			cleanup()
		}
		slowDone <- err
	}()
	<-entered

	fastDone := make(chan error, 1)
	go func() {
		_, cleanup, err := m.Open(context.Background(), target("fast", "fast.example", "ref-a"))
		if err == nil {
			cleanup()
		}
		fastDone <- err
	}()
	select {
	case err := <-fastDone:
		if err != nil {
			t.Fatalf("fast target: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Open for a healthy target blocked behind a slow target")
	}

	close(release)
	if err := <-slowDone; err != nil {
		t.Fatalf("slow target: %v", err)
	}
}

func TestOpenWaiterHonoursContext(t *testing.T) {
	m, d, _ := newTestManager(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	d.ping = func(context.Context, string) error {
		close(entered)
		<-release
		return nil
	}
	tg := target("a", "db-a.example", "ref-a")
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, c, err := m.Open(context.Background(), tg); err == nil {
			c()
		}
	}()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := m.Open(ctx, tg); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter err = %v, want deadline exceeded", err)
	}
	close(release)
	<-done
}

func TestOpenEvictsIdlePools(t *testing.T) {
	m, d, _ := newTestManager(t)
	var mu sync.Mutex
	now := time.Unix(1_700_000_000, 0)
	m.pool.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(dur time.Duration) {
		mu.Lock()
		now = now.Add(dur)
		mu.Unlock()
	}
	a := target("a", "db-a.example", "ref-a")
	b := target("b", "db-b.example", "ref-a")
	c := target("c", "db-c.example", "ref-a")

	dbA, cleanupA := mustOpen(t, m, a)
	cleanupA()
	_, cleanupB := mustOpen(t, m, b) // held: must survive eviction

	advance(defaultPoolIdleTTL + time.Minute)
	_, cleanupC := mustOpen(t, m, c) // triggers the sweep
	defer cleanupC()

	if got := d.closed.Load(); got != 1 {
		t.Fatalf("closes after sweep = %d, want 1 (only idle, unreferenced pool)", got)
	}
	if err := dbA.PingContext(context.Background()); err == nil {
		t.Fatal("evicted pool should be closed")
	}
	dbA2, cleanupA2 := mustOpen(t, m, a)
	defer cleanupA2()
	if dbA2 == dbA {
		t.Fatal("evicted pool should be recreated")
	}
	cleanupB()
}

func TestCloseClosesAllPoolsAndIsIdempotent(t *testing.T) {
	m, d, _ := newTestManager(t)
	dbA, cleanupA := mustOpen(t, m, target("a", "db-a.example", "ref-a"))
	cleanupA()
	_, cleanupB := mustOpen(t, m, target("b", "db-b.example", "ref-a"))

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := d.closed.Load(); got != 2 {
		t.Fatalf("closes = %d, want 2", got)
	}
	if err := dbA.PingContext(context.Background()); err == nil {
		t.Fatal("pool should be closed after Manager.Close")
	}
	cleanupB() // releasing after Close is safe
	if err := m.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, _, err := m.Open(context.Background(), target("a", "db-a.example", "ref-a")); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("Open after Close err = %v, want ErrManagerClosed", err)
	}
	if got := d.closed.Load(); got != 2 {
		t.Fatalf("closes after second Close = %d, want 2", got)
	}
}

func TestCloseDuringInitClosesNewHandle(t *testing.T) {
	m, d, _ := newTestManager(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	d.ping = func(context.Context, string) error {
		close(entered)
		<-release
		return nil
	}
	errCh := make(chan error, 1)
	go func() {
		_, _, err := m.Open(context.Background(), target("a", "db-a.example", "ref-a"))
		errCh <- err
	}()
	<-entered
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	close(release)
	if err := <-errCh; !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("in-flight Open err = %v, want ErrManagerClosed", err)
	}
	if got := d.closed.Load(); got != 1 {
		t.Fatalf("closes = %d, want 1", got)
	}
}

func TestZeroValueManagerOpensPerCall(t *testing.T) {
	d, name := newFakeDriver(t)
	m := Manager{Resolver: newResolver(), DialTimeout: time.Second, driverName: name}
	tg := target("a", "db-a.example", "ref-a")

	db1, cleanup1 := mustOpen(t, m, tg)
	cleanup1()
	db2, cleanup2 := mustOpen(t, m, tg)
	cleanup2()

	if db1 == db2 {
		t.Fatal("zero-value Manager should not pool")
	}
	if got := d.closed.Load(); got != 2 {
		t.Fatalf("closes = %d, want 2 (cleanup closes unpooled handles)", got)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close on zero-value Manager: %v", err)
	}
	if _, _, err := (Manager{}).Open(context.Background(), tg); err == nil {
		t.Fatal("Manager without resolver should fail, not panic")
	}
}

func TestDSNFormatsIPv6Hosts(t *testing.T) {
	m, d, _ := newTestManager(t)
	for _, tc := range []struct{ host, want string }{
		{"::1", "[::1]:1433"},
		{"fe80::1%en0", "[fe80::1%en0]:1433"},
		{"db.example", "db.example:1433"},
		{"10.0.0.5", "10.0.0.5:1433"},
	} {
		dsn := m.dsn(target("x", tc.host, "ref-a"), Credential{Username: "u", Password: "p"})
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("host %q: dsn does not parse: %v", tc.host, err)
		}
		if u.Host != tc.want {
			t.Fatalf("host %q: got %q, want %q", tc.host, u.Host, tc.want)
		}
	}
	_, cleanup := mustOpen(t, m, target("v6", "::1", "ref-a"))
	defer cleanup()
	u, err := url.Parse(d.lastDSN())
	if err != nil || u.Hostname() != "::1" || u.Port() != "1433" {
		t.Fatalf("driver received bad DSN host (err=%v)", err)
	}
}

func TestPoolKeyDoesNotContainSecrets(t *testing.T) {
	m, _, _ := newTestManager(t)
	creds := Credential{Username: "collector-user", Password: testPassword}
	key := m.pool.key(m, target("a", "db-a.example", "ref-a"), creds)
	rendered := fmt.Sprintf("%+v %#v", key, key)
	for _, secret := range []string{creds.Username, creds.Password} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("pool key contains secret material: %s", rendered)
		}
	}
	if key.credentialHash == "" {
		t.Fatal("pool key should carry a credential hash")
	}
	// Username/password boundary must be unambiguous.
	other := m.pool.key(m, target("a", "db-a.example", "ref-a"), Credential{Username: creds.Username + testPassword[:1], Password: testPassword[1:]})
	if other.credentialHash == key.credentialHash {
		t.Fatal("credential hash should distinguish username/password splits")
	}
}

func TestSessionSettingsMatchLockTimeout(t *testing.T) {
	want := fmt.Sprintf("SET LOCK_TIMEOUT %d;", LockTimeout.Milliseconds())
	if !strings.HasPrefix(SessionSettings, want) {
		t.Fatalf("SessionSettings = %q, want prefix %q", SessionSettings, want)
	}
	if !strings.Contains(SessionSettings, "SET DEADLOCK_PRIORITY LOW;") {
		t.Fatalf("SessionSettings = %q, want DEADLOCK_PRIORITY LOW", SessionSettings)
	}
	// The settings must end their statement so any query can follow.
	if !strings.HasSuffix(strings.TrimSpace(SessionSettings), ";") {
		t.Fatalf("SessionSettings %q does not end with a statement terminator", SessionSettings)
	}
}

func TestWithSessionSettings(t *testing.T) {
	for _, tc := range []struct{ name, query string }{
		{"select", "SELECT status FROM sys.dm_exec_sessions"},
		{"cte", "WITH s AS (SELECT 1 AS n) SELECT n FROM s"},
		{"multi statement", "SET NOCOUNT ON; SELECT 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := WithSessionSettings(tc.query)
			if got != SessionSettings+tc.query {
				t.Fatalf("WithSessionSettings(%q) = %q", tc.query, got)
			}
		})
	}
}

func TestClassifyLogin(t *testing.T) {
	null := sql.NullInt64{}
	yes := sql.NullInt64{Int64: 1, Valid: true}
	no := sql.NullInt64{Valid: true}
	for _, tc := range []struct {
		name            string
		member, control sql.NullInt64
		want            loginState
	}{
		{"sysadmin", yes, yes, loginState{known: true, elevated: true}},
		{"control server only", no, yes, loginState{known: true, elevated: true}},
		{"sysadmin with unknown control server", yes, null, loginState{known: true, elevated: true}},
		{"least privilege", no, no, loginState{known: true}},
		{"unknown membership", null, no, loginState{}},
		{"unknown control server", no, null, loginState{}},
		{"all unknown", null, null, loginState{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyLogin(tc.member, tc.control); got != tc.want {
				t.Fatalf("classifyLogin = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestOpenChecksLoginOncePerPool(t *testing.T) {
	for _, tc := range []struct {
		name            string
		member, control driver.Value
		sysadmin, ok    bool
		warnings        int
	}{
		{"sysadmin", int64(1), int64(1), true, true, 1},
		{"control server", int64(0), int64(1), true, true, 1},
		{"least privilege", int64(0), int64(0), false, true, 0},
		{"unknown", nil, int64(0), false, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, d, _ := newTestManager(t)
			d.setLogin(tc.member, tc.control)
			var logs bytes.Buffer
			m.Logger = slog.New(slog.NewTextHandler(&logs, nil))
			tg := target("a", "db-a.example", "ref-a")

			if _, ok := m.Sysadmin(tg); ok {
				t.Fatal("Sysadmin known before the first Open")
			}
			for range 2 {
				_, cleanup := mustOpen(t, m, tg)
				cleanup()
			}
			sysadmin, ok := m.Sysadmin(tg)
			if sysadmin != tc.sysadmin || ok != tc.ok {
				t.Fatalf("Sysadmin = %v, %v; want %v, %v", sysadmin, ok, tc.sysadmin, tc.ok)
			}
			queries := d.allQueries()
			if len(queries) != 1 {
				t.Fatalf("queries = %d, want 1 check per pool", len(queries))
			}
			if queries[0] != loginCheckQuery || !strings.HasPrefix(queries[0], SessionSettings) {
				t.Fatalf("check query = %q, want %q", queries[0], loginCheckQuery)
			}
			if got := strings.Count(logs.String(), "level=WARN"); got != tc.warnings {
				t.Fatalf("warnings = %d, want %d; logs:\n%s", got, tc.warnings, logs.String())
			}
			for _, secret := range []string{testPassword, "collector"} {
				if strings.Contains(logs.String(), secret) {
					t.Fatalf("log contains credential material %q:\n%s", secret, logs.String())
				}
			}
		})
	}
}

// fakeTime is a manually advanced clock for pool.now.
type fakeTime struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeTime) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeTime) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestOpenRechecksLoginPeriodically(t *testing.T) {
	m, d, _ := newTestManager(t)
	var logs bytes.Buffer
	var logMu sync.Mutex
	m.Logger = slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		logMu.Lock()
		defer logMu.Unlock()
		return logs.Write(p)
	}), &slog.HandlerOptions{Level: slog.LevelDebug}))
	clock := &fakeTime{now: time.Unix(1_700_000_000, 0)}
	m.pool.now = clock.Now
	var failing atomic.Bool
	d.ping = func(context.Context, string) error {
		if failing.Load() {
			return errors.New("connection reset")
		}
		return nil
	}
	tg := target("a", "db-a.example", "ref-a")
	open := func() {
		t.Helper()
		_, cleanup := mustOpen(t, m, tg)
		cleanup()
	}
	assertState := func(step string, queries int, sysadmin bool) {
		t.Helper()
		if got := len(d.allQueries()); got != queries {
			t.Fatalf("%s: queries = %d, want %d", step, got, queries)
		}
		if got, ok := m.Sysadmin(tg); !ok || got != sysadmin {
			t.Fatalf("%s: Sysadmin = %v, %v; want %v, true", step, got, ok, sysadmin)
		}
	}

	open()
	assertState("pool created", 1, false)

	d.setLogin(int64(1), int64(1))
	clock.Advance(defaultLoginCheckInterval - time.Second)
	open()
	assertState("before the interval", 1, false)

	clock.Advance(time.Second)
	open()
	assertState("interval reached: re-checked", 2, true)
	open()
	assertState("just re-checked", 2, true)

	failing.Store(true)
	clock.Advance(defaultLoginCheckInterval)
	open() // a failed re-check does not fail Open: the probe query reports it
	assertState("failed re-check keeps the last result", 3, true)
	failing.Store(false)
	open()
	assertState("failed re-check is not retried on every Open", 3, true)

	logMu.Lock()
	text := logs.String()
	logMu.Unlock()
	if got := strings.Count(text, "level=WARN"); got != 1 {
		t.Fatalf("warnings = %d, want 1 (only the change to sysadmin); logs:\n%s", got, text)
	}
	if !strings.Contains(text, "re-check failed") {
		t.Fatalf("failed re-check not logged at debug; logs:\n%s", text)
	}
}

// writerFunc adapts a function to io.Writer.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestOnlyOneCallerRechecksLogin(t *testing.T) {
	m, d, _ := newTestManager(t)
	m.Logger = slog.New(slog.DiscardHandler)
	clock := &fakeTime{now: time.Unix(1_700_000_000, 0)}
	m.pool.now = clock.Now
	tg := target("a", "db-a.example", "ref-a")
	_, cleanup := mustOpen(t, m, tg)
	cleanup()

	entered := make(chan struct{})
	release := make(chan struct{})
	var blocking atomic.Bool
	blocking.Store(true)
	d.ping = func(context.Context, string) error {
		if blocking.CompareAndSwap(true, false) {
			close(entered)
			<-release
		}
		return nil
	}
	clock.Advance(defaultLoginCheckInterval)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, c := mustOpen(t, m, tg)
		c()
	}()
	<-entered

	// While the re-check is in flight, other Opens neither wait for it nor
	// start their own, and the pool mutex is free.
	for range 5 {
		openDone := make(chan struct{})
		go func() {
			defer close(openDone)
			_, c := mustOpen(t, m, tg)
			c()
		}()
		select {
		case <-openDone:
		case <-time.After(2 * time.Second):
			t.Fatal("Open blocked behind an in-flight login re-check")
		}
	}
	if _, ok := m.Sysadmin(tg); !ok {
		t.Fatal("Sysadmin should answer from the last result during a re-check")
	}
	close(release)
	<-done
	if got := len(d.allQueries()); got != 2 {
		t.Fatalf("queries = %d, want 2 (creation and one re-check)", got)
	}
}

func TestSysadminFollowsTheMostRecentPool(t *testing.T) {
	m, d, r := newTestManager(t)
	m.Logger = slog.New(slog.DiscardHandler)
	tg := target("a", "db-a.example", "ref-a")

	d.setLogin(int64(1), int64(1))
	_, c1 := mustOpen(t, m, tg)
	c1()
	r.set("ref-a", Credential{Username: "collector", Password: "rotated"})
	d.setLogin(int64(0), int64(0))
	_, c2 := mustOpen(t, m, tg)
	c2()
	if sysadmin, ok := m.Sysadmin(tg); !ok || sysadmin {
		t.Fatalf("Sysadmin = %v, %v; want the rotated pool's false, true", sysadmin, ok)
	}

	other := tg
	other.Host = "db-b.example"
	if _, ok := m.Sysadmin(other); ok {
		t.Fatal("Sysadmin known for a target without a pool")
	}
	if _, ok := (Manager{Resolver: r}).Sysadmin(tg); ok {
		t.Fatal("Sysadmin known for a Manager without a pool")
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, ok := m.Sysadmin(tg); ok {
		t.Fatal("Sysadmin known after Close")
	}
}

// secretForms returns the forms in which password can appear in error text.
func secretForms(password string) []string {
	return []string{
		password,
		strings.TrimPrefix(url.UserPassword("", password).String(), ":"),
		url.QueryEscape(password),
		url.PathEscape(password),
	}
}

// go-mssqldb parses the DSN when it connects.  Before v1.11 its parse errors
// were the *url.Error, quoting the whole DSN; since then they are a fixed
// message, and this guards against a driver that leaks again.  A host that
// already carries a port makes the parse fail before any dial, so the real
// driver is exercised without a server.  Quotes and backslashes in the host
// or login change how url.Error quotes the DSN.
func TestOpenErrorsNeverContainCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, host string
		creds      Credential
	}{
		{"port in host", "db.example:1433", Credential{Username: "collector", Password: "S3cret!@/?#:%+ x'"}},
		{"quote in host and login", `db"x:1`, Credential{Username: `col"lector`, Password: `pa"ss\word-long`}},
		{"backslash in host", `db\x:1`, Credential{Username: `dom\collector`, Password: "S3cret-password"}},
		{"short password", "db.example:1433", Credential{Username: "collector", Password: "e"}},
	} {
		resolver := &mapResolver{creds: map[string]Credential{"ref-a": tc.creds}}
		tg := target("bad-host", tc.host, "ref-a")
		for _, manager := range []struct {
			name string
			m    Manager
		}{
			{"pooled", NewManager(resolver)},
			{"unpooled", Manager{Resolver: resolver, DialTimeout: time.Second}},
		} {
			t.Run(tc.name+"/"+manager.name, func(t *testing.T) {
				t.Cleanup(func() { _ = manager.m.Close() })
				_, _, err := manager.m.Open(context.Background(), tg)
				if err == nil {
					t.Fatal("Open succeeded for a malformed host")
				}
				if !strings.Contains(err.Error(), "parse ") {
					t.Fatalf("err = %v, want the DSN parse failure", err)
				}
				userinfo := url.UserPassword(tc.creds.Username, tc.creds.Password).String()
				secrets := []string{userinfo, strconv.Quote(userinfo), tc.creds.Username + ":", url.User(tc.creds.Username).String() + ":"}
				if len(tc.creds.Password) >= minRedactedPasswordLen {
					secrets = append(secrets, secretForms(tc.creds.Password)...)
				}
				for _, secret := range secrets {
					if strings.Contains(err.Error(), secret) {
						t.Fatalf("error contains credential material %q: %v", secret, err)
					}
				}
				var urlErr *url.Error
				if errors.As(err, &urlErr) && strings.Contains(urlErr.URL, "@") {
					t.Fatalf("url.Error.URL keeps userinfo: %q", urlErr.URL)
				}
			})
		}
	}
}

func TestRedactError(t *testing.T) {
	creds := Credential{Username: "collector", Password: "p@ss/w0rd!"}
	sentinel := errors.New("sentinel")
	dsn := "sqlserver://collector:" + strings.TrimPrefix(url.UserPassword("", creds.Password).String(), ":") + "@[db:1]:1433?database=master"
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"no secret", fmt.Errorf("dial tcp: %w", sentinel), "dial tcp: sentinel"},
		{"wrapped url error", fmt.Errorf("check: %w", &url.Error{Op: "parse", URL: dsn, Err: sentinel}),
			`check: parse "sqlserver://[db:1]:1433?database=master": sentinel`},
		{"joined url error", errors.Join(sentinel, &url.Error{Op: "parse", URL: dsn, Err: sentinel}),
			"sentinel\n" + `parse "sqlserver://[db:1]:1433?database=master": sentinel`},
		{"quoted url error with a quote in the host", fmt.Errorf("check: %w", &url.Error{Op: "parse", URL: `sqlserver://collector:` + strings.TrimPrefix(url.UserPassword("", creds.Password).String(), ":") + `@[d"b:1]:1433`, Err: sentinel}),
			`check: parse "sqlserver://[d\"b:1]:1433": sentinel`},
		{"userinfo in text", fmt.Errorf("dsn %s: %w", dsn, sentinel), "dsn sqlserver://xxxxx@[db:1]:1433?database=master: sentinel"},
		{"raw password in text", fmt.Errorf("login %s: %w", creds.Password, sentinel), "login xxxxx: sentinel"},
		{"query-escaped password in text", fmt.Errorf("dsn password=%s: %w", url.QueryEscape(creds.Password), sentinel), "dsn password=xxxxx: sentinel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := redactError(tc.err, creds)
			if tc.err == nil {
				if got != nil {
					t.Fatalf("redactError(nil) = %v", got)
				}
				return
			}
			if got.Error() != tc.want {
				t.Fatalf("redactError() = %q, want %q", got.Error(), tc.want)
			}
			if !errors.Is(got, sentinel) {
				t.Fatal("redactError broke the error chain")
			}
			for _, secret := range secretForms(creds.Password) {
				if strings.Contains(got.Error(), secret) {
					t.Fatalf("redacted error contains %q", secret)
				}
			}
		})
	}
}

// A short password is redacted only where it appears as part of the DSN
// userinfo; replacing it everywhere would mangle the text and reveal it.
func TestRedactErrorShortPassword(t *testing.T) {
	creds := Credential{Username: "collector", Password: "e"}
	sentinel := errors.New("sentinel")
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"plain text untouched", fmt.Errorf("login error: %w", sentinel), "login error: sentinel"},
		{"userinfo removed", fmt.Errorf("dsn sqlserver://collector:e@db: %w", sentinel), "dsn sqlserver://xxxxx@db: sentinel"},
		{"url error scrubbed", fmt.Errorf("check: %w", &url.Error{Op: "parse", URL: "sqlserver://collector:e@[db:1]:1433", Err: sentinel}),
			`check: parse "sqlserver://[db:1]:1433": sentinel`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactError(tc.err, creds).Error(); got != tc.want {
				t.Fatalf("redactError() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRedactURL(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"sqlserver://u:p%40ss@db:1433?database=master", "sqlserver://db:1433?database=master"},
		{"sqlserver://u:p@[db.example:1433]:1433?x=y", "sqlserver://[db.example:1433]:1433?x=y"},
		{"sqlserver://u:p@db/instance", "sqlserver://db/instance"},
		{"sqlserver://db:1433?email=a@b", "sqlserver://db:1433?email=a@b"},
		{"not a url", "not a url"},
	} {
		if got := redactURL(tc.raw); got != tc.want {
			t.Errorf("redactURL(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}
