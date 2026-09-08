package storage

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	contract "example.com/transaction-test/contracts/storage"
	"example.com/transaction-test/plainstore"
	"example.com/transaction-test/queue"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

//go:embed outbox.sql
var queueSchema string

func database(t testing.TB, prepared bool) (*Store, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("STEGO_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("STEGO_REQUIRE_POSTGRES") == "1" {
			t.Fatal("PostgreSQL is required")
		}
		t.Skip("set STEGO_TEST_POSTGRES_DSN to test transactions")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*config)
	t.Cleanup(func() { admin.Close() })
	name := "stego_store_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, `DROP DATABASE "`+name+`" WITH (FORCE)`); err != nil {
			t.Errorf("remove private database: %v", err)
		}
	})
	config.Database = name
	db := stdlib.OpenDB(*config)
	db.SetMaxOpenConns(8)
	t.Cleanup(func() { db.Close() })
	orm, err := gorm.Open(postgres.New(postgres.Config{Conn: db}), &gorm.Config{PrepareStmt: prepared, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(orm.WithContext(ctx)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, queueSchema); err != nil {
		t.Fatal(err)
	}
	return NewStore(orm), db
}

func message(key string) queue.Message {
	return queue.Message{ID: uuid.New(), Destination: "audit", ResourceKey: key, Kind: "record.changed", Payload: []byte(`{"value":1}`)}
}
func record(name string) Record {
	return Record{Meta: Meta{ID: uuid.NewString()}, Name: name, Value: 1}
}
func count(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAtomicWriteAndNotifications(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		for _, commit := range []bool{false, true} {
			t.Run(fmt.Sprintf("prepared=%v/commit=%v", prepared, commit), func(t *testing.T) {
				store, db := database(t, prepared)
				item := record("one")
				veto := errors.New("domain rule failed")
				err := store.WithTransaction(context.Background(), func(ctx context.Context, scope contract.Transaction) error {
					tx := scope.(*Store)
					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) > 10*time.Second {
						t.Fatal("transaction deadline missing")
					}
					var isolation string
					if err := tx.db.Raw("SHOW transaction_isolation").Scan(&isolation).Error; err != nil {
						return err
					}
					if isolation != "serializable" {
						t.Fatalf("isolation = %s", isolation)
					}
					if err := tx.Create(ctx, "Record", item); err != nil {
						return err
					}
					if _, err := tx.Get(ctx, "Record", item.ID); err != nil {
						return err
					}
					if err := tx.Notify(message(item.ID)); err != nil {
						return err
					}
					if err := tx.Notify(message("second-key")); err != nil {
						return err
					}
					if count(t, db, "records") != 0 || count(t, db, "stego_outbox.messages") != 0 {
						t.Fatal("uncommitted state became visible")
					}
					if !commit {
						return veto
					}
					return nil
				})
				want := 0
				if commit {
					if err != nil {
						t.Fatal(err)
					}
					want = 1
				} else if !errors.Is(err, veto) {
					t.Fatalf("lost callback error: %v", err)
				}
				if count(t, db, "records") != want || count(t, db, "stego_outbox.messages") != 2*want {
					t.Fatal("write and notifications did not commit together")
				}
			})
		}
	}
}

func TestNotificationFailureRollsBack(t *testing.T) {
	for _, mode := range []string{"invalid-json", "duplicate-id", "over-limit", "oversized", "missing-schema"} {
		t.Run(mode, func(t *testing.T) {
			store, db := database(t, false)
			if mode == "missing-schema" {
				if _, err := db.Exec("DROP SCHEMA stego_outbox CASCADE"); err != nil {
					t.Fatal(err)
				}
			}
			item := record("one")
			err := store.WithTransaction(context.Background(), func(ctx context.Context, scope contract.Transaction) error {
				tx := scope.(*Store)
				if err := tx.Create(ctx, "Record", item); err != nil {
					return err
				}
				event := message(item.ID)
				switch mode {
				case "invalid-json":
					event.Payload = []byte(`{"value":1,"value":2}`)
				case "oversized":
					event.Payload = make([]byte, queue.MaxPayloadBytes+1)
				}
				_ = tx.Notify(event) // A rejected notification must prevent commit even if ignored.
				if mode == "duplicate-id" {
					_ = tx.Notify(event)
				}
				if mode == "over-limit" {
					for i := 0; i < queue.MaxBatchSize; i++ {
						_ = tx.Notify(message(item.ID))
					}
				}
				return nil
			})
			if err == nil {
				t.Fatal("notification failure returned success")
			}
			if count(t, db, "records") != 0 {
				t.Fatal("resource committed without notification")
			}
			if mode != "missing-schema" && count(t, db, "stego_outbox.messages") != 0 {
				t.Fatal("partial notification batch committed")
			}
		})
	}
}

func TestTransactionScopeAndPayloadCopy(t *testing.T) {
	store, db := database(t, false)
	event := message("key")
	if err := store.Notify(event); !errors.Is(err, ErrTransactionRequired) {
		t.Fatal(err)
	}
	var retained *Store
	if err := store.WithTransaction(context.Background(), func(ctx context.Context, scope contract.Transaction) error {
		tx := scope.(*Store)
		retained = tx
		if err := tx.WithTransaction(ctx, func(context.Context, contract.Transaction) error { t.Fatal("nested callback ran"); return nil }); !errors.Is(err, ErrTransactionNested) {
			t.Fatal(err)
		}
		if err := tx.Notify(event); err != nil {
			return err
		}
		event.Payload[9] = '9'
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := retained.Notify(event); !errors.Is(err, ErrTransactionClosed) {
		t.Fatal(err)
	}
	if err := retained.Create(context.Background(), "Record", record("late")); err == nil {
		t.Fatal("retained transaction accepted a write")
	}
	var value int
	if err := db.QueryRow("SELECT (payload->>'value')::int FROM stego_outbox.messages").Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != 1 {
		t.Fatalf("payload was not copied: %d", value)
	}
}

func TestCanceledAndPanickedTransaction(t *testing.T) {
	store, db := database(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.WithTransaction(ctx, func(context.Context, contract.Transaction) error { t.Fatal("canceled callback ran"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recover() != "stop" {
				t.Error("callback panic was not preserved")
			}
		}()
		_ = store.WithTransaction(context.Background(), func(ctx context.Context, scope contract.Transaction) error {
			tx := scope.(*Store)
			if err := tx.Create(ctx, "Record", record("panic")); err != nil {
				t.Fatal(err)
			}
			if err := tx.Notify(message("panic")); err != nil {
				t.Fatal(err)
			}
			panic("stop")
		})
	}()
	ctx, cancel = context.WithCancel(context.Background())
	err := store.WithTransaction(ctx, func(ctx context.Context, scope contract.Transaction) error {
		tx := scope.(*Store)
		if err := tx.Create(ctx, "Record", record("canceled")); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if count(t, db, "records") != 0 || count(t, db, "stego_outbox.messages") != 0 {
		t.Fatal("canceled or panicked transaction committed")
	}
}

func TestSerializationFailureDoesNotReplay(t *testing.T) {
	store, db := database(t, false)
	item := record("shared")
	if err := store.Create(context.Background(), "Record", item); err != nil {
		t.Fatal(err)
	}
	read := make(chan struct{})
	release := make(chan struct{})
	result := make(chan error, 1)
	var calls atomic.Int32
	go func() {
		result <- store.WithTransaction(context.Background(), func(ctx context.Context, scope contract.Transaction) error {
			tx := scope.(*Store)
			calls.Add(1)
			if _, err := tx.Get(ctx, "Record", item.ID); err != nil {
				return err
			}
			close(read)
			<-release
			item.Value = 3
			if err := tx.Replace(ctx, "Record", item.ID, item); err != nil {
				return err
			}
			return tx.Notify(message(item.ID))
		})
	}()
	select {
	case <-read:
	case <-time.After(3 * time.Second):
		t.Fatal("transaction did not read")
	}
	if _, err := db.Exec("UPDATE records SET value = 2 WHERE id = $1", item.ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	err := <-result
	var state interface{ SQLState() string }
	if !errors.As(err, &state) || state.SQLState() != "40001" {
		t.Fatalf("expected serialization failure, got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("callback was replayed")
	}
	if count(t, db, "stego_outbox.messages") != 0 {
		t.Fatal("failed transaction committed a notification")
	}
}

func TestDatabaseDeadlineRollsBack(t *testing.T) {
	store, db := database(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := store.WithTransaction(ctx, func(ctx context.Context, scope contract.Transaction) error {
		tx := scope.(*Store)
		if err := tx.Create(ctx, "Record", record("deadline")); err != nil {
			return err
		}
		if err := tx.Notify(message("deadline")); err != nil {
			return err
		}
		return tx.db.WithContext(ctx).Exec("SELECT pg_sleep(10)").Error
	})
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("database deadline did not stop work: %v", err)
	}
	if count(t, db, "records") != 0 || count(t, db, "stego_outbox.messages") != 0 {
		t.Fatal("timed out transaction committed")
	}
}

func TestMutationMethodsShareTransaction(t *testing.T) {
	store, db := database(t, false)
	item := record("existing")
	if err := store.Create(context.Background(), "Record", item); err != nil {
		t.Fatal(err)
	}
	veto := errors.New("rule failed")
	err := store.WithTransaction(context.Background(), func(ctx context.Context, scope contract.Transaction) error {
		tx := scope.(*Store)
		item.Value = 2
		if err := tx.Replace(ctx, "Record", item.ID, item); err != nil {
			return err
		}
		item.Value = 3
		created, err := tx.Upsert(ctx, "Record", item, []string{"name"}, "")
		if err != nil {
			return err
		}
		if created {
			t.Fatal("upsert did not see transaction state")
		}
		value, err := tx.Get(ctx, "Record", item.ID)
		if err != nil {
			return err
		}
		if value.(Record).Value != 3 {
			t.Fatal("upsert escaped the transaction")
		}
		if err := tx.Delete(ctx, "Record", item.ID); err != nil {
			return err
		}
		if _, err := tx.Get(ctx, "Record", item.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("delete escaped transaction: %v", err)
		}
		if err := tx.Notify(message(item.ID)); err != nil {
			return err
		}
		return veto
	})
	if !errors.Is(err, veto) {
		t.Fatalf("lost mutation failure: %v", err)
	}
	value, err := store.Get(context.Background(), "Record", item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if value.(Record).Value != 1 || count(t, db, "stego_outbox.messages") != 0 {
		t.Fatal("failed scope left mutation state")
	}
}

func TestInvalidTransactionInputs(t *testing.T) {
	callback := func(context.Context, contract.Transaction) error {
		t.Fatal("invalid transaction ran callback")
		return nil
	}
	if err := (*Store)(nil).WithTransaction(context.Background(), callback); err == nil {
		t.Fatal("nil store accepted")
	}
	if err := NewStore(&gorm.DB{}).WithTransaction(context.Background(), callback); err == nil {
		t.Fatal("invalid database accepted")
	}
	store, _ := database(t, false)
	if err := store.WithTransaction(context.Background(), nil); err == nil {
		t.Fatal("nil callback accepted")
	}
	db := store.db.Begin()
	if db.Error != nil {
		t.Fatal(db.Error)
	}
	defer db.Rollback()
	if err := NewStore(db).WithTransaction(context.Background(), callback); !errors.Is(err, ErrTransactionNested) {
		t.Fatal(err)
	}
	broken := store.db.Session(&gorm.Session{})
	want := errors.New("existing connection error")
	broken.AddError(want)
	if err := NewStore(broken).WithTransaction(context.Background(), callback); !errors.Is(err, want) {
		t.Fatal(err)
	}
}

func BenchmarkTransactionalCreateNotify(b *testing.B) {
	store, _ := database(b, false)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		item := record(fmt.Sprintf("record-%d", i))
		if err := store.WithTransaction(ctx, func(ctx context.Context, scope contract.Transaction) error {
			tx := scope.(*Store)
			if err := tx.Create(ctx, "Record", item); err != nil {
				return err
			}
			return tx.Notify(message(item.ID))
		}); err != nil {
			b.Fatal(err)
		}
	}
}

func TestUnavailableNotificationsPreventCommit(t *testing.T) {
	store, db := database(t, false)
	plain := plainstore.NewStore(store.db)
	err := plain.WithTransaction(context.Background(), func(ctx context.Context, tx contract.Transaction) error {
		if err := tx.Create(ctx, "Record", record("unavailable")); err != nil {
			return err
		}
		_ = tx.Notify(message("unavailable"))
		return nil
	})
	if !errors.Is(err, contract.ErrNotificationsUnavailable) {
		t.Fatalf("notification rejection lost: %v", err)
	}
	if count(t, db, "records") != 0 {
		t.Fatal("ignored unavailable notification allowed commit")
	}
}
