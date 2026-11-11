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

	"github.com/jsell-rh/stego/registry/runtime/internal/pgtest"
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

// loginRole makes a role that can log in with a password, that is no superuser and holds no BYPASSRLS, and gives
// the pool of a connection as that role with the search path schema; the role is dropped when the test ends.
func loginRole(t *testing.T, admin *sql.DB, schema, prefix string) (string, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	var b [6]byte
	_, _ = rand.Read(b[:])
	role := prefix + "_" + hex.EncodeToString(b[:])
	if _, err := admin.ExecContext(ctx, "CREATE ROLE "+role+" LOGIN PASSWORD 'idem-test' NOSUPERUSER NOBYPASSRLS"); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(pgtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.User, cfg.Password = role, "idem-test"
	cfg.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() {
		_ = db.Close()
		_, _ = admin.ExecContext(context.Background(), "DROP OWNED BY "+role)
		_, _ = admin.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+role)
	})
	return role, db
}

// inTenant runs one statement in a transaction whose app.tenant is tenant, and gives the rows it changed.
func inTenant(ctx context.Context, db *sql.DB, tenant, statement string, args ...any) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "SELECT pg_catalog.set_config('app.tenant', $1, true)", tenant); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, statement, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// countIn counts the rows of query in a transaction whose app.tenant is tenant; an empty tenant sets none.
func countIn(t *testing.T, db *sql.DB, tenant, query string) int {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if tenant != "" {
		if _, err := tx.ExecContext(ctx, "SELECT pg_catalog.set_config('app.tenant', $1, true)", tenant); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := tx.QueryRowContext(ctx, query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestKeyTableTenancy(t *testing.T) {
	ctx := context.Background()
	schemaOf := func(t *testing.T, db *sql.DB) string {
		t.Helper()
		var schema string
		if err := db.QueryRowContext(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
			t.Fatal(err)
		}
		return schema
	}

	t.Run("ST-SUP-RP07-LC11-T01 Two tenants cannot see each other's key rows", func(t *testing.T) {
		db, admin := testDB(t)
		schema := schemaOf(t, db)
		role, runtime := loginRole(t, admin, schema, "idem_runtime")
		// The role gets every privilege on the table, so only row-level security keeps the tenants apart.
		for _, grant := range []string{"GRANT USAGE ON SCHEMA " + schema + " TO " + role,
			"GRANT SELECT, INSERT, UPDATE, DELETE ON " + schema + ".idempotency_keys TO " + role} {
			if _, err := admin.ExecContext(ctx, grant); err != nil {
				t.Fatal(err)
			}
		}
		for _, tenant := range []string{"t1", "t2"} {
			for _, key := range []string{"k1", "k2"} {
				if err := insertKey(ctx, db, tenant, "p1", key, "completed"); err != nil {
					t.Fatal(err)
				}
			}
		}
		if n := countIn(t, runtime, "t1", "SELECT count(*) FROM idempotency_keys"); n != 2 {
			t.Fatalf("tenant t1 sees %d key rows, want its own 2", n)
		}
		if n := countIn(t, runtime, "t1", "SELECT count(*) FROM idempotency_keys WHERE tenant_id = 't2'"); n != 0 {
			t.Fatalf("tenant t1 sees %d key rows of t2", n)
		}
		if n := countIn(t, runtime, "", "SELECT count(*) FROM idempotency_keys"); n != 0 {
			t.Fatalf("a transaction with no tenant sees %d key rows", n)
		}
		now := time.Now()
		if _, err := inTenant(ctx, runtime, "t1", `INSERT INTO idempotency_keys (tenant_id, principal_id, key,
			fingerprint, status, locked_at, expires_at) VALUES ('t2', 'p1', 'k3', 'f', 'in_progress', $1, $1)`, now); pgCode(err) != "42501" {
			t.Fatalf("tenant t1 wrote a key row of t2: %v", err)
		}
		if n, err := inTenant(ctx, runtime, "t1", `UPDATE idempotency_keys SET tenant_id = 't2' WHERE key = 'k1'`); pgCode(err) != "42501" {
			t.Fatalf("tenant t1 moved its key row to t2: %d rows, %v", n, err)
		}
		if n, err := inTenant(ctx, runtime, "t1", `UPDATE idempotency_keys SET fingerprint = 'x' WHERE tenant_id = 't2'`); err != nil || n != 0 {
			t.Fatalf("tenant t1 changed %d key rows of t2: %v", n, err)
		}
		if n, err := inTenant(ctx, runtime, "t1", `DELETE FROM idempotency_keys`); err != nil || n != 2 {
			t.Fatalf("tenant t1 deleted %d key rows, want its own 2: %v", n, err)
		}
		if n := countRows(t, db, "SELECT count(*) FROM idempotency_keys WHERE tenant_id = 't2' AND fingerprint = 'f'"); n != 2 {
			t.Fatalf("%d key rows of t2 are left as they were, want 2", n)
		}
		var enabled, forced bool
		if err := db.QueryRowContext(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class
			WHERE oid = 'idempotency_keys'::regclass`).Scan(&enabled, &forced); err != nil || !enabled || !forced {
			t.Fatalf("row-level security enabled %v, forced %v: %v", enabled, forced, err)
		}
	})

	t.Run("ST-SUP-RP07-LC11-T02 Only the purge function spans tenants", func(t *testing.T) {
		db, admin := testDB(t)
		schema := schemaOf(t, db)
		role, runtime := loginRole(t, admin, schema, "idem_runtime")
		for _, grant := range []string{"GRANT USAGE ON SCHEMA " + schema + " TO " + role,
			"GRANT SELECT, INSERT, UPDATE, DELETE ON " + schema + ".idempotency_keys TO " + role} {
			if _, err := admin.ExecContext(ctx, grant); err != nil {
				t.Fatal(err)
			}
		}
		now := time.Now()
		for _, tenant := range []string{"t1", "t2"} {
			if _, err := db.ExecContext(ctx, `INSERT INTO idempotency_keys
				(tenant_id, principal_id, key, fingerprint, status, response_code, locked_at, expires_at)
				SELECT $1, 'p1', 'old' || g, 'f', 'completed', 201, $2, $2 FROM generate_series(1, 3) AS g`,
				tenant, now.Add(-time.Minute)); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO idempotency_keys
				(tenant_id, principal_id, key, fingerprint, status, response_code, locked_at, expires_at)
				VALUES ($1, 'p1', 'live', 'f', 'completed', 201, $2, $3)`, tenant, now, now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
		// PUBLIC has no EXECUTE, so the runtime role cannot purge before the grant of the migration job.
		if _, err := Purge(ctx, runtime, PurgeOptions{}); pgCode(err) != "42501" {
			t.Fatalf("Purge with no grant gave %v, want a refusal", err)
		}
		// A direct delete with no tenant removes nothing.
		if res, err := runtime.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE expires_at <= now()`); err != nil {
			t.Fatal(err)
		} else if n, _ := res.RowsAffected(); n != 0 {
			t.Fatalf("a delete with no tenant removed %d rows", n)
		}
		if _, err := admin.ExecContext(ctx, "GRANT EXECUTE ON FUNCTION "+schema+"."+PurgeFunction+"(timestamptz, integer) TO "+role); err != nil {
			t.Fatal(err)
		}
		// A cut-off two hours ahead is held at the database's clock, so no live key goes.
		future := func() time.Time { return now.Add(2 * time.Hour) }
		n, err := Purge(ctx, runtime, PurgeOptions{Clock: future})
		if err != nil || n != 6 {
			t.Fatalf("Purge = %d, %v; want the 6 expired rows of both tenants", n, err)
		}
		if got := countRows(t, db, "SELECT count(*) FROM idempotency_keys WHERE key = 'live'"); got != 2 {
			t.Fatalf("%d live keys kept, want 2", got)
		}
		for _, limit := range []int{0, -1, MaxPurgeBatch + 1} {
			if _, err := runtime.ExecContext(ctx, "SELECT "+PurgeFunction+"(now(), $1)", limit); err == nil {
				t.Fatalf("the purge function took the limit %d", limit)
			}
		}
		// Purge refuses such a batch before it sends a statement.
		if _, err := Purge(ctx, runtime, PurgeOptions{Batch: MaxPurgeBatch + 1}); err == nil || pgCode(err) != "" {
			t.Fatalf("Purge with a batch above MaxPurgeBatch gave %v, want its own refusal", err)
		}
		var definer, public bool
		var owner, config string
		if err := db.QueryRowContext(ctx, `SELECT p.prosecdef, pg_get_userbyid(p.proowner), array_to_string(p.proconfig, ','),
			has_function_privilege('public', p.oid, 'EXECUTE')
			FROM pg_proc p WHERE p.oid = ($1 || '.' || $2 || '(timestamptz, integer)')::regprocedure`,
			schema, PurgeFunction).Scan(&definer, &owner, &config, &public); err != nil {
			t.Fatal(err)
		}
		if !definer || owner != PurgeRole || config != "search_path=pg_catalog, pg_temp" || public {
			t.Fatalf("the purge function: definer %v, owner %s, config %q, PUBLIC execute %v", definer, owner, config, public)
		}
		// The purge role can change no key row and insert none, also in a transaction whose app.tenant names the
		// tenant of the row, where the tenant policy admits the row.
		var flags string
		if err := db.QueryRowContext(ctx, `SELECT rolcanlogin::text || rolsuper::text || rolbypassrls::text || rolinherit::text
			FROM pg_roles WHERE rolname = $1`, PurgeRole).Scan(&flags); err != nil || flags != "falsefalsefalsefalse" {
			t.Fatalf("the purge role can log in, is a superuser, bypasses row-level security or inherits (%s): %v", flags, err)
		}
		var privileges string
		if err := db.QueryRowContext(ctx, `SELECT string_agg(privilege_type, ',' ORDER BY privilege_type)
			FROM information_schema.role_table_grants WHERE grantee = $1 AND table_schema = $2 AND table_name = 'idempotency_keys'`,
			PurgeRole, schema).Scan(&privileges); err != nil || privileges != "DELETE,SELECT" {
			t.Fatalf("the purge role holds %q on the key table, want DELETE,SELECT: %v", privileges, err)
		}
		for _, tenantSet := range []bool{false, true} {
			for _, statement := range []string{
				`UPDATE idempotency_keys SET expires_at = now() + interval '1 day' WHERE tenant_id = 't1'`,
				`INSERT INTO idempotency_keys (tenant_id, principal_id, key, fingerprint, status, locked_at, expires_at)
					VALUES ('t1', 'p1', 'k', 'f', 'in_progress', now(), now())`,
			} {
				func() {
					tx, err := db.BeginTx(ctx, nil)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = tx.Rollback() }()
					if tenantSet {
						if _, err := tx.ExecContext(ctx, "SELECT pg_catalog.set_config('app.tenant', 't1', true)"); err != nil {
							t.Fatal(err)
						}
					}
					if _, err := tx.ExecContext(ctx, "SET LOCAL ROLE "+PurgeRole); err != nil {
						t.Fatal(err)
					}
					if _, err := tx.ExecContext(ctx, statement); pgCode(err) != "42501" {
						t.Fatalf("the purge role (app.tenant set %v) ran %.40s: %v", tenantSet, statement, err)
					}
				}()
			}
		}
		if n := countRows(t, db, "SELECT count(*) FROM idempotency_keys WHERE key = 'live' AND expires_at > now() + interval '2 hours'"); n != 0 {
			t.Fatalf("%d live keys were moved forward", n)
		}
	})

	t.Run("ST-SUP-RP07-LC11-T03 The migration runs again with no error", func(t *testing.T) {
		db, admin := testDB(t)
		schema := schemaOf(t, db)
		if _, err := db.ExecContext(ctx, Migration); err != nil {
			t.Fatalf("a second run as the superuser: %v", err)
		}
		// An owner that is no superuser and owns the schema, with the membership that the role migration gives; the
		// purge role gets CREATE on the schema only for the change of owner.
		owner, ownerDB := loginRole(t, admin, schema, "idem_owner")
		for _, grant := range []string{
			"GRANT " + PurgeRole + " TO " + owner + " WITH SET TRUE, INHERIT TRUE",
		} {
			if _, err := admin.ExecContext(ctx, grant); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { _, _ = admin.ExecContext(context.Background(), "REVOKE "+PurgeRole+" FROM "+owner) })
		if _, err := admin.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema+" AUTHORIZATION "+owner); err != nil {
			t.Fatal(err)
		}
		for run := 1; run <= 2; run++ {
			if _, err := ownerDB.ExecContext(ctx, Migration); err != nil {
				t.Fatalf("run %d as the owner role: %v", run, err)
			}
		}
		var policies int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_policies WHERE schemaname = $1 AND tablename = 'idempotency_keys'`,
			schema).Scan(&policies); err != nil || policies != 3 {
			t.Fatalf("%d policies on the key table after two runs, want the tenant policy and two of the purge role: %v", policies, err)
		}
		var create bool
		if err := db.QueryRowContext(ctx, "SELECT has_schema_privilege($1, $2, 'CREATE')", PurgeRole, schema).Scan(&create); err != nil || create {
			t.Fatalf("the purge role keeps CREATE on the schema after the runs: %v, %v", create, err)
		}
	})
}
