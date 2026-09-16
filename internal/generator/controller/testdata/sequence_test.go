package controller

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func sequenceFixture(name string, values ...string) NamedCursorSource[string] {
	return NamedCursorSource[string]{Name: name, Read: func(ctx context.Context, after string, limit int) (CursorPage[string], error) {
		page := CursorPage[string]{}
		for _, value := range values {
			if value <= after {
				continue
			}
			if len(page.Items) == limit {
				page.More = true
				break
			}
			page.Items = append(page.Items, CursorItem[string]{Cursor: value, Value: name + ":" + value})
		}
		return page, nil
	}}
}

func TestSequenceScanCrossesSourcesAndResumes(t *testing.T) {
	sources := []NamedCursorSource[string]{sequenceFixture("rows", "a", "b", "c"), sequenceFixture("empty"), sequenceFixture("journals", "a", "d")}
	source, err := SequenceCursorSources(sources)
	if err != nil {
		t.Fatal(err)
	}
	first, err := source(context.Background(), "", 4)
	if err != nil || !first.More || len(first.Items) != 4 || first.Items[3].Value != "journals:a" {
		t.Fatal("source boundary differs", first, err)
	}
	// Reconstruct the sequence after its first journal item. The prior row source
	// must not be queried again, and the same raw key in two sources stays distinct.
	sources[0].Read = func(context.Context, string, int) (CursorPage[string], error) {
		t.Fatal("repeated completed source")
		return CursorPage[string]{}, nil
	}
	resumed, err := SequenceCursorSources(sources)
	if err != nil {
		t.Fatal(err)
	}
	second, err := resumed(context.Background(), first.Items[3].Cursor, 4)
	if err != nil || second.More || len(second.Items) != 1 || second.Items[0].Value != "journals:d" {
		t.Fatal("resumed sequence differs", second, err)
	}
	// The constructor owns its configuration copy.
	original, err := source(context.Background(), "", 1)
	if err != nil || len(original.Items) != 1 || original.Items[0].Value != "rows:a" {
		t.Fatal("caller changed source configuration", err)
	}
	if first.Items[0].Cursor == first.Items[3].Cursor {
		t.Fatal("source cursors collide")
	}
}

func TestSequenceScanCycleKeepsFailureAcrossRestart(t *testing.T) {
	sources := []NamedCursorSource[string]{sequenceFixture("rows", "a", "b"), sequenceFixture("journals", "a", "b")}
	var saved Checkpoint
	access := CheckpointAccess{Load: func(context.Context) (Checkpoint, error) { return saved, nil }, Save: func(_ context.Context, version int64, after string) error {
		if saved.Version != version {
			return errors.New("checkpoint conflict")
		}
		saved = Checkpoint{Version: version + 1, After: after}
		return nil
	}}
	var reached []string
	fault := errors.New("provider failed")
	run := func(fail bool) (CycleState, error) {
		source, err := SequenceCursorSources(sources)
		if err != nil {
			t.Fatal(err)
		}
		return ScanCycle(context.Background(), "input-1", access, source, func(_ context.Context, value string) error {
			reached = append(reached, value)
			if fail && value == "rows:a" {
				return fault
			}
			return nil
		}, func(error) bool { return true }, ScanOptions{PageSize: 2, MaxPages: 1, PageTimeout: time.Second}, ObservationOptions{WorkTimeout: time.Second, CommitTimeout: time.Second})
	}
	first, err := run(true)
	if !errors.Is(err, ErrCycleFailed) || first.Complete || !first.Failed {
		t.Fatal("failure was not retained", first, err)
	}
	second, err := run(false)
	if !errors.Is(err, ErrCycleFailed) || !second.Complete || !second.Failed {
		t.Fatal("restart lost prior failure", second, err)
	}
	if !reflect.DeepEqual(reached, []string{"rows:a", "rows:b", "journals:a", "journals:b"}) {
		t.Fatal("later work did not progress", reached)
	}
	reached = nil
	third, err := run(false)
	if err != nil || third.Complete || third.Failed {
		t.Fatal("new cycle did not reset", third, err)
	}
	fourth, err := run(false)
	if err != nil || !fourth.Complete || fourth.Failed {
		t.Fatal("successful sequence did not complete", fourth, err)
	}
}

func TestSequenceScanRejectsInvalidPagesBeforeEffects(t *testing.T) {
	for _, bad := range []CursorPage[string]{
		{More: true}, {Items: []CursorItem[string]{{Cursor: "x"}, {Cursor: "x"}}},
		{Items: []CursorItem[string]{{Cursor: strings.Repeat("x", 321)}}},
		{Items: []CursorItem[string]{{Cursor: "bad\x00key"}}},
	} {
		source, err := SequenceCursorSources([]NamedCursorSource[string]{sequenceFixture("valid", "a"), {Name: "bad", Read: func(context.Context, string, int) (CursorPage[string], error) { return bad, nil }}})
		if err != nil {
			t.Fatal(err)
		}
		effects := 0
		_, err = ScanFrom(context.Background(), "", source, func(string) error { effects++; return nil }, ScanOptions{PageSize: 4, MaxPages: 1, PageTimeout: time.Second})
		if !errors.Is(err, ErrScanContract) || effects != 0 {
			t.Fatal("invalid later source permitted effects", effects, err)
		}
	}
}

func TestSequenceScanRejectsInvalidConfigurationAndCursors(t *testing.T) {
	for _, sources := range [][]NamedCursorSource[string]{nil, {sequenceFixture("same"), sequenceFixture("same")}, {sequenceFixture("bad.name")}, {{Name: "missing"}}, make([]NamedCursorSource[string], 17)} {
		if _, err := SequenceCursorSources(sources); !errors.Is(err, ErrScanContract) {
			t.Fatal("invalid sources accepted", err)
		}
	}
	calls := 0
	source, err := SequenceCursorSources([]NamedCursorSource[string]{{Name: "one", Read: func(context.Context, string, int) (CursorPage[string], error) {
		calls++
		return CursorPage[string]{}, nil
	}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, after := range []string{"1.one.", "1.unknown.YQ", "2.one.YQ", "1.one.YQ==", "1.one.YR", "1.one.AA", "1.one._w", strings.Repeat("x", 513)} {
		if _, err := source(context.Background(), after, 1); !errors.Is(err, ErrScanContract) {
			t.Fatal("invalid cursor accepted", err)
		}
	}
	if _, err := source(nil, "", 1); !errors.Is(err, ErrScanContract) {
		t.Fatal(err)
	}
	for _, limit := range []int{-1, 0, 1001} {
		if _, err := source(context.Background(), "", limit); !errors.Is(err, ErrScanContract) {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := source(ctx, "", 1); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("invalid input reached a source")
	}
}

func TestSequenceScanBoundsAndSourceFailure(t *testing.T) {
	key := strings.Repeat("é", 160)
	source, err := SequenceCursorSources([]NamedCursorSource[string]{sequenceFixture("empty"), sequenceFixture(strings.Repeat("n", 32), key)})
	if err != nil {
		t.Fatal(err)
	}
	page, err := source(context.Background(), "", 1)
	if err != nil || page.More || len(page.Items) != 1 {
		t.Fatal("bounded cursor was rejected", err)
	}
	if _, err := EncodeCycle(CycleState{Source: "version", After: page.Items[0].Cursor}); err != nil {
		t.Fatal("cursor does not fit the cycle", err)
	}
	end, err := source(context.Background(), page.Items[0].Cursor, 1)
	if err != nil || end.More || len(end.Items) != 0 {
		t.Fatal("end cursor differs", err)
	}
	fault := errors.New("source failed")
	failed, _ := SequenceCursorSources([]NamedCursorSource[string]{sequenceFixture("first", "a"), {Name: "failed", Read: func(context.Context, string, int) (CursorPage[string], error) { return CursorPage[string]{}, fault }}})
	result, err := failed(context.Background(), "", 2)
	if !errors.Is(err, fault) || len(result.Items) != 0 {
		t.Fatal("source error exposed a partial page", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	canceled, _ := SequenceCursorSources([]NamedCursorSource[string]{{Name: "canceled", Read: func(context.Context, string, int) (CursorPage[string], error) {
		cancel()
		return CursorPage[string]{}, nil
	}}})
	if _, err := canceled(ctx, "", 1); !errors.Is(err, context.Canceled) {
		t.Fatal("source cancellation was hidden", err)
	}
}
