package storage

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestDatabaseTransportPolicy(t *testing.T) {
	t.Setenv("STEGO_DATABASE_ALLOW_INSECURE_LOOPBACK", "0")
	for _, mode := range []string{"disable", "prefer", "allow", "require", "verify-ca"} {
		if _, err := databaseConfiguration("host=127.0.0.1 user=test dbname=test sslmode=" + mode); err == nil {
			t.Fatal("weak database transport accepted", mode)
		}
	}
	config, err := databaseConfiguration("host=db.example.test user=test dbname=test sslmode=verify-full")
	if err != nil || config.TLSConfig == nil || config.TLSConfig.InsecureSkipVerify || config.TLSConfig.MinVersion < tls.VersionTLS12 {
		t.Fatal("verified TLS configuration failed", err)
	}
	t.Setenv("STEGO_DATABASE_ALLOW_INSECURE_LOOPBACK", "1")
	for _, host := range []string{"127.0.0.1", "::1"} {
		if _, err := databaseConfiguration("host=" + host + " user=test dbname=test sslmode=disable"); err != nil {
			t.Fatal(err)
		}
	}
	for _, host := range []string{"localhost", "192.0.2.1", "/tmp", "127.0.0.1,192.0.2.1"} {
		if _, err := databaseConfiguration("host=" + host + " user=test dbname=test sslmode=disable"); err == nil {
			t.Fatal("loopback exception accepted another target", host)
		}
	}
	if _, err := databaseConfiguration("host=127.0.0.1 user=test dbname=test sslmode=prefer"); err == nil {
		t.Fatal("test exception permitted an unverified TLS fallback")
	}
	for _, value := range []string{"", "true", "false", " 1", "private-setting"} {
		t.Setenv("STEGO_DATABASE_ALLOW_INSECURE_LOOPBACK", value)
		if _, err := databaseConfiguration("host=localhost sslmode=verify-full"); err == nil || err.Error() != "invalid database setting: STEGO_DATABASE_ALLOW_INSECURE_LOOPBACK" {
			t.Fatal("invalid test exception setting lost its safe error", err)
		}
	}
}

func databaseTestCertificate(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	encodedKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	pair, err := tls.X509KeyPair(certPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encodedKey}))
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(name, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	return pair, name
}

func TestDatabaseVerifiedTLSConnection(t *testing.T) {
	cfg, err := pgx.ParseConfig(poolTestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	certificate, ca := databaseTestCertificate(t)
	_, otherCA := databaseTestCertificate(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	var workers sync.WaitGroup
	go func() {
		defer close(stopped)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conn.Close()
				closeOnCancel := context.AfterFunc(ctx, func() { conn.Close() })
				defer closeOnCancel()
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				var request [8]byte
				if _, err := io.ReadFull(conn, request[:]); err != nil || request != [8]byte{0, 0, 0, 8, 4, 210, 22, 47} {
					return
				}
				if _, err := conn.Write([]byte{'S'}); err != nil {
					return
				}
				secure := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
				if err := secure.HandshakeContext(ctx); err != nil {
					return
				}
				upstream, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(int(cfg.Port))))
				if err != nil {
					return
				}
				defer upstream.Close()
				stopUpstream := context.AfterFunc(ctx, func() { upstream.Close() })
				defer stopUpstream()
				copied := make(chan struct{})
				go func() { defer close(copied); io.Copy(upstream, secure); upstream.Close() }()
				io.Copy(secure, upstream)
				conn.Close()
				upstream.Close()
				<-copied
			}()
		}
	}()
	defer func() { cancel(); listener.Close(); <-stopped; workers.Wait() }()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	t.Setenv("STEGO_DATABASE_ALLOW_INSECURE_LOOPBACK", "0")
	for _, tc := range []struct {
		host, ca string
		valid    bool
	}{{"localhost", ca, true}, {"127.0.0.1", ca, false}, {"localhost", otherCA, false}} {
		address := &url.URL{Scheme: "postgres", Host: net.JoinHostPort(tc.host, port), Path: "/" + cfg.Database, User: url.UserPassword(cfg.User, cfg.Password)}
		query := url.Values{"sslmode": {"verify-full"}, "sslrootcert": {tc.ca}}
		address.RawQuery = query.Encode()
		pool, err := OpenDatabase(address.String())
		if err != nil {
			t.Fatal(err)
		}
		call, stop := context.WithTimeout(ctx, 5*time.Second)
		var value int
		err = pool.QueryRowContext(call, "SELECT 42").Scan(&value)
		stop()
		pool.Close()
		if tc.valid && (err != nil || value != 42) {
			t.Fatal("verified PostgreSQL connection failed", err)
		}
		if !tc.valid && err == nil {
			t.Fatal("PostgreSQL accepted the wrong certificate identity or trust root")
		}
	}
}
