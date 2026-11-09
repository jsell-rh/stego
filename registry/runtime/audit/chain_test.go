package audit

import (
	"bytes"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestChain(t *testing.T) {
	t.Run("ST-SUP-RP05-LC03-T02 The hash covers the redacted record, not the pii value", func(t *testing.T) {
		hashWith := func(email string) []byte {
			e := sampleEvent("alpha", "update")
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
			e := sampleEvent("alpha", "update")
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
		e := sampleEvent("alpha", "update")
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

	t.Run("ST-SUP-RP05-LC04-T04 prev_hash links each row to the previous row of the same service chain", func(t *testing.T) {
		f := newPG(t)
		w := f.writer(&standInOutbox{})
		var stored []Event
		for _, s := range []string{"alpha", "beta", "alpha", "beta", "alpha"} {
			stored = append(stored, f.recordN(w, s, 1)[0])
		}
		last := map[string][]byte{}
		for i, e := range stored {
			want, ok := last[e.Service]
			if !ok {
				want = ZeroHash()
			}
			if !bytes.Equal(e.PrevHash, want) {
				t.Errorf("row %d of %s has prev_hash %x, want %x", i+1, e.Service, e.PrevHash, want)
			}
			last[e.Service] = e.Hash
		}
		// The stored rows say the same.
		rows, err := f.other.Query(f.ctx, "SELECT service, prev_hash, hash FROM "+EventsTable+" ORDER BY recorded_at, seq")
		if err != nil {
			t.Fatal(err)
		}
		type r struct {
			service    string
			prev, hash []byte
		}
		got, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (r, error) {
			var x r
			err := row.Scan(&x.service, &x.prev, &x.hash)
			return x, err
		})
		if err != nil {
			t.Fatal(err)
		}
		last = map[string][]byte{}
		for i, x := range got {
			want, ok := last[x.service]
			if !ok {
				want = ZeroHash()
			}
			if !bytes.Equal(x.prev, want) {
				t.Errorf("stored row %d of %s has prev_hash %x, want %x", i+1, x.service, x.prev, want)
			}
			last[x.service] = x.hash
		}
	})

	t.Run("ST-SUP-RP05-LC04-T05 The first row of a chain has 32 zero bytes as prev_hash", func(t *testing.T) {
		f := newPG(t)
		w := f.writer(&standInOutbox{})
		f.recordN(w, "gamma", 1)
		var prev []byte
		if err := f.other.QueryRow(f.ctx, "SELECT prev_hash FROM "+EventsTable+" WHERE service = 'gamma'").Scan(&prev); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(prev, make([]byte, 32)) {
			t.Errorf("the first prev_hash is %x, want 32 zero bytes", prev)
		}
	})

	t.Run("ST-SUP-RP05-LC06-T01 The verifier accepts an intact chain", func(t *testing.T) {
		f := newPG(t)
		w := f.writer(&standInOutbox{})
		f.recordN(w, "alpha", 5)
		sink := f.publishAnchor()
		findings := f.verify("alpha", sink.latest("alpha"))
		if len(findings) != 0 {
			t.Errorf("findings on an intact chain: %v", findings)
		}
		if a := sink.latest("alpha"); a == nil || a.Count != 5 {
			t.Errorf("the anchor is %v, want the count 5", a)
		}
	})

	t.Run("ST-SUP-RP05-LC06-T02 The verifier names the first row whose hash differs", func(t *testing.T) {
		f := newPG(t)
		w := f.writer(&standInOutbox{})
		rows := f.recordN(w, "alpha", 5)
		sink := f.publishAnchor()
		f.tamper("UPDATE " + EventsTable + " SET action = 'tampered' WHERE service = 'alpha' AND seq = 3")
		findings := f.verify("alpha", sink.latest("alpha"))
		if len(findings) != 1 || findings[0].Kind != FindingHash || findings[0].Seq != 3 || findings[0].ID != rows[2].ID {
			t.Errorf("findings %v, want one hash finding on row 3 (%s)", findings, rows[2].ID)
		}
	})

	t.Run("ST-SUP-RP05-LC06-T03 The verifier names the first row whose prev_hash differs", func(t *testing.T) {
		f := newPG(t)
		w := f.writer(&standInOutbox{})
		rows := f.recordN(w, "alpha", 5)
		sink := f.publishAnchor()
		f.tamper("UPDATE " + EventsTable + " SET prev_hash = '\\x" + string(bytes.Repeat([]byte("ee"), 32)) + "' WHERE service = 'alpha' AND seq = 4")
		findings := f.verify("alpha", sink.latest("alpha"))
		if len(findings) == 0 || findings[0].Kind != FindingPrevHash || findings[0].Seq != 4 || findings[0].ID != rows[3].ID {
			t.Errorf("findings %v, want first a prev_hash finding on row 4 (%s)", findings, rows[3].ID)
		}
	})

	t.Run("ST-SUP-RP05-LC06-T04 The verifier reports a chain head that differs from the latest anchor", func(t *testing.T) {
		f := newPG(t)
		w := f.writer(&standInOutbox{})
		rows := f.recordN(w, "alpha", 5)
		sink := f.publishAnchor()
		// Replace the head row consistently: new content, its prev_hash kept as the hash of row 4, and the hash
		// recomputed, so the chain walk alone passes.
		head := rows[4]
		head.Action = "rewritten"
		head, err := Link(head, rows[3].Hash)
		if err != nil {
			t.Fatal(err)
		}
		f.tamperArgs("UPDATE "+EventsTable+" SET action = $1, prev_hash = $2, hash = $3 WHERE service = 'alpha' AND seq = 5",
			head.Action, head.PrevHash, head.Hash)
		if got := f.verify("alpha", nil); len(got) != 0 {
			t.Fatalf("the chain walk alone finds %v, want nothing", got)
		}
		findings := f.verify("alpha", sink.latest("alpha"))
		if len(findings) != 1 || findings[0].Kind != FindingHead {
			t.Errorf("findings %v, want one finding that the head differs from the anchor", findings)
		}
	})

	t.Run("ST-SUP-RP05-LC06-T05 The verifier reports truncation when the row count is lower than the anchor", func(t *testing.T) {
		f := newPG(t)
		w := f.writer(&standInOutbox{})
		f.recordN(w, "alpha", 5)
		sink := f.publishAnchor()
		f.tamper("DELETE FROM " + EventsTable + " WHERE service = 'alpha' AND seq > 3")
		findings := f.verify("alpha", sink.latest("alpha"))
		found := false
		for _, x := range findings {
			if x.Kind == FindingTruncated && x.Anchored == 5 && x.Found == 3 {
				found = true
			}
		}
		if !found {
			t.Errorf("findings %v, want a truncation finding with the anchored count 5 and the found count 3", findings)
		}
	})
}

func (f *pgFixture) verify(service string, latest *Anchor) []Finding {
	f.t.Helper()
	findings, err := Verify(f.ctx, f.other, service, latest)
	if err != nil {
		f.t.Fatalf("Verify: %v", err)
	}
	return findings
}

// publishAnchor runs one anchor period under the lease and delivers the anchor messages to a fake sink.
func (f *pgFixture) publishAnchor() *fakeSink {
	f.t.Helper()
	before := len(f.messages())
	a, err := NewAnchorer(AnchorConfig{Period: time.Hour, Lease: &fakeLease{held: true}, RunTx: f.runTx(nil),
		Outbox: &standInOutbox{}})
	if err != nil {
		f.t.Fatal(err)
	}
	t0 := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)
	for _, now := range []time.Time{t0, t0.Add(time.Hour)} {
		if err := a.Tick(f.ctx, now); err != nil {
			f.t.Fatal(err)
		}
	}
	sink := &fakeSink{}
	d, err := NewDestination(sink)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, m := range f.messages()[before:] {
		if err := d.Deliver(f.ctx, m.key, m.payload); err != nil {
			f.t.Fatal(err)
		}
	}
	return sink
}

// tamper runs a statement as the owner role with the reject triggers disabled, then enables them again.
func (f *pgFixture) tamper(sql string) {
	f.tamperArgs(sql)
}

func (f *pgFixture) tamperArgs(sql string, args ...any) {
	f.t.Helper()
	tables := []string{EventsTable}
	parts, err := partitionsOf(f)
	if err != nil {
		f.t.Fatal(err)
	}
	tables = append(tables, parts...)
	f.inOwnerTx(func(tx pgx.Tx) error {
		for _, tbl := range tables {
			if _, err := tx.Exec(f.ctx, "ALTER TABLE "+tbl+" DISABLE TRIGGER USER"); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(f.ctx, sql, args...); err != nil {
			return err
		}
		for _, tbl := range tables {
			if _, err := tx.Exec(f.ctx, "ALTER TABLE "+tbl+" ENABLE TRIGGER USER"); err != nil {
				return err
			}
		}
		return nil
	})
}

func partitionsOf(f *pgFixture) ([]string, error) {
	rows, err := f.owner.Query(f.ctx, selectPartitions)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
