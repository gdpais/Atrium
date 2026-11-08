package app

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeServerCert writes a self-signed certificate for 127.0.0.1 named cn,
// and returns the files and a pool that trusts it.
func writeServerCert(t *testing.T, dir, cn string) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return certFile, keyFile, pool
}

func adminGet(t *testing.T, client *http.Client, url string) (*http.Response, error) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	return client.Do(req)
}

func TestRunServesAdminOnlyOverTLSWhenConfigured(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, pool := writeServerCert(t, dir, "admin-1")
	adminAddr := make(chan net.Addr, 1)
	f := startRun(t, 0, testTimeouts(), func(deps *runDeps) {
		deps.cfg.AdminTLS = AdminTLS{ListenAddr: "127.0.0.1:0", CertFile: certFile, KeyFile: keyFile}
		deps.onAdminListen = func(addr net.Addr) { adminAddr <- addr }
	})
	addr := <-f.addr
	admin := <-adminAddr

	// The plain listener keeps health and metrics, and has no admin routes.
	for path, want := range map[string]int{"/healthz": 200, "/metrics": 200, "/admin/config": 404, "/admin/config/reload": 404} {
		resp, err := adminGet(t, http.DefaultClient, fmt.Sprintf("http://%s%s", addr, path))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("plain %s = %d, want %d", path, resp.StatusCode, want)
		}
	}

	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	resp, err := adminGet(t, client, fmt.Sprintf("https://%s/admin/config", admin))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.TLS.PeerCertificates[0].Subject.CommonName != "admin-1" {
		t.Fatalf("https /admin/config = %d", resp.StatusCode)
	}
	resp, err = client.Get(fmt.Sprintf("https://%s/admin/config", admin))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("https /admin/config without the token = %d, want 401", resp.StatusCode)
	}
	resp, err = client.Get(fmt.Sprintf("https://%s/metrics", admin))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("https /metrics = %d, want 404: the admin listener serves only /admin", resp.StatusCode)
	}

	// A renewed certificate is served by the next handshake.
	_, _, pool2 := writeServerCert(t, dir, "admin-2")
	client2 := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool2}}}
	resp, err = adminGet(t, client2, fmt.Sprintf("https://%s/admin/config", admin))
	if err != nil {
		t.Fatalf("after renewal: %v", err)
	}
	_ = resp.Body.Close()
	if cn := resp.TLS.PeerCertificates[0].Subject.CommonName; cn != "admin-2" {
		t.Fatalf("served %s after renewal, want admin-2", cn)
	}

	// Plain HTTP to the admin port is refused.
	if resp, err := adminGet(t, http.DefaultClient, fmt.Sprintf("http://%s/admin/config", admin)); err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("plain HTTP to the admin port = %d, want 400", resp.StatusCode)
		}
	}

	f.cancel()
	if err := <-f.result; err != nil {
		t.Fatalf("run returned %v", err)
	}
	if _, err := client2.Get(fmt.Sprintf("https://%s/admin/config", admin)); err == nil {
		t.Fatal("admin listener still serving after run returned")
	}
}

func TestRunRefusesIncompleteAdminTLS(t *testing.T) {
	dir := t.TempDir()
	certFile, _, _ := writeServerCert(t, dir, "admin")
	for name, tlsCfg := range map[string]AdminTLS{
		"cert without key": {CertFile: certFile},
		"key without cert": {KeyFile: filepath.Join(dir, "tls.key")},
		"missing files":    {CertFile: filepath.Join(dir, "missing.crt"), KeyFile: filepath.Join(dir, "missing.key")},
	} {
		t.Run(name, func(t *testing.T) {
			listened := false
			f := startRun(t, 0, testTimeouts(), func(deps *runDeps) {
				deps.cfg.AdminTLS = tlsCfg
				deps.onListen = func(net.Addr) { listened = true }
			})
			select {
			case err := <-f.result:
				if err == nil || !strings.Contains(err.Error(), "admin TLS") {
					t.Fatalf("run = %v, want an admin TLS error", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("run started with an incomplete admin TLS configuration")
			}
			if listened {
				t.Fatal("run listened before refusing the configuration")
			}
		})
	}
}
