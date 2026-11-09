package redis

import (
	"bytes"
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"atrium/internal/credentials"
	"atrium/internal/credentials/credentialstest"
)

func TestConfigValidate(t *testing.T) {
	for _, addr := range []string{"valkey:6379", "10.0.0.7:6379", "[::1]:6380", "master.atrium.abc123.euw1.cache.amazonaws.com:6379"} {
		if err := (Config{Addr: addr, CredentialRef: "secret/api-redis"}).Validate(); err != nil {
			t.Errorf("Validate(%q) = %v", addr, err)
		}
	}
	for _, tc := range []struct {
		cfg  Config
		want string
	}{
		{Config{Addr: "valkey", CredentialRef: "secret/api-redis"}, "want host:port"},
		{Config{Addr: ":6379", CredentialRef: "secret/api-redis"}, "want host:port"},
		{Config{Addr: "valkey:0", CredentialRef: "secret/api-redis"}, "port must be"},
		{Config{Addr: "valkey:redis", CredentialRef: "secret/api-redis"}, "port must be"},
		{Config{Addr: "redis://valkey:6379", CredentialRef: "secret/api-redis"}, "want host:port"},
		{Config{Addr: "valkey:6379"}, "credential"},
	} {
		err := tc.cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Validate(%+v) = %v, want an error about %q", tc.cfg, err, tc.want)
		}
		if _, err := New(tc.cfg, credentials.Env{}); err == nil {
			t.Errorf("New(%+v) accepted a config Validate rejects", tc.cfg)
		}
	}
}

func TestNewSetsTLS(t *testing.T) {
	client, err := New(Config{Addr: "master.example.cache.amazonaws.com:6379", TLS: true, CredentialRef: "secret/api-redis"}, credentials.Env{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	got := client.Options().TLSConfig
	if got == nil || got.InsecureSkipVerify || got.ServerName != "master.example.cache.amazonaws.com" || got.MinVersion != tls.VersionTLS12 {
		t.Fatalf("TLS config = %+v, want verification against the host name, TLS 1.2 or later", got)
	}

	plain, err := New(Config{Addr: "valkey:6379", CredentialRef: "secret/api-redis"}, credentials.Env{})
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if plain.Options().TLSConfig != nil {
		t.Fatal("TLS configured with TLS false")
	}
}

func TestCredentialsProviderReadsTheCredentialEveryTime(t *testing.T) {
	dir := t.TempDir()
	credentialstest.WriteSecretVolume(t, dir, map[string]string{
		"ATRIUM_CREDENTIAL_SECRET_API_REDIS": "default:first\n",
	})
	provider := credentialsProvider(credentials.Files{Dir: dir}, "secret/api-redis")
	if user, password, err := provider(context.Background()); err != nil || user != "default" || password != "first" {
		t.Fatalf("provider = %q, %q, %v", user, password, err)
	}
	credentialstest.WriteSecretVolume(t, dir, map[string]string{
		"ATRIUM_CREDENTIAL_SECRET_API_REDIS": ":token-only\n",
	})
	if user, password, err := provider(context.Background()); err != nil || user != "" || password != "token-only" {
		t.Fatalf("after rotation: provider = %q, %q, %v", user, password, err)
	}
	credentialstest.WriteSecretVolume(t, dir, map[string]string{})
	if _, _, err := provider(context.Background()); err == nil || !strings.Contains(err.Error(), "missing credential") {
		t.Fatalf("removed credential: err = %v", err)
	}
}

// New must not connect, and with nothing listening a PING fails promptly.
func TestPingFailsWhileRedisIsDown(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ATRIUM_CREDENTIAL_SECRET_API_REDIS", "default:unused")
	client, err := New(Config{Addr: addr, CredentialRef: "secret/api-redis"}, credentials.Env{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := Ping(ctx, client); err == nil || ctx.Err() != nil {
		t.Fatalf("Ping = %v (context %v), want a prompt connection error", err, ctx.Err())
	}
}

// go-redis's log lines become JSON warnings like the API's other logs.
func TestSlogAdapter(t *testing.T) {
	var buf bytes.Buffer
	slogAdapter{logger: slog.New(slog.NewJSONHandler(&buf, nil))}.Printf(context.Background(), "pool: %s\n", "conn has unread data")
	if !strings.Contains(buf.String(), `"level":"WARN"`) || !strings.Contains(buf.String(), `"msg":"redis: pool: conn has unread data"`) {
		t.Fatalf("log = %s", buf.String())
	}
}
