package app

import (
	"crypto/tls"
	"errors"
	"fmt"
)

// DefaultAdminListenAddr is where the admin endpoints listen over HTTPS when
// [AdminTLS] names a certificate and no address.
const DefaultAdminListenAddr = ":8443"

// AdminTLS serves the admin endpoints (GET /admin/config, POST
// /admin/config/reload) on their own HTTPS listener. With a certificate set,
// ListenAddr keeps /metrics and the health endpoints over plain HTTP, for
// Prometheus and the kubelet, and no longer serves /admin: the bearer token
// is only accepted over TLS.
type AdminTLS struct {
	// ListenAddr is the HTTPS address; empty means [DefaultAdminListenAddr].
	ListenAddr string
	// CertFile and KeyFile are the PEM server certificate and key, both or
	// neither. They are read at every TLS handshake, so a renewed
	// certificate (cert-manager) is served without a restart.
	CertFile string
	KeyFile  string
}

// Enabled reports whether a certificate or key is named.
func (a AdminTLS) Enabled() bool { return a.CertFile != "" || a.KeyFile != "" }

func (a AdminTLS) listenAddr() string {
	if a.ListenAddr == "" {
		return DefaultAdminListenAddr
	}
	return a.ListenAddr
}

// serverConfig loads the certificate once, so a wrong path or key fails the
// start, and returns a TLS 1.2+ configuration that loads it again at each
// handshake.
func (a AdminTLS) serverConfig() (*tls.Config, error) {
	if a.CertFile == "" || a.KeyFile == "" {
		return nil, errors.New("admin TLS needs both a certificate file and a key file")
	}
	load := func() (*tls.Certificate, error) {
		cert, err := tls.LoadX509KeyPair(a.CertFile, a.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load admin TLS certificate: %w", err)
		}
		return &cert, nil
	}
	if _, err := load(); err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return load() },
	}, nil
}
