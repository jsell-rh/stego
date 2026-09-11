package main

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
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestGeneratedHTTPTLS(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("TLS file input requires Unix")
	}
	t.Setenv("STEGO_HTTP_REQUIRE_TLS", "1")
	t.Setenv("STEGO_HTTP_TLS_CERT", "")
	t.Setenv("STEGO_HTTP_TLS_KEY", "")
	if _, err := stegoHTTPTransport(); err == nil {
		t.Fatal("missing TLS settings accepted")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certificate := filepath.Join(dir, "cert.pem")
	private := filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certificate, certPEM, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(private, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0440); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STEGO_HTTP_TLS_CERT", certificate)
	t.Setenv("STEGO_HTTP_TLS_KEY", private)
	config, err := stegoHTTPTransport()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }), ReadHeaderTimeout: time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(tls.NewListener(listener, config)) }()
	defer func() { _ = server.Close(); <-done }()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certPEM)
	for _, test := range []struct {
		name string
		tls  *tls.Config
		want bool
	}{
		{"verified", &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}, true},
		{"unknown CA", &tls.Config{MinVersion: tls.VersionTLS13}, false},
		{"wrong host", &tls.Config{RootCAs: roots, ServerName: "other.invalid", MinVersion: tls.VersionTLS13}, false},
		{"TLS 1.2", &tls.Config{RootCAs: roots, MaxVersion: tls.VersionTLS12}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			transport := &http.Transport{TLSClientConfig: test.tls}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: time.Second}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+listener.Addr().String(), nil)
			if err != nil {
				t.Fatal(err)
			}
			res, err := client.Do(req)
			if err == nil {
				res.Body.Close()
			}
			if (err == nil) != test.want {
				t.Fatalf("TLS result: %v", err)
			}
		})
	}
	for _, mode := range []os.FileMode{0644, 0460, 0540} {
		if err := os.Chmod(private, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := stegoHTTPTransport(); err == nil {
			t.Fatalf("unsafe key mode accepted: %o", mode)
		}
	}
	if err := os.Chmod(private, 0440); err != nil {
		t.Fatal(err)
	}
	projected := filepath.Join(dir, "projected-key")
	if err := os.Symlink(private, projected); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STEGO_HTTP_TLS_KEY", projected)
	if _, err := stegoHTTPTransport(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"relative", dir, filepath.Join(dir, "missing")} {
		t.Setenv("STEGO_HTTP_TLS_KEY", name)
		if _, err := stegoHTTPTransport(); err == nil {
			t.Fatal("invalid key accepted")
		}
	}
	t.Setenv("STEGO_HTTP_TLS_KEY", private)
	t.Setenv("STEGO_HTTP_REQUIRE_TLS", "false")
	if _, err := stegoHTTPTransport(); err == nil {
		t.Fatal("ambiguous switch accepted")
	}
}
