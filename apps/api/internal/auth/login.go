package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/prometheus/client_golang/prometheus"

	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/password"
	"heartbeat/apps/api/internal/store"
	"heartbeat/apps/api/internal/users"
	"heartbeat/internal/telemetry"
)

var tracer = telemetry.Tracer("heartbeat/api/auth")

// MetricLoginAttempts counts logins by outcome: succeeded, failed (wrong
// credentials or a disabled user) and rate_limited (refused unchecked).
const MetricLoginAttempts = "heartbeat_api_login_attempts_total"

// Values of the outcome label of MetricLoginAttempts.
const (
	outcomeSucceeded   = "succeeded"
	outcomeFailed      = "failed"
	outcomeRateLimited = "rate_limited"
)

// NewAttemptsCounter returns the login counter, with each outcome at 0 so
// that rate() and increase() see the first of each.
func NewAttemptsCounter() *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: MetricLoginAttempts,
		Help: "Logins, by outcome: succeeded, failed (wrong credentials or a disabled user), rate_limited (refused without checking).",
	}, []string{"outcome"})
	for _, outcome := range []string{outcomeSucceeded, outcomeFailed, outcomeRateLimited} {
		c.WithLabelValues(outcome)
	}
	return c
}

// Why a login failed, in its audit record. The client is told none of
// them: each gets the same answer.
const (
	ReasonUnknownEmail   = "unknown_email"
	ReasonNoPassword     = "no_password"
	ReasonWrongPassword  = "wrong_password"
	ReasonDisabled       = "disabled"
	ReasonUnreadableHash = "unreadable_hash"
	// ReasonChangedMeanwhile is a login whose password matched, but whose
	// user's password changed, or who was disabled, before it could be
	// recorded: the change wins.
	ReasonChangedMeanwhile = "changed_meanwhile"
)

// ActionSetPasswordFailed is a refused change of a user's own password: the
// current password given was wrong. Its record says so, never the password.
const ActionSetPasswordFailed = "set_password_failed"

// statusActive is the users.status of a user who may log in.
const statusActive = "active"

// ErrInvalidCredentials is a failed login: a wrong password, an unknown
// email or a disabled user, which the client is not told apart.
var ErrInvalidCredentials = errors.New("auth: wrong email or password")

// ErrWrongPassword is a change of a user's own password whose current
// password is wrong.
var ErrWrongPassword = errors.New("auth: the current password is wrong")

// errChangedMeanwhile is a login that could not be recorded: the user's
// password changed, or they were disabled, since the login checked it.
var errChangedMeanwhile = errors.New("auth: the user changed during the login")

// InvalidError is a login request that cannot be checked, such as an email
// that is not an address.
type InvalidError struct{ reason string }

func (e *InvalidError) Error() string { return e.reason }

// RateLimitedError is a login refused, unchecked, because of earlier
// failures.
type RateLimitedError struct {
	// RetryAfter is how long until the limit resets.
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("auth: too many failed logins; retry in %s", e.RetryAfter.Round(time.Second))
}

// DB is PostgreSQL: a pool, which runs queries and begins transactions.
type DB interface {
	store.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Client is who sent a request.
type Client struct {
	// Address is the client's IP address.
	Address netip.Addr
	// RequestID identifies the request in audit records.
	RequestID string
}

// User is a logged-in user.
type User struct {
	ID          string
	Email       string
	DisplayName string
	Roles       []string
}

// Principal is who a request with a session comes from.
type Principal struct {
	Session Session
	User    User
}

// Service logs users in and out. It is safe for concurrent use.
type Service struct {
	db       DB
	hasher   *password.Hasher
	sessions *Sessions
	limits   *Limits
	attempts *prometheus.CounterVec
	logger   *slog.Logger
	// decoy returns a hash of a random password, checked for an email with
	// no hash, so that a login takes as long whether the account exists or
	// not.
	decoy func() (string, error)
}

// NewService returns a Service. A nil attempts counts nothing; a nil logger
// discards logs.
func NewService(db DB, hasher *password.Hasher, sessions *Sessions, limits *Limits, attempts *prometheus.CounterVec, logger *slog.Logger) *Service {
	if attempts == nil {
		attempts = NewAttemptsCounter()
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	s := &Service{db: db, hasher: hasher, sessions: sessions, limits: limits, attempts: attempts, logger: logger}
	s.decoy = sync.OnceValues(func() (string, error) {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return "", err
		}
		return hasher.Hash(context.Background(), base64.RawStdEncoding.EncodeToString(raw))
	})
	return s
}

// ExpiresAt is when session ends however active it is.
func (s *Service) ExpiresAt(session Session) time.Time { return s.sessions.ExpiresAt(session) }

// Login checks email and secret, and starts a session: it returns the user
// and the session's token. A wrong password, an unknown email and a
// disabled user are all [ErrInvalidCredentials], take as long, and count
// against the limits; past them a login is a [*RateLimitedError], refused
// unchecked. The limits count a login before its password is checked, so
// concurrent logins cannot pass them; one whose password could not be
// checked, or matched but could not be recorded, as when PostgreSQL does
// not answer, does not count. One whose password matched but whose user's
// password changed, or who was disabled, before it was recorded fails and
// counts, as a disabled user's does. A request it
// cannot check is an [*InvalidError]. A hash made with older parameters is
// replaced at a successful login.
//
// A successful login writes its audit record, the user's last login time
// and any new hash in one transaction, and creates the session in Redis
// before that commits; if the commit fails, the session is deleted again,
// and its token was never returned. A failed login is audited too.
func (s *Service) Login(ctx context.Context, email, secret string, client Client) (Principal, string, error) {
	email, err := users.NormalizeEmail(email)
	if err != nil {
		return Principal{}, "", &InvalidError{reason: err.Error()}
	}
	if secret == "" || len(secret) > password.MaxBytes || !utf8.ValidString(secret) {
		return Principal{}, "", &InvalidError{reason: fmt.Sprintf("password: want 1 to %d bytes of UTF-8", password.MaxBytes)}
	}
	wait, err := s.limits.Begin(ctx, email, client.Address)
	if err != nil {
		return Principal{}, "", err
	}
	if wait > 0 {
		s.attempts.WithLabelValues(outcomeRateLimited).Inc()
		return Principal{}, "", &RateLimitedError{RetryAfter: wait}
	}

	user, err := store.New(s.db).GetLoginUser(ctx, email)
	known := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, "", s.unchecked(ctx, email, client, fmt.Errorf("look up the user: %w", err))
	}
	hash := user.PasswordHash.String
	if !known || !user.PasswordHash.Valid {
		if hash, err = s.decoy(); err != nil {
			return Principal{}, "", s.unchecked(ctx, email, client, err)
		}
	}
	match, rehash, err := s.verify(ctx, secret, hash)
	malformed := errors.Is(err, password.ErrMalformedHash)
	if err != nil && !malformed {
		return Principal{}, "", s.unchecked(ctx, email, client, err)
	}

	var reason string
	switch {
	case !known:
		reason = ReasonUnknownEmail
	case !user.PasswordHash.Valid:
		reason = ReasonNoPassword
	case malformed:
		s.logger.ErrorContext(ctx, "auth: the user's password hash cannot be read; set a new password", "user_id", user.ID.String())
		reason = ReasonUnreadableHash
	case !match:
		reason = ReasonWrongPassword
	// After the password, so a disabled user's login takes as long.
	case user.Status != statusActive:
		reason = ReasonDisabled
	}
	if reason != "" {
		return Principal{}, "", s.fail(ctx, email, user.ID, reason, client)
	}
	principal, token, err := s.succeed(ctx, email, user, secret, rehash, client)
	if errors.Is(err, errChangedMeanwhile) {
		return Principal{}, "", s.fail(ctx, email, user.ID, ReasonChangedMeanwhile, client)
	}
	if err != nil {
		// The password matched: whatever stopped the login, it did not
		// fail, so an outage cannot lock the account.
		return Principal{}, "", s.unchecked(ctx, email, client, err)
	}
	return principal, token, nil
}

// verify checks secret against hash in a span of its own: it is most of a
// login's time.
func (s *Service) verify(ctx context.Context, secret, hash string) (bool, bool, error) {
	ctx, span := tracer.Start(ctx, "password.verify")
	defer span.End()
	return s.hasher.Verify(ctx, secret, hash)
}

// unchecked takes back the limits' count of a login that did not fail but
// was not completed: its password could not be checked, or it matched and
// the login could not be recorded. It returns err. A login whose password
// was checked and did not match keeps its count.
func (s *Service) unchecked(ctx context.Context, email string, client Client, err error) error {
	if err := s.limits.Unchecked(context.WithoutCancel(ctx), email, client.Address); err != nil {
		s.logger.WarnContext(ctx, "auth: take back the count of a login that was not checked", "error", err)
	}
	return err
}

// fail audits a failed login, which the limits counted before it was
// checked, and returns ErrInvalidCredentials.
func (s *Service) fail(ctx context.Context, email string, userID pgtype.UUID, reason string, client Client) error {
	attempt, err := audit.NewID()
	if err != nil {
		return err
	}
	record := failedLogin{Email: email, Reason: reason}
	if userID.Valid {
		record.UserID = userID.String()
	}
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		_, err := audit.Write(ctx, store.New(tx), audit.Change{
			RequestID:  client.RequestID,
			Actor:      audit.ClientActor(client.Address.String()),
			Action:     audit.ActionLoginFailed,
			EntityType: audit.EntitySession,
			EntityID:   attempt.String(),
			After:      record,
		})
		return err
	})
	if err != nil {
		return fmt.Errorf("audit the failed login: %w", err)
	}
	s.attempts.WithLabelValues(outcomeFailed).Inc()
	return ErrInvalidCredentials
}

// succeed records a login for email whose password matched and starts its
// session.
func (s *Service) succeed(ctx context.Context, email string, login store.GetLoginUserRow, secret string, rehash bool, client Client) (Principal, string, error) {
	var newHash string
	if rehash {
		var err error
		hashCtx, span := tracer.Start(ctx, "password.hash")
		newHash, err = s.hasher.Hash(hashCtx, secret)
		span.End()
		if err != nil {
			// The login stands; the next one tries again.
			s.logger.WarnContext(ctx, "auth: upgrade the password hash", "user_id", login.ID.String(), "error", err)
			newHash = ""
		}
	}

	var (
		principal Principal
		token     string
		created   bool
		upgraded  bool
	)
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		// Only while the user still has the hash this login checked and is
		// active: a password change or a disabling that holds the user's
		// row, or committed since, ends their sessions, and this login must
		// not start one after it.
		n, err := q.RecordLogin(ctx, store.RecordLoginParams{ID: login.ID, PasswordHash: login.PasswordHash})
		if err != nil {
			return fmt.Errorf("record the login: %w", err)
		}
		if n == 0 {
			return errChangedMeanwhile
		}
		if newHash != "" {
			n, err := q.UpgradePasswordHash(ctx, store.UpgradePasswordHashParams{
				ID: login.ID, OldHash: login.PasswordHash, NewHash: pgtype.Text{String: newHash, Valid: true},
			})
			if err != nil {
				return fmt.Errorf("upgrade the password hash: %w", err)
			}
			upgraded = n == 1
		}
		row, err := q.GetSessionUser(ctx, login.ID)
		if err != nil {
			return fmt.Errorf("read the user: %w", err)
		}
		user := User{ID: row.ID.String(), Email: row.Email, DisplayName: row.DisplayName, Roles: row.Roles}
		session, t, err := s.sessions.Create(ctx, user.ID, client.Address.String())
		if err != nil {
			return err
		}
		principal, token, created = Principal{Session: session, User: user}, t, true
		_, err = audit.Write(ctx, q, audit.Change{
			RequestID:  client.RequestID,
			Actor:      audit.UserActor(user.ID, user.Email),
			Action:     audit.ActionLogin,
			EntityType: audit.EntitySession,
			EntityID:   session.ID,
			After:      s.record(principal),
		})
		return err
	})
	if err != nil {
		if created {
			if err := s.sessions.Delete(context.WithoutCancel(ctx), principal.Session); err != nil {
				s.logger.WarnContext(ctx, "auth: delete the session of a login that failed", "session_id", principal.Session.ID, "error", err)
			}
		}
		return Principal{}, "", err
	}
	if upgraded {
		s.logger.InfoContext(ctx, "auth: password hash upgraded to the current parameters", "user_id", principal.User.ID)
	}
	if err := s.limits.Succeeded(context.WithoutCancel(ctx), email, client.Address); err != nil {
		s.logger.WarnContext(ctx, "auth: clear the account's failed logins", "user_id", principal.User.ID, "error", err)
	}
	s.attempts.WithLabelValues(outcomeSucceeded).Inc()
	return principal, token, nil
}

// Authenticate returns who token's session belongs to, or [ErrNoSession].
// It reads the user on every call, so a disabled user's sessions stop
// working at once and a role change applies to the next request; a
// disabled user's session is deleted.
func (s *Service) Authenticate(ctx context.Context, token string) (Principal, error) {
	session, err := s.sessions.Lookup(ctx, token)
	if err != nil {
		return Principal{}, err
	}
	var id pgtype.UUID
	if err := id.Scan(session.UserID); err != nil {
		return Principal{}, fmt.Errorf("session %s: user ID: %w", session.ID, err)
	}
	row, err := store.New(s.db).GetSessionUser(ctx, id)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, fmt.Errorf("read the session's user: %w", err)
	}
	if err != nil || row.Status != statusActive {
		if err := s.sessions.Delete(ctx, session); err != nil {
			s.logger.WarnContext(ctx, "auth: delete a disabled user's session", "session_id", session.ID, "error", err)
		}
		return Principal{}, ErrNoSession
	}
	return Principal{
		Session: session,
		User:    User{ID: row.ID.String(), Email: row.Email, DisplayName: row.DisplayName, Roles: row.Roles},
	}, nil
}

// Logout ends p's session and audits it, in one transaction: if the session
// cannot be deleted, nothing is recorded.
func (s *Service) Logout(ctx context.Context, p Principal, client Client) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		_, err := audit.Write(ctx, store.New(tx), audit.Change{
			RequestID:  client.RequestID,
			Actor:      audit.UserActor(p.User.ID, p.User.Email),
			Action:     audit.ActionLogout,
			EntityType: audit.EntitySession,
			EntityID:   p.Session.ID,
			Before:     s.record(p),
		})
		if err != nil {
			return err
		}
		return s.sessions.Delete(ctx, p.Session)
	})
}

// sessionRecord is a session as audit records show it: its ID, never its
// token.
type sessionRecord struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Email     string    `json:"email"`
	Address   string    `json:"address"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Service) record(p Principal) sessionRecord {
	return sessionRecord{
		ID: p.Session.ID, UserID: p.User.ID, Email: p.User.Email, Address: p.Session.Address,
		CreatedAt: p.Session.Created, ExpiresAt: s.sessions.ExpiresAt(p.Session),
	}
}

// failedLogin is a refused login as its audit record shows it: the email
// tried and why, never the password.
type failedLogin struct {
	Email  string `json:"email"`
	UserID string `json:"user_id,omitempty"`
	Reason string `json:"reason"`
}

// ChangePassword sets a new password for p's user, given their current one,
// and ends every session of theirs, p's included. The current password is
// checked as a login's is, against the same limits: a wrong one is
// [ErrWrongPassword], counts as a failure and is audited; past the limits
// the change is a [*RateLimitedError], refused unchecked. A password that
// is not acceptable is an [*InvalidError].
func (s *Service) ChangePassword(ctx context.Context, p Principal, current, next string, client Client) error {
	if current == "" || len(current) > password.MaxBytes || !utf8.ValidString(current) {
		return &InvalidError{reason: fmt.Sprintf("current_password: want 1 to %d bytes of UTF-8", password.MaxBytes)}
	}
	if err := password.Check(next); err != nil {
		return &InvalidError{reason: "new_" + err.Error()}
	}
	email := p.User.Email
	wait, err := s.limits.Begin(ctx, email, client.Address)
	if err != nil {
		return err
	}
	if wait > 0 {
		return &RateLimitedError{RetryAfter: wait}
	}
	id, err := uuid.Parse(p.User.ID)
	if err != nil {
		return s.unchecked(ctx, email, client, fmt.Errorf("session %s: user ID: %w", p.Session.ID, err))
	}
	// By the session's user ID: the email may belong to someone else by now.
	hash, err := store.New(s.db).GetPasswordHash(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if err != nil {
		return s.unchecked(ctx, email, client, fmt.Errorf("look up the user: %w", err))
	}
	var match bool
	if hash.Valid {
		match, _, err = s.verify(ctx, current, hash.String)
		if err != nil && !errors.Is(err, password.ErrMalformedHash) {
			return s.unchecked(ctx, email, client, err)
		}
	}
	actor := audit.UserActor(p.User.ID, email)
	if !match {
		return s.wrongPassword(ctx, actor, p.User.ID, email, client)
	}
	err = users.SetPassword(ctx, s.db, s.hasher, s.sessions,
		users.Caller{Actor: actor, RequestID: client.RequestID},
		users.PasswordChange{UserID: id, Password: next, IfHash: hash.String})
	if errors.Is(err, users.ErrStalePassword) {
		// Changed since it was checked: the current password given is
		// not the current one any more.
		return s.wrongPassword(ctx, actor, p.User.ID, email, client)
	}
	if err != nil {
		return s.unchecked(ctx, email, client, err)
	}
	if err := s.limits.Succeeded(context.WithoutCancel(ctx), email, client.Address); err != nil {
		s.logger.WarnContext(ctx, "auth: clear the account's failed logins", "user_id", p.User.ID, "error", err)
	}
	return nil
}

// wrongPassword audits a refused change of a user's own password, which the
// limits counted as a failure, and returns ErrWrongPassword.
func (s *Service) wrongPassword(ctx context.Context, actor audit.Actor, userID, email string, client Client) error {
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		_, err := audit.Write(ctx, store.New(tx), audit.Change{
			RequestID:  client.RequestID,
			Actor:      actor,
			Action:     ActionSetPasswordFailed,
			EntityType: audit.EntityUser,
			EntityID:   userID,
			After:      failedPasswordChange{ID: userID, Email: email, Reason: ReasonWrongPassword},
		})
		return err
	})
	if err != nil {
		return fmt.Errorf("audit the refused password change: %w", err)
	}
	return ErrWrongPassword
}

// failedPasswordChange is a refused change of a user's own password as its
// audit record shows it: never the password tried.
type failedPasswordChange struct {
	ID     string `json:"id"`
	Email  string `json:"email"`
	Reason string `json:"reason"`
}
