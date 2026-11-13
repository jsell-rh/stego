package idempotency

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/jsell-rh/stego/registry/runtime/internal/pgtest"
	"github.com/jsell-rh/stego/registry/runtime/manifest"
)

func TestManifest(t *testing.T) {
	t.Run("ST-SUP-RP07-LC12-T01 The manifest of the key table holds what its migration makes and refuses a change of its policies, grants and function", func(t *testing.T) {
		ctx := context.Background()
		db, admin := testDB(t)
		var schema, owner string
		if err := db.QueryRowContext(ctx, "SELECT current_schema(), pg_get_userbyid(relowner) FROM pg_class WHERE oid = 'idempotency_keys'::regclass").Scan(&schema, &owner); err != nil {
			t.Fatal(err)
		}
		const runtimeRole = "idem_test_runtime"
		// The grants of the runtime role give it the key table, after the migration (ST-SUP-RP14-LC03).
		if _, err := db.ExecContext(ctx, "GRANT SELECT, INSERT, UPDATE ON idempotency_keys TO "+runtimeRole); err != nil {
			t.Fatal(err)
		}
		m, err := Manifest(schema, testPurgeRole, runtimeRole)
		if err != nil {
			t.Fatal(err)
		}
		m.Owner = owner
		if err := manifest.Check(ctx, admin, m); err != nil {
			t.Fatalf("Check refused the database that the migration made: %v", err)
		}
		other := schema + "_other"
		if _, err := admin.ExecContext(ctx, "CREATE ROLE "+other+" NOLOGIN"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = admin.ExecContext(context.Background(), "DROP OWNED BY "+other)
			_, _ = admin.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+other)
		})
		table := schema + ".idempotency_keys"
		purge := schema + ".idempotency_keys_purge(timestamptz, integer)"
		for _, c := range []struct{ name, change, restore, fault string }{
			{"the tenant policy for one role", "ALTER POLICY idempotency_keys_tenant ON " + table + " TO " + runtimeRole,
				"ALTER POLICY idempotency_keys_tenant ON " + table + " TO PUBLIC", "policy idempotency_keys_tenant is not the policy of the manifest"},
			{"an extra role on a purge policy", "ALTER POLICY idempotency_keys_purge_delete ON " + table + " TO " + testPurgeRole + ", " + runtimeRole,
				"ALTER POLICY idempotency_keys_purge_delete ON " + table + " TO " + testPurgeRole, "policy idempotency_keys_purge_delete applies to other roles than the manifest names"},
			{"an extra permissive policy", "CREATE POLICY open_door ON " + table + " TO " + other + " USING (true)",
				"DROP POLICY open_door ON " + table, "policy open_door is a permissive policy that the manifest does not name"},
			{"DELETE for the runtime role", "GRANT DELETE ON " + table + " TO " + runtimeRole, "REVOKE DELETE ON " + table + " FROM " + runtimeRole,
				"the privilege DELETE to " + runtimeRole + " is granted, and the manifest does not grant it"},
			{"UPDATE for the purge role", "GRANT UPDATE (status) ON " + table + " TO " + testPurgeRole, "REVOKE UPDATE (status) ON " + table + " FROM " + testPurgeRole,
				"the privilege UPDATE (status) to " + testPurgeRole + " is granted, and the manifest does not grant it"},
			{"SELECT for PUBLIC", "GRANT SELECT ON " + table + " TO PUBLIC", "REVOKE SELECT ON " + table + " FROM PUBLIC",
				"the privilege SELECT to PUBLIC is granted, and the manifest does not grant it"},
			{"EXECUTE on the purge function for another role", "GRANT EXECUTE ON FUNCTION " + purge + " TO " + other,
				"REVOKE EXECUTE ON FUNCTION " + purge + " FROM " + other, "function " + schema + ".idempotency_keys_purge may be run by roles that the manifest does not name"},
			{"the purge function of another owner", "ALTER FUNCTION " + purge + " OWNER TO " + other,
				"ALTER FUNCTION " + purge + " OWNER TO " + testPurgeRole, "function " + schema + ".idempotency_keys_purge has another owner"},
			{"the purge function STRICT", "ALTER FUNCTION " + purge + " STRICT", "ALTER FUNCTION " + purge + " CALLED ON NULL INPUT",
				"function " + schema + ".idempotency_keys_purge has another strictness, volatility or result than the manifest"},
			{"a function of the purge role outside the manifest", "CREATE FUNCTION " + schema + ".peek() RETURNS bigint LANGUAGE sql SECURITY DEFINER " +
				"SET search_path = pg_catalog, pg_temp AS 'SELECT count(*) FROM " + table + "'; ALTER FUNCTION " + schema + ".peek() OWNER TO " + testPurgeRole,
				"DROP FUNCTION " + schema + ".peek()", "the confined role " + testPurgeRole + " owns the function " + schema + ".peek(), which the manifest does not name for it"},
			{"an overload of the purge function", "CREATE FUNCTION " + schema + ".idempotency_keys_purge(timestamptz, double precision) RETURNS bigint LANGUAGE sql AS 'SELECT 0::bigint'",
				"DROP FUNCTION " + schema + ".idempotency_keys_purge(timestamptz, double precision)",
				"function " + schema + ".idempotency_keys_purge(timestamp with time zone,double precision) has the name of a function of the manifest, and is not one of it"},
			{"CREATE on the schema for the runtime role", "GRANT CREATE ON SCHEMA " + schema + " TO " + runtimeRole, "REVOKE CREATE ON SCHEMA " + schema + " FROM " + runtimeRole,
				"schema " + schema + ": the privilege CREATE to " + runtimeRole + " is granted, and the manifest gives it to no role but the owner"},
			{"a dropped check", "ALTER TABLE " + table + " DROP CONSTRAINT idempotency_keys_check",
				"ALTER TABLE " + table + " ADD CONSTRAINT idempotency_keys_check CHECK (status <> 'completed' OR response_code IS NOT NULL)",
				"constraint idempotency_keys_check is missing"},
		} {
			if _, err := admin.ExecContext(ctx, c.change); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			err := manifest.Check(ctx, admin, m)
			var mismatch *manifest.Mismatch
			if !errors.As(err, &mismatch) || len(mismatch.Faults) != 1 || !strings.HasSuffix(mismatch.Faults[0], c.fault) {
				t.Errorf("%s: Check gave %v, want the one fault %q", c.name, err, c.fault)
			}
			if _, err := admin.ExecContext(ctx, c.restore); err != nil {
				t.Fatalf("%s: restore: %v", c.name, err)
			}
		}
		if err := manifest.Check(ctx, admin, m); err != nil {
			t.Fatalf("Check refused the restored database: %v", err)
		}
		// A CREATE on the schema that the purge role held before the migration is gone after it (#244 I3).
		if _, err := admin.ExecContext(ctx, "GRANT CREATE ON SCHEMA "+schema+" TO "+testPurgeRole); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, testMigration(t)); err != nil {
			t.Fatalf("apply Migration again: %v", err)
		}
		if err := manifest.Check(ctx, admin, m); err != nil {
			t.Fatalf("Check refused the database after the migration ran with a CREATE of the purge role: %v", err)
		}
		if _, err := Manifest("Public; DROP", testPurgeRole, runtimeRole); err == nil {
			t.Error("Manifest took a schema that is not a plain SQL name")
		}
		if _, err := Manifest(schema, runtimeRole, runtimeRole); err == nil {
			t.Error("Manifest took one name for the purge and the runtime role")
		}
	})

	t.Run("ST-SUP-RP07-LC12-T02 Wrap and Purge use the key table and the purge function of their schema when an earlier schema of the search path has ones of the same name", func(t *testing.T) {
		ctx := context.Background()
		db, admin := testDB(t)
		schema := testSchema(t, db)
		earlier := schema + "_earlier"
		// A schema earlier on the search path holds a purge function of the same signature and a key table of the
		// same name, with a completed row for the key of the request.
		for _, statement := range []string{
			"CREATE SCHEMA " + earlier,
			"CREATE FUNCTION " + earlier + ".idempotency_keys_purge(p_now timestamptz, p_limit integer) RETURNS bigint " +
				"LANGUAGE plpgsql AS $x$ BEGIN RAISE EXCEPTION 'the earlier purge ran'; END $x$",
			"CREATE TABLE " + earlier + ".idempotency_keys (LIKE " + schema + ".idempotency_keys)",
			"INSERT INTO " + earlier + ".idempotency_keys (tenant_id, principal_id, key, fingerprint, status, response_code, response_body, locked_at, expires_at) " +
				"VALUES ('t1', 'p1', 'k1', 'f1', 'completed', 200, 'planted', now(), now() + interval '1 hour')",
		} {
			if _, err := admin.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { _, _ = admin.ExecContext(context.Background(), "DROP SCHEMA "+earlier+" CASCADE") })
		cfg, err := pgx.ParseConfig(pgtest.DSN(t))
		if err != nil {
			t.Fatal(err)
		}
		cfg.RuntimeParams["search_path"] = earlier + ", " + schema
		shadowed := stdlib.OpenDB(*cfg)
		t.Cleanup(func() { _ = shadowed.Close() })
		if _, err := Purge(ctx, shadowed, PurgeOptions{Schema: schema}); err != nil {
			t.Fatalf("Purge gave %v, want the purge function of the schema of the key table", err)
		}
		ran := false
		res, err := runWrap(ctx, shadowed, Request{Tenant: "t1", Principal: "p1", Key: "k1", Fingerprint: "f1", Schema: schema},
			func(context.Context, Tx) (Response, error) { ran = true; return Response{Status: 201}, nil })
		if err != nil || !ran || res.Replayed || res.Response.Status != 201 {
			t.Fatalf("Wrap = %+v, %v, ran %v; want the handler run on the key table of the schema, not the planted row", res, err, ran)
		}
		// The key is now completed in the key table of the schema, so the same request replays its own response, not
		// the planted one.
		again, err := runWrap(ctx, shadowed, Request{Tenant: "t1", Principal: "p1", Key: "k1", Fingerprint: "f1", Schema: schema},
			func(context.Context, Tx) (Response, error) { return Response{Status: 500}, nil })
		if err != nil || !again.Replayed || again.Response.Status != 201 || string(again.Response.Body) == "planted" {
			t.Fatalf("Wrap = %+v, %v; want the replay of the stored 201", again, err)
		}
	})
}
