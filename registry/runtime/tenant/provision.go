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

// runtimeRolePlaceholder and schemaPlaceholder stand for the quoted runtime role and the quoted schema in
// migrationText.
const (
	runtimeRolePlaceholder = "{{runtime_role}}"
	schemaPlaceholder      = "{{schema}}"
)

// migrationText creates the tenant list. The runtime role can only read the table; it changes the table only
// through the two functions, which run as the owner and change only the tenant of the transaction. Each function
// has the fixed search_path pg_catalog, pg_temp and names its table with its schema, so no table or function that
// a caller creates, in pg_temp or elsewhere, can stand in for them. The CHECK keeps the rule of a tenant ID, so no
// row can stop the iterator.
const migrationText = `CREATE TABLE {{schema}}.stego_tenants (
	tenant_id text PRIMARY KEY CHECK (octet_length(tenant_id) BETWEEN 1 AND 255 AND tenant_id !~ '[\x01-\x1f\x7f]'),
	provisioned_at timestamptz NOT NULL DEFAULT now(),
	retired_at timestamptz
);

REVOKE ALL ON {{schema}}.stego_tenants FROM PUBLIC;
GRANT SELECT ON {{schema}}.stego_tenants TO {{runtime_role}};

CREATE FUNCTION {{schema}}.stego_provision_tenant(p_tenant text) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
	IF p_tenant IS NULL OR p_tenant = '' OR p_tenant IS DISTINCT FROM pg_catalog.current_setting('app.tenant', true) THEN
		RAISE EXCEPTION 'stego_provision_tenant: the tenant % is not the tenant of the transaction', p_tenant;
	END IF;
	INSERT INTO {{schema}}.stego_tenants (tenant_id) VALUES (p_tenant) ON CONFLICT (tenant_id) DO NOTHING;
	RETURN FOUND;
END
$$;

CREATE FUNCTION {{schema}}.stego_retire_tenant(p_tenant text) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
	IF p_tenant IS NULL OR p_tenant = '' OR p_tenant IS DISTINCT FROM pg_catalog.current_setting('app.tenant', true) THEN
		RAISE EXCEPTION 'stego_retire_tenant: the tenant % is not the tenant of the transaction', p_tenant;
	END IF;
	UPDATE {{schema}}.stego_tenants SET retired_at = pg_catalog.now() WHERE tenant_id = p_tenant AND retired_at IS NULL;
	RETURN FOUND;
END
$$;

REVOKE ALL ON FUNCTION {{schema}}.stego_provision_tenant(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION {{schema}}.stego_retire_tenant(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION {{schema}}.stego_provision_tenant(text) TO {{runtime_role}};
GRANT EXECUTE ON FUNCTION {{schema}}.stego_retire_tenant(text) TO {{runtime_role}};
`

// quoteName quotes one SQL name, such as a schema or a role, and refuses an empty name or one with a NUL.
func quoteName(kind, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("tenant: no %s", kind)
	}
	if strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("tenant: the %s holds a NUL character", kind)
	}
	return pgx.Identifier{name}.Sanitize(), nil
}

// Migration returns the SQL text that creates the tenant list stego_tenants and its two write functions in schema,
// for the migration job to run as the owner role. runtimeRole is the role the service runs as: it may read the
// table and call the functions, and it can write the table only through them (ST-SUP-RP04-LC07).
func Migration(schema, runtimeRole string) (string, error) {
	quotedSchema, err := quoteName("schema", schema)
	if err != nil {
		return "", err
	}
	quotedRole, err := quoteName("runtime role", runtimeRole)
	if err != nil {
		return "", err
	}
	text := strings.ReplaceAll(migrationText, schemaPlaceholder, quotedSchema)
	return strings.ReplaceAll(text, runtimeRolePlaceholder, quotedRole), nil
}

// List reads the tenant list of schema: the tenants of the table stego_tenants that have no retire time, in tenant
// ID order (ST-SUP-RP04-LC07). It names the table with its schema, so a temporary table of the same name is not
// read. It reads as the role of db, which is the runtime role in a service.
func List(ctx context.Context, db *sql.DB, schema string) ([]Tenant, error) {
	if db == nil {
		return nil, errors.New("tenant: no database")
	}
	quotedSchema, err := quoteName("schema", schema)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, "SELECT tenant_id FROM "+quotedSchema+
		".stego_tenants WHERE retired_at IS NULL ORDER BY tenant_id COLLATE \"C\"")
	if err != nil {
		return nil, fmt.Errorf("tenant: read the tenant list: %w", err)
	}
	defer rows.Close()
	var tenants []Tenant
	for rows.Next() {
		t, err := ScanRows(rows)
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
// list or not at all. It gets the transaction as a Querier, so it cannot commit; Provision and Retire commit.
type Hook func(ctx context.Context, tx Querier, t Tenant) error

// Provision adds t to the tenant list of schema and runs hooks in order, all in one transaction with t set, and commits only
// when every hook succeeds (ST-SUP-RP04-LC07). It refuses the zero tenant before it sends a statement, and returns
// ErrProvisioned, with no hook run, when the list already holds t. The caller builds t from the Requested source,
// after the permission of the request is decided.
func Provision(ctx context.Context, db *sql.DB, schema string, t Tenant, hooks []Hook) error {
	return change(ctx, db, schema, t, hooks, "stego_provision_tenant", ErrProvisioned)
}

// Retire sets the retire time of t in the tenant list of schema and runs hooks in order, all in one transaction with t set,
// and commits only when every hook succeeds (ST-SUP-RP04-LC07). It deletes no row. It refuses the zero tenant before
// it sends a statement, and returns ErrNotProvisioned, with no hook run, when t is not an active tenant of the list.
func Retire(ctx context.Context, db *sql.DB, schema string, t Tenant, hooks []Hook) error {
	return change(ctx, db, schema, t, hooks, "stego_retire_tenant", ErrNotProvisioned)
}

func change(ctx context.Context, db *sql.DB, schema string, t Tenant, hooks []Hook, function string, unchanged error) error {
	if t.IsZero() {
		return ErrZero
	}
	quotedSchema, err := quoteName("schema", schema)
	if err != nil {
		return err
	}
	statement := "SELECT " + quotedSchema + "." + function + "($1)"
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
		if err := hook(ctx, querier{tx: tx}, t); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("tenant: hook %d of tenant %s: %w", i, t.id, err)
		}
	}
	return Commit(ctx, tx, t)
}
