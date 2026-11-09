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
		writeRow := func(ctx context.Context, tx *sql.Tx, tenant Tenant) error {
			_, err := tx.ExecContext(ctx, "INSERT INTO items (tenant_id, note) VALUES ($1, 'hook')", tenant.ID())
			return err
		}
		hookFailure := errors.New("the hook fails")
		failAfterWrite := func(ctx context.Context, tx *sql.Tx, tenant Tenant) error {
			if err := writeRow(ctx, tx, tenant); err != nil {
				return err
			}
			return hookFailure
		}
		for _, id := range []string{"t2", "t1"} {
			if err := Provision(ctx, env.runtimeDB, mustRequested(t, id), []Hook{writeRow}); err != nil {
				t.Fatalf("provision %s: %v", id, err)
			}
		}
		if err := Provision(ctx, env.runtimeDB, mustRequested(t, "t3"), []Hook{failAfterWrite}); !errors.Is(err, hookFailure) {
			t.Fatalf("provision t3 with a failing hook returned %v, want the hook's error", err)
		}

		visit := func() (visited []string, rows map[string]int) {
			t.Helper()
			rows = map[string]int{}
			err := EachListed(ctx, env.runtimeDB, func(ctx context.Context, tx *sql.Tx, tenant Tenant) error {
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

		if err := Retire(ctx, env.runtimeDB, mustRequested(t, "t2"), nil); err != nil {
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
}
