package command

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jsell-rh/stego/registry/runtime/idempotency"
	"github.com/jsell-rh/stego/registry/runtime/kafkatest"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
	"github.com/jsell-rh/stego/registry/runtime/outbox/kafkaoutbox"
)

func TestKafkaEvent(t *testing.T) {
	t.Run("ST-SUP-RP01-LC30-T01 A command's event arrives once, with its entity key and its headers", func(t *testing.T) {
		broker := kafkatest.Start(t)
		e := newEnv(t)
		// With no declared topic, the event of the kind <entity topic>.created goes to the topic of the entity.
		topic := "orders-" + uuid.NewString()[:8]
		kind := topic + ".created"
		broker.CreateTopic(t, topic, 3)
		// The command declares the Kafka destination as the generated code does: idempotent by the event ID, not must
		// land, with the event type as its effect.
		cmd := e.command("create-order", func(spec *CommandSpec[testProbe]) {
			spec.Destinations = append(spec.Destinations, Destination{Name: kafkaoutbox.DestinationName, Idempotent: true,
				Effects: []string{kind}})
		}, e.orders)
		payload := `{"id":"o-k1","status":"open"}`
		if _, err := e.run(cmd, e.request("t1", u7(t), "k-kafka", `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
			o, err := repo(t, h, e.orders).Create(ctx, order{ID: "o-k1", Status: "open"}, nil)
			if err != nil {
				return idempotency.Response{}, err
			}
			return ok, h.Effects().Stage(ctx, kafkaoutbox.DestinationName, kind, o, json.RawMessage(payload))
		}); err != nil {
			t.Fatal(err)
		}
		var id uuid.UUID
		var key, outgoing string
		if err := e.admin.QueryRowContext(e.ctx, `SELECT id, resource_key, outgoing_key FROM stego_outbox.messages
			WHERE destination = $1`, kafkaoutbox.DestinationName).Scan(&id, &key, &outgoing); err != nil {
			t.Fatal(err)
		}

		destination, err := kafkaoutbox.New(kafkaoutbox.Settings{Brokers: []string{broker.Plain.Addr()}, InsecureLoopback: true}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer destination.Close()
		config := outbox.DefaultWorkerConfig()
		config.Concurrency, config.PollInterval, config.AttemptTimeout = 2, 20*time.Millisecond, 5*time.Second
		config.RetryMin, config.RetryMax = 100*time.Millisecond, time.Second
		worker, err := outbox.NewWorker(e.wqueue, config)
		if err != nil {
			t.Fatal(err)
		}
		if err := worker.Register(destination.Registration()); err != nil {
			t.Fatal(err)
		}
		// The audit sink messages of the run complete at once.
		if err := worker.Register(outbox.Destination{Name: "audit-sink", Idempotent: true,
			Handler: func(context.Context, *sql.Tx, outbox.Delivery) error { return nil }}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(e.ctx)
		stopped := make(chan struct{})
		go func() { defer close(stopped); _ = worker.Run(ctx) }()
		defer func() { cancel(); <-stopped }()

		records := broker.Records(t, topic, 1, time.Minute, 3*time.Second)
		if len(records) != 1 {
			t.Fatalf("the topic holds %d records, want the one event", len(records))
		}
		r := records[0]
		if string(r.Key) != "t1\x1forder:o-k1" || key != "order:o-k1" {
			t.Fatalf("the record key is %q and the message key %q, want the tenant t1, the separator and the entity order:o-k1", r.Key, key)
		}
		if string(r.Value) != payload {
			t.Fatalf("the record value is %s, want the staged payload %s", r.Value, payload)
		}
		want := map[string]string{kafkaoutbox.HeaderEventID: id.String(), kafkaoutbox.HeaderIdempotencyKey: outgoing,
			kafkaoutbox.HeaderEventType: kind, kafkaoutbox.HeaderEventAction: "created", kafkaoutbox.HeaderTenant: "t1", kafkaoutbox.HeaderContentType: "application/json"}
		for name, value := range want {
			if got, once := kafkatest.Header(r, name); !once || got != value {
				t.Fatalf("the header %s is %q (once: %v), want %q", name, got, once, value)
			}
		}
		deadline := time.Now().Add(10 * time.Second)
		for e.count(`SELECT count(*) FROM stego_outbox.messages WHERE id = $1 AND state = 'done'`, id) != 1 {
			if time.Now().After(deadline) {
				t.Fatal("the message is not done after its record arrived")
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
}
