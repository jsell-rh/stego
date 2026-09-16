package browser

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	schema "example.com/browser-test/out/browser/schema"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestSchemaInputValidation(t *testing.T) {
	if !errors.Is(schema.Verify(nil, nil), schema.ErrSchema) {
		t.Fatal("nil verification accepted")
	}
	if !errors.Is(schema.Bootstrap(context.Background(), nil, "runtime"), schema.ErrSchema) {
		t.Fatal("nil connection accepted")
	}
	if !errors.Is(schema.Bootstrap(nil, new(sql.Conn), "runtime"), schema.ErrSchema) {
		t.Fatal("nil context accepted")
	}
	if !errors.Is(schema.Bootstrap(context.Background(), new(sql.Conn), "bad;role"), schema.ErrSchema) {
		t.Fatal("unsafe role accepted")
	}
}

func TestManagedBrowserSchema(t *testing.T) {
	db := database(t)
	raw, _ := schemaFixtures.Load(db)
	fixture := raw.(schemaFixture)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if schema.Verify(ctx, db) == nil {
		t.Fatal("missing schema accepted")
	}
	// Both owner connections use the same durable schema lock.
	var wait sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			conn, err := fixture.owner.Conn(ctx)
			if err == nil {
				defer conn.Close()
				err = schema.Bootstrap(ctx, conn, fixture.user)
			}
			results <- err
		}()
	}
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal("concurrent schema setup", err)
		}
	}
	if err := schema.Verify(ctx, db); err != nil {
		t.Fatal("runtime verification", err)
	}
	var before, after string
	snapshot := `SELECT string_agg(oid::text||':'||xmin::text,',' ORDER BY oid) FROM pg_catalog.pg_class WHERE relnamespace='public'::regnamespace`
	if err := db.QueryRowContext(ctx, snapshot).Scan(&before); err != nil {
		t.Fatal(err)
	}
	migrate(t, db)
	if err := db.QueryRowContext(ctx, snapshot).Scan(&after); err != nil || before != after {
		t.Fatal("retry changed schema", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO public.stego_browser_sessions(id_hash,payload,state,expires_at) VALUES(decode(repeat('00',32),'hex'),decode(repeat('00',28),'hex'),'login',CURRENT_TIMESTAMP); UPDATE public.stego_browser_sessions SET state='active'; DELETE FROM public.stego_browser_sessions`); err != nil {
		t.Fatal("runtime data operations", err)
	}
	for _, query := range []string{
		"CREATE TABLE public.forbidden(value integer)", "CREATE TEMP TABLE forbidden(value integer)",
		"ALTER TABLE public.stego_browser_sessions ADD COLUMN forbidden integer", "TRUNCATE public.stego_browser_sessions",
	} {
		_, err := db.ExecContext(ctx, query)
		var remote *pgconn.PgError
		if !errors.As(err, &remote) || remote.Code != "42501" {
			t.Fatal("runtime schema operation was not denied", err)
		}
	}
	// A new runtime connection performs the same checks after restart.
	db.SetMaxIdleConns(0)
	if err := schema.Verify(ctx, db); err != nil {
		t.Fatal("runtime reconnect", err)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if !errors.Is(schema.Verify(canceled, db), context.Canceled) {
		t.Fatal("verification lost cancellation")
	}
	ownerConn, err := fixture.owner.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ownerConn.Close()
	if !errors.Is(schema.Bootstrap(canceled, ownerConn, fixture.user), context.Canceled) {
		t.Fatal("schema setup lost cancellation")
	}
	if schema.Verify(ctx, fixture.owner) == nil {
		t.Fatal("owner connection accepted as runtime")
	}
}

func TestBrowserSchemaRefusesDrift(t *testing.T) {
	for name, change := range map[string]string{
		"extra-column":    "ALTER TABLE public.stego_browser_sessions ADD COLUMN extra text",
		"changed-default": "ALTER TABLE public.stego_browser_sessions ALTER COLUMN changed_at SET DEFAULT '2000-01-01'::timestamptz",
		"missing-index":   "DROP INDEX public.stego_browser_sessions_expiry",
		"changed-index":   "DROP INDEX public.stego_browser_sessions_expiry; CREATE INDEX stego_browser_sessions_expiry ON public.stego_browser_sessions(changed_at)",
		"missing-check":   "ALTER TABLE public.stego_browser_sessions DROP CONSTRAINT stego_browser_sessions_payload_check",
		"changed-check":   "ALTER TABLE public.stego_browser_sessions DROP CONSTRAINT stego_browser_sessions_payload_check; ALTER TABLE public.stego_browser_sessions ADD CHECK(octet_length(payload)>0)",
		"row-policy":      "ALTER TABLE public.stego_browser_sessions ENABLE ROW LEVEL SECURITY",
		"public-grant":    "GRANT SELECT ON public.stego_browser_sessions TO PUBLIC",
		"column-grant":    "GRANT SELECT(payload) ON public.stego_browser_sessions TO PUBLIC",
		"trigger":         "CREATE FUNCTION public.changed() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'; CREATE TRIGGER changed BEFORE INSERT ON public.stego_browser_sessions FOR EACH ROW EXECUTE FUNCTION public.changed()",
	} {
		t.Run(name, func(t *testing.T) {
			db := database(t)
			migrate(t, db)
			raw, _ := schemaFixtures.Load(db)
			fixture := raw.(schemaFixture)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := fixture.owner.ExecContext(ctx, change); err != nil {
				t.Fatal("drift fixture", err)
			}
			if schema.Verify(ctx, db) == nil {
				t.Fatal("runtime accepted schema drift")
			}
			conn, err := fixture.owner.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if schema.Bootstrap(ctx, conn, fixture.user) == nil {
				t.Fatal("bootstrap accepted schema drift")
			}
		})
	}
}

func TestBrowserSchemaBootstrapRollsBack(t *testing.T) {
	db := database(t)
	raw, _ := schemaFixtures.Load(db)
	fixture := raw.(schemaFixture)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := fixture.owner.ExecContext(ctx, "ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO PUBLIC"); err != nil {
		t.Fatal(err)
	}
	conn, err := fixture.owner.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if schema.Bootstrap(ctx, conn, fixture.user) == nil {
		t.Fatal("unsafe default table grants accepted")
	}
	var absent bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_catalog.to_regclass('public.stego_browser_sessions') IS NULL").Scan(&absent); err != nil || !absent {
		t.Fatal("failed schema setup left a table", err)
	}
	if _, err := conn.ExecContext(ctx, "ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE SELECT ON TABLES FROM PUBLIC"); err != nil {
		t.Fatal(err)
	}
	if err := schema.Bootstrap(ctx, conn, fixture.user); err != nil {
		t.Fatal("schema recovery", err)
	}
}

func TestBrowserSchemaRefusesRuntimeGrantDrift(t *testing.T) {
	for name, query := range map[string]string{
		"schema-create":       "GRANT CREATE ON SCHEMA public TO ",
		"grant-option":        "GRANT SELECT ON public.stego_browser_sessions TO ",
		"missing-data-access": "REVOKE DELETE ON public.stego_browser_sessions FROM ",
	} {
		t.Run(name, func(t *testing.T) {
			db := database(t)
			migrate(t, db)
			raw, _ := schemaFixtures.Load(db)
			fixture := raw.(schemaFixture)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			query += fixture.user
			if name == "grant-option" {
				query += " WITH GRANT OPTION"
			}
			if _, err := fixture.owner.ExecContext(ctx, query); err != nil {
				t.Fatal("grant fixture", err)
			}
			if schema.Verify(ctx, db) == nil {
				t.Fatal("runtime accepted grant drift")
			}
			conn, err := fixture.owner.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if schema.Bootstrap(ctx, conn, fixture.user) == nil {
				t.Fatal("bootstrap accepted grant drift")
			}
		})
	}
}
