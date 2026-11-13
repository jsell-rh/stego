package outbox

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jsell-rh/stego/registry/runtime/manifest"
)

func TestManifest(t *testing.T) {
	t.Run("ST-SUP-RP01-LC33-T01 The manifest of the outbox holds what its migration makes and refuses a change of its policies, grants and functions", func(t *testing.T) {
		db, _ := testDatabase(t)
		v, _ := admins.Load(db)
		admin := v.(*sql.DB)
		r, _ := relays.Load(db)
		relay := r.(string)
		var runtimeRole, workerRole string
		rq, _ := requests.Load(db)
		if err := rq.(*sql.DB).QueryRow("SELECT current_user").Scan(&runtimeRole); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow("SELECT current_user").Scan(&workerRole); err != nil {
			t.Fatal(err)
		}
		m, err := Manifest(relay, runtimeRole, workerRole)
		if err != nil {
			t.Fatal(err)
		}
		var owner string
		if err := admin.QueryRow("SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid = 'stego_outbox.messages'::regclass").Scan(&owner); err != nil {
			t.Fatal(err)
		}
		m.Owner = owner
		ctx := context.Background()
		if err := manifest.Check(ctx, admin, m); err != nil {
			t.Fatalf("Check refused the database that the migration made: %v", err)
		}
		queue, err := New(db, Options{})
		if err != nil {
			t.Fatal(err)
		}
		other := "outbox_other_" + randomSuffix(t)
		if _, err := admin.Exec("CREATE ROLE " + other + " NOLOGIN"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = admin.Exec("DROP OWNED BY " + other)
			_, _ = admin.Exec("DROP ROLE IF EXISTS " + other)
		})
		for _, c := range []struct{ name, change, restore, fault string }{
			{"an extra role on the tenant policy", "ALTER POLICY messages_tenant ON stego_outbox.messages TO " + runtimeRole + ", " + workerRole + ", " + other,
				"ALTER POLICY messages_tenant ON stego_outbox.messages TO " + runtimeRole + ", " + workerRole,
				"policy messages_tenant applies to other roles than the manifest names"},
			{"the tenant policy for every role", "ALTER POLICY messages_tenant ON stego_outbox.messages TO PUBLIC",
				"ALTER POLICY messages_tenant ON stego_outbox.messages TO " + runtimeRole + ", " + workerRole,
				"policy messages_tenant applies to other roles than the manifest names"},
			{"a relay policy with another expression", "ALTER POLICY messages_relay_select ON stego_outbox.messages USING (tenant_id <> '')",
				"ALTER POLICY messages_relay_select ON stego_outbox.messages USING (true)", "policy messages_relay_select is not the policy of the manifest"},
			{"an extra permissive policy", "CREATE POLICY open_door ON stego_outbox.messages TO " + other + " USING (true)",
				"DROP POLICY open_door ON stego_outbox.messages", "policy open_door is a permissive policy that the manifest does not name"},
			{"SELECT for PUBLIC", "GRANT SELECT ON stego_outbox.messages TO PUBLIC", "REVOKE SELECT ON stego_outbox.messages FROM PUBLIC",
				"the privilege SELECT to PUBLIC is granted, and the manifest does not grant it"},
			{"UPDATE for the runtime role", "GRANT UPDATE ON stego_outbox.messages TO " + runtimeRole, "REVOKE UPDATE ON stego_outbox.messages FROM " + runtimeRole,
				"the privilege UPDATE to " + runtimeRole + " is granted, and the manifest does not grant it"},
			{"a column grant to another role", "GRANT INSERT (state) ON stego_outbox.messages TO " + other, "REVOKE INSERT (state) ON stego_outbox.messages FROM " + other,
				"the privilege INSERT (state) to " + other + " is granted, and the manifest does not grant it"},
			{"a missing column grant", "REVOKE INSERT (payload) ON stego_outbox.messages FROM " + workerRole, "GRANT INSERT (payload) ON stego_outbox.messages TO " + workerRole,
				"the privilege INSERT (payload) to " + workerRole + " of the manifest is not granted"},
			{"a grant with the grant option", "GRANT SELECT ON stego_outbox.messages TO " + workerRole + " WITH GRANT OPTION",
				"REVOKE GRANT OPTION FOR SELECT ON stego_outbox.messages FROM " + workerRole, "the privilege SELECT to " + workerRole + " is granted with the grant option"},
			{"a relay function of another owner", "ALTER FUNCTION stego_outbox.oldest(timestamptz) OWNER TO " + other,
				"ALTER FUNCTION stego_outbox.oldest(timestamptz) OWNER TO " + relay, "function stego_outbox.oldest has another owner"},
			{"EXECUTE on claim for the runtime role", "GRANT EXECUTE ON FUNCTION stego_outbox.claim(timestamptz, integer, uuid, bigint) TO " + runtimeRole,
				"REVOKE EXECUTE ON FUNCTION stego_outbox.claim(timestamptz, integer, uuid, bigint) FROM " + runtimeRole,
				"function stego_outbox.claim may be run by roles that the manifest does not name"},
			{"a relay function that is SECURITY INVOKER", "ALTER FUNCTION stego_outbox.purge_done(timestamptz, integer, bigint) SECURITY INVOKER",
				"ALTER FUNCTION stego_outbox.purge_done(timestamptz, integer, bigint) SECURITY DEFINER", "function stego_outbox.purge_done is not the function of the manifest"},
			{"an overload of claim in the schema", "CREATE FUNCTION stego_outbox.claim(timestamptz, double precision, uuid, bigint) RETURNS SETOF stego_outbox.messages LANGUAGE plpgsql " +
				"AS $x$ BEGIN RAISE EXCEPTION 'the overload ran'; END $x$",
				"DROP FUNCTION stego_outbox.claim(timestamptz, double precision, uuid, bigint)",
				"schema stego_outbox: function stego_outbox.claim(timestamp with time zone,double precision,uuid,bigint) is a function that the manifest does not name"},
			{"a function of the relay role outside the manifest", "CREATE FUNCTION public.peek() RETURNS bigint LANGUAGE sql SECURITY DEFINER " +
				"SET search_path = pg_catalog, pg_temp AS 'SELECT count(*) FROM stego_outbox.messages'; ALTER FUNCTION public.peek() OWNER TO " + relay,
				"DROP FUNCTION public.peek()", "the confined role " + relay + " owns the function public.peek(), which the manifest does not name for it"},
			{"claim made STRICT", "ALTER FUNCTION stego_outbox.claim(timestamptz, integer, uuid, bigint) STRICT",
				"ALTER FUNCTION stego_outbox.claim(timestamptz, integer, uuid, bigint) CALLED ON NULL INPUT",
				"function stego_outbox.claim has another strictness, volatility or result than the manifest"},
			{"USAGE on the schema for PUBLIC", "GRANT USAGE ON SCHEMA stego_outbox TO PUBLIC", "REVOKE USAGE ON SCHEMA stego_outbox FROM PUBLIC",
				"schema stego_outbox: the privilege USAGE to PUBLIC is granted, and the manifest does not grant it"},
			{"CREATE on the schema for the relay role", "GRANT CREATE ON SCHEMA stego_outbox TO " + relay, "REVOKE CREATE ON SCHEMA stego_outbox FROM " + relay,
				"schema stego_outbox: the privilege CREATE to " + relay + " is granted, and the manifest does not grant it"},
			{"another default of the state", "ALTER TABLE stego_outbox.messages ALTER COLUMN state SET DEFAULT 'done'",
				"ALTER TABLE stego_outbox.messages ALTER COLUMN state SET DEFAULT 'pending'", "column state has another default than the manifest states"},
			{"a default on the tenant", "ALTER TABLE stego_outbox.messages ALTER COLUMN tenant_id SET DEFAULT 'victim'",
				"ALTER TABLE stego_outbox.messages ALTER COLUMN tenant_id DROP DEFAULT", "column tenant_id has a default that the manifest does not state"},
			{"a dropped check", "ALTER TABLE stego_outbox.messages DROP CONSTRAINT messages_payload_check",
				"ALTER TABLE stego_outbox.messages ADD CONSTRAINT messages_payload_check CHECK (octet_length(payload) >= 1 AND octet_length(payload) <= 1048576)",
				"table stego_outbox.messages: constraint messages_payload_check is missing"},
		} {
			if _, err := admin.Exec(c.change); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			err := manifest.Check(ctx, admin, m)
			if c.name == "an overload of claim in the schema" {
				// The queue names the type of each parameter, so its claim runs the function of the manifest.
				if _, err := queue.Claim(ctx, 8, time.Minute); err != nil {
					t.Errorf("%s: Claim gave %v, want the claim of the manifest", c.name, err)
				}
			}
			var mismatch *manifest.Mismatch
			if !errors.As(err, &mismatch) || len(mismatch.Faults) != 1 || !strings.HasSuffix(mismatch.Faults[0], c.fault) {
				t.Errorf("%s: Check gave %v, want the one fault %q", c.name, err, c.fault)
			}
			if _, err := admin.Exec(c.restore); err != nil {
				t.Fatalf("%s: restore: %v", c.name, err)
			}
		}
		if err := manifest.Check(ctx, admin, m); err != nil {
			t.Fatalf("Check refused the restored database: %v", err)
		}
	})

	t.Run("ST-SUP-RP01-LC33-T02 A claim runs the relay function of the manifest when an overload of it is in the schema", func(t *testing.T) {
		db, _ := testDatabase(t)
		v, _ := admins.Load(db)
		admin := v.(*sql.DB)
		r, _ := relays.Load(db)
		// An overload whose second parameter is double precision takes an untyped parameter before the function
		// of the manifest; the claim names each parameter's type, so it runs claim(timestamptz, integer, uuid, bigint).
		if _, err := admin.Exec(`CREATE FUNCTION stego_outbox.claim(a timestamptz, b double precision, c uuid, d bigint)
RETURNS SETOF stego_outbox.messages LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $x$ BEGIN RAISE EXCEPTION 'the overload ran'; END $x$;
ALTER FUNCTION stego_outbox.claim(timestamptz, double precision, uuid, bigint) OWNER TO ` + r.(string)); err != nil {
			t.Fatal(err)
		}
		q, err := New(db, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := q.Claim(context.Background(), 8, time.Minute); err != nil {
			t.Fatalf("Claim gave %v, want the claim of the manifest", err)
		}
	})
}
