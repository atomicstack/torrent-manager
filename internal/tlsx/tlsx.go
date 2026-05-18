// Package tlsx loads matching server-side and client-side TLS configs for
// mutual TLS authentication.
package tlsx

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// ServerConfig builds a *tls.Config that requires + verifies client certs
// signed by the provided CA. MinVersion is TLS 1.3.
func ServerConfig(caPath, certPath, keyPath string) (*tls.Config, error) {
	caPool, err := loadCAPool(caPath)
	if err != nil {
		return nil, err
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load server keypair: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{pair},
		ClientCAs:    caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// ClientConfig builds a *tls.Config that presents a client cert and trusts the
// server only if signed by the provided CA. MinVersion is TLS 1.3.
func ClientConfig(caPath, certPath, keyPath string) (*tls.Config, error) {
	caPool, err := loadCAPool(caPath)
	if err != nil {
		return nil, err
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load client keypair: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{pair},
		RootCAs:      caPool,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

func loadCAPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read ca: %w", err)
	}
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("ca file %s contained no certs", path)
	}
	return p, nil
}
