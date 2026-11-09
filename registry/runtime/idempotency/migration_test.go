package idempotency

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"stego.dev/runtime/internal/pgtest"
)

// TestMain gives the PostgreSQL tests of the package their local server (ST-STD-LOCAL-R04).
func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

// testDB opens a new schema on the local PostgreSQL server of pgtest, applies Migration and an application table
// orders that the test handlers write to, and drops the schema when the test ends.
func testDB(t *testing.T) (db, admin *sql.DB) {
	t.Helper()
	cfg, err := pgx.ParseConfig(pgtest.DSN(t))
	if err != nil {
		t.Fatalf("parse the server URL: %v", err)
	}
	admin = stdlib.OpenDB(*cfg)
	var b [6]byte
	_, _ = rand.Read(b[:])
	schema := "idem_" + hex.EncodeToString(b[:])
	ctx := context.Background()
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	scoped := cfg.Copy()
	scoped.RuntimeParams["search_path"] = schema
	db = stdlib.OpenDB(*scoped)
	t.Cleanup(func() {
		_ = db.Close()
		_, _ = admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close()
	})
	if _, err := db.ExecContext(ctx, Migration); err != nil {
		t.Fatalf("apply Migration: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE orders (id text PRIMARY KEY, note text)`); err != nil {
		t.Fatalf("create orders: %v", err)
	}
	return db, admin
}

// insertKey inserts one committed key row with status.
func insertKey(ctx context.Context, db *sql.DB, tenant, principal, key, status string) error {
	var code any
	if status == "completed" {
		code = 201
	}
	now := time.Now()
	_, err := db.ExecContext(ctx, `INSERT INTO idempotency_keys
		(tenant_id, principal_id, key, fingerprint, status, response_code, locked_at, expires_at)
		VALUES ($1, $2, $3, 'f', $4, $5, $6, $7)`, tenant, principal, key, status, code, now, now.Add(time.Hour))
	return err
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func TestMigration(t *testing.T) {
	ctx := context.Background()

	t.Run("ST-SUP-RP07-LC01-T01 The key table has exactly the declared columns", func(t *testing.T) {
		db, _ := testDB(t)
		rows, err := db.QueryContext(ctx, `SELECT column_name FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = 'idempotency_keys'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			got = append(got, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		want := []string{"tenant_id", "principal_id", "key", "fingerprint", "status", "response_code",
			"response_headers", "response_body", "locked_at", "expires_at"}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("columns = %v, want %v", got, want)
		}
	})

	t.Run("ST-SUP-RP07-LC01-T02 A key of 256 characters is refused", func(t *testing.T) {
		db, _ := testDB(t)
		if err := insertKey(ctx, db, "t1", "p1", strings.Repeat("k", 255), "completed"); err != nil {
			t.Fatalf("a key of 255 characters: %v", err)
		}
		if err := insertKey(ctx, db, "t1", "p1", strings.Repeat("k", 256), "completed"); err == nil {
			t.Fatal("a key of 256 characters was accepted")
		}
	})

	t.Run("ST-SUP-RP07-LC01-T03 A status other than in_progress or completed is refused", func(t *testing.T) {
		db, _ := testDB(t)
		for _, status := range []string{"in_progress", "completed"} {
			if err := insertKey(ctx, db, "t1", "p1", status, status); err != nil {
				t.Fatalf("status %s: %v", status, err)
			}
		}
		if err := insertKey(ctx, db, "t1", "p1", "done", "done"); pgCode(err) != "23514" {
			t.Fatalf("status done: err = %v, want a check violation", err)
		}
	})

	t.Run("ST-SUP-RP07-LC01-T04 The primary key is tenant, principal and key", func(t *testing.T) {
		db, _ := testDB(t)
		for _, row := range [][2]string{{"t1", "p1"}, {"t1", "p2"}, {"t2", "p1"}} {
			if err := insertKey(ctx, db, row[0], row[1], "k1", "completed"); err != nil {
				t.Fatalf("insert %v: %v", row, err)
			}
		}
		err := insertKey(ctx, db, "t1", "p1", "k1", "completed")
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != "idempotency_keys_pkey" {
			t.Fatalf("repeat of t1/p1/k1: err = %v, want a violation of the primary key", err)
		}
		var cols string
		if err := db.QueryRowContext(ctx, `SELECT string_agg(a.attname, ',' ORDER BY k.ord)
			FROM pg_index i
			JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord) ON true
			JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
			WHERE i.indrelid = 'idempotency_keys'::regclass AND i.indisprimary`).Scan(&cols); err != nil {
			t.Fatal(err)
		}
		if cols != "tenant_id,principal_id,key" {
			t.Fatalf("primary key = (%s), want (tenant_id,principal_id,key)", cols)
		}
	})
}
