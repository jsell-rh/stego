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
	"example.com/schema-gate/legacy"
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
	if _, err := storage.NewStore(db); !errors.Is(err, storage.ErrSchemaGeneration) {
		t.Fatal("old schema reached storage", err)
	}
}

func TestFreshSchemaAndStableRestart(t *testing.T) {
	db := schemaDB(t)
	if _, err := storage.NewStore(db); !errors.Is(err, storage.ErrSchemaGeneration) {
		t.Fatal("uninitialized store accepted", err)
	}
	if err := storage.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.NewStore(db); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO records(id,name) VALUES('one','kept')").Error; err != nil {
		t.Fatal(err)
	}
	if err := storage.BootstrapSchema(db, func(*gorm.DB) error { return errors.New("completed initialization was replayed") }); err != nil {
		t.Fatal(err)
	}
	if err := storage.Migrate(db); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := db.Raw("SELECT name FROM records WHERE id='one'").Scan(&name).Error; err != nil || name != "kept" {
		t.Fatal("restart changed data", err)
	}
}
func TestLegacyAndUnknownSchemasRemainUnchanged(t *testing.T) {
	for _, populated := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty-legacy", true: "populated-legacy"}[populated], func(t *testing.T) {
			db := schemaDB(t)
			if err := legacy.Migrate(db); err != nil {
				t.Fatal(err)
			}
			if populated {
				if err := db.Exec("INSERT INTO records(id,name) VALUES('legacy','kept')").Error; err != nil {
					t.Fatal(err)
				}
			}
			requireRejected(t, db)
			if !errors.Is(storage.Migrate(db), storage.ErrSchemaGeneration) {
				t.Fatal("migration accepted an old schema")
			}
			noMarker(t, db)
			if _, err := legacy.NewStore(db); err != nil {
				t.Fatal("old process can no longer open its schema", err)
			}
			var count int64
			if err := db.Table("records").Count(&count).Error; err != nil || count != map[bool]int64{false: 0, true: 1}[populated] {
				t.Fatal("rejection changed old data", err)
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
		for _, query := range []string{"REVOKE USAGE ON SCHEMA stego_schema FROM " + quoted, "REVOKE SELECT ON stego_schema.generation FROM " + quoted, "DROP ROLE " + quoted} {
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
			_, err := storage.NewStore(tx)
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
	if err := db.Exec("GRANT USAGE ON SCHEMA stego_schema TO " + quoted + "; GRANT SELECT ON stego_schema.generation TO " + quoted).Error; err != nil {
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
