package ledger

import (
	"database/sql"
	"errors"
	"math"
	"testing"
	"time"

	"stego.dev/runtime/money"
	"stego.dev/runtime/tenant"
)

// balance reads the balance of acme's account in a transaction of its own.
func (f *fixture) balance(account string) money.Money {
	f.t.Helper()
	var out money.Money
	f.inTx(f.runtime, acme, func(tx *sql.Tx) error {
		var err error
		out, err = f.usd.Balance(f.ctx, tx, f.tn(acme), account)
		return err
	})
	return out
}

// dbNow is the database clock.
func (f *fixture) dbNow() time.Time {
	f.t.Helper()
	var now time.Time
	if err := f.admin.QueryRowContext(f.ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		f.t.Fatal(err)
	}
	return now
}

// postAt posts a transfer for acme that takes effect at the time.
func (f *fixture) postAt(key string, at time.Time, entries ...Entry) Transfer {
	f.t.Helper()
	var out Transfer
	f.inTx(f.runtime, acme, func(tx *sql.Tx) error {
		var err error
		out, err = f.usd.Post(f.ctx, tx, f.tn(acme), Posting{Key: key, Code: "topup", Entries: entries, EffectiveAt: at})
		return err
	})
	return out
}

func TestBalance(t *testing.T) {
	t.Run("ST-SUP-RP08-LC07-T01 A balance is the latest snapshot plus the entries posted after it", func(t *testing.T) {
		f := newFixture(t)
		// 300 before the snapshot, which the snapshot of 1,000 does not match, so a Balance that sums every entry
		// gives 550 and one that ignores the snapshot gives 550 too.
		f.post(f.runtime, "before", dr("hot", 300), cr("cr", 300))
		f.inTx(f.owner, acme, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(f.ctx, `INSERT INTO balance_snapshots (tenant_id, ledger_id, account_id, through_xid, amount, entry_count, fence_token)
 VALUES ('acme', 'usd', 'cr', pg_snapshot_xmin(pg_current_snapshot()), 1000, 1, 0)`)
			return err
		})
		f.post(f.runtime, "after-1", dr("hot", 200), cr("cr", 200))
		f.post(f.runtime, "after-2", dr("hot", 50), cr("cr", 50))
		if got := f.balance("cr"); got != usd(1250) {
			t.Fatalf("the balance is %v; want 1250 USD, the snapshot of 1000 and the entries of 200 and 50 after it", got)
		}
		// A later snapshot is the latest one.
		f.inTx(f.owner, acme, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(f.ctx, `INSERT INTO balance_snapshots (tenant_id, ledger_id, account_id, through_xid, amount, entry_count, fence_token)
 VALUES ('acme', 'usd', 'cr', pg_snapshot_xmin(pg_current_snapshot()), 5, 3, 0)`)
			return err
		})
		f.post(f.runtime, "after-3", dr("hot", 1), cr("cr", 1))
		if got := f.balance("cr"); got != usd(6) {
			t.Fatalf("the balance is %v; want 6 USD from the latest snapshot", got)
		}
	})

	t.Run("ST-SUP-RP08-LC07-T02 A balance is signed by the account's normal side", func(t *testing.T) {
		f := newFixture(t)
		for _, a := range []string{"dr", "cr"} {
			f.post(f.runtime, "debit-"+a, dr(a, 100), cr("plain", 100))
			f.post(f.runtime, "credit-"+a, dr("hot", 30), cr(a, 30))
		}
		if got := f.balance("dr"); got != usd(70) {
			t.Fatalf("the debit-normal balance is %v; want 70 USD", got)
		}
		if got := f.balance("cr"); got != usd(-70) {
			t.Fatalf("the credit-normal balance is %v; want -70 USD", got)
		}
		// The arithmetic of the sum is exact, and a balance beyond int64 is an overflow error, not a wrapped value.
		f.post(f.runtime, "big-1", dr("dr", math.MaxInt64), cr("plain", math.MaxInt64))
		err := f.try(f.runtime, acme, func(tx *sql.Tx) error {
			_, err := f.usd.Balance(f.ctx, tx, f.tn(acme), "dr")
			return err
		})
		if !errors.Is(err, money.ErrOverflow) {
			t.Fatalf("a balance of 70 + MaxInt64: %v; want ErrOverflow", err)
		}
		if _, err := f.call(func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
			return f.usd.Post(f.ctx, tx, tn, Posting{Key: "big-2", Code: "topup", Entries: []Entry{dr("dr", math.MaxInt64), dr("dr", 1), cr("plain", 1)}})
		}); !errors.Is(err, money.ErrOverflow) {
			t.Fatalf("debits over MaxInt64: %v; want ErrOverflow", err)
		}
	})

	t.Run("ST-SUP-RP08-LC07-T03 A balance at a time selects entries by effective_at", func(t *testing.T) {
		f := newFixture(t)
		// Every time is fixed relative to the database clock and lies in the past, so the bound on EffectiveAt never
		// refuses it, whatever the hour the test runs at.
		now := f.dbNow()
		older, newer := now.Add(-2*time.Hour), now.Add(-time.Hour)
		f.postAt("newer", newer, dr("dr", 40), cr("plain", 40))
		f.postAt("older", older, dr("dr", 7), cr("plain", 7))
		var between, all money.Money
		f.inTx(f.runtime, acme, func(tx *sql.Tx) error {
			var err error
			if between, err = f.usd.BalanceAt(f.ctx, tx, f.tn(acme), "dr", now.Add(-90*time.Minute)); err != nil {
				return err
			}
			all, err = f.usd.BalanceAt(f.ctx, tx, f.tn(acme), "dr", newer)
			return err
		})
		if between != usd(7) || all != usd(47) {
			t.Fatalf("the balance 90 minutes ago is %v and at the newer entry %v; want 7 USD and 47 USD", between, all)
		}
	})

	t.Run("ST-SUP-RP08-LC07-T04 Every entry keeps effective_at and posted_at", func(t *testing.T) {
		f := newFixture(t)
		tr := f.postAt("backdated", f.dbNow().Add(-48*time.Hour), dr("dr", 9), cr("plain", 9))
		var rows, ordered int
		f.inTx(f.owner, acme, func(tx *sql.Tx) error {
			return tx.QueryRowContext(f.ctx, `SELECT count(*), count(*) FILTER (WHERE effective_at < posted_at)
 FROM entries WHERE transfer_id = $1 AND effective_at IS NOT NULL AND posted_at IS NOT NULL`, tr.ID).Scan(&rows, &ordered)
		})
		if rows != 2 || ordered != 2 {
			t.Fatalf("%d entries keep both times and %d have effective_at before posted_at; want 2 and 2", rows, ordered)
		}
		var nullable int
		f.inTx(f.owner, acme, func(tx *sql.Tx) error {
			return tx.QueryRowContext(f.ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'entries'
 AND column_name IN ('effective_at', 'posted_at') AND is_nullable = 'NO'`).Scan(&nullable)
		})
		if nullable != 2 {
			t.Fatalf("%d of effective_at and posted_at are NOT NULL; want 2", nullable)
		}
	})

	t.Run("ST-SUP-RP08-LC07-T05 A forged snapshot cannot be written", func(t *testing.T) {
		f := newFixture(t)
		f.post(f.runtime, "k1", dr("hot", 30), cr("cr", 30))
		err := f.raw(`INSERT INTO balance_snapshots (tenant_id, ledger_id, account_id, through_xid, amount, entry_count, fence_token)
 VALUES ('acme', 'usd', 'cr', pg_snapshot_xmin(pg_current_snapshot()), 1000000, 1, 1)`)
		if sqlState(err) != "42501" {
			t.Fatalf("a direct snapshot insert as the runtime role: %v; want a permission error", err)
		}
		if err := f.raw("SELECT ledger_snapshot('acme', 'usd', 1, 1000000)"); sqlState(err) != "42883" {
			t.Fatalf("ledger_snapshot with an amount: %v; want no such function", err)
		}
		var args string
		f.inTx(f.owner, acme, func(tx *sql.Tx) error {
			return tx.QueryRowContext(f.ctx, "SELECT pg_get_function_arguments('ledger_snapshot'::regproc)").Scan(&args)
		})
		if args != "p_tenant text, p_ledger text, p_token bigint" {
			t.Fatalf("ledger_snapshot takes %q; want no amount", args)
		}
		if got := f.balance("cr"); got != usd(30) {
			t.Fatalf("the balance is %v; want 30 USD, the sum of the entries", got)
		}
	})

	t.Run("ST-SUP-RP08-LC07-T06 An expired hold does not count against the limit", func(t *testing.T) {
		f := newFixture(t, withTimeout(time.Second))
		f.post(f.runtime, "fund", dr("hot", 100), cr("limited", 100))
		p := f.pend("hold", dr("limited", 100), cr("plain", 100))
		debit := func(key string) error {
			_, err := f.call(func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
				return f.usd.Post(f.ctx, tx, tn, Posting{Key: key, Code: "purchase", Entries: []Entry{dr("limited", 1), cr("plain", 1)}})
			})
			return err
		}
		if err := debit("before"); !errors.Is(err, ErrBelowMinimum) {
			t.Fatalf("a debit of 1 while the hold is open: %v; want ErrBelowMinimum", err)
		}
		// At the boundary: a transaction that began before the expiry and posts after it counts the hold as expired,
		// because ledger_post reads the clock after its locks.
		tx, err := tenant.Begin(f.ctx, f.runtime, f.tn(acme), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		var started time.Time
		if err := tx.QueryRowContext(f.ctx, "SELECT now()").Scan(&started); err != nil {
			t.Fatal(err)
		}
		if !started.Before(p.ExpiresAt) {
			t.Fatalf("the transaction began at %v, not before the expiry %v", started, p.ExpiresAt)
		}
		f.waitPast(p.ExpiresAt)
		if _, err := f.usd.Post(f.ctx, tx, f.tn(acme), Posting{Key: "after", Code: "purchase", Entries: []Entry{dr("limited", 1), cr("plain", 1)}}); err != nil {
			t.Fatalf("a debit of 1 after the hold expired, in a transaction that began before: %v; want it accepted", err)
		}
	})
}
