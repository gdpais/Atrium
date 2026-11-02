package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Failed-login limits (ADR 0013 §7). Past either, a login is refused
// without checking the password until the window that began with the first
// failure ends. The account limit stops guessing one user's password from
// many addresses, at the cost of letting anyone lock a user out for the
// window; the address limit stops one client trying many accounts, and is
// higher because an office can share an address.
const (
	AccountFailures = 10
	AddressFailures = 50
	FailureWindow   = 15 * time.Minute
)

// Redis key prefixes of the failure counters. An account's key ends in the
// SHA-256 of its email, so Redis holds no email.
const (
	accountKeyPrefix = "heartbeat:login-failures:account:"
	addressKeyPrefix = "heartbeat:login-failures:address:"
)

// Limits counts failed logins per account and per client address in Redis.
// The counts are checked before a login and raised after it fails, so a
// burst of concurrent attempts can pass the limits by the few that were
// already running.
type Limits struct {
	Redis goredis.Cmdable
}

// Wait returns how long until a login for email from address may be tried,
// 0 when it may be tried now.
func (l *Limits) Wait(ctx context.Context, email string, address netip.Addr) (time.Duration, error) {
	var wait time.Duration
	for _, c := range counters(email, address) {
		count, err := l.Redis.Get(ctx, c.key).Int()
		if errors.Is(err, goredis.Nil) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("read the failed logins: %w", err)
		}
		if count < c.limit {
			continue
		}
		ttl, err := l.Redis.PTTL(ctx, c.key).Result()
		if err != nil {
			return 0, fmt.Errorf("read the failed logins: %w", err)
		}
		if ttl <= 0 {
			// Expired since the read, or, never expected, without a TTL.
			ttl = time.Second
		}
		wait = max(wait, ttl)
	}
	return wait, nil
}

// Failed counts a failed login for email from address.
func (l *Limits) Failed(ctx context.Context, email string, address netip.Addr) error {
	_, err := l.Redis.TxPipelined(ctx, func(pipe goredis.Pipeliner) error {
		for _, c := range counters(email, address) {
			pipe.Incr(ctx, c.key)
			// The window starts at the first failure; later ones keep it.
			pipe.ExpireNX(ctx, c.key, FailureWindow)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("count the failed login: %w", err)
	}
	return nil
}

// Succeeded clears email's failures. The address's stay, so that a client
// cannot reset its count by logging in to an account of its own.
func (l *Limits) Succeeded(ctx context.Context, email string) error {
	if err := l.Redis.Del(ctx, accountKey(email)).Err(); err != nil {
		return fmt.Errorf("clear the failed logins: %w", err)
	}
	return nil
}

type counter struct {
	key   string
	limit int
}

func counters(email string, address netip.Addr) []counter {
	return []counter{
		{accountKey(email), AccountFailures},
		{addressKeyPrefix + addressBlock(address), AddressFailures},
	}
}

func accountKey(email string) string {
	sum := sha256.Sum256([]byte(email))
	return accountKeyPrefix + hex.EncodeToString(sum[:])
}

// addressBlock is what an address is counted as: an IPv4 address, or the
// /64 an IPv6 address is in, since one client commonly has a whole /64.
func addressBlock(address netip.Addr) string {
	address = address.Unmap()
	if address.Is6() {
		return netip.PrefixFrom(address, 64).Masked().String()
	}
	return address.String()
}
