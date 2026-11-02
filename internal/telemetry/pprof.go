package telemetry

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"time"
)

// startPprof serves the runtime profiles on addr and returns the server, or
// nil when addr is empty or not usable.
//
// Profiles expose memory contents and cost CPU, so the listener accepts only a
// loopback address. `kubectl port-forward` reaches it inside the pod, which
// needs no token and no NetworkPolicy rule.
func startPprof(addr string, logger *slog.Logger) *http.Server {
	if addr == "" {
		return nil
	}
	if !loopback(addr) {
		logger.Error("telemetry: pprof listener not started: not a loopback address", "addr", addr)
		return nil
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Error("telemetry: pprof listener not started", "addr", addr, "error", err)
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	// No write timeout: a CPU profile answers when its duration has passed.
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("telemetry: pprof listener failed", "error", err)
		}
	}()
	logger.Info("telemetry: pprof listening", "addr", listener.Addr().String())
	return server
}

// loopback reports whether addr, a host:port, can only be reached from inside
// the pod. Empty hosts and 0.0.0.0 listen everywhere, so they do not count.
func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
