//go:build integration

package tests

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"atrium/internal/testreap"
)

type postgresStack struct {
	Root    string
	Compose string
	Project string
	DBName  string
}

func newPostgresStack(t *testing.T, root string) *postgresStack {
	t.Helper()
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	s := &postgresStack{
		Root: root, Compose: filepath.Join(root, "infra/docker-compose.test.yml"),
		Project: "atrium-test-" + hex.EncodeToString(id[:]), DBName: "atrium_smoke",
	}
	// Remove the projects of test processes that died before their cleanup.
	testreap.ReapOnce(t)
	// Register before startup so partially created resources are cleaned up too.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		output, err := s.command(ctx, "down", "--volumes", "--timeout", "10").CombinedOutput()
		if err != nil {
			t.Errorf("cleanup project %s: %v\n%s", s.Project, err, output)
		}
	})
	t.Logf("isolated PostgreSQL project: %s", s.Project)
	s.run(t, "config", "--quiet")
	s.run(t, "up", "-d", "--wait", "--wait-timeout", "90", "postgres")
	return s
}

func (s *postgresStack) command(ctx context.Context, args ...string) *exec.Cmd {
	base := []string{"compose", "--env-file", os.DevNull, "--project-name", s.Project, "-f", s.Compose}
	cmd := exec.CommandContext(ctx, "docker", append(base, args...)...)
	cmd.Dir = s.Root
	// Keep the selected Docker daemon, but ignore developer Compose overrides.
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "COMPOSE_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	// Label the containers as this process's.
	cmd.Env = append(cmd.Env, testreap.ComposeEnv()...)
	return cmd
}

func (s *postgresStack) run(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	output, err := s.command(ctx, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("project %s: compose %s: %v\n%s", s.Project, strings.Join(args, " "), err, output)
	}
	return string(output)
}

func (s *postgresStack) ResetDatabase(t *testing.T) {
	t.Helper()
	s.PSQL(t, "postgres", fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE);", s.DBName))
	s.PSQL(t, "postgres", fmt.Sprintf("CREATE DATABASE %s;", s.DBName))
}

func (s *postgresStack) ApplyMigration(t *testing.T, path string) string {
	t.Helper()
	return s.run(t, "exec", "-T", "postgres", "psql", "-U", "atrium", "-d", s.DBName,
		"-v", "ON_ERROR_STOP=1", "-f", path)
}

func (s *postgresStack) PSQL(t *testing.T, database, query string) string {
	t.Helper()
	return s.run(t, "exec", "-T", "postgres", "psql", "-U", "atrium", "-d", database,
		"-v", "ON_ERROR_STOP=1", "-c", query)
}

// PSQLResult runs query as PSQL does, but returns its output and error
// instead of failing the test, for a statement that should fail.
func (s *postgresStack) PSQLResult(t *testing.T, database, query string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	output, err := s.command(ctx, "exec", "-T", "postgres", "psql", "-U", "atrium", "-d", database,
		"-v", "ON_ERROR_STOP=1", "-c", query).CombinedOutput()
	return string(output), err
}
