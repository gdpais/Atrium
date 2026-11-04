//go:build integration

package tests

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"heartbeat/internal/testreap"
)

// A Compose project left behind by a test process that is gone is removed,
// its network included: the fixture labels its containers with the process
// that started them.
func TestReapRemovesLeftoverComposeProject(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	s := &postgresStack{
		Root: root, Compose: filepath.Join(root, "infra/docker-compose.test.yml"),
		Project: "heartbeat-test-" + hex.EncodeToString(id[:]),
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = s.command(ctx, "down", "--volumes", "--timeout", "0").Run()
	})

	// Create the project as a test process that has exited would have.
	dead := exitedPID(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	create := s.command(ctx, "create", "postgres")
	create.Env = append(create.Env, testreap.PIDEnv+"="+strconv.Itoa(dead))
	if out, err := create.CombinedOutput(); err != nil {
		t.Fatalf("compose create: %v\n%s", err, out)
	}
	filter := "label=com.docker.compose.project=" + s.Project
	if got := dockerOutput(t, "ps", "-a", "--filter", filter, "--format", `{{.Label "`+testreap.PIDLabel+`"}}`); got != strconv.Itoa(dead) {
		t.Fatalf("project container labelled with process %q, want %d", got, dead)
	}
	if dockerOutput(t, "network", "ls", "-q", "--filter", filter) == "" {
		t.Fatal("compose create made no project network")
	}

	// Another test process can reap the project first, so only what is left
	// counts.
	if _, err := testreap.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	if got := dockerOutput(t, "ps", "-a", "-q", "--filter", filter); got != "" {
		t.Errorf("project %s still has containers %s", s.Project, got)
	}
	if got := dockerOutput(t, "network", "ls", "-q", "--filter", filter); got != "" {
		t.Errorf("project %s still has networks %s", s.Project, got)
	}
}

func dockerOutput(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		t.Fatalf("docker %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

// exitedPID returns the ID of a process that has exited and been waited for.
func exitedPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}
