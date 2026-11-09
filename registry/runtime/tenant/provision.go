package tenant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ErrProvisioned is the error of Provision for a tenant that the tenant list already holds, retired or not.
var ErrProvisioned = errors.New("tenant: the tenant is already in the tenant list")

// ErrNotProvisioned is the error of Retire for a tenant that the tenant list does not hold, or holds as retired.
var ErrNotProvisioned = errors.New("tenant: the tenant is not an active tenant of the tenant list")

// runtimeRolePlaceholder stands for the quoted runtime role in migrationText.
const runtimeRolePlaceholder = "{{runtime_role}}"

// migrationText creates the tenant list. The runtime role can only read the table; it changes the table only
// through the two functions, which run as the owner and change only the tenant of the transaction.
const migrationText = `CREATE TABLE stego_tenants (
	tenant_id text PRIMARY KEY CHECK (tenant_id <> ''),
	provisioned_at timestamptz NOT NULL DEFAULT now(),
	retired_at timestamptz
);

REVOKE ALL ON stego_tenants FROM PUBLIC;
GRANT SELECT ON stego_tenants TO {{runtime_role}};

CREATE FUNCTION stego_provision_tenant(p_tenant text) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path FROM CURRENT AS $$
BEGIN
	IF p_tenant IS NULL OR p_tenant = '' OR p_tenant IS DISTINCT FROM current_setting('app.tenant', true) THEN
		RAISE EXCEPTION 'stego_provision_tenant: the tenant % is not the tenant of the transaction', p_tenant;
	END IF;
	INSERT INTO stego_tenants (tenant_id) VALUES (p_tenant) ON CONFLICT (tenant_id) DO NOTHING;
	RETURN FOUND;
END
$$;

CREATE FUNCTION stego_retire_tenant(p_tenant text) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path FROM CURRENT AS $$
BEGIN
	IF p_tenant IS NULL OR p_tenant = '' OR p_tenant IS DISTINCT FROM current_setting('app.tenant', true) THEN
		RAISE EXCEPTION 'stego_retire_tenant: the tenant % is not the tenant of the transaction', p_tenant;
	END IF;
	UPDATE stego_tenants SET retired_at = now() WHERE tenant_id = p_tenant AND retired_at IS NULL;
	RETURN FOUND;
END
$$;

REVOKE ALL ON FUNCTION stego_provision_tenant(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION stego_retire_tenant(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION stego_provision_tenant(text) TO {{runtime_role}};
GRANT EXECUTE ON FUNCTION stego_retire_tenant(text) TO {{runtime_role}};
`

// Migration returns the SQL text that creates the tenant list stego_tenants and its two write functions, for the
// migration job to run as the owner role in the service's schema. runtimeRole is the role the service runs as: it
// may read the table and call the functions, and it can write the table only through them (ST-SUP-RP04-LC07).
func Migration(runtimeRole string) (string, error) {
	if runtimeRole == "" {
		return "", errors.New("tenant: no runtime role")
	}
	if strings.ContainsRune(runtimeRole, 0) {
		return "", errors.New("tenant: the runtime role holds a NUL character")
	}
	return strings.ReplaceAll(migrationText, runtimeRolePlaceholder, pgx.Identifier{runtimeRole}.Sanitize()), nil
}

// List reads the tenant list: the tenants of the table stego_tenants that have no retire time, in tenant ID order
// (ST-SUP-RP04-LC07). It reads as the role of db, which is the runtime role in a service.
func List(ctx context.Context, db *sql.DB) ([]Tenant, error) {
	if db == nil {
		return nil, errors.New("tenant: no database")
	}
	rows, err := db.QueryContext(ctx,
		"SELECT tenant_id FROM stego_tenants WHERE retired_at IS NULL ORDER BY tenant_id COLLATE \"C\"")
	if err != nil {
		return nil, fmt.Errorf("tenant: read the tenant list: %w", err)
	}
	defer rows.Close()
	var tenants []Tenant
	for rows.Next() {
		var column Column
		if err := rows.Scan(&column); err != nil {
			return nil, fmt.Errorf("tenant: read the tenant list: %w", err)
		}
		t, err := New(column)
		if err != nil {
			return nil, fmt.Errorf("tenant: read the tenant list: %w", err)
		}
		tenants = append(tenants, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tenant: read the tenant list: %w", err)
	}
	return tenants, nil
}

// Hook is the work that the generated code registers for a tenant's provisioning or retirement. It runs in the
// transaction that writes the tenant list, with that tenant set, so its writes commit with the change of the tenant
// list or not at all.
type Hook func(ctx context.Context, tx *sql.Tx, t Tenant) error

// Provision adds t to the tenant list and runs hooks in order, all in one transaction with t set, and commits only
// when every hook succeeds (ST-SUP-RP04-LC07). It refuses the zero tenant before it sends a statement, and returns
// ErrProvisioned, with no hook run, when the list already holds t. The caller builds t from the Requested source,
// after the permission of the request is decided.
func Provision(ctx context.Context, db *sql.DB, t Tenant, hooks []Hook) error {
	return change(ctx, db, t, hooks, "SELECT stego_provision_tenant($1)", ErrProvisioned)
}

// Retire sets the retire time of t in the tenant list and runs hooks in order, all in one transaction with t set,
// and commits only when every hook succeeds (ST-SUP-RP04-LC07). It deletes no row. It refuses the zero tenant before
// it sends a statement, and returns ErrNotProvisioned, with no hook run, when t is not an active tenant of the list.
func Retire(ctx context.Context, db *sql.DB, t Tenant, hooks []Hook) error {
	return change(ctx, db, t, hooks, "SELECT stego_retire_tenant($1)", ErrNotProvisioned)
}

func change(ctx context.Context, db *sql.DB, t Tenant, hooks []Hook, statement string, unchanged error) error {
	if t.IsZero() {
		return ErrZero
	}
	tx, err := Begin(ctx, db, t, nil)
	if err != nil {
		return err
	}
	var changed bool
	if err := tx.QueryRowContext(ctx, statement, t.id).Scan(&changed); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("tenant: change the tenant list for %s: %w", t.id, err)
	}
	if !changed {
		_ = tx.Rollback()
		return fmt.Errorf("%w: %s", unchanged, t.id)
	}
	for i, hook := range hooks {
		if hook == nil {
			_ = tx.Rollback()
			return fmt.Errorf("tenant: hook %d of tenant %s is nil", i, t.id)
		}
		if err := hook(ctx, tx, t); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("tenant: hook %d of tenant %s: %w", i, t.id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("tenant: commit the change of tenant %s: %w", t.id, err)
	}
	return nil
}
