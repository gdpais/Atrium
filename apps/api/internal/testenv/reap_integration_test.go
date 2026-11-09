//go:build integration

package testenv

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"atrium/internal/testreap"
)

// A container left behind by a test process that is gone is removed, even
// paused (as a test simulating an outage leaves it). One whose process still
// runs, or that another host (or PID namespace) sharing the daemon started,
// is kept.
func TestReapRemovesOnlyOrphanedContainers(t *testing.T) {
	t.Parallel()
	host := testreap.Host()
	if host == "" {
		t.Fatal("no host name")
	}
	pid, end := testreap.StandIn(t)
	orphan := labelled(t, host, pid)
	docker(t, nil, "pause", orphan)
	live := labelled(t, host, os.Getpid())
	otherHost := labelled(t, host+"-elsewhere", pid)
	end()

	// Another test process can reap the orphan first, so only what is left
	// counts.
	if _, err := testreap.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if exists(t, orphan) {
		t.Errorf("orphan %s of exited process %d is still there", orphan, pid)
	}
	if !exists(t, live) {
		t.Errorf("container %s of this running process was removed", live)
	}
	if !exists(t, otherHost) {
		t.Errorf("container %s of another host was removed", otherHost)
	}
}

// labelled runs a Valkey container labelled as started by pid on host, and
// registers its removal.
func labelled(t *testing.T, host string, pid int) string {
	t.Helper()
	name := "atrium-api-test-" + randomHex(t)[:12]
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = exec.CommandContext(ctx, "docker", "unpause", name).Run()
		_ = exec.CommandContext(ctx, "docker", "rm", "-f", "-v", name).Run()
	})
	docker(t, nil, "run", "-d", "--name", name, "--tmpfs", "/data",
		"--label", testreap.HostLabel+"="+host, "--label", testreap.PIDLabel+"="+strconv.Itoa(pid), ValkeyImage)
	return name
}

func exists(t *testing.T, name string) bool {
	t.Helper()
	return strings.TrimSpace(docker(t, nil, "ps", "-a", "-q", "--filter", "name=^"+name+"$")) != ""
}
