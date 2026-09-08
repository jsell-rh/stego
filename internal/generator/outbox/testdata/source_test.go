package queue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func startSource(t *testing.T, db *sql.DB) (*Source, <-chan error) {
	t.Helper()
	source, err := NewSource(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- source.Run(context.Background()) }()
	t.Cleanup(source.Close)
	return source, done
}
func nextEvent(t *testing.T, sub Subscription) Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	event, err := sub.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return event
}
func noEvent(t *testing.T, sub Subscription) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := sub.Next(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unexpected event: %v", err)
	}
}
func TestEventSourceCommitAndMultipleProcesses(t *testing.T) {
	db := testDatabase(t)
	first, _ := startSource(t, db)
	second, _ := startSource(t, db)
	a, err := first.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := second.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	value := message("committed")
	value.Payload = []byte(`{"secret":"must not enter the live notice"}`)
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO records(id) VALUES($1)`, value.ResourceKey); err != nil {
		t.Fatal(err)
	}
	if err := Enqueue(context.Background(), tx, value); err != nil {
		t.Fatal(err)
	}
	noEvent(t, a)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []Subscription{a, b} {
		event := nextEvent(t, sub)
		if event.ID != value.ID || event.ResourceKey != value.ResourceKey || event.Kind != value.Kind || event.Destination != value.Destination {
			t.Fatalf("wrong event: %+v", event)
		}
		data, _ := json.Marshal(event)
		if strings.Contains(string(data), "secret") || strings.Contains(string(data), "payload") {
			t.Fatal("notice contains resource data")
		}
		var found bool
		if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM records WHERE id=$1)`, event.ResourceKey).Scan(&found); err != nil || !found {
			t.Fatal("event preceded the committed resource")
		}
	}
	tx, err = db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Enqueue(context.Background(), tx, message("rolled-back")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	noEvent(t, a)
	noEvent(t, b)
	// Different IDs keep notices distinct inside one transaction.
	one, two := message("same-key"), message("same-key")
	enqueue(t, db, one, two)
	for _, sub := range []Subscription{a, b} {
		if nextEvent(t, sub).ID != one.ID || nextEvent(t, sub).ID != two.ID {
			t.Fatal("transaction event order changed")
		}
	}
}
func TestEventSourceBoundsAndSlowConsumer(t *testing.T) {
	db := testDatabase(t)
	source, _ := startSource(t, db)
	slow, err := source.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	fast, err := source.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer fast.Close()
	event := Event{ID: uuid.New(), Destination: "audit", ResourceKey: "record", Kind: "record.updated"}
	for range SubscriptionBuffer + 1 {
		source.dispatch(event)
		if nextEvent(t, fast).ID != event.ID {
			t.Fatal("fast consumer lost event")
		}
	}
	if _, err := slow.Next(context.Background()); !errors.Is(err, ErrSlowConsumer) {
		t.Fatalf("slow consumer was not closed: %v", err)
	}
	fast.Close()
	var subs []Subscription
	for range MaxSubscriptions {
		sub, err := source.Subscribe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		subs = append(subs, sub)
	}
	if _, err := source.Subscribe(context.Background()); !errors.Is(err, ErrEventCapacity) {
		t.Fatalf("capacity limit ignored: %v", err)
	}
	for _, sub := range subs {
		sub.Close()
	}
	ctx, cancel := context.WithCancel(context.Background())
	sub, err := source.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := sub.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("subscription cancellation: %v", err)
	}
	source.Close()
	if _, err := source.Subscribe(context.Background()); !errors.Is(err, ErrEventSourceUnavailable) {
		t.Fatal("closed source accepted subscription")
	}
}
func TestEventSourceFailureRequiresNewSubscription(t *testing.T) {
	db := testDatabase(t)
	source, done := startSource(t, db)
	sub, err := source.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	pid := source.conn.PgConn().PID()
	if _, err := db.Exec(`SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrEventSourceUnavailable) {
			t.Fatalf("session failure hidden: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("source did not stop after session loss")
	}
	if _, err := sub.Next(context.Background()); !errors.Is(err, ErrEventSourceUnavailable) {
		t.Fatalf("subscription survived loss: %v", err)
	}
	if _, err := source.Subscribe(context.Background()); !errors.Is(err, ErrEventSourceUnavailable) {
		t.Fatal("failed source silently resumed")
	}
	replacement, _ := startSource(t, db)
	next, err := replacement.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	value := message("after-reconnect")
	enqueue(t, db, value)
	if nextEvent(t, next).ID != value.ID {
		t.Fatal("new session missed event")
	}
}
func TestEventNoticeValidation(t *testing.T) {
	event := Event{ID: uuid.New(), Destination: "audit", ResourceKey: strings.Repeat("\x01", 256), Kind: "record.updated"}
	encoded, err := encodeEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeEvent([]byte(encoded))
	if err != nil || decoded != event {
		t.Fatalf("maximum escaped key: %v", err)
	}
	for _, value := range []string{`{}`, `null`, encoded + `{}`, strings.Replace(encoded, `"kind":`, `"kind":"a","kind":`, 1), strings.Replace(encoded, `"kind":`, `"unknown":`, 1), strings.Replace(encoded, `"record.updated"`, `"bad\ud800"`, 1), strings.Repeat("x", MaxNoticeBytes+1)} {
		if _, err := decodeEvent([]byte(value)); err == nil {
			t.Fatalf("invalid notice accepted: %s", value)
		}
	}
}
