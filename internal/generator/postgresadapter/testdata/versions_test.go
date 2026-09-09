package storage

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	contract "example.com/transaction-test/contracts/storage"
	"gorm.io/gorm"
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

func TestGenerationsAndIndependentObservationGroups(t *testing.T) {
	store, db := database(t, false)
	ctx := context.Background()
	forged := "forged"
	if err := store.Create(ctx, "Record", Record{Meta: Meta{ID: "observed"}, Name: "desired", Health: &forged}); err != nil {
		t.Fatal(err)
	}
	row := versionRecord(t, store, "observed")
	if row.ResourceGeneration != 1 || row.Health != nil || row.ObservedGeneration("health") != 0 {
		t.Fatal("creation accepted an observation", row)
	}
	if err := store.ObserveIfVersion(ctx, "Record", row.ID, row.ResourceVersion, "health", map[string]any{"health": "Healthy"}); err != nil {
		t.Fatal(err)
	}
	row = versionRecord(t, store, row.ID)
	if row.ObservedGeneration("health") != 1 || row.ObservedGeneration("identity") != 0 || row.ResourceGeneration != 1 || row.Health == nil || *row.Health != "Healthy" {
		t.Fatal("observation changed the wrong generation", row)
	}
	observed := row.ResourceVersion
	row.Name = "new desired"
	row.Health = &forged
	if err := store.Replace(ctx, "Record", row.ID, row); err != nil {
		t.Fatal(err)
	}
	row = versionRecord(t, store, row.ID)
	if row.ResourceGeneration != 2 || row.ObservedGeneration("health") != 1 || *row.Health != "Healthy" {
		t.Fatal("desired write changed an observation or missed a generation", row)
	}
	if err := store.ObserveIfVersion(ctx, "Record", row.ID, observed, "health", map[string]any{"health": "Healthy"}); !errors.Is(err, contract.ErrVersionConflict) {
		t.Fatal("old observation accepted", err)
	}
	// A repeated value can confirm the new generation after fresh external work.
	if err := store.ObserveIfVersion(ctx, "Record", row.ID, row.ResourceVersion, "health", map[string]any{"health": "Healthy"}); err != nil {
		t.Fatal(err)
	}
	row = versionRecord(t, store, row.ID)
	if row.ResourceGeneration != 2 || row.ObservedGeneration("health") != 2 || row.ObservedGeneration("identity") != 0 {
		t.Fatal("confirmation marked another group current")
	}
	if err := store.ObserveIfVersion(ctx, "Record", row.ID, row.ResourceVersion, "identity", map[string]any{"identity": "Ready"}); err != nil {
		t.Fatal(err)
	}
	row = versionRecord(t, store, row.ID)
	if row.ResourceGeneration != 2 || row.ObservedGeneration("identity") != 2 || row.ObservedGeneration("health") != 2 {
		t.Fatal("independent observation was lost")
	}
	if _, err := db.Exec("UPDATE records SET value=5, stego_generation=900 WHERE id='observed'"); err != nil {
		t.Fatal(err)
	}
	row = versionRecord(t, store, row.ID)
	if row.ResourceGeneration != 2 {
		t.Fatal("auxiliary write changed generation", row.ResourceGeneration)
	}
	if _, err := db.Exec("UPDATE records SET name='NEW DESIRED' WHERE id='observed'"); err != nil {
		t.Fatal(err)
	}
	row = versionRecord(t, store, row.ID)
	if row.ResourceGeneration != 3 || row.ObservedGeneration("health") != 2 {
		t.Fatal("raw desired change did not invalidate observation")
	}
	if err := store.Delete(ctx, "Record", row.ID); err != nil {
		t.Fatal(err)
	}
	var generation int64
	if err := db.QueryRow("SELECT stego_generation FROM records WHERE id='observed'").Scan(&generation); err != nil || generation != 4 {
		t.Fatal("deletion did not change desired intent", generation, err)
	}
}

func TestObservationRejectsOtherFieldsAndRollsBackMetadata(t *testing.T) {
	store, _ := database(t, true)
	ctx := context.Background()
	if err := store.Create(ctx, "Record", Record{Meta: Meta{ID: "group"}, Name: "desired"}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []struct {
		group  string
		values map[string]any
	}{
		{"health", nil}, {"health", map[string]any{"name": "bad"}}, {"health", map[string]any{"health": "Healthy", "identity": "bad"}}, {"other", map[string]any{"health": "Healthy"}},
	} {
		if err := store.ObserveIfVersion(ctx, "Record", "group", 1, bad.group, bad.values); err == nil {
			t.Fatal("invalid observation accepted", bad.group)
		}
	}
	rejected := errors.New("event failed")
	err := store.WithTransaction(ctx, func(ctx context.Context, tx contract.Transaction) error {
		if err := tx.(contract.ObservationWriter).ObserveIfVersion(ctx, "Record", "group", 1, "health", map[string]any{"health": "Healthy"}); err != nil {
			return err
		}
		return rejected
	})
	if !errors.Is(err, rejected) {
		t.Fatal(err)
	}
	row := versionRecord(t, store, "group")
	if row.Health != nil || row.ResourceVersion != 1 || row.ResourceGeneration != 1 || row.ObservedGeneration("health") != 0 {
		t.Fatal("rollback retained an observation", row)
	}
}

func TestChangedGenerationContractInvalidatesObservations(t *testing.T) {
	store, db := database(t, false)
	ctx := context.Background()
	if err := store.Create(ctx, "Record", Record{Meta: Meta{ID: "upgrade"}, Name: "desired"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveIfVersion(ctx, "Record", "upgrade", 1, "health", map[string]any{"health": "Healthy"}); err != nil {
		t.Fatal(err)
	}
	var definition string
	if err := db.QueryRow("SELECT pg_get_functiondef(tgfoid) FROM pg_trigger WHERE tgrelid='records'::regclass AND tgname='stego_resource_revision'").Scan(&definition); err != nil {
		t.Fatal(err)
	}
	definition = strings.Replace(definition, "generation contract", "previous generation contract", 1)
	if _, err := db.Exec(definition); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(store.db); err == nil {
		t.Fatal("old contract accepted at startup")
	}
	if err := Migrate(store.db); err != nil {
		t.Fatal(err)
	}
	row := versionRecord(t, store, "upgrade")
	if row.ResourceGeneration != 2 || row.ResourceVersion != 3 || row.ObservedGeneration("health") != 0 {
		t.Fatal("contract upgrade kept old observations current", row)
	}
	if err := Migrate(store.db); err != nil {
		t.Fatal(err)
	}
	repeated := versionRecord(t, store, "upgrade")
	if repeated.ResourceGeneration != row.ResourceGeneration || repeated.ResourceVersion != row.ResourceVersion {
		t.Fatal("repeated migration changed generation")
	}
	if err := store.ObserveIfVersion(ctx, "Record", "upgrade", 2, "health", map[string]any{"health": "Healthy"}); !errors.Is(err, contract.ErrVersionConflict) {
		t.Fatal("old token accepted after contract change", err)
	}
}

func TestObservationPreservesTypedBinaryAndTimeValues(t *testing.T) {
	store, _ := database(t, false)
	ctx := context.Background()
	if err := store.Create(ctx, "Record", Record{Meta: Meta{ID: "typed"}, Name: "desired"}); err != nil {
		t.Fatal(err)
	}
	certificate := []byte{0, 1, 2, 254, 255}
	checked := time.Date(2026, 9, 9, 12, 0, 0, 123456000, time.UTC)
	if err := store.ObserveIfVersion(ctx, "Record", "typed", 1, "evidence", map[string]any{"certificate": certificate, "checked_at": checked}); err != nil {
		t.Fatal(err)
	}
	row := versionRecord(t, store, "typed")
	if !bytes.Equal(row.Certificate, certificate) || row.CheckedAt == nil || !row.CheckedAt.Equal(checked) || row.ObservedGeneration("evidence") != 1 {
		t.Fatal("typed observation changed its value", row)
	}
}

func TestDesiredJSONUsesStructuralEqualityAndDistinguishesNull(t *testing.T) {
	store, db := database(t, false)
	ctx := context.Background()
	if err := store.Create(ctx, "Record", Record{Meta: Meta{ID: "json"}, Name: "desired", DesiredConfig: []byte(`{"a":1,"b":2}`)}); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		value      string
		generation int64
	}{
		{`'{"b":2.0,"a":1}'::jsonb`, 1}, {`'null'::jsonb`, 2}, {`NULL`, 3},
	} {
		if _, err := db.Exec("UPDATE records SET desired_config=" + step.value + " WHERE id='json'"); err != nil {
			t.Fatal(err)
		}
		if row := versionRecord(t, store, "json"); row.ResourceGeneration != step.generation {
			t.Fatal("wrong JSON generation", row.ResourceGeneration, step.generation)
		}
	}
}

func TestFailedMigrationRollsBackSchemaAndObservationContract(t *testing.T) {
	store, db := database(t, false)
	ctx := context.Background()
	if err := store.Create(ctx, "Record", Record{Meta: Meta{ID: "atomic-upgrade"}, Name: "desired"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveIfVersion(ctx, "Record", "atomic-upgrade", 1, "health", map[string]any{"health": "Healthy"}); err != nil {
		t.Fatal(err)
	}
	var definition string
	if err := db.QueryRow("SELECT pg_get_functiondef(tgfoid) FROM pg_trigger WHERE tgrelid='records'::regclass AND tgname='stego_resource_revision'").Scan(&definition); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(strings.Replace(definition, "generation contract", "previous generation contract", 1)); err != nil {
		t.Fatal(err)
	}
	original := migrations
	defer func() { migrations = original }()
	failed := errors.New("later migration failed")
	migrations = append(append([]Migration{}, original...), Migration{Name: "probe", Func: func(db *gorm.DB) error {
		return db.Exec("ALTER TABLE records ADD COLUMN migration_probe integer").Error
	}}, Migration{Name: "reject", Func: func(*gorm.DB) error { return failed }})
	if err := Migrate(store.db); !errors.Is(err, failed) {
		t.Fatal(err)
	}
	var columns int
	if err := db.QueryRow("SELECT count(*) FROM pg_attribute WHERE attrelid='records'::regclass AND attname='migration_probe' AND NOT attisdropped").Scan(&columns); err != nil || columns != 0 {
		t.Fatal("failed migration changed schema", columns, err)
	}
	row := versionRecord(t, store, "atomic-upgrade")
	if row.ResourceVersion != 2 || row.ResourceGeneration != 1 || row.ObservedGeneration("health") != 1 {
		t.Fatal("failed migration changed generation or observations", row)
	}
	if _, err := NewStore(store.db); err == nil {
		t.Fatal("failed migration installed the new contract")
	}
	migrations = original
	if err := Migrate(store.db); err != nil {
		t.Fatal(err)
	}
	if row := versionRecord(t, store, "atomic-upgrade"); row.ResourceVersion != 3 || row.ResourceGeneration != 2 || row.ObservedGeneration("health") != 0 {
		t.Fatal("successful migration did not invalidate observations")
	}
}

func TestCurrentObservationsControlListsAndPredicates(t *testing.T) {
	store, db := database(t, false)
	ctx := context.Background()
	const pending = "Pending \\ ' ?"
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("SET standard_conforming_strings=off"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a-current", "z-stale"} {
		if err := store.Create(ctx, "Record", Record{Meta: Meta{ID: id}, Name: id}); err != nil {
			t.Fatal(err)
		}
		if err := store.ObserveIfVersion(ctx, "Record", id, 1, "health", map[string]any{"health": "Healthy"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("UPDATE records SET name='new-intent' WHERE id='z-stale'"); err != nil {
		t.Fatal(err)
	}
	raw := versionRecord(t, store, "z-stale")
	projected := raw.CurrentObservations()
	if raw.Health == nil || *raw.Health != "Healthy" || projected.Health == nil || *projected.Health != pending || projected.Identity != nil {
		t.Fatal("current projection changed retained data", raw, projected)
	}
	related := contract.RelatedFilter{Entity: "Record", ForeignField: "id", Values: map[string][]string{"health": {pending}}}
	for _, test := range []struct {
		name, scope, value, id string
		options                contract.ListOptions
	}{
		{name: "scope", scope: "health", value: pending, id: "z-stale"},
		{name: "implicit", id: "z-stale", options: contract.ListOptions{ImplicitFilters: map[string]string{"health": pending}}},
		{name: "filter", id: "z-stale", options: contract.ListOptions{Filter: &contract.RowFilter{Field: "health", Values: []string{pending}}}},
		{name: "text", id: "z-stale", options: contract.ListOptions{Filter: &contract.RowFilter{Text: &contract.TextMatch{Fields: []string{"health"}, Value: pending}}}},
		{name: "related", id: "z-stale", options: contract.ListOptions{Related: []contract.RelatedFilter{related}}},
		{name: "related tree", id: "z-stale", options: contract.ListOptions{Filter: &contract.RowFilter{Related: &related}}},
		{name: "confirmed", id: "a-current", options: contract.ListOptions{ImplicitFilters: map[string]string{"health": "Healthy"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.options.Page, test.options.Size = 1, 10
			result, err := store.List(ctx, "Record", test.scope, test.value, test.options)
			if err != nil {
				t.Fatal(err)
			}
			rows := result.Items.([]Record)
			if result.Total != 1 || len(rows) != 1 || rows[0].ID != test.id {
				t.Fatal("predicate used stale observation", result)
			}
			test.options.CountOnly = true
			count, err := store.List(ctx, "Record", test.scope, test.value, test.options)
			if err != nil || count.Total != 1 {
				t.Fatal("count differs from list", count, err)
			}
		})
	}

	for _, id := range []string{"a-current", "z-stale"} {
		sparse, err := store.List(ctx, "Record", "id", id, contract.ListOptions{Page: 1, Size: 1, Fields: []string{"health"}})
		if err != nil {
			t.Fatal(err)
		}
		row := sparse.Items.([]Record)[0]
		projected := row.CurrentObservations()
		if row.ResourceVersion < 1 || row.ResourceGeneration < 1 || row.Health == nil || projected.Health == nil || *row.Health != *projected.Health {
			t.Fatal("sparse list lost observation metadata", row)
		}
	}
	result, err := store.List(ctx, "Record", "", "", contract.ListOptions{Page: 1, Size: 1, OrderBy: []contract.OrderByField{{Field: "health", Direction: "desc"}}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 2 || result.Items.([]Record)[0].ID != "z-stale" {
		t.Fatal("ordering used stale observation", result)
	}
	if err := store.Delete(ctx, "Record", "z-stale"); err != nil {
		t.Fatal(err)
	}
	result, err = store.List(ctx, "Record", "health", pending, contract.ListOptions{Page: 1, Size: 10})
	if err != nil || result.Total != 0 {
		t.Fatal("projection exposed deleted row", result, err)
	}
}

func TestCurrentObservationMetadataFailsClosedPerGroup(t *testing.T) {
	healthy, identity := "Healthy", "bound"
	for _, metadata := range []string{`{}`, `{"health":"2"}`, `{"health":2.0}`, `{"health":3}`, `{"health":-1}`} {
		row := Record{Health: &healthy, ResourceGeneration: 2, ObservedGenerations: []byte(metadata)}
		if row.ObservedGeneration("health") != 0 || *row.CurrentObservations().Health == healthy {
			t.Fatal("invalid observation appears current", metadata)
		}
	}
	row := Record{Health: &healthy, Identity: &identity, ResourceGeneration: 2, ObservedGenerations: []byte(`{"health":2,"identity":"invalid"}`)}
	current := row.CurrentObservations()
	if current.Health == nil || *current.Health != healthy || current.Identity != nil {
		t.Fatal("group metadata is not independent", current)
	}
}
