package pgconn_test

import (
	"bufio"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/jsell-rh/stego/registry/runtime/internal/pgtest"
	"github.com/jsell-rh/stego/registry/runtime/internal/seal"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
	"github.com/jsell-rh/stego/registry/runtime/pgconn"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// The test uses the PostgreSQL server of pgtest (ST-STD-LOCAL).
func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

type identity struct {
	seal.IdentityMark
	tenant string
}

func (i identity) TenantClaim() (string, bool) { return i.tenant, i.tenant != "" }

func TestListener(t *testing.T) {
	t.Run("ST-SUP-RP01-LC20-T01 The runtime's listener adapter delivers notices and refuses a pooled DSN", func(t *testing.T) {
		ctx := context.Background()
		dsn := pgtest.Database(t, "pgconn")
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		suffix := make([]byte, 6)
		if _, err := rand.Read(suffix); err != nil {
			t.Fatal(err)
		}
		role, worker := "pgconn_runtime_"+hex.EncodeToString(suffix), "pgconn_worker_"+hex.EncodeToString(suffix)
		for _, r := range []string{role, worker} {
			if _, err := db.ExecContext(ctx, "CREATE ROLE "+r+" NOLOGIN"); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_, _ = db.ExecContext(context.Background(), "DROP OWNED BY "+r)
				_, _ = db.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+r)
			})
		}
		ddl, err := outbox.Migration(role, worker)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			t.Fatal(err)
		}

		listener, err := pgconn.Listen(ctx, loopback(dsn))
		if err != nil {
			t.Fatal(err)
		}
		source, err := outbox.NewSource(ctx, listener, db)
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = source.Run(ctx) }()
		t.Cleanup(source.Close)
		tn, err := tenant.New(tenant.Claim(identity{IdentityMark: seal.NewIdentityMark("t1", true), tenant: "t1"}))
		if err != nil {
			t.Fatal(err)
		}
		sub, err := source.Subscribe(ctx, tn)
		if err != nil {
			t.Fatal(err)
		}
		defer sub.Close()
		q, err := outbox.New(db, outbox.Options{LiveEvents: true})
		if err != nil {
			t.Fatal(err)
		}
		m := outbox.Message{ID: uuid.New(), Destination: "audit", ResourceKey: "doc:1", Kind: "doc.created", Payload: []byte(`{}`)}
		tx, err := tenant.Begin(ctx, db, tn, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := q.Enqueue(ctx, tx, m); err != nil {
			t.Fatal(err)
		}
		if err := tenant.Commit(ctx, tx, tn); err != nil {
			t.Fatal(err)
		}
		wait, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		event, err := sub.Next(wait)
		if err != nil || event.ID != m.ID {
			t.Fatalf("the adapter delivered %+v, %v", event, err)
		}

		// Two local stand-ins of a pooler between the client and the server: one gives the client a backend key
		// of its own, as PgBouncer does; one passes no notification of the server to the client.
		for name, filter := range map[string]func(byte, []byte) ([]byte, bool){
			"own backend key": func(kind byte, body []byte) ([]byte, bool) {
				if kind == 'K' && len(body) >= 4 {
					body[0] ^= 0x40
				}
				return body, true
			},
			"no notifications": func(kind byte, body []byte) ([]byte, bool) { return body, kind != 'A' },
		} {
			pooled := proxy(t, dsn, filter)
			l, err := pgconn.Listen(ctx, loopback(pooled))
			if err == nil {
				err = l.Listen(ctx, outbox.NoticeChannel)
				_ = l.Close(ctx)
			}
			if !errors.Is(err, pgconn.ErrPooled) {
				t.Fatalf("the stand-in pooler with %s gave %v", name, err)
			}
		}
		direct, err := pgconn.Listen(ctx, loopback(dsn))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = direct.Close(ctx) }()
		var pid uint32
		if err := db.QueryRowContext(ctx, `SELECT pid FROM pg_stat_activity WHERE pid = $1`, direct.PID()).Scan(&pid); err != nil || pid != direct.PID() {
			t.Fatalf("PID %d is not a backend of the server: %v", direct.PID(), err)
		}
	})
}

// loopback is the configuration of the local test server of dsn, which the test reaches without TLS on a loopback
// IP address, as the adapter's loopback test mode allows.
func loopback(dsn string) tenant.Config { return tenant.Config{DSN: dsn, InsecureLoopback: true} }

// countingServer is a local TCP stand-in of a hostile server: it counts the connections it accepts and closes each
// at once. It returns the address and the count.
func countingServer(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var accepted atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_ = c.Close()
		}
	}()
	return ln.Addr().String(), &accepted
}

func TestListenerChecks(t *testing.T) {
	t.Run("ST-SUP-RP01-LC20-T02 The listener makes the checks of tenant.Open before and after it connects", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		addr, accepted := countingServer(t)
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			t.Fatal(err)
		}
		// A DSN whose TLS does not verify the server is refused before any connection, so no startup message and no
		// password reach the server.
		for _, mode := range []string{"disable", "allow", "prefer", "require"} {
			dsn := "postgres://victim:s3cret-pass@" + addr + "/db?sslmode=" + mode
			if l, err := pgconn.Listen(ctx, tenant.Config{DSN: dsn}); err == nil {
				_ = l.Close(ctx)
				t.Fatalf("Listen took sslmode=%s", mode)
			}
			if n := accepted.Load(); n != 0 {
				t.Fatalf("with sslmode=%s the server got %d connections, want none", mode, n)
			}
		}
		// The loopback test mode allows no TLS only for a loopback IP address, never a name.
		if _, err := pgconn.Listen(ctx, tenant.Config{DSN: "postgres://victim:s3cret-pass@localhost:" + port + "/db?sslmode=disable",
			InsecureLoopback: true}); err == nil {
			t.Fatal("Listen took the name localhost without TLS")
		}
		// The pooler guard refuses a session setting before any connection.
		for _, dsn := range []string{
			"postgres://victim:s3cret-pass@" + addr + "/db?sslmode=disable&options=-c%20app.tenant%3Dt1",
			"postgres://victim:s3cret-pass@" + addr + "/db?sslmode=disable&search_path=evil",
		} {
			if _, err := pgconn.Listen(ctx, loopback(dsn)); err == nil || !strings.Contains(err.Error(), "pooler guard") {
				t.Fatalf("Listen took the session setting of %s: %v", dsn, err)
			}
		}
		if n := accepted.Load(); n != 0 {
			t.Fatalf("the server got %d connections, want none", n)
		}

		// A session that carries a tenant outside a transaction, here from a role default, is refused after the
		// connect.
		dsn := pgtest.Database(t, "pgconn")
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		suffix := make([]byte, 6)
		if _, err := rand.Read(suffix); err != nil {
			t.Fatal(err)
		}
		role := "pgconn_tenant_" + hex.EncodeToString(suffix)
		for _, statement := range []string{
			"CREATE ROLE " + role + " LOGIN PASSWORD 'pgconn-test'",
			"ALTER ROLE " + role + " SET app.tenant = 't1'",
		} {
			if _, err := db.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { _, _ = db.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+role) })
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword(role, "pgconn-test")
		if l, err := pgconn.Listen(ctx, loopback(u.String())); err == nil {
			_ = l.Close(ctx)
			t.Fatal("Listen took a session that carries app.tenant")
		} else if !strings.Contains(err.Error(), "pooler guard") {
			t.Fatalf("Listen refused the session for another reason: %v", err)
		}
	})
}

// proxy is a local TCP stand-in between a client and the server of dsn. It passes the client's bytes as they are,
// and passes each message of the server through filter, which may change its body or drop it. The DSN must use
// sslmode=disable, so the stream is plain protocol messages. It returns the DSN of the stand-in.
func proxy(t *testing.T, dsn string, filter func(kind byte, body []byte) ([]byte, bool)) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	server := u.Host
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer client.Close()
				backend, err := net.Dial("tcp", server)
				if err != nil {
					return
				}
				defer backend.Close()
				go func() { _, _ = io.Copy(backend, client) }()
				r := bufio.NewReader(backend)
				for {
					head := make([]byte, 5)
					if _, err := io.ReadFull(r, head); err != nil {
						return
					}
					body := make([]byte, int(binary.BigEndian.Uint32(head[1:]))-4)
					if _, err := io.ReadFull(r, body); err != nil {
						return
					}
					out, pass := filter(head[0], body)
					if !pass {
						continue
					}
					binary.BigEndian.PutUint32(head[1:], uint32(len(out)+4))
					if _, err := client.Write(append(head, out...)); err != nil {
						return
					}
				}
			}()
		}
	}()
	u.Host = ln.Addr().String()
	q := u.Query()
	q.Set("sslmode", "disable")
	u.RawQuery = q.Encode()
	return u.String()
}
