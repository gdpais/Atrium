package links

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"heartbeat/internal/config"
)

// DefaultRefresh is how often a Source rereads integrations.yaml, at most:
// a ConfigMap change reaches the mounted file within about a minute, and a
// link that is 30 seconds stale does no harm.
const DefaultRefresh = 30 * time.Second

// ErrUnavailable is a Source that has never read a valid file.
var ErrUnavailable = errors.New("links: no valid integrations.yaml has been read")

// Source is the integrations config links are rendered from: the file the
// DB collector reads, mounted from the heartbeat-integrations ConfigMap. It
// reads the file when first asked and again when asked after Refresh, so it
// needs no watcher. A file the loader or config.ValidateLinkTemplates
// refuses never replaces a valid one (config.Manager): the links of the last
// valid file are served, and the error is logged once. A nil *Source has no
// file: no links.
type Source struct {
	path    string
	logger  *slog.Logger
	refresh time.Duration
	now     func() time.Time

	mu      sync.Mutex
	manager *config.Manager
	// read is when the file was last read; zero before the first time.
	read time.Time
	// lastErr is the last read's error, "" when it succeeded, to log each
	// error once.
	lastErr string
}

// NewSource returns a Source for the file at path and reads it once, so a
// bad file is logged at startup. An empty path returns nil.
func NewSource(path string, logger *slog.Logger) *Source {
	if path == "" {
		return nil
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	s := &Source{path: path, logger: logger, refresh: DefaultRefresh, now: time.Now}
	_, _ = s.Config()
	return s
}

// Config returns the active config: the last valid file read, or
// ErrUnavailable when none has been. With a nil Source it returns a config
// with no platform, which renders no links.
func (s *Source) Config() (config.RuntimeConfig, error) {
	if s == nil {
		return config.RuntimeConfig{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if now := s.now(); s.read.IsZero() || now.Sub(s.read) >= s.refresh {
		s.read = now
		s.load()
	}
	if s.manager == nil {
		return config.RuntimeConfig{}, ErrUnavailable
	}
	return s.manager.Snapshot().Config, nil
}

// load reads the file, with s.mu held.
func (s *Source) load() {
	var err error
	if s.manager == nil {
		s.manager, err = config.NewManager(s.path, config.WithValidator(func(cfg config.RuntimeConfig) error {
			return config.ValidateLinkTemplates(cfg.Dashboards)
		}))
	} else {
		_, err = s.manager.Reload()
	}
	if err == nil {
		if s.lastErr != "" {
			s.logger.Info("links: integrations config read again", "path", s.path, "version", s.manager.Snapshot().Config.Version)
		}
		s.lastErr = ""
		return
	}
	if err.Error() == s.lastErr {
		return
	}
	s.lastErr = err.Error()
	if s.manager == nil {
		s.logger.Error("links: cannot read the integrations config; link requests answer 503 until it can be read", "path", s.path, "error", err)
		return
	}
	s.logger.Warn("links: integrations config rejected; serving the links of the last valid one", "path", s.path,
		"version", s.manager.Snapshot().Config.Version, "error", err)
}
