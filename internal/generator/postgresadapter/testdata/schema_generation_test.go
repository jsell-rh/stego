package storage_test

import (
	"context"
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
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func schemaDB(t *testing.T) *gorm.DB {
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
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	return db
}
func noMarker(t *testing.T, db *gorm.DB) {
	t.Helper()
	var exists bool
	if err := db.Raw("SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname='stego_schema')").Scan(&exists).Error; err != nil || exists {
		t.Fatal("rejected or interrupted initialization left a schema marker", err)
	}
}
func requireRejected(t *testing.T, db *gorm.DB) {
	t.Helper()
	called := false
	if !errors.Is(storage.BootstrapSchema(db, func(*gorm.DB) error { called = true; return nil }), storage.ErrSchemaGeneration) || called {
		t.Fatal("old schema reached initialization")
	}
	if err := storage.VerifySchema(db); !errors.Is(err, storage.ErrSchemaGeneration) {
		t.Fatal("old schema passed the marker check", err)
	}
}

func TestFreshSchemaAndStableRestart(t *testing.T) {
	db := schemaDB(t)
	if err := storage.VerifySchema(db); !errors.Is(err, storage.ErrSchemaGeneration) {
		t.Fatal("uninitialized database accepted", err)
	}
	if err := storage.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := storage.VerifySchema(db); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO records(id,name) VALUES(gen_random_uuid(),'kept')").Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.BootstrapSchema(db, func(*gorm.DB) error { return errors.New("completed initialization was replayed") }); err != nil {
		t.Fatal(err)
	}
	if err := storage.Migrate(db); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Raw("SELECT count(*) FROM records WHERE name='kept'").Scan(&count).Error; err != nil || count != 1 {
		t.Fatal("restart changed data", err)
	}
}

// TestUnmarkedSchemasRemainUnchanged runs the migration on a database that holds tables but no marker, as a
// database of another program has, and on unknown objects: each is refused and left as it was.
func TestUnmarkedSchemasRemainUnchanged(t *testing.T) {
	for _, populated := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty-table", true: "populated-table"}[populated], func(t *testing.T) {
			db := schemaDB(t)
			if err := db.Exec("CREATE TABLE public.records(id text PRIMARY KEY, name text)").Error; err != nil {
				t.Fatal(err)
			}
			if populated {
				if err := db.Exec("INSERT INTO records(id,name) VALUES('other','kept')").Error; err != nil {
					t.Fatal(err)
				}
			}
			requireRejected(t, db)
			if !errors.Is(storage.Migrate(db), storage.ErrSchemaGeneration) {
				t.Fatal("migration accepted an unmarked schema")
			}
			noMarker(t, db)
			var count int64
			if err := db.Table("records").Count(&count).Error; err != nil || count != map[bool]int64{false: 0, true: 1}[populated] {
				t.Fatal("rejection changed the data", err)
			}
		})
	}
	for _, setup := range []string{"CREATE SCHEMA unknown", "CREATE TYPE public.unknown AS ENUM('kept')", "CREATE FUNCTION public.unknown() RETURNS integer LANGUAGE SQL AS 'SELECT 1'"} {
		db := schemaDB(t)
		if err := db.Exec(setup).Error; err != nil {
			t.Fatal(err)
		}
		requireRejected(t, db)
		noMarker(t, db)
	}
}
func TestGenerationAndDefinitionMismatch(t *testing.T) {
	db := schemaDB(t)
	if err := storage.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(future.Migrate(db), future.ErrSchemaGeneration) {
		t.Fatal("future generation accepted")
	}
	if !errors.Is(changed.Migrate(db), changed.ErrSchemaGeneration) {
		t.Fatal("changed declaration accepted")
	}
	if err := storage.VerifySchema(db); err != nil {
		t.Fatal("mismatch changed the original marker", err)
	}
	var extra bool
	if err := db.Raw("SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='records' AND column_name='extra')").Scan(&extra).Error; err != nil || extra {
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
			failures <- storage.BootstrapSchema(db, func(tx *gorm.DB) error {
				calls.Add(1)
				return tx.Exec("CREATE TABLE public.once_only(value integer)").Error
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
			err := storage.BootstrapSchema(db, func(tx *gorm.DB) error {
				if err := tx.Exec("CREATE TABLE public.interrupted(value integer)").Error; err != nil {
					return err
				}
				if mode == "callback-error" {
					return errors.New("injected initialization error")
				}
				var pid int
				if err := tx.Raw("SELECT pg_catalog.pg_backend_pid()").Scan(&pid).Error; err != nil {
					return err
				}
				return db.Exec("SELECT pg_catalog.pg_terminate_backend(?)", pid).Error
			})
			if err == nil {
				t.Fatal("interrupted initialization passed")
			}
			noMarker(t, db)
			var exists bool
			if err := db.Raw("SELECT pg_catalog.to_regclass('public.interrupted') IS NOT NULL").Scan(&exists).Error; err != nil || exists {
				t.Fatal("partial schema survived rollback", err)
			}
			if err := storage.Migrate(db); err != nil {
				t.Fatal("retry failed", err)
			}
		})
	}
}
func TestUnknownMarkerCannotExecuteView(t *testing.T) {
	db := schemaDB(t)
	if err := db.Exec(`CREATE SCHEMA stego_schema; CREATE SEQUENCE public.view_calls;
 CREATE FUNCTION public.marker_side_effect() RETURNS text LANGUAGE plpgsql AS $$BEGIN PERFORM nextval('public.view_calls'); RETURN 'fresh-v1'; END$$;
 CREATE VIEW stego_schema.generation AS SELECT true AS singleton,public.marker_side_effect() AS generation,'unknown'::text AS definition`).Error; err != nil {
		t.Fatal(err)
	}
	requireRejected(t, db)
	var called bool
	if err := db.Raw("SELECT is_called FROM public.view_calls").Scan(&called).Error; err != nil || called {
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
		if err := storage.Migrate(db); err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
		requireRejected(t, db)
		var generation string
		if err := db.Raw("SELECT generation FROM stego_schema.generation").Scan(&generation).Error; err != nil || generation != storage.SchemaGeneration {
			t.Fatal("rejection changed the marker", err)
		}
	}
	db := schemaDB(t)
	if err := storage.Migrate(db); err != nil {
		t.Fatal(err)
	}
	role := "schema_reader_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{role}.Sanitize()
	if err := db.Exec("CREATE ROLE " + quoted + " NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT").Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, query := range []string{"REVOKE USAGE ON SCHEMA stego_schema FROM " + quoted, "REVOKE SELECT ON stego_schema.generation FROM " + quoted, "REVOKE SELECT ON stego_schema.identity FROM " + quoted, "DROP ROLE " + quoted} {
			if err := db.Exec(query).Error; err != nil {
				t.Error(err)
			}
		}
	})
	checkRole := func(want bool) {
		t.Helper()
		if err := db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("SET LOCAL ROLE " + quoted).Error; err != nil {
				return err
			}
			err := storage.VerifySchema(tx)
			if want && err != nil {
				t.Error("read-only role cannot verify the schema", err)
			}
			if !want && !errors.Is(err, storage.ErrSchemaGeneration) {
				t.Error("missing marker read permission was accepted", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	checkRole(false)
	if err := db.Exec("GRANT USAGE ON SCHEMA stego_schema TO " + quoted + "; GRANT SELECT ON stego_schema.generation TO " + quoted + "; GRANT SELECT ON stego_schema.identity TO " + quoted).Error; err != nil {
		t.Fatal(err)
	}
	checkRole(true)
	denied := errors.New("expected marker write denial")
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL ROLE " + quoted).Error; err != nil {
			return err
		}
		if tx.Exec("UPDATE stego_schema.generation SET generation='other'").Error == nil {
			t.Error("application role can change the marker")
		}
		return denied
	})
	if !errors.Is(err, denied) {
		t.Fatal(err)
	}
	if err := storage.VerifySchema(db); err != nil {
		t.Fatal("denied write changed the marker", err)
	}
}

func TestSchemaBootstrapHonorsShortDeadline(t *testing.T) {
	db := schemaDB(t)
	held := db.Begin()
	if held.Error != nil {
		t.Fatal(held.Error)
	}
	defer held.Rollback()
	if err := held.Exec("SELECT pg_catalog.pg_advisory_xact_lock(1937007983,1935894637)").Error; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	called := false
	start := time.Now()
	err := storage.BootstrapSchema(db.WithContext(ctx), func(*gorm.DB) error { called = true; return nil })
	if err == nil || called || time.Since(start) > 2*time.Second {
		t.Fatal("blocked bootstrap ignored the caller deadline", err)
	}
	if err := held.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	noMarker(t, db)
	if err := storage.Migrate(db); err != nil {
		t.Fatal("retry after canceled lock failed", err)
	}
}

func ledgerApplied(t *testing.T, db *gorm.DB) []string {
	t.Helper()
	versions, err := storage.AppliedMigrations(db)
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
	if err := storage.Migrate(db); err != nil {
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
	if err := storage.Migrate(db); err != nil {
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
	if err := storage.Migrate(db); err != nil {
		t.Fatal(err)
	}
	storage.Register("900_ledger_test", storage.Digest("900_ledger_test probe"), func(db *gorm.DB) error {
		return db.Exec("CREATE TABLE ledger_probe (id boolean PRIMARY KEY)").Error
	})
	if err := storage.Migrate(db); err != nil {
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
	if err := storage.Migrate(db); err != nil {
		t.Fatal(err)
	}
	// Re-application is rejected even through the SQL entry point.
	if err := storage.ApplyMigration(db, "900_ledger_test", "CREATE TABLE ledger_probe (id boolean PRIMARY KEY)"); err == nil {
		t.Fatal("re-application was accepted")
	}
}

func ledgerDigests(t *testing.T, db *gorm.DB) map[string]string {
	t.Helper()
	var rows []struct{ Version, Digest string }
	if err := db.Raw("SELECT version, digest FROM stego_schema.migrations ORDER BY version").Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	digests := map[string]string{}
	for _, row := range rows {
		digests[row.Version] = row.Digest
	}
	return digests
}

func TestMigrationLedgerRejectsGapsAndDrift(t *testing.T) {
	t.Run("ST-SUP-RC01-OC11-T03 Digest drift and a foreign row are refused", func(t *testing.T) {
		db := schemaDB(t)
		if err := storage.Migrate(db); err != nil {
			t.Fatal(err)
		}
		applied := ledgerApplied(t, db)
		recorded := ledgerDigests(t, db)[applied[0]]
		// An edited migration changes its digest; the ledger row no longer
		// matches the registered history and the next run refuses to continue.
		if err := db.Exec("UPDATE stego_schema.migrations SET digest='tampered' WHERE version=?", applied[0]).Error; err != nil {
			t.Fatal(err)
		}
		if err := storage.Migrate(db); !errors.Is(err, storage.ErrMigrationLedger) {
			t.Fatal("digest drift was accepted", err)
		}
		// A ledger row for a version the code never registered is a foreign
		// history; it is rejected rather than trusted.
		if err := db.Exec("UPDATE stego_schema.migrations SET digest=? WHERE version=?", recorded, applied[0]).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("INSERT INTO stego_schema.migrations(version,digest) VALUES('zzz_foreign',encode(sha256('zzz_foreign'::bytea),'hex'))").Error; err != nil {
			t.Fatal(err)
		}
		if err := storage.Migrate(db); !errors.Is(err, storage.ErrMigrationLedger) {
			t.Fatal("a foreign ledger row was accepted", err)
		}
	})
	t.Run("ST-SUP-RC01-OC11-T04 An edited migration body is refused at startup", func(t *testing.T) {
		db := schemaDB(t)
		if err := storage.Migrate(db); err != nil {
			t.Fatal(err)
		}
		// The ledger holds the body digest of an earlier body; the registered
		// body differs, so startup refuses to continue.
		applied := ledgerApplied(t, db)
		before := ledgerDigests(t, db)
		earlier := storage.Digest("func(db *gorm.DB) error { return nil }")
		for _, version := range applied {
			if before[version] == storage.Digest(version) {
				t.Fatal("a migration records the digest of its name", version)
			}
		}
		if err := db.Exec("UPDATE stego_schema.migrations SET digest=? WHERE version=?", earlier, applied[0]).Error; err != nil {
			t.Fatal(err)
		}
		if err := storage.Migrate(db); !errors.Is(err, storage.ErrMigrationLedger) {
			t.Fatal("an edited migration was accepted", err)
		}
		if got := ledgerDigests(t, db)[applied[0]]; got != earlier {
			t.Fatal("a refused startup changed the ledger", got)
		}
	})
}

func TestApplyMigrationRecordsAndValidates(t *testing.T) {
	db := schemaDB(t)
	if err := storage.Migrate(db); err != nil {
		t.Fatal(err)
	}
	body := "CREATE TABLE ledger_external (id boolean PRIMARY KEY)"
	if err := storage.ApplyMigration(db, "902_ledger_external", body); err != nil {
		t.Fatal(err)
	}
	if err := storage.ApplyMigration(db, "902_ledger_external", body); err == nil {
		t.Fatal("external re-application was accepted")
	}
	// An edited body on the same version is drift.
	if err := storage.ApplyMigration(db, "902_ledger_external", "CREATE TABLE ledger_external2 (id boolean PRIMARY KEY)"); err == nil {
		t.Fatal("digest drift was accepted")
	}
	// A version before the recorded history is a rollback.
	if err := storage.ApplyMigration(db, "000_ledger_rollback", "CREATE TABLE ledger_rollback (id boolean PRIMARY KEY)"); err == nil {
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
