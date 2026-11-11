DO $$
BEGIN
	IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'tenancy_check_owner') THEN
		CREATE ROLE "tenancy_check_owner" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
	END IF;
	IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'tenancy_check_runtime') THEN
		CREATE ROLE "tenancy_check_runtime" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
	END IF;
	IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'tenancy_check_worker') THEN
		CREATE ROLE "tenancy_check_worker" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
	END IF;
	IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'tenancy_check_relay') THEN
		CREATE ROLE "tenancy_check_relay" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
	END IF;
	IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'stego_outbox_relay') THEN
		CREATE ROLE "stego_outbox_relay" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
	END IF;
	IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'tenancy_check_idempotency_purge') THEN
		CREATE ROLE "tenancy_check_idempotency_purge" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT;
	END IF;
	EXECUTE pg_catalog.format('REVOKE CONNECT, CREATE, TEMPORARY ON DATABASE %I FROM PUBLIC', pg_catalog.current_database());
	EXECUTE pg_catalog.format('GRANT CONNECT, CREATE ON DATABASE %I TO "tenancy_check_owner"', pg_catalog.current_database());
	EXECUTE pg_catalog.format('GRANT CONNECT ON DATABASE %I TO "tenancy_check_runtime"', pg_catalog.current_database());
	EXECUTE pg_catalog.format('GRANT CONNECT ON DATABASE %I TO "tenancy_check_worker"', pg_catalog.current_database());
	EXECUTE pg_catalog.format('GRANT CONNECT ON DATABASE %I TO "tenancy_check_relay"', pg_catalog.current_database());
END
$$;
COMMIT;
DO $$
DECLARE
	v_open bigint;
BEGIN
	SELECT pg_catalog.count(*) INTO v_open FROM pg_catalog.pg_stat_activity AS a
		LEFT JOIN pg_catalog.pg_roles AS r ON r.oid = a.usesysid
		WHERE a.datname = pg_catalog.current_database() AND a.pid <> pg_catalog.pg_backend_pid()
			AND a.backend_type = 'client backend' AND NOT COALESCE(r.rolsuper, false)
			AND COALESCE(r.rolname, '') NOT IN ('tenancy_check_owner', 'tenancy_check_runtime', 'tenancy_check_worker', 'tenancy_check_relay');
	IF v_open > 0 THEN
		RAISE EXCEPTION 'tenancy: % sessions of roles that are not roles of this service are open in this database; close them, then run the role migration again', v_open;
	END IF;
END
$$;
ALTER ROLE "tenancy_check_owner" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
ALTER ROLE "tenancy_check_runtime" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT;
ALTER ROLE "tenancy_check_worker" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT;
ALTER ROLE "tenancy_check_relay" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT;
ALTER ROLE "tenancy_check_idempotency_purge" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO "tenancy_check_owner";
GRANT CREATE ON SCHEMA public TO "tenancy_check_owner" WITH GRANT OPTION;
GRANT USAGE ON SCHEMA public TO "tenancy_check_runtime";
GRANT USAGE ON SCHEMA public TO "tenancy_check_worker";
GRANT USAGE ON SCHEMA public TO "tenancy_check_relay";
GRANT "stego_outbox_relay" TO "tenancy_check_owner" WITH SET TRUE, INHERIT TRUE;
GRANT CREATE ON SCHEMA public TO "stego_outbox_relay";
GRANT "tenancy_check_idempotency_purge" TO "tenancy_check_owner" WITH SET TRUE, INHERIT FALSE;
