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
