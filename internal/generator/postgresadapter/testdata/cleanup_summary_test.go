package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	contract "example.com/transaction-test/contracts/storage"
)

func TestCleanupSummaryTracksOwnersAndReopenedWork(t *testing.T) {
	store, db := database(t, true)
	// Scope remains exact even when ordinary database equality ignores case.
	if _, err := db.Exec(`CREATE COLLATION cleanup_ci (provider=icu, locale='und-u-ks-level2', deterministic=false)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE records ALTER COLUMN name TYPE text COLLATE cleanup_ci`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	read := func(owner, field, value string) contract.CleanupSummary {
		t.Helper()
		result, err := store.ReadCleanupSummary(ctx, "Record", owner, "", field, value)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	empty := read("workload", "", "")
	if empty.Pending != 0 || empty.OldestPending != nil || empty.ObservedAt.IsZero() {
		t.Fatal(empty)
	}
	for _, name := range []string{"first", "second", "quoted' OR true --"} {
		if err := store.Create(ctx, "Record", Record{Meta: Meta{ID: name}, Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	if read("identity", "", "").Pending != 0 {
		t.Fatal("live resources counted as deleted work")
	}
	if err := store.Delete(ctx, "Record", "first"); err != nil {
		t.Fatal(err)
	}
	row := cleanupRecord(t, store, "first")
	first := read("workload", "", "")
	if first.Pending != 1 || first.OldestPending == nil || !first.OldestPending.Equal(row.DeletedAt.Time) {
		t.Fatal(first, row.DeletedAt)
	}
	if err := store.ObserveCleanupIfVersion(ctx, "Record", "first", row.ResourceVersion, "workload", true); err != nil {
		t.Fatal(err)
	}
	if read("workload", "", "").Pending != 0 || read("identity", "", "").Pending != 1 {
		t.Fatal("one owner changed another summary")
	}
	row = cleanupRecord(t, store, "first")
	if err := store.ObserveCleanupIfVersion(ctx, "Record", "first", row.ResourceVersion, "workload", false); err != nil {
		t.Fatal(err)
	}
	reopened := read("workload", "", "")
	if reopened.Pending != 1 || !reopened.OldestPending.Equal(*first.OldestPending) {
		t.Fatal("reopened cleanup lost original deletion time", reopened)
	}
	for _, name := range []string{"second", "quoted' OR true --"} {
		if err := store.Delete(ctx, "Record", name); err != nil {
			t.Fatal(err)
		}
	}
	if read("workload", "", "").Pending != 3 || read("workload", "name", "second").Pending != 1 || read("workload", "name", "quoted' OR true --").Pending != 1 {
		t.Fatal("summary scope was not applied")
	}
	if read("workload", "name", "FIRST").Pending != 0 || read("workload", "name", "first").Pending != 1 {
		t.Fatal("scope used non-exact equality")
	}
	before := cleanupRecord(t, store, "first").ResourceVersion
	read("workload", "", "")
	if cleanupRecord(t, store, "first").ResourceVersion != before {
		t.Fatal("summary changed resource revision")
	}
	rollback := errors.New("rollback summary")
	err := store.WithTransaction(ctx, func(ctx context.Context, tx contract.Transaction) error {
		row, err := tx.(contract.RetainedReader).GetRetained(ctx, "Record", "first")
		if err != nil {
			return err
		}
		if err := tx.(contract.CleanupWriter).ObserveCleanupIfVersion(ctx, "Record", "first", row.(Record).ResourceVersion, "workload", true); err != nil {
			return err
		}
		current, err := tx.(contract.CleanupSummaryReader).ReadCleanupSummary(ctx, "Record", "workload", "", "", "")
		if err != nil {
			return err
		}
		if current.Pending != 2 {
			t.Fatal("transaction summary ignored its write", current)
		}
		return rollback
	})
	if !errors.Is(err, rollback) || read("workload", "", "").Pending != 3 {
		t.Fatal("summary transaction did not roll back", err)
	}
}

func TestCleanupSummaryUsesRetainedTargetScope(t *testing.T) {
	store, db := database(t, true)
	ctx := context.Background()
	if err := store.Create(ctx, "Placement", Placement{Meta: Meta{ID: "moving"}, Target: "old' target", Name: "placement"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE placements SET target='new' WHERE id='moving'"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "Placement", "moving"); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"old' target", "new", "unknown"} {
		summary, err := store.ReadCleanupSummary(ctx, "Placement", "worker", target, "", "")
		expected := int64(1)
		if target == "unknown" {
			expected = 0
		}
		if err != nil || summary.Pending != expected {
			t.Fatal(target, summary, err)
		}
	}
	row := placement(t, store, "moving")
	if err := store.ObserveTargetCleanupIfVersion(ctx, "Placement", "moving", row.ResourceVersion, "worker", "old' target", true); err != nil {
		t.Fatal(err)
	}
	summary, err := store.ReadCleanupSummary(ctx, "Placement", "worker", "old' target", "", "")
	if err != nil || summary.Pending != 0 {
		t.Fatal(summary, err)
	}
	summary, err = store.ReadCleanupSummary(ctx, "Placement", "worker", "new", "", "")
	if err != nil || summary.Pending != 1 {
		t.Fatal(summary, err)
	}
}
func TestCleanupSummaryRejectsInvalidInputs(t *testing.T) {
	store, _ := database(t, true)
	for _, args := range [][5]string{
		{"Unknown", "workload", "", "", ""}, {"Measurement", "workload", "", "", ""},
		{"Record", "unknown", "", "", ""}, {"Record", "workload", "target", "", ""},
		{"Placement", "worker", "", "", ""}, {"Placement", "worker", strings.Repeat("x", 257), "", ""},
		{"Record", "workload", "", "name", ""}, {"Record", "workload", "", "", "name"},
		{"Record", "workload", "", "value", "1"}, {"Record", "workload", "", "health", "Pending"},
		{"Record", "workload", "", "name' OR true --", "first"}, {"Record", "workload", "", "name", "bad\x00value"},
	} {
		if _, err := store.ReadCleanupSummary(context.Background(), args[0], args[1], args[2], args[3], args[4]); !errors.Is(err, contract.ErrCleanupSummary) {
			t.Fatal(args, err)
		}
	}
	if _, err := store.ReadCleanupSummary(nil, "Record", "workload", "", "", ""); !errors.Is(err, contract.ErrCleanupSummary) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.ReadCleanupSummary(ctx, "Record", "workload", "", "", ""); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func BenchmarkCleanupSummary(b *testing.B) {
	for _, size := range []int{1000, 10000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			store, db := database(b, true)
			if _, err := db.Exec("INSERT INTO records(id,name,value,created_time,updated_time,deleted_at) SELECT 'summary-'||n, 'name-'||n, 0, now(), now(), now()-interval '1 hour' FROM generate_series(1,$1) n", size); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				summary, err := store.ReadCleanupSummary(context.Background(), "Record", "workload", "", "", "")
				if err != nil || summary.Pending != int64(size) {
					b.Fatal(summary, err)
				}
			}
		})
	}
}

func TestCleanupSummaryCombinesExactScopes(t *testing.T) {
	store, db := database(t, true)
	ctx := context.Background()
	if _, err := db.Exec(`CREATE COLLATION cleanup_scope_ci (provider=icu, locale='und-u-ks-level2', deterministic=false);
ALTER TABLE placements ALTER COLUMN name TYPE text COLLATE cleanup_scope_ci`); err != nil {
		t.Fatal(err)
	}
	for i, fields := range [][2]string{{"north", "alpha"}, {"south", "alpha"}, {"north", "beta"}, {"north", "quoted' OR true --"}, {"north", "ALPHA"}, {"south", "quoted' OR true --"}} {
		id := fmt.Sprint(i)
		if err := store.Create(ctx, "Placement", Placement{Meta: Meta{ID: id}, Target: fields[0], Name: fields[1]}); err != nil {
			t.Fatal(err)
		}
		if err := store.Delete(ctx, "Placement", id); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name    string
		filters []contract.CleanupScope
		pending int64
	}{
		{"all", nil, 6},
		{"target", []contract.CleanupScope{{Field: "target", Value: "north"}}, 4},
		{"name", []contract.CleanupScope{{Field: "name", Value: "alpha"}}, 2},
		{"intersection", []contract.CleanupScope{{Field: "target", Value: "north"}, {Field: "name", Value: "alpha"}}, 1},
		{"reverse-order", []contract.CleanupScope{{Field: "name", Value: "alpha"}, {Field: "target", Value: "north"}}, 1},
		{"quoted-value", []contract.CleanupScope{{Field: "target", Value: "north"}, {Field: "name", Value: "quoted' OR true --"}}, 1},
		{"empty-result", []contract.CleanupScope{{Field: "target", Value: "south"}, {Field: "name", Value: "beta"}}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := store.ReadScopedCleanupSummary(ctx, "Placement", "identity", "", test.filters...)
			if err != nil || result.Pending != test.pending || result.ObservedAt.IsZero() || (result.OldestPending == nil) != (test.pending == 0) {
				t.Fatal("combined summary scope", result, err)
			}
		})
	}
	row := placement(t, store, "0")
	if err := store.ObserveTargetCleanupIfVersion(ctx, "Placement", row.ID, row.ResourceVersion, "worker", "north", true); err != nil {
		t.Fatal(err)
	}
	filters := []contract.CleanupScope{{Field: "target", Value: "north"}, {Field: "name", Value: "alpha"}}
	for owner, pending := range map[string]int64{"identity": 1, "worker": 0} {
		target := ""
		if owner == "worker" {
			target = "north"
		}
		result, err := store.ReadScopedCleanupSummary(ctx, "Placement", owner, target, filters...)
		if err != nil || result.Pending != pending {
			t.Fatal("combined scope lost owner or retained target", result, err)
		}
	}
}

func TestCleanupSummaryRejectsInvalidCombinedScopes(t *testing.T) {
	store, _ := database(t, true)
	for _, filters := range [][]contract.CleanupScope{
		{{Field: "name", Value: "first"}, {Field: "name", Value: "second"}},
		{{Field: "", Value: "first"}}, {{Field: "name", Value: ""}},
		{{Field: "value", Value: "1"}}, {{Field: "health", Value: "Pending"}},
		{{Field: "name' OR true --", Value: "first"}},
		{{Field: "name", Value: "bad\x00value"}}, {{Field: "name", Value: "\xff"}},
		{{Field: "name", Value: strings.Repeat("x", 257)}},
		{{Field: strings.Repeat("x", 257), Value: "first"}},
		make([]contract.CleanupScope, 9),
	} {
		if _, err := store.ReadScopedCleanupSummary(context.Background(), "Record", "workload", "", filters...); !errors.Is(err, contract.ErrCleanupSummary) {
			t.Fatal("invalid combined scope accepted", err)
		}
	}
	if _, err := store.ReadScopedCleanupSummary(nil, "Record", "workload", ""); !errors.Is(err, contract.ErrCleanupSummary) {
		t.Fatal(err)
	}
	var absent *Store
	if _, err := absent.ReadScopedCleanupSummary(context.Background(), "Record", "workload", ""); !errors.Is(err, contract.ErrCleanupSummary) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.ReadScopedCleanupSummary(ctx, "Record", "workload", "", contract.CleanupScope{Field: "name", Value: "first"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestCleanupReferencesKeepAllUnfinishedTargets(t *testing.T) {
	store, db := database(t, true)
	ctx := context.Background()
	parent := "parent' OR true --"
	other := "other"
	for _, id := range []string{parent, other} {
		if err := store.Create(ctx, "Record", Record{Meta: Meta{ID: id}, Name: id}); err != nil {
			t.Fatal(err)
		}
	}
	read := func(id, owner string) bool {
		t.Helper()
		pending, err := store.HasUnfinishedReferences(ctx, contract.CleanupReference{Entity: "Placement", Field: "parent_id", ID: id, Owner: owner})
		if err != nil {
			t.Fatal(err)
		}
		return pending
	}
	if read(parent, "worker") {
		t.Fatal("absent child blocked deletion")
	}
	if err := store.Create(ctx, "Placement", Placement{Meta: Meta{ID: "child"}, Name: "child", ParentID: &parent, Target: "old"}); err != nil {
		t.Fatal(err)
	}
	if !read(parent, "worker") || read(other, "worker") {
		t.Fatal("live reference scope was lost")
	}
	if _, err := db.Exec("UPDATE placements SET target='new' WHERE id='child'"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "Placement", "child"); err != nil {
		t.Fatal(err)
	}
	if !read(parent, "worker") {
		t.Fatal("unobserved deletion did not block its parent")
	}
	observe := func(target string, complete bool) {
		t.Helper()
		row := placement(t, store, "child")
		if err := store.ObserveTargetCleanupIfVersion(ctx, "Placement", row.ID, row.ResourceVersion, "worker", target, complete); err != nil {
			t.Fatal(err)
		}
	}
	observe("new", true)
	if !read(parent, "worker") {
		t.Fatal("unfinished former target was ignored")
	}
	observe("old", true)
	if read(parent, "worker") || !read(parent, "identity") {
		t.Fatal("cleanup owner isolation was lost")
	}
	observe("old", false)
	if !read(parent, "worker") {
		t.Fatal("reopened work did not block its parent")
	}
	rollback := errors.New("rollback dependency observation")
	err := store.WithTransaction(ctx, func(ctx context.Context, tx contract.Transaction) error {
		value, err := tx.(contract.RetainedReader).GetRetained(ctx, "Placement", "child")
		if err != nil {
			return err
		}
		row := value.(Placement)
		if err := tx.(contract.TargetCleanupWriter).ObserveTargetCleanupIfVersion(ctx, "Placement", row.ID, row.ResourceVersion, "worker", "old", true); err != nil {
			return err
		}
		pending, err := tx.(contract.CleanupReferenceReader).HasUnfinishedReferences(ctx, contract.CleanupReference{Entity: "Placement", Field: "parent_id", ID: parent, Owner: "worker"})
		if err != nil {
			return err
		}
		if pending {
			t.Fatal("transaction ignored its cleanup observation")
		}
		return rollback
	})
	if !errors.Is(err, rollback) || !read(parent, "worker") {
		t.Fatal("dependency transaction did not roll back", err)
	}
	observe("old", true)
	if read(parent, "worker") {
		t.Fatal("finished child kept blocking its parent")
	}
	if err := store.Create(ctx, "Placement", Placement{Meta: Meta{ID: "unrelated"}, Name: "unrelated", ParentID: &other, Target: "old"}); err != nil {
		t.Fatal(err)
	}
	if read(parent, "worker") || !read(other, "worker") {
		t.Fatal("unrelated child changed the dependency result")
	}
}

func TestCleanupReferencesRejectInvalidQueries(t *testing.T) {
	store, _ := database(t, true)
	valid := contract.CleanupReference{Entity: "Placement", Field: "parent_id", ID: "parent", Owner: "worker"}
	for _, change := range []func(*contract.CleanupReference){
		func(r *contract.CleanupReference) { r.Entity = "unknown" },
		func(r *contract.CleanupReference) { r.Entity = "Measurement" },
		func(r *contract.CleanupReference) { r.Field = "name" },
		func(r *contract.CleanupReference) { r.Field = "parent_id' OR true --" },
		func(r *contract.CleanupReference) { r.ID = "" },
		func(r *contract.CleanupReference) { r.ID = "bad\x00value" },
		func(r *contract.CleanupReference) { r.ID = "\xff" },
		func(r *contract.CleanupReference) { r.ID = strings.Repeat("x", 257) },
		func(r *contract.CleanupReference) { r.Owner = "unknown" },
	} {
		query := valid
		change(&query)
		if _, err := store.HasUnfinishedReferences(context.Background(), query); !errors.Is(err, contract.ErrCleanupReference) {
			t.Fatal("invalid dependency query accepted", err)
		}
	}
	if _, err := store.HasUnfinishedReferences(nil, valid); !errors.Is(err, contract.ErrCleanupReference) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.HasUnfinishedReferences(ctx, valid); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
