package authz

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

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
	return e.enqueueRaw(t, id, tuple.Resource.String(), "authz."+string(operation), body, revocation.String())
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
		if got := floorText(t, env); got != "h-1" {
			t.Fatalf("the floor is %q after the first delivery, want h-1", got)
		}
		env.waitState(t, env.enqueue(t, "t2", OperationTouch, Tuple{Resource: doc("2"), Relation: "viewer", Subject: user("bob")}), outbox.Done)
		if got := floorText(t, env); got != "h-2" {
			t.Fatalf("the floor is %q after the second delivery, want h-2", got)
		}
		mu.Lock()
		defer mu.Unlock()
		// Each head read ran while the floor row lock was granted and before the floor commit.
		if len(underLock) != 2 || underLock[0] != "1/" || underLock[1] != "1/h-1" {
			t.Fatalf("at the head reads the lock and floor were %v, want [1/ 1/h-1]", underLock)
		}
		if got := env.scalar(t, "SELECT string_agg(authz_token, ',' ORDER BY id) FROM {s}.documents"); got != "w-1,w-2" {
			t.Fatalf("the rows hold %q, want the write tokens w-1,w-2", got)
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

// gated is a relationships port over a real one whose Head waits for its gate, so a test can hold a relay inside its
// floor transaction. It records the head tokens it reads.
type gated struct {
	Relationships
	entered chan struct{}
	gate    chan struct{}
	mu      sync.Mutex
	heads   []Token
}

func (g *gated) Head(ctx context.Context) (Token, error) {
	if g.gate != nil {
		close(g.entered)
		<-g.gate
	}
	head, err := g.Relationships.Head(ctx)
	g.mu.Lock()
	g.heads = append(g.heads, head)
	g.mu.Unlock()
	return head, err
}

// TestRevocationFloor proves the floor against local SpiceDB servers: a group revocation, a ban and a revocation in
// another tenant are in force on a resource whose row token is older, two relays take the floor lock in turn, and
// the floor works on the PostgreSQL datastore. On the quantized server each case first shows that the provider
// alone, at the old row token, still reads the state before the revocation, so the deny comes from the floor.
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
		result, err := handle.Evaluate(ctx, "view", doc(documentID), row, nil)
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
	stale := func(t *testing.T, db *spiceEnv, documentID string, old Token) {
		t.Helper()
		result, err := db.provider.Evaluate(ctx, EvaluationRequest{Subject: user("bob"), Action: Action{Name: "view"},
			Resource: doc(documentID), Consistency: AtLeast(old)})
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
	groupGrant := func(t *testing.T, db *spiceEnv, group, documentID string) (Tuple, Token) {
		t.Helper()
		member := Tuple{Resource: Resource{Type: "group", ID: group}, Relation: "member", Subject: user("bob")}
		if _, err := db.provider.Touch(ctx, []Tuple{member}, nil); err != nil {
			t.Fatal(err)
		}
		grant := Tuple{Resource: doc(documentID), Relation: "viewer", Subject: Subject{Type: "group", ID: group}, SubjectRelation: "member"}
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
		member, old := groupGrant(t, db, group, d1)
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
		stale(t, db, d1, old)
		if view(t, rt, env, "t1", d1) {
			t.Fatal("bob can view the document after the group revocation landed")
		}
		// Port, which a worker with no transaction of its own uses, checks at the floor of its default TableFloor too.
		result, err := rt.Port().Evaluate(ctx, EvaluationRequest{Subject: user("bob"), Action: Action{Name: "view"},
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
		old, err := db.provider.Touch(ctx, []Tuple{{Resource: doc(d1), Relation: "viewer", Subject: user("bob")}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		insert(t, env, "t1", d1, old)
		if !view(t, rt, env, "t1", d1) {
			t.Fatal("bob cannot view the document before the ban")
		}
		end := afterBoundary(t)
		tx, handle := env.begin(t, rt, "t1", mustPerson(t, "alice"), rel("document#banned"))
		if _, err := handle.Relate(ctx, Tuple{Resource: doc(d1), Relation: "banned", Subject: user("bob")}); err != nil {
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
		stale(t, db, d1, old)
		if view(t, rt, env, "t1", d1) {
			t.Fatal("bob can view the document after the ban landed")
		}
		inWindow(t, end)
	})

	t.Run("ST-SUP-RP06-LC13-T06 A revocation in one tenant is in force for a check in another tenant", func(t *testing.T) {
		env, db := newPG(t), startQuantizedSpiceDB(t, fixtureSchema)
		rt, relay := env.runtimeOf(t, db.provider, nil), env.relay(t, db.provider)
		group, d2 := ids(t)
		member, old := groupGrant(t, db, group, d2)
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
		stale(t, db, d2, old)
		if view(t, rt, env, "t2", d2) {
			t.Fatal("bob can view the document of t2 after the revocation in t1 landed")
		}
		inWindow(t, end)
	})

	t.Run("ST-SUP-RP06-LC13-T07 Concurrent relays take the floor lock in turn and never lower the floor", func(t *testing.T) {
		env, db := newPG(t), startSpiceDB(t, fixtureSchema)
		rt := env.runtimeOf(t, db.provider, nil)
		member := Tuple{Resource: Resource{Type: "group", ID: "g"}, Relation: "member", Subject: user("bob")}
		grant := Tuple{Resource: doc("d1"), Relation: "viewer", Subject: Subject{Type: "group", ID: "g"}, SubjectRelation: "member"}
		carol := Tuple{Resource: doc("d2"), Relation: "viewer", Subject: user("carol")}
		for run := 0; run < 4; run++ {
			if _, err := db.provider.Touch(ctx, []Tuple{member, grant, carol}, nil); err != nil {
				t.Fatal(err)
			}
			first, second := member, carol
			if run%2 == 1 {
				first, second = carol, member
			}
			held := &gated{Relationships: db.provider, entered: make(chan struct{}), gate: make(chan struct{})}
			later := &gated{Relationships: db.provider}
			relayA, relayB := env.relay(t, held), env.relay(t, later)
			done := make(chan error, 2)
			idA, idB := env.enqueue(t, "t1", OperationDelete, first), env.enqueue(t, "t2", OperationDelete, second)
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
		member, old := groupGrant(t, db, group, d1)
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
		stale(t, db, d1, old)
		if view(t, rt, env, "t1", d1) {
			t.Fatal("bob can view the document after the group revocation landed")
		}
		inWindow(t, end)
	})
}
