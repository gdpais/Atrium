// Package sqlserver provides a connection manager for Microsoft SQL Server
// targets used by the db-collector service.
//
// Manager.Open resolves credentials via a [CredentialResolver], builds a DSN,
// and returns a pooled [database/sql.DB] for the target.  A Manager created by
// [NewManager] keeps one bounded connection pool per distinct target, login
// and connection setting, so repeated probes against the same target reuse
// authenticated connections instead of performing a TCP, TLS and SQL login
// handshake on every call.  A pool is verified when it is first created, with
// a single query that also records whether the login is sysadmin-equivalent
// ([Manager.Sysadmin]); that check is repeated about every 10 minutes while
// the pool is in use.  Idle pools are evicted lazily and [Manager.Close]
// releases all of them.  The caller must invoke the cleanup
// function returned by Open when it has finished with the handle.
//
// Every batch the collector sends must start with [SessionSettings] (see
// [WithSessionSettings]), so a collector query never waits long on a lock and
// is always the preferred deadlock victim.
//
// Credential references are resolved by package [credentials]:
// [FileCredentialResolver] reads HEARTBEAT_CREDENTIAL_<REF> from a file in a
// directory, such as a mounted Kubernetes Secret, on every [Manager.Open], so
// a rotated credential needs no restart; [EnvCredentialResolver] reads the
// environment variable of that name.
package sqlserver

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/microsoft/go-mssqldb"

	collectorconfig "heartbeat/internal/config"
	"heartbeat/internal/credentials"
	collectormetadata "heartbeat/services/db-collector/internal/metadata"
)

// Credential holds the username and password for a SQL Server connection.
type Credential = credentials.Credential

// CredentialResolver resolves a named credential reference into a
// [Credential].
type CredentialResolver = credentials.Resolver

// EnvCredentialResolver resolves credentials from environment variables
// ([credentials.Env]).
type EnvCredentialResolver = credentials.Env

// FileCredentialResolver resolves credentials from the files of a mounted
// Secret, falling back to another resolver ([credentials.Files]).
type FileCredentialResolver = credentials.Files

// Connection pool limits applied to every pooled [database/sql.DB].  Probes
// within a target run serially, so two connections per pool leave headroom for
// the occasional overlap without multiplying server-side sessions.
const (
	// defaultMaxOpenConns bounds the open connections per pool.
	defaultMaxOpenConns = 2
	// defaultMaxIdleConns bounds the idle connections kept per pool.
	defaultMaxIdleConns = 2
	// defaultConnMaxLifetime recycles connections periodically so that server
	// fail-overs, DNS changes and certificate rotations are eventually picked
	// up.
	defaultConnMaxLifetime = 10 * time.Minute
	// defaultConnMaxIdleTime closes connections that have not been used
	// recently.
	defaultConnMaxIdleTime = 5 * time.Minute
	// defaultPoolIdleTTL is how long a pool may go unused before Open evicts
	// and closes it.  This reclaims pools for targets removed by config
	// reloads.
	defaultPoolIdleTTL = 15 * time.Minute
	// defaultDriverName is the database/sql driver registered by go-mssqldb.
	defaultDriverName = "sqlserver"
	// defaultLoginCheckInterval is how often a pool in use repeats the login
	// check, so a login granted or stripped of sysadmin is noticed within
	// about one connection lifetime.
	defaultLoginCheckInterval = defaultConnMaxLifetime
	// minRedactedPasswordLen is the shortest password redactError replaces
	// wherever it appears in error text; shorter ones would mangle the text
	// and reveal the password by the pattern of replacements.
	minRedactedPasswordLen = 8
)

// LockTimeout is how long a collector statement waits for a lock before SQL
// Server cancels it with error 1222.  It is far below the default probe
// timeout (half the scrape interval, at most 10s), so a lock wait fails fast
// with a specific error instead of keeping the probe queued behind, and in
// front of, application lock requests until the probe deadline.
const LockTimeout = time.Second

// SessionSettings is the T-SQL prefix of every collector batch.  It caps lock
// waits at [LockTimeout] and makes the collector's session the preferred
// deadlock victim, so a probe never wins a deadlock against the application.
//
// The settings are sent in the same batch as the query instead of through a
// session initialisation statement, which go-mssqldb would run as an extra
// round trip on every pooled connection reuse.  Pooled connections are reset
// before the batch runs, so the settings hold for exactly that batch.  The
// LOCK_TIMEOUT value is [LockTimeout] in milliseconds; a test keeps them in
// sync.
const SessionSettings = "SET LOCK_TIMEOUT 1000; SET DEADLOCK_PRIORITY LOW;\n"

// WithSessionSettings returns query prefixed with [SessionSettings].  Every
// statement the collector sends to a target, including operator
// query_template overrides, must go through it.
func WithSessionSettings(query string) string {
	return SessionSettings + query
}

// loginCheckQuery verifies a pool and reports whether its login is
// sysadmin-equivalent: a member of the sysadmin server role, or holding
// CONTROL SERVER, which grants the same rights.  A login can always see its
// own role membership and permissions, so it needs no extra permission.
const loginCheckQuery = SessionSettings + "SELECT IS_SRVROLEMEMBER('sysadmin'), HAS_PERMS_BY_NAME(NULL, NULL, 'CONTROL SERVER')"

// loginState is what the login check found out about a pool's login.
type loginState struct {
	// known is false when SQL Server could not tell (NULL results).
	known bool
	// elevated is true when the login is a member of sysadmin or holds
	// CONTROL SERVER.
	elevated bool
}

// classifyLogin interprets the two columns of [loginCheckQuery].  Either one
// being 1 is enough to call the login elevated; otherwise both must be 0 to
// call it least-privileged, and anything else is unknown.
func classifyLogin(member, control sql.NullInt64) loginState {
	switch {
	case member.Valid && member.Int64 == 1, control.Valid && control.Int64 == 1:
		return loginState{known: true, elevated: true}
	case member.Valid && control.Valid:
		return loginState{known: true}
	default:
		return loginState{}
	}
}

// ErrManagerClosed is returned by [Manager.Open] after [Manager.Close] has
// been called on a pooled Manager (or any copy of it).
var ErrManagerClosed = errors.New("sqlserver connection manager is closed")

// Manager opens authenticated, TLS-encrypted connections to SQL Server targets.
//
// A Manager returned by [NewManager] owns shared connection-pool state behind
// a pointer, so copies of it share the same pools and closing any copy closes
// them all.  A Manager constructed as a struct literal has no pool and opens a
// fresh connection on every call to [Manager.Open].
type Manager struct {
	// Resolver provides credentials for a given credential reference string.
	Resolver CredentialResolver
	// Application is the client application name sent to the server for
	// observability purposes.
	Application string
	// DialTimeout limits how long the TCP dial of a new connection may take
	// (the driver's "dial timeout").  The pre-login, TLS and login exchanges
	// that follow ignore it; each of their reads is bounded by the socket
	// timeout instead (see socketTimeout).  It also bounds the check query run
	// when a pool is first created, except while it waits on those
	// exchanges.
	DialTimeout time.Duration
	// QueryTimeout is reserved for future use; individual query deadlines are
	// currently managed at the probe level.
	QueryTimeout time.Duration
	// Encrypt controls whether the connection uses TLS encryption.
	Encrypt bool
	// TrustServerCertificate skips hostname and chain verification while keeping
	// TLS transport enabled. This is intended for dev-only self-signed targets.
	TrustServerCertificate bool
	// Logger receives the warning logged when a target's login is a member of
	// sysadmin.  Nil means [slog.Default].
	Logger *slog.Logger

	// pool holds the shared per-target connection pools; nil disables pooling.
	pool *pool
	// driverName overrides the database/sql driver; empty means "sqlserver".
	// It exists so tests can substitute a fake driver.
	driverName string
}

// NewManager returns a Manager configured with sensible production defaults:
// 5-second dial/query timeouts, TLS encryption enabled, the application name
// set to "HeartbeatDBCollector", and connection pooling enabled.  Callers
// should invoke [Manager.Close] on shutdown to release pooled connections.
func NewManager(resolver CredentialResolver) Manager {
	return Manager{
		Resolver:     resolver,
		Application:  "HeartbeatDBCollector",
		DialTimeout:  5 * time.Second,
		QueryTimeout: 5 * time.Second,
		Encrypt:      true,
		pool:         newPool(),
	}
}

// Open resolves credentials for target and returns a [database/sql.DB] handle
// for it together with a cleanup function.  On success the caller must always
// invoke cleanup exactly once when it has finished using the handle, and must
// not call Close on the handle itself.  On failure cleanup is nil.  Open is
// safe for concurrent use.
//
// For a Manager created by [NewManager] the handle is a shared connection pool
// keyed by the target address, database, credential reference, a keyed hash
// of the resolved username and password (so a rotated credential gets a fresh
// pool) and every other connection setting.  The pool is created and verified
// with the login check query, bounded by DialTimeout, on first use; a failed
// first check is not cached, so the next Open retries.  When the last check
// is about 10 minutes old, the one Open that claims it re-runs the check
// before returning (other Opens do not wait); a failed re-check keeps the
// last known result and is retried 10 minutes later.  A sysadmin-equivalent
// or undeterminable login is logged as a warning when the pool is created and
// whenever a re-check changes the result.  cleanup releases the caller's
// reference; it does not close the pool.  Pools unused for 15 minutes are
// closed lazily by subsequent Opens.  After [Manager.Close], Open returns
// [ErrManagerClosed].
//
// For a Manager without a pool (a struct literal), Open opens and checks a new
// handle on every call (warning on every call for a sysadmin-equivalent
// login) and cleanup closes it.
func (m Manager) Open(ctx context.Context, target collectormetadata.DatabaseTarget) (*sql.DB, func(), error) {
	if m.Resolver == nil {
		return nil, nil, errors.New("sqlserver manager has no credential resolver")
	}
	creds, err := m.Resolver.Resolve(ctx, target.CredentialRef)
	if err != nil {
		return nil, nil, err
	}
	dsn := m.dsn(target, creds)
	if m.pool == nil {
		db, _, err := m.connect(ctx, target, dsn, creds, false)
		if err != nil {
			return nil, nil, err
		}
		return db, func() { _ = db.Close() }, nil
	}
	key := m.pool.key(m, target, creds)
	return m.pool.acquire(ctx, key,
		func(ctx context.Context) (*sql.DB, loginState, error) {
			return m.connect(ctx, target, dsn, creds, true)
		},
		func(ctx context.Context, db *sql.DB, previous loginState) (loginState, error) {
			return m.recheckLogin(ctx, target, db, creds, previous)
		})
}

// Sysadmin reports whether the login of target's pool is sysadmin-equivalent
// (a member of sysadmin or holding CONTROL SERVER), as of its last check,
// which is at most about 10 minutes old while the pool is in use.  ok is
// false when the Manager has no pool, target has no established pool (before
// its first successful Open, or after the pool was evicted), or SQL Server
// could not tell.  When a rotated credential left several pools for target,
// the most recently used one answers.  Sysadmin never connects to the target.
func (m Manager) Sysadmin(target collectormetadata.DatabaseTarget) (sysadmin, ok bool) {
	if m.pool == nil {
		return false, false
	}
	return m.pool.sysadmin(m.targetKey(target))
}

// Close closes every pooled connection handle and makes subsequent calls to
// [Manager.Open] on this Manager and all of its copies return
// [ErrManagerClosed].  Handles still held by callers stop accepting new
// queries.  Close returns the joined close errors, is safe to call more than
// once (later calls return nil), and is a no-op for a Manager without a pool.
func (m Manager) Close() error {
	if m.pool == nil {
		return nil
	}
	return m.pool.close()
}

func (m Manager) driver() string {
	if m.driverName != "" {
		return m.driverName
	}
	return defaultDriverName
}

// socketTimeout is the go-mssqldb "connection timeout": a deadline on every
// socket read and write, so no single read waits longer.  The driver ignores
// the query context while it reads the pre-login and TLS handshake, and after
// cancelling a query it waits for the server's acknowledgement without a
// deadline, so on a frozen or vanished server those reads would otherwise last
// until TCP gives up, which can take many minutes.
//
// It is derived from [collectorconfig.MaxProbeTimeout] so it can never cut a
// probe short: every probe is cancelled by its own timeout, at most
// MaxProbeTimeout, before a read of a healthy server reaches this deadline,
// and socketTimeoutMargin leaves room for the cancel round trip.  Only a
// connection that is already dead hits it; such a connection then fails and
// leaves the pool within about two socket timeouts.
const socketTimeout = collectorconfig.MaxProbeTimeout + socketTimeoutMargin

// socketTimeoutMargin is how much longer than the longest probe timeout a
// socket read may wait.
const socketTimeoutMargin = 5 * time.Second

// dsn builds the go-mssqldb connection URL.  The result contains the password
// and must never be logged or included in errors.
func (m Manager) dsn(target collectormetadata.DatabaseTarget, creds Credential) string {
	query := url.Values{}
	query.Set("database", target.DatabaseName)
	query.Set("app name", m.Application)
	query.Set("encrypt", strconv.FormatBool(m.Encrypt))
	query.Set("TrustServerCertificate", strconv.FormatBool(m.TrustServerCertificate))
	query.Set("dial timeout", strconv.Itoa(int(m.DialTimeout.Seconds())))
	query.Set("connection timeout", strconv.Itoa(int(socketTimeout.Seconds())))
	return (&url.URL{
		Scheme:   "sqlserver",
		User:     url.UserPassword(creds.Username, creds.Password),
		Host:     net.JoinHostPort(target.Host, strconv.Itoa(target.Port)),
		RawQuery: query.Encode(),
	}).String()
}

// connect opens a handle for dsn and verifies it with the login check,
// logging a warning when the login is sysadmin-equivalent or undeterminable.
// pooled applies the pool limits.  On failure the handle is closed and the
// error is scrubbed of creds before it is wrapped (see [redactError]):
// go-mssqldb parses dsn when it connects, and before v1.11 its parse errors
// quoted the whole DSN.
func (m Manager) connect(ctx context.Context, target collectormetadata.DatabaseTarget, dsn string, creds Credential, pooled bool) (*sql.DB, loginState, error) {
	db, err := sql.Open(m.driver(), dsn)
	if err != nil {
		return nil, loginState{}, fmt.Errorf("open sqlserver connection for target %s: %w", target.Name, redactError(err, creds))
	}
	if pooled {
		db.SetMaxOpenConns(defaultMaxOpenConns)
		db.SetMaxIdleConns(defaultMaxIdleConns)
		db.SetConnMaxLifetime(defaultConnMaxLifetime)
		db.SetConnMaxIdleTime(defaultConnMaxIdleTime)
	}
	state, err := m.checkLogin(ctx, db, creds)
	if err != nil {
		_ = db.Close()
		return nil, loginState{}, fmt.Errorf("check sqlserver target %s: %w", target.Name, err)
	}
	m.logLogin(target, state)
	return db, state, nil
}

// checkLogin runs [loginCheckQuery] on db, bounded by DialTimeout.  Errors
// are scrubbed of creds and not wrapped.
func (m Manager) checkLogin(ctx context.Context, db *sql.DB, creds Credential) (loginState, error) {
	checkCtx := ctx
	if m.DialTimeout > 0 {
		var cancel context.CancelFunc
		checkCtx, cancel = context.WithTimeout(ctx, m.DialTimeout)
		defer cancel()
	}
	// Both functions return NULL when SQL Server cannot tell.
	var member, control sql.NullInt64
	if err := db.QueryRowContext(checkCtx, loginCheckQuery).Scan(&member, &control); err != nil {
		return loginState{}, redactError(err, creds)
	}
	return classifyLogin(member, control), nil
}

// recheckLogin repeats the login check of an established pool.  It logs a
// changed result like a new one, and a failure at debug level only: the
// target's probes report connection problems themselves.
func (m Manager) recheckLogin(ctx context.Context, target collectormetadata.DatabaseTarget, db *sql.DB, creds Credential, previous loginState) (loginState, error) {
	state, err := m.checkLogin(ctx, db, creds)
	if err != nil {
		m.logger().Debug("sqlserver login re-check failed; keeping the last result",
			"target", target.Name,
			"credential_ref", target.CredentialRef,
			"error", err)
		return previous, err
	}
	if state != previous {
		m.logLogin(target, state)
	}
	return state, nil
}

// logLogin warns when state is sysadmin-equivalent or unknown.  It never logs
// the login name.
func (m Manager) logLogin(target collectormetadata.DatabaseTarget, state loginState) {
	switch {
	case !state.known:
		m.logger().Warn("could not determine whether the sqlserver login is sysadmin-equivalent",
			"target", target.Name,
			"credential_ref", target.CredentialRef)
	case state.elevated:
		m.logger().Warn("sqlserver login is sysadmin-equivalent (sysadmin or CONTROL SERVER); use a login with only the documented grants",
			"target", target.Name,
			"credential_ref", target.CredentialRef)
	}
}

// redactedSecret replaces credential material in error text.
const redactedSecret = "xxxxx"

// redactError removes credential material from a driver error, before the
// caller wraps it:
//
//   - the userinfo of the URL of every [*url.Error] in its chain, edited in
//     place (so errors.As callers see the redacted URL too) and replaced in
//     the text, raw and in the quoted form url.Error prints, because wrappers
//     such as fmt.Errorf format their text when they are created;
//   - the userinfo ("user:password@", URL-escaped as in the DSN) anywhere in
//     the text;
//   - the password itself, raw or URL-escaped, but only when it has at least
//     minRedactedPasswordLen characters.
//
// The chain is preserved for errors.Is and errors.As.  It returns nil for a
// nil err.
func redactError(err error, creds Credential) error {
	if err == nil {
		return nil
	}
	text := err.Error()
	redacted := text
	for original, safe := range redactURLErrors(err, map[string]string{}) {
		redacted = strings.ReplaceAll(redacted, strconv.Quote(original), strconv.Quote(safe))
		redacted = strings.ReplaceAll(redacted, original, safe)
	}
	if creds.Username != "" || creds.Password != "" {
		redacted = strings.ReplaceAll(redacted, url.UserPassword(creds.Username, creds.Password).String()+"@", redactedSecret+"@")
	}
	if len(creds.Password) >= minRedactedPasswordLen {
		userinfo := strings.TrimPrefix(url.UserPassword("", creds.Password).String(), ":")
		for _, secret := range []string{creds.Password, userinfo, url.QueryEscape(creds.Password), url.PathEscape(creds.Password)} {
			redacted = strings.ReplaceAll(redacted, secret, redactedSecret)
		}
	}
	if redacted == text {
		return err
	}
	return &redactedErr{text: redacted, err: err}
}

// redactURLErrors strips the userinfo from the URL of every [*url.Error] in
// err's chain, including joined errors, and records each original URL and
// its redacted form in replaced, which it returns.
func redactURLErrors(err error, replaced map[string]string) map[string]string {
	switch e := err.(type) {
	case nil:
		return replaced
	case *url.Error:
		if safe := redactURL(e.URL); safe != e.URL {
			replaced[e.URL] = safe
			e.URL = safe
		}
	}
	switch e := err.(type) {
	case interface{ Unwrap() error }:
		redactURLErrors(e.Unwrap(), replaced)
	case interface{ Unwrap() []error }:
		for _, inner := range e.Unwrap() {
			redactURLErrors(inner, replaced)
		}
	}
	return replaced
}

// redactURL drops the userinfo ("user:password@") from raw, which need not
// be a valid URL: parse errors quote the very strings that failed to parse.
// The userinfo is everything before the last "@" of the authority, which
// ends at the first "/", "?" or "#"; userinfo escapes all four characters.
func redactURL(raw string) string {
	scheme := strings.Index(raw, "://")
	if scheme < 0 {
		return raw
	}
	rest := raw[scheme+len("://"):]
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	at := strings.LastIndex(rest[:end], "@")
	if at < 0 {
		return raw
	}
	return raw[:scheme+len("://")] + rest[at+1:]
}

// redactedErr is an error whose text had credential material removed.  It
// unwraps to the original error.
type redactedErr struct {
	text string
	err  error
}

// Error implements error with the redacted text.
func (e *redactedErr) Error() string { return e.text }

// Unwrap returns the original error.
func (e *redactedErr) Unwrap() error { return e.err }

// logger returns the configured logger or [slog.Default].
func (m Manager) logger() *slog.Logger {
	if m.Logger == nil {
		return slog.Default()
	}
	return m.Logger
}

// poolKey identifies one pooled handle.  It holds every setting that affects
// the DSN; the credential is represented only by a keyed hash.
type poolKey struct {
	driver                 string
	host                   string
	port                   int
	database               string
	credentialRef          string
	credentialHash         string
	application            string
	encrypt                bool
	trustServerCertificate bool
	dialTimeout            time.Duration
}

// pool is the shared, mutex-guarded set of per-key handles behind a Manager.
type pool struct {
	// secret keys the credential hash so pool keys cannot be used to guess
	// passwords offline.
	secret  [32]byte
	idleTTL time.Duration
	now     func() time.Time

	// checkInterval is how old a pool's login check may get before the next
	// Open re-runs it; zero disables re-checks.
	checkInterval time.Duration

	mu      sync.Mutex
	closed  bool
	entries map[poolKey]*poolEntry
	// latest maps a target key (a pool key without the credential hash) to
	// the most recently used initialised entry for that target.
	latest map[poolKey]*poolEntry
}

// poolEntry is one handle, possibly still being initialised.  ready is closed
// once initialisation finishes; db or err is set before that and never
// changes afterwards.  Failed entries are removed from the pool before ready
// is closed, so an entry found in the pool with ready closed always has db.
type poolEntry struct {
	ready chan struct{}
	db    *sql.DB
	err   error

	// refs, lastUsed, login, checkedAt and rechecking are guarded by pool.mu.
	refs     int
	lastUsed time.Time
	// login is the result of the last successful login check.
	login loginState
	// checkedAt is when the login was last checked, successfully or not.
	checkedAt time.Time
	// rechecking is set while one caller re-runs the login check.
	rechecking bool
}

func newPool() *pool {
	p := &pool{
		idleTTL:       defaultPoolIdleTTL,
		now:           time.Now,
		checkInterval: defaultLoginCheckInterval,
		entries:       make(map[poolKey]*poolEntry),
		latest:        make(map[poolKey]*poolEntry),
	}
	if _, err := rand.Read(p.secret[:]); err != nil {
		// crypto/rand.Read does not fail on supported platforms.
		panic(fmt.Sprintf("sqlserver: generate pool key secret: %v", err))
	}
	return p
}

// key returns the pool key of target for the resolved creds.
func (p *pool) key(m Manager, target collectormetadata.DatabaseTarget, creds Credential) poolKey {
	mac := hmac.New(sha256.New, p.secret[:])
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(creds.Username)))
	mac.Write(n[:])
	mac.Write([]byte(creds.Username))
	mac.Write([]byte(creds.Password))
	key := m.targetKey(target)
	key.credentialHash = hex.EncodeToString(mac.Sum(nil))
	return key
}

// targetKey returns the pool key of target without the credential hash.
func (m Manager) targetKey(target collectormetadata.DatabaseTarget) poolKey {
	return poolKey{
		driver:                 m.driver(),
		host:                   target.Host,
		port:                   target.Port,
		database:               target.DatabaseName,
		credentialRef:          target.CredentialRef,
		application:            m.Application,
		encrypt:                m.Encrypt,
		trustServerCertificate: m.TrustServerCertificate,
		dialTimeout:            m.DialTimeout,
	}
}

// withoutCredential returns key with the credential hash cleared: the key of
// its target in pool.latest.
func withoutCredential(key poolKey) poolKey {
	key.credentialHash = ""
	return key
}

// sysadmin returns the login check result of the most recently used
// initialised entry for target key want; ok is false without one, or when
// its login is unknown.
func (p *pool) sysadmin(want poolKey) (sysadmin, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry := p.latest[want]
	if entry == nil || !entry.login.known {
		return false, false
	}
	return entry.login.elevated, true
}

// useLocked records a new reference to the initialised entry for key.
func (p *pool) useLocked(key poolKey, entry *poolEntry) {
	entry.refs++
	entry.lastUsed = p.now()
	p.latest[withoutCredential(key)] = entry
}

// removeLocked deletes entry, stored under key, from the pool.
func (p *pool) removeLocked(key poolKey, entry *poolEntry) {
	if p.entries[key] == entry {
		delete(p.entries, key)
	}
	if target := withoutCredential(key); p.latest[target] == entry {
		delete(p.latest, target)
	}
}

// claimRecheckLocked reports whether the caller should re-run entry's login
// check: the last check is at least checkInterval old and no other caller is
// already re-running it.  A true result marks the re-check as in progress.
func (p *pool) claimRecheckLocked(entry *poolEntry) bool {
	if p.checkInterval <= 0 || entry.rechecking || p.now().Sub(entry.checkedAt) < p.checkInterval {
		return false
	}
	entry.rechecking = true
	return true
}

// recheck re-runs entry's login check with recheck, without holding p.mu.
// A failed check keeps the last result; either way the next one is due a
// full checkInterval later, so a failing target is not re-checked on every
// Open.
func (p *pool) recheck(ctx context.Context, entry *poolEntry, previous loginState, recheck func(context.Context, *sql.DB, loginState) (loginState, error)) {
	state, err := recheck(ctx, entry.db, previous)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		entry.login = state
	}
	entry.checkedAt = p.now()
	entry.rechecking = false
}

// acquire returns the handle for key, creating it with create if needed.
// create runs without holding p.mu so a slow target does not block Opens for
// other keys; concurrent callers for the same key wait for the single
// in-flight creation instead of starting their own.  When the entry's login
// check is due, the one caller that claims it re-runs it with recheck, also
// without holding p.mu, before returning.
func (p *pool) acquire(ctx context.Context, key poolKey, create func(context.Context) (*sql.DB, loginState, error), recheck func(context.Context, *sql.DB, loginState) (loginState, error)) (*sql.DB, func(), error) {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, nil, ErrManagerClosed
		}
		stale := p.sweepLocked()
		entry, ok := p.entries[key]
		if !ok {
			entry = &poolEntry{ready: make(chan struct{})}
			p.entries[key] = entry
			p.mu.Unlock()
			closeHandles(stale)
			return p.initialize(ctx, key, entry, create)
		}
		select {
		case <-entry.ready:
			p.useLocked(key, entry)
			due := p.claimRecheckLocked(entry)
			previous := entry.login
			p.mu.Unlock()
			closeHandles(stale)
			if due {
				p.recheck(ctx, entry, previous, recheck)
			}
			return entry.db, p.releaser(entry), nil
		default:
		}
		p.mu.Unlock()
		closeHandles(stale)
		select {
		case <-entry.ready:
			if entry.err != nil {
				return nil, nil, entry.err
			}
			// Loop to take a reference under the lock; the entry may have
			// been removed by Close in the meantime.
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
}

// initialize creates entry's handle with create and publishes the result.
func (p *pool) initialize(ctx context.Context, key poolKey, entry *poolEntry, create func(context.Context) (*sql.DB, loginState, error)) (*sql.DB, func(), error) {
	db, login, err := create(ctx)
	p.mu.Lock()
	if err == nil && p.closed {
		_ = db.Close()
		db, err = nil, ErrManagerClosed
	}
	if err != nil {
		p.removeLocked(key, entry)
		entry.err = err
		close(entry.ready)
		p.mu.Unlock()
		return nil, nil, err
	}
	entry.db = db
	entry.login = login
	entry.checkedAt = p.now()
	p.useLocked(key, entry)
	close(entry.ready)
	p.mu.Unlock()
	return db, p.releaser(entry), nil
}

// releaser returns an idempotent cleanup that drops one reference to entry.
func (p *pool) releaser(entry *poolEntry) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			entry.refs--
			entry.lastUsed = p.now()
			p.mu.Unlock()
		})
	}
}

// sweepLocked removes initialised entries that have no outstanding references
// and have been idle for at least idleTTL, returning their handles for the
// caller to close after releasing p.mu.
func (p *pool) sweepLocked() []*sql.DB {
	if p.idleTTL <= 0 {
		return nil
	}
	now := p.now()
	var stale []*sql.DB
	for key, entry := range p.entries {
		if entry.db != nil && entry.refs == 0 && now.Sub(entry.lastUsed) >= p.idleTTL {
			p.removeLocked(key, entry)
			stale = append(stale, entry.db)
		}
	}
	return stale
}

func (p *pool) close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	var handles []*sql.DB
	for key, entry := range p.entries {
		// Entries still initialising are closed by their initialiser once it
		// observes p.closed.
		if entry.db != nil {
			handles = append(handles, entry.db)
		}
		p.removeLocked(key, entry)
	}
	p.mu.Unlock()
	var errs []error
	for _, db := range handles {
		if err := db.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func closeHandles(handles []*sql.DB) {
	for _, db := range handles {
		_ = db.Close()
	}
}
