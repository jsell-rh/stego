package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	access "example.com/access-test/out/contracts/databaseaccess"
	"example.com/access-test/out/queue"
	"example.com/access-test/out/storage"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func init() {
	storage.Register("access_test_outbox", func(db *gorm.DB) error { return db.Exec(queue.SchemaSQL).Error })
}

type accessFixture struct {
	admin, pool, runtime               *sql.DB
	owner                              *sql.Conn
	database, ownerRole, role, auditor string
}

func identifier(value string) string { return pgx.Identifier{value}.Sanitize() }

func fixture(t *testing.T) *accessFixture {
	t.Helper()
	dsn := os.Getenv("STEGO_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("STEGO_REQUIRE_POSTGRES") == "1" {
			t.Fatal("PostgreSQL is required")
		}
		t.Skip("PostgreSQL is not configured")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid fixture configuration")
	}
	cfg.ConnectTimeout = 3 * time.Second
	f := &accessFixture{database: "access_" + strings.ReplaceAll(uuid.NewString(), "-", "")}
	f.ownerRole, f.role, f.auditor = f.database+"_o", f.database+"_r", f.database+"_a"
	f.admin = stdlib.OpenDB(*cfg)
	f.admin.SetMaxOpenConns(2)
	t.Cleanup(func() { f.admin.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, role := range []string{f.ownerRole, f.role, f.auditor} {
		options := " NOLOGIN NOINHERIT"
		if role == f.role {
			options = " LOGIN NOINHERIT PASSWORD 'access-test-only-password'"
		}
		if _, err = f.admin.ExecContext(ctx, "CREATE ROLE "+identifier(role)+options); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := f.admin.ExecContext(ctx, "DROP ROLE "+identifier(role)); err != nil {
				t.Error(err)
			}
		})
	}
	if _, err = f.admin.ExecContext(ctx, "CREATE DATABASE "+identifier(f.database)+" OWNER "+identifier(f.ownerRole)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := f.admin.ExecContext(ctx, "DROP DATABASE "+identifier(f.database)+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})
	cfg.Database = f.database
	f.pool = stdlib.OpenDB(*cfg)
	f.pool.SetMaxOpenConns(3)
	t.Cleanup(func() { f.pool.Close() })
	f.owner, err = f.pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.owner.Close() })
	for _, statement := range []string{
		"SET ROLE " + identifier(f.ownerRole),
		"REVOKE ALL ON DATABASE " + identifier(f.database) + " FROM PUBLIC",
		"REVOKE ALL ON SCHEMA public FROM PUBLIC",
	} {
		if _, err = f.owner.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	orm, err := gorm.Open(postgres.New(postgres.Config{Conn: f.owner}), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err = storage.Migrate(orm.WithContext(ctx)); err != nil {
		t.Fatal(err)
	}
	if _, err = f.owner.ExecContext(ctx, "CREATE TABLE public.unrelated(value text); GRANT SELECT ON public.unrelated TO "+identifier(f.auditor)); err != nil {
		t.Fatal(err)
	}
	cfg.User, cfg.Password = f.role, "access-test-only-password"
	f.runtime = stdlib.OpenDB(*cfg)
	f.runtime.SetMaxOpenConns(2)
	t.Cleanup(func() { f.runtime.Close() })
	return f
}

func execute(t *testing.T, db *sql.DB, statement string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, statement); err != nil {
		t.Fatal(err)
	}
}

func grant(t *testing.T, f *accessFixture) {
	t.Helper()
	if err := access.GrantRuntime(context.Background(), f.owner, f.role); err != nil {
		t.Fatal("common installation failed", err)
	}
	if err := access.VerifyRuntime(context.Background(), f.runtime); err != nil {
		t.Fatal("common runtime check failed", err)
	}
}

func assertNoGrants(t *testing.T, f *accessFixture) {
	t.Helper()
	var allowed bool
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := f.pool.QueryRowContext(ctx, `SELECT has_database_privilege($1,current_database(),'CONNECT')
  OR has_schema_privilege($1,'public','USAGE')
  OR has_table_privilege($1,'public.records','SELECT')`, f.role).Scan(&allowed)
	if err != nil || allowed {
		t.Fatal("failed installation left partial grants", err)
	}
}

func denied(t *testing.T, db *sql.DB, statement string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := db.ExecContext(ctx, statement)
	var failure *pgconn.PgError
	if !errors.As(err, &failure) || failure.Code != "42501" {
		t.Fatal("expected a permission denial", err)
	}
}

func TestDatabaseAccessCompleteAndRepeat(t *testing.T) {
	f := fixture(t)
	if !errors.Is(access.VerifyRuntime(context.Background(), f.runtime), access.ErrAccess) {
		t.Fatal("uninstalled runtime passed")
	}
	assertNoGrants(t, f)
	grant(t, f)
	execute(t, f.runtime, "INSERT INTO public.records(id,name) VALUES('first','kept')")
	execute(t, f.runtime, "UPDATE public.records SET name='updated' WHERE id='first'")
	execute(t, f.runtime, `INSERT INTO stego_outbox.messages(id,destination,resource_key,kind,payload)
  VALUES('11111111-1111-4111-8111-111111111111','test','first','created','{}')`)
	orm, err := gorm.Open(postgres.New(postgres.Config{Conn: f.runtime}), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = storage.NewStore(orm); err != nil {
		t.Fatal("generated storage rejected the installed runtime", err)
	}
	grant(t, f)
	var name string
	if err = f.runtime.QueryRow("SELECT name FROM public.records WHERE id='first'").Scan(&name); err != nil || name != "updated" {
		t.Fatal("repeated installation changed data", err)
	}
	var retained bool
	if err = f.pool.QueryRow("SELECT has_table_privilege($1,'public.unrelated','SELECT')", f.auditor).Scan(&retained); err != nil || !retained {
		t.Fatal("operator grant changed", err)
	}
	for _, statement := range []string{
		"UPDATE stego_schema.generation SET generation='changed'",
		"CREATE TABLE public.forbidden(value text)",
		"TRUNCATE public.records",
		"SELECT * FROM public.unrelated",
		"DELETE FROM public.stego_effect_bindings",
		"DELETE FROM public.stego_resource_state",
		"DELETE FROM public.stego_resource_state_scopes",
		"DELETE FROM public.stego_scan_checkpoints",
	} {
		denied(t, f.runtime, statement)
	}
	execute(t, f.pool, "CREATE TABLE public.future_object(value text)")
	denied(t, f.runtime, "SELECT * FROM public.future_object")
	execute(t, f.runtime, "DELETE FROM public.records WHERE id='first'")
	var records int
	if err = f.runtime.QueryRow("SELECT count(*) FROM public.records").Scan(&records); err != nil || records != 0 {
		t.Fatal("required delete access failed", err)
	}
}

func TestDatabaseAccessMissingPermission(t *testing.T) {
	f := fixture(t)
	grant(t, f)
	execute(t, f.pool, "REVOKE SELECT ON stego_schema.generation FROM "+identifier(f.role))
	if !errors.Is(access.VerifyRuntime(context.Background(), f.runtime), access.ErrAccess) {
		t.Fatal("missing marker access passed")
	}
	grant(t, f)
	conn, err := f.runtime.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if !errors.Is(access.GrantRuntime(context.Background(), conn, f.role), access.ErrAccess) {
		t.Fatal("runtime login could install grants")
	}
}

func TestDatabaseAccessRejectsUnsafeSetup(t *testing.T) {
	for _, mode := range []string{"inherit", "membership", "superuser", "createdb", "createrole", "replication", "bypassrls", "nologin", "database-create", "database-temp", "schema-create", "marker-write", "marker-column-write", "public-marker-write", "grant-option", "sequence-update", "wrong-owner", "view", "row-security", "missing-object"} {
		t.Run(mode, func(t *testing.T) {
			f := fixture(t)
			role := identifier(f.role)
			var statement string
			switch mode {
			case "inherit":
				statement = "ALTER ROLE " + role + " INHERIT"
			case "membership":
				statement = "GRANT " + identifier(f.auditor) + " TO " + role
			case "superuser":
				statement = "ALTER ROLE " + role + " SUPERUSER"
			case "createdb":
				statement = "ALTER ROLE " + role + " CREATEDB"
			case "createrole":
				statement = "ALTER ROLE " + role + " CREATEROLE"
			case "replication":
				statement = "ALTER ROLE " + role + " REPLICATION"
			case "bypassrls":
				statement = "ALTER ROLE " + role + " BYPASSRLS"
			case "nologin":
				statement = "ALTER ROLE " + role + " NOLOGIN"
			case "database-create":
				statement = "GRANT CREATE ON DATABASE " + identifier(f.database) + " TO " + role
			case "database-temp":
				statement = "GRANT TEMPORARY ON DATABASE " + identifier(f.database) + " TO PUBLIC"
			case "schema-create":
				statement = "GRANT CREATE ON SCHEMA public TO " + role
			case "marker-write":
				statement = "GRANT UPDATE ON stego_schema.generation TO " + role
			case "marker-column-write":
				statement = "GRANT UPDATE(generation) ON stego_schema.generation TO " + role
			case "public-marker-write":
				statement = "GRANT UPDATE ON stego_schema.generation TO PUBLIC"
			case "grant-option":
				statement = "GRANT SELECT ON public.records TO " + role + " WITH GRANT OPTION"
			case "sequence-update":
				statement = "GRANT UPDATE ON SEQUENCE stego_outbox.messages_sequence_seq TO " + role
			case "wrong-owner":
				statement = "ALTER TABLE public.records OWNER TO " + identifier(f.auditor)
			case "view":
				statement = "DROP TABLE stego_schema.generation; CREATE SEQUENCE public.view_executed; CREATE VIEW stego_schema.generation AS SELECT nextval('public.view_executed') AS value"
			case "row-security":
				statement = "ALTER TABLE public.records ENABLE ROW LEVEL SECURITY"
			case "missing-object":
				statement = "DROP TABLE stego_outbox.messages CASCADE"
			}
			execute(t, f.pool, statement)
			err := access.GrantRuntime(context.Background(), f.owner, f.role)
			if !errors.Is(err, access.ErrAccess) || err.Error() != access.ErrAccess.Error() {
				t.Fatal("unsafe setup accepted or private error exposed", err)
			}
			if mode == "view" {
				var called bool
				if err = f.pool.QueryRow("SELECT is_called FROM public.view_executed").Scan(&called); err != nil || called {
					t.Fatal("unknown view executed", err)
				}
			}
			// Existing operator grants are rejected when unsafe, not silently removed.
			if mode == "marker-write" {
				var retained bool
				if err = f.pool.QueryRow("SELECT has_table_privilege($1,'stego_schema.generation','UPDATE')", f.role).Scan(&retained); err != nil || !retained {
					t.Fatal("installer removed an operator grant", err)
				}
			}
		})
	}
}

func TestDatabaseAccessMutationRollback(t *testing.T) {
	f := fixture(t)
	// The trigger runs after the schema grant. The preceding database CONNECT
	// grant must roll back with it. PostgreSQL excludes shared objects from DDL
	// event triggers, so CONNECT alone cannot cause this injected failure.
	execute(t, f.pool, `CREATE FUNCTION public.reject_access_grant() RETURNS event_trigger LANGUAGE plpgsql AS $body$
 BEGIN RAISE EXCEPTION 'private-grant-error'; END; $body$;
 CREATE EVENT TRIGGER reject_access_grant ON ddl_command_end WHEN TAG IN ('GRANT') EXECUTE FUNCTION public.reject_access_grant()`)
	err := access.GrantRuntime(context.Background(), f.owner, f.role)
	if !errors.Is(err, access.ErrAccess) || strings.Contains(err.Error(), "private-grant-error") {
		t.Fatal("injected failure was accepted or exposed", err)
	}
	assertNoGrants(t, f)
	execute(t, f.pool, "DROP EVENT TRIGGER reject_access_grant; DROP FUNCTION public.reject_access_grant()")
	grant(t, f)
}

func TestDatabaseAccessBoundedLockWait(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	tx, err := f.pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(1937007983,1935894637)"); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = access.GrantRuntime(short, f.owner, f.role)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 3*time.Second {
		t.Fatal("installer exceeded its caller deadline", err)
	}
	assertNoGrants(t, f)
}
