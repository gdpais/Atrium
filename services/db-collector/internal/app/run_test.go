package app

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"atrium/services/db-collector/internal/collectors"
	collectorexport "atrium/services/db-collector/internal/export"
	collectormetadata "atrium/services/db-collector/internal/metadata"
)

// slowExecutor simulates an in-flight SQL probe: once started it takes delay
// to finish and ignores cancellation, like a query waiting on the server.
type slowExecutor struct {
	delay    time.Duration
	started  chan struct{}
	inFlight atomic.Int32
	finished atomic.Int32
}

func (e *slowExecutor) RunProbe(context.Context, collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
	if e.inFlight.Add(1) == 1 {
		close(e.started)
	}
	time.Sleep(e.delay)
	e.finished.Add(1)
	return nil, nil, nil
}

// runFixture starts run in the background with a real Poller and executor.
type runFixture struct {
	cancel   context.CancelFunc
	result   chan error
	addr     chan net.Addr
	executor *slowExecutor
	closed   atomic.Bool
}

// startRun starts run with a slow executor; configure, when given, adjusts
// the dependencies first.
func startRun(t *testing.T, delay time.Duration, t2 timeouts, configure ...func(*runDeps)) *runFixture {
	t.Helper()
	manager, path := newTestConfigManager(t)
	writeTestConfig(t, path, testCollector{id: "sql-a", env: "prod", interval: "1s"})
	if _, err := manager.Reload(); err != nil {
		t.Fatal(err)
	}
	f := &runFixture{result: make(chan error, 1), addr: make(chan net.Addr, 1), executor: &slowExecutor{delay: delay, started: make(chan struct{})}}
	runner := collectors.NewRunner(f.executor, collectorexport.NewInMemoryExporter(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	t.Cleanup(cancel)
	deps := runDeps{
		cfg:           Config{ListenAddr: "127.0.0.1:0", AdminToken: "test-token"},
		configManager: manager,
		registry:      prometheus.NewRegistry(),
		newLifecycle: func(parent context.Context) *pollerLifecycle {
			return newPollerLifecycle(parent, runner, nil)
		},
		closeConnections: func() error { f.closed.Store(true); return nil },
		timeouts:         t2,
		onListen:         func(addr net.Addr) { f.addr <- addr },
	}
	for _, fn := range configure {
		fn(&deps)
	}
	go func() { f.result <- run(ctx, deps) }()
	return f
}

func TestRunShutsDownSynchronouslyAfterInFlightCycle(t *testing.T) {
	f := startRun(t, 200*time.Millisecond, testTimeouts())
	addr := <-f.addr
	select {
	case <-f.executor.started:
	case <-time.After(2 * time.Second):
		t.Fatal("poller never ran a probe")
	}
	resp, err := http.Get(fmt.Sprintf("http://%s/healthz", addr))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %v %v", resp, err)
	}
	_ = resp.Body.Close()

	start := time.Now()
	f.cancel()
	select {
	case err := <-f.result:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return")
	}
	if f.executor.finished.Load() != f.executor.inFlight.Load() {
		t.Fatal("run returned while a probe was still in flight")
	}
	if !f.closed.Load() {
		t.Fatal("connection manager was not closed")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("shutdown took %s", elapsed)
	}
	if _, err := http.Get(fmt.Sprintf("http://%s/healthz", addr)); err == nil {
		t.Fatal("http server still serving after run returned")
	}
}

func TestRunShutdownIsBoundedWhenPollerHangs(t *testing.T) {
	timeouts := testTimeouts()
	timeouts.pollerStop = 100 * time.Millisecond
	f := startRun(t, 3*time.Second, timeouts)
	<-f.addr
	<-f.executor.started
	start := time.Now()
	f.cancel()
	select {
	case <-f.result:
	case <-time.After(2 * time.Second):
		t.Fatal("run hung on a poller that ignores cancellation")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("shutdown exceeded its bound: %s", elapsed)
	}
	if !f.closed.Load() {
		t.Fatal("connection manager was not closed after poller stop timeout")
	}
}

func TestRunReturnsListenError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	manager, _ := newTestConfigManager(t)
	err = run(context.Background(), runDeps{
		cfg:           Config{ListenAddr: listener.Addr().String()},
		configManager: manager,
		registry:      prometheus.NewRegistry(),
		newLifecycle: func(parent context.Context) *pollerLifecycle {
			return newPollerLifecycleWithRun(parent, nil, newFakePollers().run)
		},
		timeouts: testTimeouts(),
	})
	if err == nil {
		t.Fatal("expected listen error")
	}
}
