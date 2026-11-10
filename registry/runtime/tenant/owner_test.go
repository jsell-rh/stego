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
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// countingServer accepts TCP connections and closes them at once, and counts them.
func countingServer(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var count atomic.Int64
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			count.Add(1)
			_ = conn.Close()
		}
	}()
	return listener.Addr().String(), &count
}

func TestTenantConnectors(t *testing.T) {
	t.Run("ST-SUP-RP04-LC06-T01 The owner connector opens only in the migration job", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		address, count := countingServer(t)
		cfg := Config{DSN: "postgres://owner:secret@" + address + "/service?sslmode=disable&connect_timeout=2"}
		for _, mode := range []string{"api", "worker", "controller", "all"} {
			db, err := OpenOwner(ctx, mode, cfg)
			if err == nil {
				_ = db.Close()
				t.Fatalf("the owner connector opened in the mode %s", mode)
			}
			if !strings.Contains(err.Error(), `"`+mode+`"`) {
				t.Errorf("the refusal in the mode %s does not name the mode: %v", mode, err)
			}
		}
		if got := count.Load(); got != 0 {
			t.Fatalf("the refused calls opened %d connections, want none", got)
		}
		// In the migration job the connector connects: to the counting server, which closes the connection, and
		// to the local PostgreSQL server.
		if db, err := OpenOwner(ctx, MigrationJob, cfg); err == nil {
			_ = db.Close()
		}
		if count.Load() == 0 {
			t.Fatal("the owner connector made no connection in the migration job")
		}
		env := newPG(t)
		db, err := OpenOwner(ctx, MigrationJob, Config{DSN: env.dsn(env.owner)})
		if err != nil {
			t.Fatalf("the owner connector did not open in the migration job: %v", err)
		}
		_ = db.Close()
	})

	t.Run("ST-SUP-RP04-LC06-T02 The runtime connector refuses a role that owns a table", func(t *testing.T) {
		env := newPG(t)
		ctx := context.Background()
		owning := env.role(t, "owning", "")
		adminExec(t, env.db, "CREATE TABLE owned (id int)", "ALTER TABLE owned OWNER TO "+owning)
		db, err := Open(ctx, Config{DSN: env.dsn(owning)})
		if err == nil {
			_ = db.Close()
			t.Fatal("the runtime connector started as a role that owns a table")
		}
		if !strings.Contains(err.Error(), owning) || !strings.Contains(err.Error(), "public.owned") {
			t.Fatalf("the startup error does not name the role %s and the table public.owned: %v", owning, err)
		}
	})

	t.Run("ST-SUP-RP04-LC06-T03 The runtime connector refuses a role with BYPASSRLS", func(t *testing.T) {
		env := newPG(t)
		ctx := context.Background()
		bypassing := env.role(t, "bypassing", "BYPASSRLS")
		db, err := Open(ctx, Config{DSN: env.dsn(bypassing)})
		if err == nil {
			_ = db.Close()
			t.Fatal("the runtime connector started as a role with BYPASSRLS")
		}
		if !strings.Contains(err.Error(), "BYPASSRLS") {
			t.Errorf("the startup error does not name BYPASSRLS: %v", err)
		}
		// A superuser bypasses row-level security as BYPASSRLS does.
		superuser := env.role(t, "superuser", "SUPERUSER")
		if db, err := Open(ctx, Config{DSN: env.dsn(superuser)}); err == nil {
			_ = db.Close()
			t.Error("the runtime connector started as a superuser")
		} else if !strings.Contains(err.Error(), "is a superuser") {
			t.Errorf("the startup error for the superuser does not say so: %v", err)
		}
		plain, err := Open(ctx, Config{DSN: env.dsn(env.runtime)})
		if err != nil {
			t.Fatalf("the runtime connector did not start as the plain runtime role: %v", err)
		}
		_ = plain.Close()
	})
	t.Run("ST-SUP-RP04-LC06-T04 The runtime connector refuses a role that gets superuser or BYPASSRLS through membership", func(t *testing.T) {
		env := newPG(t)
		ctx := context.Background()
		bypassGroup := env.role(t, "bypassgroup", "NOLOGIN BYPASSRLS")
		bypassMember := env.role(t, "bypassmember", "INHERIT")
		superGroup := env.role(t, "supergroup", "NOLOGIN SUPERUSER")
		superMember := env.role(t, "supermember", "INHERIT")
		adminExec(t, env.admin, "GRANT "+bypassGroup+" TO "+bypassMember, "GRANT "+superGroup+" TO "+superMember)
		// A member of the outbox relay role could take it and change every tenant's messages.
		relayMember := env.role(t, "relaymember", "INHERIT")
		adminExec(t, env.admin, `DO $do$ BEGIN CREATE ROLE stego_outbox_relay NOLOGIN;
EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL; END $do$`, "GRANT stego_outbox_relay TO "+relayMember)
		for role, want := range map[string]string{
			bypassMember: "holds BYPASSRLS through its membership of " + bypassGroup,
			superMember:  "is a superuser through its membership of " + superGroup,
			relayMember:  "is a member of the outbox relay or hook roles stego_outbox_relay",
		} {
			db, err := Open(ctx, Config{DSN: env.dsn(role)})
			if err == nil {
				_ = db.Close()
				t.Errorf("the runtime connector started as the role %s", role)
				continue
			}
			if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), role) {
				t.Errorf("the startup error for %s does not say %q: %v", role, want, err)
			}
		}
		plain, err := Open(ctx, Config{DSN: env.dsn(env.runtime)})
		if err != nil {
			t.Fatalf("the runtime connector did not start as the plain runtime role: %v", err)
		}
		_ = plain.Close()
	})
}

func TestOpened(t *testing.T) {
	t.Run("ST-SUP-RP04-LC08-T01 Opened is true only for an open pool of Open or OpenOwner", func(t *testing.T) {
		env := newPG(t)
		ctx := context.Background()
		db, err := Open(ctx, Config{DSN: env.dsn(env.runtime)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		owner, err := OpenOwner(ctx, MigrationJob, Config{DSN: env.dsn(env.owner)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = owner.Close() })
		plain, err := sql.Open("pgx", env.dsn(env.runtime))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = plain.Close() })
		// A copy of the pool value, made by reflection so that vet does not refuse the copy of its lock.
		copied := reflect.New(reflect.TypeOf(db).Elem())
		copied.Elem().Set(reflect.ValueOf(db).Elem())
		for name, c := range map[string]struct {
			db   *sql.DB
			want bool
		}{
			"the pool of Open":        {db, true},
			"the pool of OpenOwner":   {owner, true},
			"a pool of sql.Open":      {plain, false},
			"a copy of the Open pool": {copied.Interface().(*sql.DB), false},
			"no pool":                 {nil, false},
		} {
			if got := Opened(c.db); got != c.want {
				t.Errorf("Opened of %s is %v, want %v", name, got, c.want)
			}
		}
		closed, err := Open(ctx, Config{DSN: env.dsn(env.runtime)})
		if err != nil {
			t.Fatal(err)
		}
		if !Opened(closed) {
			t.Fatal("Opened of a new pool of Open is false")
		}
		_ = closed.Close()
		if Opened(closed) {
			t.Error("Opened of a closed pool of Open is true")
		}

		// Earlier pools are collected; many new pools, of sql.Open and of Open, are checked by concurrent callers
		// with no false positive.
		for i := 0; i < 5; i++ {
			old, err := Open(ctx, Config{DSN: env.dsn(env.runtime)})
			if err != nil {
				t.Fatal(err)
			}
			_ = old.Close()
		}
		for i := 0; i < 5; i++ {
			runtime.GC()
		}
		var fresh []*sql.DB
		for i := 0; i < 3; i++ {
			p, err := Open(ctx, Config{DSN: env.dsn(env.runtime)})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Close() })
			fresh = append(fresh, p)
		}
		var others []*sql.DB
		for i := 0; i < 200; i++ {
			p, err := sql.Open("pgx", env.dsn(env.runtime))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Close() })
			others = append(others, p)
		}
		var wrong atomic.Int64
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for _, p := range others {
					if Opened(p) {
						wrong.Add(1)
					}
				}
				for _, p := range fresh {
					if !Opened(p) {
						wrong.Add(1)
					}
				}
			}()
		}
		wg.Wait()
		if n := wrong.Load(); n != 0 {
			t.Errorf("%d answers of Opened were wrong under concurrent callers", n)
		}
	})
}

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

		// A configuration with an after-connect hook can set a session value, and is refused.
		hooked := opener(env.runtime)
		hooked.AfterConnect = func(context.Context, *pgconn.PgConn) error { return nil }
		netHooked := opener(env.runtime)
		netHooked.AfterNetConnect = func(_ context.Context, _ *pgconn.Config, c net.Conn) (net.Conn, error) { return c, nil }
		for name, c := range map[string]*pgx.ConnConfig{"AfterConnect": hooked, "AfterNetConnect": netHooked} {
			if err := Guard(Config{Conn: c}); err == nil {
				t.Errorf("Guard took a configuration with %s", name)
			}
			if _, err := Open(ctx, Config{Conn: c}); err == nil {
				t.Errorf("Open took a configuration with %s", name)
			}
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
