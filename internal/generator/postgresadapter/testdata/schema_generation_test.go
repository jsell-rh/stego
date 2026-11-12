package storage_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"example.com/schema-gate/changed"
	"example.com/schema-gate/future"
	"example.com/schema-gate/storage"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

var bg = context.Background()

func schemaDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("STEGO_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("STEGO_REQUIRE_POSTGRES") == "1" {
			t.Fatal("PostgreSQL fixture is required")
		}
		t.Skip("PostgreSQL fixture is not configured")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { admin.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	name := "schema_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})
	cfg.Database = name
	pool := stdlib.OpenDB(*cfg)
	pool.SetMaxOpenConns(4)
	t.Cleanup(func() { pool.Close() })
	return pool
}

// exec runs one statement on db and fails the test on an error.
func exec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(bg, query, args...); err != nil {
		t.Fatal(err)
	}
}
func noMarker(t *testing.T, db *sql.DB) {
	t.Helper()
	var exists bool
	if err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname='stego_schema')").Scan(&exists); err != nil || exists {
		t.Fatal("rejected or interrupted initialization left a schema marker", err)
	}
}
func requireRejected(t *testing.T, db *sql.DB) {
	t.Helper()
	called := false
	if !errors.Is(storage.BootstrapSchema(bg, db, func(context.Context, *sql.Tx) error { called = true; return nil }), storage.ErrSchemaGeneration) || called {
		t.Fatal("old schema reached initialization")
	}
	if err := storage.VerifySchema(bg, db); !errors.Is(err, storage.ErrSchemaGeneration) {
		t.Fatal("old schema passed the marker check", err)
	}
}

func TestFreshSchemaAndStableRestart(t *testing.T) {
	db := schemaDB(t)
	if err := storage.VerifySchema(bg, db); !errors.Is(err, storage.ErrSchemaGeneration) {
		t.Fatal("uninitialized database accepted", err)
	}
	if err := storage.Migrate(bg, db); err != nil {
		t.Fatal(err)
	}
	if err := storage.VerifySchema(bg, db); err != nil {
		t.Fatal(err)
	}
	exec(t, db, "INSERT INTO records(id,name) VALUES(gen_random_uuid(),'kept')")
	if err := storage.BootstrapSchema(bg, db, func(context.Context, *sql.Tx) error { return errors.New("completed initialization was replayed") }); err != nil {
		t.Fatal(err)
	}
	if err := storage.Migrate(bg, db); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.QueryRow("SELECT count(*) FROM records WHERE name='kept'").Scan(&count); err != nil || count != 1 {
		t.Fatal("restart changed data", err)
	}
}

// TestUnmarkedSchemasRemainUnchanged runs the migration on a database that holds tables but no marker, as a
// database of another program has, and on unknown objects: each is refused and left as it was.
func TestUnmarkedSchemasRemainUnchanged(t *testing.T) {
	for _, populated := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty-table", true: "populated-table"}[populated], func(t *testing.T) {
			db := schemaDB(t)
			exec(t, db, "CREATE TABLE public.records(id text PRIMARY KEY, name text)")
			if populated {
				exec(t, db, "INSERT INTO records(id,name) VALUES('other','kept')")
			}
			requireRejected(t, db)
			if !errors.Is(storage.Migrate(bg, db), storage.ErrSchemaGeneration) {
				t.Fatal("migration accepted an unmarked schema")
			}
			noMarker(t, db)
			var count int64
			if err := db.QueryRow("SELECT count(*) FROM records").Scan(&count); err != nil || count != map[bool]int64{false: 0, true: 1}[populated] {
				t.Fatal("rejection changed the data", err)
			}
		})
	}
	for _, setup := range []string{"CREATE SCHEMA unknown", "CREATE TYPE public.unknown AS ENUM('kept')", "CREATE FUNCTION public.unknown() RETURNS integer LANGUAGE SQL AS 'SELECT 1'"} {
		db := schemaDB(t)
		exec(t, db, setup)
		requireRejected(t, db)
		noMarker(t, db)
	}
}
func TestGenerationAndDefinitionMismatch(t *testing.T) {
	db := schemaDB(t)
	if err := storage.Migrate(bg, db); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(future.Migrate(bg, db), future.ErrSchemaGeneration) {
		t.Fatal("future generation accepted")
	}
	if !errors.Is(changed.Migrate(bg, db), changed.ErrSchemaGeneration) {
		t.Fatal("changed declaration accepted")
	}
	if err := storage.VerifySchema(bg, db); err != nil {
		t.Fatal("mismatch changed the original marker", err)
	}
	var extra bool
	if err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='records' AND column_name='extra')").Scan(&extra); err != nil || extra {
		t.Fatal("mismatch changed the table", err)
	}
}
func TestConcurrentSchemaBootstrap(t *testing.T) {
	db := schemaDB(t)
	var calls atomic.Int32
	var group sync.WaitGroup
	start := make(chan struct{})
	failures := make(chan error, 2)
	for i := 0; i < 2; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			failures <- storage.BootstrapSchema(bg, db, func(ctx context.Context, tx *sql.Tx) error {
				calls.Add(1)
				_, err := tx.ExecContext(ctx, "CREATE TABLE public.once_only(value integer)")
				return err
			})
		}()
	}
	close(start)
	group.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("concurrent startup repeated initialization", calls.Load())
	}
}
func TestInterruptedSchemaBootstrapCanRetry(t *testing.T) {
	for _, mode := range []string{"callback-error", "connection-loss"} {
		t.Run(mode, func(t *testing.T) {
			db := schemaDB(t)
			err := storage.BootstrapSchema(bg, db, func(ctx context.Context, tx *sql.Tx) error {
				if _, err := tx.ExecContext(ctx, "CREATE TABLE public.interrupted(value integer)"); err != nil {
					return err
				}
				if mode == "callback-error" {
					return errors.New("injected initialization error")
				}
				var pid int
				if err := tx.QueryRowContext(ctx, "SELECT pg_catalog.pg_backend_pid()").Scan(&pid); err != nil {
					return err
				}
				_, err := db.ExecContext(ctx, "SELECT pg_catalog.pg_terminate_backend($1)", pid)
				return err
			})
			if err == nil {
				t.Fatal("interrupted initialization passed")
			}
			noMarker(t, db)
			var exists bool
			if err := db.QueryRow("SELECT pg_catalog.to_regclass('public.interrupted') IS NOT NULL").Scan(&exists); err != nil || exists {
				t.Fatal("partial schema survived rollback", err)
			}
			if err := storage.Migrate(bg, db); err != nil {
				t.Fatal("retry failed", err)
			}
		})
	}
}
func TestUnknownMarkerCannotExecuteView(t *testing.T) {
	db := schemaDB(t)
	exec(t, db, `CREATE SCHEMA stego_schema; CREATE SEQUENCE public.view_calls;
 CREATE FUNCTION public.marker_side_effect() RETURNS text LANGUAGE plpgsql AS $$BEGIN PERFORM nextval('public.view_calls'); RETURN 'fresh-v1'; END$$;
 CREATE VIEW stego_schema.generation AS SELECT true AS singleton,public.marker_side_effect() AS generation,'unknown'::text AS definition`)
	requireRejected(t, db)
	var called bool
	if err := db.QueryRow("SELECT is_called FROM public.view_calls").Scan(&called); err != nil || called {
		t.Fatal("schema check executed an unknown view", err)
	}
}

func TestSchemaMarkerShapeAndPrivileges(t *testing.T) {
	for _, statement := range []string{
		"ALTER TABLE stego_schema.generation ENABLE ROW LEVEL SECURITY",
		"GRANT INSERT ON stego_schema.generation TO PUBLIC",
		"GRANT CREATE ON SCHEMA stego_schema TO PUBLIC",
		"ALTER TABLE stego_schema.generation ADD COLUMN extra text",
		"ALTER TABLE stego_schema.generation ALTER COLUMN definition TYPE varchar",
	} {
		db := schemaDB(t)
		if err := storage.Migrate(bg, db); err != nil {
			t.Fatal(err)
		}
		exec(t, db, statement)
		requireRejected(t, db)
		var generation string
		if err := db.QueryRow("SELECT generation FROM stego_schema.generation").Scan(&generation); err != nil || generation != storage.SchemaGeneration {
			t.Fatal("rejection changed the marker", err)
		}
	}
	db := schemaDB(t)
	if err := storage.Migrate(bg, db); err != nil {
		t.Fatal(err)
	}
	role := "schema_reader_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{role}.Sanitize()
	exec(t, db, "CREATE ROLE "+quoted+" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT")
	t.Cleanup(func() {
		for _, query := range []string{"REVOKE USAGE ON SCHEMA stego_schema FROM " + quoted, "REVOKE SELECT ON stego_schema.generation FROM " + quoted, "REVOKE SELECT ON stego_schema.identity FROM " + quoted, "DROP ROLE " + quoted} {
			if _, err := db.ExecContext(bg, query); err != nil {
				t.Error(err)
			}
		}
	})
	// asRole runs step on one connection of db, as the role from SET ROLE, which holds for each transaction on it.
	asRole := func(step func(conn *sql.Conn)) {
		t.Helper()
		conn, err := db.Conn(bg)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.ExecContext(bg, "SET ROLE "+quoted); err != nil {
			t.Fatal(err)
		}
		step(conn)
		if _, err := conn.ExecContext(bg, "RESET ROLE"); err != nil {
			t.Fatal(err)
		}
	}
	checkRole := func(want bool) {
		t.Helper()
		asRole(func(conn *sql.Conn) {
			err := storage.VerifySchema(bg, conn)
			if want && err != nil {
				t.Error("read-only role cannot verify the schema", err)
			}
			if !want && !errors.Is(err, storage.ErrSchemaGeneration) {
				t.Error("missing marker read permission was accepted", err)
			}
		})
	}
	checkRole(false)
	exec(t, db, "GRANT USAGE ON SCHEMA stego_schema TO "+quoted+"; GRANT SELECT ON stego_schema.generation TO "+quoted+"; GRANT SELECT ON stego_schema.identity TO "+quoted)
	checkRole(true)
	asRole(func(conn *sql.Conn) {
		if _, err := conn.ExecContext(bg, "UPDATE stego_schema.generation SET generation='other'"); err == nil {
			t.Error("application role can change the marker")
		}
	})
	if err := storage.VerifySchema(bg, db); err != nil {
		t.Fatal("denied write changed the marker", err)
	}
}

func TestSchemaBootstrapHonorsShortDeadline(t *testing.T) {
	db := schemaDB(t)
	held, err := db.BeginTx(bg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Rollback()
	if _, err := held.ExecContext(bg, "SELECT pg_catalog.pg_advisory_xact_lock(1937007983,1935894637)"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	called := false
	start := time.Now()
	err = storage.BootstrapSchema(ctx, db, func(context.Context, *sql.Tx) error { called = true; return nil })
	if err == nil || called || time.Since(start) > 2*time.Second {
		t.Fatal("blocked bootstrap ignored the caller deadline", err)
	}
	if err := held.Rollback(); err != nil {
		t.Fatal(err)
	}
	noMarker(t, db)
	if err := storage.Migrate(bg, db); err != nil {
		t.Fatal("retry after canceled lock failed", err)
	}
}

func ledgerApplied(t *testing.T, db *sql.DB) []string {
	t.Helper()
	versions, err := storage.AppliedMigrations(bg, db)
	if err != nil {
		t.Fatal(err)
	}
	return versions
}

func TestMigrationLedgerRecordsBootstrapAndRestart(t *testing.T) {
	t.Run("ST-SUP-RC01-OC11-T01 The ledger records the bootstrap and a restart applies nothing", testMigrationLedgerRecordsBootstrapAndRestart)
}

func testMigrationLedgerRecordsBootstrapAndRestart(t *testing.T) {
	db := schemaDB(t)
	if err := storage.Migrate(bg, db); err != nil {
		t.Fatal(err)
	}
	applied := ledgerApplied(t, db)
	if len(applied) == 0 {
		t.Fatal("bootstrap recorded no migrations")
	}
	want := "001_initial"
	if applied[0] != want {
		t.Fatal("initial migration is not recorded first", applied)
	}
	// A restart with the same registrations applies nothing new.
	before := ledgerApplied(t, db)
	if err := storage.Migrate(bg, db); err != nil {
		t.Fatal(err)
	}
	after := ledgerApplied(t, db)
	if len(before) != len(after) {
		t.Fatal("restart applied migrations again", before, after)
	}
}

func TestMigrationLedgerAppliesNewRegistrations(t *testing.T) {
	t.Run("ST-SUP-RC01-OC11-T02 A new registration is applied once and recorded", testMigrationLedgerAppliesNewRegistrations)
}

func testMigrationLedgerAppliesNewRegistrations(t *testing.T) {
	db := schemaDB(t)
	if err := storage.Migrate(bg, db); err != nil {
		t.Fatal(err)
	}
	storage.Register("900_ledger_test", storage.Digest("900_ledger_test probe"), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "CREATE TABLE ledger_probe (id boolean PRIMARY KEY)")
		return err
	})
	if err := storage.Migrate(bg, db); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, version := range ledgerApplied(t, db) {
		if version == "900_ledger_test" {
			found = true
		}
	}
	if !found {
		t.Fatal("new registration was not applied")
	}
	// A second run does not re-apply it.
	if err := storage.Migrate(bg, db); err != nil {
		t.Fatal(err)
	}
	// Re-application is rejected even through the SQL entry point.
	if err := storage.ApplyMigration(bg, db, "900_ledger_test", "CREATE TABLE ledger_probe (id boolean PRIMARY KEY)"); err == nil {
		t.Fatal("re-application was accepted")
	}
}

func ledgerDigests(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query("SELECT version, digest FROM stego_schema.migrations ORDER BY version")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	digests := map[string]string{}
	for rows.Next() {
		var version, digest string
		if err := rows.Scan(&version, &digest); err != nil {
			t.Fatal(err)
		}
		digests[version] = digest
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return digests
}

func TestMigrationLedgerRejectsGapsAndDrift(t *testing.T) {
	t.Run("ST-SUP-RC01-OC11-T03 Digest drift and a foreign row are refused", func(t *testing.T) {
		db := schemaDB(t)
		if err := storage.Migrate(bg, db); err != nil {
			t.Fatal(err)
		}
		applied := ledgerApplied(t, db)
		recorded := ledgerDigests(t, db)[applied[0]]
		// An edited migration changes its digest; the ledger row no longer
		// matches the registered history and the next run refuses to continue.
		exec(t, db, "UPDATE stego_schema.migrations SET digest='tampered' WHERE version=$1", applied[0])
		if err := storage.Migrate(bg, db); !errors.Is(err, storage.ErrMigrationLedger) {
			t.Fatal("digest drift was accepted", err)
		}
		// A ledger row for a version the code never registered is a foreign
		// history; it is rejected rather than trusted.
		exec(t, db, "UPDATE stego_schema.migrations SET digest=$1 WHERE version=$2", recorded, applied[0])
		exec(t, db, "INSERT INTO stego_schema.migrations(version,digest) VALUES('zzz_foreign',encode(sha256('zzz_foreign'::bytea),'hex'))")
		if err := storage.Migrate(bg, db); !errors.Is(err, storage.ErrMigrationLedger) {
			t.Fatal("a foreign ledger row was accepted", err)
		}
	})
	t.Run("ST-SUP-RC01-OC11-T04 An edited migration body is refused at startup", func(t *testing.T) {
		db := schemaDB(t)
		if err := storage.Migrate(bg, db); err != nil {
			t.Fatal(err)
		}
		// The ledger holds the body digest of an earlier body; the registered
		// body differs, so startup refuses to continue.
		applied := ledgerApplied(t, db)
		before := ledgerDigests(t, db)
		earlier := storage.Digest("func(ctx context.Context, tx *sql.Tx) error { return nil }")
		for _, version := range applied {
			if before[version] == storage.Digest(version) {
				t.Fatal("a migration records the digest of its name", version)
			}
		}
		exec(t, db, "UPDATE stego_schema.migrations SET digest=$1 WHERE version=$2", earlier, applied[0])
		if err := storage.Migrate(bg, db); !errors.Is(err, storage.ErrMigrationLedger) {
			t.Fatal("an edited migration was accepted", err)
		}
		if got := ledgerDigests(t, db)[applied[0]]; got != earlier {
			t.Fatal("a refused startup changed the ledger", got)
		}
	})
}

func TestApplyMigrationRecordsAndValidates(t *testing.T) {
	db := schemaDB(t)
	if err := storage.Migrate(bg, db); err != nil {
		t.Fatal(err)
	}
	body := "CREATE TABLE ledger_external (id boolean PRIMARY KEY)"
	if err := storage.ApplyMigration(bg, db, "902_ledger_external", body); err != nil {
		t.Fatal(err)
	}
	if err := storage.ApplyMigration(bg, db, "902_ledger_external", body); err == nil {
		t.Fatal("external re-application was accepted")
	}
	// An edited body on the same version is drift.
	if err := storage.ApplyMigration(bg, db, "902_ledger_external", "CREATE TABLE ledger_external2 (id boolean PRIMARY KEY)"); err == nil {
		t.Fatal("digest drift was accepted")
	}
	// A version before the recorded history is a rollback.
	if err := storage.ApplyMigration(bg, db, "000_ledger_rollback", "CREATE TABLE ledger_rollback (id boolean PRIMARY KEY)"); err == nil {
		t.Fatal("a rolled-back version was accepted")
	}
	found := false
	for _, version := range ledgerApplied(t, db) {
		if version == "902_ledger_external" {
			found = true
		}
	}
	if !found {
		t.Fatal("external migration was not recorded")
	}
}

// TestMigrationLedgerAppliesOnceInOrder runs the migration job four times at once on a database whose ledger
// holds the bootstrap, with migrations registered out of their name order, and with two names whose byte order
// is not the order of a linguistic collation such as en_US.
func TestMigrationLedgerAppliesOnceInOrder(t *testing.T) {
	t.Run("ST-SUP-RC01-OC11-T05 Concurrent migration jobs apply each migration once and in name order", func(t *testing.T) {
		db := schemaDB(t)
		if err := storage.Migrate(bg, db); err != nil {
			t.Fatal(err)
		}
		insert := func(name string) storage.MigrationFunc {
			return func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, "INSERT INTO ledger_runs(name) VALUES($1)", name)
				return err
			}
		}
		// Registered after the migrations that need its table: the apply order is the name order.
		storage.Register("980_x_z", storage.Digest("980_x_z probe"), insert("980_x_z"))
		storage.Register("980_xy", storage.Digest("980_xy probe"), insert("980_xy"))
		storage.Register("950_ledger_runs", storage.Digest("950_ledger_runs probe"), func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "CREATE TABLE ledger_runs(seq bigserial PRIMARY KEY, name text NOT NULL); INSERT INTO ledger_runs(name) VALUES('950_ledger_runs')")
			return err
		})
		var group sync.WaitGroup
		start := make(chan struct{})
		failures := make(chan error, 4)
		for i := 0; i < 4; i++ {
			group.Add(1)
			go func() {
				defer group.Done()
				<-start
				failures <- storage.Migrate(bg, db)
			}()
		}
		close(start)
		group.Wait()
		close(failures)
		for err := range failures {
			if err != nil {
				t.Fatal("a concurrent migration job failed", err)
			}
		}
		rows, err := db.Query("SELECT name FROM ledger_runs ORDER BY seq")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var runs []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			runs = append(runs, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if want := []string{"950_ledger_runs", "980_x_z", "980_xy"}; strings.Join(runs, ",") != strings.Join(want, ",") {
			t.Fatalf("the migrations ran %v, want each once in name order %v", runs, want)
		}
		applied := ledgerApplied(t, db)
		if got := strings.Join(applied[len(applied)-3:], ","); got != "950_ledger_runs,980_x_z,980_xy" {
			t.Fatalf("the ledger reads %v, want the byte order of the names", applied)
		}
		// A job after them reads the ledger in the order it was applied and applies nothing.
		if err := storage.Migrate(bg, db); err != nil {
			t.Fatal("a later migration job refused the ledger", err)
		}
		var count int
		if err := db.QueryRow("SELECT count(*) FROM ledger_runs").Scan(&count); err != nil || count != 3 {
			t.Fatal("a later migration job applied a migration again", count, err)
		}
	})
}
