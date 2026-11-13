DO $$
DECLARE
	v_role text;
	v_login boolean;
	v_fault text;
	v_members text;
	v_migration text := NULLIF(pg_catalog.current_setting('stego.migration_login', true), '');
BEGIN
	FOR v_role, v_login IN VALUES ('user_management_owner', true), ('user_management_runtime', true), ('user_management_worker', true), ('user_management_relay', true), ('user_management_idempotency_purge', false), ('user_management_outbox_relay', false) LOOP
		SELECT CASE
			WHEN EXISTS (SELECT 1 FROM pg_catalog.pg_database AS d WHERE d.datname <> pg_catalog.current_database()
				AND (d.datdba = r.oid OR EXISTS (SELECT 1 FROM pg_catalog.aclexplode(d.datacl) AS a WHERE a.grantee = r.oid))
				AND NOT EXISTS (SELECT 1 FROM pg_catalog.aclexplode(d.datacl) AS a JOIN pg_catalog.pg_roles AS o ON o.oid = a.grantee
					WHERE o.rolname = 'user_management_owner' AND a.privilege_type = 'CREATE'))
				THEN 'holds a privilege on another database, which is not a database of this service'
			WHEN r.rolcanlogin AND NOT v_login THEN 'can log in'
			WHEN EXISTS (SELECT 1 FROM pg_catalog.pg_auth_members AS m JOIN pg_catalog.pg_roles AS g ON g.oid = m.roleid
				WHERE m.member = r.oid AND NOT (r.rolname = 'user_management_owner' AND g.rolname IN ('user_management_idempotency_purge', 'user_management_outbox_relay')))
				THEN 'is a member of a role that this service does not grant'
		END INTO v_fault FROM pg_catalog.pg_roles AS r WHERE r.rolname = v_role;
		IF v_fault IS NOT NULL THEN
			RAISE EXCEPTION 'tenancy: the role % exists and is not a role of this service alone: it %; give the service role names of its own', v_role, v_fault;
		END IF;
		SELECT pg_catalog.string_agg(mr.rolname || CASE WHEN m.admin_option THEN ' with ADMIN' ELSE '' END, ', ' ORDER BY mr.rolname)
			INTO v_members FROM pg_catalog.pg_auth_members AS m JOIN pg_catalog.pg_roles AS g ON g.oid = m.roleid
			JOIN pg_catalog.pg_roles AS mr ON mr.oid = m.member
			WHERE g.rolname = v_role AND NOT (NOT m.admin_option AND (
				(g.rolname IN ('user_management_idempotency_purge', 'user_management_outbox_relay') AND mr.rolname = 'user_management_owner')
				OR (g.rolname = 'user_management_owner' AND mr.rolname IS NOT DISTINCT FROM v_migration)));
		IF v_members IS NOT NULL THEN
			RAISE EXCEPTION 'tenancy: the role % exists and is not a role of this service alone: it has the members %, which this service does not grant; name the migration login in stego.migration_login or revoke them', v_role, v_members;
		END IF;
	END LOOP;
	SELECT pg_catalog.string_agg(mr.rolname || CASE WHEN m.admin_option THEN ' with ADMIN' ELSE '' END, ', ' ORDER BY mr.rolname)
		INTO v_members FROM pg_catalog.pg_auth_members AS m JOIN pg_catalog.pg_roles AS g ON g.oid = m.roleid
		JOIN pg_catalog.pg_roles AS mr ON mr.oid = m.member WHERE g.rolname = v_migration;
	IF v_members IS NOT NULL THEN
		RAISE EXCEPTION 'tenancy: the migration login % that stego.migration_login names has the members %, which reach the owner role through it; revoke them, or name a login with no member', v_migration, v_members;
	END IF;
	IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'user_management_owner') THEN
		CREATE ROLE "user_management_owner" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
	END IF;
	IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'user_management_runtime') THEN
		CREATE ROLE "user_management_runtime" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
	END IF;
	IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'user_management_worker') THEN
		CREATE ROLE "user_management_worker" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
	END IF;
	IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'user_management_relay') THEN
		CREATE ROLE "user_management_relay" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
	END IF;
	IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'user_management_idempotency_purge') THEN
		CREATE ROLE "user_management_idempotency_purge" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT;
	END IF;
	IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'user_management_outbox_relay') THEN
		CREATE ROLE "user_management_outbox_relay" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT;
	END IF;
	EXECUTE pg_catalog.format('REVOKE CONNECT, CREATE, TEMPORARY ON DATABASE %I FROM PUBLIC', pg_catalog.current_database());
	EXECUTE pg_catalog.format('GRANT CONNECT, CREATE ON DATABASE %I TO "user_management_owner"', pg_catalog.current_database());
	EXECUTE pg_catalog.format('GRANT CONNECT ON DATABASE %I TO "user_management_runtime"', pg_catalog.current_database());
	EXECUTE pg_catalog.format('GRANT CONNECT ON DATABASE %I TO "user_management_worker"', pg_catalog.current_database());
	EXECUTE pg_catalog.format('GRANT CONNECT ON DATABASE %I TO "user_management_relay"', pg_catalog.current_database());
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
			AND COALESCE(r.rolname, '') NOT IN ('user_management_owner', 'user_management_runtime', 'user_management_worker', 'user_management_relay');
	IF v_open > 0 THEN
		RAISE EXCEPTION 'tenancy: % sessions of roles that are not roles of this service are open in this database; close them, then run the role migration again', v_open;
	END IF;
END
$$;
ALTER ROLE "user_management_owner" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
ALTER ROLE "user_management_runtime" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT;
ALTER ROLE "user_management_worker" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT;
ALTER ROLE "user_management_relay" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT;
ALTER ROLE "user_management_idempotency_purge" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT;
ALTER ROLE "user_management_outbox_relay" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO "user_management_owner";
GRANT CREATE ON SCHEMA public TO "user_management_owner" WITH GRANT OPTION;
GRANT USAGE ON SCHEMA public TO "user_management_runtime";
GRANT USAGE ON SCHEMA public TO "user_management_worker";
GRANT USAGE ON SCHEMA public TO "user_management_relay";
GRANT "user_management_idempotency_purge" TO "user_management_owner" WITH SET TRUE, INHERIT FALSE;
GRANT "user_management_outbox_relay" TO "user_management_owner" WITH SET TRUE, INHERIT FALSE;
