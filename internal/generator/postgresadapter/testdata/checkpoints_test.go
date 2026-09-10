package storage

import (
	"context"
	"errors"
	contract "example.com/transaction-test/contracts/storage"
	"math"
	"strings"
	"testing"
)

func TestCheckpointsSurviveStoreReplacementAndRejectOldWriters(t *testing.T) {
	s, _ := database(t, false)
	ctx := context.Background()
	first, err := s.LoadCheckpoint(ctx, "Record", "id", "users")
	if err != nil || first.Version != 0 {
		t.Fatal(first, err)
	}
	if _, err := s.SaveCheckpoint(ctx, "Record", "id", "users", 0, "a"); !errors.Is(err, ErrTransactionRequired) {
		t.Fatal(err)
	}
	save := func(store *Store, version int64, after string) error {
		return store.WithTransaction(ctx, func(ctx context.Context, tx contract.Transaction) error {
			_, err := tx.(contract.CheckpointStore).SaveCheckpoint(ctx, "Record", "id", "users", version, after)
			return err
		})
	}
	if err := save(s, 0, "a"); err != nil {
		t.Fatal(err)
	}
	next, err := NewStore(s.db)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := next.LoadCheckpoint(ctx, "Record", "id", "users")
	if err != nil || loaded.After != "a" || loaded.Version != 1 {
		t.Fatal(loaded, err)
	}
	if err := save(next, 1, ""); err != nil {
		t.Fatal(err)
	}
	for _, v := range []int64{0, 1} {
		if err := save(s, v, "old"); !errors.Is(err, contract.ErrCheckpointConflict) {
			t.Fatal("stale writer accepted", v, err)
		}
	}
	loaded, err = s.LoadCheckpoint(ctx, "Record", "id", "users")
	if err != nil || loaded.After != "" || loaded.Version != 2 {
		t.Fatal(loaded, err)
	}
	for _, key := range []struct{ id, scope string }{{"ID", "users"}, {"id", "Users"}} {
		value, err := s.LoadCheckpoint(ctx, "Record", key.id, key.scope)
		if err != nil || value.Version != 0 {
			t.Fatal("keys were not exact", value, err)
		}
	}
}
func TestCheckpointFailureRollsBackAndClosedStoreCannotWrite(t *testing.T) {
	s, _ := database(t, false)
	ctx := context.Background()
	var retained contract.CheckpointStore
	err := s.WithTransaction(ctx, func(ctx context.Context, tx contract.Transaction) error {
		retained = tx.(contract.CheckpointStore)
		if _, err := retained.SaveCheckpoint(ctx, "Record", "id", "users", 0, "a"); err != nil {
			return err
		}
		_, _ = retained.SaveCheckpoint(ctx, "Record", "id", "users", 0, "b")
		return nil
	})
	if !errors.Is(err, contract.ErrCheckpointConflict) {
		t.Fatal("ignored failure committed", err)
	}
	value, err := s.LoadCheckpoint(ctx, "Record", "id", "users")
	if err != nil || value.Version != 0 {
		t.Fatal("partial checkpoint committed", value, err)
	}
	if _, err := retained.SaveCheckpoint(ctx, "Record", "id", "users", 0, "a"); !errors.Is(err, ErrTransactionClosed) {
		t.Fatal(err)
	}
	if _, err := retained.LoadCheckpoint(ctx, "Record", "id", "users"); !errors.Is(err, ErrTransactionClosed) {
		t.Fatal(err)
	}
	for _, test := range []struct {
		entity, id, scope, after string
		version                  int64
	}{{"Unknown", "id", "scope", "a", 0}, {"Record", "", "scope", "a", 0}, {"Record", "id", strings.Repeat("s", 129), "a", 0}, {"Record", "id", "scope", strings.Repeat("a", 1025), 0}, {"Record", "id", "scope", "\x00", 0}, {"Record", "id", "scope", "a", -1}, {"Record", "id", "scope", "a", math.MaxInt64}} {
		err := s.WithTransaction(ctx, func(ctx context.Context, tx contract.Transaction) error {
			_, err := tx.(contract.CheckpointStore).SaveCheckpoint(ctx, test.entity, test.id, test.scope, test.version, test.after)
			return err
		})
		if !errors.Is(err, contract.ErrCheckpoint) {
			t.Fatal("invalid checkpoint accepted", err)
		}
	}
}
func TestCheckpointSchemaMustHaveExactPrimaryKey(t *testing.T) {
	s, db := database(t, false)
	if _, err := db.Exec("ALTER TABLE stego_scan_checkpoints DROP CONSTRAINT stego_scan_checkpoints_pkey"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(s.db); err == nil {
		t.Fatal("missing checkpoint key accepted")
	}
}

func TestConcurrentCheckpointWritersHaveOneWinner(t *testing.T) {
	s, _ := database(t, false)
	ctx := context.Background()
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, after := range []string{"a", "b"} {
		go func(after string) {
			<-start
			results <- s.WithLockedResource(ctx, "Record", "id", "resource", func(ctx context.Context, tx contract.Transaction, _ any) error {
				_, err := tx.(contract.CheckpointStore).SaveCheckpoint(ctx, "Record", "resource", "users", 0, after)
				return err
			})
		}(after)
	}
	row := record("checkpoint-resource")
	row.ID = "resource"
	if err := s.Create(ctx, "Record", row); err != nil {
		t.Fatal(err)
	}
	close(start)
	winners := 0
	for range 2 {
		err := <-results
		if err == nil {
			winners++
		} else if !errors.Is(err, contract.ErrCheckpointConflict) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatal("checkpoint had multiple winners", winners)
	}
}

func TestCheckpointSchemaMustKeepCursorBounds(t *testing.T) {
	s, db := database(t, false)
	if _, err := db.Exec("ALTER TABLE stego_scan_checkpoints DROP CONSTRAINT stego_scan_checkpoints_after_cursor_check"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(s.db); err == nil {
		t.Fatal("missing cursor bound accepted")
	}
}
