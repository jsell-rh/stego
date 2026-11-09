package tenant

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestTenantProvisioning(t *testing.T) {
	t.Run("ST-SUP-RP04-LC07-T01 The iterator visits the provisioned tenants, and a failed hook adds no tenant", func(t *testing.T) {
		env := newPG(t)
		ctx := context.Background()
		writeRow := func(ctx context.Context, tx Querier, tenant Tenant) error {
			_, err := tx.ExecContext(ctx, "INSERT INTO items (tenant_id, note) VALUES ($1, 'hook')", tenant.ID())
			return err
		}
		hookFailure := errors.New("the hook fails")
		failAfterWrite := func(ctx context.Context, tx Querier, tenant Tenant) error {
			if err := writeRow(ctx, tx, tenant); err != nil {
				return err
			}
			return hookFailure
		}
		for _, id := range []string{"t2", "t1"} {
			if err := Provision(ctx, env.runtimeDB, "public", mustRequested(t, id), []Hook{writeRow}); err != nil {
				t.Fatalf("provision %s: %v", id, err)
			}
		}
		if err := Provision(ctx, env.runtimeDB, "public", mustRequested(t, "t3"), []Hook{failAfterWrite}); !errors.Is(err, hookFailure) {
			t.Fatalf("provision t3 with a failing hook returned %v, want the hook's error", err)
		}

		visit := func() (visited []string, rows map[string]int) {
			t.Helper()
			rows = map[string]int{}
			err := EachListed(ctx, env.runtimeDB, "public", func(ctx context.Context, tx Querier, tenant Tenant) error {
				visited = append(visited, tenant.ID())
				var count int
				if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM items").Scan(&count); err != nil {
					return err
				}
				rows[tenant.ID()] = count
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			return visited, rows
		}
		visited, rows := visit()
		if strings.Join(visited, ",") != "t1,t2" || rows["t1"] != 1 || rows["t2"] != 1 {
			t.Fatalf("Each visited %v with the hook rows %v, want t1 and t2 in that order with one hook row each", visited, rows)
		}
		var listed, hookRows int
		if err := env.db.QueryRowContext(ctx, "SELECT count(*) FROM stego_tenants WHERE tenant_id = 't3'").Scan(&listed); err != nil {
			t.Fatal(err)
		}
		if err := env.db.QueryRowContext(ctx, "SELECT count(*) FROM items WHERE tenant_id = 't3'").Scan(&hookRows); err != nil {
			t.Fatal(err)
		}
		if listed != 0 || hookRows != 0 {
			t.Fatalf("after the failed hook the list holds %d rows and items %d rows of t3, want none", listed, hookRows)
		}

		if err := Retire(ctx, env.runtimeDB, "public", mustRequested(t, "t2"), nil); err != nil {
			t.Fatalf("retire t2: %v", err)
		}
		visited, _ = visit()
		if strings.Join(visited, ",") != "t1" {
			t.Fatalf("after the retirement of t2 Each visited %v, want t1 only", visited)
		}
		var retired sql.NullTime
		if err := env.db.QueryRowContext(ctx, "SELECT retired_at FROM stego_tenants WHERE tenant_id = 't2'").Scan(&retired); err != nil {
			t.Fatalf("the row of t2 is not in the tenant list after the retirement: %v", err)
		}
		if !retired.Valid {
			t.Fatal("the row of t2 has no retire time after the retirement")
		}
		if err := env.db.QueryRowContext(ctx, "SELECT count(*) FROM items WHERE tenant_id = 't2'").Scan(&hookRows); err != nil || hookRows != 1 {
			t.Fatalf("after the retirement items holds %d rows of t2 (%v), want its hook row", hookRows, err)
		}

		_, err := env.runtimeDB.ExecContext(ctx, "INSERT INTO stego_tenants (tenant_id) VALUES ('t4')")
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("the insert into stego_tenants as the runtime role returned %v, want a refusal for privilege (42501)", err)
		}

	})

	t.Run("ST-SUP-RP04-LC07-T02 The provisioning functions cannot be hijacked through search_path", func(t *testing.T) {
		env := newPG(t)
		ctx := context.Background()
		for _, function := range []string{"stego_provision_tenant", "stego_retire_tenant"} {
			var config string
			var definer bool
			if err := env.db.QueryRowContext(ctx,
				"SELECT array_to_string(coalesce(proconfig, '{}'), ';'), prosecdef FROM pg_proc WHERE proname = $1", function).Scan(&config, &definer); err != nil {
				t.Fatal(err)
			}
			if !definer || config != "search_path=pg_catalog, pg_temp" {
				t.Fatalf("%s has SECURITY DEFINER %v and proconfig %q, want search_path=pg_catalog, pg_temp", function, definer, config)
			}
		}
		// A decoy schema first on the caller's search_path, with a table stego_tenants and functions named like
		// the ones the provisioning statements call; each decoy function raises, so a call of one fails the test.
		adminExec(t, env.db,
			"CREATE SCHEMA decoy",
			"CREATE TABLE decoy.stego_tenants (tenant_id text, provisioned_at timestamptz, retired_at timestamptz)",
			"CREATE FUNCTION decoy.current_setting(text, boolean) RETURNS text LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'decoy current_setting'; END $$",
			"CREATE FUNCTION decoy.set_config(text, text, boolean) RETURNS text LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'decoy set_config'; END $$",
			"CREATE FUNCTION decoy.now() RETURNS timestamptz LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'decoy now'; END $$",
			"GRANT USAGE ON SCHEMA decoy TO "+env.runtime,
			"GRANT ALL ON decoy.stego_tenants TO "+env.runtime,
			"GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA decoy TO "+env.runtime,
		)
		decoyURL := env.url(env.runtime, rolePassword)
		query := decoyURL.Query()
		query.Set("search_path", "decoy,pg_catalog,public")
		decoyURL.RawQuery = query.Encode()
		db := openURL(t, decoyURL, nil)
		db.SetMaxOpenConns(1)
		var path string
		if err := db.QueryRowContext(ctx, "SHOW search_path").Scan(&path); err != nil || strings.ReplaceAll(path, " ", "") != "decoy,pg_catalog,public" {
			t.Fatalf("the caller's search_path is %q (%v), want the decoy schema first", path, err)
		}
		if err := Provision(ctx, db, "public", mustRequested(t, "t1"), nil); err != nil {
			t.Fatalf("Provision with the decoy search_path: %v", err)
		}
		if err := Retire(ctx, db, "public", mustRequested(t, "t1"), nil); err != nil {
			t.Fatalf("Retire with the decoy search_path: %v", err)
		}
		var real, decoy int
		if err := env.db.QueryRowContext(ctx, "SELECT count(*) FROM public.stego_tenants WHERE tenant_id = 't1' AND retired_at IS NOT NULL").Scan(&real); err != nil {
			t.Fatal(err)
		}
		if err := env.db.QueryRowContext(ctx, "SELECT count(*) FROM decoy.stego_tenants").Scan(&decoy); err != nil {
			t.Fatal(err)
		}
		if real != 1 || decoy != 0 {
			t.Fatalf("the real tenant list holds %d retired rows of t1 and the decoy %d rows, want 1 and 0", real, decoy)
		}

		// A temporary table stego_tenants on the connection that runs Provision, Retire and List is never the
		// tenant list either.
		env.runtimeDB.SetMaxOpenConns(1)
		pinned, err := env.runtimeDB.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pinned.ExecContext(ctx, "CREATE TEMP TABLE stego_tenants (tenant_id text PRIMARY KEY,"+
			" provisioned_at timestamptz NOT NULL DEFAULT now(), retired_at timestamptz)"); err != nil {
			t.Fatal(err)
		}
		if _, err := pinned.ExecContext(ctx, "INSERT INTO stego_tenants (tenant_id) VALUES ('t9')"); err != nil {
			t.Fatal(err)
		}
		_ = pinned.Close() // the one connection, with its temporary table, goes back to the pool
		if err := Retire(ctx, env.runtimeDB, "public", mustRequested(t, "t9"), nil); !errors.Is(err, ErrNotProvisioned) {
			t.Fatalf("Retire of t9, which only the temporary table holds, returned %v, want ErrNotProvisioned", err)
		}
		if err := Provision(ctx, env.runtimeDB, "public", mustRequested(t, "t5"), nil); err != nil {
			t.Fatal(err)
		}
		listed, err := List(ctx, env.runtimeDB, "public")
		if err != nil || len(listed) != 1 || listed[0].ID() != "t5" {
			t.Fatalf("with the temporary table List gave %v (%v), want t5 only", listed, err)
		}
		var temp int
		if err := env.runtimeDB.QueryRowContext(ctx, "SELECT count(*) FROM pg_temp.stego_tenants").Scan(&temp); err != nil || temp != 1 {
			t.Fatalf("the temporary table is not on the connection of the test (%d rows, %v)", temp, err)
		}
	})

	t.Run("ST-SUP-RP04-LC07-T03 A tenant ID outside the CHECK is refused", func(t *testing.T) {
		env := newPG(t)
		ctx := context.Background()
		hookRuns := 0
		hook := func(context.Context, Querier, Tenant) error { hookRuns++; return nil }
		for _, bad := range []string{"", strings.Repeat("x", 256), "t\nnewline"} {
			// New refuses the ID by the same rule as the CHECK, so Provision cannot get it; the CHECK refuses it
			// for any other writer of the table.
			if tenant, err := New(Requested(ProvisionAction, bad, allowedFor(bad))); err == nil {
				t.Errorf("New accepted the tenant ID %q", bad)
				_ = Provision(ctx, env.runtimeDB, "public", tenant, []Hook{hook})
			}
			_, err := env.db.ExecContext(ctx, "INSERT INTO stego_tenants (tenant_id) VALUES ($1)", bad)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
				t.Errorf("the CHECK did not refuse the tenant ID %q: %v", bad, err)
			}
		}
		longest := strings.Repeat("x", 255)
		if err := Provision(ctx, env.runtimeDB, "public", mustRequested(t, longest), []Hook{hook}); err != nil {
			t.Fatalf("Provision of a tenant ID of 255 bytes: %v", err)
		}
		var rows int
		if err := env.db.QueryRowContext(ctx, "SELECT count(*) FROM stego_tenants").Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 1 || hookRuns != 1 {
			t.Fatalf("the tenant list holds %d rows and the hook ran %d times, want the one tenant of 255 bytes", rows, hookRuns)
		}
	})
}
