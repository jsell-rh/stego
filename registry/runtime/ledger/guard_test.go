package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"
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
		// The owner who sets the guard setting by hand changes the status with no new row: the commit refuses it.
		err = f.try(f.owner, acme, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(f.ctx, "SELECT set_config($1, 'settle:' || $2, true)", GuardSetting, p.ID); err != nil {
				return err
			}
			_, err := tx.ExecContext(f.ctx, "UPDATE transfers SET status = 'posted' WHERE id = $1", p.ID)
			return err
		})
		if sqlState(err) != "23514" || !strings.Contains(err.Error(), "no new row") {
			t.Fatalf("a status change with no new row: %v; want the commit refused", err)
		}
		// The post function from pending, as the owner.
		post := func(key string) error {
			return f.try(f.owner, acme, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(f.ctx, `SELECT ledger_post_pending('acme', 'usd', $1, $2::uuid,
 '[{"account": "limited", "side": "debit", "amount": 100}, {"account": "plain", "side": "credit", "amount": 100}]')`, key, p.ID)
				return err
			})
		}
		if err := post("owner-post-1"); err != nil {
			t.Fatalf("the post function on the pending transfer: %v; want it accepted from pending", err)
		}
		if n := f.count("transfers", "id = $1 AND status = 'posted'", p.ID); n != 1 {
			t.Fatal("the status is not posted")
		}
		if err := post("owner-post-2"); sqlState(err) != codeAlreadyPosted {
			t.Fatalf("the post function on the posted transfer: %v; want %s", err, codeAlreadyPosted)
		}
		if err := f.raw("SELECT ledger_void('acme', 'usd', 'void', $1::uuid)", p.ID); sqlState(err) != codeAlreadyPosted {
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

	t.Run("ST-SUP-RP08-LC09-T03 The runtime role has SELECT only on the ledger tables", func(t *testing.T) {
		f := newFixture(t)
		for _, table := range []string{"transfers", "entries", "accounts", "balance_snapshots"} {
			for privilege, want := range map[string]bool{"SELECT": true, "INSERT": false, "UPDATE": false, "DELETE": false, "TRUNCATE": false} {
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
		// The runtime role runs the owner functions and ledger_balance, and no other ledger function; PUBLIC none.
		rows, err := f.owner.QueryContext(f.ctx, `SELECT p.oid::regprocedure::text, has_function_privilege($1, p.oid, 'EXECUTE'),
 has_function_privilege('public', p.oid, 'EXECUTE'), p.prosecdef FROM pg_proc p WHERE p.proname LIKE 'ledger%' ORDER BY 1`, runtimeRole)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var runs []string
		for rows.Next() {
			var fn string
			var runtime, public, definer bool
			if err := rows.Scan(&fn, &runtime, &public, &definer); err != nil {
				t.Fatal(err)
			}
			if public {
				t.Errorf("PUBLIC may run %s", fn)
			}
			if runtime {
				runs = append(runs, fn)
				if !definer && !strings.HasPrefix(fn, "ledger_balance(") {
					t.Errorf("%s, which the runtime role runs, is not SECURITY DEFINER", fn)
				}
			}
		}
		want := []string{"ledger_balance(text,text)"}
		for _, fn := range OwnerFunctions {
			want = append(want, strings.ReplaceAll(strings.ReplaceAll(fn, ", ", ","), "timestamptz", "timestamp with time zone"))
		}
		slices.Sort(want)
		slices.Sort(runs)
		if fmt.Sprint(runs) != fmt.Sprint(want) {
			t.Fatalf("the runtime role runs %v; want %v", runs, want)
		}
		// Each owner function fixes its search_path.
		var loose int
		if err := f.owner.QueryRowContext(f.ctx, `SELECT count(*) FROM pg_proc WHERE proname LIKE 'ledger%'
 AND NOT coalesce(proconfig @> ARRAY['search_path=pg_catalog, pg_temp'], false)`).Scan(&loose); err != nil {
			t.Fatal(err)
		}
		if loose != 0 {
			t.Fatalf("%d ledger functions have no fixed search_path", loose)
		}
	})

	t.Run("ST-SUP-RP08-LC09-T04 Accounts cannot be changed or deleted", func(t *testing.T) {
		f := newFixture(t)
		account := f.row("accounts", "id = 'limited'")
		for _, statement := range []string{
			"UPDATE accounts SET normal = 'debit' WHERE id = 'limited'",
			"UPDATE accounts SET currency = 'EUR' WHERE id = 'limited'",
			"UPDATE accounts SET min_balance = NULL WHERE id = 'limited'",
			"DELETE FROM accounts WHERE id = 'limited'",
			"TRUNCATE accounts",
		} {
			err := f.try(f.owner, acme, func(tx *sql.Tx) error { _, err := tx.ExecContext(f.ctx, statement); return err })
			if sqlState(err) != "42501" || !strings.Contains(err.Error(), guardMessage) {
				t.Fatalf("%s as the owner: %v; want the guard's error", statement, err)
			}
		}
		if f.row("accounts", "id = 'limited'") != account {
			t.Fatal("the account changed")
		}
	})

	t.Run("ST-SUP-RP08-LC09-T05 Each owner function checks its arguments and tenant", func(t *testing.T) {
		f := newFixture(t)
		o := f.post(f.runtime, "orig", dr("hot", 50), cr("plain", 50))
		f.post(f.runtime, "fund", dr("hot", 100), cr("limited", 100))
		p := f.pend("hold", dr("limited", 10), cr("plain", 10))
		pair := `[{"account": "hot", "side": "debit", "amount": 5}, {"account": "plain", "side": "credit", "amount": 5}]`
		negative := `[{"account": "hot", "side": "debit", "amount": -5}, {"account": "plain", "side": "credit", "amount": -5}]`
		before := fmt.Sprint(f.count("transfers", "true"), f.count("entries", "true"), f.count("accounts", "true"), f.count("balance_snapshots", "true"))
		calls := map[string][]any{
			"post for another tenant":         {"SELECT ledger_post('other', 'usd', 'k', 'topup', $1::jsonb, NULL)", pair},
			"pend for another tenant":         {"SELECT ledger_pend('other', 'usd', 'k', 'purchase', $1::jsonb, NULL)", pair},
			"post pending for another tenant": {"SELECT ledger_post_pending('other', 'usd', 'k', $1::uuid, '[]')", p.ID},
			"void for another tenant":         {"SELECT ledger_void('other', 'usd', 'k', $1::uuid)", p.ID},
			"reverse for another tenant":      {"SELECT ledger_reverse('other', 'usd', 'k', $1::uuid, '[]')", o.ID},
			"snapshot for another tenant":     {"SELECT ledger_snapshot('other', 'usd', 1)"},
			"open for another tenant":         {"SELECT ledger_open('other', 'usd', 'x', 'fees', 'credit', 'USD', NULL, false)"},
			"post a negative amount":          {"SELECT ledger_post('acme', 'usd', 'k', 'topup', $1::jsonb, NULL)", negative},
			"pend a negative amount":          {"SELECT ledger_pend('acme', 'usd', 'k', 'purchase', $1::jsonb, NULL)", negative},
			"post pending a negative amount":  {"SELECT ledger_post_pending('acme', 'usd', 'k', $1::uuid, $2::jsonb)", p.ID, negative},
			"post an empty entry list":        {"SELECT ledger_post('acme', 'usd', 'k', 'topup', '[]', NULL)"},
			"pend an empty entry list":        {"SELECT ledger_pend('acme', 'usd', 'k', 'purchase', '[]', NULL)"},
			"post pending no entries":         {"SELECT ledger_post_pending('acme', 'usd', 'k', $1::uuid, '[]')", p.ID},
			"reverse no entries":              {"SELECT ledger_reverse('acme', 'usd', 'k', $1::uuid, '[]')", o.ID},
			"post pending a null ID":          {"SELECT ledger_post_pending('acme', 'usd', 'k', NULL, $1::jsonb)", pair},
			"void a null ID":                  {"SELECT ledger_void('acme', 'usd', 'k', NULL)"},
			"reverse a null ID":               {"SELECT ledger_reverse('acme', 'usd', 'k', NULL, $1::jsonb)", pair},
			"open a null ID":                  {"SELECT ledger_open('acme', 'usd', NULL, 'fees', 'credit', 'USD', NULL, false)"},
			"snapshot a null token":           {"SELECT ledger_snapshot('acme', 'usd', NULL)"},
			"post a code not declared":        {"SELECT ledger_post('acme', 'usd', 'k', 'refund', $1::jsonb, NULL)", pair},
			"post a null key":                 {"SELECT ledger_post('acme', 'usd', NULL, 'topup', $1::jsonb, NULL)", pair},
			"post a ledger not declared":      {"SELECT ledger_post('acme', 'eur', 'k', 'topup', $1::jsonb, NULL)", pair},
			"post a fraction":                 {`SELECT ledger_post('acme', 'usd', 'k', 'topup', '[{"account": "hot", "side": "debit", "amount": 1.5}, {"account": "plain", "side": "credit", "amount": 1.5}]', NULL)`},
			"post beyond int64":               {`SELECT ledger_post('acme', 'usd', 'k', 'topup', '[{"account": "hot", "side": "debit", "amount": 9223372036854775808}, {"account": "plain", "side": "credit", "amount": 9223372036854775808}]', NULL)`},
			"open in another currency":        {"SELECT ledger_open('acme', 'usd', 'x', 'fees', 'credit', 'EUR', NULL, false)"},
			"open a hot account with a limit": {"SELECT ledger_open('acme', 'usd', 'x', 'fees', 'credit', 'USD', 0, true)"},
		}
		for name, c := range calls {
			if err := f.raw(c[0].(string), c[1:]...); err == nil {
				t.Errorf("%s: accepted", name)
			}
		}
		after := fmt.Sprint(f.count("transfers", "true"), f.count("entries", "true"), f.count("accounts", "true"), f.count("balance_snapshots", "true"))
		if after != before {
			t.Fatalf("the row counts were %s and are %s", before, after)
		}
	})

	t.Run("ST-SUP-RP08-LC03-T06 The runtime role cannot insert a transfer or an entry directly", func(t *testing.T) {
		f := newFixture(t)
		o := f.post(f.runtime, "orig", dr("hot", 50), cr("plain", 50))
		before := fmt.Sprint(f.count("transfers", "true"), f.count("entries", "true"))
		for _, statement := range []string{
			`INSERT INTO transfers (tenant_id, ledger_id, idempotency_key, fingerprint, kind, code, amount, currency, status, effective_at)
 VALUES ('acme', 'usd', 'raw', 'fp', 'transfer', 'topup', 5, 'USD', 'posted', now())`,
			`INSERT INTO entries (tenant_id, ledger_id, transfer_id, seq, account_id, side, amount, currency, pending, effective_at, posted_at)
 VALUES ('acme', 'usd', '` + o.ID + `', 9, 'hot', 'debit', 5, 'USD', false, now(), now()), ('acme', 'usd', '` + o.ID + `', 10, 'plain', 'credit', 5, 'USD', false, now(), now())`,
			`INSERT INTO balance_snapshots (tenant_id, ledger_id, account_id, through_xid, amount, entry_count, fence_token)
 VALUES ('acme', 'usd', 'hot', '1', 1000000, 1, 1)`,
			`INSERT INTO accounts (tenant_id, ledger_id, id, code, normal, currency) VALUES ('acme', 'usd', 'x', 'fees', 'credit', 'USD')`,
		} {
			if err := f.raw(statement); sqlState(err) != "42501" {
				t.Fatalf("%s as the runtime role: %v; want a permission error", statement, err)
			}
		}
		for _, table := range []string{"transfers", "entries"} {
			if err := f.copyAsRuntime(table); sqlState(err) != "42501" {
				t.Fatalf("COPY %s as the runtime role: %v; want a permission error", table, err)
			}
		}
		if after := fmt.Sprint(f.count("transfers", "true"), f.count("entries", "true")); after != before {
			t.Fatalf("the row counts were %s and are %s", before, after)
		}
	})

	t.Run("ST-SUP-RP08-LC06-T05 A crafted reversal row is refused", func(t *testing.T) {
		f := newFixture(t)
		o := f.post(f.runtime, "orig", dr("hot", 50), cr("plain", 50))
		balances := fmt.Sprint(f.balance("hot"), f.balance("plain"), f.balance("dr"), f.balance("cr"))
		for name, entries := range map[string]string{
			"other accounts":       `[{"account": "cr", "side": "credit", "amount": 50}, {"account": "dr", "side": "debit", "amount": 50}]`,
			"the original's sides": `[{"account": "hot", "side": "debit", "amount": 50}, {"account": "plain", "side": "credit", "amount": 50}]`,
			"another amount":       `[{"account": "hot", "side": "credit", "amount": 40}, {"account": "plain", "side": "debit", "amount": 40}]`,
			"another order":        `[{"account": "plain", "side": "debit", "amount": 50}, {"account": "hot", "side": "credit", "amount": 50}]`,
		} {
			err := f.raw("SELECT ledger_reverse('acme', 'usd', $1, $2::uuid, $3::jsonb)", "crafted-"+name, o.ID, entries)
			if sqlState(err) != codeNotMirror {
				t.Fatalf("ledger_reverse with %s: %v; want %s", name, err, codeNotMirror)
			}
		}
		if got, err := f.getTransfer(o.ID); err != nil || got.ReversedBy != "" {
			t.Fatalf("reversed_by of the original is %q, %v; want unset", got.ReversedBy, err)
		}
		if after := fmt.Sprint(f.balance("hot"), f.balance("plain"), f.balance("dr"), f.balance("cr")); after != balances {
			t.Fatalf("the balances were %s and are %s", balances, after)
		}
	})
}

// copyAsRuntime runs COPY FROM STDIN into the table as the runtime role, through pgx's own connection.
func (f *fixture) copyAsRuntime(table string) error {
	conn, err := f.runtime.Conn(f.ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Raw(func(dc any) error {
		pg := dc.(*stdlib.Conn).Conn().PgConn()
		_, err := pg.CopyFrom(context.Background(), strings.NewReader(""), "COPY "+table+" FROM STDIN")
		return err
	})
}
