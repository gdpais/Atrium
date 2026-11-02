//go:build integration

// Package testenv starts disposable PostgreSQL and Valkey containers for the
// API's integration tests (make test-integration).
//
// Each container has a random name (heartbeat-api-test-<id>), a random
// password, tmpfs storage and one port published on 127.0.0.1 only, at a
// random host port; it is removed when the test ends. Nothing here touches a
// developer database, the kind cluster or another test's containers.
package testenv

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"

	"heartbeat/apps/api/internal/redis"
	"heartbeat/apps/api/internal/store"
	"heartbeat/internal/credentials"
)

// ValkeyImage is the Valkey server the tests run: the newest major Amazon
// ElastiCache supports, pinned exactly (ADR 0015). The chart's Valkey moves
// with it.
const ValkeyImage = "valkey/valkey:9.1.3"

// Credential references the containers' logins are stored under.
const (
	PostgresRef = "secret/api-postgres"
	RedisRef    = "secret/api-redis"
)

// Resolver is a fixed set of credentials, by reference.
type Resolver map[string]credentials.Credential

// Resolve implements [credentials.Resolver].
func (r Resolver) Resolve(_ context.Context, ref string) (credentials.Credential, error) {
	creds, ok := r[ref]
	if !ok {
		return credentials.Credential{}, fmt.Errorf("missing credential for ref %s", ref)
	}
	return creds, nil
}

// Container is a running test container.
type Container struct {
	// Name is the container's name.
	Name string
	// Addr is the host:port its service is published on, on 127.0.0.1.
	Addr string
	// Credential is its login.
	Credential credentials.Credential
}

// Pause freezes the container's processes, as an unresponsive server.
func (c Container) Pause(t *testing.T) { docker(t, nil, "pause", c.Name) }

// Unpause resumes a paused container.
func (c Container) Unpause(t *testing.T) { docker(t, nil, "unpause", c.Name) }

// Exec runs a command in the container and returns its output.
func (c Container) Exec(t *testing.T, args ...string) string {
	t.Helper()
	return docker(t, nil, append([]string{"exec", c.Name}, args...)...)
}

// Postgres starts PostgreSQL, the image of the PostgreSQL integration
// fixture (infra/docker-compose.test.yml), so both test the same major. It
// returns the container and the API's settings for it, with an empty
// database and sslmode disable.
func Postgres(t *testing.T) (Container, store.Config) {
	t.Helper()
	password := randomHex(t)
	c := start(t, postgresImage(t), "5432/tcp",
		[]string{"POSTGRES_USER=heartbeat", "POSTGRES_DB=heartbeat", "POSTGRES_PASSWORD=" + password},
		"--tmpfs", "/var/lib/postgresql/data",
		"-e", "POSTGRES_USER", "-e", "POSTGRES_DB", "-e", "POSTGRES_PASSWORD",
		// Over TCP: the entrypoint's temporary server during initdb
		// listens on the Unix socket only.
		"--health-cmd", "pg_isready -h 127.0.0.1 -U heartbeat -d heartbeat",
		"--health-interval", "1s", "--health-timeout", "5s", "--health-retries", "90",
	)
	c.Credential = credentials.Credential{Username: "heartbeat", Password: password}
	waitHealthy(t, c.Name)
	host, port := splitAddr(t, c.Addr)
	return c, store.Config{
		Host: host, Port: port, Database: "heartbeat", SSLMode: "disable", CredentialRef: PostgresRef,
	}
}

// MigratedPostgres starts PostgreSQL as [Postgres] does, applies every
// migration, and returns a pool on it that is closed when the test ends.
func MigratedPostgres(t *testing.T) (Container, store.Config, *pgxpool.Pool) {
	t.Helper()
	c, cfg := Postgres(t)
	resolver := Resolver{PostgresRef: c.Credential}
	if _, err := store.MigrateUp(context.Background(), cfg, resolver, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	pool, err := store.NewPool(cfg, resolver)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return c, cfg, pool
}

// Valkey starts Valkey with a password (the "default" user's) and returns
// the container and the API's settings for it, without TLS.
func Valkey(t *testing.T) (Container, redis.Config) {
	t.Helper()
	password := randomHex(t)
	c := start(t, ValkeyImage, "6379/tcp",
		[]string{"VALKEY_EXTRA_FLAGS=--requirepass " + password + " --appendonly no"},
		"--tmpfs", "/data", "-e", "VALKEY_EXTRA_FLAGS",
		"--health-cmd", `valkey-cli --no-auth-warning -a "$(echo $VALKEY_EXTRA_FLAGS | cut -d' ' -f2)" ping | grep -q PONG`,
		"--health-interval", "1s", "--health-timeout", "5s", "--health-retries", "60",
	)
	c.Credential = credentials.Credential{Username: "default", Password: password}
	waitHealthy(t, c.Name)
	return c, redis.Config{Addr: c.Addr, CredentialRef: RedisRef}
}

// start runs image detached under a random name, publishing port on a
// random 127.0.0.1 port, and registers its removal. env holds secrets: it
// reaches docker through the environment, never the command line.
func start(t *testing.T, image, port string, env []string, args ...string) Container {
	t.Helper()
	name := "heartbeat-api-test-" + randomHex(t)[:12]
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		// A paused container cannot be removed by some runtimes.
		_ = exec.CommandContext(ctx, "docker", "unpause", name).Run()
		if out, err := exec.CommandContext(ctx, "docker", "rm", "-f", "-v", name).CombinedOutput(); err != nil {
			t.Errorf("remove container %s: %v\n%s", name, err, out)
		}
	})
	run := append([]string{"run", "-d", "--name", name, "-p", "127.0.0.1::" + strings.TrimSuffix(port, "/tcp")}, args...)
	docker(t, env, append(run, image)...)
	addr := publishedAddr(t, name, port)
	t.Logf("container %s (%s) on %s", name, image, addr)
	return Container{Name: name, Addr: addr}
}

// publishedAddr returns the host address of the container's port. Docker
// can report it a moment after `docker run` returns, more so when several
// containers start at once, so it is polled for up to 30 seconds.
func publishedAddr(t *testing.T, name, port string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		out := strings.TrimSpace(docker(t, nil, "port", name, port))
		// One line per binding; there is only the 127.0.0.1 one.
		addr, _, _ := strings.Cut(out, "\n")
		if _, _, err := net.SplitHostPort(addr); err == nil {
			return addr
		}
		if time.Now().After(deadline) {
			t.Fatalf("container %s publishes no %s after 30s (%q)\n%s", name, port, out, docker(t, nil, "logs", "--tail", "50", name))
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// waitHealthy waits up to two minutes for the container's health check.
func waitHealthy(t *testing.T, name string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		status := strings.TrimSpace(docker(t, nil, "inspect", "-f", "{{.State.Health.Status}}", name))
		if status == "healthy" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("container %s is %s after two minutes\n%s", name, status, docker(t, nil, "logs", "--tail", "50", name))
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// docker runs the docker CLI with extra environment variables and returns
// its standard output; it fails the test on an error.
func docker(t *testing.T, env []string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = append(os.Environ(), env...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", args[0], err, stderr.String())
	}
	return string(out)
}

// postgresImage returns the image of the postgres service in
// infra/docker-compose.test.yml.
func postgresImage(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "infra/docker-compose.test.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	image := fixture.Services["postgres"].Image
	if image == "" {
		t.Fatal("infra/docker-compose.test.yml has no postgres image")
	}
	return image
}

// repoRoot walks up from the working directory to the directory of go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the working directory")
		}
		dir = parent
	}
}

func splitAddr(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return host, n
}

func randomHex(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b[:])
}
