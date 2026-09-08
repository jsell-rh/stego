package queue

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func workerConfig() WorkerConfig {
	config := DefaultWorkerConfig()
	config.Concurrency = 2
	config.PollInterval = 10 * time.Millisecond
	config.AttemptTimeout = 100 * time.Millisecond
	config.RetryMin = 20 * time.Millisecond
	config.RetryMax = 100 * time.Millisecond
	return config
}

func startWorker(t *testing.T, worker *Worker) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- worker.Run(ctx) }()
	t.Cleanup(cancel)
	return cancel, result
}

func stopWorker(t *testing.T, cancel context.CancelFunc, result <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("worker did not stop")
	}
}

func awaitWorker(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("worker condition did not become true")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWorkerRetriesWithStableIdentityAndNoPayloadErrorText(t *testing.T) {
	db := testDatabase(t)
	q, _ := New(db)
	first, next := message("one"), message("one")
	enqueue(t, db, first, next)
	var attempts atomic.Int32
	var mu sync.Mutex
	var delivered []uuid.UUID
	handler := func(ctx context.Context, message Message) error {
		mu.Lock()
		delivered = append(delivered, message.ID)
		mu.Unlock()
		if message.ID == first.ID && attempts.Add(1) == 1 {
			return errors.New("secret payload must not be stored as an error")
		}
		return nil
	}
	worker, err := NewWorker(q, map[string]Handler{"audit": handler}, workerConfig())
	if err != nil {
		t.Fatal(err)
	}
	cancel, result := startWorker(t, worker)
	awaitWorker(t, func() bool { return worker.Stats().Delivered == 2 })
	stopWorker(t, cancel, result)
	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 3 || delivered[0] != first.ID || delivered[1] != first.ID || delivered[2] != next.ID {
		t.Fatalf("delivery order changed: %v", delivered)
	}
	stats := worker.Stats()
	if stats.Retried != 1 || stats.DeliveryFailures != 1 || stats.LostLeases != 0 || stats.DatabaseFailures != 0 {
		t.Fatalf("worker counters: %+v", stats)
	}
	if count(t, db, "stego_outbox.messages") != 0 {
		t.Fatal("acknowledged messages remain queued")
	}
}

func TestWorkerRetainsUnknownDestinations(t *testing.T) {
	db := testDatabase(t)
	q, _ := New(db)
	event := message("one")
	event.Destination = "new-destination"
	enqueue(t, db, event)
	worker, err := NewWorker(q, map[string]Handler{"audit": func(context.Context, Message) error { t.Error("wrong destination handler ran"); return nil }}, workerConfig())
	if err != nil {
		t.Fatal(err)
	}
	cancel, result := startWorker(t, worker)
	awaitWorker(t, func() bool { return worker.Stats().Retried > 0 })
	stopWorker(t, cancel, result)
	var code string
	if err := db.QueryRow("SELECT failure_code FROM stego_outbox.messages WHERE id = $1", event.ID).Scan(&code); err != nil || code != "destination-unknown" {
		t.Fatalf("unknown destination was not retained: %q, %v", code, err)
	}
	if worker.Stats().UnknownDestinations == 0 {
		t.Fatal("unknown destination is not observable")
	}
}

func TestWorkerHonorsAttemptDeadlineAndRetainsFailures(t *testing.T) {
	db := testDatabase(t)
	q, _ := New(db)
	event := message("one")
	enqueue(t, db, event)
	var called atomic.Int32
	worker, err := NewWorker(q, map[string]Handler{"audit": func(ctx context.Context, message Message) error {
		called.Add(1)
		<-ctx.Done()
		return ctx.Err()
	}}, workerConfig())
	if err != nil {
		t.Fatal(err)
	}
	cancel, result := startWorker(t, worker)
	awaitWorker(t, func() bool { return worker.Stats().Retried > 0 })
	stopWorker(t, cancel, result)
	if worker.Stats().Delivered != 0 || called.Load() == 0 || count(t, db, "stego_outbox.messages") != 1 {
		t.Fatal("timed-out message was lost")
	}
}

func TestWorkerShutdownAcknowledgesCompletedDelivery(t *testing.T) {
	db := testDatabase(t)
	q, _ := New(db)
	enqueue(t, db, message("one"))
	entered := make(chan struct{})
	exited := make(chan struct{})
	worker, err := NewWorker(q, map[string]Handler{"audit": func(ctx context.Context, message Message) error {
		close(entered)
		<-ctx.Done()
		close(exited)
		return nil // The sink completed its delivery during shutdown.
	}}, workerConfig())
	if err != nil {
		t.Fatal(err)
	}
	cancel, result := startWorker(t, worker)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("delivery did not start")
	}
	stopWorker(t, cancel, result)
	select {
	case <-exited:
	default:
		t.Fatal("worker returned with an active handler")
	}
	if worker.Stats().Delivered != 1 || count(t, db, "stego_outbox.messages") != 0 {
		t.Fatal("shutdown lost a completed acknowledgement")
	}
}

func TestWorkerCannotAcknowledgeChangedLease(t *testing.T) {
	db := testDatabase(t)
	q, _ := New(db)
	event := message("one")
	enqueue(t, db, event)
	worker, err := NewWorker(q, map[string]Handler{"audit": func(ctx context.Context, message Message) error {
		_, err := db.ExecContext(ctx, "UPDATE stego_outbox.messages SET lease_token = $1 WHERE id = $2", uuid.New(), message.ID)
		return err
	}}, workerConfig())
	if err != nil {
		t.Fatal(err)
	}
	cancel, result := startWorker(t, worker)
	awaitWorker(t, func() bool { return worker.Stats().LostLeases == 1 })
	stopWorker(t, cancel, result)
	if count(t, db, "stego_outbox.messages") != 1 || worker.Stats().Delivered != 0 {
		t.Fatal("stale worker removed a message")
	}
}

func TestWorkerLimitsConcurrencyAndCopiesHandlers(t *testing.T) {
	db := testDatabase(t)
	q, _ := New(db)
	for i := 0; i < 8; i++ {
		enqueue(t, db, message(uuid.NewString()))
	}
	var active, maximum atomic.Int32
	release := make(chan struct{})
	handlers := map[string]Handler{"audit": func(ctx context.Context, message Message) error {
		now := active.Add(1)
		defer active.Add(-1)
		for current := maximum.Load(); now > current && !maximum.CompareAndSwap(current, now); current = maximum.Load() {
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	worker, err := NewWorker(q, handlers, workerConfig())
	if err != nil {
		t.Fatal(err)
	}
	delete(handlers, "audit")
	cancel, result := startWorker(t, worker)
	awaitWorker(t, func() bool { return active.Load() == 2 })
	if err := worker.Run(context.Background()); err == nil {
		t.Fatal("same worker ran twice")
	}
	close(release)
	awaitWorker(t, func() bool { return worker.Stats().Delivered == 8 })
	stopWorker(t, cancel, result)
	if maximum.Load() != 2 || active.Load() != 0 {
		t.Fatalf("wrong concurrency: max=%d active=%d", maximum.Load(), active.Load())
	}
}

func TestWorkerReportsDatabaseFailuresWithoutDroppingMessages(t *testing.T) {
	db := testDatabase(t)
	q, _ := New(db)
	enqueue(t, db, message("one"))
	worker, err := NewWorker(q, map[string]Handler{"audit": func(context.Context, Message) error { return nil }}, workerConfig())
	if err != nil {
		t.Fatal(err)
	}
	// Removing the connection pool simulates a persistent connection failure.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	cancel, result := startWorker(t, worker)
	awaitWorker(t, func() bool { return worker.Stats().DatabaseFailures > 0 })
	stopWorker(t, cancel, result)
	if worker.Stats().Delivered != 0 {
		t.Fatal("database failure counted as delivery")
	}
}

func TestWorkerRejectsInvalidLimitsAndBoundsRetryDelay(t *testing.T) {
	q := &Queue{db: &sql.DB{}}
	handlers := map[string]Handler{"audit": func(context.Context, Message) error { return nil }}
	for _, change := range []func(*WorkerConfig){
		func(c *WorkerConfig) { c.Concurrency = 0 },
		func(c *WorkerConfig) { c.Concurrency = 33 },
		func(c *WorkerConfig) { c.LeaseDuration = c.AttemptTimeout },
		func(c *WorkerConfig) { c.PollInterval = 0 },
		func(c *WorkerConfig) { c.AttemptTimeout = 0 },
		func(c *WorkerConfig) { c.AttemptTimeout = time.Duration(math.MaxInt64) },
		func(c *WorkerConfig) { c.RetryMax = c.RetryMin - 1 },
	} {
		config := DefaultWorkerConfig()
		change(&config)
		if _, err := NewWorker(q, handlers, config); err == nil {
			t.Fatal("accepted unsafe worker limits")
		}
	}
	if _, err := NewWorker(q, map[string]Handler{"audit": nil}, DefaultWorkerConfig()); err == nil {
		t.Fatal("accepted nil handler")
	}
	worker, err := NewWorker(q, handlers, DefaultWorkerConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, attempts := range []int64{1, 2, 100, math.MaxInt64} {
		for i := 0; i < 100; i++ {
			delay := worker.retryDelay(attempts)
			if delay < worker.config.RetryMin/2 || delay > worker.config.RetryMax {
				t.Fatalf("retry delay escaped bounds: %v", delay)
			}
		}
	}
}
