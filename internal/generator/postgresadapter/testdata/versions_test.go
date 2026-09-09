package storage

import (
	"context"
	"errors"
	"sync"
	"testing"

	contract "example.com/transaction-test/contracts/storage"
)

func versionRecord(t *testing.T, store *Store, id string) Record {
	t.Helper()
	value, err := store.Get(context.Background(), "Record", id)
	if err != nil {
		t.Fatal(err)
	}
	return value.(Record)
}

func TestResourceVersionCoversAllWritesAndRejectsOldObservation(t *testing.T) {
	store, db := database(t, false)
	ctx := context.Background()
	row := Record{Meta: Meta{ID: "version-record"}, Name: "before", ResourceVersion: 800}
	if err := store.Create(ctx, "Record", row); err != nil {
		t.Fatal(err)
	}
	observed := versionRecord(t, store, row.ID)
	if observed.ResourceVersion != 1 {
		t.Fatal("insert accepted a caller revision", observed.ResourceVersion)
	}
	if _, err := db.Exec("UPDATE records SET name='changed', stego_revision=900 WHERE id=$1", row.ID); err != nil {
		t.Fatal(err)
	}
	current := versionRecord(t, store, row.ID)
	if current.ResourceVersion != 2 {
		t.Fatal("raw SQL bypassed revision advance", current.ResourceVersion)
	}
	observed.Name = "stale success"
	if err := store.ReplaceIfVersion(ctx, "Record", row.ID, observed.ResourceVersion, observed); !errors.Is(err, contract.ErrVersionConflict) {
		t.Fatal("old observation was accepted", err)
	}
	if current = versionRecord(t, store, row.ID); current.Name != "changed" || current.ResourceVersion != 2 {
		t.Fatal("failed write changed state")
	}
	current.Name = "accepted"
	current.ResourceVersion = 700
	if err := store.ReplaceIfVersion(ctx, "Record", row.ID, 2, current); err != nil {
		t.Fatal(err)
	}
	if current = versionRecord(t, store, row.ID); current.Name != "accepted" || current.ResourceVersion != 3 {
		t.Fatal("conditional update failed")
	}
	if err := store.Replace(ctx, "Record", row.ID, current); err != nil {
		t.Fatal(err)
	}
	if current = versionRecord(t, store, row.ID); current.ResourceVersion != 4 {
		t.Fatal("ordinary replace bypassed revision")
	}
	if err := Migrate(store.db); err != nil {
		t.Fatal(err)
	}
	if current = versionRecord(t, store, row.ID); current.ResourceVersion != 4 {
		t.Fatal("migration reset an existing revision")
	}
}

func TestResourceVersionConflictRollsBackWork(t *testing.T) {
	store, db := database(t, false)
	ctx := context.Background()
	if err := store.Create(ctx, "Record", Record{Meta: Meta{ID: "target"}, Name: "target"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE records SET value=1 WHERE id='target'"); err != nil {
		t.Fatal(err)
	}
	err := store.WithTransaction(ctx, func(ctx context.Context, tx contract.Transaction) error {
		if err := tx.Create(ctx, "Record", Record{Meta: Meta{ID: "other"}, Name: "other"}); err != nil {
			return err
		}
		return tx.(contract.VersionedWriter).ReplaceIfVersion(ctx, "Record", "target", 1, Record{Name: "stale"})
	})
	if !errors.Is(err, contract.ErrVersionConflict) {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, "Record", "other"); !errors.Is(err, contract.ErrNotFound) {
		t.Fatal("failed conditional write committed other work", err)
	}
	if versionRecord(t, store, "target").ResourceVersion != 2 {
		t.Fatal("failed transaction changed revision")
	}
}

func TestOnlyOneConcurrentVersionWriterSucceeds(t *testing.T) {
	store, _ := database(t, true)
	ctx := context.Background()
	if err := store.Create(ctx, "Record", Record{Meta: Meta{ID: "race"}, Name: "race"}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for i := range 2 {
		workers.Go(func() {
			<-start
			results <- store.ReplaceIfVersion(ctx, "Record", "race", 1, Record{Name: "race", Value: int64(i)})
		})
	}
	close(start)
	workers.Wait()
	close(results)
	passed, conflicted := 0, 0
	for err := range results {
		if err == nil {
			passed++
		} else if errors.Is(err, contract.ErrVersionConflict) {
			conflicted++
		} else {
			t.Fatal(err)
		}
	}
	if passed != 1 || conflicted != 1 || versionRecord(t, store, "race").ResourceVersion != 2 {
		t.Fatal(passed, conflicted)
	}
}

func TestVersionedIdentityAndDeletionCannotBeReversed(t *testing.T) {
	store, db := database(t, false)
	ctx := context.Background()
	if err := store.Create(ctx, "Record", Record{Meta: Meta{ID: "retained"}, Name: "retained"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE records SET id='replacement' WHERE id='retained'"); err == nil {
		t.Fatal("identity changed")
	}
	if err := store.Delete(ctx, "Record", "retained"); err != nil {
		t.Fatal(err)
	}
	var revision int64
	if err := db.QueryRow("SELECT stego_revision FROM records WHERE id='retained'").Scan(&revision); err != nil || revision != 2 {
		t.Fatal(revision, err)
	}
	for _, statement := range []string{"UPDATE records SET deleted_at=NULL WHERE id='retained'", "DELETE FROM records WHERE id='retained'"} {
		if _, err := db.Exec(statement); err == nil {
			t.Fatal("deletion history was removed", statement)
		}
	}
	if err := store.ReplaceIfVersion(ctx, "Record", "retained", 2, Record{Name: "restored"}); !errors.Is(err, contract.ErrVersionConflict) {
		t.Fatal("deleted row accepted a write", err)
	}
}

func TestVersionStartupRejectsDisabledTrigger(t *testing.T) {
	store, db := database(t, false)
	if _, err := db.Exec("ALTER TABLE records DISABLE TRIGGER stego_resource_revision"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(store.db); err == nil {
		t.Fatal("disabled version trigger was accepted")
	}
	if err := Migrate(store.db); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(store.db); err != nil {
		t.Fatal(err)
	}
}

func TestConditionalWriteRollsBackRevisionAndValue(t *testing.T) {
	store, _ := database(t, false)
	ctx := context.Background()
	if err := store.Create(ctx, "Record", Record{Meta: Meta{ID: "rollback"}, Name: "before"}); err != nil {
		t.Fatal(err)
	}
	rejected := errors.New("reject transaction")
	err := store.WithTransaction(ctx, func(ctx context.Context, tx contract.Transaction) error {
		if err := tx.(contract.VersionedWriter).ReplaceIfVersion(ctx, "Record", "rollback", 1, Record{Name: "after"}); err != nil {
			return err
		}
		return rejected
	})
	if !errors.Is(err, rejected) {
		t.Fatal(err)
	}
	row := versionRecord(t, store, "rollback")
	if row.ResourceVersion != 1 || row.Name != "before" {
		t.Fatal("rollback retained an uncommitted revision", row)
	}
}

func TestRevisionOverflowFailsWithoutChangingState(t *testing.T) {
	store, db := database(t, false)
	ctx := context.Background()
	if err := store.Create(ctx, "Record", Record{Meta: Meta{ID: "overflow"}, Name: "before"}); err != nil {
		t.Fatal(err)
	}
	// Only a trusted administrator can reach the boundary without prior writes.
	for _, sql := range []string{"ALTER TABLE records DISABLE TRIGGER stego_resource_revision", "UPDATE records SET stego_revision=9223372036854775807 WHERE id='overflow'", "ALTER TABLE records ENABLE TRIGGER stego_resource_revision"} {
		if _, err := db.Exec(sql); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ReplaceIfVersion(ctx, "Record", "overflow", 9223372036854775807, Record{Name: "after"}); err == nil {
		t.Fatal("revision overflow accepted")
	}
	row := versionRecord(t, store, "overflow")
	if row.ResourceVersion != 9223372036854775807 || row.Name != "before" {
		t.Fatal("overflow changed state")
	}
}
