package authz

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/google/uuid"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"stego.dev/runtime/outbox"
	"stego.dev/runtime/tenant"
)

// enqueue writes the outbox message of one tuple write for the relay in a transaction of tenant id, through the
// outbox queue, as the handle writes it, and gives its identity.
func (e *pgEnv) enqueue(t *testing.T, id string, operation Operation, tuple Tuple) uuid.UUID {
	t.Helper()
	revocation := uuid.New()
	body, err := json.Marshal(payload{Version: payloadVersion, Operation: operation, Revocation: revocation,
		Tuple: tupleJSON{ResourceType: tuple.Resource.Type, ResourceID: tuple.Resource.ID, Relation: tuple.Relation,
			SubjectType: tuple.Subject.Type, SubjectID: tuple.Subject.ID, SubjectRelation: tuple.SubjectRelation}})
	if err != nil {
		t.Fatal(err)
	}
	_, key, ok := splitResourceID(tuple.Resource.ID)
	if !ok {
		t.Fatalf("the tuple %s is not tenant-qualified", tuple.Name())
	}
	return e.enqueueRaw(t, id, resourceKey(tuple.Resource.Type, key), "authz."+string(operation), body, revocation.String())
}

func (e *pgEnv) enqueueRaw(t *testing.T, id, key, kind string, body []byte, group string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	message := outbox.Message{ID: uuid.New(), Destination: DestinationName, ResourceKey: key, Kind: kind,
		Payload: body, Group: group, MustLand: true}
	tx, err := tenant.Begin(ctx, e.runtime, mustTenant(t, id), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := e.queue.Enqueue(ctx, tx, message); err != nil {
		t.Fatal(err)
	}
	if err := tenant.Commit(ctx, tx, mustTenant(t, id)); err != nil {
		t.Fatal(err)
	}
	return message.ID
}

func floorText(t *testing.T, env *pgEnv) string {
	t.Helper()
	return env.scalar(t, "SELECT token FROM {s}.authz_floor")
}

// logRecorder is a log exporter of the tests that keeps the event names it gets.
type logRecorder struct {
	mu     sync.Mutex
	events []string
}

func (l *logRecorder) Export(_ context.Context, records []sdklog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, record := range records {
		l.events = append(l.events, record.EventName())
	}
	return nil
}
func (*logRecorder) Shutdown(context.Context) error   { return nil }
func (*logRecorder) ForceFlush(context.Context) error { return nil }

func (l *logRecorder) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.events)
}

// alertRelay builds a relay with a metric reader and a log recorder, and gives a function that counts the alert
// metric and events that name the destination authz.
func alertRelay(t *testing.T) (*Relay, func() (metric, events int64)) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	logs := &logRecorder{}
	logger := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logs)))
	relay, err := NewRelay(RelayConfig{Name: "authz-relay", Relationships: &fakeRelationships{}, DB: fakeDB(t),
		Schema: "public", Meter: meter, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	return relay, func() (int64, int64) {
		var data metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &data); err != nil {
			t.Fatal(err)
		}
		var total int64
		for _, scope := range data.ScopeMetrics {
			for _, m := range scope.Metrics {
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok || m.Name != "authz.revocation.late" {
					continue
				}
				for _, point := range sum.DataPoints {
					if destination, _ := point.Attributes.Value("destination"); destination.AsString() == "authz" {
						total += point.Value
					}
				}
			}
		}
		return total, int64(logs.count())
	}
}

// observed builds a metric reader and a log recorder for a worker, and gives a function that counts the metric
// name and the events of the event name that name the destination authz.
func observed(t *testing.T) (sdkmetric.Reader, *sdkmetric.MeterProvider, *sdklog.LoggerProvider, func(metric, event string) (int64, int64)) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	logs := &logRecorder{}
	logger := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logs)))
	return reader, meter, logger, func(name, event string) (int64, int64) {
		var data metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &data); err != nil {
			t.Fatal(err)
		}
		var total int64
		for _, scope := range data.ScopeMetrics {
			for _, m := range scope.Metrics {
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok || m.Name != name {
					continue
				}
				for _, point := range sum.DataPoints {
					if destination, _ := point.Attributes.Value("destination"); destination.AsString() == "authz" {
						total += point.Value
					}
				}
			}
		}
		logs.mu.Lock()
		defer logs.mu.Unlock()
		var events int64
		for _, e := range logs.events {
			if e == event {
				events++
			}
		}
		return total, events
	}
}

// oldestAge reads the gauge outbox.oldest.age{destination=authz} of the worker's metric reader.
func oldestAge(reader sdkmetric.Reader) OldestAge {
	return func(ctx context.Context) (time.Duration, error) {
		var data metricdata.ResourceMetrics
		if err := reader.Collect(ctx, &data); err != nil {
			return 0, err
		}
		for _, scope := range data.ScopeMetrics {
			for _, m := range scope.Metrics {
				gauge, ok := m.Data.(metricdata.Gauge[float64])
				if !ok || m.Name != "outbox.oldest.age" {
					continue
				}
				for _, point := range gauge.DataPoints {
					if destination, _ := point.Attributes.Value("destination"); destination.AsString() == "authz" {
						return time.Duration(point.Value * float64(time.Second)), nil
					}
				}
			}
		}
		return 0, errors.New("the gauge has no point for authz")
	}
}

func TestRelay(t *testing.T) {
	ctx := context.Background()
	viewer := Tuple{Resource: doc("1"), Relation: "viewer", Subject: user("bob")}
	refused := Refused(errors.New("invalid argument"))

	t.Run("ST-SUP-RP06-LC06-T01 The relay delivers the tuples of one resource key in order", func(t *testing.T) {
		env := newPG(t)
		fake := &fakeRelationships{before: func(n int, _ string) {
			if n == 1 {
				time.Sleep(300 * time.Millisecond)
			}
		}}
		var ids []uuid.UUID
		for _, subject := range []string{"a", "b", "c"} {
			ids = append(ids, env.enqueue(t, "t1", OperationTouch, Tuple{Resource: doc("1"), Relation: "viewer", Subject: user(subject)}))
		}
		env.startWorker(t, env.relay(t, fake))
		for _, id := range ids {
			env.waitState(t, id, outbox.Done)
		}
		var subjects []string
		for _, touch := range fake.touches {
			subjects = append(subjects, touch[0].Subject.ID)
		}
		if got := strings.Join(subjects, ""); got != "abc" {
			t.Fatalf("the provider got the writes %q, want abc in enqueue order", got)
		}
		if got := strings.Join(fake.log(), ","); got != "start 1,end 1,start 2,end 2,start 3,end 3" {
			t.Fatalf("the writes ran %s, want each after the one before it succeeded", got)
		}
	})

	t.Run("ST-SUP-RP06-LC06-T02 The relay stores the returned token on the owning row", func(t *testing.T) {
		env := newPG(t)
		if _, err := env.admin.ExecContext(ctx, "INSERT INTO "+env.schema+".documents (tenant_id, id) VALUES ('t1', '1'), ('t2', '1')"); err != nil {
			t.Fatal(err)
		}
		env.startWorker(t, env.relay(t, &fakeRelationships{writes: []Token{named("w", 31)}}))
		env.waitState(t, env.enqueue(t, "t1", OperationTouch, viewer), outbox.Done)
		if got := env.scalar(t, "SELECT authz_token FROM {s}.documents WHERE tenant_id = 't1' AND id = '1'"); got != "w-31" {
			t.Fatalf("the row holds the token %q, want w-31", got)
		}
		if got := env.scalar(t, "SELECT authz_token FROM {s}.documents WHERE tenant_id = 't2' AND id = '1'"); got != "" {
			t.Fatalf("the row of another tenant holds the token %q", got)
		}
	})

	t.Run("ST-SUP-RP06-LC06-T03 An entry with a permanent error is parked with an alert", func(t *testing.T) {
		env := newPG(t)
		_, meter, logger, count := observed(t)
		fake := &fakeRelationships{fail: func(int) error { return refused }}
		worker := env.newWorker(t, env.relay(t, fake), func(c *outbox.WorkerConfig) { c.Meter = meter; c.Logger = logger })
		run(t, worker)
		id := env.enqueue(t, "t1", OperationTouch, viewer)
		env.waitState(t, id, outbox.Parked)
		time.Sleep(200 * time.Millisecond)
		if got := env.scalar(t, "SELECT attempts::text FROM stego_outbox.messages WHERE id = $1", id); got != "1" || len(fake.touches) != 1 {
			t.Fatalf("the entry has %s attempts and %d provider calls, want 1 and no retry", got, len(fake.touches))
		}
		if metric, events := count("outbox.parked", "outbox.parked"); metric != 1 || events != 1 {
			t.Fatalf("the park raised %d metrics and %d events that name authz, want one of each", metric, events)
		}
	})

	t.Run("ST-SUP-RP06-LC06-T04 A parked entry does not halt the other keys", func(t *testing.T) {
		env := newPG(t)
		fake := &fakeRelationships{fail: func(n int) error {
			if n == 1 {
				return refused
			}
			return nil
		}}
		poison := env.enqueue(t, "t1", OperationTouch, viewer)
		worker := env.newWorker(t, env.relay(t, fake), func(c *outbox.WorkerConfig) { c.Concurrency = 1 })
		run(t, worker)
		env.waitState(t, poison, outbox.Parked)
		other := []uuid.UUID{
			env.enqueue(t, "t1", OperationTouch, Tuple{Resource: doc("2"), Relation: "viewer", Subject: user("bob")}),
			env.enqueue(t, "t1", OperationDelete, Tuple{Resource: doc("2"), Relation: "viewer", Subject: user("carol")}),
		}
		behind := env.enqueue(t, "t1", OperationTouch, Tuple{Resource: doc("1"), Relation: "viewer", Subject: user("dave")})
		for _, id := range other {
			env.waitState(t, id, outbox.Done)
		}
		time.Sleep(200 * time.Millisecond)
		if got := env.state(t, behind); got != "pending" {
			t.Fatalf("the later write of document:1 is %s, want held behind the parked entry", got)
		}
	})

	t.Run("ST-SUP-RP06-LC06-T05 The authz destination is registered as idempotent and must land", func(t *testing.T) {
		env := newPG(t)
		worker := env.newWorker(t, env.relay(t, &fakeRelationships{}), nil)
		registrations := worker.Registrations()
		if len(registrations) != 1 || registrations[0].Name != "authz" || !registrations[0].Idempotent || !registrations[0].MustLand {
			t.Fatalf("the worker holds %+v, want authz, idempotent and must land", registrations)
		}
		if relay := env.relay(t, &fakeRelationships{}); relay.Destination().StateHook == nil {
			t.Fatal("the relay registers no state hook")
		}
		// A payload the relay cannot read is a permanent refusal, which the outbox parks.
		run(t, worker)
		id := env.enqueueRaw(t, "t1", "document:1", "authz.touch", []byte(`{"v":1,"op":"create"}`), uuid.NewString())
		env.waitState(t, id, outbox.Parked)
	})

	t.Run("ST-SUP-RP06-LC13-T01 The relay raises the floor to the head it reads under the floor lock", func(t *testing.T) {
		env := newPG(t)
		if _, err := env.admin.ExecContext(ctx, "INSERT INTO "+env.schema+".documents (tenant_id, id) VALUES ('t1', '1'), ('t2', '2')"); err != nil {
			t.Fatal(err)
		}
		fake := &fakeRelationships{}
		var mu sync.Mutex
		var underLock []string
		fake.onHead = func() {
			var locks, floor string
			_ = env.admin.QueryRowContext(ctx, "SELECT count(*)::text FROM pg_catalog.pg_locks AS l JOIN pg_catalog.pg_class AS c ON c.oid = l.relation "+
				"WHERE c.relname = 'authz_floor' AND l.mode = 'RowShareLock' AND l.granted").Scan(&locks)
			_ = env.admin.QueryRowContext(ctx, "SELECT token FROM authz.authz_floor").Scan(&floor)
			mu.Lock()
			underLock = append(underLock, locks+"/"+floor)
			mu.Unlock()
		}
		first := env.enqueue(t, "t1", OperationDelete, viewer)
		worker := env.newWorker(t, env.relay(t, fake), func(c *outbox.WorkerConfig) { c.Concurrency = 1 })
		run(t, worker)
		env.waitState(t, first, outbox.Done)
		if got := floorText(t, env); got != "w-1" {
			t.Fatalf("the floor is %q after the first delivery, want w-1, the written_at of the tuple and marker write", got)
		}
		env.waitState(t, env.enqueue(t, "t2", OperationTouch, Tuple{Resource: docIn("t2", "2"), Relation: "viewer", Subject: user("bob")}), outbox.Done)
		if got := floorText(t, env); got != "w-2" {
			t.Fatalf("the floor is %q after the second delivery, want w-2", got)
		}
		mu.Lock()
		defer mu.Unlock()
		// Both writes, the delete and the touch, carried the floor marker in the same request.
		markers := 0
		for _, event := range fake.events {
			if event == "marker" {
				markers++
			}
		}
		if markers != 2 {
			t.Fatalf("the provider got %d requests with the floor marker, want 2: %v", markers, fake.events)
		}
		// Each write of the tuple and the marker ran while the floor row lock was granted and before the floor commit.
		if len(underLock) != 2 || underLock[0] != "1/" || underLock[1] != "1/w-1" {
			t.Fatalf("at the writes the lock and floor were %v, want [1/ 1/w-1]", underLock)
		}
		// The owning row holds the same written_at token as the floor.
		if got := env.scalar(t, "SELECT string_agg(authz_token, ',' ORDER BY id) FROM {s}.documents"); got != "w-1,w-2" {
			t.Fatalf("the rows hold %q, want the tokens w-1,w-2 of their writes", got)
		}
	})

	t.Run("ST-SUP-RP06-LC13-T10 The floor comes from the relay's own marker write", func(t *testing.T) {
		env := newPG(t)
		fake := &fakeRelationships{heads: []Token{named("m", 1)}}
		var lockedAtMarker string
		fake.onHead = func() {
			_ = env.admin.QueryRowContext(ctx, "SELECT count(*)::text FROM pg_catalog.pg_locks AS l JOIN pg_catalog.pg_class AS c ON c.oid = l.relation "+
				"WHERE c.relname = 'authz_floor' AND l.mode = 'RowShareLock' AND l.granted").Scan(&lockedAtMarker)
		}
		id := env.enqueue(t, "t1", OperationDelete, viewer)
		env.startWorker(t, env.relay(t, fake))
		env.waitState(t, id, outbox.Done)
		if got := strings.Join(fake.events, ","); got != "start 1,marker,end 1" {
			t.Fatalf("the provider got %s, want one request with the tuple's write and the marker TOUCH", got)
		}
		if lockedAtMarker != "1" {
			t.Fatalf("the marker TOUCH ran with %q floor row locks, want inside the floor lock", lockedAtMarker)
		}
		if got := floorText(t, env); got != "m-1" {
			t.Fatalf("the floor is %q, want the marker's written_at token m-1", got)
		}
		// Against a real SpiceDB whose ReadSchema answers an older revision: the relay reads no head, and a check
		// right after the delivery sees the revocation.
		db := startSpiceDB(t, fixtureSchema)
		old, err := db.provider.Touch(ctx, []Tuple{{Resource: doc("t10"), Relation: "viewer", Subject: person(t, "bob")}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		var reads atomic.Int64
		conn, err := grpc.NewClient(testingServer.endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithPerRPCCredentials(db.key),
			grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
				if strings.HasSuffix(method, "/ReadSchema") {
					reads.Add(1)
					reply.(*v1.ReadSchemaResponse).ReadAt = &v1.ZedToken{Token: old.Value()}
					return nil
				}
				return invoker(ctx, method, req, reply, cc, opts...)
			}))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		watched, err := NewSpiceDB(conn)
		if err != nil {
			t.Fatal(err)
		}
		real := newPG(t)
		rt := real.runtimeOf(t, db.provider, nil)
		tx, handle := real.begin(t, rt, "t1", mustPerson(t, "alice"), rel("document#viewer"))
		if _, err := handle.Unrelate(ctx, Tuple{Resource: doc("t10"), Relation: "viewer", Subject: person(t, "bob")}); err != nil {
			t.Fatal(err)
		}
		real.commit(t, tx, "t1")
		real.startWorker(t, real.relay(t, watched))
		real.waitAll(t, outbox.Done)
		if reads.Load() != 0 {
			t.Fatalf("the relay read the head %d times", reads.Load())
		}
		_, bob := real.begin(t, rt, "t1", mustPerson(t, "bob"))
		if result, err := bob.Evaluate(ctx, "view", doc("t10"), old, nil); err != nil || result.Allowed {
			t.Fatalf("the check after the delivery gave %+v, %v; want the revocation seen", result, err)
		}
		// At start-up the relay checks that the schema declares the marker, and refuses one that does not.
		if err := real.relay(t, db.provider).Check(ctx); err != nil {
			t.Fatalf("Check with the marker declared gave %v", err)
		}
		bare := startSpiceDB(t, strings.Replace(fixtureSchema, "definition stego_floor {\n\trelation marker: stego_floor\n}\n", "", 1))
		if err := real.relay(t, bare.provider).Check(ctx); err == nil {
			t.Fatal("Check with no marker in the schema let the relay start")
		}
	})

	t.Run("ST-SUP-RP06-LC13-T08 The floor is raised before the entry is done", func(t *testing.T) {
		env := newPG(t)
		rt := env.runtimeOf(t, &recorder{}, nil)
		alice := mustPerson(t, "alice")
		tx, handle := env.begin(t, rt, "t1", alice, rel("document#viewer"))
		revocation, err := handle.Unrelate(ctx, viewer)
		if err != nil {
			t.Fatal(err)
		}
		env.commit(t, tx, "t1")
		fake := &fakeRelationships{heads: []Token{tok(9), tok(9)}}
		relay := env.relay(t, fake)
		stopped := errors.New("the relay stops")
		var once sync.Once
		var floorWhileStopped, stateWhileStopped string
		relay.afterFloor = func() error {
			var err error
			once.Do(func() {
				_ = env.admin.QueryRowContext(ctx, "SELECT token FROM authz.authz_floor").Scan(&floorWhileStopped)
				_ = env.admin.QueryRowContext(ctx, "SELECT state FROM stego_outbox.messages").Scan(&stateWhileStopped)
				err = stopped
			})
			return err
		}
		env.startWorker(t, relay)
		env.waitAll(t, outbox.Done)
		if floorWhileStopped != "t-9" || stateWhileStopped == "done" {
			t.Fatalf("when the relay stopped the floor was %q and the entry %s, want t-9 and not done", floorWhileStopped, stateWhileStopped)
		}
		if len(fake.deletes) != 2 {
			t.Fatalf("the provider got %d deletes, want the entry delivered again", len(fake.deletes))
		}
		if got := floorText(t, env); got != "t-9" {
			t.Fatalf("the floor is %q after the restart, want t-9", got)
		}
		if s, err := rt.Status(ctx, mustTenant(t, "t1"), alice, revocation.ID()); err != nil || s.State != RevocationComplete || s.Token != tok(9) {
			t.Fatalf("the revocation is %+v, %v after the restart, want complete with t-9", s, err)
		}
	})

	t.Run("ST-SUP-RP06-LC14-T01 A late authz entry raises one alert each time it crosses the bound", func(t *testing.T) {
		clock := &testClock{t: time.Now()}
		env := newPGClock(t, clock)
		reader, meter, _, _ := observed(t)
		relay, alerts := alertRelay(t)
		blocked := make(chan struct{})
		fake := &fakeRelationships{before: func(int, string) { <-blocked }}
		worker := env.newWorker(t, env.relay(t, fake), func(c *outbox.WorkerConfig) { c.Meter = meter })
		age := oldestAge(reader)
		step := func(want int64) {
			t.Helper()
			relay.check(ctx, age)
			if metric, events := alerts(); metric != want || events != want {
				t.Fatalf("there are %d alert metrics and %d events, want %d", metric, events, want)
			}
		}
		// Three entries wait while the provider blocks, and the clock passes 6 seconds.
		for _, subject := range []string{"a", "b", "c"} {
			env.enqueue(t, "t1", OperationTouch, Tuple{Resource: doc(subject), Relation: "viewer", Subject: user("bob")})
		}
		step(0)
		clock.add(6 * time.Second)
		step(1)
		step(1)
		// The provider is released, and the entries land.
		close(blocked)
		fake.before = nil
		run(t, worker)
		env.waitAll(t, outbox.Done)
		step(1)
		// One more entry waits while the provider blocks again, and the clock passes 6 seconds again.
		again := make(chan struct{})
		defer close(again)
		fake.mu.Lock()
		fake.before = func(int, string) { <-again }
		fake.mu.Unlock()
		env.enqueue(t, "t1", OperationTouch, Tuple{Resource: doc("d"), Relation: "viewer", Subject: user("bob")})
		clock.add(6 * time.Second)
		step(2)
	})

	t.Run("ST-SUP-RP06-LC14-T02 A failed read of the oldest age is counted and alerts", func(t *testing.T) {
		relay, alerts := alertRelay(t)
		failing := errors.New("the gauge cannot be read")
		readings := []func(context.Context) (time.Duration, error){
			func(context.Context) (time.Duration, error) { return 0, failing },
			func(context.Context) (time.Duration, error) { return 0, failing },
			func(context.Context) (time.Duration, error) { return 0, failing },
			func(context.Context) (time.Duration, error) { return time.Second, nil },
		}
		watched, cancel := context.WithCancel(ctx)
		defer cancel()
		next := make(chan func(context.Context) (time.Duration, error))
		done := make(chan error, 1)
		go func() {
			done <- relay.Watch(watched, time.Millisecond, func(ctx context.Context) (time.Duration, error) {
				select {
				case read := <-next:
					return read(ctx)
				case <-ctx.Done():
					return 0, ctx.Err()
				}
			})
		}()
		for _, read := range readings {
			next <- read
		}
		next <- func(context.Context) (time.Duration, error) { return time.Second, nil }
		cancel()
		<-done
		if got := relay.ReadFailures(); got != 3 {
			t.Fatalf("the failure counter is %d, want 3", got)
		}
		if metric, events := alerts(); metric != 1 || events != 1 {
			t.Fatalf("there are %d alert metrics and %d events, want one at the first failure", metric, events)
		}
	})

	t.Run("ST-SUP-RP06-LC16-T01 The delivery alert bound defaults to 5 seconds and refuses a bound that is not positive", func(t *testing.T) {
		config := RelayConfig{Name: "authz-relay", Relationships: &fakeRelationships{}, DB: fakeDB(t), Schema: "public"}
		relay, err := NewRelay(config)
		if err != nil {
			t.Fatal(err)
		}
		if relay.AlertBound() != 5*time.Second {
			t.Fatalf("the bound is %s, want 5s", relay.AlertBound())
		}
		for _, bound := range []time.Duration{0, -time.Second} {
			if _, err := NewRelay(config, WithAlertBound(bound)); err == nil {
				t.Errorf("the bound %s is accepted", bound)
			}
		}
		if relay, err := NewRelay(config, WithAlertBound(2*time.Second)); err != nil || relay.AlertBound() != 2*time.Second {
			t.Errorf("the bound 2s gave %v, %v", relay, err)
		}
	})
}

// person is the subject of the verified identity of the test for the user ID id, as a handle bound to it decides.
func person(t *testing.T, id string) Subject {
	t.Helper()
	return mustPerson(t, id).Subject()
}

// gated is a relationships port over a real one whose write with the floor marker waits for its gate, so a test can
// hold a relay inside its floor transaction. It records the tokens of those writes.
type gated struct {
	Relationships
	entered chan struct{}
	gate    chan struct{}
	mu      sync.Mutex
	heads   []Token
}

func (g *gated) Write(ctx context.Context, touches []Tuple, deletes []Filter, preconditions []Precondition) (Token, error) {
	marker := false
	for _, tuple := range touches {
		marker = marker || tuple == DefaultMarker
	}
	if marker && g.gate != nil {
		close(g.entered)
		<-g.gate
	}
	token, err := g.Relationships.Write(ctx, touches, deletes, preconditions)
	if marker {
		g.mu.Lock()
		g.heads = append(g.heads, token)
		g.mu.Unlock()
	}
	return token, err
}

// TestRevocationFloor proves the floor against local SpiceDB servers: a group revocation, a ban and a revocation in
// another tenant are in force on a resource whose row token is older, two relays take the floor lock in turn, and
// the floor works on the PostgreSQL datastore. On the quantized server each case first shows that the provider
// alone, at the old row token, still reads the state before the revocation, so the deny comes from the floor.
func TestRevocationForgery(t *testing.T) {
	ctx := context.Background()

	t.Run("ST-SUP-RP06-LC21-T02 The runtime role cannot forge a completed revocation", func(t *testing.T) {
		env := newPG(t)
		rt := env.runtimeOf(t, &recorder{}, nil)
		fake := &fakeRelationships{}
		tx, handle := env.begin(t, rt, "t1", mustPerson(t, "alice"), rel("document#viewer"))
		revocation, err := handle.Unrelate(ctx, Tuple{Resource: doc("1"), Relation: "viewer", Subject: user("bob")})
		if err != nil {
			t.Fatal(err)
		}
		env.commit(t, tx, "t1")
		id := env.ids(t)[0]
		// A message of another destination in the same group, which the runtime role may enqueue.
		other := outbox.Message{ID: uuid.New(), Destination: "billing", ResourceKey: "charge:1", Kind: "charge",
			Payload: []byte(`{}`), Group: revocation.ID().String()}
		asRuntime := func(work func(tx *sql.Tx) error) error {
			tx, err := tenant.Begin(ctx, env.runtime, mustTenant(t, "t1"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			if err := work(tx); err != nil {
				return err
			}
			return tenant.Commit(ctx, tx, mustTenant(t, "t1"))
		}
		if err := asRuntime(func(tx *sql.Tx) error { return env.queue.Enqueue(ctx, tx, other) }); err != nil {
			t.Fatal(err)
		}
		// The runtime role cannot stall the revocation with tuple rows: one for the billing message, one for a message
		// that does not exist, and one for an authz message of an earlier transaction are each refused at the commit.
		earlier := outbox.Message{ID: uuid.New(), Destination: DestinationName, ResourceKey: "document:9", Kind: "authz.touch",
			Payload: []byte(`{}`), Group: revocation.ID().String()}
		if err := asRuntime(func(tx *sql.Tx) error { return env.queue.Enqueue(ctx, tx, earlier) }); err != nil {
			t.Fatal(err)
		}
		for name, message := range map[string]uuid.UUID{"the billing message": other.ID, "no message": uuid.New(),
			"an authz message of an earlier transaction": earlier.ID} {
			if err := asRuntime(func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, "INSERT INTO authz.authz_revocation_tuples (message_id, revocation_id, tenant_id) VALUES ($1, $2, 't1')",
					message, revocation.ID())
				return err
			}); err == nil {
				t.Errorf("a tuple row for %s was committed", name)
			}
		}
		// A forged tuple row for the billing message, as the superuser, so the hook role's call below can be tried.
		conn, err := env.admin.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, statement := range []string{"SET session_replication_role = replica",
			"INSERT INTO authz.authz_revocation_tuples (message_id, revocation_id, tenant_id) VALUES ('" + other.ID.String() + "', '" + revocation.ID().String() + "', 't1')",
			"RESET session_replication_role"} {
			if _, err := conn.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
		_ = conn.Close()
		if err := asRuntime(func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "UPDATE stego_outbox.messages SET state = 'done' WHERE id = $1", id)
			return err
		}); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("the runtime role's UPDATE of the message gave %v, want a permission error", err)
		}
		for name, message := range map[string]uuid.UUID{"the authz message": id, "a message of billing": other.ID} {
			if err := asRuntime(func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, "SELECT authz.authz_revocation_state($1, $2, 'done')", message, revocation.ID())
				return err
			}); err == nil {
				t.Errorf("the state function took done for %s from the runtime role", name)
			}
		}
		// The worker role, a hook role, still gets no count for a message of another destination.
		var counted bool
		// The worker role cannot set a state either; the test sets the billing message done as the superuser.
		if _, err := env.admin.ExecContext(ctx, "UPDATE stego_outbox.messages SET state = 'done' WHERE id = $1", other.ID); err != nil {
			t.Fatal(err)
		}
		wtx, err := tenant.Begin(ctx, env.workerDB, mustTenant(t, "t1"), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = wtx.Rollback() }()
		if _, err := wtx.ExecContext(ctx, "SAVEPOINT s"); err != nil {
			t.Fatal(err)
		}
		if _, err := wtx.ExecContext(ctx, "UPDATE stego_outbox.messages SET state = 'done' WHERE id = $1", id); err == nil ||
			!strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("the worker role's UPDATE of the message gave %v, want a permission error", err)
		}
		if _, err := wtx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT s"); err != nil {
			t.Fatal(err)
		}
		if err := wtx.QueryRowContext(ctx, "SELECT authz.authz_revocation_state($1, $2, 'done')", other.ID, revocation.ID()).Scan(&counted); err != nil || counted {
			t.Fatalf("the hook role's call for a message of billing gave %t, %v; want no count", counted, err)
		}
		_ = wtx.Rollback()
		if s, err := rt.Status(ctx, mustTenant(t, "t1"), mustPerson(t, "alice"), revocation.ID()); err != nil || s.State != RevocationPending {
			t.Fatalf("the revocation is %+v, %v; want still pending", s, err)
		}
		if len(fake.touches)+len(fake.deletes) != 0 {
			t.Fatal("the provider got a write")
		}
		// Only READ COMMITTED runs the function.
		rtx, err := env.workerDB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rtx.Rollback() }()
		if _, err := rtx.ExecContext(ctx, "SELECT pg_catalog.set_config('app.tenant', 't1', true)"); err != nil {
			t.Fatal(err)
		}
		if _, err := rtx.ExecContext(ctx, "SELECT authz.authz_revocation_state($1, $2, 'parked')", id, revocation.ID()); err == nil ||
			!strings.Contains(err.Error(), "READ COMMITTED") {
			t.Fatalf("the function under REPEATABLE READ gave %v, want a refusal", err)
		}
	})
}

func TestRevocationFloor(t *testing.T) {
	ctx := context.Background()

	// ids gives the object IDs of one case, which the cases do not share on a shared server.
	ids := func(t *testing.T) (group, document string) {
		t.Helper()
		suffix := uuid.NewString()[:8]
		return "g-" + suffix, "d-" + suffix
	}
	// afterBoundary waits until the quantization boundary after the setup writes has passed, so that checks up to
	// the next boundary read the state at it, and returns the time of the next boundary.
	afterBoundary := func(t *testing.T) time.Time {
		t.Helper()
		next := time.Now().Truncate(quantum).Add(quantum)
		time.Sleep(time.Until(next) + quantum/5)
		return next.Add(quantum)
	}
	inWindow := func(t *testing.T, end time.Time) {
		t.Helper()
		if time.Now().After(end.Add(-quantum / 10)) {
			t.Fatalf("the case took longer than the quantization interval %s, so its stale read proves nothing", quantum)
		}
	}
	insert := func(t *testing.T, env *pgEnv, tenantID, documentID string, token Token) {
		t.Helper()
		if _, err := env.admin.ExecContext(ctx, "INSERT INTO "+env.schema+".documents (tenant_id, id, authz_token) VALUES ($1, $2, $3)",
			tenantID, documentID, token.Text()); err != nil {
			t.Fatal(err)
		}
	}
	rowToken := func(t *testing.T, env *pgEnv, tenantID, documentID string) Token {
		t.Helper()
		token, err := ParseToken(env.scalar(t, "SELECT authz_token FROM {s}.documents WHERE tenant_id = $1 AND id = $2", tenantID, documentID))
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	allowed := func(t *testing.T, rt *Runtime, env *pgEnv, tenantID, who, documentID string, row Token) bool {
		t.Helper()
		_, handle := env.begin(t, rt, tenantID, mustPerson(t, who))
		result, err := handle.Evaluate(ctx, "view", docIn(tenantID, documentID), row, nil)
		if err != nil {
			t.Fatal(err)
		}
		return result.Allowed
	}
	view := func(t *testing.T, rt *Runtime, env *pgEnv, tenantID, documentID string) bool {
		t.Helper()
		return allowed(t, rt, env, tenantID, "bob", documentID, rowToken(t, env, tenantID, documentID))
	}
	// stale checks view with the provider alone at the old row token: it must still allow, or the stand-in shows
	// no stale read and the case could not fail.
	stale := func(t *testing.T, db *spiceEnv, tenantID, documentID string, old Token) {
		t.Helper()
		result, err := db.provider.Evaluate(ctx, EvaluationRequest{Subject: person(t, "bob"), Action: Action{Name: "view"},
			Resource: docIn(tenantID, documentID), Consistency: AtLeast(old)})
		if err != nil {
			t.Fatal(err)
		}
		if !result.Allowed {
			t.Fatal("the provider alone denies at the old row token: the stand-in shows no stale read, so the case cannot fail")
		}
	}
	// landAll runs the outbox worker with the relay until every message is done.
	landAll := func(t *testing.T, env *pgEnv, relay *Relay, _ string) {
		t.Helper()
		env.startWorker(t, relay)
		env.waitAll(t, outbox.Done)
	}
	// groupGrant makes bob a member of the group of t1 and gives that group view on the document of tenantID. The
	// grant is written to the provider directly: a document of t2 with a group of t1 is only for LC13-T06, whose
	// revocation runs in t1 and whose check runs in t2.
	groupGrant := func(t *testing.T, db *spiceEnv, group, tenantID, documentID string) (Tuple, Token) {
		t.Helper()
		groupResource := Resource{Type: "group", ID: qualify("t1", group)}
		member := Tuple{Resource: groupResource, Relation: "member", Subject: person(t, "bob")}
		if _, err := db.provider.Touch(ctx, []Tuple{member}, nil); err != nil {
			t.Fatal(err)
		}
		grant := Tuple{Resource: docIn(tenantID, documentID), Relation: "viewer", Subject: Subject{Type: "group", ID: groupResource.ID}, SubjectRelation: "member"}
		token, err := db.provider.Touch(ctx, []Tuple{grant}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return member, token
	}

	t.Run("ST-SUP-RP06-LC13-T03 A check after a group revocation denies on a resource whose row token is older", func(t *testing.T) {
		env, db := newPG(t), startQuantizedSpiceDB(t, fixtureSchema)
		rt, relay := env.runtimeOf(t, db.provider, nil), env.relay(t, db.provider)
		group, d1 := ids(t)
		member, old := groupGrant(t, db, group, "t1", d1)
		insert(t, env, "t1", d1, old)
		if !view(t, rt, env, "t1", d1) {
			t.Fatal("bob cannot view the document through the group before the revocation")
		}
		end := afterBoundary(t)
		tx, handle := env.begin(t, rt, "t1", mustPerson(t, "alice"), rel("group#member"))
		if _, err := handle.Unrelate(ctx, member); err != nil {
			t.Fatal(err)
		}
		env.commit(t, tx, "t1")
		landAll(t, env, relay, "t1")
		if rowToken(t, env, "t1", d1) != old {
			t.Fatal("the row token of the document changed; the case needs it older than the revocation")
		}
		stale(t, db, "t1", d1, old)
		if view(t, rt, env, "t1", d1) {
			t.Fatal("bob can view the document after the group revocation landed")
		}
		// Port, which a worker with no transaction of its own uses, checks at the floor of its default TableFloor too.
		result, err := rt.Port().Evaluate(ctx, EvaluationRequest{Subject: person(t, "bob"), Action: Action{Name: "view"},
			Resource: doc(d1), Consistency: AtLeast(old)})
		if err != nil || result.Allowed {
			t.Fatalf("Port at the old row token gave %+v, %v; want a deny at the floor", result, err)
		}
		inWindow(t, end)
	})

	t.Run("ST-SUP-RP06-LC13-T04 A touch that bans a subject is in force on every resource after it lands", func(t *testing.T) {
		env, db := newPG(t), startQuantizedSpiceDB(t, fixtureSchema)
		rt, relay := env.runtimeOf(t, db.provider, nil), env.relay(t, db.provider)
		_, d1 := ids(t)
		old, err := db.provider.Touch(ctx, []Tuple{{Resource: doc(d1), Relation: "viewer", Subject: person(t, "bob")}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		insert(t, env, "t1", d1, old)
		if !view(t, rt, env, "t1", d1) {
			t.Fatal("bob cannot view the document before the ban")
		}
		end := afterBoundary(t)
		tx, handle := env.begin(t, rt, "t1", mustPerson(t, "alice"), rel("document#banned"))
		if _, err := handle.Relate(ctx, Tuple{Resource: doc(d1), Relation: "banned", Subject: person(t, "bob")}); err != nil {
			t.Fatal(err)
		}
		env.commit(t, tx, "t1")
		// The relay stores no token on a row here, so the row of the document keeps its older token and only the
		// floor can put the ban in force.
		relay.owners = map[string]ownerTable{}
		landAll(t, env, relay, "t1")
		if rowToken(t, env, "t1", d1) != old {
			t.Fatal("the row token of the document changed; the case needs it older than the ban")
		}
		stale(t, db, "t1", d1, old)
		if view(t, rt, env, "t1", d1) {
			t.Fatal("bob can view the document after the ban landed")
		}
		inWindow(t, end)
	})

	t.Run("ST-SUP-RP06-LC13-T06 A revocation in one tenant is in force for a check in another tenant", func(t *testing.T) {
		env, db := newPG(t), startQuantizedSpiceDB(t, fixtureSchema)
		rt, relay := env.runtimeOf(t, db.provider, nil), env.relay(t, db.provider)
		group, d2 := ids(t)
		member, old := groupGrant(t, db, group, "t2", d2)
		insert(t, env, "t2", d2, old)
		if !view(t, rt, env, "t2", d2) {
			t.Fatal("bob cannot view the document of t2 before the revocation")
		}
		end := afterBoundary(t)
		tx, handle := env.begin(t, rt, "t1", mustPerson(t, "alice"), rel("group#member"))
		if _, err := handle.Unrelate(ctx, member); err != nil {
			t.Fatal(err)
		}
		env.commit(t, tx, "t1")
		landAll(t, env, relay, "t1")
		stale(t, db, "t2", d2, old)
		if view(t, rt, env, "t2", d2) {
			t.Fatal("bob can view the document of t2 after the revocation in t1 landed")
		}
		inWindow(t, end)
	})

	t.Run("ST-SUP-RP06-LC13-T07 Concurrent relays take the floor lock in turn and never lower the floor", func(t *testing.T) {
		env, db := newPG(t), startSpiceDB(t, fixtureSchema)
		rt := env.runtimeOf(t, db.provider, nil)
		group := Resource{Type: "group", ID: qualify("t1", "g")}
		member := Tuple{Resource: group, Relation: "member", Subject: person(t, "bob")}
		grant := Tuple{Resource: doc("d1"), Relation: "viewer", Subject: Subject{Type: "group", ID: group.ID}, SubjectRelation: "member"}
		carol := Tuple{Resource: docIn("t2", "d2"), Relation: "viewer", Subject: person(t, "carol")}
		for run := 0; run < 4; run++ {
			if _, err := db.provider.Touch(ctx, []Tuple{member, grant, carol}, nil); err != nil {
				t.Fatal(err)
			}
			// Each tuple is delivered in its own tenant: member in t1, carol in t2.
			first, second, firstTenant, secondTenant := member, carol, "t1", "t2"
			if run%2 == 1 {
				first, second, firstTenant, secondTenant = carol, member, "t2", "t1"
			}
			held := &gated{Relationships: db.provider, entered: make(chan struct{}), gate: make(chan struct{})}
			later := &gated{Relationships: db.provider}
			relayA, relayB := env.relay(t, held), env.relay(t, later)
			done := make(chan error, 2)
			idA, idB := env.enqueue(t, firstTenant, OperationDelete, first), env.enqueue(t, secondTenant, OperationDelete, second)
			go func() { done <- env.deliverByHand(t, relayA, idA) }()
			<-held.entered
			go func() { done <- env.deliverByHand(t, relayB, idB) }()
			// The second relay's floor transaction waits for the first one's lock.
			waited := false
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
				if env.scalar(t, "SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE usename = $1 AND wait_event_type = 'Lock'",
					env.roles[2]) == "1" {
					waited = true
					break
				}
			}
			close(held.gate)
			for range 2 {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
			if !waited {
				t.Fatalf("run %d: the second relay did not wait for the floor lock", run)
			}
			if len(later.heads) != 1 || floorText(t, env) != later.heads[0].Text() {
				t.Fatalf("run %d: the floor is %q, want the head %v that the later transaction read", run, floorText(t, env), later.heads)
			}
			if allowed(t, rt, env, "t1", "bob", "d1", Token{}) || allowed(t, rt, env, "t2", "carol", "d2", Token{}) {
				t.Fatalf("run %d: a removed relation still allows at the floor", run)
			}
		}
	})

	t.Run("ST-SUP-RP06-LC13-T09 The floor and a group revocation work on the SpiceDB PostgreSQL datastore", func(t *testing.T) {
		env, db := newPG(t), startPostgresSpiceDB(t, fixtureSchema)
		rt, relay := env.runtimeOf(t, db.provider, nil), env.relay(t, db.provider)
		group, d1 := ids(t)
		member, old := groupGrant(t, db, group, "t1", d1)
		insert(t, env, "t1", d1, old)
		if !view(t, rt, env, "t1", d1) {
			t.Fatal("bob cannot view the document through the group before the revocation")
		}
		end := afterBoundary(t)
		// The PostgreSQL datastore serves the first transaction after the quantization boundary: write one before
		// the revocation, so a check at the old row token reads the state before the revocation.
		if _, err := db.provider.Touch(ctx, []Tuple{{Resource: doc(d1 + "-other"), Relation: "viewer", Subject: user("erin")}}, nil); err != nil {
			t.Fatal(err)
		}
		tx, handle := env.begin(t, rt, "t1", mustPerson(t, "alice"), rel("group#member"))
		if _, err := handle.Unrelate(ctx, member); err != nil {
			t.Fatal(err)
		}
		env.commit(t, tx, "t1")
		landAll(t, env, relay, "t1")
		if floorText(t, env) == "" {
			t.Fatal("the floor is not raised")
		}
		if rowToken(t, env, "t1", d1) != old {
			t.Fatal("the row token of the document changed; the case needs it older than the revocation")
		}
		stale(t, db, "t1", d1, old)
		if view(t, rt, env, "t1", d1) {
			t.Fatal("bob can view the document after the group revocation landed")
		}
		inWindow(t, end)
	})
}

// idProbe wraps a provider of both ports and records every object ID that STEGO sends it: the resource of each
// evaluation, tuple write and delete, and the subject of each tuple and delete filter, direct or subject set.
type idProbe struct {
	Decision
	Relationships
	mu  sync.Mutex
	ids []string
}

func (p *idProbe) record(ids ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ids = append(p.ids, ids...)
}

func (p *idProbe) recordTuples(tuples []Tuple) {
	for _, tuple := range tuples {
		p.record(tuple.Resource.ID, tuple.Subject.ID)
	}
}

func (p *idProbe) Evaluate(ctx context.Context, request EvaluationRequest) (EvaluationResult, error) {
	p.record(request.Resource.ID)
	return p.Decision.Evaluate(ctx, request)
}

func (p *idProbe) EvaluateBatch(ctx context.Context, request BatchRequest) ([]EvaluationResult, error) {
	for _, evaluation := range request.Evaluations {
		p.record(evaluation.Resource.ID)
	}
	return p.Decision.EvaluateBatch(ctx, request)
}

func (p *idProbe) Touch(ctx context.Context, tuples []Tuple, preconditions []Precondition) (Token, error) {
	p.recordTuples(tuples)
	return p.Relationships.Touch(ctx, tuples, preconditions)
}

func (p *idProbe) Delete(ctx context.Context, filters []Filter, preconditions []Precondition) (Token, error) {
	for _, filter := range filters {
		p.record(filter.ResourceID, filter.SubjectID)
	}
	return p.Relationships.Delete(ctx, filters, preconditions)
}

func (p *idProbe) Write(ctx context.Context, touches []Tuple, deletes []Filter, preconditions []Precondition) (Token, error) {
	p.recordTuples(touches)
	for _, filter := range deletes {
		p.record(filter.ResourceID, filter.SubjectID)
	}
	return p.Relationships.Write(ctx, touches, deletes, preconditions)
}

func (p *idProbe) sent() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.ids...)
}

// droppedSum reads the metric authz.search.dropped of reader.
func droppedSum(t *testing.T, reader sdkmetric.Reader) int64 {
	t.Helper()
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok && m.Name == "authz.search.dropped" {
				for _, point := range sum.DataPoints {
					total += point.Value
				}
			}
		}
	}
	return total
}

func TestTenantResources(t *testing.T) {
	ctx := context.Background()
	order := func(t *testing.T, tenantID, key string) Resource {
		t.Helper()
		resource, err := NewResource(mustTenant(t, tenantID), "order", key)
		if err != nil {
			t.Fatal(err)
		}
		return resource
	}

	t.Run("ST-SUP-RP06-LC22-T01 The same key in two tenants does not share a permission", func(t *testing.T) {
		env, db := newPG(t), startSpiceDB(t, fixtureSchema)
		for _, statement := range []string{
			"CREATE TABLE {s}.orders (tenant_id text NOT NULL, id text NOT NULL, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id))",
			"ALTER TABLE {s}.orders ENABLE ROW LEVEL SECURITY",
			"ALTER TABLE {s}.orders FORCE ROW LEVEL SECURITY",
			"CREATE POLICY orders_tenant ON {s}.orders USING (tenant_id = current_setting('app.tenant', true)) WITH CHECK (tenant_id = current_setting('app.tenant', true))",
			"GRANT SELECT, UPDATE (authz_token) ON {s}.orders TO " + env.roles[1],
			"INSERT INTO {s}.orders (tenant_id, id) VALUES ('t1', 'o-1'), ('t2', 'o-1')",
		} {
			if _, err := env.admin.ExecContext(ctx, strings.ReplaceAll(statement, "{s}", env.schema)); err != nil {
				t.Fatalf("%.60s: %v", statement, err)
			}
		}
		probe := &idProbe{Decision: db.provider, Relationships: db.provider}
		reader := sdkmetric.NewManualReader()
		rt, err := New(Config{Provider: probe, DB: env.runtime, Schema: env.schema, Outbox: env.queue,
			Meter: sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))})
		if err != nil {
			t.Fatal(err)
		}
		relay, err := NewRelay(RelayConfig{Name: "authz-relay", Relationships: probe, DB: env.relayDB, Schema: env.schema,
			Owners: map[string]Owner{"order": {Table: "orders", Key: "id"}}})
		if err != nil {
			t.Fatal(err)
		}
		bob := mustPerson(t, "bob")
		tx, handle := env.begin(t, rt, "t2", mustPerson(t, "alice"), rel("order#viewer"))
		if _, err := handle.Relate(ctx, Tuple{Resource: order(t, "t2", "o-1"), Relation: "viewer", Subject: bob.Subject()}); err != nil {
			t.Fatal(err)
		}
		env.commit(t, tx, "t2")
		env.startWorker(t, relay)
		env.waitAll(t, outbox.Done)
		// The relay stored its token on o-1 of t2 only: the key part of the ID names the row, the tenant its tenant.
		if got := env.scalar(t, "SELECT string_agg(tenant_id || '=' || (authz_token <> '')::text, ',' ORDER BY tenant_id) FROM {s}.orders"); got != "t1=false,t2=true" {
			t.Fatalf("the rows hold the tokens %s, want only o-1 of t2 with a token", got)
		}
		type outcome struct {
			allowed bool
			listed  int
			found   []string
		}
		check := func(tenantID string) outcome {
			_, h := env.begin(t, rt, tenantID, bob)
			resource := order(t, tenantID, "o-1")
			result, err := h.Evaluate(ctx, "view", resource, Token{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := h.List("view", nil).Page(ctx, []Row{{Resource: resource}})
			if err != nil {
				t.Fatal(err)
			}
			page, err := h.Search(ctx, "view", "order", nil, Page{Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			return outcome{allowed: result.Allowed, listed: len(rows), found: page.Keys}
		}
		if got := check("t2"); !got.allowed || got.listed != 1 || len(got.found) != 1 || got.found[0] != "o-1" {
			t.Fatalf("in t2 bob got %+v, want an allow, one listed row and the search key o-1", got)
		}
		// In t1 the check is a deny, which the query runtime answers as not found (ST-SUP-RP11-LC06).
		if got := check("t1"); got.allowed || got.listed != 0 || len(got.found) != 0 {
			t.Fatalf("in t1 bob got %+v, want a deny, an empty list and an empty search", got)
		}
		// The search in t1 got o-1 of t2 from SpiceDB, which looks up across tenants, and dropped it.
		if rt.DroppedResults() != 1 || droppedSum(t, reader) != 1 {
			t.Fatalf("the runtime dropped %d search results, metric %d; want o-1 of t2 dropped once", rt.DroppedResults(), droppedSum(t, reader))
		}
		// Bob's grant is removed in t2 through the handle, so a delete filter reaches the provider too.
		tx, handle = env.begin(t, rt, "t2", mustPerson(t, "alice"), rel("order#viewer"))
		if _, err := handle.Unrelate(ctx, Tuple{Resource: order(t, "t2", "o-1"), Relation: "viewer", Subject: bob.Subject()}); err != nil {
			t.Fatal(err)
		}
		env.commit(t, tx, "t2")
		env.waitAll(t, outbox.Done)
		if got := check("t2"); got.allowed || got.listed != 0 {
			t.Fatalf("in t2 bob got %+v after the delete, want a deny", got)
		}
		// Every object ID that went to the provider is the floor marker, the subject ID of bob, or tenant-qualified.
		sent := probe.sent()
		seen := map[string]bool{}
		subjects := 0
		for _, id := range sent {
			switch {
			case id == DefaultMarker.Resource.ID:
			case id == bob.Subject().ID:
				subjects++
			default:
				if _, _, ok := splitResourceID(id); !ok {
					t.Errorf("the object ID %q went to the provider without its tenant", id)
				}
				seen[id] = true
			}
		}
		if !seen[order(t, "t1", "o-1").ID] || !seen[order(t, "t2", "o-1").ID] || len(seen) != 2 || subjects != 2 {
			t.Fatalf("the provider got the object IDs %v, want o-1 of t1 and of t2 in their tenant-qualified forms and bob in the touch and the delete", sent)
		}
	})

	t.Run("ST-SUP-RP06-LC22-T02 A search result of another tenant is dropped", func(t *testing.T) {
		fake := &recorder{search: func(SearchRequest) SearchResult {
			return SearchResult{Resources: []Resource{order(t, "t1", "o-1"), order(t, "t2", "o-2"), {Type: "order", ID: "o-3"},
				{Type: "document", ID: qualify("t1", "o-4")}}}
		}}
		reader := sdkmetric.NewManualReader()
		queue, err := outbox.New(fakeDB(t), outbox.Options{})
		if err != nil {
			t.Fatal(err)
		}
		rt, err := New(Config{Provider: fake, DB: fakeDB(t), Schema: "public", Outbox: queue,
			Meter: sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))})
		if err != nil {
			t.Fatal(err)
		}
		handle := bindUnit(t, rt, nil, mustPerson(t, "bob"))
		page, err := handle.Search(ctx, "view", "order", nil, Page{})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Resources) != 1 || page.Resources[0] != order(t, "t1", "o-1") || len(page.Keys) != 1 || page.Keys[0] != "o-1" {
			t.Fatalf("the page maps %v to the keys %v, want only o-1 of t1", page.Resources, page.Keys)
		}
		if rt.DroppedResults() != 3 || droppedSum(t, reader) != 3 {
			t.Fatalf("the runtime counted %d dropped results, metric %d; want o-2 of t2, the bare ID o-3 and the document o-4", rt.DroppedResults(), droppedSum(t, reader))
		}
	})

	t.Run("ST-SUP-RP06-LC22-T05 A search page is filled to its limit and is never empty before the end", func(t *testing.T) {
		// pages gives the provider pages in turn: each page holds its resources and a next token, except the last.
		searcher := func(pages ...[]Resource) *recorder {
			return &recorder{search: func(request SearchRequest) SearchResult {
				n := 0
				if cursor, _, ok := request.Page.Token.CursorFor("fake"); ok {
					n = int(cursor[0] - '0')
				}
				result := SearchResult{}
				if n < len(pages) {
					result.Resources = pages[n]
				}
				if n+1 < len(pages) {
					result.Next = NewPageToken("fake", string(rune('0'+n+1)), Fresh())
				}
				return result
			}}
		}
		foreign := []Resource{order(t, "t2", "o-9")}
		fake := searcher(foreign, foreign, []Resource{order(t, "t1", "o-1")}, []Resource{order(t, "t1", "o-2")}, []Resource{order(t, "t1", "o-3")})
		handle := bindUnit(t, newUnitRuntime(t, fake), nil, mustPerson(t, "bob"))
		page, err := handle.Search(ctx, "view", "order", nil, Page{Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Keys) != 2 || page.Keys[0] != "o-1" || page.Keys[1] != "o-2" || page.Next.IsZero() {
			t.Fatalf("the page holds %v with next %v, want o-1 and o-2 after two foreign pages, and a next page", page.Keys, page.Next)
		}
		if len(fake.searchCalls) != 4 || fake.searchCalls[3].Page.Limit != 1 || fake.searchCalls[0].Page.Limit != 2 {
			t.Fatalf("the provider got %d calls, want 4 with the limit 2 at first and 1 for the last one", len(fake.searchCalls))
		}
		last, err := handle.Search(ctx, "view", "order", nil, Page{Limit: 2, Token: page.Next})
		if err != nil || len(last.Keys) != 1 || last.Keys[0] != "o-3" || !last.Next.IsZero() {
			t.Fatalf("the last page is %v, %v; want o-3 and the end", last.Keys, err)
		}
		// With no limit, one result of the tenant fills the page.
		fake = searcher(foreign, []Resource{order(t, "t1", "o-1")}, []Resource{order(t, "t1", "o-2")})
		handle = bindUnit(t, newUnitRuntime(t, fake), nil, mustPerson(t, "bob"))
		if page, err := handle.Search(ctx, "view", "order", nil, Page{}); err != nil || len(page.Keys) != 1 || page.Keys[0] != "o-1" {
			t.Fatalf("the page with no limit is %v, %v; want o-1", page.Keys, err)
		}
		// Only foreign pages that end give an empty page at the end, and only foreign pages that do not end stop at
		// the bound with an error, never with an empty page and a next token.
		fake = searcher(foreign, foreign)
		handle = bindUnit(t, newUnitRuntime(t, fake), nil, mustPerson(t, "bob"))
		if page, err := handle.Search(ctx, "view", "order", nil, Page{Limit: 5}); err != nil || len(page.Keys) != 0 || !page.Next.IsZero() {
			t.Fatalf("foreign pages that end gave %v, %v; want an empty last page", page, err)
		}
		endless := &recorder{search: func(SearchRequest) SearchResult {
			return SearchResult{Resources: foreign, Next: NewPageToken("fake", "more", Fresh())}
		}}
		handle = bindUnit(t, newUnitRuntime(t, endless), nil, mustPerson(t, "bob"))
		if _, err := handle.Search(ctx, "view", "order", nil, Page{Limit: 5}); !errors.Is(err, ErrSearchExhausted) {
			t.Fatalf("endless foreign pages gave %v, want ErrSearchExhausted", err)
		}
		if len(endless.searchCalls) != maxSearchCalls || maxSearchCalls > 50 {
			t.Fatalf("the provider got %d calls, want the bound %d", len(endless.searchCalls), maxSearchCalls)
		}
	})

	t.Run("ST-SUP-RP06-LC22-T03 A resource ID of another tenant, or one without a tenant, is refused before the provider and the outbox", func(t *testing.T) {
		fake := &recorder{allow: func(EvaluationRequest) bool { return true }}
		rt := newUnitRuntime(t, fake)
		handle := bindUnit(t, rt, nil, mustPerson(t, "alice"), rel("order#viewer"), rel("order#parent"))
		other, bare := order(t, "t2", "o-1"), Resource{Type: "order", ID: "o-1"}
		for name, call := range map[string]func(Resource) error{
			"Evaluate": func(r Resource) error { _, err := handle.Evaluate(ctx, "view", r, Token{}, nil); return err },
			"EvaluateBatch": func(r Resource) error {
				_, err := handle.EvaluateBatch(ctx, "view", []Row{{Resource: order(t, "t1", "o-2")}, {Resource: r}}, nil)
				return err
			},
			"List": func(r Resource) error { _, err := handle.List("view", nil).Page(ctx, []Row{{Resource: r}}); return err },
			"DecideTenant": func(r Resource) error {
				_, _, err := handle.DecideTenant(ctx, tenant.ProvisionAction, "t9", r, nil)
				return err
			},
			"Relate": func(r Resource) error {
				_, err := handle.Relate(ctx, Tuple{Resource: r, Relation: "viewer", Subject: user("bob")})
				return err
			},
			"Unrelate": func(r Resource) error {
				_, err := handle.Unrelate(ctx, Tuple{Resource: r, Relation: "viewer", Subject: user("bob")})
				return err
			},
			"Relate of a direct subject": func(r Resource) error {
				_, err := handle.Relate(ctx, Tuple{Resource: order(t, "t1", "o-1"), Relation: "parent",
					Subject: Subject{Type: "folder", ID: r.ID}})
				return err
			},
			"Relate of a subject set": func(r Resource) error {
				_, err := handle.Relate(ctx, Tuple{Resource: order(t, "t1", "o-1"), Relation: "viewer",
					Subject: Subject{Type: "group", ID: r.ID}, SubjectRelation: "member"})
				return err
			},
			"Port.Evaluate": func(r Resource) error {
				_, err := rt.Port().Evaluate(ctx, EvaluationRequest{Subject: user("bob"), Action: Action{Name: "view"},
					Resource: r, Consistency: Fresh()})
				return err
			},
			"Port.EvaluateBatch": func(r Resource) error {
				_, err := rt.Port().EvaluateBatch(ctx, BatchRequest{Evaluations: []EvaluationRequest{{Subject: user("bob"),
					Action: Action{Name: "view"}, Resource: r, Consistency: Fresh()}}})
				return err
			},
		} {
			for kind, resource := range map[string]Resource{"of t2": other, "without a tenant": bare} {
				if strings.HasPrefix(name, "Port.") && kind == "of t2" {
					continue // Port has no tenant: it checks the form only.
				}
				if name == "Relate of a direct subject" && kind == "without a tenant" {
					continue // A direct subject without the shape of a resource ID is a principal, such as a person.
				}
				want := ErrOtherTenant
				if kind == "without a tenant" {
					want = ErrNotQualified
				}
				if err := call(resource); !errors.Is(err, want) {
					t.Errorf("%s of a resource %s gave %v, want %v", name, kind, err, want)
				}
			}
		}
		if n := fake.requests(); n != 0 {
			t.Fatalf("the provider got %d requests for refused resource IDs", n)
		}
		// The relay refuses a tuple whose resource or subject set names another tenant than the outbox row, so the
		// outbox parks it, and writes nothing.
		relationships := &fakeRelationships{}
		relay, err := NewRelay(RelayConfig{Name: "authz-relay", Relationships: relationships, DB: fakeDB(t), Schema: "public"})
		if err != nil {
			t.Fatal(err)
		}
		for name, tuple := range map[string]Tuple{
			"a resource of t2": {Resource: order(t, "t2", "o-1"), Relation: "viewer", Subject: user("bob")},
			"a subject set of t2": {Resource: order(t, "t1", "o-1"), Relation: "viewer",
				Subject: Subject{Type: "group", ID: qualify("t2", "g")}, SubjectRelation: "member"},
			"a bare resource ID": {Resource: Resource{Type: "order", ID: "o-1"}, Relation: "viewer", Subject: user("bob")},
			"a direct subject of t2": {Resource: order(t, "t1", "o-1"), Relation: "parent",
				Subject: Subject{Type: "folder", ID: qualify("t2", "f")}},
		} {
			body, err := json.Marshal(payload{Version: payloadVersion, Operation: OperationTouch, Revocation: uuid.New(),
				Tuple: tupleJSON{ResourceType: tuple.Resource.Type, ResourceID: tuple.Resource.ID, Relation: tuple.Relation,
					SubjectType: tuple.Subject.Type, SubjectID: tuple.Subject.ID, SubjectRelation: tuple.SubjectRelation}})
			if err != nil {
				t.Fatal(err)
			}
			delivery := outbox.Delivery{Message: outbox.Message{ID: uuid.New(), Destination: DestinationName,
				ResourceKey: "order:o-1", Payload: body}, Tenant: mustTenant(t, "t1")}
			err = relay.Deliver(ctx, floorTx(t, &floorSeq{}), delivery)
			if !errors.Is(err, ErrOtherTenant) && !errors.Is(err, ErrNotQualified) {
				t.Errorf("the delivery of %s gave %v, want a refusal of its tenant", name, err)
			}
			if !outbox.IsPermanent(err) {
				t.Errorf("the refusal of %s is not permanent, so the outbox would not park it: %v", name, err)
			}
		}
		// A tuple of t1 whose key is not the resource key of its outbox row is refused too.
		body, err := json.Marshal(payload{Version: payloadVersion, Operation: OperationTouch, Revocation: uuid.New(),
			Tuple: tupleJSON{ResourceType: "order", ResourceID: qualify("t1", "o-2"), Relation: "viewer", SubjectType: "user", SubjectID: "bob"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := relay.Deliver(ctx, floorTx(t, &floorSeq{}), outbox.Delivery{Message: outbox.Message{ID: uuid.New(),
			Destination: DestinationName, ResourceKey: "order:o-1", Payload: body}, Tenant: mustTenant(t, "t1")}); err == nil {
			t.Error("the delivery of o-2 under the resource key order:o-1 is not refused")
		}
		if relationships.starts != 0 || len(relationships.touches) != 0 {
			t.Fatalf("the relay wrote %d tuples of refused deliveries", len(relationships.touches))
		}
		// The outbox's Resolve names outbox_destination:<name>; Decide sends it in the form of the message's tenant.
		if _, err := rt.Decide(ctx, outbox.DecisionRequest{Subject: "user:ops", Permission: "resolve",
			Resource: "outbox_destination:billing", Tenant: mustTenant(t, "t1")}); err != nil {
			t.Fatal(err)
		}
		if got := fake.evaluations[0].Resource; got.Type != "outbox_destination" || got.ID != qualify("t1", "billing") {
			t.Fatalf("Decide sent %s, want outbox_destination of billing in the form of t1", got)
		}
		if _, err := rt.Decide(ctx, outbox.DecisionRequest{Subject: "user:ops", Permission: "resolve",
			Resource: "outbox_destination:billing"}); err == nil {
			t.Fatal("Decide with no tenant is not refused")
		}
	})

	t.Run("ST-SUP-RP06-LC22-T06 A tenant whose ID equals an issuer cannot become a delegate of that issuer's person", func(t *testing.T) {
		env, db := newPG(t), startSpiceDB(t, fixtureSchema)
		rt := env.runtimeOf(t, db.provider, nil)
		const issuer = "https://idp.authz.test/realms/r1"
		person, err := Person("user", identityOf(t, issuer, "k"))
		if err != nil {
			t.Fatal(err)
		}
		attacker := mustPerson(t, "mallory")
		// The victim tenant gives the person view on its report r.
		report, err := NewResource(mustTenant(t, "tv"), "report", "r")
		if err != nil {
			t.Fatal(err)
		}
		tx, handle := env.begin(t, rt, "tv", mustPerson(t, "alice"), rel("report#viewer"))
		if _, err := handle.Relate(ctx, Tuple{Resource: report, Relation: "viewer", Subject: person.Subject()}); err != nil {
			t.Fatal(err)
		}
		env.commit(t, tx, "tv")
		// The attacker's tenant has the issuer as its ID, and writes a delegate tuple on its own user resource whose
		// key is the person's subject value.
		own, err := NewResource(mustTenant(t, issuer), "user", "k")
		if err != nil {
			t.Fatal(err)
		}
		if own.ID == person.Subject().ID {
			t.Fatalf("the resource %s has the ID of the person's subject", own)
		}
		tx, handle = env.begin(t, rt, issuer, mustPerson(t, "alice"), rel("user#delegate"))
		if _, err := handle.Relate(ctx, Tuple{Resource: own, Relation: "delegate", Subject: attacker.Subject()}); err != nil {
			t.Fatal(err)
		}
		env.commit(t, tx, issuer)
		env.startWorker(t, env.relay(t, db.provider))
		env.waitAll(t, outbox.Done)
		can := func(who Principal) bool {
			t.Helper()
			_, h := env.begin(t, rt, "tv", who)
			result, err := h.Evaluate(ctx, "view", report, Token{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			return result.Allowed
		}
		if !can(person) {
			t.Fatal("the person cannot view the report, so the case proves nothing")
		}
		if can(attacker) {
			t.Fatal("the delegate tuple of the issuer's tenant gives the attacker the victim tenant's report")
		}
	})

	t.Run("ST-SUP-RP06-LC22-T07 A message of payload version 1 is upgraded with the tenant of its row and delivered", func(t *testing.T) {
		env := newPG(t)
		if _, err := env.admin.ExecContext(ctx, "INSERT INTO "+env.schema+".documents (tenant_id, id) VALUES ('t1', 'd1'), ('t2', 'd1')"); err != nil {
			t.Fatal(err)
		}
		fake := &fakeRelationships{}
		id := env.enqueueRaw(t, "t2", "document:d1", "authz.touch", []byte(`{"v":1,"op":"touch","revocation":"`+uuid.NewString()+
			`","tuple":{"resource_type":"document","resource_id":"d1","relation":"viewer","subject_type":"group","subject_id":"g","subject_relation":"member"}}`),
			uuid.NewString())
		env.startWorker(t, env.relay(t, fake))
		env.waitState(t, id, outbox.Done)
		fake.mu.Lock()
		touches := fake.touches
		fake.mu.Unlock()
		want := Tuple{Resource: docIn("t2", "d1"), Relation: "viewer", Subject: Subject{Type: "group", ID: qualify("t2", "g")}, SubjectRelation: "member"}
		if len(touches) != 1 || len(touches[0]) != 1 || touches[0][0] != want {
			t.Fatalf("the provider got %v, want the tuple of d1 and the group g in the form of t2", touches)
		}
		if got := env.scalar(t, "SELECT string_agg(tenant_id || '=' || authz_token, ',' ORDER BY tenant_id) FROM {s}.documents"); got != "t1=,t2=w-1" {
			t.Fatalf("the rows hold %s, want the token on d1 of t2 only", got)
		}
	})

	t.Run("ST-SUP-RP06-LC22-T04 The tenant-qualified form is exact, injective and within SpiceDB's ID rules", func(t *testing.T) {
		t1 := mustTenant(t, "t1")
		id, err := ResourceID(t1, "o-1")
		if err != nil || id != "dDE/by0x" {
			t.Fatalf("ResourceID(t1, o-1) is %q, %v; want dDE/by0x, base64url of t1, a /, and base64url of o-1", id, err)
		}
		forms := map[string]string{}
		pairs := [][2]string{{"t1", "o-1"}, {"t2", "o-1"}, {"a|b", "c"}, {"a", "b|c"}, {"a/b", "c"}, {"a", "b/c"}, {"t1", "ä/:*\x00 ü"}, {"t1", "*"},
			{"t1", strings.Repeat("k", 700)}}
		for _, pair := range pairs {
			id, err := ResourceID(mustTenant(t, pair[0]), pair[1])
			if err != nil {
				t.Fatalf("ResourceID(%q, %q): %v", pair[0], pair[1], err)
			}
			if earlier, ok := forms[id]; ok {
				t.Fatalf("%q and %q give the one ID %s", earlier, pair, id)
			}
			forms[id] = pair[0] + "/" + pair[1]
			reference := &v1.ObjectReference{ObjectType: "order", ObjectId: id}
			if err := reference.Validate(); err != nil {
				t.Errorf("SpiceDB refuses the ID of %q: %v", pair, err)
			}
			if tenantID, key, ok := splitResourceID(id); !ok || tenantID != pair[0] || key != pair[1] {
				t.Errorf("the ID of %q reads back as %q, %q, %t", pair, tenantID, key, ok)
			}
		}
		// The longest key that fits gives an ID of at most 1024 bytes, which SpiceDB accepts; one more byte is refused.
		longest := (MaxResourceIDBytes - len("dDE/")) * 3 / 4
		id, err = ResourceID(t1, strings.Repeat("k", longest))
		if err != nil || len(id) > MaxResourceIDBytes || (&v1.ObjectReference{ObjectType: "order", ObjectId: id}).Validate() != nil {
			t.Fatalf("the longest key gave %d bytes, %v", len(id), err)
		}
		if _, err := ResourceID(t1, strings.Repeat("k", longest+1)); err == nil {
			t.Fatal("a key whose ID is longer than 1024 bytes is not refused")
		}
		if _, err := ResourceID(tenant.Tenant{}, "o-1"); !errors.Is(err, tenant.ErrZero) {
			t.Fatalf("the zero tenant gave %v", err)
		}
		if _, err := ResourceID(t1, ""); err == nil {
			t.Fatal("an empty key is not refused")
		}
		if _, err := ServiceAccount("relay/x"); err == nil {
			t.Fatal("a service account name with a / is not refused")
		}
		// Only the one canonical form reads back: no padding, no other alphabet, no second |, no extra bits.
		// A person's subject ID, base64url(issuer)|base64url(subject), never has the shape of a resource ID, also when
		// the issuer equals a tenant ID; the | form of before is not the shape either.
		person := subjectID("t1", "o-1")
		for _, id := range []string{"o-1", "dDE/", "/by0x", "dDE=/by0x", "dDE/by0x/x", "dD+/by0x", "dDF/by0x", "dDE|by0x", person} {
			if tenantID, key, ok := splitResourceID(id); ok {
				t.Errorf("the ID %q, not a canonical form, reads back as %q, %q", id, tenantID, key)
			}
		}
	})
}
