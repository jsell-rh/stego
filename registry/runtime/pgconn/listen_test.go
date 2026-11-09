package pgconn_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"strings"
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

		for _, pooled := range []string{dsn + "&pool_mode=transaction", dsn + "&options=-c%20pool_mode%3Dtransaction"} {
			if !strings.Contains(dsn, "?") {
				pooled = strings.Replace(pooled, "&", "?", 1)
			}
			l, err := pgconn.Listen(ctx, pooled)
			if err == nil {
				_ = l.Close(ctx)
			}
			// The refusal comes at start, from the adapter, before any session opens.
			if err == nil || !strings.Contains(err.Error(), "pool mode") {
				t.Fatalf("the pooled DSN %s gave %v", pooled, err)
			}
		}
	})
}
