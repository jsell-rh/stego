package outbox

import (
	"context"
	"strings"
	"testing"

	"stego.dev/runtime/tenant"
)

func TestPayloadLimits(t *testing.T) {
	t.Run("ST-SUP-RP01-LC28-T01 A destination carries the payload limit it sets, up to 1 MiB", func(t *testing.T) {
		db, _ := testDatabase(t)
		ctx := context.Background()
		q, err := New(db, Options{PayloadLimits: map[string]int{"audit-sink": MaxDestinationPayloadBytes}})
		if err != nil {
			t.Fatal(err)
		}
		big := func(destination string, size int) Message {
			m := message(destination, "k:"+destination)
			m.Payload = []byte(`{"note":"` + strings.Repeat("x", size) + `"}`)
			return m
		}
		try := func(m Message) error {
			tn := testTenant(t, "tenant-a")
			tx, err := tenant.Begin(ctx, db, tn, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			if err := q.Enqueue(ctx, tx, m); err != nil {
				return err
			}
			return tenant.Commit(ctx, tx, tn)
		}
		if err := try(big("audit-sink", 900<<10)); err != nil {
			t.Fatalf("a message of 900 KiB to audit-sink gave %v", err)
		}
		if err := try(big("audit-sink", 1<<20)); err == nil {
			t.Fatal("a message above 1 MiB to audit-sink was taken")
		}
		if err := try(big("billing", 65<<10)); err == nil {
			t.Fatal("a message above 64 KiB to a destination with no limit of its own was taken")
		}
		if q.PayloadLimit("billing") != MaxPayloadBytes || q.PayloadLimit("audit-sink") != 1<<20 {
			t.Fatal("the payload limits are not the ones set")
		}
		for name, limits := range map[string]map[string]int{"2 MiB": {"audit-sink": 2 << 20}, "zero": {"audit-sink": 0},
			"no destination name": {"": 1024}} {
			if _, err := New(db, Options{PayloadLimits: limits}); err == nil {
				t.Errorf("a payload limit of %s was taken", name)
			}
		}
		// A table of an earlier migration, whose check takes 128 KiB, gets the new check when the migration runs again.
		a := admin(t, db)
		var runtimeRole, workerRole string
		if err := a.QueryRowContext(ctx, `SELECT roles[1]::text, roles[2]::text FROM (SELECT array_agg(r ORDER BY r) AS roles
FROM pg_policies, unnest(roles) AS r WHERE tablename = 'messages' AND policyname = 'messages_tenant') p`).Scan(&runtimeRole, &workerRole); err != nil {
			t.Fatal(err)
		}
		var current string
		if err := db.QueryRowContext(ctx, "SELECT current_user").Scan(&current); err != nil {
			t.Fatal(err)
		}
		if current == workerRole {
			runtimeRole, workerRole = workerRole, runtimeRole
		}
		// The payload is stored as the exact bytes that Enqueue took, with their spacing and key order.
		exact := message("billing", "k:exact")
		exact.Payload = []byte(`{ "z": 1,  "a": [true, null] }`)
		if err := try(exact); err != nil {
			t.Fatal(err)
		}
		var stored []byte
		if err := admin(t, db).QueryRowContext(ctx, `SELECT payload FROM stego_outbox.messages WHERE id = $1`, exact.ID).Scan(&stored); err != nil ||
			string(stored) != string(exact.Payload) {
			t.Fatalf("the stored payload is %q, want the exact bytes %q: %v", stored, exact.Payload, err)
		}
		ddl, err := Migration(runtimeRole, workerRole)
		if err != nil {
			t.Fatal(err)
		}
		checkOID := func() (oid int64, def string) {
			t.Helper()
			if err := a.QueryRowContext(ctx, `SELECT oid::bigint, pg_get_constraintdef(oid) FROM pg_constraint
WHERE conname = 'messages_payload_check' AND conrelid = 'stego_outbox.messages'::regclass`).Scan(&oid, &def); err != nil {
				t.Fatal(err)
			}
			return oid, def
		}
		// A second run of the migration keeps a check whose definition is the same.
		first, def := checkOID()
		if _, err := a.ExecContext(ctx, ddl); err != nil {
			t.Fatalf("the migration again: %v", err)
		}
		if again, _ := checkOID(); again != first || !strings.Contains(def, "1048576") {
			t.Fatalf("the migration made the same check again (%d, %d) or it is %q", first, again, def)
		}
		// A table of an earlier migration: a jsonb payload and a check of 128 KiB.
		if _, err := a.ExecContext(ctx, `DELETE FROM stego_outbox.messages;
ALTER TABLE stego_outbox.messages DROP CONSTRAINT messages_payload_check;
ALTER TABLE stego_outbox.messages ALTER COLUMN payload TYPE jsonb USING convert_from(payload, 'UTF8')::jsonb;
ALTER TABLE stego_outbox.messages ADD CONSTRAINT messages_payload_check
	CHECK (jsonb_typeof(payload) = 'object' AND octet_length(payload::text) <= 131072)`); err != nil {
			t.Fatal(err)
		}
		if _, err := a.ExecContext(ctx, ddl); err != nil {
			t.Fatalf("the migration on an old table: %v", err)
		}
		var kind string
		if err := a.QueryRowContext(ctx, `SELECT format_type(atttypid, atttypmod) FROM pg_attribute
WHERE attrelid = 'stego_outbox.messages'::regclass AND attname = 'payload'`).Scan(&kind); err != nil || kind != "bytea" {
			t.Fatalf("the payload column of an old table is %q after the migration: %v", kind, err)
		}
		var validated bool
		if err := a.QueryRowContext(ctx, `SELECT convalidated FROM pg_constraint
WHERE conname = 'messages_payload_check' AND conrelid = 'stego_outbox.messages'::regclass`).Scan(&validated); err != nil || !validated {
			t.Fatalf("the new check is not validated: %t, %v", validated, err)
		}
		if err := try(big("audit-sink", 900<<10)); err != nil {
			t.Fatalf("after the migration ran again on an old table, a message of 900 KiB gave %v", err)
		}
	})
}
