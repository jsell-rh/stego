package publisher

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"example.com/kafka-test/queue"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/twmb/franz-go/pkg/kgo"
)

func privateDatabase(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("STEGO_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("STEGO_REQUIRE_POSTGRES") == "1" {
			t.Fatal("PostgreSQL is required for Kafka queue integration")
		}
		t.Skip("set STEGO_TEST_POSTGRES_DSN to test Kafka queue integration")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*config)
	t.Cleanup(func() { admin.Close() })
	name := "stego_kafka_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, `DROP DATABASE "`+name+`" WITH (FORCE)`); err != nil {
			t.Error(err)
		}
	})
	config.Database = name
	db := stdlib.OpenDB(*config)
	t.Cleanup(func() { db.Close() })
	migration, err := os.ReadFile("../queue/migrations/000001_outbox.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE records (id text PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestCommittedOutboxMessageReachesTLSKafka(t *testing.T) {
	db := privateDatabase(t)
	_, config := broker(t, identity(t, "localhost"))
	publisher, err := New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(publisher.Close)
	outbox, err := queue.New(db)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	workerConfig := queue.DefaultWorkerConfig()
	workerConfig.PollInterval = 10 * time.Millisecond
	worker, err := queue.NewWorker(outbox, map[string]queue.Handler{"kafka": func(ctx context.Context, message queue.Message) error {
		calls.Add(1)
		return publisher.Publish(ctx, Record{ID: message.ID, ResourceKey: message.ResourceKey, Kind: message.Kind, Payload: message.Payload})
	}}, workerConfig)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- worker.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-result:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(6 * time.Second):
			t.Error("worker did not stop")
		}
	})
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("INSERT INTO records VALUES ('one')"); err != nil {
		t.Fatal(err)
	}
	// JSONB expands this valid short input beyond 64 KiB. The publisher must
	// accept the queue's bounded stored representation without losing the event.
	event := queue.Message{ID: uuid.New(), Destination: "kafka", ResourceKey: "one", Kind: "record.created", Payload: []byte(`{"value":1e70000}`)}
	if err := queue.Enqueue(context.Background(), tx, event); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatal("Kafka received an uncommitted message")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for worker.Stats().Delivered != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("committed message did not reach Kafka: %+v", worker.Stats())
		}
		time.Sleep(10 * time.Millisecond)
	}
	var pending, records int
	if err := db.QueryRow("SELECT count(*) FROM stego_outbox.messages").Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM records").Scan(&records); err != nil {
		t.Fatal(err)
	}
	if pending != 0 || records != 1 {
		t.Fatal("broker acknowledgement changed the application write or left the event queued")
	}
	options, err := clientOptions(config)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := kgo.NewClient(append(options, kgo.ConsumeTopics("events"), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))...)
	if err != nil {
		t.Fatal(err)
	}
	defer consumer.Close()
	read, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	messages := consumer.PollRecords(read, 1).Records()
	if len(messages) != 1 || string(messages[0].Key) != event.ResourceKey {
		t.Fatal("Kafka record is missing")
	}
	if len(messages[0].Value) <= 64<<10 {
		t.Fatal("test did not exercise JSONB expansion")
	}
	found := false
	for _, header := range messages[0].Headers {
		if header.Key == "stego-message-id" && string(header.Value) == event.ID.String() {
			found = true
		}
	}
	if !found {
		t.Fatal("Kafka record lost the durable message ID")
	}
}
