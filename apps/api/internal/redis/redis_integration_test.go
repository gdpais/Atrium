//go:build integration

package redis_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"atrium/apps/api/internal/redis"
	"atrium/apps/api/internal/testenv"
	"atrium/internal/credentials"
)

func ping(t *testing.T, cfg redis.Config, resolver credentials.Resolver) error {
	t.Helper()
	client, err := redis.New(cfg, resolver)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return redis.Ping(ctx, client)
}

// The client authenticates to Valkey with the resolved credential, as the
// "default" user or with the password alone, and a wrong one fails.
func TestPingValkey(t *testing.T) {
	t.Parallel()
	valkey, cfg := testenv.Valkey(t)

	if err := ping(t, cfg, testenv.Resolver{testenv.RedisRef: valkey.Credential}); err != nil {
		t.Fatalf("Ping as default: %v", err)
	}
	passwordOnly := credentials.Credential{Password: valkey.Credential.Password}
	if err := ping(t, cfg, testenv.Resolver{testenv.RedisRef: passwordOnly}); err != nil {
		t.Fatalf("Ping with the password alone: %v", err)
	}
	wrong := credentials.Credential{Username: "default", Password: "wrong"}
	if err := ping(t, cfg, testenv.Resolver{testenv.RedisRef: wrong}); err == nil || !strings.Contains(err.Error(), "WRONGPASS") {
		t.Fatalf("Ping with a wrong password = %v, want WRONGPASS", err)
	}
	if err := ping(t, cfg, testenv.Resolver{}); err == nil || !strings.Contains(err.Error(), "missing credential") {
		t.Fatalf("Ping without a credential = %v, want missing credential", err)
	}
}
