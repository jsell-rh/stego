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

func TestDeletionFinalizationIsPermanentAndTransactional(t *testing.T) {
	s, db := database(t, false)
	ctx := context.Background()
	r := record("finalize")
	if err := s.Create(ctx, "Record", r); err != nil {
		t.Fatal(err)
	}
	if err := s.FinalizeDeletionIfVersion(ctx, "Record", r.ID, 1); !errors.Is(err, contract.ErrVersionConflict) {
		t.Fatal("live finalization", err)
	}
	if _, err := db.Exec("UPDATE records SET stego_finalized_at=now() WHERE id=$1", r.ID); err == nil {
		t.Fatal("direct SQL finalized a live resource")
	}
	if err := s.Delete(ctx, "Record", r.ID); err != nil {
		t.Fatal(err)
	}
	row := cleanupRecord(t, s, r.ID)
	if err := s.FinalizeDeletionIfVersion(ctx, "Record", r.ID, row.ResourceVersion); !errors.Is(err, contract.ErrVersionConflict) {
		t.Fatal("incomplete finalization", err)
	}
	if _, err := db.Exec("UPDATE records SET stego_finalized_at=now() WHERE id=$1", r.ID); err == nil {
		t.Fatal("direct SQL finalized incomplete cleanup")
	}
	for _, owner := range []string{"identity", "workload"} {
		row = cleanupRecord(t, s, r.ID)
		if err := s.ObserveCleanupIfVersion(ctx, "Record", r.ID, row.ResourceVersion, owner, true); err != nil {
			t.Fatal(err)
		}
	}
	row = cleanupRecord(t, s, r.ID)
	if row.DeletionFinalizedAt != nil {
		t.Fatal("observations finalized the resource")
	}
	if err := s.FinalizeDeletionIfVersion(ctx, "Record", r.ID, row.ResourceVersion-1); !errors.Is(err, contract.ErrVersionConflict) {
		t.Fatal("stale finalization", err)
	}
	failed := errors.New("final event rejected")
	err := s.WithTransaction(ctx, func(ctx context.Context, scope contract.Transaction) error {
		if err := scope.(contract.DeletionFinalizer).FinalizeDeletionIfVersion(ctx, "Record", r.ID, row.ResourceVersion); err != nil {
			return err
		}
		return failed
	})
	if !errors.Is(err, failed) || cleanupRecord(t, s, r.ID).DeletionFinalizedAt != nil {
		t.Fatal("failed commit kept finalization", err)
	}
	if err := s.WithTransaction(ctx, func(ctx context.Context, scope contract.Transaction) error {
		if err := scope.(contract.DeletionFinalizer).FinalizeDeletionIfVersion(ctx, "Record", r.ID, row.ResourceVersion); err != nil {
			return err
		}
		return scope.(*Store).Notify(message(r.ID))
	}); err != nil {
		t.Fatal(err)
	}
	finalized := cleanupRecord(t, s, r.ID)
	if finalized.DeletionFinalizedAt == nil || finalized.ResourceVersion <= row.ResourceVersion || finalized.ResourceGeneration != row.ResourceGeneration {
		t.Fatal("invalid finalization revision")
	}
	if count(t, db, "stego_outbox.messages") != 1 {
		t.Fatal("final event missing")
	}
	if err := s.ObserveCleanupIfVersion(ctx, "Record", r.ID, finalized.ResourceVersion, "identity", false); err != nil {
		t.Fatal("late cleanup", err)
	}
	late := cleanupRecord(t, s, r.ID)
	if late.DeletionFinalizedAt == nil || !late.DeletionFinalizedAt.Equal(*finalized.DeletionFinalizedAt) || late.CleanupComplete("identity") {
		t.Fatal("late cleanup changed finalization")
	}
	for _, statement := range []string{"UPDATE records SET stego_finalized_at=NULL WHERE id=$1", "UPDATE records SET stego_finalized_at=stego_finalized_at+interval '1 second' WHERE id=$1"} {
		if _, err := db.Exec(statement, r.ID); err == nil {
			t.Fatal("direct SQL reversed finalization")
		}
	}
	if err := s.FinalizeDeletionIfVersion(ctx, "Measurement", r.ID, 1); err == nil {
		t.Fatal("unsupported finalization accepted")
	}
}

func TestDeletionFinalizationRequiresEveryRetainedTarget(t *testing.T) {
	s, db := database(t, false)
	ctx := context.Background()
	r := Asset{Meta: Meta{ID: "finalize-targets"}, Region: "first"}
	if err := s.Create(ctx, "Asset", r); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "Asset", r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE assets SET region='second' WHERE id=$1", r.ID); err != nil {
		t.Fatal(err)
	}
	load := func() Asset {
		v, err := s.GetRetained(ctx, "Asset", r.ID)
		if err != nil {
			t.Fatal(err)
		}
		return v.(Asset)
	}
	for _, target := range []string{"first", "second"} {
		for _, owner := range []string{"archive", "compute"} {
			row := load()
			if err := s.FinalizeDeletionIfVersion(ctx, "Asset", r.ID, row.ResourceVersion); !errors.Is(err, contract.ErrVersionConflict) {
				t.Fatal("unfinished target finalized", err)
			}
			if err := s.ObserveTargetCleanupIfVersion(ctx, "Asset", r.ID, row.ResourceVersion, owner, target, true); err != nil {
				t.Fatal(err)
			}
		}
	}
	row := load()
	if err := s.FinalizeDeletionIfVersion(ctx, "Asset", r.ID, row.ResourceVersion); err != nil {
		t.Fatal(err)
	}
	if load().DeletionFinalizedAt == nil {
		t.Fatal("completed target cleanup was not finalized")
	}
}

func TestDeletionFinalizationMigrationPreservesOldVisibility(t *testing.T) {
	s, db := database(t, false)
	ctx := context.Background()
	for _, id := range []string{"legacy-deleted", "live"} {
		if err := s.Create(ctx, "Record", Record{Meta: Meta{ID: id}, Name: id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Delete(ctx, "Record", "legacy-deleted"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("ALTER TABLE records DROP COLUMN stego_finalized_at"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(s.db); err == nil {
		t.Fatal("missing finalization column passed startup")
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error { return tx.Exec(ResourceVersionMigration).Error }); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(s.db); err != nil {
		t.Fatal("migrated startup", err)
	}
	if cleanupRecord(t, s, "legacy-deleted").DeletionFinalizedAt == nil {
		t.Fatal("old deleted resource can reappear")
	}
	if cleanupRecord(t, s, "live").DeletionFinalizedAt != nil {
		t.Fatal("migration finalized a live resource")
	}
	if err := s.Delete(ctx, "Record", "live"); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error { return tx.Exec(ResourceVersionMigration).Error }); err != nil {
		t.Fatal(err)
	}
	if cleanupRecord(t, s, "live").DeletionFinalizedAt != nil {
		t.Fatal("repeat migration finalized a new request")
	}
}

func TestDeletionFinalizationRejectsInvalidSchema(t *testing.T) {
	for _, statement := range []string{
		"ALTER TABLE records ALTER COLUMN stego_finalized_at SET DEFAULT now()",
		"ALTER TABLE records ALTER COLUMN stego_finalized_at SET NOT NULL",
		"ALTER TABLE records ALTER COLUMN stego_finalized_at TYPE timestamp",
	} {
		t.Run(statement, func(t *testing.T) {
			s, db := database(t, false)
			if _, err := db.Exec(statement); err != nil {
				t.Fatal(err)
			}
			if _, err := NewStore(s.db); err == nil {
				t.Fatal("invalid finalization schema passed startup")
			}
		})
	}
}

func TestDeletionFinalizationVisibilityKeepsAccessAndPages(t *testing.T) {
	s, _ := database(t, false)
	ctx := context.Background()
	for _, id := range []string{"a-live", "b-pending", "c-complete", "d-denied", "e-revoked"} {
		if err := s.Create(ctx, "Record", Record{Meta: Meta{ID: id}, Name: id}); err != nil {
			t.Fatal(err)
		}
		if id != "d-denied" {
			if err := s.Create(ctx, "Placement", Placement{Meta: Meta{ID: "grant-" + id}, Name: "reader", Target: "cluster", ParentID: &id}); err != nil {
				t.Fatal(err)
			}
		}
		if id != "a-live" {
			if err := s.Delete(ctx, "Record", id); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Delete(ctx, "Placement", "grant-e-revoked"); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"identity", "workload"} {
		row := cleanupRecord(t, s, "c-complete")
		if err := s.ObserveCleanupIfVersion(ctx, "Record", row.ID, row.ResourceVersion, owner, true); err != nil {
			t.Fatal(err)
		}
	}
	row := cleanupRecord(t, s, "c-complete")
	if err := s.FinalizeDeletionIfVersion(ctx, "Record", row.ID, row.ResourceVersion); err != nil {
		t.Fatal(err)
	}
	row = cleanupRecord(t, s, row.ID)
	if err := s.ObserveCleanupIfVersion(ctx, "Record", row.ID, row.ResourceVersion, "identity", false); err != nil {
		t.Fatal(err)
	}
	// Reconstruct the store. API visibility cannot depend on process memory.
	var err error
	s, err = NewStore(s.db)
	if err != nil {
		t.Fatal(err)
	}
	related := []contract.RelatedFilter{{Entity: "Placement", ForeignField: "parent_id", Values: map[string][]string{"name": {"reader"}}}}
	for page, want := range []string{"a-live", "b-pending", ""} {
		result, err := s.List(ctx, "Record", "", "", contract.ListOptions{IncludeDeleting: true, Related: related, Page: page + 1, Size: 1, OrderBy: []contract.OrderByField{{Field: "id", Direction: "asc"}}, Fields: []string{"name"}})
		if err != nil {
			t.Fatal(err)
		}
		rows := result.Items.([]Record)
		if result.Total != 2 {
			t.Fatal("incorrect visible count", result.Total)
		}
		if want == "" {
			if len(rows) != 0 {
				t.Fatal("page escaped access")
			}
			continue
		}
		if len(rows) != 1 || rows[0].ID != want || rows[0].DeletionFinalizedAt != nil || rows[0].DeletedAt.Valid != (want == "b-pending") {
			t.Fatal("incorrect visible page", rows)
		}
	}
	result, err := s.List(ctx, "Record", "", "", contract.ListOptions{OnlyDeleting: true, Related: related, CountOnly: true})
	if err != nil || result.Total != 1 {
		t.Fatal("pending count", result.Total, err)
	}
	for _, mode := range []contract.CursorDeletion{contract.CursorVisible, contract.CursorDeleting} {
		after := ""
		var ids []string
		for pass := 0; pass < 3; pass++ {
			result, err := s.ReadCursor(ctx, "Record", "", "", contract.CursorOptions{Limit: 1, AfterID: after, Deletion: mode, Related: related})
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range result.Items.([]Record) {
				ids = append(ids, row.ID)
			}
			if !result.More {
				break
			}
			after = result.NextID
		}
		want := []string{"b-pending"}
		if mode == contract.CursorVisible {
			want = []string{"a-live", "b-pending"}
		}
		if !slices.Equal(ids, want) {
			t.Fatal("cursor visibility", ids, want)
		}
	}
	for _, id := range []string{"b-pending", "c-complete"} {
		if _, err := s.Get(ctx, "Record", id); !errors.Is(err, contract.ErrNotFound) {
			t.Fatal("ordinary read exposed deletion", err)
		}
		if err := s.Replace(ctx, "Record", id, Record{Name: id}); !errors.Is(err, contract.ErrNotFound) {
			t.Fatal("ordinary write accepted deletion", err)
		}
		called := false
		err := s.WithLockedResource(ctx, "Record", "id", id, func(context.Context, contract.Transaction, any) error { called = true; return nil })
		if !errors.Is(err, contract.ErrNotFound) || called {
			t.Fatal("deleted resource acquired live lock", err)
		}
	}
}

func TestDeletionFinalizationVisibilityRejectsInvalidOptions(t *testing.T) {
	s, _ := database(t, false)
	ctx := context.Background()
	log := cursorLogger(s)
	for _, opts := range []contract.ListOptions{
		{IncludeDeleting: true, OnlyDeleting: true}, {IncludeDeleting: true, IncludeDeleted: true},
		{IncludeDeleting: true, OnlyDeleted: true}, {OnlyDeleting: true, IncludeDeleted: true}, {OnlyDeleting: true, OnlyDeleted: true},
	} {
		if _, err := s.List(ctx, "Record", "", "", opts); !errors.Is(err, contract.ErrDeletionVisibility) {
			t.Fatal("conflicting visibility", err)
		}
	}
	for _, opts := range []contract.ListOptions{{IncludeDeleting: true}, {OnlyDeleting: true}} {
		if _, err := s.List(ctx, "Measurement", "", "", opts); !errors.Is(err, contract.ErrDeletionVisibility) {
			t.Fatal("unsupported visibility", err)
		}
	}
	for _, mode := range []contract.CursorDeletion{contract.CursorVisible, contract.CursorDeleting} {
		if _, err := s.ReadCursor(ctx, "Measurement", "", "", contract.CursorOptions{Limit: 1, Deletion: mode}); !errors.Is(err, contract.ErrDeletionVisibility) {
			t.Fatal("unsupported cursor visibility", err)
		}
	}
	if log.reads.Load() != 0 {
		t.Fatal("invalid visibility ran a query")
	}
}

func TestDeletionFinalizationProjectionMatchesQueries(t *testing.T) {
	s, _ := database(t, false)
	ctx := context.Background()
	for _, id := range []string{"live", "pending", "finished"} {
		if err := s.Create(ctx, "Parcel", Parcel{Meta: Meta{ID: id}, Name: id}); err != nil {
			t.Fatal(err)
		}
		if err := s.ObserveIfVersion(ctx, "Parcel", id, 1, "readiness", map[string]any{"state": "Ready"}); err != nil {
			t.Fatal(err)
		}
		if id != "live" {
			if err := s.Delete(ctx, "Parcel", id); err != nil {
				t.Fatal(err)
			}
		}
	}
	load := func(id string) Parcel {
		v, err := s.GetRetained(ctx, "Parcel", id)
		if err != nil {
			t.Fatal(err)
		}
		return v.(Parcel)
	}
	row := load("finished")
	if err := s.ObserveCleanupIfVersion(ctx, "Parcel", row.ID, row.ResourceVersion, "retention", true); err != nil {
		t.Fatal(err)
	}
	row = load(row.ID)
	if err := s.FinalizeDeletionIfVersion(ctx, "Parcel", row.ID, row.ResourceVersion); err != nil {
		t.Fatal(err)
	}
	removing := "Removing \\ ' ?"
	row = load("pending")
	if row.State == nil || *row.State != "Ready" {
		t.Fatal("projection replaced stored diagnostic data")
	}
	projected := row.CurrentObservations()
	if projected.State == nil || *projected.State != removing {
		t.Fatal("pending deletion projection", projected.State)
	}
	condition := &contract.RowFilter{Field: "state", Values: []string{removing}}
	for _, countOnly := range []bool{false, true} {
		result, err := s.List(ctx, "Parcel", "", "", contract.ListOptions{IncludeDeleting: true, CountOnly: countOnly, Filter: condition, Page: 1, Size: 1, Fields: []string{"state"}})
		if err != nil || result.Total != 1 {
			t.Fatal("deletion projection filter and count", result.Total, err)
		}
		if !countOnly {
			rows := result.Items.([]Parcel)
			if len(rows) != 1 || rows[0].ID != "pending" || rows[0].State == nil || *rows[0].State != removing {
				t.Fatal("projected page differs from its filter", rows)
			}
		}
	}
	result, err := s.ReadCursor(ctx, "Parcel", "", "", contract.CursorOptions{Limit: 1, Deletion: contract.CursorVisible, Filter: condition})
	if err != nil {
		t.Fatal(err)
	}
	rows := result.Items.([]Parcel)
	if len(rows) != 1 || rows[0].ID != "pending" || result.More {
		t.Fatal("projected cursor differs from its filter", rows)
	}
	ordered, err := s.List(ctx, "Parcel", "", "", contract.ListOptions{IncludeDeleting: true, Size: 1, Page: 1, OrderBy: []contract.OrderByField{{Field: "state", Direction: "desc"}}})
	if err != nil || ordered.Total != 2 || ordered.Items.([]Parcel)[0].ID != "pending" {
		t.Fatal("ordering does not use deletion projection", err)
	}
	final := load("finished").CurrentObservations()
	if final.State == nil || *final.State != "Unknown" {
		t.Fatal("finalized resource retained pending projection")
	}
}
