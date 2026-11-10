package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// pgxListener is the Listener of the tests over one dedicated pgx connection. A service passes the same kind of
// adapter over its own driver.
type pgxListener struct{ conn *pgx.Conn }

func (l pgxListener) Listen(ctx context.Context, channel string) error {
	_, err := l.conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize())
	return err
}

func (l pgxListener) WaitForNotification(ctx context.Context) (Notification, error) {
	n, err := l.conn.WaitForNotification(ctx)
	if err != nil {
		return Notification{}, err
	}
	return Notification{Channel: n.Channel, Payload: n.Payload}, nil
}

func (l pgxListener) Close(ctx context.Context) error { return l.conn.Close(ctx) }

func startSource(t *testing.T, db *sql.DB, dsn string) (*Source, *pgx.Conn, <-chan error) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	source, err := NewSource(context.Background(), pgxListener{conn: conn}, db)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- source.Run(context.Background()) }()
	t.Cleanup(source.Close)
	return source, conn, done
}

func nextEvent(t *testing.T, sub Subscription) Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	event, err := sub.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func noEvent(t *testing.T, sub Subscription) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if event, err := sub.Next(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("an event came: %+v %v", event, err)
	}
}

// subscribe subscribes for tenant-a.
func subscribe(t *testing.T, source *Source) Subscription {
	t.Helper()
	return subscribeFor(t, source, "tenant-a")
}

func subscribeFor(t *testing.T, source *Source, tenantID string) Subscription {
	t.Helper()
	sub, err := source.Subscribe(context.Background(), testTenant(t, tenantID))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)
	return sub
}

func TestEventsNotice(t *testing.T) {
	t.Run("ST-SUP-RP01-LC01-T02 Notices reach every session after the commit, in commit order", func(t *testing.T) {
		db, dsn := testDatabase(t)
		q := liveQueue(t, db)
		first, _, _ := startSource(t, db, dsn)
		second, _, _ := startSource(t, db, dsn)
		a, b := subscribe(t, first), subscribe(t, second)
		ctx := context.Background()
		tn := testTenant(t, "tenant-a")
		value := message("audit", "committed")
		value.Payload = []byte(`{"secret":"must not enter the live notice"}`)
		tx, err := tenant.Begin(ctx, db, tn, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`INSERT INTO records (id) VALUES ($1)`, value.ResourceKey); err != nil {
			t.Fatal(err)
		}
		if err := q.Enqueue(ctx, tx, value); err != nil {
			t.Fatal(err)
		}
		noEvent(t, a)
		noEvent(t, b)
		if err := tenant.Commit(ctx, tx, tn); err != nil {
			t.Fatal(err)
		}
		for _, sub := range []Subscription{a, b} {
			event := nextEvent(t, sub)
			if event.ID != value.ID || event.ResourceKey != value.ResourceKey || event.Destination != value.Destination {
				t.Fatalf("the event is %+v", event)
			}
			data, _ := json.Marshal(event)
			if strings.Contains(string(data), "secret") {
				t.Fatal("the notice holds the payload")
			}
			var found bool
			if err := admin(t, db).QueryRow(`SELECT EXISTS (SELECT 1 FROM records WHERE id = $1)`, event.ResourceKey).Scan(&found); err != nil || !found {
				t.Fatal("the event came before the committed row")
			}
		}
		rolled, err := tenant.Begin(ctx, db, tn, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := q.Enqueue(ctx, rolled, message("audit", "rolled-back")); err != nil {
			t.Fatal(err)
		}
		if err := rolled.Rollback(); err != nil {
			t.Fatal(err)
		}
		noEvent(t, a)
		noEvent(t, b)
		one, two := message("audit", "same-key"), message("audit", "same-key")
		enqueue(t, db, q, one, two)
		for _, sub := range []Subscription{a, b} {
			if nextEvent(t, sub).ID != one.ID || nextEvent(t, sub).ID != two.ID {
				t.Fatal("the commit order of the notices changed")
			}
		}
	})
}

func TestEventsBounds(t *testing.T) {
	t.Run("ST-SUP-RP01-LC05-T02 The event source holds its subscription and queue bounds", func(t *testing.T) {
		db, dsn := testDatabase(t)
		source, _, _ := startSource(t, db, dsn)
		slow, fast := subscribe(t, source), subscribe(t, source)
		event := Event{ID: uuid.New(), Destination: "audit", ResourceKey: "record"}
		for range SubscriptionBuffer + 1 {
			source.dispatch("tenant-a", event)
			if nextEvent(t, fast).ID != event.ID {
				t.Fatal("the fast subscriber lost an event")
			}
		}
		if _, err := slow.Next(context.Background()); !errors.Is(err, ErrSlowConsumer) {
			t.Fatalf("the slow subscriber was not closed: %v", err)
		}
		fast.Close()
		var subs []Subscription
		for range MaxSubscriptions {
			sub, err := source.Subscribe(context.Background(), testTenant(t, "tenant-a"))
			if err != nil {
				t.Fatal(err)
			}
			subs = append(subs, sub)
		}
		if _, err := source.Subscribe(context.Background(), testTenant(t, "tenant-a")); !errors.Is(err, ErrEventCapacity) {
			t.Fatalf("the subscription limit was passed: %v", err)
		}
		for _, sub := range subs {
			sub.Close()
		}
		ctx, cancel := context.WithCancel(context.Background())
		sub, err := source.Subscribe(ctx, testTenant(t, "tenant-a"))
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		if _, err := sub.Next(context.Background()); !errors.Is(err, context.Canceled) {
			t.Fatalf("the canceled subscription returned %v", err)
		}
		source.Close()
		if _, err := source.Subscribe(context.Background(), testTenant(t, "tenant-a")); !errors.Is(err, ErrEventSourceUnavailable) {
			t.Fatal("the closed source accepted a subscription")
		}
	})

	t.Run("ST-SUP-RP01-LC05-T03 A lost notice session stops the source and its subscriptions", func(t *testing.T) {
		db, dsn := testDatabase(t)
		q := liveQueue(t, db)
		source, conn, done := startSource(t, db, dsn)
		sub := subscribe(t, source)
		pid := conn.PgConn().PID()
		if _, err := admin(t, db).Exec(`SELECT pg_terminate_backend($1)`, pid); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if !errors.Is(err, ErrEventSourceUnavailable) {
				t.Fatalf("the supervised task hid the lost session: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("the source did not stop after the lost session")
		}
		if _, err := sub.Next(context.Background()); !errors.Is(err, ErrEventSourceUnavailable) {
			t.Fatalf("the subscription outlived the session: %v", err)
		}
		if _, err := source.Subscribe(context.Background(), testTenant(t, "tenant-a")); !errors.Is(err, ErrEventSourceUnavailable) {
			t.Fatal("the failed source took a new subscription")
		}
		replacement, _, _ := startSource(t, db, dsn)
		next := subscribe(t, replacement)
		value := message("audit", "after-reconnect")
		enqueue(t, db, q, value)
		if nextEvent(t, next).ID != value.ID {
			t.Fatal("the replacement source missed the notice")
		}
	})
}

// liveQueue is a queue of a service that declares a live event source.
func liveQueue(t testing.TB, db *sql.DB) *Queue {
	t.Helper()
	q, err := New(db, Options{LiveEvents: true})
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestEventsTenant(t *testing.T) {
	t.Run("ST-SUP-RP01-LC21-T01 A subscriber gets only its own tenant's events", func(t *testing.T) {
		db, dsn := testDatabase(t)
		q := liveQueue(t, db)
		source, _, _ := startSource(t, db, dsn)
		if _, err := source.Subscribe(context.Background(), tenant.Tenant{}); !errors.Is(err, tenant.ErrZero) {
			t.Fatalf("the zero tenant gave %v", err)
		}
		a, b := subscribeFor(t, source, "t1"), subscribeFor(t, source, "t2")
		one, two := message("audit", "doc:1"), message("audit", "doc:1")
		enqueueAs(t, db, q, "t1", one)
		enqueueAs(t, db, q, "t2", two)
		if got := nextEvent(t, a); got.ID != one.ID {
			t.Fatalf("t1 got %+v", got)
		}
		if got := nextEvent(t, b); got.ID != two.ID {
			t.Fatalf("t2 got %+v", got)
		}
		noEvent(t, a)
		noEvent(t, b)
	})

	t.Run("ST-SUP-RP01-LC21-T02 A bad or forged notice is counted or dropped, and a notice carries only the message ID", func(t *testing.T) {
		db, dsn := testDatabase(t)
		q := liveQueue(t, db)
		source, _, done := startSource(t, requestPool(t, db), dsn)
		sub := subscribeFor(t, source, "t1")
		ctx := context.Background()
		watch, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = watch.Close(ctx) }()
		if _, err := watch.Exec(ctx, "LISTEN "+NoticeChannel); err != nil {
			t.Fatal(err)
		}
		// A notice that is not a message identity, and a forged one: the identity of a message that does not exist.
		forged := uuid.New()
		if _, err := db.Exec(`SELECT pg_notify($1, $2), pg_notify($1, $3)`, NoticeChannel, "not an event", forged.String()); err != nil {
			t.Fatal(err)
		}
		other := message("audit", "doc:1")
		enqueueAs(t, db, q, "t2", other)
		real := message("audit", "doc:2")
		enqueueAs(t, db, q, "t1", real)
		// The forged notice and t2's message are dropped for t1's subscriber: the next event is the real one.
		if got := nextEvent(t, sub); got.ID != real.ID || got.Destination != "audit" || got.ResourceKey != "doc:2" {
			t.Fatalf("the event is %+v", got)
		}
		noEvent(t, sub)
		if source.BadNotices() != 1 {
			t.Fatalf("%d bad notices are counted", source.BadNotices())
		}
		select {
		case err := <-done:
			t.Fatalf("the source stopped: %v", err)
		default:
		}
		// A session that listens sees only message identities.
		for _, want := range []string{"not an event", forged.String(), other.ID.String(), real.ID.String()} {
			wait, cancel := context.WithTimeout(ctx, 3*time.Second)
			n, err := watch.WaitForNotification(wait)
			cancel()
			if err != nil || n.Payload != want {
				t.Fatalf("the listening session saw %+v, %v, want %q", n, err, want)
			}
		}
	})
}
