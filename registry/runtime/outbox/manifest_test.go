package outbox

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

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
			{"a dropped check", "ALTER TABLE stego_outbox.messages DROP CONSTRAINT messages_payload_check",
				"ALTER TABLE stego_outbox.messages ADD CONSTRAINT messages_payload_check CHECK (octet_length(payload) >= 1 AND octet_length(payload) <= 1048576)",
				"table stego_outbox.messages: constraint messages_payload_check is missing"},
		} {
			if _, err := admin.Exec(c.change); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			err := manifest.Check(ctx, admin, m)
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
}
