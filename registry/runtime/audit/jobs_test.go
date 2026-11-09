package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeLease stands in for the controller lease of the lease runtime until that runtime is on main.
type fakeLease struct {
	mu   sync.Mutex
	held bool
}

func (l *fakeLease) Held() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held
}

func (l *fakeLease) set(v bool) {
	l.mu.Lock()
	l.held = v
	l.mu.Unlock()
}

type anchorPayload struct {
	Kind    string `json:"kind"`
	Service string `json:"service"`
	Head    string `json:"head"`
	Count   string `json:"count"`
}

func (f *pgFixture) anchorMessages() []anchorPayload {
	f.t.Helper()
	var out []anchorPayload
	for _, m := range f.messages() {
		if !strings.HasPrefix(m.key, "anchor/") {
			continue
		}
		if m.destination != SinkDestination {
			f.t.Errorf("an anchor message goes to %q, want %q", m.destination, SinkDestination)
		}
		var p anchorPayload
		if err := json.Unmarshal(m.payload, &p); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func (f *pgFixture) anchorer(lease Lease, outbox Enqueuer, period time.Duration) *Anchorer {
	f.t.Helper()
	a, err := NewAnchorer(AnchorConfig{Period: period, Lease: lease, RunTx: f.runTx(nil), Outbox: outbox})
	if err != nil {
		f.t.Fatal(err)
	}
	return a
}

func TestJobs(t *testing.T) {
	t0 := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)

	t.Run("ST-SUP-RP05-LC09-T01 The anchor job publishes the head hash and the row count of each service chain", func(t *testing.T) {
		f := newPG(t)
		w := f.writer(&standInOutbox{})
		alpha := f.recordN(w, "alpha", 3)
		beta := f.recordN(w, "beta", 2)
		a := f.anchorer(&fakeLease{held: true}, &standInOutbox{}, time.Hour)
		for _, now := range []time.Time{t0, t0.Add(time.Hour)} {
			if err := a.Tick(f.ctx, now); err != nil {
				t.Fatal(err)
			}
		}
		got := f.anchorMessages()
		want := []anchorPayload{
			{Kind: "anchor", Service: "alpha", Head: hexOf(alpha[2].Hash), Count: "3"},
			{Kind: "anchor", Service: "beta", Head: hexOf(beta[1].Hash), Count: "2"},
		}
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("anchor messages %+v, want %+v", got, want)
		}
	})

	t.Run("ST-SUP-RP05-LC09-T02 The anchor job runs once per configured period", func(t *testing.T) {
		f := newPG(t)
		f.recordN(f.writer(&standInOutbox{}), "alpha", 1)
		a := f.anchorer(&fakeLease{held: true}, &standInOutbox{}, time.Hour)
		for now := t0; !now.After(t0.Add(3 * time.Hour)); now = now.Add(10 * time.Minute) {
			if err := a.Tick(f.ctx, now); err != nil {
				t.Fatal(err)
			}
		}
		if got := f.anchorMessages(); len(got) != 3 {
			t.Errorf("%d anchor messages over three hours, want 3: %+v", len(got), got)
		}
	})

	t.Run("ST-SUP-RP05-LC09-T03 The anchor job does not run without the controller lease", func(t *testing.T) {
		f := newPG(t)
		f.recordN(f.writer(&standInOutbox{}), "alpha", 1)
		lease := &fakeLease{held: false}
		a := f.anchorer(lease, &standInOutbox{}, time.Hour)
		for now := t0; !now.After(t0.Add(3 * time.Hour)); now = now.Add(30 * time.Minute) {
			if err := a.Tick(f.ctx, now); err != nil {
				t.Fatal(err)
			}
		}
		if got := f.anchorMessages(); len(got) != 0 {
			t.Fatalf("%d anchor messages while another holder has the lease, want 0", len(got))
		}
		lease.set(true)
		t1 := t0.Add(4 * time.Hour)
		for _, now := range []time.Time{t1, t1.Add(time.Hour)} {
			if err := a.Tick(f.ctx, now); err != nil {
				t.Fatal(err)
			}
		}
		if got := f.anchorMessages(); len(got) != 1 {
			t.Errorf("%d anchor messages after the acquire, want 1", len(got))
		}
	})

	t.Run("ST-SUP-RP05-LC09-T04 A missed anchor period is a health signal", func(t *testing.T) {
		f := newPG(t)
		f.recordN(f.writer(&standInOutbox{}), "alpha", 1)
		outbox := &standInOutbox{}
		a := f.anchorer(&fakeLease{held: true}, outbox, time.Hour)
		if err := a.Tick(f.ctx, t0); err != nil {
			t.Fatal(err)
		}
		outbox.setFail(true)
		if err := a.Tick(f.ctx, t0.Add(time.Hour)); err == nil {
			t.Errorf("Tick returns no error for a missed anchor")
		}
		healthy, reason := a.Health()
		if healthy || !strings.Contains(reason, "anchor missed") {
			t.Errorf("health is %v with the reason %q, want unhealthy with a reason that names the missed anchor", healthy, reason)
		}
		outbox.setFail(false)
		if err := a.Tick(f.ctx, t0.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		// Two periods ended: one has a message and one has a signal; none is passed over.
		if got := f.anchorMessages(); len(got) != 1 {
			t.Errorf("%d anchor messages, want 1 for the period after the missed one", len(got))
		}
		if healthy, _ := a.Health(); !healthy {
			t.Errorf("the job stays unhealthy after an anchor is published again")
		}
	})

	t.Run("ST-SUP-RP05-LC10-T01 The audit table is partitioned by month on recorded_at", func(t *testing.T) {
		f := newPG(t)
		var strategy, column string
		if err := f.owner.QueryRow(f.ctx, `SELECT p.partstrat::text, a.attname FROM pg_partitioned_table p
 JOIN pg_attribute a ON a.attrelid = p.partrelid AND a.attnum = p.partattrs[0]
 WHERE p.partrelid = '`+EventsTable+`'::regclass`).Scan(&strategy, &column); err != nil {
			t.Fatal(err)
		}
		if strategy != "r" || column != "recorded_at" {
			t.Errorf("the table is partitioned with strategy %q on %q, want range (r) on recorded_at", strategy, column)
		}
		rows, err := f.owner.Query(f.ctx, `SELECT c.relname, pg_get_expr(c.relpartbound, c.oid) FROM pg_inherits i
 JOIN pg_class c ON c.oid = i.inhrelid WHERE i.inhparent = '`+EventsTable+`'::regclass`)
		if err != nil {
			t.Fatal(err)
		}
		type part struct{ name, bound string }
		parts, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (part, error) {
			var p part
			err := r.Scan(&p.name, &p.bound)
			return p, err
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(parts) < 2 {
			t.Fatalf("%d partitions, want at least the current and the next month", len(parts))
		}
		re := regexp.MustCompile(`^FOR VALUES FROM \('([^']+)'\) TO \('([^']+)'\)$`)
		for _, p := range parts {
			m := re.FindStringSubmatch(p.bound)
			if m == nil {
				t.Errorf("the partition %s has the bound %q", p.name, p.bound)
				continue
			}
			from, err1 := time.Parse("2006-01-02 15:04:05-07", m[1])
			to, err2 := time.Parse("2006-01-02 15:04:05-07", m[2])
			if err1 != nil || err2 != nil {
				t.Errorf("the partition %s has the bound %q: %v %v", p.name, p.bound, err1, err2)
				continue
			}
			if !from.Equal(monthStart(from)) || !to.Equal(from.AddDate(0, 1, 0)) {
				t.Errorf("the partition %s goes from %s to %s, want from the first day of a month to the first day of the next", p.name, from, to)
			}
		}
	})

	t.Run("ST-SUP-RP05-LC10-T02 The reject trigger raises on UPDATE, DELETE and TRUNCATE", func(t *testing.T) {
		f := newPG(t)
		stored := f.recordN(f.writer(&standInOutbox{}), "alpha", 1)[0]
		part := partitionName(monthStart(stored.RecordedAt))
		for _, tbl := range []string{EventsTable, part} {
			for _, stmt := range []string{
				"UPDATE " + tbl + " SET action = 'changed'",
				"DELETE FROM " + tbl,
				"TRUNCATE " + tbl,
			} {
				err := pgx.BeginFunc(f.ctx, f.owner, func(tx pgx.Tx) error {
					_, err := tx.Exec(f.ctx, stmt)
					return err
				})
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || !strings.Contains(pgErr.Message, rejectMessage) {
					t.Errorf("as the owner, %s gives %v, want the trigger's error", stmt, err)
				}
			}
		}
		var action string
		if err := f.owner.QueryRow(f.ctx, "SELECT action FROM "+EventsTable).Scan(&action); err != nil || action != stored.Action {
			t.Errorf("the row now holds the action %q (%v), want %q unchanged", action, err, stored.Action)
		}
	})

	t.Run("ST-SUP-RP05-LC10-T03 The runtime role can select from and insert into the audit table and cannot update, delete or truncate it", func(t *testing.T) {
		f := newPG(t)
		for priv, want := range map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": false, "DELETE": false, "TRUNCATE": false} {
			var got bool
			if err := f.owner.QueryRow(f.ctx, "SELECT has_table_privilege($1, $2, $3)", runtimeRole, EventsTable, priv).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("the runtime role has %s: %v, want %v", priv, got, want)
			}
		}
	})

	t.Run("ST-SUP-RP05-LC10-T04 The maintenance job creates the next month's partition ahead of time", func(t *testing.T) {
		f := newPG(t)
		march20 := time.Date(2026, 3, 20, 9, 0, 0, 0, time.UTC)
		f.inOwnerTx(func(tx pgx.Tx) error { return Maintain(f.ctx, tx, march20, testRetention) })
		if n := f.count("SELECT count(*) FROM pg_class WHERE relname = $1", partitionName(time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC))); n != 1 {
			t.Errorf("no partition for April 2026 after a run on 20 March")
		}
	})

	t.Run("ST-SUP-RP05-LC10-T05 The maintenance job drops the partitions older than the retention", func(t *testing.T) {
		f := newPG(t)
		march20 := time.Date(2026, 3, 20, 9, 0, 0, 0, time.UTC)
		current := monthStart(march20)
		f.inOwnerTx(func(tx pgx.Tx) error {
			for i := 1; i <= 14; i++ {
				if err := createPartition(f.ctx, tx, current.AddDate(0, -i, 0)); err != nil {
					return err
				}
			}
			return nil
		})
		f.inOwnerTx(func(tx pgx.Tx) error { return Maintain(f.ctx, tx, march20, 12) })
		for i := 1; i <= 14; i++ {
			name := partitionName(current.AddDate(0, -i, 0))
			n := f.count("SELECT count(*) FROM pg_class WHERE relname = $1", name)
			if i > 12 && n != 0 {
				t.Errorf("the partition %s, older than twelve months, is kept", name)
			}
			if i <= 12 && n != 1 {
				t.Errorf("the partition %s, inside twelve months, is dropped", name)
			}
		}
	})

	t.Run("ST-SUP-RP05-LC10-T06 The maintenance job deletes no row", func(t *testing.T) {
		f := newPG(t)
		f.recordN(f.writer(&standInOutbox{}), "alpha", 3)
		countBy := func() map[string]int64 {
			rows, err := f.owner.Query(f.ctx, "SELECT tableoid::regclass::text, count(*) FROM "+EventsTable+" GROUP BY 1")
			if err != nil {
				t.Fatal(err)
			}
			out := map[string]int64{}
			for rows.Next() {
				var name string
				var n int64
				if err := rows.Scan(&name, &n); err != nil {
					t.Fatal(err)
				}
				out[name] = n
			}
			rows.Close()
			return out
		}
		before := countBy()
		var stmts []string
		f.inOwnerTx(func(tx pgx.Tx) error {
			rec := &recordingTx{Tx: tx}
			err := Maintain(f.ctx, rec, time.Now(), testRetention)
			stmts = rec.statements
			return err
		})
		for _, s := range stmts {
			if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(s)), "DELETE") {
				t.Errorf("the maintenance job sends a DELETE: %s", s)
			}
		}
		after := countBy()
		for name, n := range before {
			if after[name] != n {
				t.Errorf("the partition %s holds %d rows after the run, want %d", name, after[name], n)
			}
		}
	})
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	var out bytes.Buffer
	for _, c := range b {
		out.WriteByte(digits[c>>4])
		out.WriteByte(digits[c&0xf])
	}
	return out.String()
}
