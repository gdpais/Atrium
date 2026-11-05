package inventory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"heartbeat/apps/api/internal/audit"
)

// FileCheckInterval is how often a FileApplier reads the inventory file: a
// ConfigMap change reaches the mounted file within about a minute, as for
// the integrations file the links are rendered from.
const FileCheckInterval = 30 * time.Second

// Inventory apply metrics, on the API's /metrics.
const (
	// MetricInventoryApplies counts reads of the inventory file that were
	// applied or refused, by result: changed (PostgreSQL changed to match
	// it), unchanged (PostgreSQL matched it already), invalid (the file
	// failed validation, or the existing rows refused it: nothing changed)
	// or error (it could not be read or applied, such as PostgreSQL down;
	// tried again at the next read). A file read again unchanged after it
	// was applied or failed validation is not counted; one the existing
	// rows refused is tried again, and counted, at each read.
	MetricInventoryApplies = "heartbeat_api_inventory_applies_total"
	// MetricInventoryAppliedTime is when the API last applied the file, or
	// found PostgreSQL matching it, as a Unix time; absent until then.
	MetricInventoryAppliedTime = "heartbeat_api_inventory_applied_timestamp_seconds"
)

// Results of an apply, the values of MetricInventoryApplies' result label.
const (
	ApplyChanged   = "changed"
	ApplyUnchanged = "unchanged"
	ApplyInvalid   = "invalid"
	ApplyError     = "error"
)

// FileApplier applies the environment's inventory file, as mounted in the
// API, to PostgreSQL when the API starts and whenever the file changes
// (ADR 0021 §3). A file that fails validation changes nothing: the last
// applied inventory stays, the problems are logged once and counted, and
// readiness is not affected.
type FileApplier struct {
	svc    *Service
	path   string
	host   string
	logger *slog.Logger

	applies *prometheus.CounterVec
	applied prometheus.Gauge

	// done is the revision last applied, or that failed validation: it is
	// not applied again until the file changes. One that PostgreSQL's rows
	// refused, or that failed with an error, is tried again at the next
	// read: the rows in the way may have changed.
	done string
	// lastErr is the last read's error, to log each one once.
	lastErr string
}

// NewFileApplier returns a FileApplier for the file at path, applied by
// svc, its changes audited as the file applied on host. An empty path
// returns nil: no inventory file.
func NewFileApplier(svc *Service, path, host string, logger *slog.Logger) *FileApplier {
	if path == "" {
		return nil
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	f := &FileApplier{
		svc: svc, path: path, host: host, logger: logger,
		applies: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricInventoryApplies,
			Help: "Reads of the inventory file applied or refused, by result: changed, unchanged, invalid or error.",
		}, []string{"result"}),
		applied: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: MetricInventoryAppliedTime,
			Help: "When the API last applied the inventory file, or found PostgreSQL matching it, as a Unix time.",
		}),
	}
	for _, result := range []string{ApplyChanged, ApplyUnchanged, ApplyInvalid, ApplyError} {
		f.applies.WithLabelValues(result)
	}
	return f
}

// Collectors are the FileApplier's metrics, to register.
func (f *FileApplier) Collectors() []prometheus.Collector {
	return []prometheus.Collector{f.applies, f.applied}
}

// Run applies the file now and after each FileCheckInterval until ctx
// ends.
func (f *FileApplier) Run(ctx context.Context) {
	ticker := time.NewTicker(FileCheckInterval)
	defer ticker.Stop()
	for {
		f.Check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Check reads the file and applies it unless it is the revision last
// applied, or last found invalid. It returns the result counted, or ""
// when it applied nothing because of that, or because ctx ended.
func (f *FileApplier) Check(ctx context.Context) string {
	data, err := readFile(f.path)
	if err != nil {
		f.fail(ApplyError, "inventory: cannot read the inventory file; the last applied inventory stays", err)
		return ApplyError
	}
	sum := sha256.Sum256(data)
	revision := "sha256:" + hex.EncodeToString(sum[:])
	if revision == f.done {
		return ""
	}
	var file File
	if len(data) > MaxFileBytes {
		err = &FileError{Problems: []string{fmt.Sprintf("larger than %d bytes", MaxFileBytes)}}
	} else {
		file, err = ParseFile(data)
	}
	if err != nil {
		f.done = revision
		f.fail(ApplyInvalid, "inventory: the inventory file is not valid; nothing was applied and the last applied inventory stays", err, "revision", revision)
		return ApplyInvalid
	}
	caller := Caller{Actor: audit.ConfigActor(f.path, f.host), RequestID: "inventory-" + revision}
	result, err := f.svc.ApplyFile(ctx, caller, revision, file)
	var invalid *FileError
	switch {
	case errors.As(err, &invalid):
		f.fail(ApplyInvalid, "inventory: PostgreSQL refuses the inventory file; nothing was applied and the last applied inventory stays", err, "revision", revision)
		return ApplyInvalid
	case err != nil:
		if ctx.Err() != nil {
			return ""
		}
		f.fail(ApplyError, "inventory: cannot apply the inventory file; trying again at the next read", err, "revision", revision)
		return ApplyError
	}
	f.done, f.lastErr = revision, ""
	f.applied.SetToCurrentTime()
	if result.Changes == 0 {
		f.applies.WithLabelValues(ApplyUnchanged).Inc()
		f.logger.Info("inventory: PostgreSQL matches the inventory file", "path", f.path, "revision", revision,
			"environment", file.Environment.Slug)
		return ApplyUnchanged
	}
	f.applies.WithLabelValues(ApplyChanged).Inc()
	f.logger.Info("inventory: inventory file applied", "path", f.path, "revision", revision,
		"environment", file.Environment.Slug, "changes", result.Changes)
	return ApplyChanged
}

// fail counts a read that ended in result, and logs err unless the last
// read failed the same way.
func (f *FileApplier) fail(result, msg string, err error, attrs ...any) {
	f.applies.WithLabelValues(result).Inc()
	if err.Error() == f.lastErr {
		return
	}
	f.lastErr = err.Error()
	f.logger.Error(msg, append([]any{"path", f.path, "error", err}, attrs...)...)
}

// readFile reads the file at path, up to one byte more than MaxFileBytes.
func readFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return data, nil
}
