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
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"stego.dev/runtime/internal/pgtest"
	"stego.dev/runtime/internal/seal"
	"stego.dev/runtime/outbox"
	"stego.dev/runtime/pgconn"
	"stego.dev/runtime/tenant"
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
		role := "pgconn_runtime_" + hex.EncodeToString(suffix)
		if _, err := db.ExecContext(ctx, "CREATE ROLE "+role+" NOLOGIN"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = db.ExecContext(context.Background(), "DROP OWNED BY "+role)
			_, _ = db.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+role)
		})
		ddl, err := outbox.Migration(role)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			t.Fatal(err)
		}

		listener, err := pgconn.Listen(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		source, err := outbox.NewSource(ctx, listener)
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = source.Run(ctx) }()
		t.Cleanup(source.Close)
		tn, err := tenant.New(tenant.Claim(identity{tenant: "t1"}))
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
			l, err := pgconn.Listen(ctx, pooled)
			if err == nil {
				err = l.Listen(ctx, outbox.NoticeChannel)
				_ = l.Close(ctx)
			}
			if !errors.Is(err, pgconn.ErrPooled) {
				t.Fatalf("the stand-in pooler with %s gave %v", name, err)
			}
		}
		direct, err := pgconn.Listen(ctx, dsn)
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
