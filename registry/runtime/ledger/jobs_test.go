package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jsell-rh/stego/registry/runtime/lease"
	"github.com/jsell-rh/stego/registry/runtime/modes"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

var holderSeq atomic.Int64

// gracePeriod is the period of the test jobs, and so the grace that Check gives an expired hold.
const gracePeriod = time.Second

// holder is a controller replica: a client of the lease row of the real leases table, and the lease it holds.
type holder struct {
	client *lease.Client
	held   atomic.Pointer[lease.Held]
}

func (f *fixture) holder() *holder { return f.holderFor(time.Minute) }

// holderFor is a replica whose lease lasts ttl.
func (f *fixture) holderFor(ttl time.Duration) *holder {
	f.t.Helper()
	c, err := lease.New(f.runtime, leaseName, fmt.Sprintf("replica-%d", holderSeq.Add(1)), lease.Settings{TTL: ttl, Renew: ttl / 2})
	if err != nil {
		f.t.Fatal(err)
	}
	return &holder{client: c}
}

// take acquires the lease, or fails the test.
func (h *holder) take(t *testing.T) *lease.Held {
	t.Helper()
	held, err := h.client.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire the lease: %v", err)
	}
	h.held.Store(held)
	return held
}

// of is the LeaseOf of the replica: the lease it holds, or nil.
func (h *holder) of() *lease.Held { return h.held.Load() }

func (f *fixture) jobs(h *holder) *Jobs { return f.jobsOn(h, f.runtime) }

// jobsOn is the jobs of the replica on the pool db.
func (f *fixture) jobsOn(h *holder, db *sql.DB) *Jobs {
	f.t.Helper()
	j, err := NewJobs(JobsConfig{
		Ledgers: []*Ledger{f.usd},
		Lease:   h.of,
		Tenants: func(context.Context) ([]tenant.Tenant, error) { return []tenant.Tenant{f.tn(acme)}, nil },
		DB:      db,
		Period:  gracePeriod,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return j
}

// snapshots returns acme's snapshots of the account as "amount/entry_count/fence_token", oldest first, and checks
// that each equals the recomputed sum of the posted entries it covers.
func (f *fixture) snapshots(account string) []string {
	f.t.Helper()
	var out []string
	f.inTx(f.owner, acme, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(f.ctx, `SELECT b.amount, b.entry_count, b.fence_token, (
  SELECT coalesce(sum(CASE WHEN e.side = a.normal THEN e.amount ELSE -e.amount END), 0) FROM entries e
  WHERE e.account_id = b.account_id AND NOT e.pending AND e.txid < b.through_xid), (
  SELECT count(*) FROM entries e WHERE e.account_id = b.account_id AND NOT e.pending AND e.txid < b.through_xid)
 FROM balance_snapshots b JOIN accounts a ON a.id = b.account_id AND a.ledger_id = b.ledger_id
 WHERE b.account_id = $1 ORDER BY b.through_xid`, account)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var amount, count, token, recomputed, covered int64
			if err := rows.Scan(&amount, &count, &token, &recomputed, &covered); err != nil {
				return err
			}
			if amount != recomputed || count != covered {
				f.t.Errorf("the snapshot of %s holds %d over %d entries; its entries sum to %d over %d", account, amount, count, recomputed, covered)
			}
			out = append(out, fmt.Sprintf("%d/%d/%d", amount, count, token))
		}
		return rows.Err()
	})
	return out
}

// burnTo uses transaction IDs, as subtransactions of the owner, until the next one is ahead of the next power of
// ten by about n, so that the next n transactions cross it.
// localBurn is the most transaction IDs that a local run of burnTo burns.
const localBurn = 1000000

func (f *fixture) burnTo(t *testing.T, n uint64) {
	t.Helper()
	var next uint64
	if err := f.owner.QueryRowContext(f.ctx, "SELECT pg_current_xact_id()::text::bigint").Scan(&next); err != nil {
		t.Fatal(err)
	}
	power := uint64(10)
	for power <= next+n {
		power *= 10
	}
	// A subtransaction that writes takes a transaction ID of its own, and one statement burns about a million in a
	// few seconds. In CI the test crosses the power of ten on any server below 10^8, and fails above it; an ordinary
	// local run burns at most localBurn, as heavy work stays out of the workstation (AGENTS.md), and else logs that
	// the order test does not cross one.
	burn := power - n - next
	switch {
	case os.Getenv("CI") == "" && burn > localBurn:
		t.Logf("the next transaction ID %d is %d from a power of ten; a local run burns at most %d, so the order test does not cross one (CI does)", next, burn, localBurn)
		return
	case burn > 20000000:
		t.Fatalf("the next transaction ID %d is too far from a power of ten to cross one; run the test on another server", next)
	}
	if _, err := f.owner.ExecContext(f.ctx, `CREATE TEMP TABLE burn (x int);
DO $$ BEGIN FOR i IN 1..`+fmt.Sprint(burn)+` LOOP BEGIN INSERT INTO burn VALUES (i); EXCEPTION WHEN OTHERS THEN NULL; END; END LOOP; END $$;
DROP TABLE burn`); err != nil {
		t.Fatal(err)
	}
}

func TestJobs(t *testing.T) {
	t.Run("ST-SUP-RP08-LC08-T01 Snapshot writes one row per account after the declared number of entries", func(t *testing.T) {
		f := newFixture(t)
		h := f.holder()
		held := h.take(t)
		j := f.jobs(h)
		f.post(f.runtime, "a-1", dr("dr", 10), dr("dr", 20), dr("dr", 30), cr("cr", 60))
		f.post(f.runtime, "b-1", dr("plain", 5), dr("plain", 6), cr("hot", 11))
		if _, err := j.Snapshot(f.ctx); err != nil {
			t.Fatal(err)
		}
		first := f.snapshots("dr")
		if len(first) != 1 || !strings.HasPrefix(first[0], "60/3/") {
			t.Fatalf("the snapshots of A after the first run are %v; want one of 60 over 3 entries", first)
		}
		if got := f.snapshots("plain"); len(got) != 0 {
			t.Fatalf("B has the snapshots %v after two entries; want none", got)
		}
		// A pending transfer's entries count toward no snapshot.
		f.pend("held", dr("dr", 1), dr("dr", 1), dr("dr", 1), cr("plain", 3))
		if _, err := j.Snapshot(f.ctx); err != nil {
			t.Fatal(err)
		}
		if got := f.snapshots("dr"); len(got) != 1 {
			t.Fatalf("A has the snapshots %v after a pending transfer; want the first only", got)
		}
		f.post(f.runtime, "a-2", dr("dr", 1), dr("dr", 2), cr("hot", 3), cr("hot", 4), dr("dr", 4))
		if _, err := j.Snapshot(f.ctx); err != nil {
			t.Fatal(err)
		}
		second := f.snapshots("dr")
		token := fmt.Sprint(held.Token())
		if len(second) != 2 || second[0] != first[0] || second[1] != "67/6/"+token {
			t.Fatalf("the snapshots of A after the second run are %v; want %s and 67/6/%s", second, first[0], token)
		}
		if got := f.snapshots("plain"); len(got) != 0 {
			t.Fatalf("B has the snapshots %v; want none", got)
		}
		if got := f.balance("dr"); got != usd(67) {
			t.Fatalf("the balance of A from its snapshot is %v; want 67 USD", got)
		}
	})

	t.Run("ST-SUP-RP08-LC08-T02 Check finds a ledger that does not sum to zero", func(t *testing.T) {
		f := newFixture(t)
		h := f.holder()
		h.take(t)
		j := f.jobs(h)
		tr := f.post(f.runtime, "k1", dr("dr", 10), cr("cr", 10))
		if err := j.Check(f.ctx); err != nil {
			t.Fatalf("Check of a sound ledger: %v", err)
		}
		f.inTx(f.owner, acme, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(f.ctx, "ALTER TABLE entries DISABLE TRIGGER entries_balance"); err != nil {
				return err
			}
			if _, err := tx.ExecContext(f.ctx, `INSERT INTO entries (tenant_id, ledger_id, transfer_id, seq, account_id, side, amount, currency, pending, effective_at, posted_at)
 SELECT tenant_id, ledger_id, id, 9, 'dr', 'debit', 4, currency, false, effective_at, posted_at FROM transfers WHERE id = $1`, tr.ID); err != nil {
				return err
			}
			_, err := tx.ExecContext(f.ctx, "ALTER TABLE entries ENABLE TRIGGER entries_balance")
			return err
		})
		err := j.Check(f.ctx)
		if err == nil || !strings.Contains(err.Error(), "the ledger usd of the tenant acme does not sum to zero") {
			t.Fatalf("Check: %v; want a failure that names the ledger usd", err)
		}
	})

	t.Run("ST-SUP-RP08-LC08-T03 Check finds a snapshot that differs from its recomputed sum", func(t *testing.T) {
		f := newFixture(t)
		h := f.holder()
		h.take(t)
		j := f.jobs(h)
		f.post(f.runtime, "k1", dr("dr", 10), dr("dr", 20), dr("dr", 30), cr("cr", 60))
		if n, err := j.Snapshot(f.ctx); err != nil || n != 1 {
			t.Fatalf("Snapshot: %d, %v", n, err)
		}
		// A Jobs that has checked the snapshot once does not read it again; this check runs on another Jobs, as a
		// replica that took the lease over would.
		if err := j.Check(f.ctx); err != nil {
			t.Fatalf("Check of a sound snapshot: %v", err)
		}
		j = f.jobs(h)
		var through string
		f.inTx(f.owner, acme, func(tx *sql.Tx) error {
			return tx.QueryRowContext(f.ctx, "UPDATE balance_snapshots SET amount = amount + 1 WHERE account_id = 'dr' RETURNING through_xid::text").Scan(&through)
		})
		err := j.Check(f.ctx)
		want := fmt.Sprintf("the snapshot through_xid %s of the account dr", through)
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "holds 61") {
			t.Fatalf("Check: %v; want a failure that names the account dr and the snapshot %s", err, through)
		}
	})

	t.Run("ST-SUP-RP08-LC08-T04 Check finds an expired pending transfer that is not voided", func(t *testing.T) {
		f := newFixture(t, withTimeout(time.Second))
		h := f.holder()
		h.take(t)
		j := f.jobs(h)
		p := f.pend("hold", dr("dr", 5), cr("cr", 5))
		voided := f.pend("hold-2", dr("dr", 5), cr("cr", 5))
		if _, err := f.call(func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
			return f.usd.Void(f.ctx, tx, tn, voided.ID, "void")
		}); err != nil {
			t.Fatal(err)
		}
		if err := j.Check(f.ctx); err != nil {
			t.Fatalf("Check before the expiry: %v", err)
		}
		// Within one sweep period of the expiry, the sweep has its grace.
		f.waitPast(p.ExpiresAt)
		if err := j.Check(f.ctx); err != nil && time.Now().Before(p.ExpiresAt.Add(gracePeriod-100*time.Millisecond)) {
			t.Fatalf("Check within the grace of the sweep: %v", err)
		}
		f.waitPast(p.ExpiresAt.Add(gracePeriod))
		err := j.Check(f.ctx)
		if err == nil || !strings.Contains(err.Error(), "the expiry sweep did not void the pending transfer "+p.ID) || strings.Contains(err.Error(), voided.ID) {
			t.Fatalf("Check after the expiry: %v; want a failure of the sweep that names %s only", err, p.ID)
		}
		// Once it is voided, the check passes again.
		if _, err := f.call(func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) { return f.usd.Void(f.ctx, tx, tn, p.ID, "void-2") }); err != nil {
			t.Fatal(err)
		}
		if err := j.Check(f.ctx); err != nil {
			t.Fatalf("Check after the void: %v", err)
		}
	})

	t.Run("ST-SUP-RP08-LC08-T05 Snapshot and Check run only in controller mode under the lease, with the fencing token", func(t *testing.T) {
		f := newFixture(t)
		h := f.holder()
		j := f.jobs(h)
		f.post(f.runtime, "k1", dr("dr", 10), dr("dr", 20), dr("dr", 30), cr("cr", 60))
		// The process in the api and the worker mode starts neither job.
		registry := modes.NewRegistry()
		if err := registry.Register(modes.Controller, j.Components(nil)...); err != nil {
			t.Fatal(err)
		}
		for _, sel := range []modes.Selection{{modes.API}, {modes.Worker}, {modes.API, modes.Worker}} {
			if c := registry.Components(sel); len(c) != 0 {
				t.Fatalf("the selection %v runs %d components", sel, len(c))
			}
		}
		names := []string{}
		for _, c := range registry.Components(modes.Selection{modes.Controller}) {
			names = append(names, c.Name)
		}
		if strings.Join(names, ",") != SnapshotJob+","+CheckJob+","+SweepJob {
			t.Fatalf("the controller mode runs %v", names)
		}
		// Another holder has the lease: this replica does no work.
		otherReplica := f.holder()
		otherHeld := otherReplica.take(t)
		if _, err := h.client.Acquire(f.ctx); !errors.Is(err, lease.ErrTaken) {
			t.Fatalf("acquire while another holder has the lease: %v", err)
		}
		if n, err := j.Snapshot(f.ctx); n != 0 || err != nil || len(f.snapshots("dr")) != 0 {
			t.Fatalf("Snapshot without the lease: %d, %v", n, err)
		}
		if err := j.Check(f.ctx); err != nil {
			t.Fatalf("Check without the lease: %v", err)
		}
		// As the holder, in the controller mode: the snapshot carries the holder's token.
		if err := otherHeld.Release(f.ctx); err != nil {
			t.Fatal(err)
		}
		held := h.take(t)
		ctx, cancel := context.WithCancel(f.ctx)
		running, err := registry.Start(ctx, modes.Selection{modes.Controller})
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for len(f.snapshots("dr")) == 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
		if err := running.Wait(); err != nil {
			t.Fatal(err)
		}
		if got := f.snapshots("dr"); len(got) != 1 || got[0] != fmt.Sprintf("60/3/%d", held.Token()) {
			t.Fatalf("the snapshots in the controller mode are %v; want one with the token %d", got, held.Token())
		}
		// Another holder raised the token: the write is fenced and writes no snapshot row.
		f.post(f.runtime, "k2", dr("dr", 1), dr("dr", 2), dr("dr", 3), cr("cr", 6))
		f.inTx(f.owner, acme, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(f.ctx, "UPDATE leases SET token = token + 5, holder = 'usurper' WHERE name = $1", leaseName)
			return err
		})
		if n, err := j.Snapshot(f.ctx); !errors.Is(err, lease.ErrFenced) || n != 0 {
			t.Fatalf("Snapshot after the token was raised: %d, %v; want the fenced error", n, err)
		}
		if got := f.snapshots("dr"); len(got) != 1 {
			t.Fatalf("the snapshots after the fenced run are %v; want the first only", got)
		}
	})

	t.Run("ST-SUP-RP08-LC08-T06 A failed check marks the service unhealthy", func(t *testing.T) {
		f := newFixture(t, withTimeout(time.Second))
		h := f.holder()
		h.take(t)
		j := f.jobs(h)
		if err := j.Check(f.ctx); err != nil {
			t.Fatal(err)
		}
		if ok, reason := j.Health(); !ok || reason != "" {
			t.Fatalf("the health after a passed check is %v %q", ok, reason)
		}
		p := f.pend("hold", dr("dr", 5), cr("cr", 5))
		f.waitPast(p.ExpiresAt.Add(gracePeriod))
		if err := j.Check(f.ctx); err == nil {
			t.Fatal("Check passed with an expired pending transfer")
		}
		ok, reason := j.Health()
		if ok || !strings.HasPrefix(reason, CheckJob+" failed: ") {
			t.Fatalf("the health after a failed check is %v %q; want unhealthy with a reason that names ledger_check", ok, reason)
		}
	})

	t.Run("ST-SUP-RP08-LC08-T07 Check verifies snapshots one after another", func(t *testing.T) {
		f := newFixture(t, func(s *Settings) { s.SnapshotEvery = 10 })
		h := f.holder()
		h.take(t)
		j := f.jobs(h)
		// The transaction IDs of the rounds cross a power of ten, so an order of the snapshots as text would differ
		// from their order as xid8.
		f.burnTo(t, 550)
		for round := range 50 {
			for i := range 10 {
				f.post(f.runtime, fmt.Sprintf("k-%d-%d", round, i), dr("dr", int64(round+1)), cr("cr", int64(round+1)))
			}
			if n, err := j.Snapshot(f.ctx); err != nil || n != 2 {
				t.Fatalf("Snapshot in round %d: %d, %v; want 2 rows", round, n, err)
			}
		}
		var through string
		f.inTx(f.owner, acme, func(tx *sql.Tx) error {
			return tx.QueryRowContext(f.ctx, `UPDATE balance_snapshots SET amount = amount + 1 WHERE account_id = 'dr'
 AND through_xid = (SELECT through_xid FROM balance_snapshots WHERE account_id = 'dr' ORDER BY through_xid OFFSET 24 LIMIT 1)
 RETURNING through_xid::text`).Scan(&through)
		})
		// The statistics of the entries table count the rows that each scan reads. The pools are closed, so every
		// backend has flushed its counts, and the check runs on a pool of its own, which is closed after it.
		for _, db := range []*sql.DB{f.runtime, f.traced, f.owner} {
			db.Close()
		}
		stats := f.open(ownerRole, nil)
		entriesRead := func() int64 {
			// Each other session of the test's database has ended and flushed its counts.
			for i := 0; ; i++ {
				var others int
				if err := stats.QueryRowContext(f.ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid()
 AND backend_type = 'client backend'`).Scan(&others); err != nil {
					t.Fatal(err)
				}
				if others == 0 {
					break
				}
				if i == 300 {
					t.Fatalf("%d other sessions of the test database do not end", others)
				}
				time.Sleep(100 * time.Millisecond)
			}
			var last int64 = -1
			for range 100 {
				var n int64
				if err := stats.QueryRowContext(f.ctx, `SELECT coalesce(seq_tup_read, 0) + coalesce(idx_tup_fetch, 0)
 FROM pg_stat_user_tables WHERE relname = 'entries'`).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n == last {
					return n
				}
				last = n
				time.Sleep(200 * time.Millisecond)
			}
			t.Fatal("the statistics of entries do not settle")
			return 0
		}
		before := entriesRead()
		checkDB := f.open(runtimeRole, nil)
		j2 := f.jobsOn(h, checkDB)
		err := j2.Check(f.ctx)
		checkDB.Close()
		time.Sleep(300 * time.Millisecond)
		read := entriesRead() - before
		if err == nil || strings.Count(err.Error(), "the snapshot through_xid") != 1 ||
			!strings.Contains(err.Error(), "the snapshot through_xid "+through+" of the account dr") {
			t.Fatalf("Check: %v; want a failure that names the snapshot %s of dr only", err, through)
		}
		if read < 1000 || read > 1100 {
			t.Fatalf("Check read %d entry rows for 1000 entries; want each entry once", read)
		}
		// The position stays before the failure, so the next check reports it again; once it is mended, the check
		// passes, and a later check reads only the entries after the last checked position.
		f.runtime, f.owner, f.traced = f.open(runtimeRole, nil), f.open(ownerRole, nil), f.open(runtimeRole, f.trace)
		j2.cfg.DB = f.runtime
		if err := j2.Check(f.ctx); err == nil || !strings.Contains(err.Error(), "the snapshot through_xid "+through+" of the account dr") {
			t.Fatalf("the second Check: %v; want the same failure", err)
		}
		mark := f.trace.mark()
		if err := f.jobsOn(h, f.traced).Check(f.ctx); err == nil {
			t.Fatal("a new Jobs passed the check before the mend")
		}
		sent := strings.ToLower(strings.Join(f.trace.since(mark), "\n"))
		if !strings.Contains(sent, "begin isolation level repeatable read") || !strings.Contains(sent, "set transaction read only") {
			t.Fatalf("Check does not run as REPEATABLE READ READ ONLY; it sent:\n%s", sent)
		}
		f.inTx(f.owner, acme, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(f.ctx, "UPDATE balance_snapshots SET amount = amount - 1 WHERE account_id = 'dr' AND through_xid = $1::xid8", through)
			return err
		})
		if err := j2.Check(f.ctx); err != nil {
			t.Fatalf("Check after the mend: %v", err)
		}
		for i := range 10 {
			f.post(f.runtime, fmt.Sprintf("late-%d", i), dr("dr", 1), cr("cr", 1))
		}
		if n, err := j2.Snapshot(f.ctx); err != nil || n != 2 {
			t.Fatalf("the late Snapshot: %d, %v", n, err)
		}
		f.runtime.Close()
		f.owner.Close()
		before = entriesRead()
		checkDB = f.open(runtimeRole, nil)
		j2.cfg.DB = checkDB
		if err := j2.Check(f.ctx); err != nil {
			t.Fatalf("the incremental Check: %v", err)
		}
		checkDB.Close()
		time.Sleep(300 * time.Millisecond)
		if read := entriesRead() - before; read < 20 || read > 40 {
			t.Fatalf("the incremental Check read %d entry rows for 20 new entries", read)
		}
	})

	t.Run("ST-SUP-RP08-LC08-T08 A job ends before the lease deadline", func(t *testing.T) {
		f := newFixture(t)
		f.post(f.runtime, "fund", dr("hot", 100), cr("limited", 100))
		// The other session's long transaction holds the account's row lock. The job ends before the lease deadline
		// whether it starts with the whole budget or near the end of the lease, and a renew does not wait for it.
		for _, c := range []struct {
			name  string
			late  time.Duration
			lock  time.Duration
			codes []string
		}{
			{"whole budget", 0, 0, []string{"55P03"}},
			{"long lock timeout", 0, time.Minute, []string{"55P03"}},
			{"near the lease end", 1500 * time.Millisecond, time.Minute, []string{"55P03", ""}},
		} {
			h := f.holderFor(2 * time.Second)
			held := h.take(t)
			j, err := NewJobs(JobsConfig{Ledgers: []*Ledger{f.usd}, Lease: h.of, DB: f.runtime, LockTimeout: c.lock,
				Tenants: func(context.Context) ([]tenant.Tenant, error) { return []tenant.Tenant{f.tn(acme)}, nil }})
			if err != nil {
				t.Fatal(err)
			}
			locker, err := tenant.Begin(f.ctx, f.owner, f.tn(acme), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := locker.ExecContext(f.ctx, "SELECT 1 FROM accounts WHERE id = 'limited' FOR UPDATE"); err != nil {
				t.Fatal(err)
			}
			type renew struct {
				wait time.Duration
				err  error
			}
			renewed := make(chan renew, 1)
			started := make(chan struct{})
			go func() {
				<-started
				start := time.Now()
				err := held.Renew(f.ctx)
				renewed <- renew{time.Since(start), err}
			}()
			time.Sleep(c.late)
			done := make(chan error, 1)
			go func() {
				_, err := j.Snapshot(f.ctx)
				done <- err
			}()
			if c.late == 0 {
				f.waitForLockWait(t)
			}
			close(started)
			err = <-done
			ended := time.Now()
			r := <-renewed
			_ = locker.Rollback()
			if !slices.Contains(c.codes, sqlState(err)) || err == nil {
				t.Fatalf("%s: Snapshot under a held account lock: %v; want SQLSTATE %v", c.name, err, c.codes)
			}
			if sqlState(err) == "" && !errors.Is(err, ErrDeadline) {
				t.Fatalf("%s: %v; want the lock timeout or ErrDeadline", c.name, err)
			}
			if !ended.Before(held.Expires()) {
				t.Fatalf("%s: Snapshot ended at %v, after the lease deadline %v", c.name, ended, held.Expires())
			}
			// The renew waits at most for the lock timeout of the job, a quarter of its budget of half the renew
			// slack: 125 ms for this lease.
			if r.err != nil || r.wait > 400*time.Millisecond {
				t.Fatalf("%s: the renew waited %v for the job: %v", c.name, r.wait, r.err)
			}
			if err := held.Release(f.ctx); err != nil {
				t.Fatal(err)
			}
		}
	})

	t.Run("ST-SUP-RP08-LC11-T01 The sweep voids an expired hold once", func(t *testing.T) {
		f := newFixture(t, withTimeout(time.Second))
		p := f.pend("hold", dr("dr", 5), cr("cr", 5))
		// Replica b holds the lease; it expires, and replica a takes it over with a higher token, so b is fenced.
		b := f.holder()
		b.take(t)
		f.inTx(f.owner, acme, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(f.ctx, "UPDATE leases SET expires_at = now() - interval '1 second' WHERE name = $1", leaseName)
			return err
		})
		a := f.holder()
		a.take(t)
		f.waitPast(p.ExpiresAt)
		if n, err := f.jobs(b).Sweep(f.ctx); n != 0 || !errors.Is(err, lease.ErrFenced) {
			t.Fatalf("the sweep of the fenced replica: %d, %v; want the fenced error", n, err)
		}
		if n := f.count("transfers", "pending_id = $1", p.ID); n != 0 {
			t.Fatalf("the fenced replica wrote %d voids", n)
		}
		ja := f.jobs(a)
		for run, want := range []int{1, 0} {
			if n, err := ja.Sweep(f.ctx); err != nil || n != want {
				t.Fatalf("sweep %d: %d, %v; want %d voids", run+1, n, err, want)
			}
		}
		if n := f.count("transfers", "pending_id = $1 AND kind = 'void' AND idempotency_key = ledger_key('acme', $2)", p.ID, "expire:"+p.ID); n != 1 {
			t.Fatalf("%d voids with the key expire:%s; want 1", n, p.ID)
		}
		if err := ja.Check(f.ctx); err != nil {
			t.Fatalf("Check after the sweep: %v", err)
		}
	})
}
