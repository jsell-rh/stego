package authz

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/registry/runtime/manifest"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
)

func TestManifest(t *testing.T) {
	t.Run("ST-SUP-RP06-LC23-T01 The manifest of the revocation tables holds what their migration makes and refuses a change of their policies, grants, trigger and function", func(t *testing.T) {
		ctx := context.Background()
		e := newPG(t)
		m, err := Manifest(e.schema, e.roles[0], e.roles[2], e.outboxRelay, e.roles[1])
		if err != nil {
			t.Fatal(err)
		}
		if err := e.admin.QueryRowContext(ctx, "SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid = 'authz.authz_floor'::regclass").Scan(&m.Owner); err != nil {
			t.Fatal(err)
		}
		// The outbox relay role owns the relay functions of the outbox too, so the check joins the outbox manifest.
		queue, err := outbox.Manifest(e.outboxRelay, e.roles[0], e.roles[1])
		if err != nil {
			t.Fatal(err)
		}
		m = manifest.Join(m, queue)
		if err := manifest.Check(ctx, e.admin, m); err != nil {
			t.Fatalf("Check refused the database that the migration made: %v", err)
		}
		runtimeRole, workerRole, relayRole := e.roles[0], e.roles[1], e.roles[2]
		state := "authz.authz_revocation_state(uuid, uuid, text)"
		for _, c := range []struct{ name, change, restore, fault string }{
			{"UPDATE of the floor for the runtime role", "GRANT UPDATE ON authz.authz_floor TO " + runtimeRole, "REVOKE UPDATE ON authz.authz_floor FROM " + runtimeRole,
				"the privilege UPDATE to " + runtimeRole + " is granted, and the manifest does not grant it"},
			{"a column UPDATE of the counts for the runtime role", "GRANT UPDATE (done) ON authz.authz_revocations TO " + runtimeRole,
				"REVOKE UPDATE (done) ON authz.authz_revocations FROM " + runtimeRole, "the privilege UPDATE (done) to " + runtimeRole + " is granted, and the manifest does not grant it"},
			{"SELECT of the tuples for PUBLIC", "GRANT SELECT ON authz.authz_revocation_tuples TO PUBLIC", "REVOKE SELECT ON authz.authz_revocation_tuples FROM PUBLIC",
				"the privilege SELECT to PUBLIC is granted, and the manifest does not grant it"},
			{"the tenant policy for one role", "ALTER POLICY authz_revocations_tenant ON authz.authz_revocations TO " + runtimeRole,
				"ALTER POLICY authz_revocations_tenant ON authz.authz_revocations TO PUBLIC", "policy authz_revocations_tenant is not the policy of the manifest"},
			{"an extra permissive policy", "CREATE POLICY open_door ON authz.authz_revocation_tuples TO " + workerRole + " USING (true)",
				"DROP POLICY open_door ON authz.authz_revocation_tuples", "policy open_door is a permissive policy that the manifest does not name"},
			{"EXECUTE on the state hook for the relay login role", "GRANT EXECUTE ON FUNCTION " + state + " TO " + relayRole,
				"REVOKE EXECUTE ON FUNCTION " + state + " FROM " + relayRole, "function authz.authz_revocation_state may be run by roles that the manifest does not name"},
			{"the state hook of another owner", "ALTER FUNCTION " + state + " OWNER TO " + relayRole, "ALTER FUNCTION " + state + " OWNER TO " + e.outboxRelay,
				"function authz.authz_revocation_state has another owner"},
			{"the state hook STRICT", "ALTER FUNCTION " + state + " STRICT", "ALTER FUNCTION " + state + " CALLED ON NULL INPUT",
				"function authz.authz_revocation_state has another strictness, volatility or result than the manifest"},
			{"the tuple check of another owner", "ALTER FUNCTION authz.authz_revocation_tuple_check() OWNER TO " + relayRole,
				"ALTER FUNCTION authz.authz_revocation_tuple_check() OWNER TO " + e.outboxRelay,
				"trigger authz_revocation_tuple_check runs the function authz.authz_revocation_tuple_check, which the role " + e.outboxRelay + " does not own"},
			{"the tuple check disabled", "ALTER TABLE authz.authz_revocation_tuples DISABLE TRIGGER authz_revocation_tuple_check",
				"ALTER TABLE authz.authz_revocation_tuples ENABLE TRIGGER authz_revocation_tuple_check", "trigger authz_revocation_tuple_check is disabled"},
			{"a function of the outbox relay role outside the manifest", "CREATE FUNCTION authz.peek() RETURNS bigint LANGUAGE sql SECURITY DEFINER " +
				"SET search_path = pg_catalog, pg_temp AS 'SELECT count(*) FROM authz.authz_revocations'; ALTER FUNCTION authz.peek() OWNER TO " + e.outboxRelay,
				"DROP FUNCTION authz.peek()", "the confined role " + e.outboxRelay + " owns the function authz.peek(), which the manifest does not name for it"},
			{"a dropped foreign key", "ALTER TABLE authz.authz_revocation_tuples DROP CONSTRAINT authz_revocation_tuples_revocation_id_fkey",
				"ALTER TABLE authz.authz_revocation_tuples ADD CONSTRAINT authz_revocation_tuples_revocation_id_fkey FOREIGN KEY (revocation_id) REFERENCES authz.authz_revocations (id)",
				"constraint authz_revocation_tuples_revocation_id_fkey is missing"},
		} {
			if _, err := e.admin.ExecContext(ctx, c.change); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			err := manifest.Check(ctx, e.admin, m)
			var mismatch *manifest.Mismatch
			if !errors.As(err, &mismatch) || len(mismatch.Faults) != 1 || !strings.HasSuffix(mismatch.Faults[0], c.fault) {
				t.Errorf("%s: Check gave %v, want the one fault %q", c.name, err, c.fault)
			}
			if _, err := e.admin.ExecContext(ctx, c.restore); err != nil {
				t.Fatalf("%s: restore: %v", c.name, err)
			}
		}
		if err := manifest.Check(ctx, e.admin, m); err != nil {
			t.Fatalf("Check refused the restored database: %v", err)
		}
		if _, err := Manifest(`Authz"; DROP`, e.roles[0], e.roles[2], e.outboxRelay, e.roles[1]); err == nil {
			t.Error("Manifest took a schema that is not a plain SQL name")
		}
	})
}
