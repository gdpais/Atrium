package sqlserver

import (
	"testing"
	"time"

	"github.com/microsoft/go-mssqldb/msdsn"

	collectorconfig "atrium/internal/config"
)

// The driver must parse the DSN into the intended timeouts; a misspelt
// parameter name would be ignored silently.
func TestDSNSetsDriverTimeouts(t *testing.T) {
	m := NewManager(newResolver())
	t.Cleanup(func() { _ = m.Close() })
	config, err := msdsn.Parse(m.dsn(target("a", "db-a.example", "ref-a"), Credential{Username: "u", Password: testPassword}))
	if err != nil {
		t.Fatalf("driver rejects the DSN: %v", err)
	}
	if config.DialTimeout != 5*time.Second {
		t.Fatalf("dial timeout = %s, want 5s", config.DialTimeout)
	}
	if config.ConnTimeout != socketTimeout {
		t.Fatalf("connection timeout = %s, want %s", config.ConnTimeout, socketTimeout)
	}
	// The DSN carries whole seconds; the socket deadline must still outlast
	// the longest probe timeout so it never cuts a running probe short.
	if config.ConnTimeout <= collectorconfig.MaxProbeTimeout {
		t.Fatalf("connection timeout %s does not outlast the maximum probe timeout %s", config.ConnTimeout, collectorconfig.MaxProbeTimeout)
	}
}
