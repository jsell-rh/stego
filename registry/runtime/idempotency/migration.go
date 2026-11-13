package idempotency

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/jsell-rh/stego/registry/runtime/manifest"
)

// PurgeFunction is the name of the purge function in the schema of the key table.
const PurgeFunction = "idempotency_keys_purge"

// MaxPurgeBatch is the largest batch of one call of the purge function.
const MaxPurgeBatch = 10000

// Migration gives the DDL text that creates the key table idempotency_keys (ST-SUP-RP07-LC01) in the current
// schema, for the purge role purgeRole and the runtime role runtimeRole of the service. A service applies it with its own migrations, as the table
// owner, in one transaction; it runs again with no error on a database that it made. The primary key (tenant_id,
// principal_id, key) scopes a key to one client, so one client's key never collides with another's. A key is at
// most 255 characters, and status is in_progress or completed. response_headers keeps the stored headers in a
// length-prefixed binary form, and response_body keeps the stored body as bytes, so a replay gives both back byte for
// byte. The index on expires_at serves Purge.
//
// The table is tenant-owned (ST-SUP-RP07-LC11, ST-STD-TXN-R01): row-level security is enabled and forced, with the
// tenant policy on the transaction-local tenant setting app.tenant, USING and WITH CHECK, so a request can neither
// see nor write a key row of another tenant. The purge spans tenants only through the SECURITY DEFINER function
// idempotency_keys_purge(timestamptz, integer) of the table's schema, owned by purgeRole, with search_path =
// pg_catalog, pg_temp and the table named with its schema. purgeRole is the NOLOGIN role of this service alone,
// <service role prefix>_idempotency_purge, which the tenancy role migration makes with NOINHERIT; it has a policy
// for SELECT and one for DELETE and holds only those two privileges on the table, so it can change no row, also with
// app.tenant set. PUBLIC gets no EXECUTE on the function; runtimeRole gets it, from purgeRole, the owner. The function takes no cut-off later than the database's clock, so a caller cannot remove a live key, and
// refuses a limit outside 1 to MaxPurgeBatch.
//
// The text makes the function as purgeRole with SET ROLE, so the role that runs it needs membership in purgeRole
// WITH SET TRUE only (INHERIT FALSE is enough), and the session user must be able to SET ROLE back to that role. It
// gives purgeRole USAGE on the schema when it has none, and CREATE only while it makes the function, which it takes
// back in the same transaction; so the role that runs it must be able to grant CREATE on the schema, as its owner
// is. It makes purgeRole when it is missing, which needs CREATEROLE. Migration refuses a role name that is not a
// lower-case SQL name of at most 63 characters, and one name for both roles.
func Migration(purgeRole, runtimeRole string) (string, error) {
	for _, role := range []string{purgeRole, runtimeRole} {
		if !rolePattern.MatchString(role) {
			return "", fmt.Errorf("idempotency: %q is not a role name the migration accepts", role)
		}
	}
	if purgeRole == runtimeRole {
		return "", fmt.Errorf("idempotency: the purge role must be a role apart from the runtime role")
	}
	return strings.NewReplacer("{{purge}}", purgeRole, "{{runtime}}", runtimeRole).Replace(migration), nil
}

// Manifest gives what the code of the key table needs of the database that Migration makes in schema for the same
// roles (ST-SUP-RP07-LC12, ST-SUP-RP15-LC01): the table idempotency_keys with its columns, its checks and its primary
// key, row-level security with the tenant policy for every role and the two purge policies for purgeRole only, and
// exactly these grants of the table: SELECT and DELETE to purgeRole, and SELECT, INSERT and UPDATE to runtimeRole,
// which the grants of the runtime role give it; and the purge function, SECURITY DEFINER with its fixed search path,
// owned by purgeRole, with EXECUTE for runtimeRole only; purgeRole owns no other object, and CREATE on schema goes
// to no role but its owner and the owner of the manifest. schema is a lower-case SQL name that needs no quotes, such as
// public. Manifest refuses the roles that Migration refuses.
func Manifest(schema, purgeRole, runtimeRole string) (manifest.Manifest, error) {
	if !rolePattern.MatchString(schema) {
		return manifest.Manifest{}, fmt.Errorf("idempotency: %q is not a schema name the manifest accepts", schema)
	}
	text, err := Migration(purgeRole, runtimeRole)
	if err != nil {
		return manifest.Manifest{}, err
	}
	_, rest, found := strings.Cut(text, "AS $fn$")
	source, _, closed := strings.Cut(rest, "$fn$")
	if !found || !closed {
		return manifest.Manifest{}, fmt.Errorf("idempotency: the migration has no function %s", PurgeFunction)
	}
	columns := []manifest.Column{
		{Name: "tenant_id", Type: "text", NotNull: true}, {Name: "principal_id", Type: "text", NotNull: true},
		{Name: "key", Type: "character varying(255)", NotNull: true}, {Name: "fingerprint", Type: "text", NotNull: true},
		{Name: "status", Type: "text", NotNull: true}, {Name: "response_code", Type: "integer"},
		{Name: "response_headers", Type: "bytea"}, {Name: "response_body", Type: "bytea"},
		{Name: "locked_at", Type: "timestamp with time zone", NotNull: true}, {Name: "expires_at", Type: "timestamp with time zone", NotNull: true},
	}
	constraints := []manifest.Constraint{
		{Name: "idempotency_keys_pkey", Kind: manifest.KindPrimaryKey, Definition: "PRIMARY KEY (tenant_id, principal_id, key)"},
		{Name: "idempotency_keys_status_check", Kind: manifest.KindCheck, Definition: "CHECK ((status = ANY (ARRAY['in_progress'::text, 'completed'::text])))"},
		{Name: "idempotency_keys_check", Kind: manifest.KindCheck, Definition: "CHECK (((status <> 'completed'::text) OR (response_code IS NOT NULL)))"},
	}
	wall := "(tenant_id = current_setting('app.tenant'::text, true))"
	policies := []manifest.Policy{
		{Name: "idempotency_keys_tenant", Command: "*", Using: wall, WithCheck: wall},
		{Name: "idempotency_keys_purge_select", Command: "r", Using: "true", Roles: []string{purgeRole}},
		{Name: "idempotency_keys_purge_delete", Command: "d", Using: "true", Roles: []string{purgeRole}},
	}
	grants := []manifest.Grant{
		{Role: purgeRole, Privileges: []string{"SELECT", "DELETE"}},
		{Role: runtimeRole, Privileges: []string{"SELECT", "INSERT", "UPDATE"}},
	}
	return manifest.Manifest{
		Tables: []manifest.Table{{Schema: schema, Name: "idempotency_keys", Columns: columns, Constraints: constraints,
			Indexes:     []manifest.Index{{Name: "idempotency_keys_pkey", Columns: []string{"tenant_id", "principal_id", "key"}}},
			RowSecurity: true, Policies: policies, CheckGrants: true, Grants: grants}},
		Functions: []manifest.Function{{Name: schema + "." + PurgeFunction, Args: "timestamptz, integer", Language: "plpgsql",
			Source: strings.ReplaceAll(source, "%1$I", schema), SecurityDefiner: true, Config: []string{"search_path=pg_catalog, pg_temp"},
			Owner: purgeRole, Execute: []string{runtimeRole}, Returns: "bigint"}},
		// The purge function spans tenants with the rights of its owner, so the purge role owns nothing else. The
		// schema may be public, so only its CREATE is checked: no other role can make an overload of the function.
		Schemas:  []manifest.Schema{{Name: schema, Shared: true}},
		Confined: []string{purgeRole},
	}, nil
}

// rolePattern is a lower-case SQL name of at most 63 characters, which needs no quotes.
var rolePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

const migration = `CREATE TABLE IF NOT EXISTS idempotency_keys (
	tenant_id text NOT NULL,
	principal_id text NOT NULL,
	key varchar(255) NOT NULL,
	fingerprint text NOT NULL,
	status text NOT NULL CHECK (status IN ('in_progress', 'completed')),
	response_code integer,
	response_headers bytea,
	response_body bytea,
	locked_at timestamptz NOT NULL,
	expires_at timestamptz NOT NULL,
	PRIMARY KEY (tenant_id, principal_id, key),
	CHECK (status <> 'completed' OR response_code IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS idempotency_keys_expires_at ON idempotency_keys (expires_at);

DO $do$ BEGIN
	-- Only a role with CREATEROLE can run CREATE ROLE, so the text creates the role only when it is missing; the
	-- tenancy role migration makes it. A run at the same time that creates it first is not an error.
	IF NOT EXISTS (SELECT FROM pg_catalog.pg_roles WHERE rolname = '{{purge}}') THEN
		CREATE ROLE {{purge}} NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT;
	END IF;
EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL;
END $do$;

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS idempotency_keys_tenant ON idempotency_keys;
CREATE POLICY idempotency_keys_tenant ON idempotency_keys
	USING (tenant_id = pg_catalog.current_setting('app.tenant', true))
	WITH CHECK (tenant_id = pg_catalog.current_setting('app.tenant', true));
DROP POLICY IF EXISTS idempotency_keys_purge_select ON idempotency_keys;
CREATE POLICY idempotency_keys_purge_select ON idempotency_keys FOR SELECT TO {{purge}} USING (true);
DROP POLICY IF EXISTS idempotency_keys_purge_delete ON idempotency_keys;
CREATE POLICY idempotency_keys_purge_delete ON idempotency_keys FOR DELETE TO {{purge}} USING (true);
REVOKE ALL ON idempotency_keys FROM {{purge}};
GRANT SELECT, DELETE ON idempotency_keys TO {{purge}};

-- The purge role makes the function itself, so the role that runs the text needs only SET on it. CREATE on the
-- schema is given for that alone and taken back below, in the same transaction.
DO $do$
DECLARE
	v_schema text := pg_catalog.current_schema();
	v_role text := current_user;
	v_create boolean;
BEGIN
	IF v_schema IS NULL THEN
		RAISE EXCEPTION 'idempotency: the migration needs a current schema';
	END IF;
	IF NOT pg_catalog.has_schema_privilege('{{purge}}', v_schema, 'USAGE') THEN
		EXECUTE pg_catalog.format('GRANT USAGE ON SCHEMA %I TO {{purge}}', v_schema);
	END IF;
	v_create := pg_catalog.has_schema_privilege('{{purge}}', v_schema, 'CREATE');
	IF NOT v_create THEN
		EXECUTE pg_catalog.format('GRANT CREATE ON SCHEMA %I TO {{purge}}', v_schema);
	END IF;
	SET ROLE {{purge}};
	EXECUTE pg_catalog.format($f$CREATE OR REPLACE FUNCTION %1$I.idempotency_keys_purge(p_now timestamptz, p_limit integer)
RETURNS bigint
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
	v_clock timestamptz := pg_catalog.clock_timestamp();
	v_now timestamptz := LEAST(COALESCE(p_now, v_clock), v_clock);
	v_count bigint;
BEGIN
	IF p_limit IS NULL OR p_limit < 1 OR p_limit > 10000 THEN
		RAISE EXCEPTION 'idempotency: the purge limit is 1 to 10000';
	END IF;
	DELETE FROM %1$I.idempotency_keys WHERE (tenant_id, principal_id, key) IN (
		SELECT k.tenant_id, k.principal_id, k.key FROM %1$I.idempotency_keys AS k
		WHERE k.expires_at <= v_now
		ORDER BY k.expires_at
		LIMIT p_limit);
	GET DIAGNOSTICS v_count = ROW_COUNT;
	RETURN v_count;
END
$fn$$f$, v_schema);
	EXECUTE pg_catalog.format('REVOKE ALL ON FUNCTION %I.idempotency_keys_purge(timestamptz, integer) FROM PUBLIC', v_schema);
	EXECUTE pg_catalog.format('GRANT EXECUTE ON FUNCTION %I.idempotency_keys_purge(timestamptz, integer) TO {{runtime}}', v_schema);
	EXECUTE pg_catalog.format('SET ROLE %I', v_role);
	IF NOT v_create THEN
		EXECUTE pg_catalog.format('REVOKE CREATE ON SCHEMA %I FROM {{purge}}', v_schema);
	END IF;
END
$do$;
`
