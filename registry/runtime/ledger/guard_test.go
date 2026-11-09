package ledger

import (
	"database/sql"
	"strings"
	"testing"
)

func TestGuard(t *testing.T) {
	t.Run("ST-SUP-RP08-LC09-T01 The guard trigger raises on an update or a delete of a transfer or an entry", func(t *testing.T) {
		f := newFixture(t)
		tr := f.post(f.runtime, "k1", dr("hot", 500), cr("plain", 500))
		transfer := f.row("transfers", "id = $1", tr.ID)
		entries := f.row("entries", "transfer_id = $1", tr.ID)
		for _, statement := range []string{
			"UPDATE transfers SET amount = 1 WHERE id = $1",
			"DELETE FROM transfers WHERE id = $1",
			"UPDATE entries SET amount = 1 WHERE transfer_id = $1",
			"DELETE FROM entries WHERE transfer_id = $1",
		} {
			err := f.try(f.owner, acme, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(f.ctx, statement, tr.ID)
				return err
			})
			if sqlState(err) != "42501" || !strings.Contains(err.Error(), guardMessage) {
				t.Fatalf("%s as the owner: %v; want the trigger's error", statement, err)
			}
		}
		if f.row("transfers", "id = $1", tr.ID) != transfer || f.row("entries", "transfer_id = $1", tr.ID) != entries {
			t.Fatal("the rows changed")
		}
		// TRUNCATE is refused too, by the statement trigger.
		err := f.try(f.owner, acme, func(tx *sql.Tx) error { _, err := tx.ExecContext(f.ctx, "TRUNCATE entries"); return err })
		if !strings.Contains(err.Error(), guardMessage) {
			t.Fatalf("TRUNCATE entries as the owner: %v; want the trigger's error", err)
		}
	})

	t.Run("ST-SUP-RP08-LC09-T02 A transfer status changes only through the post and void function and only from pending", func(t *testing.T) {
		f := newFixture(t)
		f.post(f.runtime, "fund", dr("hot", 1000), cr("limited", 1000))
		p := f.pend("hold", dr("limited", 100), cr("plain", 100))
		err := f.try(f.owner, acme, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(f.ctx, "UPDATE transfers SET status = 'posted' WHERE id = $1", p.ID)
			return err
		})
		if sqlState(err) != "42501" {
			t.Fatalf("a direct update of the status as the owner: %v; want the trigger's error", err)
		}
		// A direct update with the guard setting of another transfer, or that also changes the amount, is refused.
		err = f.try(f.owner, acme, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(f.ctx, "SELECT set_config($1, 'settle:' || $2, true)", GuardSetting, p.ID); err != nil {
				return err
			}
			_, err := tx.ExecContext(f.ctx, "UPDATE transfers SET status = 'posted', amount = 99 WHERE id = $1", p.ID)
			return err
		})
		if sqlState(err) != "42501" {
			t.Fatalf("an update of the amount with the status: %v; want the trigger's error", err)
		}
		// The post function alone changes the status, and the commit then refuses it: no new row names the
		// pending transfer.
		err = f.try(f.owner, acme, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(f.ctx, "SELECT ledger_settle($1::uuid, 'posted')", p.ID)
			return err
		})
		if sqlState(err) != "23514" || !strings.Contains(err.Error(), "no new row") {
			t.Fatalf("the post function with no new row: %v; want the commit refused", err)
		}
		// The post function in the statement that inserts the post row, as the owner.
		owner := func(key string) error {
			return f.try(f.owner, acme, func(tx *sql.Tx) error {
				_, err := f.usd.insert(f.ctx, tx, f.tn(acme), write{key: key, kind: Settle, code: "purchase", amount: 100, status: Posted,
					pendingID: p.ID, entries: []Entry{dr("limited", 100), cr("plain", 100)}, after: afterSettle, settleTo: Posted})
				return err
			})
		}
		if err := owner("owner-post-1"); err != nil {
			t.Fatalf("the post function on the pending transfer: %v; want it accepted from pending", err)
		}
		if n := f.count("transfers", "id = $1 AND status = 'posted'", p.ID); n != 1 {
			t.Fatal("the status is not posted")
		}
		if err := owner("owner-post-2"); sqlState(err) != codeAlreadyPosted {
			t.Fatalf("the post function on the posted transfer: %v; want %s", err, codeAlreadyPosted)
		}
		err = f.try(f.owner, acme, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(f.ctx, "SELECT ledger_settle($1::uuid, 'voided')", p.ID)
			return err
		})
		if sqlState(err) != codeAlreadyPosted {
			t.Fatalf("the void function on the posted transfer: %v; want %s", err, codeAlreadyPosted)
		}
		// The function restores the guard setting.
		q := f.pend("hold-2", dr("limited", 5), cr("plain", 5))
		err = f.try(f.runtime, acme, func(tx *sql.Tx) error {
			if _, err := f.usd.Void(f.ctx, tx, f.tn(acme), q.ID, "void-2"); err != nil {
				return err
			}
			var setting string
			if err := tx.QueryRowContext(f.ctx, "SELECT current_setting($1, true)", GuardSetting).Scan(&setting); err != nil {
				return err
			}
			if setting != "" {
				t.Errorf("the guard setting is %q after the function", setting)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("ST-SUP-RP08-LC09-T03 The runtime role has SELECT and INSERT only on the ledger tables", func(t *testing.T) {
		f := newFixture(t)
		for _, table := range []string{"transfers", "entries", "accounts", "balance_snapshots"} {
			for privilege, want := range map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": false, "DELETE": false, "TRUNCATE": false} {
				var has bool
				if err := f.owner.QueryRowContext(f.ctx, "SELECT has_table_privilege($1, $2, $3)", runtimeRole, table, privilege).Scan(&has); err != nil {
					t.Fatal(err)
				}
				if has != want {
					t.Fatalf("the runtime role has %s on %s: %v; want %v", privilege, table, has, want)
				}
			}
			var owner string
			if err := f.owner.QueryRowContext(f.ctx, "SELECT tableowner FROM pg_tables WHERE tablename = $1", table).Scan(&owner); err != nil {
				t.Fatal(err)
			}
			if owner != ownerRole {
				t.Fatalf("the owner of %s is %s", table, owner)
			}
		}
		// PUBLIC cannot run the owner functions.
		for _, fn := range []string{"ledger_settle(uuid, text)", "ledger_reverse(uuid, uuid)", "ledger_lock(text, jsonb)"} {
			var public bool
			if err := f.owner.QueryRowContext(f.ctx, "SELECT has_function_privilege('public', $1, 'EXECUTE')", fn).Scan(&public); err != nil {
				t.Fatal(err)
			}
			if public {
				t.Fatalf("PUBLIC may run %s", fn)
			}
		}
	})
}
