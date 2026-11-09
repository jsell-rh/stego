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

		// The table CHECK keeps the rule of a tenant ID, so no row of the list can stop the iterator.
		for _, bad := range []string{"", "t\x01", strings.Repeat("x", 256)} {
			if _, err := env.db.ExecContext(ctx, "INSERT INTO stego_tenants (tenant_id) VALUES ($1)", bad); err == nil {
				t.Errorf("the tenant list accepted the tenant ID %q", bad)
			}
		}

		// A temporary table stego_tenants of the runtime role, on the connection that runs Provision, Retire and
		// List, is never the tenant list: the functions and List name the table with its schema.
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
		if err := env.db.QueryRowContext(ctx, "SELECT count(*) FROM stego_tenants WHERE tenant_id = 't5'").Scan(&listed); err != nil || listed != 1 {
			t.Fatalf("after Provision of t5 the tenant list holds %d rows of t5 (%v), want 1", listed, err)
		}
		visited, _ = visit()
		if strings.Join(visited, ",") != "t1,t5" {
			t.Fatalf("with the temporary table Each visited %v, want t1 and t5", visited)
		}
		var temp int
		if err := env.runtimeDB.QueryRowContext(ctx, "SELECT count(*) FROM pg_temp.stego_tenants").Scan(&temp); err != nil {
			t.Fatalf("the temporary table is not on the connection of the test: %v", err)
		}
	})
}
