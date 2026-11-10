package idempotency

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// This file runs inside a module that holds the emitted idempotency package. It applies the emitted SchemaSQL to
// the local PostgreSQL server that STEGO_TEST_POSTGRES_DSN names, in a schema of its own.

func TestEmittedMigration(t *testing.T) {
	t.Run("ST-SUP-RC30-OC04-T04 The emitted migration makes the key table scoped by tenant, principal and key", emittedMigration)
}

func emittedMigration(t *testing.T) {
	dsn := os.Getenv("STEGO_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Fatal("STEGO_TEST_POSTGRES_DSN names no PostgreSQL server")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	var b [6]byte
	_, _ = rand.Read(b[:])
	schema := "idem_gen_" + hex.EncodeToString(b[:])
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	defer admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	conn, err := admin.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SET search_path TO "+schema); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, SchemaSQL); err != nil {
		t.Fatalf("apply SchemaSQL: %v", err)
	}
	if _, err := conn.ExecContext(ctx, SchemaSQL); err != nil {
		t.Fatalf("apply SchemaSQL a second time: %v", err)
	}

	insert := func(tenant, principal, key string) error {
		now := time.Now()
		_, err := conn.ExecContext(ctx, `INSERT INTO idempotency_keys
			(tenant_id, principal_id, key, fingerprint, status, locked_at, expires_at)
			VALUES ($1, $2, $3, 'f', 'in_progress', $4, $5)`, tenant, principal, key, now, now.Add(Retention))
		return err
	}
	if err := insert("A", "p1", "K1"); err != nil {
		t.Fatalf("first key row: %v", err)
	}
	var pgErr *pgconn.PgError
	if err := insert("A", "p1", "K1"); !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("the same tenant, principal and key again: error %v, want a unique violation 23505", err)
	}
	if err := insert("A", "p2", "K1"); err != nil {
		t.Fatalf("the same key for another principal: %v", err)
	}
	if err := insert("B", "p1", "K1"); err != nil {
		t.Fatalf("the same key in another tenant: %v", err)
	}
	var rows int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM idempotency_keys WHERE key = 'K1'`).Scan(&rows); err != nil || rows != 3 {
		t.Fatalf("key rows: %d (%v), want 3", rows, err)
	}
	var index int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM pg_indexes
		WHERE schemaname = $1 AND indexname = 'idempotency_keys_expires_at'`, schema).Scan(&index); err != nil || index != 1 {
		t.Fatalf("the index on expires_at: %d (%v), want 1", index, err)
	}
}
