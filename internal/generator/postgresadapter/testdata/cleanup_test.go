package storage

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	contract "example.com/transaction-test/contracts/storage"
	"gorm.io/gorm"
)

func cleanupRecord(t *testing.T, store *Store, id string) Record {
	t.Helper()
	value, err := store.GetRetained(context.Background(), "Record", id)
	if err != nil {
		t.Fatal(err)
	}
	return value.(Record)
}
func TestCleanupOwnersRequireDeletedStateAndCurrentRevision(t *testing.T) {
	store, db := database(t, true)
	ctx := context.Background()
	if err := store.Create(ctx, "Record", Record{Meta: Meta{ID: "cleanup"}, Name: "before", CleanupState: []byte(`{"identity":true,"workload":true}`)}); err != nil {
		t.Fatal(err)
	}
	live := cleanupRecord(t, store, "cleanup")
	if pending, err := live.PendingCleanup(); err != nil || len(pending) != 0 || live.CleanupComplete("workload") {
		t.Fatal("live row has completed cleanup", pending, err)
	}
	if err := store.ObserveCleanupIfVersion(ctx, "Record", "cleanup", 1, "workload", true); !errors.Is(err, contract.ErrVersionConflict) {
		t.Fatal("live cleanup accepted", err)
	}
	if err := store.Delete(ctx, "Record", "cleanup"); err != nil {
		t.Fatal(err)
	}
	deleted := cleanupRecord(t, store, "cleanup")
	if pending, err := deleted.PendingCleanup(); err != nil || !slices.Equal(pending, []string{"identity", "workload"}) || deleted.ResourceVersion != 2 {
		t.Fatal("deletion lost owners", pending, err)
	}
	if err := store.ObserveCleanupIfVersion(ctx, "Record", "cleanup", 1, "workload", true); !errors.Is(err, contract.ErrVersionConflict) {
		t.Fatal("old completion accepted", err)
	}
	if err := store.ObserveCleanupIfVersion(ctx, "Record", "cleanup", 2, "unknown", true); err == nil {
		t.Fatal("unknown owner accepted")
	}
	results := make(chan error, 2)
	for _, owner := range []string{"identity", "workload"} {
		go func() { results <- store.ObserveCleanupIfVersion(ctx, "Record", "cleanup", 2, owner, true) }()
	}
	success, conflict := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			success++
		} else if errors.Is(err, contract.ErrVersionConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal("concurrent completions did not conflict", success, conflict)
	}
	row := cleanupRecord(t, store, "cleanup")
	pending, err := row.PendingCleanup()
	if err != nil || len(pending) != 1 || row.ResourceVersion != 3 {
		t.Fatal("owner update replaced another state", pending, err)
	}
	if err := store.ObserveCleanupIfVersion(ctx, "Record", "cleanup", 3, pending[0], true); err != nil {
		t.Fatal(err)
	}
	row = cleanupRecord(t, store, "cleanup")
	if !row.CleanupComplete("identity") || !row.CleanupComplete("workload") || row.CleanupComplete("unknown") {
		t.Fatal("cleanup confirmations are wrong")
	}
	if err := store.ObserveCleanupIfVersion(ctx, "Record", "cleanup", 4, "workload", false); err != nil {
		t.Fatal(err)
	}
	row = cleanupRecord(t, store, "cleanup")
	if row.CleanupComplete("workload") || !row.CleanupComplete("identity") {
		t.Fatal("late effects did not reopen only their owner")
	}
	if _, err := db.Exec("UPDATE records SET name='changed' WHERE id='cleanup'"); err != nil {
		t.Fatal(err)
	}
	row = cleanupRecord(t, store, "cleanup")
	if pending, err := row.PendingCleanup(); err != nil || len(pending) != 2 || row.ResourceVersion != 6 {
		t.Fatal("changed cleanup inputs kept a confirmation", pending, err)
	}
	for _, bad := range []string{`null`, `[]`, `{"workload":true}`, `{"identity":true,"workload":null}`, `{"identity":true,"workload":true,"extra":false}`} {
		if _, err := db.Exec("UPDATE records SET stego_cleanup=$1::jsonb WHERE id='cleanup'", bad); err == nil {
			t.Fatal("invalid cleanup metadata accepted", bad)
		}
	}
	if cleanupRecord(t, store, "cleanup").ResourceVersion != 6 {
		t.Fatal("invalid metadata changed revision")
	}
}

func TestCleanupObservationAndEventsRollbackTogether(t *testing.T) {
	store, db := database(t, false)
	ctx := context.Background()
	if err := store.Create(ctx, "Record", Record{Meta: Meta{ID: "rollback-cleanup"}, Name: "rollback"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "Record", "rollback-cleanup"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("ALTER TABLE stego_outbox.messages ADD CONSTRAINT reject_cleanup CHECK(false) NOT VALID"); err != nil {
		t.Fatal(err)
	}
	err := store.WithTransaction(ctx, func(ctx context.Context, tx contract.Transaction) error {
		if err := tx.(contract.CleanupWriter).ObserveCleanupIfVersion(ctx, "Record", "rollback-cleanup", 2, "workload", true); err != nil {
			return err
		}
		return tx.Notify(message("rollback-cleanup"))
	})
	if err == nil {
		t.Fatal("event failure committed cleanup")
	}
	row := cleanupRecord(t, store, "rollback-cleanup")
	if row.ResourceVersion != 2 || row.CleanupComplete("workload") {
		t.Fatal("failed event kept cleanup observation")
	}
	if _, err := db.Exec("ALTER TABLE stego_outbox.messages DROP CONSTRAINT reject_cleanup"); err != nil {
		t.Fatal(err)
	}
	if err := store.WithTransaction(ctx, func(ctx context.Context, tx contract.Transaction) error {
		if err := tx.(contract.CleanupWriter).ObserveCleanupIfVersion(ctx, "Record", "rollback-cleanup", 2, "workload", true); err != nil {
			return err
		}
		return tx.Notify(message("rollback-cleanup"))
	}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM stego_outbox.messages").Scan(&count); err != nil || count != 1 || !cleanupRecord(t, store, "rollback-cleanup").CleanupComplete("workload") {
		t.Fatal("cleanup and event did not commit", count, err)
	}
}

func TestCleanupMigrationCannotRemoveOwners(t *testing.T) {
	store, _ := database(t, false)
	ctx := context.Background()
	if err := store.Create(ctx, "Record", Record{Meta: Meta{ID: "owner-migration"}, Name: "owner"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "Record", "owner-migration"); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveCleanupIfVersion(ctx, "Record", "owner-migration", 2, "workload", true); err != nil {
		t.Fatal(err)
	}
	before := cleanupRecord(t, store, "owner-migration")
	if err := Migrate(store.db); err != nil {
		t.Fatal(err)
	}
	after := cleanupRecord(t, store, "owner-migration")
	if after.ResourceVersion != before.ResourceVersion || !after.CleanupComplete("workload") {
		t.Fatal("repeat migration invalidated current cleanup")
	}
	removed := strings.ReplaceAll(ResourceVersionMigration, "workload", "replacement")
	err := store.db.Transaction(func(tx *gorm.DB) error { return tx.Exec(removed).Error })
	if err == nil || !strings.Contains(err.Error(), "cleanup owners cannot be removed") {
		t.Fatal("owner removal was not rejected", err)
	}
	if _, err := NewStore(store.db); err != nil {
		t.Fatal("failed migration changed trigger contract", err)
	}
	after = cleanupRecord(t, store, "owner-migration")
	if after.ResourceVersion != before.ResourceVersion || !after.CleanupComplete("workload") {
		t.Fatal("failed owner removal changed cleanup")
	}
}

//go:embed cleanup_added.sql
var addedCleanupContract string

//go:embed cleanup_removed.sql
var removedCleanupContract string

func TestCleanupContractChangesRequireNewObservations(t *testing.T) {
	store, db := database(t, false)
	ctx := context.Background()
	if err := store.Create(ctx, "Record", Record{Meta: Meta{ID: "cleanup-upgrade"}, Name: "upgrade"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "Record", "cleanup-upgrade"); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveCleanupIfVersion(ctx, "Record", "cleanup-upgrade", 2, "workload", true); err != nil {
		t.Fatal(err)
	}
	err := store.db.Transaction(func(tx *gorm.DB) error { return tx.Exec(removedCleanupContract).Error })
	if err == nil || !strings.Contains(err.Error(), "cleanup owners cannot be removed") {
		t.Fatal("all cleanup owners were removed", err)
	}
	if _, err := NewStore(store.db); err != nil {
		t.Fatal("failed removal changed the contract", err)
	}
	if err := store.db.Transaction(func(tx *gorm.DB) error { return tx.Exec(addedCleanupContract).Error }); err != nil {
		t.Fatal(err)
	}
	var revision int64
	var encoded []byte
	if err := db.QueryRow("SELECT stego_revision,stego_cleanup FROM records WHERE id='cleanup-upgrade'").Scan(&revision, &encoded); err != nil {
		t.Fatal(err)
	}
	var observations map[string]bool
	if json.Unmarshal(encoded, &observations) != nil || len(observations) != 3 || revision != 4 {
		t.Fatal("owner addition lost metadata", revision, string(encoded))
	}
	for _, owner := range []string{"identity", "workload", "archive"} {
		if done, ok := observations[owner]; !ok || done {
			t.Fatal("changed cleanup contract kept old confirmation", owner)
		}
	}
	if _, err := NewStore(store.db); err == nil {
		t.Fatal("old generated code accepted the new cleanup contract")
	}
	if err := store.db.Transaction(func(tx *gorm.DB) error { return tx.Exec(addedCleanupContract).Error }); err != nil {
		t.Fatal(err)
	}
	var repeated int64
	if err := db.QueryRow("SELECT stego_revision FROM records WHERE id='cleanup-upgrade'").Scan(&repeated); err != nil || repeated != revision {
		t.Fatal("repeat cleanup migration changed revision", repeated, err)
	}
}
