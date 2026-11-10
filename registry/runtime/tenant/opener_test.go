package tenant

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// serverTLS makes a CA and a server certificate for localhost and 127.0.0.1, writes the server's certificate and key
// into the data folder of the test server with COPY TO PROGRAM, and turns TLS on with ALTER SYSTEM and a reload. It turns TLS
// off again when the test ends. It gives the pool of the CA.
func serverTLS(t *testing.T, admin *sql.DB) *x509.CertPool {
	t.Helper()
	ctx := context.Background()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "tenant test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: big.NewInt(2),
		Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	var dataDir string
	if err := admin.QueryRowContext(ctx, "SHOW data_directory").Scan(&dataDir); err != nil {
		t.Fatal(err)
	}
	write := func(name, text string) {
		t.Helper()
		lines := strings.Split(strings.TrimSpace(text), "\n")
		quoted := make([]string, len(lines))
		for i, l := range lines {
			quoted[i] = "('" + l + "')"
		}
		// The server reads a key only when no group or other may read it, so the file is written with umask 077
		// by COPY TO PROGRAM, which the superuser may run.
		if _, err := admin.ExecContext(ctx, "COPY (SELECT line FROM (VALUES "+strings.Join(quoted, ", ")+
			") AS v(line)) TO PROGRAM 'rm -f "+dataDir+"/"+name+" && umask 077 && cat > "+dataDir+"/"+name+"'"); err != nil {
			t.Fatalf("write %s on the server: %v", name, err)
		}
	}
	write("stego_test_server.crt", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})))
	write("stego_test_server.key", string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})))
	for _, statement := range []string{"ALTER SYSTEM SET ssl_cert_file = 'stego_test_server.crt'",
		"ALTER SYSTEM SET ssl_key_file = 'stego_test_server.key'", "ALTER SYSTEM SET ssl = on", "SELECT pg_reload_conf()"} {
		if _, err := admin.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	t.Cleanup(func() {
		for _, statement := range []string{"ALTER SYSTEM RESET ssl", "ALTER SYSTEM RESET ssl_cert_file",
			"ALTER SYSTEM RESET ssl_key_file", "SELECT pg_reload_conf()"} {
			_, _ = admin.ExecContext(context.Background(), statement)
		}
	})
	time.Sleep(200 * time.Millisecond)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return pool
}

func TestOpener(t *testing.T) {
	t.Run("ST-SUP-RP04-LC09-T01 A pool from the adapter's configuration keeps verified TLS and its bounds", func(t *testing.T) {
		env := newPG(t)
		ctx := context.Background()
		roots := serverTLS(t, env.admin)
		// The opener's configuration: the role and database of the test, verified TLS to localhost, no fallback.
		opener := func(role string) *pgx.ConnConfig {
			t.Helper()
			c, err := pgx.ParseConfig(env.url(role, rolePassword).String())
			if err != nil {
				t.Fatal(err)
			}
			c.Fallbacks = nil
			c.TLSConfig = &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12}
			return c
		}
		bounds := PoolBounds{MaxOpen: 3, MaxIdle: 2, MaxLifetime: time.Minute, MaxIdleTime: 30 * time.Second}
		check := func(name string, db *sql.DB) {
			t.Helper()
			defer db.Close()
			var ssl bool
			if err := db.QueryRowContext(ctx, "SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()").Scan(&ssl); err != nil || !ssl {
				t.Fatalf("the %s pool does not connect over TLS: %t, %v", name, ssl, err)
			}
			if !Opened(db) {
				t.Fatalf("Opened is false for the %s pool", name)
			}
			if n := db.Stats().MaxOpenConnections; n != bounds.MaxOpen {
				t.Fatalf("the %s pool holds at most %d connections, want %d", name, n, bounds.MaxOpen)
			}
		}
		runtimeDB, err := Open(ctx, Config{Conn: opener(env.runtime), Pool: bounds})
		if err != nil {
			t.Fatalf("Open with the opener's configuration: %v", err)
		}
		check("runtime", runtimeDB)
		ownerDB, err := OpenOwner(ctx, MigrationJob, Config{Conn: opener(env.owner), Pool: bounds})
		if err != nil {
			t.Fatalf("OpenOwner with the opener's configuration: %v", err)
		}
		check("owner", ownerDB)

		setting := opener(env.runtime)
		setting.RuntimeParams["search_path"] = "elsewhere"
		if _, err := Open(ctx, Config{Conn: setting}); err == nil || !strings.Contains(err.Error(), "search_path") {
			t.Fatalf("a configuration with a session setting gave %v, want the guard's refusal", err)
		}
		if err := Guard(Config{Conn: setting}); err == nil {
			t.Fatal("Guard took a configuration with a session setting")
		}

		// A pool without verified TLS is refused.
		plain := opener(env.runtime)
		plain.TLSConfig = nil
		insecure := opener(env.runtime)
		insecure.TLSConfig = &tls.Config{InsecureSkipVerify: true}
		fallback := opener(env.runtime)
		fallback.Fallbacks = []*pgconn.FallbackConfig{{Host: "127.0.0.1", Port: fallback.Port}}
		for name, c := range map[string]*pgx.ConnConfig{"no TLS": plain, "TLS that skips the check": insecure,
			"a fallback without TLS": fallback} {
			if _, err := Open(ctx, Config{Conn: c}); !errors.Is(err, errNoVerifiedTLS) {
				t.Errorf("a configuration with %s gave %v, want a refusal", name, err)
			}
		}
		// The opener's configuration is used as it is: a DSN beside it is refused, so nothing parses one.
		if _, err := Open(ctx, Config{Conn: opener(env.runtime), DSN: env.url(env.runtime, rolePassword).String()}); err == nil {
			t.Fatal("a configuration with both Conn and DSN was accepted")
		}
		// A certificate of another name is not verified.
		wrong := opener(env.runtime)
		wrong.TLSConfig = &tls.Config{RootCAs: roots, ServerName: "elsewhere.example"}
		if _, err := Open(ctx, Config{Conn: wrong}); err == nil {
			t.Fatal("a server certificate for another name was accepted")
		}
	})
}
