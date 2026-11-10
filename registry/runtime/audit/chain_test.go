package audit

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

func TestChain(t *testing.T) {
	t.Run("ST-SUP-RP05-LC03-T02 The hash covers the redacted record, not the pii value", func(t *testing.T) {
		hashWith := func(email string) []byte {
			e := sampleEvent("t1", "update")
			e.ID = UUID{1}
			e.OccurredAt = time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)
			e.RecordedAt = e.OccurredAt
			if err := e.SetStates(Settings{Level: LevelChange}, Fields{"email": PII},
				map[string]any{"email": "old@example.com", "n": 1}, map[string]any{"email": email, "n": 2}); err != nil {
				t.Fatal(err)
			}
			e.PrevHash = ZeroHash()
			h, err := Hash(e)
			if err != nil {
				t.Fatal(err)
			}
			return h
		}
		if a, b := hashWith("one@example.com"), hashWith("two@example.com"); !bytes.Equal(a, b) {
			t.Errorf("the hashes differ: %x and %x", a, b)
		}
	})

	t.Run("ST-SUP-RP05-LC03-T05 A secret field does not change the hash", func(t *testing.T) {
		hashWith := func(secret string) []byte {
			e := sampleEvent("t1", "update")
			e.ID = UUID{2}
			e.OccurredAt = time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)
			e.RecordedAt = e.OccurredAt
			if err := e.SetStates(Settings{Level: LevelChange}, Fields{"api_key": Secret},
				map[string]any{"api_key": "sk-0", "n": 1}, map[string]any{"api_key": secret, "n": 2}); err != nil {
				t.Fatal(err)
			}
			e.PrevHash = ZeroHash()
			h, err := Hash(e)
			if err != nil {
				t.Fatal(err)
			}
			return h
		}
		if a, b := hashWith("sk-1111"), hashWith("sk-2222"); !bytes.Equal(a, b) {
			t.Errorf("the hashes differ: %x and %x", a, b)
		}
	})

	t.Run("ST-SUP-RP05-LC04-T03 The hash is SHA-256 over the canonical redacted record followed by prev_hash", func(t *testing.T) {
		e := sampleEvent("t1", "update")
		if err := e.SetStates(Settings{Level: LevelChange}, Fields{"email": PII},
			map[string]any{"email": "a@example.com"}, map[string]any{"email": "b@example.com"}); err != nil {
			t.Fatal(err)
		}
		e = linked(t, e)
		e.PrevHash = bytes.Repeat([]byte{0xab}, 32)
		got, err := Hash(e)
		if err != nil {
			t.Fatal(err)
		}
		rec, err := CanonicalRecord(e)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(append(append([]byte{}, rec...), e.PrevHash...))
		if !bytes.Equal(got, sum[:]) {
			t.Errorf("Hash gives %x, SHA-256 of the record and prev_hash is %x", got, sum)
		}
		if bytes.Contains(rec, []byte("a@example.com")) || bytes.Contains(rec, []byte("prev_hash")) {
			t.Errorf("the canonical record is not the redacted record without the hashes: %s", rec)
		}
	})

	t.Run("ST-SUP-RP05-LC04-T04 prev_hash links each row to the previous row of the same tenant's chain", func(t *testing.T) {
		f := newPG(t)
		w := f.writer()
		for _, tenant := range []string{"t1", "t2", "t1", "t2", "t1"} {
			f.recordN(w, tenant, 1)
		}
		for tenant, n := range map[string]int64{"t1": 3, "t2": 2} {
			if err := f.inTx(f.owner, tenant, func(tx *sql.Tx) error {
				rows, err := tx.QueryContext(f.ctx, "SELECT seq, tenant_id, prev_hash, hash FROM "+EventsTable+" ORDER BY seq")
				if err != nil {
					return err
				}
				defer rows.Close()
				prev := ZeroHash()
				want := int64(1)
				for rows.Next() {
					var seq int64
					var tn string
					var p, h []byte
					if err := rows.Scan(&seq, &tn, &p, &h); err != nil {
						return err
					}
					if tn != tenant || seq != want || !bytes.Equal(p, prev) {
						t.Errorf("row %d of %s: tenant %s, seq %d, prev_hash %x; want seq %d and prev_hash %x", want, tenant, tn, seq, p, want, prev)
					}
					prev, want = h, want+1
				}
				if want-1 != n {
					t.Errorf("the chain of %s has %d rows, want %d", tenant, want-1, n)
				}
				return rows.Err()
			}); err != nil {
				t.Fatal(err)
			}
		}
	})

	t.Run("ST-SUP-RP05-LC04-T05 The first row of a chain has 32 zero bytes as prev_hash", func(t *testing.T) {
		f := newPG(t)
		f.recordN(f.writer(), "t3", 1)
		var prev []byte
		if err := f.inTx(f.owner, "t3", func(tx *sql.Tx) error {
			return tx.QueryRowContext(f.ctx, "SELECT prev_hash FROM "+EventsTable).Scan(&prev)
		}); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(prev, make([]byte, 32)) {
			t.Errorf("the first prev_hash is %x, want 32 zero bytes", prev)
		}
	})

	t.Run("ST-SUP-RP05-LC01-T07 The operation is stored, sent and covered by the hash of version 2", func(t *testing.T) {
		f := newPG(t)
		// A row of hash_version 1, written before operation existed, still verifies with the old encoding.
		old := f.seedVersion1("t1")
		w := f.writer()
		tx := f.begin("t1")
		e := sampleEvent("t1", "update")
		e.Operation = "reserve-and-charge"
		e.HashVersion = HashVersion2
		stored, err := w.Record(f.ctx, tx, f.tn("t1"), []Event{e})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.commit(tx, "t1"); err != nil {
			t.Fatal(err)
		}
		if n := f.count("t1", "SELECT count(*) FROM "+EventsTable+" WHERE seq = 2 AND operation = 'reserve-and-charge' AND hash_version = 2"); n != 1 {
			t.Errorf("%d rows hold the operation reserve-and-charge with hash_version 2, want 1", n)
		}
		if n := f.count("t1", "SELECT count(*) FROM "+EventsTable+" WHERE seq = 1 AND operation = '' AND hash_version = 1 AND id = $1::uuid", old.ID.String()); n != 1 {
			t.Errorf("the row of version 1 is not stored as version 1")
		}
		sink := f.deliver(f.messages())
		var sent []Event
		for _, batch := range sink.appends {
			sent = append(sent, batch...)
		}
		if len(sent) != 1 || sent[0].ID != stored[0].ID || sent[0].Operation != "reserve-and-charge" || sent[0].HashVersion != 2 {
			t.Errorf("the sink got %+v, want the event with the operation reserve-and-charge and hash_version 2", sent)
		}
		if findings := f.verify("t1", nil); len(findings) != 0 {
			t.Fatalf("findings on a chain of a version 1 row and a version 2 row: %v", findings)
		}
		f.tamper("t1", "UPDATE "+EventsTable+" SET operation = 'refund' WHERE seq = 2")
		findings := f.verify("t1", nil)
		if len(findings) != 1 || findings[0].Kind != FindingHash || findings[0].Seq != 2 {
			t.Errorf("findings %v, want one hash_differs finding on row 2, whose operation changed", findings)
		}
	})

	t.Run("ST-SUP-RP05-LC06-T01 The verifier accepts an intact chain", func(t *testing.T) {
		f := newPG(t)
		f.recordN(f.writer(), "t1", 5)
		sink := f.publishAnchor("t1")
		if a := sink.of("t1"); len(a) != 1 || a[0].Count != 5 {
			t.Fatalf("the sink holds %v, want one anchor with the count 5", a)
		}
		if findings := f.verify("t1", sink.of("t1")); len(findings) != 0 {
			t.Errorf("findings on an intact chain: %v", findings)
		}
	})

	t.Run("ST-SUP-RP05-LC06-T02 The verifier names the first row whose hash differs", func(t *testing.T) {
		f := newPG(t)
		rows := f.recordN(f.writer(), "t1", 5)
		sink := f.publishAnchor("t1")
		f.tamper("t1", "UPDATE "+EventsTable+" SET action = 'tampered' WHERE seq = 3")
		findings := f.verify("t1", sink.of("t1"))
		if len(findings) != 1 || findings[0].Kind != FindingHash || findings[0].Seq != 3 || findings[0].ID != rows[2].ID {
			t.Errorf("findings %v, want one hash finding on row 3 (%s)", findings, rows[2].ID)
		}
	})

	t.Run("ST-SUP-RP05-LC06-T03 The verifier names the first row whose prev_hash differs", func(t *testing.T) {
		f := newPG(t)
		rows := f.recordN(f.writer(), "t1", 5)
		sink := f.publishAnchor("t1")
		f.tamper("t1", "UPDATE "+EventsTable+" SET prev_hash = '\\x"+strings.Repeat("ee", 32)+"' WHERE seq = 4")
		findings := f.verify("t1", sink.of("t1"))
		if len(findings) == 0 || findings[0].Kind != FindingPrevHash || findings[0].Seq != 4 || findings[0].ID != rows[3].ID {
			t.Errorf("findings %v, want first a prev_hash finding on row 4 (%s)", findings, rows[3].ID)
		}
	})

	t.Run("ST-SUP-RP05-LC06-T04 The verifier reports a chain head that differs from the latest anchor", func(t *testing.T) {
		f := newPG(t)
		rows := f.recordN(f.writer(), "t1", 5)
		sink := f.publishAnchor("t1")
		head := rows[4]
		head.Action = "rewritten"
		head, err := Link(head, rows[3].Hash)
		if err != nil {
			t.Fatal(err)
		}
		f.tamper("t1", "UPDATE "+EventsTable+" SET action = $1, prev_hash = $2, hash = $3 WHERE seq = 5",
			head.Action, head.PrevHash, head.Hash)
		if got := f.verify("t1", nil); len(got) != 0 {
			t.Fatalf("the chain walk alone finds %v, want nothing", got)
		}
		findings := f.verify("t1", sink.of("t1"))
		if len(findings) != 1 || findings[0].Kind != FindingHead || findings[0].Seq != 5 {
			t.Errorf("findings %v, want one finding that the head at seq 5 differs from the anchor", findings)
		}
	})

	t.Run("ST-SUP-RP05-LC06-T05 The verifier reports truncation when the row count is lower than the anchor", func(t *testing.T) {
		f := newPG(t)
		f.recordN(f.writer(), "t1", 5)
		sink := f.publishAnchor("t1")
		f.tamper("t1", "DELETE FROM "+EventsTable+" WHERE seq > 3")
		findings := f.verify("t1", sink.of("t1"))
		if !hasFinding(findings, Finding{Kind: FindingTruncated, Anchored: 5, Found: 3}) {
			t.Errorf("findings %v, want a truncation finding with the anchored count 5 and the highest seq 3", findings)
		}
	})

	t.Run("ST-SUP-RP05-LC06-T06 The verifier accepts a chain after a retention drop and still finds a deleted first row", func(t *testing.T) {
		f := newPG(t)
		old := monthStart(time.Now()).AddDate(0, -14, 0)
		f.createMonth(old)
		f.seed("t1", []time.Time{old.Add(time.Hour), old.Add(2 * time.Hour), old.Add(3 * time.Hour)})
		f.recordN(f.writer(), "t1", 2)
		sink := f.publishAnchor("t1")
		if a := sink.of("t1"); len(a) != 1 || a[0].Count != 5 {
			t.Fatalf("the sink holds %v, want one anchor with the count 5", a)
		}
		before := len(f.messages())
		if _, err := f.maintainer(nil).Run(f.ctx); err != nil {
			t.Fatal(err)
		}
		if n := f.count("", "SELECT count(*) FROM pg_class WHERE relname = $1", partitionName(old)); n != 0 {
			t.Fatalf("the old partition is not dropped")
		}
		anchors := append(sink.of("t1"), f.deliver(f.messages()[before:]).of("t1")...)
		if findings := f.verify("t1", anchors); len(findings) != 0 {
			t.Errorf("findings after the retention drop: %v", findings)
		}
		f.tamper("t1", "DELETE FROM "+EventsTable+" WHERE seq = 4")
		findings := f.verify("t1", anchors)
		if len(findings) != 1 || findings[0].Kind != FindingSeq || findings[0].Seq != 5 {
			t.Errorf("findings %v, want one finding that the first row after the checkpoint seq 3 has the seq 5", findings)
		}
	})

	t.Run("ST-SUP-RP05-LC06-T07 The verifier reports an anchor that is lower than an earlier one", func(t *testing.T) {
		f := newPG(t)
		rows := f.recordN(f.writer(), "t1", 5)
		sink := f.publishAnchor("t1")
		f.tamper("t1", "DELETE FROM "+EventsTable+" WHERE seq >= 4")
		anchors := append(sink.of("t1"), Anchor{Tenant: "t1", Kind: AnchorPeriodic, Head: rows[2].Hash, Count: 3})
		findings := f.verify("t1", anchors)
		if !hasFinding(findings, Finding{Kind: FindingForged, Anchor: 1, Anchored: 3}) {
			t.Errorf("findings %v, want a forged-anchor finding for the anchor with the count 3", findings)
		}
		if !hasFinding(findings, Finding{Kind: FindingTruncated, Anchored: 5, Found: 3}) {
			t.Errorf("findings %v, want a truncation finding against the count 5", findings)
		}
	})

	t.Run("ST-SUP-RP05-LC06-T08 The verifier keeps the chains of two tenants apart", func(t *testing.T) {
		f := newPG(t)
		w := f.writer()
		f.recordN(w, "t1", 3)
		f.recordN(w, "t2", 3)
		f.tamper("t2", "UPDATE "+EventsTable+" SET action = 'tampered' WHERE seq = 2")
		if got := f.verify("t1", nil); len(got) != 0 {
			t.Errorf("findings for t1: %v, want none", got)
		}
		if got := f.verify("t2", nil); len(got) != 1 || got[0].Seq != 2 {
			t.Errorf("findings for t2: %v, want one on seq 2", got)
		}
		// Each walk reads only its own tenant's rows: under row-level security a transaction of t1 sees no row
		// of t2, and Verify refuses a transaction of another tenant.
		if err := f.inTx(f.runtime, "t1", func(tx *sql.Tx) error {
			var n int64
			if err := tx.QueryRowContext(f.ctx, "SELECT count(*) FROM "+EventsTable+" WHERE tenant_id = 't2'").Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				t.Errorf("a transaction of t1 sees %d rows of t2", n)
			}
			if _, err := Verify(f.ctx, tx, f.tn("t2"), nil); err == nil {
				t.Errorf("Verify of t2 runs in a transaction of t1")
			}
			rec := &recordingTx{Tx: tx}
			if _, err := Verify(f.ctx, rec, tenant.Tenant{}, nil); !errors.Is(err, tenant.ErrZero) || len(rec.statements) != 0 {
				t.Errorf("Verify of the zero tenant gives %v after %d statements, want tenant.ErrZero before any", err, len(rec.statements))
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("ST-SUP-RP05-LC06-T12 A checkpoint that the sink does not hold is forged", forgedCheckpointTest)

	t.Run("ST-SUP-RP05-LC06-T09 The verifier reports a deleted first row", func(t *testing.T) {
		f := newPG(t)
		rows := f.recordN(f.writer(), "t1", 4)
		f.tamper("t1", "DELETE FROM "+EventsTable+" WHERE seq = 1")
		findings := f.verify("t1", nil)
		if len(findings) != 1 || findings[0].Kind != FindingSeq || findings[0].Seq != 2 || findings[0].ID != rows[1].ID {
			t.Errorf("findings %v, want one seq finding on row 2", findings)
		}
	})

	t.Run("ST-SUP-RP05-LC06-T10 The verifier reports a deleted middle row", func(t *testing.T) {
		f := newPG(t)
		f.recordN(f.writer(), "t1", 5)
		f.tamper("t1", "DELETE FROM "+EventsTable+" WHERE seq = 3")
		findings := f.verify("t1", nil)
		if len(findings) != 1 || findings[0].Kind != FindingSeq || findings[0].Seq != 4 {
			t.Errorf("findings %v, want one seq finding on row 4", findings)
		}
	})

	t.Run("ST-SUP-RP05-LC06-T11 The verifier reports two rows in the wrong order", func(t *testing.T) {
		f := newPG(t)
		f.recordN(f.writer(), "t1", 5)
		f.tamper("t1", "UPDATE "+EventsTable+" SET seq = CASE seq WHEN 2 THEN 3 ELSE 2 END WHERE seq IN (2, 3)")
		findings := f.verify("t1", nil)
		if len(findings) != 1 || findings[0].Kind != FindingPrevHash || findings[0].Seq != 2 {
			t.Errorf("findings %v, want one prev_hash finding on seq 2", findings)
		}
	})
}

func forgedCheckpointTest(t *testing.T) {
	t.Run("a checkpoint that the sink does not hold", func(t *testing.T) {
		f := newPG(t)
		w := f.writer()
		f.recordN(w, "t1", 3)
		early := f.publishAnchor("t1").of("t1") // count 3
		rows := f.recordN(w, "t1", 2)
		late := f.publishAnchorAt(time.Now().Add(time.Hour), "t1").of("t1") // count 5
		anchors := append(early, late...)
		if len(anchors) != 2 || anchors[0].Count != 3 || anchors[1].Count != 5 {
			t.Fatalf("the sink holds %v, want periodic anchors with the counts 3 and 5", anchors)
		}
		f.insertCheckpoint("t1", 5, rows[1].Hash)
		f.tamper("t1", "UPDATE "+EventsTable+" SET action = 'tampered' WHERE seq = 2")
		f.tamper("t1", "DELETE FROM "+EventsTable+" WHERE seq > 3")
		findings := f.verify("t1", anchors)
		if !hasFinding(findings, Finding{Kind: FindingCheckpoint, Anchored: 5}) {
			t.Errorf("findings %v, want a forged-checkpoint finding at seq 5", findings)
		}
		if !slices.ContainsFunc(findings, func(x Finding) bool { return x.Kind == FindingHash && x.Seq == 2 }) {
			t.Errorf("findings %v, want the walk from the zero hash to report the edited row 2", findings)
		}
		if !hasFinding(findings, Finding{Kind: FindingTruncated, Anchored: 5, Found: 3}) {
			t.Errorf("findings %v, want a truncation finding against the count 5", findings)
		}
	})
	t.Run("a checkpoint whose hash differs from the sink's retention anchor", func(t *testing.T) {
		f := newPG(t)
		rows := f.recordN(f.writer(), "t1", 3)
		f.insertCheckpoint("t1", 3, bytes.Repeat([]byte{9}, 32))
		anchors := []Anchor{{Tenant: "t1", Kind: AnchorRetention, Head: rows[2].Hash, Count: 3}}
		findings := f.verify("t1", anchors)
		if !hasFinding(findings, Finding{Kind: FindingCheckpoint, Anchored: 3}) {
			t.Errorf("findings %v, want a forged-checkpoint finding at seq 3", findings)
		}
	})
}

// insertCheckpoint writes a retention checkpoint as the owner, as a forger with the owner's rights would.
func (f *pgFixture) insertCheckpoint(id string, seq int64, hash []byte) {
	f.t.Helper()
	if err := f.inTx(f.owner, id, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(f.ctx, "INSERT INTO "+CheckpointsTable+" (tenant_id, seq, hash, partition_name) VALUES ($1, $2, $3, 'forged')", id, seq, hash)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
}

func hasKind(findings []Finding, k FindingKind) bool {
	for _, f := range findings {
		if f.Kind == k {
			return true
		}
	}
	return false
}

func hasFinding(findings []Finding, want Finding) bool {
	for _, f := range findings {
		if f.Kind == want.Kind && f.Anchored == want.Anchored && f.Found == want.Found && (want.Anchor == 0 || f.Anchor == want.Anchor) {
			return true
		}
	}
	return false
}

// verify runs Verify in a transaction of the tenant as the runtime role.
func (f *pgFixture) verify(tenant string, anchors []Anchor) []Finding {
	f.t.Helper()
	var findings []Finding
	if err := f.inTx(f.runtime, tenant, func(tx *sql.Tx) error {
		var err error
		findings, err = Verify(f.ctx, tx, f.tn(tenant), anchors)
		return err
	}); err != nil {
		f.t.Fatalf("Verify: %v", err)
	}
	return findings
}

// publishAnchor runs the anchor job once under the lease for the tenants, which anchors each chain at once, and
// delivers the new anchor messages to a fake sink.
func (f *pgFixture) publishAnchor(tenants ...string) *fakeSink {
	f.t.Helper()
	return f.publishAnchorAt(time.Now(), tenants...)
}

func (f *pgFixture) publishAnchorAt(now time.Time, tenants ...string) *fakeSink {
	f.t.Helper()
	before := len(f.messages())
	a := f.anchorer(f.lease(), time.Hour, tenants...)
	if err := a.Tick(f.ctx, now); err != nil {
		f.t.Fatal(err)
	}
	return f.deliver(f.messages()[before:])
}

func (f *pgFixture) deliver(msgs []outboxMessage) *fakeSink {
	f.t.Helper()
	sink := &fakeSink{}
	d, err := NewDestination(sink)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, m := range msgs {
		if err := d.Deliver(f.ctx, m.key, m.payload); err != nil {
			f.t.Fatal(err)
		}
	}
	return sink
}

// tamper runs a statement as the owner role in a transaction of the tenant, with the reject triggers disabled on
// the table and its partitions, then enables them again.
func (f *pgFixture) tamper(tenant, q string, args ...any) {
	f.t.Helper()
	tables := []string{EventsTable}
	rows, err := f.owner.QueryContext(f.ctx, `SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
 WHERE i.inhparent = '`+EventsTable+`'::regclass`)
	if err != nil {
		f.t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			f.t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	if err := f.inTx(f.owner, tenant, func(tx *sql.Tx) error {
		for _, tbl := range tables {
			if _, err := tx.ExecContext(f.ctx, "ALTER TABLE "+tbl+" DISABLE TRIGGER USER"); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(f.ctx, q, args...); err != nil {
			return err
		}
		for _, tbl := range tables {
			if _, err := tx.ExecContext(f.ctx, "ALTER TABLE "+tbl+" ENABLE TRIGGER USER"); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		f.t.Fatalf("tamper: %v", err)
	}
}

func TestOnBehalfOf(t *testing.T) {
	t.Run("ST-SUP-RP05-LC01-T08 The principal on whose behalf a worker acted is stored, sent and covered by the hash of version 3", func(t *testing.T) {
		f := newPG(t)
		f.seedVersion1("t1")
		w := f.writer()
		tx := f.begin("t1")
		v2 := sampleEvent("t1", "update")
		v2.Operation, v2.HashVersion = "reserve-and-charge", HashVersion2
		v3 := sampleEvent("t1", "update")
		v3.Operation, v3.OnBehalfOf = "apply-partner-replies", "user:u-7"
		stored, err := w.Record(f.ctx, tx, f.tn("t1"), []Event{v2, v3})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.commit(tx, "t1"); err != nil {
			t.Fatal(err)
		}
		if n := f.count("t1", "SELECT count(*) FROM "+EventsTable+" WHERE seq = 3 AND on_behalf_of = 'user:u-7' AND hash_version = 3"); n != 1 {
			t.Errorf("%d rows hold on_behalf_of user:u-7 with hash_version 3, want 1", n)
		}
		bad := sampleEvent("t1", "update")
		bad.HashVersion, bad.OnBehalfOf = HashVersion2, "user:u-7"
		if _, err := New(bad, time.Now()); err == nil {
			t.Error("a row of hash_version 2 with on_behalf_of was accepted")
		}
		sink := f.deliver(f.messages())
		var sent []Event
		for _, batch := range sink.appends {
			sent = append(sent, batch...)
		}
		if len(sent) != 2 || sent[1].ID != stored[1].ID || sent[1].OnBehalfOf != "user:u-7" || sent[1].HashVersion != 3 ||
			sent[0].HashVersion != 2 || sent[0].OnBehalfOf != "" {
			t.Errorf("the sink got %+v, want the version 2 row and the version 3 row with on_behalf_of", sent)
		}
		if findings := f.verify("t1", nil); len(findings) != 0 {
			t.Fatalf("findings on a chain of rows of versions 1, 2 and 3: %v", findings)
		}
		f.tamper("t1", "UPDATE "+EventsTable+" SET on_behalf_of = 'user:u-8' WHERE seq = 3")
		findings := f.verify("t1", nil)
		if len(findings) != 1 || findings[0].Kind != FindingHash || findings[0].Seq != 3 {
			t.Errorf("findings %v, want one hash_differs finding on row 3, whose on_behalf_of changed", findings)
		}
	})
}

// seedVersion1 writes the first row of the tenant's chain as a row of hash_version 1, with no operation, as the
// runtime wrote rows before operation existed, and moves the chain head.
func (f *pgFixture) seedVersion1(tenant string) Event {
	f.t.Helper()
	var out Event
	err := f.inTx(f.owner, tenant, func(tx *sql.Tx) error {
		now := time.Now()
		e := sampleEvent(tenant, "seeded-v1")
		e.HashVersion = HashVersion1
		e, err := New(e, now)
		if err != nil {
			return err
		}
		e.RecordedAt = storedTime(now)
		if e, err = Link(e, ZeroHash()); err != nil {
			return err
		}
		// The hash of version 1 is made here, apart from the package's own switch of versions: the canonical record
		// without operation and hash_version, then prev_hash, as the runtime wrote rows before operation existed.
		v2 := e
		v2.HashVersion = HashVersion2
		rec, err := CanonicalRecord(v2)
		if err != nil {
			return err
		}
		var tree map[string]any
		d := json.NewDecoder(bytes.NewReader(rec))
		d.UseNumber()
		if err := d.Decode(&tree); err != nil {
			return err
		}
		delete(tree, "operation")
		delete(tree, "hash_version")
		old, err := Canonical(tree)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(append(old, e.PrevHash...))
		e.Hash = sum[:]
		if _, err := tx.ExecContext(f.ctx, insertEvent, tenant, 1, e.ID.String(), e.OccurredAt, e.RecordedAt,
			e.ActorID, string(e.ActorKind), string(e.ActorIP), string(e.UserAgent), e.Action, e.ResourceType,
			e.ResourceID, string(e.Outcome), e.ErrorCode, e.RequestID, e.TraceID, e.SpanID, e.IdempotencyKey,
			nil, nil, nil, e.Service, e.ServiceVersion, e.DeclarationVersion, e.PrevHash, e.Hash, e.Operation, e.HashVersion, e.OnBehalfOf); err != nil {
			return err
		}
		out = e
		_, err = tx.ExecContext(f.ctx, `INSERT INTO `+HeadsTable+` (tenant_id, seq, head, last_recorded_at) VALUES ($1, 1, $2, $3)`,
			tenant, e.Hash, e.RecordedAt)
		return err
	})
	if err != nil {
		f.t.Fatalf("seed a version 1 row: %v", err)
	}
	return out
}
