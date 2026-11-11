package idempotency

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"

	stegoidempotency "github.com/jsell-rh/stego/registry/runtime/idempotency"
)

// This file runs inside a module that holds the emitted idempotency package of the test declaration: the
// collection todos of Todo (title, done, note) with create and patch, patchable title and done. The tests that need
// PostgreSQL use the local server that STEGO_TEST_POSTGRES_DSN names, each in a schema of its own.

func fingerprint(t *testing.T, fn func(*http.Request, []byte) (string, error), method, target, body string) string {
	t.Helper()
	sum, err := fn(httptest.NewRequest(method, target, nil), []byte(body))
	if err != nil {
		t.Fatalf("%s %s %s: %v", method, target, body, err)
	}
	return sum
}

func TestEmitted(t *testing.T) {
	t.Run("ST-SUP-RC30-OC03-T01 The fingerprint covers the method, the path and the canonical body", func(t *testing.T) {
		const path = "/api/todo/v1/todos"
		base := fingerprint(t, FingerprintTodosCreate, "POST", path, `{"title":"a","done":false}`)
		if again := fingerprint(t, FingerprintTodosCreate, "POST", path, " {\n \"done\" : false,\t\"title\":\"a\"} "); again != base {
			t.Fatalf("reordered keys and whitespace: %s, want %s", again, base)
		}
		for name, other := range map[string]string{
			"a changed value":  fingerprint(t, FingerprintTodosCreate, "POST", path, `{"title":"b","done":false}`),
			"a changed path":   fingerprint(t, FingerprintTodosCreate, "POST", "/api/todo/v1/archive", `{"title":"a","done":false}`),
			"a changed method": fingerprint(t, FingerprintTodosCreate, "PATCH", path, `{"title":"a","done":false}`),
		} {
			if other == base {
				t.Errorf("%s gives the same fingerprint", name)
			}
		}
		if _, err := FingerprintTodosCreate(httptest.NewRequest("POST", path, nil), []byte(`{"title":`)); err == nil {
			t.Error("a body that is not JSON gives a fingerprint")
		}
	})

	t.Run("ST-SUP-RC30-OC03-T02 The query string is not in the fingerprint", func(t *testing.T) {
		body := `{"title":"a"}`
		one := fingerprint(t, FingerprintTodosCreate, "POST", "/api/todo/v1/todos?trace=1", body)
		two := fingerprint(t, FingerprintTodosCreate, "POST", "/api/todo/v1/todos?trace=2&x=y", body)
		if one != two {
			t.Fatalf("two queries give %s and %s, want one fingerprint", one, two)
		}
		want, err := stegoidempotency.Fingerprint("POST", "/api/todo/v1/todos", []byte(body))
		if err != nil || one != want {
			t.Fatalf("fingerprint %s, want the runtime's %s over the path without the query (%v)", one, want, err)
		}
	})

	t.Run("ST-SUP-RC30-OC03-T03 Only declared fields are canonicalised", func(t *testing.T) {
		const path = "/api/todo/v1/todos"
		create := fingerprint(t, FingerprintTodosCreate, "POST", path, `{"title":"a"}`)
		if other := fingerprint(t, FingerprintTodosCreate, "POST", path, `{"title":"a","extra":1}`); other != create {
			t.Errorf("a field the create does not declare changes the fingerprint")
		}
		if other := fingerprint(t, FingerprintTodosCreate, "POST", path, `{"title":"a","note":"n"}`); other == create {
			t.Errorf("the declared field note does not change the create fingerprint")
		}
		if other := fingerprint(t, FingerprintTodosCreate, "POST", path, `{"title":"a","done":null}`); other == create {
			t.Errorf("a declared field that is null gives the fingerprint of an absent field")
		}
		const item = "/api/todo/v1/todos/1"
		patch := fingerprint(t, FingerprintTodosPatch, "PATCH", item, `{"done":true}`)
		if other := fingerprint(t, FingerprintTodosPatch, "PATCH", item, `{"done":true,"note":"n"}`); other != patch {
			t.Errorf("note, which is not patchable, changes the patch fingerprint")
		}
		if other := fingerprint(t, FingerprintTodosPatch, "PATCH", item, `{"done":true,"title":"t"}`); other == patch {
			t.Errorf("the patchable field title does not change the patch fingerprint")
		}
	})

	t.Run("ST-SUP-RC30-OC04-T04 The emitted migration makes the key table scoped by tenant, principal and key", func(t *testing.T) {
		got, err := SchemaSQL(purgeRole, runtimeRole)
		want, wantErr := stegoidempotency.Migration(purgeRole, runtimeRole)
		if err != nil || wantErr != nil || got != want {
			t.Fatalf("SchemaSQL is not the runtime's Migration: %v, %v", err, wantErr)
		}
		db, schema := database(t)
		ctx := context.Background()
		if _, err := db.ExecContext(ctx, got); err != nil {
			t.Fatalf("apply SchemaSQL a second time: %v", err)
		}
		if err := insertKey(db, "A", "p1", "K1", time.Now().Add(Retention)); err != nil {
			t.Fatalf("first key row: %v", err)
		}
		var pgErr *pgconn.PgError
		if err := insertKey(db, "A", "p1", "K1", time.Now().Add(Retention)); !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			t.Fatalf("the same tenant, principal and key again: error %v, want a unique violation 23505", err)
		}
		if err := insertKey(db, "A", "p2", "K1", time.Now().Add(Retention)); err != nil {
			t.Fatalf("the same key for another principal: %v", err)
		}
		if err := insertKey(db, "B", "p1", "K1", time.Now().Add(Retention)); err != nil {
			t.Fatalf("the same key in another tenant: %v", err)
		}
		var rows, index int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM idempotency_keys WHERE key = 'K1'`).Scan(&rows); err != nil || rows != 3 {
			t.Fatalf("key rows: %d (%v), want 3", rows, err)
		}
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_indexes
			WHERE schemaname = $1 AND indexname = 'idempotency_keys_expires_at'`, schema).Scan(&index); err != nil || index != 1 {
			t.Fatalf("the index on expires_at: %d (%v), want 1", index, err)
		}
	})

	t.Run("ST-SUP-RC30-OC04-T06 The purge job deletes the expired key rows and keeps the live ones", func(t *testing.T) {
		db, _ := database(t)
		if err := insertKey(db, "A", "p1", "old", time.Now().Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := insertKey(db, "A", "p1", "live", time.Now().Add(Retention)); err != nil {
			t.Fatal(err)
		}
		if _, err := NewPurgeJob(nil); err == nil {
			t.Fatal("a purge job without a database")
		}
		job, err := NewPurgeJob(db)
		if err != nil {
			t.Fatal(err)
		}
		job.Interval = 10 * time.Millisecond
		runs := make(chan error, 16)
		job.Purged = func(_ int64, err error) {
			select {
			case runs <- err:
			default:
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- job.Run(ctx) }()
		for i := 0; i < 2; i++ {
			select {
			case err := <-runs:
				if err != nil {
					t.Fatalf("purge run %d: %v", i, err)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("purge run %d did not happen", i)
			}
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Run after cancel: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Run did not stop after cancel")
		}
		rows, err := db.Query(`SELECT key FROM idempotency_keys ORDER BY key`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var keys []string
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				t.Fatal(err)
			}
			keys = append(keys, key)
		}
		if len(keys) != 1 || keys[0] != "live" {
			t.Fatalf("key rows after the purge: %v, want only live", keys)
		}
	})
}

// database opens a new schema with SchemaSQL applied on the local server, and drops it when the test ends.
func database(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dsn := os.Getenv("STEGO_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Fatal("STEGO_TEST_POSTGRES_DSN names no PostgreSQL server")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	schema := "idem_gen_" + hex.EncodeToString(b[:])
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		_, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		_ = admin.Close()
	})
	if _, err := admin.Exec(`DO $$ BEGIN CREATE ROLE ` + runtimeRole + ` NOLOGIN; EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL; END $$`); err != nil {
		t.Fatal(err)
	}
	text, err := SchemaSQL(purgeRole, runtimeRole)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(text); err != nil {
		t.Fatalf("apply SchemaSQL: %v", err)
	}
	return db, schema
}

// The roles of the test service, which a tenancy role migration would make.
const (
	purgeRole   = "idem_gen_idempotency_purge"
	runtimeRole = "idem_gen_runtime"
)

func insertKey(db *sql.DB, tenant, principal, key string, expires time.Time) error {
	_, err := db.Exec(`INSERT INTO idempotency_keys (tenant_id, principal_id, key, fingerprint, status, locked_at, expires_at)
		VALUES ($1, $2, $3, 'f', 'in_progress', $4, $5)`, tenant, principal, key, time.Now(), expires)
	return err
}
