package storage

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	contract "example.com/transaction-test/contracts/storage"
	"gorm.io/gorm"
)

func saveScopeKey(ctx context.Context, s *Store, id, scope string, version int64) error {
	return s.WithTransaction(ctx, func(ctx context.Context, tx contract.Transaction) error {
		_, err := tx.(contract.ResourceStateStore).SaveResourceState(ctx, "Record", id, scope, version, []byte("sealed payload"))
		return err
	})
}
func sealScope(ctx context.Context, s *Store, scope string, revision int64) (result contract.ResourceStateScope, err error) {
	err = s.WithTransaction(ctx, func(ctx context.Context, tx contract.Transaction) error {
		var err error
		result, err = tx.(contract.ResourceStateScopeStore).SealResourceStateScope(ctx, "Record", scope, revision)
		return err
	})
	return
}
func TestResourceStateScopeClosureAndRestart(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		t.Run(fmt.Sprint(prepared), func(t *testing.T) {
			s, db := database(t, prepared)
			ctx := context.Background()
			initial, err := s.LoadResourceStateScope(ctx, "Record", "provider")
			if err != nil || initial != (contract.ResourceStateScope{}) {
				t.Fatal("absent scope differs", err)
			}
			if err := saveScopeKey(ctx, s, "a", "provider", 0); err != nil {
				t.Fatal(err)
			}
			first, err := s.LoadResourceStateScope(ctx, "Record", "provider")
			if err != nil || first.Revision != 1 || first.Sealed {
				t.Fatal("new key did not change scope", first, err)
			}
			if err := saveScopeKey(ctx, s, "a", "provider", 1); err != nil {
				t.Fatal(err)
			}
			unchanged, err := s.LoadResourceStateScope(ctx, "Record", "provider")
			if err != nil || unchanged != first {
				t.Fatal("content update changed key revision", err)
			}
			if err := saveScopeKey(ctx, s, "b", "provider", 0); err != nil {
				t.Fatal(err)
			}
			if _, err := sealScope(ctx, s, "provider", first.Revision); !errors.Is(err, contract.ErrResourceStateConflict) {
				t.Fatal("stale scan closed scope", err)
			}
			closed, err := sealScope(ctx, s, "provider", 2)
			if err != nil || !closed.Sealed || closed.Revision != 3 {
				t.Fatal("scope did not close", closed, err)
			}
			restarted, err := NewStore(s.db)
			if err != nil {
				t.Fatal(err)
			}
			repeated, err := sealScope(ctx, restarted, "provider", closed.Revision)
			if err != nil || repeated != closed {
				t.Fatal("closure is not repeat safe", err)
			}
			if err := saveScopeKey(ctx, restarted, "c", "provider", 0); !errors.Is(err, contract.ErrResourceStateConflict) {
				t.Fatal("closed scope accepted a new key", err)
			}
			if err := saveScopeKey(ctx, restarted, "a", "provider", 2); err != nil {
				t.Fatal("closure blocked existing state", err)
			}
			page, err := restarted.ListResourceStateKeys(ctx, "Record", "provider", "", 10)
			if err != nil || len(page.Keys) != 2 {
				t.Fatal("closure lost keys", err)
			}
			if err := saveScopeKey(ctx, restarted, "c", "Provider", 0); err != nil {
				t.Fatal("scope matching is not exact", err)
			}
			for _, sql := range []string{
				"DELETE FROM stego_resource_state_scopes WHERE scope='provider'",
				"UPDATE stego_resource_state_scopes SET sealed=false,revision=revision+1 WHERE scope='provider'",
				"UPDATE stego_resource_state_scopes SET scope='changed',revision=revision+1 WHERE scope='Provider'",
				"INSERT INTO stego_resource_state(entity,resource_id,scope,version,data) VALUES('Record','direct','provider',1,''::bytea)",
			} {
				if _, err := db.Exec(sql); err == nil {
					t.Fatal("SQL bypassed scope history")
				}
			}
		})
	}
}

func TestResourceStateScopeRollbackAndIgnoredFailure(t *testing.T) {
	s, _ := database(t, false)
	ctx := context.Background()
	fault := errors.New("observation failed")
	err := s.WithTransaction(ctx, func(ctx context.Context, tx contract.Transaction) error {
		if _, err := tx.(contract.ResourceStateScopeStore).SealResourceStateScope(ctx, "Record", "empty", 0); err != nil {
			return err
		}
		return fault
	})
	if !errors.Is(err, fault) {
		t.Fatal(err)
	}
	if value, err := s.LoadResourceStateScope(ctx, "Record", "empty"); err != nil || value != (contract.ResourceStateScope{}) {
		t.Fatal("rollback retained closure", value, err)
	}
	if err := saveScopeKey(ctx, s, "new", "empty", 0); err != nil {
		t.Fatal("rollback blocked registration", err)
	}
	var retained contract.ResourceStateScopeStore
	err = s.WithTransaction(ctx, func(ctx context.Context, tx contract.Transaction) error {
		retained = tx.(contract.ResourceStateScopeStore)
		if _, err := tx.(contract.ResourceStateStore).SaveResourceState(ctx, "Record", "rollback", "other", 0, nil); err != nil {
			return err
		}
		_, _ = retained.SealResourceStateScope(ctx, "Record", "empty", 0)
		return nil
	})
	if !errors.Is(err, contract.ErrResourceStateConflict) {
		t.Fatal("ignored closure error committed", err)
	}
	if value, err := s.LoadResourceState(ctx, "Record", "rollback", "other"); err != nil || value.Version != 0 {
		t.Fatal("failed seal retained another write", err)
	}
	if _, err := retained.LoadResourceStateScope(ctx, "Record", "empty"); !errors.Is(err, ErrTransactionClosed) {
		t.Fatal(err)
	}
	if _, err := retained.SealResourceStateScope(ctx, "Record", "empty", 1); !errors.Is(err, ErrTransactionClosed) {
		t.Fatal(err)
	}
	closed, err := sealScope(ctx, s, "Record-empty", 0)
	if err != nil || !closed.Sealed || closed.Revision != 1 {
		t.Fatal("empty scope cannot close", err)
	}
}

func TestResourceStateScopeConcurrentInsertAndClosure(t *testing.T) {
	for _, sealFirst := range []bool{true, false} {
		t.Run(fmt.Sprint(sealFirst), func(t *testing.T) {
			s, _ := database(t, false)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := saveScopeKey(ctx, s, "a", "provider", 0); err != nil {
				t.Fatal(err)
			}
			entered := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			unlock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unlock()
			first := make(chan error, 1)
			second := make(chan error, 1)
			go func() {
				first <- s.WithTransaction(ctx, func(ctx context.Context, tx contract.Transaction) error {
					var err error
					if sealFirst {
						_, err = tx.(contract.ResourceStateScopeStore).SealResourceStateScope(ctx, "Record", "provider", 1)
					} else {
						_, err = tx.(contract.ResourceStateStore).SaveResourceState(ctx, "Record", "b", "provider", 0, nil)
					}
					close(entered)
					if err != nil {
						return err
					}
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			go func() {
				if sealFirst {
					second <- saveScopeKey(ctx, s, "b", "provider", 0)
				} else {
					_, err := sealScope(ctx, s, "provider", 1)
					second <- err
				}
			}()
			// The first transaction has the scope lock. A second operation cannot report
			// success while that transaction is open.
			select {
			case err := <-second:
				t.Fatal("scope operation escaped the open transaction", err)
			case <-time.After(20 * time.Millisecond):
			}
			unlock()
			select {
			case err := <-first:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			select {
			case err := <-second:
				if !errors.Is(err, contract.ErrResourceStateConflict) {
					t.Fatal("concurrent operation did not reject stale state", err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			current, err := s.LoadResourceStateScope(ctx, "Record", "provider")
			if err != nil || current.Sealed != sealFirst || current.Revision != 2 {
				t.Fatal("serialized result differs", current, err)
			}
		})
	}
}

func TestResourceStateScopeInvalidRead(t *testing.T) {
	s := &Store{db: &gorm.DB{}}
	if _, err := s.LoadResourceStateScope(nil, "Record", "scope"); !errors.Is(err, contract.ErrResourceState) {
		t.Fatal(err)
	}
	for _, scope := range []string{"", "bad\x00scope", "\xff"} {
		if _, err := s.LoadResourceStateScope(context.Background(), "Record", scope); !errors.Is(err, contract.ErrResourceState) {
			t.Fatal(err)
		}
	}
	if _, err := s.LoadResourceStateScope(context.Background(), "Unknown", "scope"); !errors.Is(err, contract.ErrResourceState) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.LoadResourceStateScope(ctx, "Record", "scope"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestResourceStateScopeInvalidSeal(t *testing.T) {
	s, _ := database(t, false)
	if _, err := s.SealResourceStateScope(context.Background(), "Record", "scope", 0); !errors.Is(err, ErrTransactionRequired) {
		t.Fatal(err)
	}
	if _, err := sealScope(context.Background(), s, "scope", -1); !errors.Is(err, contract.ErrResourceState) {
		t.Fatal(err)
	}
	if _, err := sealScope(context.Background(), s, "scope", math.MaxInt64); !errors.Is(err, contract.ErrResourceStateConflict) {
		t.Fatal(err)
	}
}

func TestResourceStateScopeSchemaGuards(t *testing.T) {
	for _, sql := range []string{
		"ALTER TABLE stego_resource_state_scopes DISABLE TRIGGER stego_resource_state_scope_guard",
		"ALTER TABLE stego_resource_state DISABLE TRIGGER stego_resource_state_key_guard",
		"ALTER TABLE stego_resource_state_scopes DROP CONSTRAINT stego_resource_state_scopes_pkey",
		"ALTER TABLE stego_resource_state_scopes ALTER COLUMN sealed DROP NOT NULL",
		"ALTER TABLE stego_resource_state_scopes ENABLE ROW LEVEL SECURITY",
	} {
		t.Run(sql, func(t *testing.T) {
			s, db := database(t, false)
			if _, err := db.Exec(sql); err != nil {
				t.Fatal(err)
			}
			if _, err := NewStore(s.db); err == nil {
				t.Fatal("invalid scope schema accepted")
			}
		})
	}
}

func TestResourceStateScopeBackfillAndOverflow(t *testing.T) {
	s, db := database(t, true)
	ctx := context.Background()
	if err := saveScopeKey(ctx, s, "old-a", "provider", 0); err != nil {
		t.Fatal(err)
	}
	if err := saveScopeKey(ctx, s, "old-b", "provider", 0); err != nil {
		t.Fatal(err)
	}
	// Simulate migration 010: retained states exist, but the key guard and scope
	// catalog do not. Run only migration 011, then verify the complete store.
	for _, sql := range []string{"DROP TRIGGER stego_resource_state_key_guard ON stego_resource_state", "DROP TABLE stego_resource_state_scopes"} {
		if _, err := db.Exec(sql); err != nil {
			t.Fatal(err)
		}
	}
	migrated := false
	for _, migration := range migrations {
		if migration.Name == "011_resource_state_scopes" {
			if err := migration.Func(s.db); err != nil {
				t.Fatal(err)
			}
			migrated = true
		}
	}
	if !migrated {
		t.Fatal("scope migration is missing")
	}
	reopened, err := NewStore(s.db)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := reopened.LoadResourceStateScope(ctx, "Record", "provider")
	if err != nil || scope.Revision != 2 || scope.Sealed {
		t.Fatal("backfill lost key coverage", scope, err)
	}
	if _, err := db.Exec("INSERT INTO stego_resource_state_scopes(entity,scope,revision,sealed) VALUES('Record','exhausted',9223372036854775807,false)"); err != nil {
		t.Fatal(err)
	}
	if err := saveScopeKey(ctx, reopened, "new", "exhausted", 0); err == nil {
		t.Fatal("scope revision overflow accepted a key")
	}
	if _, err := sealScope(ctx, reopened, "exhausted", math.MaxInt64); !errors.Is(err, contract.ErrResourceState) {
		t.Fatal("scope seal overflow accepted", err)
	}
}
