package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"atrium/apps/api/internal/audit"
)

// Session lifetimes (ADR 0013 §8): a session ends after IdleTimeout without
// a request, or MaxAge after login, whichever comes first.
const (
	DefaultIdleTimeout = 8 * time.Hour
	DefaultMaxAge      = 7 * 24 * time.Hour
)

// touchInterval is how often a session's last-seen time is written: at most
// once a minute, not on every request, so the idle timeout is exact to a
// minute.
const touchInterval = time.Minute

// Redis key prefixes. A session's key ends in the SHA-256 of its token, so
// Redis never holds a token, and a user's index lists those hashes.
const (
	sessionKeyPrefix = "atrium:session:"
	userKeyPrefix    = "atrium:user-sessions:"
)

// tokenBytes is the size of a session token: 256 random bits.
const tokenBytes = 32

// ErrNoSession reports a token that names no live session.
var ErrNoSession = errors.New("auth: no session")

// Session is one login, as Redis keeps it under the hash of its token.
type Session struct {
	// ID identifies the session in audit records and logs. It is not the
	// token and cannot be used to log in.
	ID string `json:"id"`
	// UserID is the user who logged in.
	UserID string `json:"user_id"`
	// Address is the client address the login came from.
	Address string `json:"address"`
	// Created is the time of the login; LastSeen that of the latest
	// request, to a minute.
	Created  time.Time `json:"created"`
	LastSeen time.Time `json:"last_seen"`

	// key is the hex SHA-256 of the token.
	key string
}

// Sessions keeps sessions in Redis. Each one lives under the SHA-256 of its
// token and expires by itself at the earlier of its idle and absolute
// deadlines; a set per user lists their sessions, so all of them can be
// ended together.
type Sessions struct {
	Redis goredis.Cmdable
	// IdleTimeout and MaxAge are the session lifetimes.
	IdleTimeout, MaxAge time.Duration
	// Now returns the time; nil means time.Now.
	Now func() time.Time
}

func (s *Sessions) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// ExpiresAt is when session ends however active it is.
func (s *Sessions) ExpiresAt(session Session) time.Time {
	return session.Created.Add(s.MaxAge)
}

// deadline is when session ends unless a request comes first.
func (s *Sessions) deadline(session Session) time.Time {
	idle, expires := session.LastSeen.Add(s.IdleTimeout), s.ExpiresAt(session)
	if idle.Before(expires) {
		return idle
	}
	return expires
}

// Create starts a session for userID, logged in from address, and returns
// it with its token: 256 random bits, base64url-encoded, the cookie's value.
// The token is returned once and never stored.
func (s *Sessions) Create(ctx context.Context, userID, address string) (Session, string, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return Session{}, "", fmt.Errorf("session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	id, err := audit.NewID()
	if err != nil {
		return Session{}, "", err
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	session := Session{ID: id.String(), UserID: userID, Address: address, Created: now, LastSeen: now, key: keyOf(raw)}
	value, err := json.Marshal(session)
	if err != nil {
		return Session{}, "", err
	}
	_, err = s.Redis.TxPipelined(ctx, func(pipe goredis.Pipeliner) error {
		pipe.SetArgs(ctx, sessionKeyPrefix+session.key, value, goredis.SetArgs{Mode: "NX", ExpireAt: s.deadline(session)})
		pipe.SAdd(ctx, userKeyPrefix+userID, session.key)
		// Every session in the set ends within MaxAge from now.
		pipe.Expire(ctx, userKeyPrefix+userID, s.MaxAge)
		return nil
	})
	if err != nil {
		return Session{}, "", fmt.Errorf("create the session: %w", err)
	}
	return session, token, nil
}

// Lookup returns the live session token names, or [ErrNoSession]. It
// records the request as the session's latest, at most once a minute,
// which moves its idle deadline.
func (s *Sessions) Lookup(ctx context.Context, token string) (Session, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != tokenBytes {
		return Session{}, ErrNoSession
	}
	key := keyOf(raw)
	value, err := s.Redis.Get(ctx, sessionKeyPrefix+key).Bytes()
	if errors.Is(err, goredis.Nil) {
		return Session{}, ErrNoSession
	}
	if err != nil {
		return Session{}, fmt.Errorf("look up the session: %w", err)
	}
	var session Session
	if err := json.Unmarshal(value, &session); err != nil {
		return Session{}, fmt.Errorf("session %s: %w", key[:8], err)
	}
	session.key = key
	now := s.now()
	if !now.Before(s.deadline(session)) {
		return Session{}, ErrNoSession
	}
	if now.Sub(session.LastSeen) < touchInterval {
		return session, nil
	}
	session.LastSeen = now.UTC().Truncate(time.Millisecond)
	if value, err = json.Marshal(session); err != nil {
		return Session{}, err
	}
	// XX: a session deleted meanwhile, as at logout, stays deleted.
	err = s.Redis.SetArgs(ctx, sessionKeyPrefix+key, value, goredis.SetArgs{Mode: "XX", ExpireAt: s.deadline(session)}).Err()
	if errors.Is(err, goredis.Nil) {
		return Session{}, ErrNoSession
	}
	if err != nil {
		return Session{}, fmt.Errorf("touch the session: %w", err)
	}
	return session, nil
}

// Delete ends session.
func (s *Sessions) Delete(ctx context.Context, session Session) error {
	_, err := s.Redis.TxPipelined(ctx, func(pipe goredis.Pipeliner) error {
		pipe.Del(ctx, sessionKeyPrefix+session.key)
		pipe.SRem(ctx, userKeyPrefix+session.UserID, session.key)
		return nil
	})
	if err != nil {
		return fmt.Errorf("delete the session: %w", err)
	}
	return nil
}

// DeleteUser ends every session of userID, as disabling a user or changing
// their password does. A session created while it runs may survive it.
func (s *Sessions) DeleteUser(ctx context.Context, userID string) error {
	keys, err := s.Redis.SMembers(ctx, userKeyPrefix+userID).Result()
	if err != nil {
		return fmt.Errorf("list the user's sessions: %w", err)
	}
	if len(keys) == 0 {
		return nil
	}
	_, err = s.Redis.TxPipelined(ctx, func(pipe goredis.Pipeliner) error {
		for _, key := range keys {
			pipe.Del(ctx, sessionKeyPrefix+key)
		}
		pipe.SRem(ctx, userKeyPrefix+userID, toAny(keys)...)
		return nil
	})
	if err != nil {
		return fmt.Errorf("delete the user's sessions: %w", err)
	}
	return nil
}

func keyOf(token []byte) string {
	sum := sha256.Sum256(token)
	return hex.EncodeToString(sum[:])
}

func toAny(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}
