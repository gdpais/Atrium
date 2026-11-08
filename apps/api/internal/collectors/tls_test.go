package collectors

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	heartbeatconfig "heartbeat/internal/config"
)

// The API reads a collector whose admin endpoints are on HTTPS, verifying
// its certificate with the configured CA, and refuses one it cannot verify.
func TestClientReadsAnHTTPSCollectorWithItsCA(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"version": "abc", "config": heartbeatconfig.RuntimeConfig{}.Redacted()})
	}))
	defer server.Close()
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}

	httpClient, err := NewHTTPClient(caFile)
	if err != nil {
		t.Fatal(err)
	}
	verified := &Client{URL: server.URL, Token: token, HTTP: httpClient}
	if _, err := verified.SQLServer(context.Background()); err != nil {
		t.Fatalf("with the CA: %v", err)
	}

	systemRoots, err := NewHTTPClient("")
	if err != nil {
		t.Fatal(err)
	}
	unverified := &Client{URL: server.URL, Token: token, HTTP: systemRoots}
	if _, err := unverified.SQLServer(context.Background()); err == nil {
		t.Fatal("a collector whose certificate the system roots do not know was read")
	}
}

func TestNewHTTPClientRejectsABadCAFile(t *testing.T) {
	dir := t.TempDir()
	notPEM := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(notPEM, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{notPEM, filepath.Join(dir, "missing.crt")} {
		if _, err := NewHTTPClient(path); err == nil {
			t.Fatalf("%s: want an error", path)
		}
	}
}
