package storage

import (
	"bytes"
	"context"
	"errors"
	"math"
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
