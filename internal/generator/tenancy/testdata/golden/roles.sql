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
	EXECUTE pg_catalog.format('REVOKE CREATE, TEMPORARY ON DATABASE %I FROM PUBLIC', pg_catalog.current_database());
	EXECUTE pg_catalog.format('GRANT CONNECT, CREATE ON DATABASE %I TO "tenancy_check_owner"', pg_catalog.current_database());
	EXECUTE pg_catalog.format('GRANT CONNECT ON DATABASE %I TO "tenancy_check_runtime"', pg_catalog.current_database());
	EXECUTE pg_catalog.format('GRANT CONNECT ON DATABASE %I TO "tenancy_check_worker"', pg_catalog.current_database());
	EXECUTE pg_catalog.format('GRANT CONNECT ON DATABASE %I TO "tenancy_check_relay"', pg_catalog.current_database());
END
$$;
ALTER ROLE "tenancy_check_owner" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
ALTER ROLE "tenancy_check_runtime" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT;
ALTER ROLE "tenancy_check_worker" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT;
ALTER ROLE "tenancy_check_relay" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE, CREATE ON SCHEMA public TO "tenancy_check_owner";
GRANT USAGE ON SCHEMA public TO "tenancy_check_runtime";
GRANT USAGE ON SCHEMA public TO "tenancy_check_worker";
GRANT USAGE ON SCHEMA public TO "tenancy_check_relay";
GRANT "stego_outbox_relay" TO "tenancy_check_owner" WITH SET TRUE, INHERIT TRUE;
GRANT CREATE ON SCHEMA public TO "stego_outbox_relay";
