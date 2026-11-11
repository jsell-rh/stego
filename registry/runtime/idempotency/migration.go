package idempotency

// PurgeRole is the NOLOGIN role that owns the purge function, the only code that reads and deletes key rows of more
// than one tenant (ST-SUP-RP07-LC11). It is a role of its own: no superuser, no BYPASSRLS, it inherits nothing, it
// holds only SELECT and DELETE on the key table and owns only the purge function. The tenancy role migration makes
// it, gives the owner role membership in it WITH SET TRUE, INHERIT TRUE and gives it USAGE and CREATE on the schema
// public; tenant.Open refuses a runtime role that is a member of it.
const PurgeRole = "stego_idempotency_purge"

// PurgeFunction is the name of the purge function in the schema of the key table.
const PurgeFunction = "idempotency_keys_purge"

// MaxPurgeBatch is the largest batch of one call of the purge function.
const MaxPurgeBatch = 10000

// Migration is the DDL text that creates the key table idempotency_keys (ST-SUP-RP07-LC01) in the current schema.
// A service applies it with its own migrations, as the table owner, in one transaction; it runs again with no error
// on a database that it made. The primary key (tenant_id, principal_id, key) scopes a key to one client, so one
// client's key never collides with another's. A key is at most 255 characters, and status is in_progress or
// completed. response_headers keeps the stored headers in a length-prefixed binary form, and response_body keeps
// the stored body as bytes, so a replay gives both back byte for byte. The index on expires_at serves Purge.
//
// The table is tenant-owned (ST-SUP-RP07-LC11, ST-STD-TXN-R01): row-level security is enabled and forced, with the
// tenant policy on the transaction-local tenant setting app.tenant, USING and WITH CHECK, so a request can neither
// see nor write a key row of another tenant. The purge spans tenants only through the SECURITY DEFINER function
// idempotency_keys_purge(timestamptz, integer) of the table's schema, owned by PurgeRole, with search_path =
// pg_catalog, pg_temp and the table named with its schema. PurgeRole has a policy for SELECT and one for DELETE and
// holds only those two privileges on the table, so it can change no row, also with app.tenant set. PUBLIC gets no
// EXECUTE on the function: the runtime role gets it from the grants of the migration job. The function takes no
// cut-off later than the database's clock, so a caller cannot remove a live key, and refuses a limit outside 1 to
// MaxPurgeBatch. The text makes PurgeRole when it is missing, which needs CREATEROLE. A role other than a superuser
// that runs the text needs membership in PurgeRole WITH SET TRUE, INHERIT TRUE. The text gives PurgeRole USAGE on
// the schema when it has none, and CREATE for the change of owner only, which it takes back in the same
// transaction; a role that cannot grant CREATE on the schema needs PurgeRole to hold it.
const Migration = `CREATE TABLE IF NOT EXISTS idempotency_keys (
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
	-- Only a role with CREATEROLE can run CREATE ROLE, so the text creates the role only when it is missing. A run at
	-- the same time that creates it first is not an error.
	IF NOT EXISTS (SELECT FROM pg_catalog.pg_roles WHERE rolname = 'stego_idempotency_purge') THEN
		CREATE ROLE stego_idempotency_purge NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT;
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
CREATE POLICY idempotency_keys_purge_select ON idempotency_keys FOR SELECT TO stego_idempotency_purge USING (true);
DROP POLICY IF EXISTS idempotency_keys_purge_delete ON idempotency_keys;
CREATE POLICY idempotency_keys_purge_delete ON idempotency_keys FOR DELETE TO stego_idempotency_purge USING (true);
REVOKE ALL ON idempotency_keys FROM stego_idempotency_purge;
GRANT SELECT, DELETE ON idempotency_keys TO stego_idempotency_purge;

DO $do$
DECLARE
	v_schema text := pg_catalog.current_schema();
	v_create boolean;
BEGIN
	IF v_schema IS NULL THEN
		RAISE EXCEPTION 'idempotency: the migration needs a current schema';
	END IF;
	-- The purge function reads the table through its schema.
	IF NOT pg_catalog.has_schema_privilege('stego_idempotency_purge', v_schema, 'USAGE') THEN
		EXECUTE pg_catalog.format('GRANT USAGE ON SCHEMA %I TO stego_idempotency_purge', v_schema);
	END IF;
	-- The owner of a function needs CREATE on its schema. When the role has none, the text gives it for the change
	-- of owner only, and takes it back in the same transaction.
	v_create := pg_catalog.has_schema_privilege('stego_idempotency_purge', v_schema, 'CREATE');
	IF NOT v_create THEN
		EXECUTE pg_catalog.format('GRANT CREATE ON SCHEMA %I TO stego_idempotency_purge', v_schema);
	END IF;
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
	EXECUTE pg_catalog.format('ALTER FUNCTION %I.idempotency_keys_purge(timestamptz, integer) OWNER TO stego_idempotency_purge', v_schema);
	EXECUTE pg_catalog.format('REVOKE ALL ON FUNCTION %I.idempotency_keys_purge(timestamptz, integer) FROM PUBLIC', v_schema);
	IF NOT v_create THEN
		EXECUTE pg_catalog.format('REVOKE CREATE ON SCHEMA %I FROM stego_idempotency_purge', v_schema);
	END IF;
END
$do$;
`
