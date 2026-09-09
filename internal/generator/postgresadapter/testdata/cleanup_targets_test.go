package storage

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	contract "example.com/transaction-test/contracts/storage"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func placement(t *testing.T, store *Store, id string) Placement {
	t.Helper()
	value, err := store.GetRetained(context.Background(), "Placement", id)
	if err != nil {
		t.Fatal(err)
	}
	return value.(Placement)
}

func TestCleanupRetainsEveryTargetBeforeProviderWork(t *testing.T) {
	store, db := database(t, true)
	ctx := context.Background()
	row := Placement{Meta: Meta{ID: "moving"}, Target: "first", Name: "job", CleanupTargetState: []byte(`{"worker":{"forged":true}}`)}
	if err := store.Create(ctx, "Placement", row); err != nil {
		t.Fatal(err)
	}
	row = placement(t, store, "moving")
	targets, err := row.CleanupTargets()
	if err != nil || len(targets["worker"]) != 1 || targets["worker"]["first"] {
		t.Fatal("creation did not record its target", targets, err)
	}
	if _, ok := targets["worker"]["first"]; !ok {
		t.Fatal("current target is missing")
	}
	if err := store.ObserveTargetCleanupIfVersion(ctx, "Placement", "moving", 1, "worker", "first", true); !errors.Is(err, contract.ErrVersionConflict) {
		t.Fatal("live target completed", err)
	}
	if _, err := db.Exec("UPDATE placements SET target='second' WHERE id='moving'"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE placements SET target='first' WHERE id='moving'"); err != nil {
		t.Fatal(err)
	}
	row = placement(t, store, "moving")
	targets, err = row.CleanupTargets()
	if err != nil || len(targets["worker"]) != 2 || row.ResourceVersion != 3 {
		t.Fatal("move discarded a target", row.ResourceVersion, targets, err)
	}
	if err := store.Delete(ctx, "Placement", "moving"); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveCleanupIfVersion(ctx, "Placement", "moving", 4, "worker", true); err == nil {
		t.Fatal("global observation bypassed target evidence")
	}
	if err := store.ObserveTargetCleanupIfVersion(ctx, "Placement", "moving", 4, "worker", "unknown", true); !errors.Is(err, contract.ErrVersionConflict) {
		t.Fatal("unknown target accepted", err)
	}
	outcomes := make(chan error, 2)
	for _, target := range []string{"first", "second"} {
		go func() {
			outcomes <- store.ObserveTargetCleanupIfVersion(ctx, "Placement", "moving", 4, "worker", target, true)
		}()
	}
	success, conflict := 0, 0
	for range 2 {
		err := <-outcomes
		if err == nil {
			success++
		} else if errors.Is(err, contract.ErrVersionConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal("concurrent target writes lost revision protection", success, conflict)
	}
	row = placement(t, store, "moving")
	targets, err = row.CleanupTargets()
	if err != nil || row.CleanupComplete("worker") {
		t.Fatal("partial target completion completed its owner", err)
	}
	for target, complete := range targets["worker"] {
		if !complete {
			if err := store.ObserveTargetCleanupIfVersion(ctx, "Placement", "moving", row.ResourceVersion, "worker", target, true); err != nil {
				t.Fatal(err)
			}
		}
	}
	row = placement(t, store, "moving")
	if !row.CleanupComplete("worker") || row.CleanupComplete("identity") {
		t.Fatal("target aggregate changed an independent owner")
	}
	orm, err := gorm.Open(postgres.New(postgres.Config{Conn: db}), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(orm); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewStore(orm)
	if err != nil {
		t.Fatal(err)
	}
	after := placement(t, restarted, "moving")
	if after.ResourceVersion != row.ResourceVersion || !after.CleanupComplete("worker") {
		t.Fatal("restart or repeat migration lost cleanup")
	}
	if err := store.ObserveTargetCleanupIfVersion(ctx, "Placement", "moving", row.ResourceVersion, "worker", "second", false); err != nil {
		t.Fatal(err)
	}
	if placement(t, store, "moving").CleanupComplete("worker") {
		t.Fatal("late effect did not reopen its owner")
	}
	if _, err := db.Exec("UPDATE placements SET name='changed' WHERE id='moving'"); err != nil {
		t.Fatal(err)
	}
	row = placement(t, store, "moving")
	targets, err = row.CleanupTargets()
	if err != nil || len(targets["worker"]) != 2 || targets["worker"]["first"] || targets["worker"]["second"] {
		t.Fatal("input change lost history or kept confirmation", targets, err)
	}
	for _, bad := range []string{`{}`, `{"worker":{}}`, `{"worker":{"first":true}}`, `{"worker":{"first":true,"second":true,"foreign":false}}`, `{"worker":{"first":null,"second":false}}`} {
		if _, err := db.Exec("UPDATE placements SET stego_cleanup_targets=$1 WHERE id='moving'", bad); err == nil {
			t.Fatal("invalid target history accepted", bad)
		}
	}
	list, err := store.List(ctx, "Placement", "", "", contract.ListOptions{Page: 1, Size: 10, IncludeDeleted: true, Fields: []string{"id"}})
	if err != nil {
		t.Fatal(err)
	}
	sparse := list.Items.([]Placement)
	if len(sparse) != 1 {
		t.Fatal("sparse cleanup list lost its resource")
	}
	if targets, err := sparse[0].CleanupTargets(); err != nil || len(targets["worker"]) != 2 {
		t.Fatal("sparse list lost target metadata", targets, err)
	}
	invalid := row
	invalid.CleanupState = []byte(`{"worker":true,"identity":false}`)
	if invalid.CleanupComplete("worker") {
		t.Fatal("invalid target aggregate proved completion")
	}
	if placement(t, store, "moving").ResourceVersion != row.ResourceVersion {
		t.Fatal("rejected target mutation changed revision")
	}
}

func TestTargetCleanupAndEventRollbackTogether(t *testing.T) {
	store, db := database(t, true)
	ctx := context.Background()
	if err := store.Create(ctx, "Placement", Placement{Meta: Meta{ID: "atomic"}, Target: "target", Name: "job"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "Placement", "atomic"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("ALTER TABLE stego_outbox.messages ADD CONSTRAINT reject_target CHECK(false) NOT VALID"); err != nil {
		t.Fatal(err)
	}
	observe := func() error {
		return store.WithTransaction(ctx, func(ctx context.Context, tx contract.Transaction) error {
			if err := tx.(contract.TargetCleanupWriter).ObserveTargetCleanupIfVersion(ctx, "Placement", "atomic", 2, "worker", "target", true); err != nil {
				return err
			}
			return tx.Notify(message("atomic"))
		})
	}
	if err := observe(); err == nil {
		t.Fatal("failed event committed cleanup")
	}
	if row := placement(t, store, "atomic"); row.ResourceVersion != 2 || row.CleanupComplete("worker") {
		t.Fatal("cleanup survived event rollback")
	}
	if _, err := db.Exec("ALTER TABLE stego_outbox.messages DROP CONSTRAINT reject_target"); err != nil {
		t.Fatal(err)
	}
	if err := observe(); err != nil {
		t.Fatal(err)
	}
	if !placement(t, store, "atomic").CleanupComplete("worker") || count(t, db, "stego_outbox.messages") != 1 {
		t.Fatal("target observation and event did not commit")
	}
}

func TestTargetHistoryHasABoundWithoutDiscardingTargets(t *testing.T) {
	store, db := database(t, true)
	ctx := context.Background()
	if err := store.Create(ctx, "Placement", Placement{Meta: Meta{ID: "bounded"}, Target: "target-0", Name: "job"}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 128; i++ {
		if _, err := db.Exec("UPDATE placements SET target=$1 WHERE id='bounded'", fmt.Sprintf("target-%d", i)); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, err := db.Exec("UPDATE placements SET target='overflow' WHERE id='bounded'"); err == nil {
		t.Fatal("unbounded target history accepted")
	}
	row := placement(t, store, "bounded")
	targets, err := row.CleanupTargets()
	if err != nil || len(targets["worker"]) != 128 || row.Target != "target-127" || row.ResourceVersion != 128 {
		t.Fatal("target limit discarded state", row.ResourceVersion, len(targets["worker"]), err)
	}
}

//go:embed target_removed.sql
var removedTargetContract string

//go:embed target_changed.sql
var changedTargetContract string

//go:embed target_same_mapping.sql
var sameMappingContract string

func TestTargetMigrationDoesNotDiscardOrInventHistory(t *testing.T) {
	store, db := database(t, false)
	ctx := context.Background()
	if err := store.Create(ctx, "Placement", Placement{Meta: Meta{ID: "migration"}, Target: "first", Name: "job"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE placements SET target='second' WHERE id='migration'"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "Placement", "migration"); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveTargetCleanupIfVersion(ctx, "Placement", "migration", 3, "worker", "first", true); err != nil {
		t.Fatal(err)
	}
	before := placement(t, store, "migration")
	for _, statement := range []string{removedTargetContract, changedTargetContract} {
		err := store.db.Transaction(func(tx *gorm.DB) error { return tx.Exec(statement).Error })
		if err == nil || !strings.Contains(err.Error(), "cleanup target") {
			t.Fatal("target contract change was accepted", err)
		}
		if _, err := NewStore(store.db); err != nil {
			t.Fatal("failed migration changed the trigger", err)
		}
		if after := placement(t, store, "migration"); after.ResourceVersion != before.ResourceVersion || string(after.CleanupTargetState) != string(before.CleanupTargetState) {
			t.Fatal("failed migration changed history")
		}
	}
	if err := store.db.Transaction(func(tx *gorm.DB) error { return tx.Exec(sameMappingContract).Error }); err != nil {
		t.Fatal(err)
	}
	var state []byte
	var version int64
	if err := db.QueryRow("SELECT stego_cleanup_targets,stego_revision FROM placements WHERE id='migration'").Scan(&state, &version); err != nil {
		t.Fatal(err)
	}
	var targets map[string]map[string]bool
	if json.Unmarshal(state, &targets) != nil || len(targets["worker"]) != 2 || targets["worker"]["first"] || targets["worker"]["second"] || version != before.ResourceVersion+1 {
		t.Fatal("new inputs discarded history or kept old evidence", string(state), version)
	}
	if _, err := NewStore(store.db); err == nil {
		t.Fatal("old application accepted the changed target contract")
	}
	if err := store.db.Transaction(func(tx *gorm.DB) error { return tx.Exec(sameMappingContract).Error }); err != nil {
		t.Fatal(err)
	}
	var repeated int64
	if err := db.QueryRow("SELECT stego_revision FROM placements WHERE id='migration'").Scan(&repeated); err != nil || repeated != version {
		t.Fatal("repeat contract changed revision", repeated, err)
	}
}

func TestTargetUpgradeRejectsUnrecordedEarlierTargets(t *testing.T) {
	store, db := database(t, false)
	if err := store.db.Transaction(func(tx *gorm.DB) error { return tx.Exec(removedTargetContract).Error }); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO placements(id,target,name) VALUES('legacy','current','legacy')"); err != nil {
		t.Fatal(err)
	}
	err := Migrate(store.db)
	if err == nil || !strings.Contains(err.Error(), "explicit migration") {
		t.Fatal("current target was treated as complete history", err)
	}
	var state string
	if err := db.QueryRow("SELECT stego_cleanup_targets::text FROM placements WHERE id='legacy'").Scan(&state); err != nil || state != "{}" {
		t.Fatal("failed history upgrade changed state", state, err)
	}
}

func BenchmarkTargetCleanupObservation(b *testing.B) {
	for _, targets := range []int{1, 32, 128} {
		b.Run(fmt.Sprintf("targets_%d", targets), func(b *testing.B) {
			store, db := database(b, true)
			ctx := context.Background()
			if err := store.Create(ctx, "Placement", Placement{Meta: Meta{ID: "measured"}, Target: "target-0", Name: "job"}); err != nil {
				b.Fatal(err)
			}
			for i := 1; i < targets; i++ {
				if _, err := db.Exec("UPDATE placements SET target=$1 WHERE id='measured'", fmt.Sprintf("target-%d", i)); err != nil {
					b.Fatal(err)
				}
			}
			if err := store.Delete(ctx, "Placement", "measured"); err != nil {
				b.Fatal(err)
			}
			revision := int64(targets + 1)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := store.ObserveTargetCleanupIfVersion(ctx, "Placement", "measured", revision, "worker", "target-0", i%2 == 0); err != nil {
					b.Fatal(err)
				}
				revision++
			}
			b.StopTimer()
		})
	}
}

func TestOpaqueTargetValuesStayBoundParameters(t *testing.T) {
	store, _ := database(t, true)
	ctx := context.Background()
	target := "region ' ? ☃ \\ [target]"
	if err := store.Create(ctx, "Placement", Placement{Meta: Meta{ID: "opaque"}, Target: target, Name: "job"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "Placement", "opaque"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", strings.Repeat("a", 257), string([]byte{0xff}), "region\x00bad"} {
		if err := store.ObserveTargetCleanupIfVersion(ctx, "Placement", "opaque", 2, "worker", bad, true); err == nil {
			t.Fatal("invalid target accepted")
		}
	}
	if err := store.ObserveTargetCleanupIfVersion(ctx, "Placement", "opaque", 2, "worker", target, true); err != nil {
		t.Fatal(err)
	}
	if row := placement(t, store, "opaque"); !row.CleanupComplete("worker") {
		t.Fatal("opaque target did not complete")
	}
}
