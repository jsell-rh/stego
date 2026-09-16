package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"
)

var journalConflict = errors.New("test state conflict")
var journalLostResult = errors.New("test state result lost")

type journalFixture struct {
	mu     sync.Mutex
	record SealedStateRecord
	writes int
	fault  string
}

func (f *journalFixture) persistence(t *testing.T) StatePersistence {
	t.Helper()
	check := func(ctx context.Context) {
		t.Helper()
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 10*time.Second {
			t.Error("storage operation has no bounded deadline")
		}
	}
	return StatePersistence{
		Load: func(ctx context.Context) (SealedStateRecord, error) {
			check(ctx)
			f.mu.Lock()
			defer f.mu.Unlock()
			return SealedStateRecord{f.record.Version, bytes.Clone(f.record.Data)}, nil
		},
		Save: func(ctx context.Context, expected int64, data []byte) (SealedStateRecord, error) {
			check(ctx)
			f.mu.Lock()
			defer f.mu.Unlock()
			f.writes++
			if expected != f.record.Version {
				return SealedStateRecord{}, journalConflict
			}
			f.record = SealedStateRecord{expected + 1, bytes.Clone(data)}
			answer := SealedStateRecord{f.record.Version, bytes.Clone(data)}
			switch f.fault {
			case "lost result":
				return SealedStateRecord{}, journalLostResult
			case "version":
				answer.Version++
			case "mutated argument":
				data[0] ^= 1
				answer.Data = data
			}
			return answer, nil
		},
	}
}
func newJournalFixture(t *testing.T) (*StateJournal, *journalFixture) {
	t.Helper()
	p, err := NewStateProtector([][]byte{bytes.Repeat([]byte{1}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	f := &journalFixture{}
	j, err := NewStateJournal(p, StateKey{"instance", "Record", "one", "identity"}, f.persistence(t), 256)
	if err != nil {
		t.Fatal(err)
	}
	return j, f
}
func TestStateJournalRecoveryAndConcurrentSave(t *testing.T) {
	j, f := newJournalFixture(t)
	ctx := context.Background()
	initial, err := j.Load(ctx)
	if err != nil || initial.Version() != 0 || len(initial.Reveal()) != 0 {
		t.Fatal("absent state differs", err)
	}
	private := []byte("private provider migration checkpoint")
	saved, err := j.Save(ctx, initial, private)
	if err != nil || saved.Version() != 1 || !bytes.Equal(saved.Reveal(), private) {
		t.Fatal("save failed", err)
	}
	if bytes.Contains(f.record.Data, private) {
		t.Fatal("storage received plaintext")
	}
	copyValue := saved.Reveal()
	copyValue[0] ^= 1
	if !bytes.Equal(saved.Reveal(), private) {
		t.Fatal("snapshot exposed its byte storage")
	}
	// A restart has no cached record and must authenticate storage again.
	p, _ := NewStateProtector([][]byte{bytes.Repeat([]byte{1}, 32)})
	restarted, err := NewStateJournal(p, j.key, f.persistence(t), 256)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.Save(ctx, saved, nil); !errors.Is(err, ErrStateJournal) {
		t.Fatal("snapshot from another journal accepted")
	}
	restored, err := restarted.Load(ctx)
	if err != nil || restored.Version() != 1 || !bytes.Equal(restored.Reveal(), private) {
		t.Fatal("restart lost state", err)
	}
	results := make(chan error, 2)
	for range 2 {
		go func() { _, err := restarted.Save(ctx, restored, nil); results <- err }()
	}
	successes, conflicts := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			successes++
		} else if errors.Is(err, journalConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 || f.writes != 3 {
		t.Fatal("competing writes did not use one storage call each")
	}
	empty, err := restarted.Load(ctx)
	if err != nil || empty.Version() != 2 || len(empty.Reveal()) != 0 {
		t.Fatal("empty state lost version history", err)
	}
	for _, object := range []any{j, *j, saved, f.record} {
		for _, format := range []string{"%v", "%+v", "%#v", "%d", "%x", "%s", "%q"} {
			value := fmt.Sprintf(format, object)
			if len(value) > 64 || bytes.Contains([]byte(value), private) {
				t.Fatal("journal formatting disclosed private state")
			}
		}
		if _, err := json.Marshal(object); err == nil {
			t.Fatal("implicit journal export accepted")
		}
	}
}
func TestStateJournalUncertainWritesRequireRead(t *testing.T) {
	for _, fault := range []string{"lost result", "version", "mutated argument"} {
		t.Run(fault, func(t *testing.T) {
			j, f := newJournalFixture(t)
			ctx := context.Background()
			initial, err := j.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			f.fault = fault
			failed, err := j.Save(ctx, initial, []byte("saved before provider mutation"))
			if err == nil || failed.journal != nil || f.writes != 1 {
				t.Fatal("uncertain write returned usable state or was retried")
			}
			recovered, err := j.Load(ctx)
			if err != nil || recovered.Version() != 1 || string(recovered.Reveal()) != "saved before provider mutation" {
				t.Fatal("committed uncertain result did not recover", err)
			}
		})
	}
}
func TestStateJournalRejectsInvalidStorageAndBounds(t *testing.T) {
	ctx := context.Background()
	for _, fault := range []string{"negative", "zero with data", "empty versioned", "damaged", "wrong key", "wrong version", "oversize"} {
		t.Run(fault, func(t *testing.T) {
			j, f := newJournalFixture(t)
			sealed, _ := j.protector.Seal(j.key, 1, []byte("private"))
			f.record = SealedStateRecord{1, sealed}
			switch fault {
			case "negative":
				f.record.Version = -1
			case "zero with data":
				f.record.Version = 0
			case "empty versioned":
				f.record.Data = nil
			case "damaged":
				f.record.Data[len(sealed)-1] ^= 1
			case "wrong key":
				j.key.ResourceID = "other"
			case "wrong version":
				f.record.Version = 2
			case "oversize":
				f.record.Data = make([]byte, 257)
			}
			result, err := j.Load(ctx)
			if err == nil || result.journal != nil || len(result.Reveal()) != 0 || f.writes != 0 {
				t.Fatal("invalid storage produced usable state")
			}
		})
	}
	j, f := newJournalFixture(t)
	initial, err := j.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = j.Save(ctx, StateSnapshot{}, nil); err == nil {
		t.Fatal("unloaded snapshot accepted")
	}
	if _, err = j.Save(ctx, initial, make([]byte, 180)); err == nil {
		t.Fatal("oversize plaintext accepted")
	}
	overflow := StateSnapshot{journal: j, version: math.MaxInt64}
	if _, err = j.Save(ctx, overflow, nil); err == nil {
		t.Fatal("version overflow accepted")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = j.Load(canceled); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled load accepted")
	}
	if _, err = j.Save(canceled, initial, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled save accepted")
	}
	if f.writes != 0 {
		t.Fatal("invalid save reached storage")
	}
	if _, err = j.Save(ctx, initial, make([]byte, 179)); err != nil {
		t.Fatal("maximum record rejected", err)
	}
	if len(f.record.Data) != 256 {
		t.Fatal("configured envelope bound differs")
	}
	for _, limit := range []int{0, 76, MaxProtectedStateBytes + 1} {
		if _, err := NewStateJournal(j.protector, j.key, f.persistence(t), limit); err == nil {
			t.Fatal("invalid journal limit accepted")
		}
	}
	if _, err := NewStateJournal(nil, j.key, f.persistence(t), 256); err == nil {
		t.Fatal("missing key accepted")
	}
	if _, err := NewStateJournal(j.protector, StateKey{}, f.persistence(t), 256); err == nil {
		t.Fatal("missing resource binding accepted")
	}
	if _, err := NewStateJournal(j.protector, j.key, StatePersistence{}, 256); err == nil {
		t.Fatal("missing storage accepted")
	}
}
