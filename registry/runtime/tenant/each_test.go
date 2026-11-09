package tenant

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestTenantEach(t *testing.T) {
	t.Run("ST-SUP-RP04-LC05-T01 Each opens one transaction per tenant with that tenant set", func(t *testing.T) {
		env := newPG(t)
		ctx := context.Background()
		tenants := []Tenant{mustTenant(t, "tenant-a"), mustTenant(t, "tenant-b"), mustTenant(t, "tenant-c")}
		var calls []string
		transactions := map[string]bool{}
		err := Each(ctx, env.runtimeDB, tenants, func(ctx context.Context, tx Querier, tenant Tenant) error {
			var setting, xid string
			if err := tx.QueryRowContext(ctx,
				"SELECT current_setting('app.tenant'), txid_current()::text").Scan(&setting, &xid); err != nil {
				return err
			}
			if setting != tenant.ID() {
				t.Errorf("the transaction of %s holds the tenant %q", tenant.ID(), setting)
			}
			calls = append(calls, setting)
			transactions[xid] = true
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(calls, ",") != "tenant-a,tenant-b,tenant-c" {
			t.Fatalf("Each made the calls %v, want one for each of tenant-a, tenant-b and tenant-c", calls)
		}
		if len(transactions) != 3 {
			t.Fatalf("the three calls ran in %d distinct transactions, want 3", len(transactions))
		}
	})

	t.Run("ST-SUP-RP04-LC05-T02 A failed tenant does not stop the others, and every failure is reported at the end", func(t *testing.T) {
		env := newPG(t)
		ctx := context.Background()
		tenants := []Tenant{mustTenant(t, "tenant-a"), mustTenant(t, "tenant-b"), mustTenant(t, "tenant-c")}
		failure := errors.New("the function fails")
		err := Each(ctx, env.runtimeDB, tenants, func(ctx context.Context, tx Querier, tenant Tenant) error {
			if _, err := tx.ExecContext(ctx, "INSERT INTO items (tenant_id, note) VALUES ($1, 'each')", tenant.ID()); err != nil {
				return err
			}
			if tenant.ID() != "tenant-b" {
				return failure
			}
			return nil
		})
		var eachErr *EachError
		if !errors.As(err, &eachErr) {
			t.Fatalf("Each returned %v, want an *EachError", err)
		}
		var failed []string
		for _, f := range eachErr.Failures {
			failed = append(failed, f.Tenant.ID())
		}
		if strings.Join(failed, ",") != "tenant-a,tenant-c" || !errors.Is(err, failure) {
			t.Fatalf("Each reported the failed tenants %v (%v), want tenant-a and tenant-c", failed, err)
		}
		if !strings.Contains(err.Error(), "tenant-a") || !strings.Contains(err.Error(), "tenant-c") {
			t.Fatalf("the error does not name tenant-a and tenant-c: %v", err)
		}
		rows, err := env.db.QueryContext(ctx, "SELECT tenant_id FROM items ORDER BY tenant_id")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var committed []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			committed = append(committed, id)
		}
		if strings.Join(committed, ",") != "tenant-b" {
			t.Fatalf("the committed rows are of %v, want tenant-b only", committed)
		}

		// A cancel while the function runs fails that tenant and the tenants after it with the error of the
		// context, so errors.Is(err, context.Canceled) holds.
		canceled, cancel := context.WithCancel(ctx)
		defer cancel()
		err = Each(canceled, env.runtimeDB, tenants, func(ctx context.Context, tx Querier, tenant Tenant) error {
			cancel()
			return errors.New("the function stops after the cancel")
		})
		if !errors.As(err, &eachErr) || len(eachErr.Failures) != 3 {
			t.Fatalf("Each with a cancel during the function returned %v, want three failures", err)
		}
		for _, f := range eachErr.Failures {
			if !errors.Is(f.Err, context.Canceled) {
				t.Errorf("the failure of %s does not wrap context.Canceled: %v", f.Tenant.ID(), f.Err)
			}
		}
	})
}
