package queue

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWorkerDoesNotStoreSinkErrorText(t *testing.T) {
	db := testDatabase(t)
	q, _ := New(db)
	event := message("one")
	enqueue(t, db, event)
	config := workerConfig()
	config.RetryMin, config.RetryMax = time.Minute, time.Minute
	worker, err := NewWorker(q, map[string]Handler{"audit": func(context.Context, Message) error {
		return errors.New("credential=private-value")
	}}, config)
	if err != nil {
		t.Fatal(err)
	}
	cancel, result := startWorker(t, worker)
	awaitWorker(t, func() bool { return worker.Stats().Retried == 1 })
	stopWorker(t, cancel, result)
	var code string
	if err := db.QueryRow("SELECT failure_code FROM stego_outbox.messages WHERE id = $1", event.ID).Scan(&code); err != nil || code != "delivery-failed" {
		t.Fatalf("sink error text escaped the worker: code=%q, err=%v", code, err)
	}
}

func TestWorkerDoesNotAcknowledgeSuccessAfterAttemptDeadline(t *testing.T) {
	db := testDatabase(t)
	q, _ := New(db)
	event := message("one")
	enqueue(t, db, event)
	config := workerConfig()
	config.RetryMin, config.RetryMax = time.Minute, time.Minute
	worker, err := NewWorker(q, map[string]Handler{"audit": func(ctx context.Context, _ Message) error {
		<-ctx.Done()
		return nil
	}}, config)
	if err != nil {
		t.Fatal(err)
	}
	cancel, result := startWorker(t, worker)
	awaitWorker(t, func() bool { return worker.Stats().Retried == 1 })
	stopWorker(t, cancel, result)
	if worker.Stats().Delivered != 0 {
		t.Fatal("late success was acknowledged")
	}
	var code string
	if err := db.QueryRow("SELECT failure_code FROM stego_outbox.messages WHERE id = $1", event.ID).Scan(&code); err != nil || code != "delivery-timeout" {
		t.Fatalf("late success was not retained: code=%q, err=%v", code, err)
	}
}
