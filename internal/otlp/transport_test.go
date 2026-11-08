package otlp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

// testCA signs certificates for the tests.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue returns a certificate for name and its key, both PEM.
func (ca *testCA) issue(t *testing.T, name string, usage x509.ExtKeyUsage) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// mtlsReceiver is an https OTLP receiver that requires a client certificate
// signed by ca, and records each request's client name and Authorization.
type mtlsReceiver struct {
	mu      sync.Mutex
	clients []string
	auth    []string
}

func startMTLSReceiver(t *testing.T, ca *testCA) (*httptest.Server, *mtlsReceiver) {
	t.Helper()
	certPEM, keyPEM := ca.issue(t, "receiver", x509.ExtKeyUsageServerAuth)
	serverCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	rec := &mtlsReceiver{}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.clients = append(rec.clients, r.TLS.PeerCertificates[0].Subject.CommonName)
		rec.auth = append(rec.auth, r.Header.Get("Authorization"))
		rec.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, rec
}

func oneRecord() []*logspb.ResourceLogs {
	return []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{TimeUnixNano: 1}}}}}}
}

func TestExportOverMutualTLSWithHeadersAndRotation(t *testing.T) {
	ca := newTestCA(t)
	server, rec := startMTLSReceiver(t, ca)
	dir := t.TempDir()
	files := TLSFiles{CAFile: filepath.Join(dir, "ca.crt"), CertFile: filepath.Join(dir, "tls.crt"), KeyFile: filepath.Join(dir, "tls.key")}
	writeFile(t, files.CAFile, ca.pem)
	certPEM, keyPEM := ca.issue(t, "collector-1", x509.ExtKeyUsageClientAuth)
	writeFile(t, files.CertFile, certPEM)
	writeFile(t, files.KeyFile, keyPEM)
	headersDir := t.TempDir()
	writeFile(t, filepath.Join(headersDir, "Authorization"), []byte("Bearer one\n"))

	httpClient, err := NewHTTPClient(files)
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient()
	client.HTTP = httpClient
	client.MaxAttempts = 1
	client.Headers = HeadersFromDir(headersDir)
	if err := client.Export(context.Background(), server.URL, oneRecord()); err != nil {
		t.Fatalf("export: %v", err)
	}

	// Rotate the certificate and the token: the next connection and the
	// next request use them, without a new client.
	certPEM, keyPEM = ca.issue(t, "collector-2", x509.ExtKeyUsageClientAuth)
	writeFile(t, files.CertFile, certPEM)
	writeFile(t, files.KeyFile, keyPEM)
	writeFile(t, filepath.Join(headersDir, "Authorization"), []byte("Bearer two"))
	httpClient.CloseIdleConnections()
	if err := client.Export(context.Background(), server.URL, oneRecord()); err != nil {
		t.Fatalf("export after rotation: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if strings.Join(rec.clients, ",") != "collector-1,collector-2" {
		t.Fatalf("client certificates %v, want collector-1 then collector-2", rec.clients)
	}
	if strings.Join(rec.auth, ",") != "Bearer one,Bearer two" {
		t.Fatalf("Authorization %v", rec.auth)
	}
}

func TestExportRefusesAnUnknownReceiver(t *testing.T) {
	ca := newTestCA(t)
	server, rec := startMTLSReceiver(t, ca)
	dir := t.TempDir()
	certPEM, keyPEM := ca.issue(t, "collector", x509.ExtKeyUsageClientAuth)
	files := TLSFiles{CertFile: filepath.Join(dir, "tls.crt"), KeyFile: filepath.Join(dir, "tls.key")}
	writeFile(t, files.CertFile, certPEM)
	writeFile(t, files.KeyFile, keyPEM)
	// No CA file: the system roots do not know the test CA.
	httpClient, err := NewHTTPClient(files)
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient()
	client.HTTP = httpClient
	client.MaxAttempts = 1
	err = client.Export(context.Background(), server.URL, oneRecord())
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("export to an unverified receiver = %v, want a certificate error", err)
	}
	if len(rec.clients) != 0 {
		t.Fatal("the receiver got a request")
	}
}

func TestNewHTTPClientRejectsIncompleteFiles(t *testing.T) {
	dir := t.TempDir()
	notPEM := filepath.Join(dir, "ca.crt")
	writeFile(t, notPEM, []byte("not a certificate"))
	for name, files := range map[string]TLSFiles{
		"cert without key": {CertFile: filepath.Join(dir, "tls.crt")},
		"key without cert": {KeyFile: filepath.Join(dir, "tls.key")},
		"missing CA":       {CAFile: filepath.Join(dir, "missing.crt")},
		"CA without PEM":   {CAFile: notPEM},
		"missing cert":     {CertFile: filepath.Join(dir, "tls.crt"), KeyFile: filepath.Join(dir, "tls.key")},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewHTTPClient(files); err == nil {
				t.Fatal("want an error")
			}
		})
	}
	client, err := NewHTTPClient(TLSFiles{})
	if err != nil || client.Transport != nil || client.Timeout != DefaultRequestTimeout {
		t.Fatalf("no files = %+v, %v; want the default transport and timeout", client, err)
	}
}

// secretVolume lays dir out as kubelet mounts a Secret: the keys are
// symlinks into a ..data directory.
func secretVolume(t *testing.T, keys map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	data := filepath.Join(dir, "..2026_11_08_00_00_00.000000001")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	for key, value := range keys {
		writeFile(t, filepath.Join(data, key), []byte(value))
	}
	if err := os.Symlink(filepath.Base(data), filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	for key := range keys {
		if err := os.Symlink(filepath.Join("..data", key), filepath.Join(dir, key)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestHeadersFromDirReadsASecretVolume(t *testing.T) {
	dir := secretVolume(t, map[string]string{"authorization": "Bearer s3cret\n", "X-Scope-OrgID": "tenant-a"})
	headers, err := HeadersFromDir(dir)()
	if err != nil {
		t.Fatal(err)
	}
	if len(headers) != 2 || headers.Get("Authorization") != "Bearer s3cret" || headers.Get("X-Scope-OrgID") != "tenant-a" {
		t.Fatalf("headers = %v", headers)
	}
}

func TestHeadersFromDirRejectsBadFilesWithoutQuotingThem(t *testing.T) {
	for name, keys := range map[string]map[string]string{
		"reserved header": {"Content-Type": "text/plain-s3cret"},
		"invalid name":    {"bad name": "s3cret"},
		"empty value":     {"Authorization": " \n"},
		"invalid value":   {"Authorization": "Bearer s3cret\r\nX-Evil: 1"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := HeadersFromDir(secretVolume(t, keys))()
			if err == nil {
				t.Fatal("want an error")
			}
			if strings.Contains(err.Error(), "s3cret") {
				t.Fatalf("error quotes the value: %v", err)
			}
		})
	}
	if _, err := HeadersFromDir(filepath.Join(t.TempDir(), "missing"))(); err == nil {
		t.Fatal("a missing directory: want an error")
	}
}

func TestExportSendsNothingWhenHeadersFail(t *testing.T) {
	rec := &receiver{t: t}
	server := httptest.NewServer(rec)
	defer server.Close()
	client := NewClient()
	client.MaxAttempts = 2
	client.sleep = func(context.Context, time.Duration) bool { return true }
	client.Headers = HeadersFromDir(filepath.Join(t.TempDir(), "missing"))
	err := client.Export(context.Background(), server.URL, oneRecord())
	if err == nil || !strings.Contains(err.Error(), "attempt 2 of 2") {
		t.Fatalf("export = %v, want a retried failure", err)
	}
	if rec.attempts != 0 {
		t.Fatalf("the receiver got %d requests without the headers", rec.attempts)
	}
}

func TestExportDoesNotFollowRedirects(t *testing.T) {
	var leaked []string
	var mu sync.Mutex
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		leaked = append(leaked, r.Header.Get("Api-Key"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer elsewhere.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "api-key"), []byte("secret"))
	for name, client := range map[string]*Client{"NewClient": NewClient(), "NewHTTPClient": nil} {
		t.Run(name, func(t *testing.T) {
			if client == nil {
				httpClient, err := NewHTTPClient(TLSFiles{})
				if err != nil {
					t.Fatal(err)
				}
				client = NewClient()
				client.HTTP = httpClient
			}
			client.MaxAttempts = 1
			client.Headers = HeadersFromDir(dir)
			err := client.Export(context.Background(), redirector.URL, oneRecord())
			if err == nil || !strings.Contains(err.Error(), "307") {
				t.Fatalf("export through a redirect = %v, want it rejected with the 307", err)
			}
		})
	}
	if len(leaked) != 0 {
		t.Fatalf("the redirect target got %d requests", len(leaked))
	}
}
