//go:build ignore

// This TLS registry is a bounded CI fixture. It is not a deployment service.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
)

func main() {
	output := flag.String("output", "", "new private fixture directory")
	flag.Parse()
	if os.Getenv("CI") != "true" || *output == "" {
		log.Fatal("the registry fixture requires CI and an output directory")
	}
	if err := os.Mkdir(*output, 0700); err != nil {
		log.Fatal("cannot create fixture directory")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatal("cannot create fixture key")
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "STEGO CI registry"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	raw, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		log.Fatal("cannot create fixture certificate")
	}
	if err := os.WriteFile(filepath.Join(*output, "ca.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw}), 0600); err != nil {
		log.Fatal("cannot save fixture CA")
	}
	if err := os.WriteFile(filepath.Join(*output, "credentials.json"), []byte("{\"username\":\"test\",\"password\":\"fixture-credential\"}\n"), 0600); err != nil {
		log.Fatal("cannot save fixture credentials")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal("cannot start fixture listener")
	}
	defer listener.Close()
	tlsListener := tls.NewListener(listener, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{raw}, PrivateKey: key}}})
	backend := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 60 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 32 << 10, ErrorLog: log.New(io.Discard, "", 0), Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "test" || password != "fixture-credential" {
			w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
			w.WriteHeader(401)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 130<<20)
		backend.ServeHTTP(w, r)
	})}
	data, _ := json.Marshal(map[string]string{"repository": listener.Addr().String() + "/test/application"})
	if err := os.WriteFile(filepath.Join(*output, "endpoint.json"), append(data, '\n'), 0600); err != nil {
		log.Fatal("cannot save fixture endpoint")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	go func() { <-ctx.Done(); server.Close() }()
	if err := server.Serve(tlsListener); err != http.ErrServerClosed {
		log.Fatal("fixture registry stopped unexpectedly")
	}
}
