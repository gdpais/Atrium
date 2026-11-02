// Package redis connects the API to Redis, which runs as Valkey (ADR 0015):
// a go-redis client whose credential is resolved on every new connection.
//
// Redis holds transient state only (ADR 0013). The API must not depend on
// commands newer than Redis 7.2, the protocol Valkey speaks.
package redis

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"

	goredis "github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"

	"heartbeat/internal/credentials"
)

// Config says where Redis is and how to log in.
type Config struct {
	// Addr is the server's host:port.
	Addr string
	// TLS turns on TLS, verified against the system's CA certificates and
	// the host name in Addr.
	TLS bool
	// CredentialRef names the "username:password" credential the client
	// authenticates with (package credentials). An empty username
	// authenticates with the password alone, as Redis's requirepass and an
	// ElastiCache AUTH token expect; Valkey's ACL user for that is
	// "default".
	CredentialRef string
}

// Validate reports the first setting that cannot work.
func (c Config) Validate() error {
	host, port, err := net.SplitHostPort(c.Addr)
	if err != nil || host == "" {
		return fmt.Errorf("address %q: want host:port, such as valkey:6379", c.Addr)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("address %q: port must be 1-65535", c.Addr)
	}
	if c.CredentialRef == "" {
		return errors.New("no credential reference")
	}
	return nil
}

// New returns a client for cfg. It does not connect: the first command
// does, so the API starts while Redis is down and becomes ready once it is
// up. It fails only on settings that cannot work.
func New(cfg Config, resolver credentials.Resolver) (*goredis.Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	opts := &goredis.Options{
		Addr:                       cfg.Addr,
		CredentialsProviderContext: credentialsProvider(resolver, cfg.CredentialRef),
		// A command ends at its context's deadline, such as the readiness
		// timeout, rather than at ReadTimeout (3s) when that comes first.
		ContextTimeoutEnabled: true,
		// CLIENT MAINT_NOTIFICATIONS announces upgrades of Redis's own
		// managed offerings; Valkey does not have it, so don't send it.
		MaintNotificationsConfig: &maintnotifications.Config{Mode: maintnotifications.ModeDisabled},
	}
	if cfg.TLS {
		host, _, _ := net.SplitHostPort(cfg.Addr)
		opts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
	}
	client := goredis.NewClient(opts)
	client.AddHook(tracingHook{})
	return client, nil
}

// credentialsProvider returns the login of each new connection, read again
// every time, so a rotated credential is used by the next connection
// without a restart.
func credentialsProvider(resolver credentials.Resolver, ref string) func(context.Context) (string, string, error) {
	return func(ctx context.Context) (string, string, error) {
		creds, err := resolver.Resolve(ctx, ref)
		if err != nil {
			return "", "", err
		}
		return creds.Username, creds.Password, nil
	}
}

// Ping returns nil when client reaches Redis and is authenticated.
func Ping(ctx context.Context, client *goredis.Client) error {
	return client.Ping(ctx).Err()
}

// SetLogger sends go-redis's own log lines, which it writes to standard
// error as plain text by default, to logger as warnings. It applies to
// every client in the process.
func SetLogger(logger *slog.Logger) {
	goredis.SetLogger(slogAdapter{logger: logger})
}

type slogAdapter struct {
	logger *slog.Logger
}

func (a slogAdapter) Printf(ctx context.Context, format string, v ...any) {
	a.logger.WarnContext(ctx, "redis: "+strings.TrimSpace(fmt.Sprintf(format, v...)))
}
