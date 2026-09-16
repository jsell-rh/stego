package storage

import (
	"bytes"
	"context"
	"errors"
	"gorm.io/gorm"
	"math"
	"strings"
	"testing"

	contract "example.com/transaction-test/contracts/storage"
)

func saveResourceState(s *Store, version int64, data []byte) error {
	return s.WithTransaction(context.Background(), func(ctx context.Context, tx contract.Transaction) error {
		_, err := tx.(contract.ResourceStateStore).SaveResourceState(ctx, "Record", "resource", "provider", version, data)
		return err
	})
}

func TestResourceStateRestartAndStaleWriters(t *testing.T) {
	s, db := database(t, false)
	ctx := context.Background()
	if value, err := s.LoadResourceState(ctx, "Record", "resource", "provider"); err != nil || value.Version != 0 || len(value.Data) != 0 {
		t.Fatal("absent state differs", err)
	}
	if _, err := s.SaveResourceState(ctx, "Record", "resource", "provider", 0, []byte{0, 255}); !errors.Is(err, ErrTransactionRequired) {
		t.Fatal("state write escaped transaction", err)
	}
	content := bytes.Repeat([]byte{0, 255, 128, 1}, contract.MaxResourceStateBytes/4)
	if err := saveResourceState(s, 0, content); err != nil {
		t.Fatal(err)
	}
	next, err := NewStore(s.db)
	if err != nil {
		t.Fatal(err)
	}
	value, err := next.LoadResourceState(ctx, "Record", "resource", "provider")
	if err != nil || value.Version != 1 || !bytes.Equal(value.Data, content) {
		t.Fatal("restart lost binary state", err)
	}
	value.Data[0] = 12
	value, err = next.LoadResourceState(ctx, "Record", "resource", "provider")
	if err != nil || !bytes.Equal(value.Data, content) {
		t.Fatal("returned bytes changed stored state", err)
	}
	if err := saveResourceState(next, 1, nil); err != nil {
		t.Fatal(err)
	}
	for _, version := range []int64{0, 1} {
		if err := saveResourceState(s, version, []byte("old")); !errors.Is(err, contract.ErrResourceStateConflict) {
			t.Fatal("stale writer accepted", err)
		}
	}
	value, err = next.LoadResourceState(ctx, "Record", "resource", "provider")
	if err != nil || value.Version != 2 || len(value.Data) != 0 {
		t.Fatal("clear lost retained version", err)
	}
	for _, key := range []struct{ id, scope string }{{"Resource", "provider"}, {"resource", "Provider"}} {
		value, err := next.LoadResourceState(ctx, "Record", key.id, key.scope)
		if err != nil || value.Version != 0 {
			t.Fatal("record key is not exact", err)
		}
	}
	for _, statement := range []string{
		"DELETE FROM stego_resource_state",
		"UPDATE stego_resource_state SET resource_id='different',version=version+1",
		"UPDATE stego_resource_state SET version=version+2",
		"UPDATE stego_resource_state SET version=1",
		"UPDATE stego_resource_state SET data='changed'::bytea",
	} {
		if _, err := db.Exec(statement); err == nil {
			t.Fatal("state history guard accepted a forbidden write")
		}
	}
}

func TestResourceStateFailurePreventsCommit(t *testing.T) {
	s, _ := database(t, false)
	ctx := context.Background()
	var retained contract.ResourceStateStore
	err := s.WithTransaction(ctx, func(ctx context.Context, tx contract.Transaction) error {
		retained = tx.(contract.ResourceStateStore)
		if _, err := retained.SaveResourceState(ctx, "Record", "resource", "provider", 0, []byte("first")); err != nil {
			return err
		}
		_, _ = retained.SaveResourceState(ctx, "Record", "resource", "provider", 0, []byte("stale"))
		return nil
	})
	if !errors.Is(err, contract.ErrResourceStateConflict) {
		t.Fatal("ignored save failure committed", err)
	}
	if value, err := s.LoadResourceState(ctx, "Record", "resource", "provider"); err != nil || value.Version != 0 {
		t.Fatal("failed transaction retained state", err)
	}
	if _, err := retained.LoadResourceState(ctx, "Record", "resource", "provider"); !errors.Is(err, ErrTransactionClosed) {
		t.Fatal(err)
	}
	if _, err := retained.SaveResourceState(ctx, "Record", "resource", "provider", 0, nil); !errors.Is(err, ErrTransactionClosed) {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		entity, id, scope string
		version           int64
		data              []byte
	}{
		{"Unknown", "resource", "provider", 0, nil}, {"Record", "", "provider", 0, nil}, {"Record", "resource", "\x00", 0, nil},
		{"Record", "resource", "provider", -1, nil}, {"Record", "resource", "provider", math.MaxInt64, nil},
		{"Record", "resource", "provider", 0, make([]byte, contract.MaxResourceStateBytes+1)},
	} {
		err := s.WithTransaction(ctx, func(ctx context.Context, tx contract.Transaction) error {
			_, err := tx.(contract.ResourceStateStore).SaveResourceState(ctx, tc.entity, tc.id, tc.scope, tc.version, tc.data)
			return err
		})
		if !errors.Is(err, contract.ErrResourceState) {
			t.Fatal("invalid state accepted", err)
		}
	}
}

func TestResourceStateConcurrentCreateHasOneWinner(t *testing.T) {
	s, _ := database(t, false)
	ctx := context.Background()
	row := record("state-resource")
	row.ID = "resource"
	if err := s.Create(ctx, "Record", row); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, data := range []string{"first", "second"} {
		go func(data string) {
			<-start
			results <- s.WithLockedResource(ctx, "Record", "id", "resource", func(ctx context.Context, tx contract.Transaction, _ any) error {
				_, err := tx.(contract.ResourceStateStore).SaveResourceState(ctx, "Record", "resource", "provider", 0, []byte(data))
				return err
			})
		}(data)
	}
	close(start)
	winners := 0
	for range 2 {
		err := <-results
		if err == nil {
			winners++
		} else if !errors.Is(err, contract.ErrResourceStateConflict) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatal("state creation had multiple winners", winners)
	}
}

func TestResourceStateSchemaGuard(t *testing.T) {
	for _, change := range []string{
		"ALTER TABLE stego_resource_state DROP CONSTRAINT stego_resource_state_pkey",
		"DROP INDEX stego_resource_state_scope_idx",
		"DROP INDEX stego_resource_state_scope_idx; CREATE INDEX stego_resource_state_scope_idx ON stego_resource_state(entity,resource_id,scope)",
		"DROP INDEX stego_resource_state_scope_idx; CREATE INDEX stego_resource_state_scope_idx ON stego_resource_state(entity,scope,resource_id) WHERE version>1",
		"ALTER TABLE stego_resource_state DISABLE TRIGGER stego_resource_state_guard",
		"ALTER TABLE stego_resource_state ALTER COLUMN data DROP NOT NULL",
		"ALTER TABLE stego_resource_state DROP CONSTRAINT stego_resource_state_data_check",
		"ALTER TABLE stego_resource_state ENABLE ROW LEVEL SECURITY",
	} {
		t.Run(change, func(t *testing.T) {
			s, db := database(t, false)
			if _, err := db.Exec(change); err != nil {
				t.Fatal(err)
			}
			if _, err := NewStore(s.db); err == nil {
				t.Fatal("invalid state schema accepted")
			}
		})
	}
}

func TestResourceStateKeysRetainCoverage(t *testing.T) {
	s, _ := database(t, false)
	ctx := context.Background()
	save := func(entity, id, scope string, version int64, data []byte) {
		t.Helper()
		if err := s.WithTransaction(ctx, func(ctx context.Context, tx contract.Transaction) error {
			_, err := tx.(contract.ResourceStateStore).SaveResourceState(ctx, entity, id, scope, version, data)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	// No domain rows exist. The scope and entity must still isolate these records.
	for _, id := range []string{"a", "B", "é"} {
		save("Record", id, "provider", 0, []byte("sealed"))
	}
	save("Record", "a", "provider", 1, nil)
	save("Record", "other-scope", "Provider", 0, nil)
	save("Measurement", "other-entity", "provider", 0, nil)
	first, err := s.ListResourceStateKeys(ctx, "Record", "provider", "", 2)
	if err != nil || !first.More || len(first.Keys) != 2 || first.Keys[0].ResourceID != "B" || first.Keys[1].ResourceID != "a" || first.Keys[1].Version != 2 {
		t.Fatalf("first page differs: %+v %v", first, err)
	}
	// Rebuild the store and change contents between pages. Neither operation can
	// remove the later ID from this scan. A new earlier ID needs a new scan.
	next, err := NewStore(s.db)
	if err != nil {
		t.Fatal(err)
	}
	save("Record", "é", "provider", 1, nil)
	save("Record", "A", "provider", 0, nil)
	second, err := next.ListResourceStateKeys(ctx, "Record", "provider", first.Keys[1].ResourceID, 2)
	if err != nil || second.More || len(second.Keys) != 1 || second.Keys[0].ResourceID != "é" || second.Keys[0].Version != 2 {
		t.Fatalf("resumed page differs: %+v %v", second, err)
	}
	all, err := next.ListResourceStateKeys(ctx, "Record", "provider", "", 1000)
	if err != nil || all.More || len(all.Keys) != 4 || all.Keys[0].ResourceID != "A" {
		t.Fatal("new scan lost a retained record", err)
	}
	empty, err := next.ListResourceStateKeys(ctx, "Record", "provider", "é", 1)
	if err != nil || empty.More || len(empty.Keys) != 0 {
		t.Fatal("end page differs", err)
	}
	exact, err := next.ListResourceStateKeys(ctx, "Record", "Provider", "", 1)
	if err != nil || exact.More || len(exact.Keys) != 1 || exact.Keys[0].ResourceID != "other-scope" {
		t.Fatal("scope differs", err)
	}
}

func TestResourceStateKeysRejectInvalidInput(t *testing.T) {
	// An uninitialized GORM handle must never receive an invalid query.
	s := &Store{db: &gorm.DB{}}
	for _, tc := range []struct {
		entity, scope, after string
		limit                int
	}{
		{"record", "provider", "", 1}, {"Record", "", "", 1}, {"Record", strings.Repeat("s", 129), "", 1},
		{"Record", "provider", "a\x00b", 1}, {"Record", "provider", "\xff", 1}, {"Record", "provider", strings.Repeat("a", 257), 1},
		{"Record", "provider", "", 0}, {"Record", "provider", "", 1001}, {"Record", "provider", "", -1},
	} {
		if _, err := s.ListResourceStateKeys(context.Background(), tc.entity, tc.scope, tc.after, tc.limit); !errors.Is(err, contract.ErrResourceState) {
			t.Fatal("invalid page accepted", err)
		}
	}
	if _, err := s.ListResourceStateKeys(nil, "Record", "provider", "", 1); !errors.Is(err, contract.ErrResourceState) {
		t.Fatal("nil context accepted", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.ListResourceStateKeys(ctx, "Record", "provider", "", 1); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled read accepted", err)
	}
}

func TestResourceStateKeysRejectClosedTransaction(t *testing.T) {
	s, _ := database(t, false)
	var retained contract.ResourceStateKeyReader
	if err := s.WithTransaction(context.Background(), func(ctx context.Context, tx contract.Transaction) error {
		retained = tx.(contract.ResourceStateKeyReader)
		_, err := retained.ListResourceStateKeys(ctx, "Record", "provider", "", 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := retained.ListResourceStateKeys(context.Background(), "Record", "provider", "", 1); !errors.Is(err, ErrTransactionClosed) {
		t.Fatal("closed transaction accepted", err)
	}
}
