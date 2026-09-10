package storage

import (
	"context"
	_ "embed"
	"errors"
	"strings"
	"testing"

	contract "example.com/transaction-test/contracts/storage"
	"gorm.io/gorm"
)

func conditionRecord(t *testing.T, s *Store, id string) Record {
	t.Helper()
	value, err := s.GetRetained(context.Background(), "Record", id)
	if err != nil {
		t.Fatal(err)
	}
	return value.(Record)
}
func conditionWrite(s *Store, id string, revision int64, owner, status, reason string) error {
	return s.WithTransaction(context.Background(), func(ctx context.Context, tx contract.Transaction) error {
		return tx.(contract.ConditionWriter).ObserveConditionsIfVersion(ctx, "Record", id, revision, owner, []contract.ConditionUpdate{{Name: "Ready", Status: status, Reason: reason, Message: "Safe provider result"}})
	})
}
func TestConditionsRetainEvidenceAndSuppressOldGeneration(t *testing.T) {
	s, _ := database(t, false)
	ctx := context.Background()
	row := record("condition-generation")
	row.ID = "conditions"
	if err := s.Create(ctx, "Record", row); err != nil {
		t.Fatal(err)
	}
	current := conditionRecord(t, s, row.ID)
	values, err := current.CurrentConditions()
	if err != nil || values["identity"]["Ready"].Status != "Unknown" {
		t.Fatal(values, err)
	}
	if err := conditionWrite(s, row.ID, current.ResourceVersion, "identity", "False", "ProviderUnavailable"); err != nil {
		t.Fatal(err)
	}
	current = conditionRecord(t, s, row.ID)
	values, err = current.Conditions()
	if err != nil {
		t.Fatal(err)
	}
	failed := values["identity"]["Ready"]
	if !failed.Current || failed.Status != "False" || failed.ObservedGeneration != current.ResourceGeneration || failed.LastTransitionTime.IsZero() {
		t.Fatal(failed)
	}
	if err := conditionWrite(s, row.ID, current.ResourceVersion, "identity", "False", "ProviderDenied"); err != nil {
		t.Fatal(err)
	}
	current = conditionRecord(t, s, row.ID)
	values, err = current.Conditions()
	if err != nil || !values["identity"]["Ready"].LastTransitionTime.Equal(failed.LastTransitionTime) {
		t.Fatal("reason change moved transition time", values, err)
	}
	stale := current.ResourceVersion
	if err := conditionWrite(s, row.ID, current.ResourceVersion, "health", "True", "WorkloadReady"); err != nil {
		t.Fatal(err)
	}
	if err := conditionWrite(s, row.ID, stale, "identity", "True", "IdentityReady"); !errors.Is(err, ErrVersionConflict) {
		t.Fatal("stale condition was accepted", err)
	}
	current = conditionRecord(t, s, row.ID)
	if err := s.ReplaceIfVersion(ctx, "Record", row.ID, current.ResourceVersion, map[string]any{"name": "new-condition-generation", "value": 1}); err != nil {
		t.Fatal(err)
	}
	current = conditionRecord(t, s, row.ID)
	values, err = current.CurrentConditions()
	if err != nil || values["identity"]["Ready"].Status != "Unknown" || values["health"]["Ready"].Current {
		t.Fatal("old generation appeared current", values, err)
	}
	raw, err := current.Conditions()
	if err != nil || raw["identity"]["Ready"].Reason != "ProviderDenied" {
		t.Fatal("old evidence was removed", raw, err)
	}
	if err := conditionWrite(s, row.ID, current.ResourceVersion, "identity", "True", "IdentityReady"); err != nil {
		t.Fatal(err)
	}
	current = conditionRecord(t, s, row.ID)
	values, err = current.Conditions()
	if err != nil || !values["identity"]["Ready"].Current || !values["identity"]["Ready"].LastTransitionTime.After(failed.LastTransitionTime) {
		t.Fatal("new status did not move transition time", values, err)
	}
	if err := s.Delete(ctx, "Record", row.ID); err != nil {
		t.Fatal(err)
	}
	deleted := conditionRecord(t, s, row.ID)
	values, err = deleted.CurrentConditions()
	if err != nil || values["identity"]["Ready"].Current || values["identity"]["Ready"].Status != "Unknown" {
		t.Fatal("deleted resource appeared ready", values, err)
	}
	if err := conditionWrite(s, row.ID, deleted.ResourceVersion, "identity", "True", "IdentityReady"); !errors.Is(err, ErrVersionConflict) {
		t.Fatal("deleted resource accepted condition", err)
	}
}
func TestConditionFailureRollsBackEventsAndResource(t *testing.T) {
	s, db := database(t, false)
	ctx := context.Background()
	row := record("condition-rollback")
	row.ID = "conditions"
	if err := s.Create(ctx, "Record", row); err != nil {
		t.Fatal(err)
	}
	err := s.WithTransaction(ctx, func(ctx context.Context, tx contract.Transaction) error {
		if err := tx.Notify(message("conditions")); err != nil {
			return err
		}
		writer := tx.(contract.ConditionWriter)
		if err := writer.ObserveConditionsIfVersion(ctx, "Record", row.ID, 1, "identity", []contract.ConditionUpdate{{Name: "Ready", Status: "False", Reason: "ProviderUnavailable"}}); err != nil {
			return err
		}
		_ = writer.ObserveConditionsIfVersion(ctx, "Record", row.ID, 1, "identity", []contract.ConditionUpdate{{Name: "Ready", Status: "True", Reason: "IdentityReady"}})
		return nil
	})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatal("ignored stale condition committed", err)
	}
	current := conditionRecord(t, s, row.ID)
	if current.ResourceVersion != 1 {
		t.Fatal("partial condition committed", current.ResourceVersion)
	}
	var messages int64
	if err := db.QueryRow("SELECT count(*) FROM stego_outbox.messages").Scan(&messages); err != nil || messages != 0 {
		t.Fatal("condition event committed alone", messages, err)
	}
	for _, test := range []struct{ owner, status, reason, message string }{{"unknown", "True", "Ready", ""}, {"identity", "true", "Ready", ""}, {"identity", "True", "invalid reason", ""}, {"identity", "True", "Ready", strings.Repeat("x", 1025)}, {"identity", "True", "Ready", "private\nline"}} {
		err := s.WithTransaction(ctx, func(ctx context.Context, tx contract.Transaction) error {
			return tx.(contract.ConditionWriter).ObserveConditionsIfVersion(ctx, "Record", row.ID, 1, test.owner, []contract.ConditionUpdate{{Name: "Ready", Status: test.status, Reason: test.reason, Message: test.message}})
		})
		if !errors.Is(err, ErrCondition) {
			t.Fatal("invalid condition accepted", err)
		}
	}
}
func TestConditionSchemaAndDataAreRequired(t *testing.T) {
	s, db := database(t, false)
	ctx := context.Background()
	row := record("condition-schema")
	row.ID = "conditions"
	if err := s.Create(ctx, "Record", row); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE records SET stego_conditions='null'::jsonb WHERE id='conditions'"); err == nil {
		t.Fatal("invalid condition object accepted")
	}
	if _, err := db.Exec("ALTER TABLE records DROP COLUMN stego_conditions"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(s.db); err == nil {
		t.Fatal("missing condition column accepted")
	}
}

func TestConditionsRejectMalformedStoredEvidence(t *testing.T) {
	for _, data := range []string{
		`{"identity":{"ClientReady":{}}}`,
		`{"identity":{"Ready":{"status":null,"reason":"Ready","message":"","observed_generation":1,"last_transition_time":"2026-09-10T00:00:00Z"}}}`,
		`{"identity":{"Ready":{"status":"True","reason":null,"message":"","observed_generation":1,"last_transition_time":"2026-09-10T00:00:00Z"}}}`,
		`{"identity":{"Ready":{"status":"True","reason":"Ready","message":"","observed_generation":2,"last_transition_time":"2026-09-10T00:00:00Z"}}}`,
	} {
		row := Record{ResourceGeneration: 1, ConditionState: []byte(data)}
		if _, err := row.Conditions(); !errors.Is(err, ErrCondition) {
			t.Fatal("malformed stored condition accepted", err)
		}
	}
}

//go:embed no_conditions.sql
var noConditionsMigration string

//go:embed removed_conditions.sql
var removedConditionsMigration string

func TestConditionUpgradeInvalidatesEarlierObservations(t *testing.T) {
	s, db := database(t, false)
	ctx := context.Background()
	if err := s.db.Transaction(func(tx *gorm.DB) error { return tx.Exec(noConditionsMigration).Error }); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("ALTER TABLE records DROP COLUMN stego_conditions"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO records(id,name,value,created_time,updated_time) VALUES('old','old-condition-schema',1,now(),now())"); err != nil {
		t.Fatal(err)
	}
	if err := s.ObserveIfVersion(ctx, "Record", "old", 1, "health", map[string]any{"health": "Healthy"}); err != nil {
		t.Fatal(err)
	}
	old := conditionRecord(t, s, "old")
	if old.ObservedGeneration("health") != old.ResourceGeneration {
		t.Fatal("old observation was not established")
	}
	if _, err := NewStore(s.db); err == nil {
		t.Fatal("new store accepted the old schema")
	}
	// The deployment contract starts new API connections after schema changes.
	// Drop connections whose prepared queries used the earlier SELECT row shape.
	db.SetMaxIdleConns(0)
	if err := s.db.Transaction(migrateResourceVersions); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(s.db); err != nil {
		t.Fatal(err)
	}
	current := conditionRecord(t, s, "old")
	conditions, err := current.CurrentConditions()
	if err != nil || conditions["identity"]["Ready"].Current || current.ResourceGeneration != old.ResourceGeneration+1 || current.ObservedGeneration("health") != 0 {
		t.Fatal("upgrade retained old current evidence", current.ResourceGeneration, conditions, err)
	}
	if err := conditionWrite(s, "old", current.ResourceVersion, "identity", "True", "IdentityReady"); err != nil {
		t.Fatal(err)
	}
	observed := conditionRecord(t, s, "old")
	if err := s.db.Transaction(func(tx *gorm.DB) error { return tx.Exec(removedConditionsMigration).Error }); err == nil {
		t.Fatal("migration removed observed condition history")
	}
	if _, err := NewStore(s.db); err != nil {
		t.Fatal("failed migration changed the schema contract", err)
	}
	retained := conditionRecord(t, s, "old")
	if retained.ResourceVersion != observed.ResourceVersion {
		t.Fatal("failed migration changed condition history")
	}
}
