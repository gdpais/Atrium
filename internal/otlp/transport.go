package otlp

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/net/http/httpguts"
)

// TLSFiles names the PEM files that secure the connection to an OTLP/HTTP
// receiver over https.  Every field is optional: without CAFile the system
// roots verify the receiver; CertFile and KeyFile, set together, give a
// client certificate for mutual TLS.
type TLSFiles struct {
	// CAFile holds the certificates that verify the receiver, in place of the
	// system roots.  It is read once, when the client is built.
	CAFile string
	// CertFile and KeyFile are the client certificate and its key.  They are
	// read at every TLS handshake, so a rotated certificate is used by the
	// next connection without a restart.
	CertFile string
	KeyFile  string
}

// Empty reports whether no file is named.
func (f TLSFiles) Empty() bool {
	return f.CAFile == "" && f.CertFile == "" && f.KeyFile == ""
}

// NewHTTPClient returns an HTTP client for OTLP/HTTP export with the
// per-attempt [DefaultRequestTimeout] and, when files names any, a TLS
// configuration built from them.  It fails when only one of CertFile and
// KeyFile is set, when the CA file cannot be read or holds no certificate,
// or when the client certificate cannot be loaded now.
func NewHTTPClient(files TLSFiles) (*http.Client, error) {
	client := &http.Client{Timeout: DefaultRequestTimeout}
	if files.Empty() {
		return client, nil
	}
	config, err := tlsConfig(files)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = config
	client.Transport = transport
	return client, nil
}

func tlsConfig(files TLSFiles) (*tls.Config, error) {
	config := &tls.Config{MinVersion: tls.VersionTLS12}
	if files.CAFile != "" {
		pem, err := os.ReadFile(files.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read OTLP CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("OTLP CA file %s holds no PEM certificate", files.CAFile)
		}
		config.RootCAs = pool
	}
	if (files.CertFile == "") != (files.KeyFile == "") {
		return nil, errors.New("OTLP client certificate needs both a certificate file and a key file")
	}
	if files.CertFile != "" {
		// Load once now, so a wrong path fails the start instead of every
		// export, then again at each handshake to follow rotation.
		if _, err := tls.LoadX509KeyPair(files.CertFile, files.KeyFile); err != nil {
			return nil, fmt.Errorf("load OTLP client certificate: %w", err)
		}
		config.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			cert, err := tls.LoadX509KeyPair(files.CertFile, files.KeyFile)
			if err != nil {
				return nil, fmt.Errorf("load OTLP client certificate: %w", err)
			}
			return &cert, nil
		}
	}
	return config, nil
}

// reservedHeaders are set by the client itself and cannot be configured.
var reservedHeaders = []string{"Content-Encoding", "Content-Length", "Content-Type", "Host"}

// HeadersFromDir returns a [Client.Headers] function that reads dir at each
// request: every regular file whose name does not start with a dot is one
// header, the file name its name and the content, without surrounding white
// space, its value.  That is the layout of a Kubernetes Secret mounted as a
// volume, whose ..data entries are skipped, so a rotated key is sent from the
// next request on.  Errors name the file, never its value.
func HeadersFromDir(dir string) func() (http.Header, error) {
	return func() (http.Header, error) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("read OTLP headers: %w", err)
		}
		headers := http.Header{}
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, ".") {
				continue
			}
			path := filepath.Join(dir, name)
			info, err := os.Stat(path) // follows the Secret volume's symlinks
			if err != nil {
				return nil, fmt.Errorf("read OTLP header %s: %w", name, err)
			}
			if !info.Mode().IsRegular() {
				continue
			}
			if !httpguts.ValidHeaderFieldName(name) {
				return nil, fmt.Errorf("OTLP header file %s: not a valid header name", name)
			}
			if slices.Contains(reservedHeaders, http.CanonicalHeaderKey(name)) {
				return nil, fmt.Errorf("OTLP header file %s: the client sets this header itself", name)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("read OTLP header %s: %w", name, err)
			}
			value := strings.TrimSpace(string(raw))
			if value == "" || !httpguts.ValidHeaderFieldValue(value) {
				return nil, fmt.Errorf("OTLP header file %s: empty or not a valid header value", name)
			}
			headers.Set(name, value)
		}
		return headers, nil
	}
}
