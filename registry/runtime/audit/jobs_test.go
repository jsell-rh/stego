package audit

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"stego.dev/runtime/lease"
	"stego.dev/runtime/tenant"
)

var errFenced = lease.ErrFenced

// testLease is one holder of the controller lease of the lease runtime, on the leases table of the test database.
type testLease struct {
	f      *pgFixture
	mu     sync.Mutex
	client *lease.Client
	held   *lease.Held
}

var holderSeq atomic.Int64

// newLease returns a holder that does not hold the lease yet.
func (f *pgFixture) newLease() *testLease {
	f.t.Helper()
	c, err := lease.New(f.runtime, leaseName, fmt.Sprintf("holder-%d", holderSeq.Add(1)), lease.Settings{})
	if err != nil {
		f.t.Fatal(err)
	}
	return &testLease{f: f, client: c}
}

// lease returns a holder that holds the lease.
func (f *pgFixture) lease() *testLease {
	l := f.newLease()
	l.take()
	return l
}

// take lets the lease of any other holder run out, as the owner, and acquires it with a higher token.
func (l *testLease) take() {
	l.f.t.Helper()
	if _, err := l.f.owner.ExecContext(l.f.ctx, "UPDATE leases SET expires_at = '-infinity' WHERE name = $1", leaseName); err != nil {
		l.f.t.Fatal(err)
	}
	h, err := l.client.Acquire(l.f.ctx)
	if err != nil {
		l.f.t.Fatal(err)
	}
	l.mu.Lock()
	l.held = h
	l.mu.Unlock()
}

// lose releases the lease.
func (l *testLease) lose() {
	l.mu.Lock()
	h := l.held
	l.held = nil
	l.mu.Unlock()
	if h != nil {
		if err := h.Release(l.f.ctx); err != nil {
			l.f.t.Fatal(err)
		}
	}
}

// of is the LeaseOf of the holder.
func (l *testLease) of() *lease.Held {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held
}

func (f *pgFixture) tenants(ids ...string) TenantsOf {
	list := make([]tenant.Tenant, len(ids))
	for i, id := range ids {
		list[i] = f.tn(id)
	}
	return func(context.Context) ([]tenant.Tenant, error) { return list, nil }
}

func (f *pgFixture) anchorer(l *testLease, period time.Duration, ids ...string) *Anchorer {
	f.t.Helper()
	a, err := NewAnchorer(AnchorConfig{Period: period, Lease: l.of, Tenants: f.tenants(ids...), DB: f.runtime, Outbox: f.outbox})
	if err != nil {
		f.t.Fatal(err)
	}
	return a
}

func (f *pgFixture) maintainer(now func() time.Time) *Maintainer {
	f.t.Helper()
	m, err := NewMaintainer(MaintainConfig{Lease: f.lease().of, DB: f.audit, Outbox: f.outbox, Now: now})
	if err != nil {
		f.t.Fatal(err)
	}
	return m
}

type anchorPayload struct {
	Kind       string `json:"kind"`
	Tenant     string `json:"tenant"`
	AnchorKind string `json:"anchor_kind"`
	Head       string `json:"head"`
	Count      string `json:"count"`
	key        string
	rowTenant  string
}

func (f *pgFixture) anchorMessages(prefix string) []anchorPayload {
	f.t.Helper()
	var out []anchorPayload
	for _, m := range f.messages() {
		if !strings.HasPrefix(m.key, prefix) {
			continue
		}
		var p anchorPayload
		if err := json.Unmarshal(m.payload, &p); err != nil {
			f.t.Fatal(err)
		}
		p.key, p.rowTenant = m.key, m.tenant
		out = append(out, p)
	}
	return out
}

func clock(h, m int) time.Time { return time.Date(2026, 3, 20, h, m, 0, 0, time.UTC) }

func TestJobs(t *testing.T) {
	t.Run("ST-SUP-RP05-LC09-T01 The anchor job publishes the head hash and the row count of each tenant's chain", func(t *testing.T) {
		f := newPG(t)
		w := f.writer()
		t1 := f.recordN(w, "t1", 3)
		t2 := f.recordN(w, "t2", 2)
		a := f.anchorer(f.lease(), time.Hour, "t1", "t2")
		if err := a.Tick(f.ctx, clock(10, 20)); err != nil {
			t.Fatal(err)
		}
		got := f.anchorMessages("anchor/")
		if len(got) != 2 {
			t.Fatalf("%d anchor messages, want 2: %+v", len(got), got)
		}
		for i, want := range []struct {
			tenant, head, count string
		}{{"t1", hex.EncodeToString(t1[2].Hash), "3"}, {"t2", hex.EncodeToString(t2[1].Hash), "2"}} {
			g := got[i]
			if g.Tenant != want.tenant || g.rowTenant != want.tenant || g.Head != want.head || g.Count != want.count || g.AnchorKind != "periodic" {
				t.Errorf("anchor %d is %+v, want %s with the head %s and the count %s on an outbox row of %s", i, g, want.tenant, want.head, want.count, want.tenant)
			}
		}
	})

	t.Run("ST-SUP-RP05-LC09-T02 The anchor job anchors each fixed boundary once", func(t *testing.T) {
		f := newPG(t)
		a := f.anchorer(f.lease(), time.Hour, "t1", "t2")
		// The lease is taken at 10:20 while the chains hold no row: nothing to anchor yet.
		if err := a.Tick(f.ctx, clock(10, 20)); err != nil {
			t.Fatal(err)
		}
		w := f.writer()
		f.recordN(w, "t1", 1)
		f.recordN(w, "t2", 1)
		for now := clock(10, 30); !now.After(clock(13, 20)); now = now.Add(10 * time.Minute) {
			if err := a.Tick(f.ctx, now); err != nil {
				t.Fatal(err)
			}
		}
		// Then the clock jumps over three boundaries.
		if err := a.Tick(f.ctx, clock(17, 20)); err != nil {
			t.Fatal(err)
		}
		for _, tenant := range []string{"t1", "t2"} {
			var keys []string
			for _, m := range f.anchorMessages("anchor/" + tenant + "/") {
				keys = append(keys, strings.TrimPrefix(m.key, "anchor/"+tenant+"/"))
			}
			want := []string{"2026-03-20T11:00:00.000000Z", "2026-03-20T12:00:00.000000Z", "2026-03-20T13:00:00.000000Z", "2026-03-20T17:00:00.000000Z"}
			if strings.Join(keys, ",") != strings.Join(want, ",") {
				t.Errorf("the anchors of %s are for %v, want %v", tenant, keys, want)
			}
		}
		if n := a.LateAnchors(); n != 0 {
			t.Errorf("%d late anchors, want 0", n)
		}
		if n := a.SkippedBoundaries(); n != 6 {
			t.Errorf("%d skipped boundaries, want 6: 14:00, 15:00 and 16:00 of each chain", n)
		}
	})

	t.Run("ST-SUP-RP05-LC09-T03 The anchor job does not run without the controller lease", func(t *testing.T) {
		f := newPG(t)
		f.recordN(f.writer(), "t1", 1)
		lease := f.lease()
		lease.lose()
		f.lease() // another holder, with a higher token
		a := f.anchorer(lease, time.Hour, "t1")
		for now := clock(10, 0); !now.After(clock(13, 0)); now = now.Add(30 * time.Minute) {
			if err := a.Tick(f.ctx, now); err != nil {
				t.Fatal(err)
			}
		}
		if got := f.anchorMessages("anchor/"); len(got) != 0 {
			t.Fatalf("%d anchor messages while another holder has the lease, want 0", len(got))
		}
		lease.take()
		if err := a.Tick(f.ctx, clock(14, 5)); err != nil {
			t.Fatal(err)
		}
		if got := f.anchorMessages("anchor/"); len(got) != 1 {
			t.Errorf("%d anchor messages after the acquire, want 1", len(got))
		}
	})

	t.Run("ST-SUP-RP05-LC09-T04 A missed anchor period is a health signal", func(t *testing.T) {
		f := newPG(t)
		f.recordN(f.writer(), "t1", 1)
		lease := f.lease()
		a := f.anchorer(lease, time.Hour, "t1")
		if err := a.Tick(f.ctx, clock(10, 30)); err != nil {
			t.Fatal(err)
		}
		f.outbox.setFail(true)
		if err := a.Tick(f.ctx, clock(11, 1)); err == nil {
			t.Errorf("Tick returns no error for a missed anchor")
		}
		healthy, reason := a.Health()
		if healthy || !strings.Contains(reason, "anchor missed") || !strings.Contains(reason, "t1") || !strings.Contains(reason, "11:00") {
			t.Errorf("health is %v with the reason %q, want unhealthy with a reason that names the missed anchor", healthy, reason)
		}
		f.outbox.setFail(false)
		if err := a.Tick(f.ctx, clock(11, 2)); err != nil {
			t.Fatal(err)
		}
		got := f.anchorMessages("anchor/")
		if len(got) != 2 || !strings.HasSuffix(got[1].key, "11:00:00.000000Z") {
			t.Errorf("anchor messages %+v, want 10:00 and then 11:00 anchored after the failure", got)
		}
		if healthy, _ := a.Health(); !healthy {
			t.Errorf("the job stays unhealthy after the boundary is anchored")
		}
		// A transaction that fails at its commit, after the anchor was written, is a miss too: the health clears
		// only once the commit succeeds.
		f.outbox.mu.Lock()
		f.outbox.onTx = func(ctx context.Context, tx Tx) error {
			_, err := tx.ExecContext(ctx, "INSERT INTO test_deferred VALUES (1), (1)")
			return err
		}
		f.outbox.mu.Unlock()
		if err := a.Tick(f.ctx, clock(12, 1)); err == nil {
			t.Errorf("Tick returns no error for a commit that fails")
		}
		if healthy, _ := a.Health(); healthy {
			t.Errorf("the job is healthy after the anchor transaction failed at its commit")
		}
		f.outbox.mu.Lock()
		f.outbox.onTx = nil
		f.outbox.mu.Unlock()
		// A miss of a process that then loses the lease is cleared: the new holder reports it.
		lease.lose()
		if err := a.Tick(f.ctx, clock(12, 2)); err != nil {
			t.Fatal(err)
		}
		if healthy, reason := a.Health(); !healthy {
			t.Errorf("a process without the lease reports %q", reason)
		}
	})

	t.Run("ST-SUP-RP05-LC09-T05 A lease that moves at each boundary still anchors every boundary", func(t *testing.T) {
		f := newPG(t)
		f.recordN(f.writer(), "t1", 1)
		la, lb := f.lease(), f.newLease()
		ra, rb := f.anchorer(la, time.Hour, "t1"), f.anchorer(lb, time.Hour, "t1")
		if err := ra.Tick(f.ctx, clock(10, 30)); err != nil {
			t.Fatal(err)
		}
		holders := []struct {
			from, to *testLease
			cur, nxt *Anchorer
		}{{la, lb, ra, rb}, {lb, la, rb, ra}, {la, lb, ra, rb}}
		for i, h := range holders {
			b := clock(11+i, 0)
			// The holder ticks just before the boundary, then loses the lease; the other replica takes it just
			// after the boundary and anchors at once.
			if err := h.cur.Tick(f.ctx, b.Add(-time.Minute)); err != nil {
				t.Fatal(err)
			}
			h.from.lose()
			if err := h.cur.Tick(f.ctx, b.Add(-30*time.Second)); err != nil {
				t.Fatal(err)
			}
			h.to.take()
			if err := h.nxt.Tick(f.ctx, b.Add(20*time.Second)); err != nil {
				t.Fatal(err)
			}
			// The new holder publishes before the next boundary is due: the anchor is not late, and no boundary
			// is passed over.
			if n := ra.LateAnchors() + rb.LateAnchors() + ra.SkippedBoundaries() + rb.SkippedBoundaries(); n != 0 {
				t.Errorf("after the boundary %s, %d boundaries are counted as late or skipped, want 0", b.Format("15:04"), n)
			}
		}
		got := f.anchorMessages("anchor/t1/")
		if len(got) != 4 {
			t.Fatalf("%d anchors, want one for 10:00 and one for each of the three boundaries: %+v", len(got), got)
		}
		for i, m := range got {
			if want := clock(10+i, 0).Format("2006-01-02T15:04:05.000000Z"); !strings.HasSuffix(m.key, want) {
				t.Errorf("anchor %d is for %s, want %s", i, m.key, want)
			}
		}
	})

	t.Run("ST-SUP-RP05-LC09-T06 A fenced holder's anchor commits nothing", func(t *testing.T) {
		f := newPG(t)
		f.recordN(f.writer(), "t1", 1)
		la := f.lease()
		a := f.anchorer(la, time.Hour, "t1")
		if err := a.Tick(f.ctx, clock(10, 30)); err != nil {
			t.Fatal(err)
		}
		readBoundary := func() time.Time {
			var b time.Time
			if err := f.inTx(f.owner, "t1", func(tx *sql.Tx) error {
				return tx.QueryRowContext(f.ctx, "SELECT boundary FROM "+BoundariesTable).Scan(&b)
			}); err != nil {
				t.Fatal(err)
			}
			return b
		}
		before, msgs := readBoundary(), len(f.anchorMessages("anchor/"))
		lb := f.newLease()
		f.outbox.onEnqueue = func() { lb.take() } // B takes the lease after A computed its anchor, before A commits
		err := a.Tick(f.ctx, clock(11, 1))
		if !errors.Is(err, errFenced) {
			t.Errorf("A's anchor gives %v, want the fenced error", err)
		}
		if got := len(f.anchorMessages("anchor/")); got != msgs {
			t.Errorf("%d anchor messages from the fenced holder, want 0", got-msgs)
		}
		if after := readBoundary(); !after.Equal(before) {
			t.Errorf("the stored boundary moved from %s to %s", before, after)
		}
	})

	t.Run("ST-SUP-RP05-LC09-T07 The anchor comes from the audit table, not from the chain-head row", func(t *testing.T) {
		f := newPG(t)
		rows := f.recordN(f.writer(), "t1", 5)
		if err := f.inTx(f.owner, "t1", func(tx *sql.Tx) error {
			_, err := tx.ExecContext(f.ctx, "UPDATE "+HeadsTable+" SET seq = 3, head = $1", rows[2].Hash)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		a := f.anchorer(f.lease(), time.Hour, "t1")
		if err := a.Tick(f.ctx, clock(10, 30)); err != nil {
			t.Fatal(err)
		}
		got := f.anchorMessages("anchor/")
		if len(got) != 1 || got[0].Count != "5" || got[0].Head != hex.EncodeToString(rows[4].Hash) {
			t.Errorf("anchors %+v, want the count 5 and the hash of row 5", got)
		}
	})

	t.Run("ST-SUP-RP05-LC09-T08 Boundaries passed over are counted as skipped", func(t *testing.T) {
		f := newPG(t)
		f.recordN(f.writer(), "t1", 2)
		if err := f.inTx(f.runtime, "t1", func(tx *sql.Tx) error {
			_, err := tx.ExecContext(f.ctx, "INSERT INTO "+BoundariesTable+" (tenant_id, boundary) VALUES ('t1', $1)", clock(10, 0))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		a := f.anchorer(f.lease(), time.Hour, "t1")
		if err := a.Tick(f.ctx, clock(14, 5)); err != nil {
			t.Fatal(err)
		}
		got := f.anchorMessages("anchor/t1/")
		if len(got) != 1 || !strings.HasSuffix(got[0].key, "T14:00:00.000000Z") {
			t.Errorf("anchors %+v, want one for 14:00", got)
		}
		if n := a.SkippedBoundaries(); n != 3 {
			t.Errorf("%d skipped boundaries, want 3 for 11:00, 12:00 and 13:00", n)
		}
		if n := a.LateAnchors(); n != 0 {
			t.Errorf("%d late anchors, want 0", n)
		}
		var b time.Time
		if err := f.inTx(f.owner, "t1", func(tx *sql.Tx) error {
			return tx.QueryRowContext(f.ctx, "SELECT boundary FROM "+BoundariesTable).Scan(&b)
		}); err != nil || !b.Equal(clock(14, 0)) {
			t.Errorf("the stored boundary is %s (%v), want 14:00", b, err)
		}
	})

	t.Run("ST-SUP-RP05-LC09-T09 A stored boundary only grows, is never far ahead, and a future one is a health signal", func(t *testing.T) {
		f := newPG(t)
		f.recordN(f.writer(), "t1", 1)
		write := func(q string) error {
			return f.inTx(f.runtime, "t1", func(tx *sql.Tx) error {
				_, err := tx.ExecContext(f.ctx, q)
				return err
			})
		}
		if err := write("INSERT INTO " + BoundariesTable + " (tenant_id, boundary) VALUES ('t1', now() + interval '2 hours')"); err == nil {
			t.Errorf("a boundary two periods ahead of now is stored")
		}
		if err := write("INSERT INTO " + BoundariesTable + " (tenant_id, boundary) VALUES ('t1', now())"); err != nil {
			t.Fatal(err)
		}
		if err := write("UPDATE " + BoundariesTable + " SET boundary = boundary - interval '1 hour'"); err == nil {
			t.Errorf("a stored boundary moves back")
		}
		// The job's clock is in March 2026, and the stored boundary is the database's now, far after it.
		a := f.anchorer(f.lease(), time.Hour, "t1")
		if err := a.Tick(f.ctx, clock(10, 30)); err != nil {
			t.Fatal(err)
		}
		if healthy, reason := a.Health(); healthy || !strings.Contains(reason, "more than one period after") {
			t.Errorf("health is %v with %q, want unhealthy for a stored boundary in the future", healthy, reason)
		}
		if got := f.anchorMessages("anchor/"); len(got) != 0 {
			t.Errorf("%d anchors, want none while the stored boundary is in the future", len(got))
		}
	})

	t.Run("ST-SUP-RP05-LC10-T10 A maintenance call takes the lease lock before it locks a partition", func(t *testing.T) {
		f := newPG(t)
		if _, err := f.owner.ExecContext(f.ctx, "DROP TABLE "+partitionName(monthStart(time.Now()).AddDate(0, 3, 0))); err != nil {
			t.Fatal(err)
		}
		m := f.maintainer(nil)
		// Another session holds the lease row, as an acquire that waits would.
		blocker, err := f.owner.BeginTx(f.ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback()
		if _, err := blocker.ExecContext(f.ctx, "SELECT 1 FROM leases WHERE name = $1 FOR UPDATE", leaseName); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		mark := f.trace.mark()
		_, err = m.Run(f.ctx)
		if sqlState(err) != "55P03" && !strings.Contains(fmt.Sprint(err), "lock timeout") {
			t.Errorf("Run gives %v, want a lock timeout on the lease row", err)
		}
		if d := time.Since(start); d > 4*time.Second {
			t.Errorf("Run waited %s for the lease row, want about 2 s", d)
		}
		if n := f.count("", "SELECT count(*) FROM pg_inherits WHERE inhparent = '"+EventsTable+"'::regclass"); n != 3 {
			t.Errorf("%d partitions, want 3: the call must not reach the function while the lease row is locked", n)
		}
		for _, tx := range f.trace.transactions(mark) {
			for _, q := range tx {
				if strings.Contains(q, MaintainFunction) {
					t.Errorf("the call ran audit_maintain() before it held the lease row: %s", q)
				}
			}
		}
	})

	t.Run("ST-SUP-RP05-LC10-T11 The DDL text runs again on a database that it made, in one transaction", func(t *testing.T) {
		f := newPG(t)
		ddl, err := DDL(Config{RuntimeRole: runtimeRole, AnchorPeriod: time.Hour, RetentionMonths: 12})
		if err != nil {
			t.Fatal(err)
		}
		shape := func() string {
			var text string
			if err := f.owner.QueryRowContext(f.ctx, `SELECT (SELECT count(*) FROM pg_inherits WHERE inhparent = '`+EventsTable+`'::regclass)::text || '/' ||
 (SELECT count(*) FROM pg_policies WHERE schemaname = 'public' AND policyname = 'tenant_wall')::text || '/' ||
 (SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal AND tgname LIKE 'audit%')::text`).Scan(&text); err != nil {
				t.Fatal(err)
			}
			return text
		}
		before := shape()
		for run := 1; run <= 2; run++ {
			tx, err := f.owner.BeginTx(f.ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(f.ctx, ddl); err != nil {
				_ = tx.Rollback()
				t.Fatalf("run %d of the DDL text on a database that it made: %v", run, err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
		if after := shape(); after != before {
			t.Errorf("partitions, policies and triggers are %s after two more runs, want %s as before", after, before)
		}
	})

	t.Run("ST-SUP-RP05-LC10-T01 The audit table is partitioned by month on recorded_at", func(t *testing.T) {
		f := newPG(t)
		var strategy, column string
		var defaults int64
		if err := f.owner.QueryRowContext(f.ctx, `SELECT p.partstrat::text, a.attname, p.partdefid::bigint FROM pg_partitioned_table p
 JOIN pg_attribute a ON a.attrelid = p.partrelid AND a.attnum = p.partattrs[0]
 WHERE p.partrelid = '`+EventsTable+`'::regclass`).Scan(&strategy, &column, &defaults); err != nil {
			t.Fatal(err)
		}
		if strategy != "r" || column != "recorded_at" || defaults != 0 {
			t.Errorf("the table is partitioned with strategy %q on %q and default partition %d, want range (r) on recorded_at and none", strategy, column, defaults)
		}
		rows, err := f.owner.QueryContext(f.ctx, `SELECT c.relname, pg_get_expr(c.relpartbound, c.oid) FROM pg_inherits i
 JOIN pg_class c ON c.oid = i.inhrelid WHERE i.inhparent = '`+EventsTable+`'::regclass`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		re := regexp.MustCompile(`^FOR VALUES FROM \('([^']+)'\) TO \('([^']+)'\)$`)
		n := 0
		for rows.Next() {
			var name, bound string
			if err := rows.Scan(&name, &bound); err != nil {
				t.Fatal(err)
			}
			n++
			m := re.FindStringSubmatch(bound)
			if m == nil {
				t.Errorf("the partition %s has the bound %q", name, bound)
				continue
			}
			from, err1 := time.Parse("2006-01-02 15:04:05-07", m[1])
			to, err2 := time.Parse("2006-01-02 15:04:05-07", m[2])
			if err1 != nil || err2 != nil || !from.Equal(monthStart(from)) || !to.Equal(from.AddDate(0, 1, 0)) {
				t.Errorf("the partition %s has the bound %q, want from the first day of a month to the first day of the next", name, bound)
			}
		}
		if n != 4 {
			t.Errorf("%d partitions, want the current month and the next three", n)
		}
	})

	t.Run("ST-SUP-RP05-LC10-T02 The reject trigger raises on UPDATE, DELETE and TRUNCATE", func(t *testing.T) {
		f := newPG(t)
		stored := f.recordN(f.writer(), "t1", 1)[0]
		part := partitionName(monthStart(stored.RecordedAt))
		for _, tbl := range []string{EventsTable, part} {
			for _, stmt := range []string{"UPDATE " + tbl + " SET action = 'changed'", "DELETE FROM " + tbl, "TRUNCATE " + tbl} {
				err := f.inTx(f.owner, "t1", func(tx *sql.Tx) error {
					_, err := tx.ExecContext(f.ctx, stmt)
					return err
				})
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || !strings.Contains(pgErr.Message, rejectMessage) {
					t.Errorf("as the owner, %s gives %v, want the trigger's error", stmt, err)
				}
			}
		}
		var action string
		if err := f.inTx(f.owner, "t1", func(tx *sql.Tx) error {
			return tx.QueryRowContext(f.ctx, "SELECT action FROM "+EventsTable).Scan(&action)
		}); err != nil || action != stored.Action {
			t.Errorf("the row holds the action %q (%v), want %q unchanged", action, err, stored.Action)
		}
	})

	t.Run("ST-SUP-RP05-LC10-T03 The runtime role holds only its declared privileges on the four audit tables", func(t *testing.T) {
		f := newPG(t)
		want := map[string][]string{
			EventsTable:      {"SELECT", "INSERT"},
			HeadsTable:       {"SELECT", "INSERT", "UPDATE"},
			BoundariesTable:  {"SELECT", "INSERT", "UPDATE"},
			CheckpointsTable: {"SELECT"},
		}
		for table, granted := range want {
			for _, priv := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"} {
				var got bool
				if err := f.owner.QueryRowContext(f.ctx, "SELECT has_table_privilege($1, $2, $3)", runtimeRole, table, priv).Scan(&got); err != nil {
					t.Fatal(err)
				}
				if exp := contains(granted, priv); got != exp {
					t.Errorf("the runtime role has %s on %s: %v, want %v", priv, table, got, exp)
				}
			}
		}
	})

	t.Run("ST-SUP-RP05-LC10-T04 The maintenance function creates the partitions of the next three months ahead of time", func(t *testing.T) {
		f := newPG(t, withClock("'2026-03-20 09:00:00+00'::timestamptz"))
		// newPG already called the function until no step was left; drop the partitions and count again.
		for _, m := range []string{"202603", "202604", "202605", "202606"} {
			if _, err := f.owner.ExecContext(f.ctx, "DROP TABLE "+partitionPrefix+m); err != nil {
				t.Fatal(err)
			}
		}
		if calls := f.maintainAll(); calls != 4 {
			t.Errorf("%d calls, want one partition per call and four partitions", calls)
		}
		for _, m := range []string{"202603", "202604", "202605", "202606"} {
			if n := f.count("", "SELECT count(*) FROM pg_class WHERE relname = $1", partitionPrefix+m); n != 1 {
				t.Errorf("no partition %s", m)
			}
		}
	})

	t.Run("ST-SUP-RP05-LC10-T05 The maintenance function drops the partitions older than the retention, one per call", func(t *testing.T) {
		f := newPG(t, withClock("'2026-03-20 09:00:00+00'::timestamptz"))
		current := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
		var times []time.Time
		for i := 14; i >= 1; i-- {
			m := current.AddDate(0, -i, 0)
			f.createMonth(m)
			times = append(times, m.Add(time.Hour))
		}
		seeded := f.seed("t1", times)
		var steps []string
		for calls := 0; calls < 10; calls++ {
			more, tenants := f.maintainOnce()
			steps = append(steps, strings.Join(tenants, ","))
			if !more {
				break
			}
		}
		if len(steps) != 2 {
			t.Errorf("the drops took %d calls (%v), want 2, one per partition", len(steps), steps)
		}
		for i := 1; i <= 14; i++ {
			name := partitionName(current.AddDate(0, -i, 0))
			n := f.count("", "SELECT count(*) FROM pg_class WHERE relname = $1", name)
			if i > 12 && n != 0 {
				t.Errorf("the partition %s, older than twelve months, is kept", name)
			}
			if i <= 12 && n != 1 {
				t.Errorf("the partition %s, inside twelve months, is dropped", name)
			}
		}
		var seqs []int64
		if err := f.inTx(f.owner, "t1", func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(f.ctx, "SELECT seq, hash FROM "+CheckpointsTable+" ORDER BY seq")
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var seq int64
				var hash []byte
				if err := rows.Scan(&seq, &hash); err != nil {
					return err
				}
				if string(hash) != string(seeded[seq-1].Hash) {
					t.Errorf("the checkpoint at seq %d has another hash than the row", seq)
				}
				seqs = append(seqs, seq)
			}
			return rows.Err()
		}); err != nil {
			t.Fatal(err)
		}
		if len(seqs) != 2 || seqs[0] != 1 || seqs[1] != 2 {
			t.Errorf("the checkpoints are at %v, want [1 2], one for each dropped partition", seqs)
		}
	})

	t.Run("ST-SUP-RP05-LC10-T06 The maintenance function deletes no row", func(t *testing.T) {
		f := newPG(t)
		old := monthStart(time.Now()).AddDate(0, -14, 0)
		f.createMonth(old)
		f.seed("t1", []time.Time{old.Add(time.Hour)})
		f.recordN(f.writer(), "t1", 3)
		countBy := func() map[string]int64 {
			out := map[string]int64{}
			if err := f.inTx(f.owner, "t1", func(tx *sql.Tx) error {
				rows, err := tx.QueryContext(f.ctx, "SELECT tableoid::regclass::text, count(*) FROM "+EventsTable+" GROUP BY 1")
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var name string
					var n int64
					if err := rows.Scan(&name, &n); err != nil {
						return err
					}
					out[name] = n
				}
				return rows.Err()
			}); err != nil {
				t.Fatal(err)
			}
			return out
		}
		before := countBy()
		if before[partitionName(old)] != 1 {
			t.Fatalf("the old partition holds %d rows, want 1", before[partitionName(old)])
		}
		// The statements inside the function are its source; the loop's own statements are recorded below.
		var logged []string
		var body string
		if err := f.owner.QueryRowContext(f.ctx, "SELECT prosrc FROM pg_proc WHERE proname = $1", MaintainFunction).Scan(&body); err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.ToUpper(body), "\n") {
			if regexp.MustCompile(`\bDELETE\b|\bTRUNCATE\b`).MatchString(line) {
				logged = append(logged, strings.TrimSpace(line))
			}
		}
		if len(logged) != 0 {
			t.Errorf("the maintenance function holds a DELETE or a TRUNCATE: %v", logged)
		}
		mark := f.trace.mark()
		if _, err := f.maintainer(nil).Run(f.ctx); err != nil {
			t.Fatal(err)
		}
		var stmts []string
		for _, tx := range f.trace.transactions(mark) {
			stmts = append(stmts, tx...)
		}
		if len(stmts) == 0 {
			t.Fatal("the statement recorder saw no statement of the maintenance loop")
		}
		for _, s := range stmts {
			if regexp.MustCompile(`(?i)\b(DELETE|TRUNCATE)\b`).MatchString(s) {
				t.Errorf("the maintenance loop sends %s", s)
			}
		}
		after := countBy()
		if _, ok := after[partitionName(old)]; ok || f.count("", "SELECT count(*) FROM pg_class WHERE relname = $1", partitionName(old)) != 0 {
			t.Errorf("the old partition is not dropped whole")
		}
		for name, n := range before {
			if name != partitionName(old) && after[name] != n {
				t.Errorf("the partition %s holds %d rows after the run, want %d", name, after[name], n)
			}
		}
	})

	t.Run("ST-SUP-RP05-LC10-T07 Row-level security keeps each tenant's audit rows apart", func(t *testing.T) {
		f := newPG(t)
		w := f.writer()
		f.recordN(w, "t1", 2)
		f.recordN(w, "t2", 2)
		for _, a := range []*Anchorer{f.anchorer(f.lease(), time.Hour, "t1", "t2")} {
			if err := a.Tick(f.ctx, clock(10, 0)); err != nil {
				t.Fatal(err)
			}
		}
		f.inTx(f.owner, "t1", func(tx *sql.Tx) error {
			_, err := tx.ExecContext(f.ctx, "INSERT INTO "+CheckpointsTable+" (tenant_id, seq, hash, partition_name) VALUES ('t1', 0, '\\x00', 'none')")
			return err
		})
		f.inTx(f.owner, "t2", func(tx *sql.Tx) error {
			_, err := tx.ExecContext(f.ctx, "INSERT INTO "+CheckpointsTable+" (tenant_id, seq, hash, partition_name) VALUES ('t2', 0, '\\x00', 'none')")
			return err
		})
		for _, table := range []string{EventsTable, HeadsTable, BoundariesTable, CheckpointsTable} {
			var mine, others, none int64
			if err := f.inTx(f.runtime, "t1", func(tx *sql.Tx) error {
				if err := tx.QueryRowContext(f.ctx, "SELECT count(*) FILTER (WHERE tenant_id = 't1'), count(*) FILTER (WHERE tenant_id <> 't1') FROM "+table).Scan(&mine, &others); err != nil {
					return err
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.inTx(f.runtime, "", func(tx *sql.Tx) error {
				return tx.QueryRowContext(f.ctx, "SELECT count(*) FROM "+table).Scan(&none)
			}); err != nil {
				t.Fatal(err)
			}
			if mine == 0 || others != 0 || none != 0 {
				t.Errorf("%s: a transaction of t1 sees %d own and %d other rows, one with no tenant %d; want own rows only, and none", table, mine, others, none)
			}
			var forced bool
			if err := f.owner.QueryRowContext(f.ctx, "SELECT relforcerowsecurity FROM pg_class WHERE relname = $1", table).Scan(&forced); err != nil || !forced {
				t.Errorf("%s: relforcerowsecurity is %v (%v), want true", table, forced, err)
			}
		}
	})

	t.Run("ST-SUP-RP05-LC10-T08 The maintenance function is an owner function that only the runtime role may call", func(t *testing.T) {
		f := newPG(t)
		var secdef bool
		var owner string
		var nargs int
		var config []byte
		if err := f.owner.QueryRowContext(f.ctx, `SELECT p.prosecdef, r.rolname, p.pronargs, array_to_string(p.proconfig, ';')
 FROM pg_proc p JOIN pg_roles r ON r.oid = p.proowner WHERE p.proname = $1`, MaintainFunction).Scan(&secdef, &owner, &nargs, &config); err != nil {
			t.Fatal(err)
		}
		if !secdef || owner != ownerRole || nargs != 0 || !strings.Contains(string(config), "search_path=pg_catalog, pg_temp") || !strings.Contains(string(config), "lock_timeout=2s") {
			t.Errorf("audit_maintain: SECURITY DEFINER %v, owner %s, %d arguments, config %q", secdef, owner, nargs, config)
		}
		for role, want := range map[string]bool{runtimeRole: true, otherRole: false, "public": false} {
			var got bool
			q := "SELECT has_function_privilege($1, 'audit_maintain()', 'EXECUTE')"
			if role == "public" {
				q = "SELECT coalesce(bool_or(a.grantee = 0), false) FROM pg_proc p, aclexplode(p.proacl) a WHERE p.proname = 'audit_maintain' AND $1 <> ''"
			}
			if err := f.owner.QueryRowContext(f.ctx, q, role).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("EXECUTE for %s is %v, want %v", role, got, want)
			}
		}
		other := f.open(otherRole, nil)
		_, err := other.ExecContext(f.ctx, "SELECT * FROM public.audit_maintain()")
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("another role calls audit_maintain with %v, want a permission error", err)
		}
		// A decoy schema first on the search_path does not change the table that the function acts on.
		if _, err := f.owner.ExecContext(f.ctx, `CREATE SCHEMA decoy; CREATE TABLE decoy.audit_event (x int);
 CREATE TABLE decoy.audit_retention_checkpoints (x int); GRANT USAGE ON SCHEMA decoy TO `+runtimeRole+`;
 DROP TABLE `+partitionName(monthStart(time.Now()).AddDate(0, 3, 0))); err != nil {
			t.Fatal(err)
		}
		if err := f.inTx(f.runtime, "", func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(f.ctx, "SET LOCAL search_path = decoy, public"); err != nil {
				return err
			}
			rows, err := tx.QueryContext(f.ctx, "SELECT step_left FROM audit_maintain()")
			if err != nil {
				return err
			}
			rows.Close()
			return rows.Err()
		}); err != nil {
			t.Fatalf("the call with the decoy search_path: %v", err)
		}
		if n := f.count("", "SELECT count(*) FROM pg_inherits WHERE inhparent = 'public.audit_event'::regclass"); n != 4 {
			t.Errorf("the real audit table has %d partitions after the call, want 4", n)
		}
		if n := f.count("", "SELECT count(*) FROM pg_inherits WHERE inhparent = 'decoy.audit_event'::regclass"); n != 0 {
			t.Errorf("the decoy table got %d partitions", n)
		}
	})

	t.Run("ST-SUP-RP05-LC10-T09 A missing partition for the next month is a health signal", func(t *testing.T) {
		f := newPG(t, withClock("'2026-03-20 09:00:00+00'::timestamptz"))
		if _, err := f.owner.ExecContext(f.ctx, "DROP TABLE "+partitionPrefix+"202604"); err != nil {
			t.Fatal(err)
		}
		m := f.maintainer(func() time.Time { return clock(9, 0) })
		if err := m.Check(f.ctx); err != nil {
			t.Fatal(err)
		}
		if ok, reason := m.Health(); ok || !strings.Contains(reason, "2026-04") {
			t.Errorf("health is %v with %q, want unhealthy with a reason that names 2026-04", ok, reason)
		}
		if _, err := m.Run(f.ctx); err != nil {
			t.Fatal(err)
		}
		if ok, reason := m.Health(); !ok {
			t.Errorf("health is unhealthy (%q) after a call of the maintenance function", reason)
		}
	})

	t.Run("ST-SUP-RP05-LC11-T01 A drop writes the checkpoint of each chain in its own transaction", func(t *testing.T) {
		f := newPG(t)
		old := monthStart(time.Now()).AddDate(0, -14, 0)
		f.createMonth(old)
		s1 := f.seed("t1", []time.Time{old.Add(time.Hour), old.Add(2 * time.Hour)})
		s2 := f.seed("t2", []time.Time{old.Add(time.Hour)})
		if _, err := f.maintainer(nil).Run(f.ctx); err != nil {
			t.Fatal(err)
		}
		if n := f.count("", "SELECT count(*) FROM pg_class WHERE relname = $1", partitionName(old)); n != 0 {
			t.Errorf("the partition is not dropped")
		}
		for tenant, last := range map[string]Event{"t1": s1[1], "t2": s2[0]} {
			var seq int64
			var hash []byte
			if err := f.inTx(f.owner, tenant, func(tx *sql.Tx) error {
				return tx.QueryRowContext(f.ctx, "SELECT seq, hash FROM "+CheckpointsTable).Scan(&seq, &hash)
			}); err != nil {
				t.Fatalf("the checkpoint of %s: %v", tenant, err)
			}
			want := int64(len(s1))
			if tenant == "t2" {
				want = 1
			}
			if seq != want || string(hash) != string(last.Hash) {
				t.Errorf("the checkpoint of %s is at seq %d, want %d with the hash of its last row", tenant, seq, want)
			}
			msgs := f.anchorMessages("retention/" + tenant + "/")
			if len(msgs) != 1 || msgs[0].AnchorKind != "retention" || msgs[0].rowTenant != tenant || msgs[0].Head != hex.EncodeToString(last.Hash) {
				t.Errorf("the retention anchors of %s are %+v, want one on an outbox row of %s", tenant, msgs, tenant)
			}
		}
	})

	t.Run("ST-SUP-RP05-LC11-T02 The retention checkpoint table refuses edits", func(t *testing.T) {
		f := newPG(t)
		if err := f.inTx(f.owner, "t1", func(tx *sql.Tx) error {
			_, err := tx.ExecContext(f.ctx, "INSERT INTO "+CheckpointsTable+" (tenant_id, seq, hash, partition_name) VALUES ('t1', 3, '\\x01', 'p')")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for _, stmt := range []string{"UPDATE " + CheckpointsTable + " SET seq = 9", "DELETE FROM " + CheckpointsTable, "TRUNCATE " + CheckpointsTable} {
			err := f.inTx(f.owner, "t1", func(tx *sql.Tx) error {
				_, err := tx.ExecContext(f.ctx, stmt)
				return err
			})
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || !strings.Contains(pgErr.Message, rejectMessage) {
				t.Errorf("as the owner, %s gives %v, want the trigger's error", stmt, err)
			}
		}
		if n := f.count("t1", "SELECT count(*) FROM "+CheckpointsTable+" WHERE seq = 3"); n != 1 {
			t.Errorf("the checkpoint changed")
		}
	})
}

// maintainOnce calls audit_maintain() once as the runtime role and returns whether a step is left and the tenants
// of the checkpoints it wrote.
func (f *pgFixture) maintainOnce() (bool, []string) {
	f.t.Helper()
	var more bool
	var tenants []string
	if err := f.inTx(f.runtime, "", func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(f.ctx, callMaintain)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var tenant sql.NullString
			var seq sql.NullInt64
			var hash []byte
			if err := rows.Scan(&more, &tenant, &seq, &hash); err != nil {
				return err
			}
			if tenant.Valid {
				tenants = append(tenants, tenant.String)
			}
		}
		return rows.Err()
	}); err != nil {
		f.t.Fatal(err)
	}
	return more, tenants
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
