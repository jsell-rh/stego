package queue

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/000001_outbox.sql
var schema string

func testDatabase(t testing.TB) *sql.DB {
	t.Helper()
	dsn := os.Getenv("STEGO_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("STEGO_REQUIRE_POSTGRES") == "1" {
			t.Fatal("PostgreSQL integration tests require STEGO_TEST_POSTGRES_DSN")
		}
		t.Skip("set STEGO_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*config)
	t.Cleanup(func() { admin.Close() })
	name := "stego_outbox_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, `DROP DATABASE "`+name+`" WITH (FORCE)`); err != nil {
			t.Errorf("remove private test database: %v", err)
		}
	})
	config.Database = name
	db := stdlib.OpenDB(*config)
	db.SetMaxOpenConns(12)
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx, schema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE records (id text PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	return db
}

func message(key string) Message {
	return Message{ID: uuid.New(), Destination: "audit", ResourceKey: key, Kind: "record.created", Payload: []byte(`{"version":1}`)}
}

func enqueue(t testing.TB, db *sql.DB, messages ...Message) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := Enqueue(context.Background(), tx, messages...); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var count int
	if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestBlockedListsEachStuckKeyOnce(t *testing.T) {
	db := testDatabase(t)
	q, _ := New(db)
	enqueue(t, db, message("stuck"), message("stuck"), message("moving"), message("other"))
	deliveries, err := q.Claim(context.Background(), 2, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 2 {
		t.Fatal("claim did not lease the two oldest keys", len(deliveries))
	}
	blocked, err := q.Blocked(context.Background(), MaxBatchSize)
	if err != nil {
		t.Fatal(err)
	}
	// A key blocks while its oldest message is inside a delivery attempt.
	// The unclaimed key is ready, not blocked: it can progress.
	if len(blocked) != 2 {
		t.Fatal("blocked view does not show the in-flight keys", blocked)
	}
	var keys []string
	for _, item := range blocked {
		if item.Destination != "audit" || item.BlockedSince.IsZero() || item.AvailableAt.IsZero() {
			t.Fatal("blocked view is incomplete", item)
		}
		keys = append(keys, item.ResourceKey)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "moving,stuck" {
		t.Fatal("blocked view is not one entry per in-flight key", keys)
	}
	// A retry with a failure code keeps the key blocked for its delay, with
	// the reason visible.
	if _, err := q.Retry(context.Background(), deliveries[0].Receipt, time.Minute, "delivery-failed"); err != nil {
		t.Fatal(err)
	}
	blocked, err = q.Blocked(context.Background(), MaxBatchSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked) != 2 {
		t.Fatal("retrying a key did not keep it blocked", blocked)
	}
	found := ""
	for _, item := range blocked {
		if item.ResourceKey == deliveries[0].ResourceKey {
			found = item.FailureCode
		}
	}
	if found != "delivery-failed" {
		t.Fatal("blocked view lost the failure code", blocked)
	}
	// Once the retry delay passes and no lease is held, the key is ready
	// again: the operator view no longer lists it as blocked.
	if _, err := db.Exec("UPDATE stego_outbox.messages SET available_at = clock_timestamp() WHERE failure_code <> ''"); err != nil {
		t.Fatal(err)
	}
	blocked, err = q.Blocked(context.Background(), MaxBatchSize)
	if err != nil || len(blocked) != 1 || blocked[0].ResourceKey != "moving" {
		t.Fatal("a ready key stayed blocked", blocked, err)
	}
	for _, limit := range []int{0, MaxBatchSize + 1} {
		if _, err := q.Blocked(context.Background(), limit); err == nil {
			t.Fatal("invalid blocked limit was accepted")
		}
	}
}

func TestDeadLettersAndPurgeRemoveOnlyExhaustedMessages(t *testing.T) {
	db := testDatabase(t)
	q, _ := New(db)
	enqueue(t, db, message("doomed"), message("doomed"), message("fresh"))
	// Exhaust the doomed key: claim and retry it three times.
	for i := 0; i < 3; i++ {
		deliveries, err := q.Claim(context.Background(), 1, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if len(deliveries) != 1 || deliveries[0].ResourceKey != "doomed" {
			t.Fatal("claim did not select the oldest key", deliveries)
		}
		if _, err := q.Retry(context.Background(), deliveries[0].Receipt, 0, "delivery-failed"); err != nil {
			t.Fatal(err)
		}
	}
	dead, err := q.DeadLetters(context.Background(), MaxBatchSize, 3)
	if err != nil {
		t.Fatal(err)
	}
	// Only the first doomed message is claimed per retry round; the second
	// waits behind it. One exhausted message is enough to list the key.
	if len(dead) != 1 {
		t.Fatal("dead letters did not list the exhausted message", dead)
	}
	for _, item := range dead {
		if item.ResourceKey != "doomed" || item.FailureCode != "delivery-failed" || item.Attempts < 3 || item.ID == uuid.Nil {
			t.Fatal("dead letter view is incomplete", item)
		}
	}
	// The fresh message stays below the threshold.
	if dead, err := q.DeadLetters(context.Background(), MaxBatchSize, 4); err != nil || len(dead) != 0 {
		t.Fatal("dead letters reported a message below the threshold", dead, err)
	}
	// Retention has not passed: nothing is removed.
	removed, err := q.Purge(context.Background(), MaxBatchSize, 3, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 || count(t, db, "stego_outbox.messages") != 3 {
		t.Fatal("purge removed messages before the retention period", removed)
	}
	// After retention passes, only the exhausted message is removed.
	if _, err := db.Exec("UPDATE stego_outbox.messages SET available_at = clock_timestamp() - interval '2 hours' WHERE attempts >= 3"); err != nil {
		t.Fatal(err)
	}
	removed, err = q.Purge(context.Background(), MaxBatchSize, 3, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 || count(t, db, "stego_outbox.messages") != 2 {
		t.Fatal("purge did not remove exactly the dead letter", removed)
	}
	remaining, err := q.DeadLetters(context.Background(), MaxBatchSize, 1)
	if err != nil || len(remaining) != 0 {
		t.Fatal("dead letters remain after purge", remaining, err)
	}
	if count(t, db, "records") != 0 {
		t.Fatal("purge changed application data")
	}
	for _, args := range []struct {
		limit     int
		attempts  int64
		olderThan time.Duration
	}{{0, 1, time.Hour}, {MaxBatchSize + 1, 1, time.Hour}, {1, 0, time.Hour}, {1, 1, 0}, {1, 1, 31 * 24 * time.Hour}} {
		if _, err := q.Purge(context.Background(), args.limit, args.attempts, args.olderThan); err == nil {
			t.Fatal("invalid purge limits were accepted")
		}
	}
	if _, err := q.DeadLetters(context.Background(), 0, 1); err == nil {
		t.Fatal("invalid dead-letter limit was accepted")
	}
}

func TestWriteAndEventCommitTogether(t *testing.T) {
	db := testDatabase(t)
	for _, commit := range []bool{false, true} {
		tx, err := db.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.Exec("INSERT INTO records VALUES ('one')"); err != nil {
			t.Fatal(err)
		}
		if err := Enqueue(context.Background(), tx, message("one")); err != nil {
			t.Fatal(err)
		}
		if count(t, db, "records") != 0 || count(t, db, "stego_outbox.messages") != 0 {
			t.Fatal("uncommitted data became visible")
		}
		want := 0
		if commit {
			err, want = tx.Commit(), 1
		} else {
			err = tx.Rollback()
		}
		if err != nil {
			t.Fatal(err)
		}
		if count(t, db, "records") != want || count(t, db, "stego_outbox.messages") != want {
			t.Fatal("application data and notification did not commit together")
		}
	}
}

func TestDuplicateEventAbortsApplicationWrite(t *testing.T) {
	db := testDatabase(t)
	event := message("one")
	enqueue(t, db, event)
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("INSERT INTO records VALUES ('two')"); err != nil {
		t.Fatal(err)
	}
	if err := Enqueue(context.Background(), tx, event); err == nil {
		t.Fatal("duplicate event was accepted")
	}
	if err := tx.Commit(); err == nil {
		t.Fatal("failed transaction committed application data")
	}
	if count(t, db, "records") != 0 || count(t, db, "stego_outbox.messages") != 1 {
		t.Fatal("duplicate event changed stored data")
	}
}

func TestRetryPreservesOrderAndOtherKeysProgress(t *testing.T) {
	db := testDatabase(t)
	q, _ := New(db)
	a, b, other := message("one"), message("one"), message("two")
	enqueue(t, db, a, b, other)
	first, err := q.Claim(context.Background(), 32, time.Minute)
	if err != nil || len(first) != 2 {
		t.Fatalf("first claim: %v, %v", first, err)
	}
	var head Delivery
	for _, delivery := range first {
		if delivery.ID == b.ID {
			t.Fatal("claimed successor before its predecessor")
		}
		if delivery.ID == a.ID {
			head = delivery
		} else {
			if ok, err := q.Acknowledge(context.Background(), delivery.Receipt); !ok || err != nil {
				t.Fatalf("acknowledge other key: %v", err)
			}
		}
	}
	if ok, err := q.Retry(context.Background(), head.Receipt, time.Hour, "sink-unavailable"); !ok || err != nil {
		t.Fatalf("retry: %v", err)
	}
	if next, err := q.Claim(context.Background(), 32, time.Minute); err != nil || len(next) != 0 {
		t.Fatalf("retry did not block successor: %v, %v", next, err)
	}
	if _, err := db.Exec("UPDATE stego_outbox.messages SET available_at = clock_timestamp() WHERE id = $1", a.ID); err != nil {
		t.Fatal(err)
	}
	next, err := q.Claim(context.Background(), 32, time.Minute)
	if err != nil || len(next) != 1 || next[0].ID != a.ID || next[0].Attempts != 2 {
		t.Fatalf("retry lost identity or attempt count: %v, %v", next, err)
	}
	if ok, err := q.Acknowledge(context.Background(), next[0].Receipt); !ok || err != nil {
		t.Fatalf("retry ack: %v", err)
	}
	next, err = q.Claim(context.Background(), 32, time.Minute)
	if err != nil || len(next) != 1 || next[0].ID != b.ID {
		t.Fatalf("successor did not progress: %v, %v", next, err)
	}
}

func TestExpiredLeaseCannotAcknowledgeOrRetry(t *testing.T) {
	db := testDatabase(t)
	q, _ := New(db)
	event := message("one")
	enqueue(t, db, event)
	first, err := q.Claim(context.Background(), 1, time.Minute)
	if err != nil || len(first) != 1 {
		t.Fatalf("claim: %v", err)
	}
	if _, err := db.Exec("UPDATE stego_outbox.messages SET lease_until = clock_timestamp() - interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	if ok, err := q.Acknowledge(context.Background(), first[0].Receipt); ok || err != nil {
		t.Fatalf("expired receipt acknowledged event: %v", err)
	}
	second, err := q.Claim(context.Background(), 1, time.Minute)
	if err != nil || len(second) != 1 || second[0].ID != event.ID || second[0].Receipt.Token == first[0].Receipt.Token {
		t.Fatalf("lease did not recover: %v, %v", second, err)
	}
	if ok, err := q.Retry(context.Background(), first[0].Receipt, 0, "failure"); ok || err != nil {
		t.Fatalf("stale receipt replaced a new lease: %v", err)
	}
	if ok, err := q.Acknowledge(context.Background(), second[0].Receipt); !ok || err != nil {
		t.Fatalf("current receipt rejected: %v", err)
	}
}

func TestConcurrentWorkersHaveDistinctLeases(t *testing.T) {
	db := testDatabase(t)
	q, _ := New(db)
	for i := 0; i < 32; i++ {
		enqueue(t, db, message(fmt.Sprint(i)))
	}
	var workers sync.WaitGroup
	results := make(chan []Delivery, 8)
	errorsFound := make(chan error, 8)
	for i := 0; i < 8; i++ {
		workers.Go(func() {
			deliveries, err := q.Claim(context.Background(), 8, time.Minute)
			if err != nil {
				errorsFound <- err
				return
			}
			results <- deliveries
		})
	}
	workers.Wait()
	close(results)
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
	seen := make(map[uuid.UUID]bool)
	for batch := range results {
		for _, delivery := range batch {
			if seen[delivery.ID] {
				t.Fatal("two workers claimed the same message")
			}
			seen[delivery.ID] = true
		}
	}
	if len(seen) != 32 {
		t.Fatalf("claimed %d messages, want 32", len(seen))
	}
}

func TestConcurrentEnqueueSerializesBeforeSequenceAllocation(t *testing.T) {
	db := testDatabase(t)
	a, b := message("one"), message("one")
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := Enqueue(context.Background(), tx, a); err != nil {
		t.Fatal(err)
	}
	second, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Rollback()
	result := make(chan error, 1)
	go func() { result <- Enqueue(context.Background(), second, b) }()
	select {
	case err := <-result:
		t.Fatalf("enqueue passed an uncommitted predecessor: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if err := second.Commit(); err != nil {
		t.Fatal(err)
	}
	q, _ := New(db)
	deliveries, err := q.Claim(context.Background(), 32, time.Minute)
	if err != nil || len(deliveries) != 1 || deliveries[0].ID != a.ID {
		t.Fatalf("commit order was lost: %v, %v", deliveries, err)
	}
}

func TestInvalidPayloadsFailBeforeDatabaseAccess(t *testing.T) {
	for _, payload := range []string{`[]`, `null`, `{`, `{} {}`, `{"a":1,"a":2}`, `{"nested":{"a":1,"\u0061":2}}`, `{"nested":[{"a":1,"a":2}]}`, "{\"bad\":\"\xff\"}", strings.Repeat(" ", MaxPayloadBytes) + `{}`, `{"x":` + strings.Repeat("[", 32) + `0` + strings.Repeat("]", 32) + `}`} {
		event := message("one")
		event.Payload = []byte(payload)
		if err := validateMessage(event); err == nil {
			t.Errorf("accepted invalid payload %q", payload[:min(len(payload), 80)])
		}
	}
	valid := message("one")
	valid.Payload = []byte(`{"array":[{"a":1},{"a":2}],"text":"safe","empty":[]}`)
	if err := validateMessage(valid); err != nil {
		t.Fatal(err)
	}
	if err := Enqueue(context.Background(), nil, valid); err == nil {
		t.Fatal("accepted no transaction")
	}
	if _, err := New(nil); err == nil {
		t.Fatal("accepted no database")
	}
}

func TestCanceledQueueOperationReturnsError(t *testing.T) {
	db := testDatabase(t)
	q, _ := New(db)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := q.Claim(ctx, 1, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled claim: %v", err)
	}
}

func TestOutboxCrashHelper(t *testing.T) {
	mode := os.Getenv("STEGO_OUTBOX_CRASH_MODE")
	if mode == "" {
		return
	}
	config, err := pgx.ParseConfig(os.Getenv("STEGO_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	config.Database = os.Getenv("STEGO_OUTBOX_CRASH_DB")
	if !strings.HasPrefix(config.Database, "stego_outbox_test_") {
		t.Fatal("crash helper needs a private test database")
	}
	db := stdlib.OpenDB(*config)
	if mode == "uncommitted" {
		tx, err := db.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec("INSERT INTO records VALUES ('crashed')"); err != nil {
			t.Fatal(err)
		}
		if err := Enqueue(context.Background(), tx, message("crashed")); err != nil {
			t.Fatal(err)
		}
	} else {
		q, _ := New(db)
		deliveries, err := q.Claim(context.Background(), 1, time.Second)
		if err != nil || len(deliveries) != 1 {
			t.Fatalf("crash claim: %v, %v", deliveries, err)
		}
	}
	os.Exit(99)
}

func TestProcessCrashPreservesTransactionAndLeaseRules(t *testing.T) {
	for _, mode := range []string{"uncommitted", "claimed"} {
		t.Run(mode, func(t *testing.T) {
			db := testDatabase(t)
			event := message("one")
			if mode == "claimed" {
				enqueue(t, db, event)
			}
			var name string
			if err := db.QueryRow("SELECT current_database()").Scan(&name); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOutboxCrashHelper$")
			cmd.Env = append(os.Environ(), "STEGO_OUTBOX_CRASH_MODE="+mode, "STEGO_OUTBOX_CRASH_DB="+name)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 99 {
				t.Fatalf("crash helper: %v\n%s", err, output)
			}
			if mode == "uncommitted" {
				if count(t, db, "records") != 0 || count(t, db, "stego_outbox.messages") != 0 {
					t.Fatal("process crash committed unfinished work")
				}
				return
			}
			q, _ := New(db)
			deadline := time.Now().Add(3 * time.Second)
			for {
				deliveries, err := q.Claim(context.Background(), 1, time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				if len(deliveries) == 1 {
					if deliveries[0].ID != event.ID || deliveries[0].Attempts != 2 {
						t.Fatal("crash recovery changed delivery identity")
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("crashed worker lease did not expire")
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

func BenchmarkClaimAndAcknowledge(b *testing.B) {
	db := testDatabase(b)
	q, _ := New(db)
	for i := 0; i < b.N; i++ {
		enqueue(b, db, message(fmt.Sprint(i)))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		deliveries, err := q.Claim(context.Background(), 1, time.Minute)
		if err != nil || len(deliveries) != 1 {
			b.Fatalf("claim: %v", err)
		}
		if ok, err := q.Acknowledge(context.Background(), deliveries[0].Receipt); !ok || err != nil {
			b.Fatalf("acknowledge: %v", err)
		}
	}
}
