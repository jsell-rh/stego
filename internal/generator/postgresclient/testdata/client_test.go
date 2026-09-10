package postgres

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"github.com/jackc/pgx/v5"
	"io"
	"math/big"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func certificate(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "SQL acceptance"}, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	pair, err := tls.X509KeyPair(ca, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: encoded}))
	if err != nil {
		t.Fatal(err)
	}
	return pair, ca
}
func TestConfigurationIgnoresEnvironment(t *testing.T) {
	_, ca := certificate(t)
	for key, value := range map[string]string{"PGSERVICE": "", "PGSERVICEFILE": "/absent-service", "PGPASSFILE": "/absent-password", "PGHOST": "untrusted.example", "PGPORT": "1", "PGUSER": "other", "PGDATABASE": "other", "PGPASSWORD": "other-password", "PGSSLMODE": "prefer", "PGSSLROOTCERT": "/absent-root", "PGSSLCERT": "/absent-cert", "PGSSLKEY": "/absent-key", "PGTARGETSESSIONATTRS": "standby", "PGCONNECT_TIMEOUT": "1000", "PGOPTIONS": "-c default_transaction_read_only=off", "PGTZ": "UTC", "PGREQUIREAUTH": "none", "PGCHANNELBINDING": "require"} {
		t.Setenv(key, value)
	}
	o := Options{Host: "localhost", Port: 5432, User: "reader", Database: "catalog", Password: "actual", CA: ca, DialAddress: "127.0.0.1:23456"}
	config, err := configuration(o)
	if err != nil {
		t.Fatal(err)
	}
	if config.Host != o.Host || config.Port != o.Port || config.User != o.User || config.Database != o.Database || config.Password != o.Password || config.TLSConfig == nil || config.TLSConfig.ServerName != o.Host || config.TLSConfig.InsecureSkipVerify || len(config.Fallbacks) != 0 || config.MaxProtocolMessageBodyLen != MaxValueBytes {
		t.Fatal("environment changed identity, TLS, or limits")
	}
	if len(config.RuntimeParams) != 4 || config.RuntimeParams["default_transaction_read_only"] != "on" || config.RuntimeParams["statement_timeout"] != "5000" {
		t.Fatal("unsafe session")
	}
	t.Setenv("PGSERVICE", "unrelated")
	if _, err := configuration(o); err == nil {
		t.Fatal("accepted a service override")
	}
}

func TestInvalidIdentityAndCertificateAreRejected(t *testing.T) {
	_, ca := certificate(t)
	base := Options{Host: "localhost", Port: 5432, User: "reader", Database: "catalog", Password: "actual", CA: ca}
	for _, host := range []string{"localhost", "db.example", "127.0.0.1", "::1"} {
		o := base
		o.Host = host
		if _, err := configuration(o); err != nil {
			t.Fatal("valid host rejected", host, err)
		}
	}
	for _, change := range []func(*Options){
		func(o *Options) { o.Host = "/tmp/socket" }, func(o *Options) { o.Host = "bad@host" }, func(o *Options) { o.Host = "a..example" },
		func(o *Options) { o.Port = 0 }, func(o *Options) { o.User = "" }, func(o *Options) { o.Database = "" }, func(o *Options) { o.Password = "" },
		func(o *Options) { o.CA = append(append([]byte(nil), o.CA...), []byte("private trailing data")...) },
		func(o *Options) { o.CA = []byte("-----BEGIN PRIVATE KEY-----\ninvalid\n-----END PRIVATE KEY-----") },
	} {
		o := base
		change(&o)
		if _, err := configuration(o); err == nil {
			t.Fatal("invalid identity accepted")
		}
	}
}

func TestInvalidReadDoesNotConnect(t *testing.T) {
	_, ca := certificate(t)
	o := Options{Host: "localhost", Port: 5432, User: "reader", Database: "catalog", Password: "actual", CA: ca}
	for _, address := range []string{"localhost:5432", "127.0.0.1", "127.0.0.1:0", "127.0.0.1:65536", "https://127.0.0.1:5432"} {
		if ValidateDialAddress(address) == nil {
			t.Fatal("invalid route accepted", address)
		}
	}
	for _, address := range []string{"", "127.0.0.1:5432", "[::1]:5432"} {
		if err := ValidateDialAddress(address); err != nil {
			t.Fatal(err)
		}
	}
	var value bool
	for _, check := range []func() error{
		func() error { return ReadRow(nil, o, "SELECT true", nil, &value) },
		func() error {
			return ReadRow(context.Background(), o, strings.Repeat("x", MaxQueryBytes+1), nil, &value)
		},
		func() error {
			return ReadRow(context.Background(), o, "SELECT $1", []any{strings.Repeat("x", MaxValueBytes+1)}, &value)
		},
		func() error { return ReadRow(context.Background(), o, "SELECT $1", []any{map[string]string{}}, &value) },
		func() error { return ReadRow(context.Background(), o, "SELECT true", nil, (*bool)(nil)) },
	} {
		if err := check(); err == nil {
			t.Fatal("invalid read accepted")
		} else {
			var remote *Error
			if errors.As(err, &remote) {
				t.Fatal("invalid read reached connection", err)
			}
		}
	}
}

// Terminate verified TLS in the fixture and relay the PostgreSQL protocol to
// the isolated test service. That service still performs SCRAM authentication.
func sqlFixture(t *testing.T) Options {
	t.Helper()
	dsn := os.Getenv("STEGO_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("STEGO_REQUIRE_POSTGRES") == "1" {
			t.Fatal("PostgreSQL is required")
		}
		t.Skip("set STEGO_TEST_POSTGRES_DSN")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	pair, ca := certificate(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	stopped := make(chan struct{})
	t.Cleanup(func() { listener.Close(); <-stopped; workers.Wait() })
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
				_ = conn.SetDeadline(time.Now().Add(12 * time.Second))
				var request [8]byte
				if _, err := io.ReadFull(conn, request[:]); err != nil {
					return
				}
				if binary.BigEndian.Uint32(request[:4]) != 8 || binary.BigEndian.Uint32(request[4:]) != 80877103 {
					return
				}
				if _, err := conn.Write([]byte{'S'}); err != nil {
					return
				}
				secure := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12})
				if secure.Handshake() != nil {
					return
				}
				upstream, err := net.DialTimeout("tcp", net.JoinHostPort(config.Host, strconv.Itoa(int(config.Port))), time.Second)
				if err != nil {
					t.Error(err)
					return
				}
				defer upstream.Close()
				_ = upstream.SetDeadline(time.Now().Add(12 * time.Second))
				done := make(chan struct{})
				go func() { _, _ = io.Copy(upstream, secure); upstream.Close(); close(done) }()
				_, _ = io.Copy(secure, upstream)
				conn.Close()
				<-done
			}()
		}
	}()
	return Options{Host: "localhost", Port: 5432, User: config.User, Database: config.Database, Password: config.Password, CA: ca, DialAddress: listener.Addr().String()}
}
func TestVerifiedReadAgainstPostgres(t *testing.T) {
	o := sqlFixture(t)
	var text string
	literal := "' OR true; -- private-value"
	if err := ReadRow(context.Background(), o, "SELECT $1::text", []any{literal}, &text); err != nil || text != literal {
		t.Fatal("bound value changed", err)
	}
	if err := ReadRow(context.Background(), o, "SHOW default_transaction_read_only", nil, &text); err != nil || text != "on" {
		t.Fatal("not read-only", err, text)
	}
	err := ReadRow(context.Background(), o, "CREATE TABLE public.stego_read_must_not_write(id int)", nil, &text)
	var failure *Error
	if !errors.As(err, &failure) || failure.SQLState != "25006" {
		t.Fatal("write was not rejected as read-only", err)
	}
	var absent bool
	if err := ReadRow(context.Background(), o, "SELECT to_regclass('public.stego_read_must_not_write') IS NULL", nil, &absent); err != nil || !absent {
		t.Fatal("read changed database state", err)
	}
	if err := ReadRow(context.Background(), o, "SELECT repeat('x',70000)", nil, &text); err == nil {
		t.Fatal("oversized read succeeded")
	}
	var number int64
	if err := ReadRow(context.Background(), o, "SELECT 1::bigint WHERE false", nil, &number); !errors.Is(err, ErrNoRows) {
		t.Fatal("lost no-row result", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := ReadRow(ctx, o, "SELECT pg_sleep(3)", nil, &text); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatal("read did not honor caller deadline", err, time.Since(start))
	}
	wrong := o
	wrong.Password = "wrong-private-password"
	err = ReadRow(context.Background(), wrong, "SELECT true", nil, &text)
	if !errors.As(err, &failure) || failure.Stage != "connect" || failure.SQLState != "28P01" || strings.Contains(err.Error(), wrong.Password) {
		t.Fatal("unsafe authentication result", err)
	}
	for _, change := range []func(*Options){func(o *Options) { o.Host = "wrong.example" }, func(o *Options) { _, o.CA = certificate(t) }} {
		bad := o
		change(&bad)
		if err := ReadRow(context.Background(), bad, "SELECT true", nil, &text); err == nil {
			t.Fatal("accepted unverified TLS")
		}
	}
	if err := ReadRow(context.Background(), o, "SELECT 7::bigint", nil, &number); err != nil || number != 7 {
		t.Fatal("failed recovery after rejected reads", err)
	}
}
