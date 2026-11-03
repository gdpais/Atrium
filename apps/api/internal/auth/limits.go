package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
// A login is counted as failed before its password is checked, and one that
// succeeds or is not checked to the end takes its count back
// ([Limits.Succeeded], [Limits.Unchecked]). So concurrent attempts cannot
// pass the limits: each takes its place in the counts before any of them is
// checked, and only those within the limits are checked.
type Limits struct {
	Redis goredis.Cmdable
}

// Begin counts a login for email from address as failed, before it is
// checked, and returns 0. If that takes either count past its limit, it
// takes the count back and returns how long until the window that began
// with the first failure ends: the login is refused unchecked, and counts
// for nothing.
func (l *Limits) Begin(ctx context.Context, email string, address netip.Addr) (time.Duration, error) {
	cs := counters(email, address)
	counts := make([]*goredis.IntCmd, len(cs))
	ttls := make([]*goredis.DurationCmd, len(cs))
	_, err := l.Redis.TxPipelined(ctx, func(pipe goredis.Pipeliner) error {
		for i, c := range cs {
			counts[i] = pipe.Incr(ctx, c.key)
			// The window starts at the first failure; later ones keep it.
			pipe.ExpireNX(ctx, c.key, FailureWindow)
			ttls[i] = pipe.PTTL(ctx, c.key)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("count the login: %w", err)
	}
	var wait time.Duration
	for i, c := range cs {
		if counts[i].Val() <= int64(c.limit) {
			continue
		}
		ttl := ttls[i].Val()
		if ttl <= 0 {
			// Never expected: ExpireNX has just set one.
			ttl = time.Second
		}
		wait = max(wait, ttl)
	}
	if wait > 0 {
		if err := l.takeBack(ctx, cs); err != nil {
			return 0, err
		}
	}
	return wait, nil
}

// Succeeded clears email's failures and takes the login's count back from
// address. The address's failures stay, so that a client cannot reset its
// count by logging in to an account of its own.
func (l *Limits) Succeeded(ctx context.Context, email string, address netip.Addr) error {
	_, err := l.Redis.TxPipelined(ctx, func(pipe goredis.Pipeliner) error {
		pipe.Del(ctx, accountKey(email))
		decrement(ctx, pipe, addressKey(address))
		return nil
	})
	if err != nil {
		return fmt.Errorf("clear the failed logins: %w", err)
	}
	return nil
}

// Unchecked takes back the count of a login that [Limits.Begin] counted
// but that was not checked to the end, as when PostgreSQL does not answer:
// it did not fail.
func (l *Limits) Unchecked(ctx context.Context, email string, address netip.Addr) error {
	return l.takeBack(ctx, counters(email, address))
}

// takeBack takes one back from each of cs.
func (l *Limits) takeBack(ctx context.Context, cs []counter) error {
	_, err := l.Redis.TxPipelined(ctx, func(pipe goredis.Pipeliner) error {
		for _, c := range cs {
			decrement(ctx, pipe, c.key)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("take back the login's count: %w", err)
	}
	return nil
}

// decrement takes one back from the count at key. A count that expired
// since it was raised comes back at -1: it gets a window too, so that it
// is not kept forever, and allows one more login in it, never one fewer.
func decrement(ctx context.Context, pipe goredis.Pipeliner, key string) {
	pipe.Decr(ctx, key)
	pipe.ExpireNX(ctx, key, FailureWindow)
}

type counter struct {
	key   string
	limit int
}

func counters(email string, address netip.Addr) []counter {
	return []counter{
		{accountKey(email), AccountFailures},
		{addressKey(address), AddressFailures},
	}
}

func accountKey(email string) string {
	sum := sha256.Sum256([]byte(email))
	return accountKeyPrefix + hex.EncodeToString(sum[:])
}

func addressKey(address netip.Addr) string {
	return addressKeyPrefix + addressBlock(address)
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
