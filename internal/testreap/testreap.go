//go:build integration

// Package testreap removes the Docker containers that an integration test
// process left behind. Tests remove their containers in t.Cleanup, which
// runs only while the process lives: a panic in one parallel test, a kill
// or a timeout ends the whole process, and the cleanups of the tests still
// running never run.
//
// Each test container is labelled with the host and the process ID of the
// test process that started it ([RunArgs], [ComposeEnv]). [Reap] removes the
// containers labelled with this host and a process that is no longer
// running, with their Compose networks. It never touches a container whose
// process still runs, nor one started by another host, or another PID
// namespace, that shares the Docker daemon; a reused process ID only keeps an
// orphan for longer.
package testreap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	// HostLabel holds the host of the test process ([Host]).
	HostLabel = "heartbeat.test.host"
	// PIDLabel holds its process ID.
	PIDLabel = "heartbeat.test.pid"

	// HostEnv and PIDEnv carry the labels' values into
	// infra/docker-compose.test.yml.
	HostEnv = "HEARTBEAT_TEST_HOST"
	PIDEnv  = "HEARTBEAT_TEST_PID"

	composeProjectLabel = "com.docker.compose.project"
)

// RunArgs returns the `docker run` flags that label a container as this
// process's.
func RunArgs() []string {
	return []string{"--label", HostLabel + "=" + Host(), "--label", PIDLabel + "=" + strconv.Itoa(os.Getpid())}
}

// ComposeEnv returns the environment that makes the test Compose file label
// its containers as this process's.
func ComposeEnv() []string {
	return []string{HostEnv + "=" + Host(), PIDEnv + "=" + strconv.Itoa(os.Getpid())}
}

var once sync.Once

// ReapOnce runs [Reap] the first time it is called in this process and logs
// what it removed to t; later calls do nothing. Test helpers call it before
// they start a container. Leftovers never fail a test: an error is only
// logged.
func ReapOnce(t testing.TB) {
	t.Helper()
	var removed []string
	var err error
	first := false
	once.Do(func() {
		first = true
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		removed, err = Reap(ctx)
	})
	if !first {
		return
	}
	for _, name := range removed {
		t.Logf("removed %s, left behind by a test process that is gone", name)
	}
	if err != nil {
		t.Logf("remove leftover test containers: %v", err)
	}
}

// StandIn starts a process that stands in for another test process in tests
// of [Reap], and returns its ID. Containers labelled with it are kept until
// end stops the process and waits for it; then any Reap, of this test
// process or another, removes them. A process that has already exited would
// let them be removed while the test still sets them up.
func StandIn(t testing.TB) (pid int, end func()) {
	t.Helper()
	cmd := exec.Command("sleep", "600")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stand-in process: %v", err)
	}
	// Waiting matters: an exited child not yet waited for still counts as
	// running.
	end = sync.OnceFunc(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	t.Cleanup(end)
	return cmd.Process.Pid, end
}

// Reap removes the containers labelled with this host and the ID of a
// process that is no longer running, and the networks of their Compose
// projects. It returns the names of the containers it removed. A container
// another test process removes at the same moment is not an error: Reap waits
// until it is gone, since its network cannot be removed before.
func Reap(ctx context.Context) ([]string, error) {
	h := Host()
	if h == "" {
		return nil, nil
	}
	out, err := docker(ctx, "ps", "-a", "--no-trunc", "--filter", "label="+HostLabel+"="+h,
		"--format", `{{.ID}}\t{{.Names}}\t{{.Label "`+PIDLabel+`"}}\t{{.Label "`+composeProjectLabel+`"}}`)
	if err != nil {
		return nil, err
	}
	var removed []string
	var errs []error
	var projects []string
	for line := range strings.Lines(out) {
		fields := strings.Split(strings.TrimRight(line, "\n"), "\t")
		if len(fields) != 4 {
			continue
		}
		id, name, project := fields[0], fields[1], fields[3]
		pid, err := strconv.Atoi(fields[2])
		if err != nil || pid <= 0 || running(pid) {
			continue
		}
		// A paused container cannot be removed by some runtimes.
		_, _ = docker(ctx, "unpause", id)
		if _, err := docker(ctx, "rm", "-f", "-v", id); err != nil {
			if !gone(err) {
				errs = append(errs, err)
				continue
			}
			if err := waitRemoved(ctx, id); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		removed = append(removed, name)
		if project != "" && !slices.Contains(projects, project) {
			projects = append(projects, project)
		}
	}
	for _, project := range projects {
		out, err := docker(ctx, "network", "ls", "-q", "--filter", "label="+composeProjectLabel+"="+project)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, id := range strings.Fields(out) {
			if _, err := docker(ctx, "network", "rm", id); err != nil && !gone(err) {
				errs = append(errs, err)
			}
		}
	}
	return removed, errors.Join(errs...)
}

// waitRemoved waits until the container, which another test process is
// removing, is gone.
func waitRemoved(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		out, err := docker(ctx, "ps", "-a", "-q", "--filter", "id="+id)
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) == "" {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("container %s is still being removed: %w", id, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Host returns where this process's IDs are valid: the host name and, on
// Linux, the PID namespace. A test process in a container that shares the
// Docker daemon, even with the host's name, has its own process IDs. It is ""
// if the name is unknown: containers are then labelled with an empty host,
// which Reap never matches.
func Host() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return ""
	}
	if ns, err := os.Readlink("/proc/self/ns/pid"); err == nil {
		name += "/" + ns
	}
	return name
}

// gone reports whether the daemon says the object is already removed, or
// being removed, by someone else: "No such container", "network … not
// found", "removal of container … is already in progress". Only what docker
// wrote to stderr counts, not an error running docker itself.
func gone(err error) bool {
	var failed *dockerError
	if !errors.As(err, &failed) {
		return false
	}
	return strings.Contains(failed.stderr, "No such") || strings.Contains(failed.stderr, " not found") ||
		strings.Contains(failed.stderr, "already in progress")
}

// dockerError is a docker command that failed, with what it wrote to stderr.
type dockerError struct {
	args   []string
	err    error
	stderr string
}

func (e *dockerError) Error() string {
	return fmt.Sprintf("docker %s: %v: %s", strings.Join(e.args, " "), e.err, e.stderr)
}

func (e *dockerError) Unwrap() error { return e.err }

func docker(ctx context.Context, args ...string) (string, error) {
	var stderr strings.Builder
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", &dockerError{args: args, err: err, stderr: strings.TrimSpace(stderr.String())}
	}
	return string(out), nil
}
