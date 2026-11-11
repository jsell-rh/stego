package outbox

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/idempotency"
	"github.com/jsell-rh/stego/registry/runtime/internal/seal"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// fakeDestination is the fake destination billing: an HTTP server that records each request it gets with its
// Idempotency-Key, and answers as the test tells it. Its handler sends each delivery as one request with the
// message's outgoing key.
type fakeDestination struct {
	server  *httptest.Server
	client  *http.Client
	mu      sync.Mutex
	answers []string // ok, hold, close, refuse, unavailable; the last one repeats
	keys    []string
	kinds   []string
}

func newDestination(t *testing.T, answers ...string) *fakeDestination {
	t.Helper()
	f := &fakeDestination{answers: answers, client: &http.Client{}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		f.mu.Lock()
		f.keys = append(f.keys, r.Header.Get(idempotency.KeyHeader))
		f.kinds = append(f.kinds, r.Header.Get("X-Kind"))
		answer := "ok"
		if len(f.answers) > 0 {
			answer = f.answers[0]
			if len(f.answers) > 1 {
				f.answers = f.answers[1:]
			}
		}
		f.mu.Unlock()
		switch answer {
		case "hold":
			<-r.Context().Done()
		case "close":
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		case "refuse":
			w.WriteHeader(http.StatusBadRequest)
		case "unavailable":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(func() {
		f.client.CloseIdleConnections()
		f.server.Close()
	})
	return f
}

func (f *fakeDestination) handler(ctx context.Context, _ *sql.Tx, d Delivery) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, f.server.URL, bytes.NewReader(d.Payload))
	if err != nil {
		return NotSent(err)
	}
	request.Header.Set(idempotency.KeyHeader, d.OutgoingKey)
	request.Header.Set("X-Kind", d.Kind)
	response, err := f.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusServiceUnavailable:
		return TransientRefusal(errors.New(response.Status))
	case response.StatusCode >= 400:
		return PermanentRefusal(errors.New(response.Status))
	}
	return nil
}

func (f *fakeDestination) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys...)
}

func (f *fakeDestination) requestKinds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.kinds...)
}

// fakeDecider is the fake decision port: it records each request and answers allow, deny or an error.
type fakeDecider struct {
	mu    sync.Mutex
	allow bool
	err   error
	calls []DecisionRequest
}

func (d *fakeDecider) Decide(_ context.Context, request DecisionRequest) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, request)
	return d.allow, d.err
}

// tableAuditor writes the audit row of a resolution into the table audit_rows through the caller's transaction. It
// stands in for the audit runtime (ST-SUP-RP05), which is not on main yet; the contract it proves is that the row is
// written through the same transaction as the state change.
type tableAuditor struct{}

func (tableAuditor) Record(ctx context.Context, tx *sql.Tx, e AuditEntry) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO audit_rows (message_id, tenant_id, destination, subject, resolution, at)
		VALUES ($1, $2, $3, $4, $5, $6)`, e.MessageID, e.Tenant.ID(), e.Destination, e.Subject, string(e.Resolution), e.At)
	return err
}

// resolveAs resolves a message in a transaction of tenant-a.
func resolveAs(t *testing.T, db *sql.DB, w *Worker, id uuid.UUID, subject string, r Resolution) error {
	t.Helper()
	return resolveIn(t, db, w, "tenant-a", id, subject, r)
}

// resolveIn resolves a message in a transaction of the tenant tenantID and commits it with tenant.Commit.
func resolveIn(t *testing.T, db *sql.DB, w *Worker, tenantID string, id uuid.UUID, subject string, r Resolution) error {
	t.Helper()
	tn := testTenant(t, tenantID)
	tx, err := tenant.Begin(context.Background(), db, tn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	resolved, err := w.Resolve(context.Background(), tx, ResolveRequest{ID: id, Destination: destinationOf(t, tx, id),
		Principal: person(subject), Resolution: r})
	if err != nil {
		return err
	}
	if err := tenant.Commit(context.Background(), tx, tn); err != nil {
		return err
	}
	resolved.Committed(context.Background())
	return nil
}

// testPrincipal is a principal with a seal mark, as authz.Person gives; the package cannot import the
// authorization runtime, which imports it.
type testPrincipal struct{ seal.PrincipalMark }

// person is the principal of the subject subject.
func person(subject string) Principal { return testPrincipal{seal.NewPrincipalMark(subject)} }

// destinationOf is the destination of the message id that tx sees, or none when tx does not see it.
func destinationOf(t *testing.T, tx *sql.Tx, id uuid.UUID) string {
	t.Helper()
	var destination string
	err := tx.QueryRowContext(context.Background(), `SELECT destination FROM stego_outbox.messages WHERE id = $1`, id).Scan(&destination)
	if errors.Is(err, sql.ErrNoRows) {
		return "none"
	}
	if err != nil {
		t.Fatal(err)
	}
	return destination
}

func resolve(t *testing.T, db *sql.DB, w *Worker, id uuid.UUID, r Resolution) {
	t.Helper()
	if err := resolveAs(t, db, w, id, "user:ops-1", r); err != nil {
		t.Fatal(err)
	}
}

func auditRows(t *testing.T, db *sql.DB, id uuid.UUID) int {
	return count(t, db, "SELECT count(*) FROM audit_rows WHERE message_id = $1", id)
}

// resolvingWorker is a worker with an allowing fake decision port and the table auditor.
func resolvingWorker(t *testing.T, q *Queue, decider *fakeDecider) *Worker {
	t.Helper()
	return newWorker(t, q, func(c *WorkerConfig) { c.Decider = decider; c.Auditor = tableAuditor{} })
}

func TestDispatchStates(t *testing.T) {
	t.Run("ST-SUP-RP01-LC08-T01 Every allowed transition is taken and every other one is refused", func(t *testing.T) {
		want := map[[2]State]bool{
			{Pending, Dispatching}: true,
			{Dispatching, Done}:    true, {Dispatching, Pending}: true, {Dispatching, OutcomeUnknown}: true, {Dispatching, Parked}: true,
			{OutcomeUnknown, Dispatching}: true, {OutcomeUnknown, Reconciling}: true, {OutcomeUnknown, Parked}: true,
			{Reconciling, Done}: true, {Reconciling, Pending}: true, {Reconciling, Parked}: true, {Reconciling, Reconciling}: true,
			{Parked, Done}: true, {Parked, Pending}: true, {Parked, Discarded}: true,
		}
		allowed := 0
		for _, from := range States() {
			for _, to := range States() {
				err := Transition(from, to)
				if (err == nil) != want[[2]State{from, to}] {
					t.Fatalf("%s to %s gives %v, want allowed %v", from, to, err, want[[2]State{from, to}])
				}
				if err == nil {
					allowed++
					continue
				}
				if !strings.Contains(err.Error(), string(from)) || !strings.Contains(err.Error(), string(to)) {
					t.Fatalf("the refusal %q does not name %s and %s", err, from, to)
				}
			}
		}
		if allowed != 15 || len(States()) != 7 {
			t.Fatalf("%d of %d pairs are allowed, want 15 of 49", allowed, len(States())*len(States()))
		}
		for _, pair := range [][2]State{{Pending, Dispatching}, {Dispatching, Pending}, {OutcomeUnknown, Reconciling},
			{Reconciling, Reconciling}, {Parked, Discarded}} {
			if err := Transition(pair[0], pair[1]); err != nil {
				t.Fatal(err)
			}
		}
		for _, pair := range [][2]State{{Pending, Done}, {Done, Pending}, {Discarded, Pending}, {OutcomeUnknown, Pending}} {
			if err := Transition(pair[0], pair[1]); err == nil {
				t.Fatalf("%s to %s is allowed", pair[0], pair[1])
			}
		}
	})

	t.Run("ST-SUP-RP01-LC08-T02 A refused transition leaves the stored message unchanged", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, newClock())
		m := message("billing", "charge:1")
		enqueue(t, db, q, m)
		deliveries, err := q.Claim(context.Background(), 1, time.Minute)
		if err != nil || len(deliveries) != 1 {
			t.Fatalf("claim: %v %v", deliveries, err)
		}
		if ok, err := q.acknowledge(context.Background(), deliveries[0].Receipt); !ok || err != nil {
			t.Fatalf("acknowledge: %v %v", ok, err)
		}
		before := readRow(t, db, m.ID)
		ok, err := q.retry(context.Background(), deliveries[0].Receipt, 0, "late")
		if ok || err == nil || !strings.Contains(err.Error(), "done") || !strings.Contains(err.Error(), "pending") {
			t.Fatalf("the move from done to pending gave %v, %v", ok, err)
		}
		if after := readRow(t, db, m.ID); after != before || after.State != Done {
			t.Fatalf("the row changed from %+v to %+v", before, after)
		}
	})
}

func TestDispatchOutcome(t *testing.T) {
	t.Run("ST-SUP-RP01-LC09-T01 An attempt whose deadline passes ends in outcome unknown", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, newClock())
		m := message("billing", "charge:1")
		enqueue(t, db, q, m)
		billing := newDestination(t, "hold")
		w := newWorker(t, q, nil)
		register(t, w, Destination{Name: "billing", Handler: billing.handler})
		cycle(t, w)
		if row := readRow(t, db, m.ID); row.State != OutcomeUnknown || row.FailureCode != "delivery-timeout" || len(billing.requests()) != 1 {
			t.Fatalf("the timed out attempt left %+v", row)
		}
	})

	t.Run("ST-SUP-RP01-LC09-T02 A connection lost after the send ends in outcome unknown", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, newClock())
		m := message("billing", "charge:1")
		enqueue(t, db, q, m)
		billing := newDestination(t, "close")
		w := newWorker(t, q, nil)
		register(t, w, Destination{Name: "billing", Handler: billing.handler})
		cycle(t, w)
		if row := readRow(t, db, m.ID); row.State != OutcomeUnknown || len(billing.requests()) != 1 {
			t.Fatalf("the lost connection left %+v", row)
		}
	})

	t.Run("ST-SUP-RP01-LC09-T03 Only an error the handler marks is a known failure", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, newClock())
		plain, notSent, transient, permanent := message("billing", "a"), message("billing", "b"), message("billing", "c"), message("billing", "d")
		w := newWorker(t, q, nil)
		register(t, w, Destination{Name: "billing", Handler: func(_ context.Context, _ *sql.Tx, d Delivery) error {
			switch d.ID {
			case notSent.ID:
				return NotSent(errors.New("dial failed"))
			case transient.ID:
				return TransientRefusal(errors.New("503"))
			case permanent.ID:
				return PermanentRefusal(errors.New("400"))
			}
			return errors.New("something broke")
		}})
		// Each message is enqueued and dispatched alone, so the state is the end of its first attempt.
		for _, c := range []struct {
			m    Message
			want State
		}{{plain, OutcomeUnknown}, {notSent, Pending}, {transient, Pending}, {permanent, Parked}} {
			enqueue(t, db, q, c.m)
			if n := cycle(t, w); n != 1 {
				t.Fatalf("the cycle claimed %d messages", n)
			}
			if row := readRow(t, db, c.m.ID); row.State != c.want {
				t.Fatalf("a message ended in %s, want %s", row.State, c.want)
			}
			if _, err := admin(t, db).Exec("UPDATE stego_outbox.messages SET available_at = available_at + interval '1 day' WHERE id = $1", c.m.ID); err != nil {
				t.Fatal(err)
			}
		}
	})

	t.Run("ST-SUP-RP01-LC09-T03 A handler that panics ends in outcome unknown", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, newClock())
		m := message("billing", "charge:1")
		enqueue(t, db, q, m)
		w := newWorker(t, q, nil)
		register(t, w, Destination{Name: "billing", Handler: func(context.Context, *sql.Tx, Delivery) error {
			panic("the handler broke with password=hunter2")
		}})
		if n := cycle(t, w); n != 1 {
			t.Fatalf("the cycle claimed %d messages", n)
		}
		if row := readRow(t, db, m.ID); row.State != OutcomeUnknown || row.FailureCode != "handler-panic" {
			t.Fatalf("the panic left %+v", row)
		}
	})

	t.Run("ST-SUP-RP01-LC09-T04 A lease that expires while dispatching leaves an outcome unknown", func(t *testing.T) {
		db, dsn := testDatabase(t)
		m := message("billing", "charge:1")
		enqueue(t, db, newQueue(t, db, nil), m)
		runCrash(t, dsn, "handler")
		clock := newClock()
		w := newWorker(t, newQueue(t, db, clock), nil)
		register(t, w, Destination{Name: "billing", Handler: func(context.Context, *sql.Tx, Delivery) error { return nil }})
		cycle(t, w)
		if row := readRow(t, db, m.ID); row.State != OutcomeUnknown || row.Attempts != 1 || row.FailureCode != "lease-expired" {
			t.Fatalf("the expired lease left %+v", row)
		}
		cycle(t, w)
		if row := readRow(t, db, m.ID); row.State != Parked || row.ParkedFrom.String != "outcome_unknown" {
			t.Fatalf("the message went to %+v, not on from outcome unknown", row)
		}
	})
}

func TestDispatchAgain(t *testing.T) {
	t.Run("ST-SUP-RP01-LC10-T01 An idempotent destination gets the same key on the retry", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, newClock())
		m := message("billing", "charge:1")
		m.OutgoingKey = idempotency.Derive("tenant-a", "user:alice", "inbound-1", "billing", "charge")
		enqueue(t, db, q, m)
		billing := newDestination(t, "hold", "ok")
		w := newWorker(t, q, nil)
		register(t, w, Destination{Name: "billing", Idempotent: true, Handler: billing.handler})
		cycle(t, w)
		cycle(t, w)
		keys := billing.requests()
		if len(keys) != 2 || keys[0] != m.OutgoingKey || keys[1] != m.OutgoingKey || readRow(t, db, m.ID).State != Done {
			t.Fatalf("the requests carry %v, want two of %s", keys, m.OutgoingKey)
		}
	})

	t.Run("ST-SUP-RP01-LC10-T02 A destination that is not idempotent is not sent the request again", func(t *testing.T) {
		db, _ := testDatabase(t)
		clock := newClock()
		q := newQueue(t, db, clock)
		m := message("billing", "charge:1")
		enqueue(t, db, q, m)
		billing := newDestination(t, "hold", "ok")
		w := newWorker(t, q, nil)
		register(t, w, Destination{Name: "billing", Handler: billing.handler})
		cycle(t, w)
		for i := 0; i < 10; i++ {
			clock.Advance(time.Hour)
			cycle(t, w)
			if row := readRow(t, db, m.ID); row.State == Dispatching || row.Attempts != 1 {
				t.Fatalf("the message was dispatched again: %+v", row)
			}
		}
		if len(billing.requests()) != 1 || readRow(t, db, m.ID).State != Parked {
			t.Fatalf("%d requests, row %+v", len(billing.requests()), readRow(t, db, m.ID))
		}
	})

	t.Run("ST-SUP-RP01-LC10-T03 A message from no inbound request uses its identity as its key", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, newClock())
		m := message("billing", "charge:1")
		enqueue(t, db, q, m)
		billing := newDestination(t, "hold", "ok")
		w := newWorker(t, q, nil)
		register(t, w, Destination{Name: "billing", Idempotent: true, Handler: billing.handler})
		cycle(t, w)
		cycle(t, w)
		keys := billing.requests()
		if len(keys) != 2 || keys[0] != m.ID.String() || keys[1] != m.ID.String() {
			t.Fatalf("the requests carry %v, want the message identity %s", keys, m.ID)
		}
	})
}

func TestDispatchReconcile(t *testing.T) {
	setup := func(t *testing.T, edit func(*WorkerConfig), settle time.Duration, answers ...lookupAnswer) (*sql.DB, *fakeClock, *Worker, *fakeDestination, *recordingLookup, Message) {
		db, _ := testDatabase(t)
		clock := newClock()
		q := newQueue(t, db, clock)
		m := message("billing", "charge:1")
		enqueue(t, db, q, m)
		billing := newDestination(t, "hold", "ok")
		lookup := &recordingLookup{answers: answers, clock: clock}
		w := newWorker(t, q, edit)
		register(t, w, Destination{Name: "billing", Handler: billing.handler, Lookup: lookup.lookup, SettleTime: settle})
		return db, clock, w, billing, lookup, m
	}

	t.Run("ST-SUP-RP01-LC11-T01 A lookup that answers acted finishes the message with no second send", func(t *testing.T) {
		db, clock, w, billing, lookup, m := setup(t, nil, time.Second, lookupAnswer{status: StatusActed})
		cycle(t, w)
		cycle(t, w)
		if row := readRow(t, db, m.ID); row.State != Reconciling || lookup.count() != 0 {
			t.Fatalf("after the unknown outcome: %+v, %d lookups", row, lookup.count())
		}
		clock.Advance(2 * time.Second)
		cycle(t, w)
		if lookup.count() != 1 || lookup.requests[0].ID != m.ID || lookup.requests[0].OutgoingKey != m.ID.String() ||
			lookup.requests[0].Tenant.ID() != "tenant-a" {
			t.Fatalf("the lookups are %+v", lookup.requests)
		}
		cycle(t, w)
		if row := readRow(t, db, m.ID); row.State != Done || len(billing.requests()) != 1 {
			t.Fatalf("the message is %+v after %d requests", row, len(billing.requests()))
		}
	})

	t.Run("ST-SUP-RP01-LC11-T02 A lookup that answers not acted sends the message again", func(t *testing.T) {
		db, clock, w, billing, _, m := setup(t, nil, time.Second, lookupAnswer{status: StatusNotActed})
		var states []State
		for i := 0; i < 5; i++ {
			clock.Advance(2 * time.Second)
			cycle(t, w)
			states = append(states, readRow(t, db, m.ID).State)
		}
		want := []State{OutcomeUnknown, Reconciling, Pending, Done, Done}
		for i := range want {
			if states[i] != want[i] {
				t.Fatalf("the message passed %v, want %v", states, want)
			}
		}
		keys := billing.requests()
		if len(keys) != 2 || keys[0] != keys[1] {
			t.Fatalf("the requests carry %v", keys)
		}
	})

	t.Run("ST-SUP-RP01-LC11-T03 Lookups that do not answer park the message after the bound", func(t *testing.T) {
		db, clock, w, billing, lookup, m := setup(t, func(c *WorkerConfig) { c.LookupBound = 3 }, time.Second,
			lookupAnswer{status: StatusUnknown}, lookupAnswer{err: errors.New("the status API is down")}, lookupAnswer{status: StatusUnknown})
		cycle(t, w)
		cycle(t, w)
		var delays []time.Duration
		for lookup.count() < 3 {
			row := readRow(t, db, m.ID)
			if row.State != Reconciling {
				t.Fatalf("the message left reconciling early: %+v", row)
			}
			clock.Set(row.AvailableAt)
			cycle(t, w)
			if after := readRow(t, db, m.ID); after.State == Reconciling {
				delays = append(delays, after.AvailableAt.Sub(clock.Now()))
			}
		}
		if row := readRow(t, db, m.ID); row.State != Parked || row.Lookups != 3 || len(billing.requests()) != 1 {
			t.Fatalf("after three lookups the message is %+v with %d requests", row, len(billing.requests()))
		}
		// The backoff after lookup n is from half of RetryMin times 2^(n-1) to RetryMin times 2^(n-1).
		if len(delays) != 2 || delays[1] <= delays[0] || delays[0] > 100*time.Millisecond || delays[1] < 100*time.Millisecond {
			t.Fatalf("the delays between the lookups are %v", delays)
		}
	})

	t.Run("ST-SUP-RP01-LC11-T04 The lookup waits for the attempt deadline plus the settle time", func(t *testing.T) {
		db, _ := testDatabase(t)
		clock := newClock()
		q := newQueue(t, db, clock)
		m := message("partner", "order:1")
		enqueue(t, db, q, m)
		lookup := &recordingLookup{clock: clock}
		w := newWorker(t, q, func(c *WorkerConfig) { c.AttemptTimeout = 10 * time.Second })
		sent := clock.Now()
		register(t, w, Destination{Name: "partner", Lookup: lookup.lookup, SettleTime: 30 * time.Second,
			Handler: func(context.Context, *sql.Tx, Delivery) error {
				clock.Advance(time.Second)
				return errors.New("the partner answered with a broken body")
			}})
		cycle(t, w)
		cycle(t, w)
		clock.Set(sent.Add(40*time.Second - time.Millisecond))
		cycle(t, w)
		if lookup.count() != 0 {
			t.Fatalf("a lookup ran %v after the send", lookup.times[0].Sub(sent))
		}
		clock.Set(sent.Add(40 * time.Second))
		cycle(t, w)
		if lookup.count() != 1 || lookup.times[0].Sub(sent) != 40*time.Second {
			t.Fatalf("the first lookup did not run at 40 s after the send: %v", lookup.times)
		}
	})

	t.Run("ST-SUP-RP01-LC11-T05 The lookup bound defaults to 10 lookups", func(t *testing.T) {
		db, clock, w, _, lookup, m := setup(t, func(c *WorkerConfig) { c.LookupBound = 0 }, time.Second)
		for i := 0; i < 40 && readRow(t, db, m.ID).State != Parked; i++ {
			clock.Advance(time.Minute)
			cycle(t, w)
		}
		if row := readRow(t, db, m.ID); row.State != Parked || lookup.count() != 10 || row.Lookups != 10 {
			t.Fatalf("the message is %+v after %d lookups", row, lookup.count())
		}
	})
}

// eventExporter keeps the log records that the worker emits.
type eventExporter struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (e *eventExporter) Export(_ context.Context, records []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range records {
		e.records = append(e.records, r.Clone())
	}
	return nil
}
func (e *eventExporter) Shutdown(context.Context) error   { return nil }
func (e *eventExporter) ForceFlush(context.Context) error { return nil }

func TestDispatchPark(t *testing.T) {
	t.Run("ST-SUP-RP01-LC12-T01 An unknown outcome with no safe next step is parked with an alert", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, newClock())
		m := message("billing", "charge:1")
		enqueue(t, db, q, m)
		billing := newDestination(t, "hold")
		reader, provider := metricReader()
		events := &eventExporter{}
		logs := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(events)))
		w := newWorker(t, q, func(c *WorkerConfig) { c.Meter = provider; c.Logger = logs })
		register(t, w, Destination{Name: "billing", Handler: billing.handler})
		cycle(t, w)
		cycle(t, w)
		cycle(t, w)
		if row := readRow(t, db, m.ID); row.State != Parked || row.ParkedFrom.String != "outcome_unknown" {
			t.Fatalf("the message is %+v", row)
		}
		found := collect(t, reader, "outbox.parked")
		if len(found) != 1 {
			t.Fatalf("outbox.parked is %+v", found)
		}
		sum, ok := found[0].Data.(metricdata.Sum[int64])
		want := attribute.NewSet(attribute.String("destination", "billing"), attribute.String("state", "outcome_unknown"))
		if !ok || len(sum.DataPoints) != 1 || sum.DataPoints[0].Value != 1 || !sum.DataPoints[0].Attributes.Equals(&want) {
			t.Fatalf("outbox.parked holds %+v", found[0].Data)
		}
		events.mu.Lock()
		defer events.mu.Unlock()
		if len(events.records) != 1 || events.records[0].EventName() != "outbox.parked" {
			t.Fatalf("the alert events are %+v", events.records)
		}
		got := map[string]string{}
		events.records[0].WalkAttributes(func(kv attribute.KeyValue) bool {
			got[string(kv.Key)] = kv.Value.AsString()
			return true
		})
		if got["destination"] != "billing" || got["state"] != "outcome_unknown" {
			t.Fatalf("the alert event names %v", got)
		}
	})

	t.Run("ST-SUP-RP01-LC12-T02 A parked key of one tenant does not hold back the same key of another tenant", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, newClock())
		parked, other, later := message("billing", "charge:1"), message("billing", "charge:1"), message("billing", "charge:1")
		enqueueAs(t, db, q, "tenant-a", parked)
		enqueueAs(t, db, q, "tenant-b", other)
		w := newWorker(t, q, nil)
		register(t, w, Destination{Name: "billing", Handler: func(_ context.Context, _ *sql.Tx, d Delivery) error {
			if d.ID == parked.ID || d.ID == later.ID {
				return PermanentRefusal(errors.New("400"))
			}
			return nil
		}})
		for i := 0; i < 3; i++ {
			cycle(t, w)
		}
		if a, b := readRow(t, db, parked.ID), readRow(t, db, other.ID); a.State != Parked || b.State != Done || b.TenantID != "tenant-b" {
			t.Fatalf("the messages are %+v and %+v", a, b)
		}
		// Each tenant's parked key is its own entry of the blocked view.
		enqueueAs(t, db, q, "tenant-b", later)
		cycle(t, w)
		blocked, err := q.Blocked(context.Background(), MaxBatchSize)
		if err != nil || len(blocked) != 2 || blocked[0].TenantID != "tenant-a" || blocked[1].TenantID != "tenant-b" {
			t.Fatalf("the blocked view is %+v %v", blocked, err)
		}
	})

	t.Run("ST-SUP-RP01-LC12-T02 A parked message holds back its own key only", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, newClock())
		first, second, other := message("billing", "charge:1"), message("billing", "charge:1"), message("billing", "charge:2")
		enqueue(t, db, q, first, second, other)
		w := newWorker(t, q, nil)
		register(t, w, Destination{Name: "billing", Handler: func(_ context.Context, _ *sql.Tx, d Delivery) error {
			if d.ID == first.ID {
				return PermanentRefusal(errors.New("400"))
			}
			return nil
		}})
		for i := 0; i < 4; i++ {
			cycle(t, w)
		}
		if a, b, c := readRow(t, db, first.ID), readRow(t, db, second.ID), readRow(t, db, other.ID); a.State != Parked || b.State != Pending || c.State != Done {
			t.Fatalf("the messages are %s, %s and %s", a.State, b.State, c.State)
		}
	})
}

func TestDispatchResolve(t *testing.T) {
	// parkOne parks one message of billing, which is not idempotent and has no lookup, after a permanent refusal.
	parkOne := func(t *testing.T, decider *fakeDecider, answers ...string) (*sql.DB, *Worker, *fakeDestination, Message) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, newClock())
		m := message("billing", "charge:1")
		enqueue(t, db, q, m)
		billing := newDestination(t, answers...)
		w := resolvingWorker(t, q, decider)
		register(t, w, Destination{Name: "billing", Handler: billing.handler})
		for i := 0; i < 3 && readRow(t, db, m.ID).State != Parked; i++ {
			cycle(t, w)
		}
		if row := readRow(t, db, m.ID); row.State != Parked {
			t.Fatalf("the message did not park: %+v", row)
		}
		return db, w, billing, m
	}

	t.Run("ST-SUP-RP01-LC13-T01 A parked message resolved as acted is done", func(t *testing.T) {
		db, w, billing, m := parkOne(t, &fakeDecider{allow: true}, "refuse")
		resolve(t, db, w, m.ID, Acted)
		cycle(t, w)
		row := readRow(t, db, m.ID)
		if row.State != Done || row.ResolvedBy.String != "user:ops-1" || row.Resolution.String != "acted" || !row.ResolvedAt.Valid ||
			len(billing.requests()) != 1 || auditRows(t, db, m.ID) != 1 {
			t.Fatalf("the resolved message is %+v after %d requests", row, len(billing.requests()))
		}
	})

	t.Run("ST-SUP-RP01-LC13-T02 A parked message resolved as not acted is sent again with its key", func(t *testing.T) {
		db, w, billing, m := parkOne(t, &fakeDecider{allow: true}, "hold", "ok")
		if row := readRow(t, db, m.ID); row.ParkedFrom.String != "outcome_unknown" {
			t.Fatalf("the message did not park after the timeout: %+v", row)
		}
		resolve(t, db, w, m.ID, NotActed)
		if row := readRow(t, db, m.ID); row.State != Pending || row.OutgoingKey != m.ID.String() {
			t.Fatalf("the resolved message is %+v", row)
		}
		cycle(t, w)
		keys := billing.requests()
		if len(keys) != 2 || keys[1] != keys[0] || readRow(t, db, m.ID).State != Done {
			t.Fatalf("the requests carry %v, row %+v", keys, readRow(t, db, m.ID))
		}
	})

	t.Run("ST-SUP-RP01-LC13-T03 A message that is not parked cannot be resolved", func(t *testing.T) {
		db, _ := testDatabase(t)
		clock := newClock()
		q := newQueue(t, db, clock)
		pending, reconciling, done := message("idle", "a"), message("partner", "b"), message("mail", "c")
		enqueue(t, db, q, reconciling, done, pending)
		w := resolvingWorker(t, q, &fakeDecider{allow: true})
		register(t, w, Destination{Name: "partner", Handler: func(context.Context, *sql.Tx, Delivery) error { return errors.New("broken") },
			Lookup: func(context.Context, LookupRequest) (LookupStatus, error) { return StatusUnknown, nil }, SettleTime: time.Hour})
		register(t, w, Destination{Name: "mail", Handler: func(context.Context, *sql.Tx, Delivery) error { return nil }})
		register(t, w, Destination{Name: "idle", MustLand: true, Handler: func(context.Context, *sql.Tx, Delivery) error { return nil }})
		cycle(t, w)
		cycle(t, w)
		cycle(t, w)
		if _, err := admin(t, db).Exec("UPDATE stego_outbox.messages SET available_at = available_at + interval '1 day' WHERE id = $1", pending.ID); err != nil {
			t.Fatal(err)
		}
		for id, state := range map[uuid.UUID]State{pending.ID: Pending, reconciling.ID: Reconciling, done.ID: Done} {
			before := readRow(t, db, id)
			if before.State != state {
				t.Fatalf("the fixture message is %+v, want %s", before, state)
			}
			for _, r := range []Resolution{Acted, NotActed, Discard} {
				if err := resolveAs(t, db, w, id, "user:ops-1", r); err == nil {
					t.Fatalf("a message in %s was resolved as %s", state, r)
				}
			}
			if after := readRow(t, db, id); after != before || auditRows(t, db, id) != 0 {
				t.Fatalf("the row changed from %+v to %+v", before, after)
			}
		}
	})

	t.Run("ST-SUP-RP01-LC13-T04 A resolution that the decision port does not allow is refused", func(t *testing.T) {
		decider := &fakeDecider{allow: false}
		db, w, _, m := parkOne(t, decider, "refuse")
		for _, r := range []Resolution{Acted, NotActed} {
			if err := resolveAs(t, db, w, m.ID, "user:ops-1", r); !errors.Is(err, ErrNotAllowed) {
				t.Fatalf("a denied resolution gave %v", err)
			}
		}
		if auditRows(t, db, m.ID) != 0 || readRow(t, db, m.ID).State != Parked {
			t.Fatal("a denied resolution changed the message")
		}
	})

	t.Run("ST-SUP-RP01-LC13-T05 An idempotent destination's parked message is resolved only as not acted", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, newClock())
		m := message("mail", "user:1")
		m.OutgoingKey = "key-1"
		enqueue(t, db, q, m)
		w := resolvingWorker(t, q, &fakeDecider{allow: true})
		register(t, w, Destination{Name: "mail", Idempotent: true, Handler: func(context.Context, *sql.Tx, Delivery) error {
			return PermanentRefusal(errors.New("400"))
		}})
		cycle(t, w)
		before := readRow(t, db, m.ID)
		if err := resolveAs(t, db, w, m.ID, "user:ops-1", Acted); err == nil {
			t.Fatal("acted was accepted for an idempotent destination")
		}
		if after := readRow(t, db, m.ID); after != before || after.State != Parked {
			t.Fatalf("the refused resolution changed %+v to %+v", before, after)
		}
		resolve(t, db, w, m.ID, NotActed)
		// Not acted keeps the attempts made (ST-SUP-RP01-LC22).
		if row := readRow(t, db, m.ID); row.State != Pending || row.OutgoingKey != "key-1" || row.Attempts != before.Attempts || row.Attempts != 1 {
			t.Fatalf("not acted left %+v", row)
		}
	})

	t.Run("ST-SUP-RP01-LC13-T06 The audit row of a resolution commits with the state change", func(t *testing.T) {
		db, w, _, m := parkOne(t, &fakeDecider{allow: true}, "refuse")
		ctx := context.Background()
		tx, err := tenant.Begin(ctx, db, testTenant(t, "tenant-a"), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Resolve(ctx, tx, ResolveRequest{ID: m.ID, Destination: m.Destination, Principal: person("user:ops-1"),
			Resolution: Acted}); err != nil {
			t.Fatal(err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		if auditRows(t, db, m.ID) != 0 || readRow(t, db, m.ID).State != Parked {
			t.Fatal("the rolled back resolution left a trace")
		}
		resolve(t, db, w, m.ID, Acted)
		var subject, resolution string
		var at time.Time
		if err := admin(t, db).QueryRow("SELECT subject, resolution, at FROM audit_rows WHERE message_id = $1", m.ID).Scan(&subject, &resolution, &at); err != nil {
			t.Fatal(err)
		}
		row := readRow(t, db, m.ID)
		if auditRows(t, db, m.ID) != 1 || subject != "user:ops-1" || resolution != "acted" || !at.Equal(row.ResolvedAt.Time) || row.State != Done {
			t.Fatalf("the audit row is %s %s %v, the message %+v", subject, resolution, at, row)
		}
	})

	t.Run("ST-SUP-RP01-LC13-T07 A discarded message of a must-land destination releases its key", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, newClock())
		first, second := message("authz", "document:1"), message("authz", "document:1")
		// The rows record that the destination must land and is idempotent, as its registration says.
		for _, m := range []*Message{&first, &second} {
			m.MustLand, m.Idempotent = true, true
		}
		enqueue(t, db, q, first, second)
		w := resolvingWorker(t, q, &fakeDecider{allow: true})
		var delivered []uuid.UUID
		register(t, w, Destination{Name: "authz", Idempotent: true, MustLand: true, Handler: func(_ context.Context, _ *sql.Tx, d Delivery) error {
			if d.ID == first.ID {
				return PermanentRefusal(errors.New("bad tuple"))
			}
			delivered = append(delivered, d.ID)
			return nil
		}})
		cycle(t, w)
		if n := cycle(t, w); n != 0 {
			t.Fatal("the parked message did not hold back its key")
		}
		resolve(t, db, w, first.ID, Discard)
		cycle(t, w)
		if readRow(t, db, first.ID).State != Discarded || auditRows(t, db, first.ID) != 1 || len(delivered) != 1 || delivered[0] != second.ID {
			t.Fatalf("the key was not released: %+v, delivered %v", readRow(t, db, first.ID), delivered)
		}
	})

	t.Run("ST-SUP-RP01-LC13-T08 Discarded is refused for a destination that need not land", func(t *testing.T) {
		db, w, _, m := parkOne(t, &fakeDecider{allow: true}, "refuse")
		if err := resolveAs(t, db, w, m.ID, "user:ops-1", Discard); err == nil {
			t.Fatal("a message of billing was discarded")
		}
		if auditRows(t, db, m.ID) != 0 || readRow(t, db, m.ID).State != Parked {
			t.Fatal("the refused discard changed the message")
		}
	})
}

func TestDispatchClaim(t *testing.T) {
	t.Run("ST-SUP-RP01-LC15-T01 The next claim moves an expired dispatching message to outcome unknown", func(t *testing.T) {
		db, _ := testDatabase(t)
		clock := newClock()
		q := newQueue(t, db, clock)
		m, later := message("billing", "charge:1"), message("billing", "charge:1")
		enqueue(t, db, q, m, later)
		if d, err := q.Claim(context.Background(), 1, time.Second); err != nil || len(d) != 1 {
			t.Fatalf("claim: %v %v", d, err)
		}
		clock.Advance(2 * time.Second)
		next, err := q.Claim(context.Background(), 32, time.Minute)
		if err != nil || len(next) != 0 {
			t.Fatalf("the claim also claimed %v, %v", next, err)
		}
		if row, l := readRow(t, db, m.ID), readRow(t, db, later.ID); row.State != OutcomeUnknown || l.State != Pending || l.Token.Valid {
			t.Fatalf("after the claim: %+v and %+v", row, l)
		}
	})

	t.Run("ST-SUP-RP01-LC15-T02 Two workers do not claim one message in outcome unknown", func(t *testing.T) {
		db, _ := testDatabase(t)
		clock := newClock()
		q := newQueue(t, db, clock)
		unknown, reconciling := message("billing", "charge:1"), message("billing", "charge:2")
		enqueue(t, db, q, unknown, reconciling)
		if _, err := admin(t, db).Exec(`UPDATE stego_outbox.messages SET attempts = 1, dispatched_at = $3::timestamptz,
			available_at = $3::timestamptz, state = CASE WHEN id = $1 THEN 'outcome_unknown' ELSE 'reconciling' END
			WHERE id IN ($1, $2)`, unknown.ID, reconciling.ID, clock.Now()); err != nil {
			t.Fatal(err)
		}
		clock.Advance(time.Minute)
		var wg sync.WaitGroup
		results := make(chan []Delivery, 2)
		for i := 0; i < 2; i++ {
			wg.Go(func() {
				d, err := q.Claim(context.Background(), 2, time.Minute)
				if err != nil {
					t.Error(err)
				}
				results <- d
			})
		}
		wg.Wait()
		close(results)
		seen := map[uuid.UUID]uuid.UUID{}
		for batch := range results {
			for _, d := range batch {
				if _, twice := seen[d.ID]; twice {
					t.Fatal("two workers claimed one message")
				}
				seen[d.ID] = d.Receipt.Token
			}
		}
		if len(seen) != 2 || readRow(t, db, unknown.ID).Token.UUID != seen[unknown.ID] || readRow(t, db, reconciling.ID).Token.UUID != seen[reconciling.ID] {
			t.Fatalf("the claims are %v", seen)
		}
		if readRow(t, db, unknown.ID).State != OutcomeUnknown || readRow(t, db, reconciling.ID).State != Reconciling {
			t.Fatal("a claim changed the state of a message in outcome unknown or reconciling")
		}
	})

	t.Run("ST-SUP-RP01-LC15-T03 A refund waits behind a charge whose outcome is unknown", func(t *testing.T) {
		db, _ := testDatabase(t)
		clock := newClock()
		q := newQueue(t, db, clock)
		charge, refund := message("billing", "charge:1"), message("billing", "charge:1")
		charge.Kind, refund.Kind = "charge", "refund"
		enqueue(t, db, q, charge, refund)
		billing := newDestination(t, "hold", "ok")
		lookup := &recordingLookup{}
		w := newWorker(t, q, nil)
		register(t, w, Destination{Name: "billing", Handler: billing.handler, Lookup: lookup.lookup, SettleTime: time.Second})
		for i := 0; lookup.count() < 3 && i < 20; i++ {
			clock.Advance(time.Minute)
			cycle(t, w)
			state := readRow(t, db, charge.ID).State
			if state != OutcomeUnknown && state != Reconciling {
				t.Fatalf("the charge is in %s", state)
			}
			for _, kind := range billing.requestKinds() {
				if kind == "refund" {
					t.Fatal("the refund was sent while the charge's outcome is unknown")
				}
			}
		}
		if lookup.count() != 3 || len(billing.requests()) != 1 || readRow(t, db, refund.ID).State != Pending {
			t.Fatalf("%d lookups and %d requests", lookup.count(), len(billing.requests()))
		}
	})
}

func TestDispatchPermission(t *testing.T) {
	t.Run("ST-SUP-RP01-LC17-T01 Resolve asks the decision port for resolve on the destination", func(t *testing.T) {
		decider := &fakeDecider{allow: true}
		db, w, _, m := func() (*sql.DB, *Worker, *fakeDestination, Message) {
			db, _ := testDatabase(t)
			q := newQueue(t, db, newClock())
			m := message("billing", "charge:1")
			enqueue(t, db, q, m)
			w := resolvingWorker(t, q, decider)
			register(t, w, Destination{Name: "billing", Handler: func(context.Context, *sql.Tx, Delivery) error {
				return PermanentRefusal(errors.New("400"))
			}})
			cycle(t, w)
			return db, w, nil, m
		}()
		resolve(t, db, w, m.ID, Acted)
		decider.mu.Lock()
		defer decider.mu.Unlock()
		if len(decider.calls) != 1 {
			t.Fatalf("the decision port got %d calls", len(decider.calls))
		}
		call := decider.calls[0]
		if call.Subject != "user:ops-1" || call.Permission != "resolve" || call.Resource != "outbox_destination:billing" || call.Tenant.ID() != "tenant-a" {
			t.Fatalf("the decision request is %+v", call)
		}
		if readRow(t, db, m.ID).State != Done {
			t.Fatal("the allowed resolution was not applied")
		}
	})

	t.Run("ST-SUP-RP01-LC17-T02 A deny, a port error or no port refuses the resolution", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, newClock())
		m := message("billing", "charge:1")
		enqueue(t, db, q, m)
		refuse := func(context.Context, *sql.Tx, Delivery) error { return PermanentRefusal(errors.New("400")) }
		first := resolvingWorker(t, q, &fakeDecider{allow: true})
		register(t, first, Destination{Name: "billing", Handler: refuse})
		cycle(t, first)
		var calls atomic.Int32
		for name, edit := range map[string]func(*WorkerConfig){
			"deny": func(c *WorkerConfig) { c.Decider = &fakeDecider{allow: false}; c.Auditor = tableAuditor{} },
			"port error": func(c *WorkerConfig) {
				c.Decider = &fakeDecider{allow: true, err: errors.New("down")}
				c.Auditor = tableAuditor{}
			},
			"no port": func(c *WorkerConfig) { c.Auditor = tableAuditor{} },
		} {
			calls.Add(1)
			w := newWorker(t, q, edit)
			register(t, w, Destination{Name: "billing", Handler: refuse})
			if err := resolveAs(t, db, w, m.ID, "user:ops-1", Acted); err == nil {
				t.Fatalf("the resolution with %s was applied", name)
			}
			if auditRows(t, db, m.ID) != 0 || readRow(t, db, m.ID).State != Parked {
				t.Fatalf("the resolution with %s changed the message", name)
			}
		}
		if calls.Load() != 3 {
			t.Fatal("not every case ran")
		}
	})
}

func TestResolveDecidesFirst(t *testing.T) {
	t.Run("ST-SUP-RP01-LC17-T03 Resolve decides the permission first and takes only a principal of the authorization runtime", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, newClock())
		parked, pending, other := message("billing", "charge:1"), message("billing", "charge:2"), message("billing", "charge:3")
		enqueue(t, db, q, parked)
		refuse := func(context.Context, *sql.Tx, Delivery) error { return PermanentRefusal(errors.New("400")) }
		first := resolvingWorker(t, q, &fakeDecider{allow: true})
		register(t, first, Destination{Name: "billing", Handler: refuse})
		cycle(t, first)
		enqueue(t, db, q, pending)
		enqueueAs(t, db, q, "tenant-b", other)
		ctx := context.Background()
		try := func(w *Worker, request ResolveRequest) error {
			t.Helper()
			tx, err := tenant.Begin(ctx, db, testTenant(t, "tenant-a"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			_, err = w.Resolve(ctx, tx, request)
			return err
		}
		// A deny gives one answer for a parked message, a pending one, one of another tenant and one that is not
		// there, and the decision port is asked about the named destination in the tenant of the transaction.
		deny := &fakeDecider{allow: false}
		denied := resolvingWorker(t, q, deny)
		register(t, denied, Destination{Name: "billing", Handler: refuse})
		var answers []string
		for _, id := range []uuid.UUID{parked.ID, pending.ID, other.ID, uuid.New()} {
			err := try(denied, ResolveRequest{ID: id, Destination: "billing", Principal: person("user:ops-1"), Resolution: Acted})
			if !errors.Is(err, ErrNotAllowed) {
				t.Fatalf("the denied resolution of %s gave %v, want ErrNotAllowed", id, err)
			}
			answers = append(answers, err.Error())
		}
		for _, a := range answers[1:] {
			if a != answers[0] {
				t.Fatalf("the denied resolutions gave the answers %q", answers)
			}
		}
		deny.mu.Lock()
		calls := slices.Clone(deny.calls)
		deny.mu.Unlock()
		if len(calls) != 4 {
			t.Fatalf("the decision port got %d calls, want one for each resolution", len(calls))
		}
		for _, call := range calls {
			if call.Subject != "user:ops-1" || call.Resource != "outbox_destination:billing" || call.Tenant.ID() != "tenant-a" {
				t.Fatalf("the decision request is %+v", call)
			}
		}
		// After an allow, the caller learns that a message is not there, is of another destination or is not parked.
		allowed := resolvingWorker(t, q, &fakeDecider{allow: true})
		register(t, allowed, Destination{Name: "billing", Handler: refuse})
		register(t, allowed, Destination{Name: "ledger", Handler: refuse})
		for _, c := range []struct {
			request ResolveRequest
			want    error
		}{
			{ResolveRequest{ID: uuid.New(), Destination: "billing", Principal: person("user:ops-1"), Resolution: Acted}, ErrNotFound},
			{ResolveRequest{ID: other.ID, Destination: "billing", Principal: person("user:ops-1"), Resolution: Acted}, ErrNotFound},
			{ResolveRequest{ID: parked.ID, Destination: "ledger", Principal: person("user:ops-1"), Resolution: Acted}, ErrNotFound},
			{ResolveRequest{ID: pending.ID, Destination: "billing", Principal: person("user:ops-1"), Resolution: Acted}, ErrNotParked},
		} {
			if err := try(allowed, c.request); !errors.Is(err, c.want) {
				t.Fatalf("the allowed resolution %+v gave %v, want %v", c.request, err, c.want)
			}
		}
		// A principal that the authorization runtime did not make is refused before the decision port is asked.
		counted := &fakeDecider{allow: true}
		strict := resolvingWorker(t, q, counted)
		register(t, strict, Destination{Name: "billing", Handler: refuse})
		for name, p := range map[string]Principal{"no principal": nil, "a zero mark": testPrincipal{},
			"a nil embedded principal": struct{ Principal }{}} {
			if err := try(strict, ResolveRequest{ID: parked.ID, Destination: "billing", Principal: p, Resolution: Acted}); err == nil {
				t.Fatalf("Resolve took %s", name)
			}
		}
		counted.mu.Lock()
		n := len(counted.calls)
		counted.mu.Unlock()
		if n != 0 || readRow(t, db, parked.ID).State != Parked {
			t.Fatalf("the refused principals reached the decision port %d times or changed the message", n)
		}
	})
}

func TestDispatchConflict(t *testing.T) {
	t.Run("ST-SUP-RP01-LC18-T01 Conflicts back off, do not count as attempts, and count only after 20 in a row", func(t *testing.T) {
		db, _ := testDatabase(t)
		clock := newClock()
		q := newQueue(t, db, clock)
		always, later := message("ledger", "account:1"), message("ledger", "account:1")
		enqueue(t, db, q, always, later)
		w := newWorker(t, q, func(c *WorkerConfig) { c.MaxAttempts = 3 })
		var mu sync.Mutex
		calls := map[uuid.UUID]int{}
		var twice uuid.UUID
		register(t, w, Destination{Name: "ledger", Idempotent: true, Handler: func(_ context.Context, _ *sql.Tx, d Delivery) error {
			mu.Lock()
			defer mu.Unlock()
			calls[d.ID]++
			if d.ID == always.ID || (d.ID == twice && calls[d.ID] <= 2) {
				return Conflict(errors.New("serialization failure"))
			}
			return nil
		}})
		for n := int64(1); ; n++ {
			if got := cycle(t, w); got != 1 {
				t.Fatalf("conflict %d: the cycle claimed %d messages", n, got)
			}
			row := readRow(t, db, always.ID)
			if row.State == Parked {
				if n != 23 || row.Attempts != 3 || row.ParkedFrom.String != "dispatching" {
					t.Fatalf("the message parked at conflict %d with %+v", n, row)
				}
				break
			}
			wantAttempts := max(n-20, 0)
			if row.State != Pending || row.Conflicts != n || row.Attempts != wantAttempts {
				t.Fatalf("after conflict %d the message is %+v, want %d attempts", n, row, wantAttempts)
			}
			wait := row.AvailableAt.Sub(clock.Now())
			base := min(100*time.Millisecond<<(n-1), 30*time.Second)
			low, high := base, base+base/10
			if base == 30*time.Second {
				low, high = 27*time.Second, 30*time.Second
			}
			if wait < low || wait > high {
				t.Fatalf("after conflict %d the wait is %v, want %v to %v", n, wait, low, high)
			}
			if c := cycle(t, w); c != 0 {
				t.Fatalf("after conflict %d a message was claimed before the wait", n)
			}
			clock.Set(row.AvailableAt)
			if n > 30 {
				t.Fatal("the message did not park")
			}
		}
		mu.Lock()
		if calls[later.ID] != 0 {
			t.Fatal("the later message of the key was dispatched")
		}
		mu.Unlock()
		if row := readRow(t, db, later.ID); row.State != Pending {
			t.Fatalf("the later message is %+v", row)
		}

		recovers := message("ledger", "account:2")
		mu.Lock()
		twice = recovers.ID
		mu.Unlock()
		enqueue(t, db, q, recovers)
		for i := 0; i < 3; i++ {
			clock.Advance(time.Minute)
			cycle(t, w)
		}
		// The two conflicts are taken back and the success keeps its attempt (ST-SUP-RP01-LC22).
		if row := readRow(t, db, recovers.ID); row.State != Done || row.Attempts != 1 || row.Conflicts != 0 {
			t.Fatalf("the message that recovers is %+v", row)
		}
	})
}

func TestDispatchTenantKeys(t *testing.T) {
	t.Run("ST-SUP-RP01-LC03-T04 The same resource key of two tenants is two keys", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, newClock())
		ctx := context.Background()
		p1, p2 := message("billing", "charge:1"), message("billing", "charge:1")
		enqueueAs(t, db, q, "t1", p1)
		enqueueAs(t, db, q, "t2", p2)
		var mu sync.Mutex
		var order []uuid.UUID
		w := newWorker(t, q, nil)
		register(t, w, Destination{Name: "billing", Handler: func(_ context.Context, _ *sql.Tx, d Delivery) error {
			if d.ID == p1.ID {
				return PermanentRefusal(errors.New("400"))
			}
			mu.Lock()
			order = append(order, d.ID)
			mu.Unlock()
			return nil
		}})
		cycle(t, w)
		cycle(t, w)
		// Two enqueues of the key run at once, one for each tenant: they do not wait on each other.
		s1, s2 := message("billing", "charge:1"), message("billing", "charge:1")
		t1 := testTenant(t, "t1")
		open, err := tenant.Begin(ctx, db, t1, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = open.Rollback() }()
		if err := q.Enqueue(ctx, open, s1); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			enqueueAs(t, db, q, "t2", s2)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("the enqueue of t2 waited on the open enqueue of t1")
		}
		if err := tenant.Commit(ctx, open, t1); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 4; i++ {
			cycle(t, w)
		}
		mu.Lock()
		got := append([]uuid.UUID(nil), order...)
		mu.Unlock()
		if len(got) != 2 || got[0] != p2.ID || got[1] != s2.ID {
			t.Fatalf("t2's deliveries are %v", got)
		}
		if readRow(t, db, p1.ID).State != Parked || readRow(t, db, s1.ID).State != Pending {
			t.Fatal("t1's key was not held back")
		}
		blocked, err := q.Blocked(ctx, MaxBatchSize)
		if err != nil || len(blocked) != 1 || blocked[0].TenantID != "t1" || blocked[0].ResourceKey != "charge:1" {
			t.Fatalf("the blocked view is %+v %v", blocked, err)
		}
	})
}

// auditOutbox is the outbox of the audit runtime's Writer in the tests: it enqueues each sink message on the queue.
type auditOutbox struct{ q *Queue }

func (o auditOutbox) Enqueue(ctx context.Context, tx audit.Tx, _, destination, key string, payload []byte) error {
	sqlTx, ok := tx.(*sql.Tx)
	if !ok {
		return errors.New("the audit outbox needs a *sql.Tx")
	}
	return o.q.Enqueue(ctx, sqlTx, Message{ID: uuid.New(), Destination: destination, ResourceKey: key,
		Kind: "audit.event", Payload: payload})
}

// auditWriter is the Auditor over the audit runtime's Writer: a resolution is one audit event in the chain of the
// message's tenant.
type auditWriter struct{ w *audit.Writer }

func (a auditWriter) Record(ctx context.Context, tx *sql.Tx, e AuditEntry) error {
	_, err := a.w.Record(ctx, tx, e.Tenant, []audit.Event{{
		TenantID: e.Tenant.ID(), ActorID: e.Subject, ActorKind: audit.ActorUser, Action: "outbox.resolve." + string(e.Resolution),
		ResourceType: "outbox_message", ResourceID: e.MessageID.String(), Outcome: audit.OutcomeOK, Service: "outbox-test",
	}})
	return err
}

func TestDispatchTenantSteps(t *testing.T) {
	t.Run("ST-SUP-RP01-LC19-T03 A claimed message's handler and audit run in its own tenant's transaction", func(t *testing.T) {
		db, dsn := testDatabase(t)
		a := admin(t, db)
		ddl, err := audit.DDL(audit.Config{RuntimeRole: runtimeUser(t, dsn), AnchorPeriod: time.Hour, RetentionMonths: 12})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Exec(ddl); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 64; i++ {
			var more bool
			var tid sql.NullString
			var seq sql.NullInt64
			var hash []byte
			if err := db.QueryRow(`SELECT step_left, checkpoint_tenant, checkpoint_seq, checkpoint_hash FROM `+audit.MaintainFunction+`()`).
				Scan(&more, &tid, &seq, &hash); err != nil {
				t.Fatal(err)
			}
			if !more {
				break
			}
		}
		q := newQueue(t, db, newClock())
		writer, err := audit.NewWriter(auditOutbox{q: q}, nil)
		if err != nil {
			t.Fatal(err)
		}
		m1, m2 := message("billing", "charge:1"), message("billing", "charge:1")
		enqueueAs(t, db, q, "t1", m1)
		enqueueAs(t, db, q, "t2", m2)
		seen := map[uuid.UUID]string{}
		w := newWorker(t, q, func(c *WorkerConfig) { c.Decider = &fakeDecider{allow: true}; c.Auditor = auditWriter{w: writer} })
		register(t, w, Destination{Name: "billing", Handler: func(ctx context.Context, tx *sql.Tx, d Delivery) error {
			var current string
			if err := tx.QueryRowContext(ctx, `SELECT current_setting('app.tenant', true)`).Scan(&current); err != nil {
				return NotSent(err)
			}
			seen[d.ID] = current
			return PermanentRefusal(errors.New("400"))
		}})
		cycle(t, w)
		cycle(t, w)
		if seen[m1.ID] != "t1" || seen[m2.ID] != "t2" {
			t.Fatalf("the handlers saw the tenants %v", seen)
		}
		if err := resolveIn(t, db, w, "t1", m1.ID, "user:ops-1", Acted); err != nil {
			t.Fatal(err)
		}
		if err := resolveIn(t, db, w, "t2", m2.ID, "user:ops-2", Acted); err != nil {
			t.Fatal(err)
		}
		for id, want := range map[uuid.UUID]string{m1.ID: "t1", m2.ID: "t2"} {
			var tid string
			if err := a.QueryRow(`SELECT tenant_id FROM `+audit.EventsTable+` WHERE resource_id = $1`, id.String()).Scan(&tid); err != nil || tid != want {
				t.Fatalf("the audit row of %s is in the chain of %q: %v", id, tid, err)
			}
		}
	})
}

func TestDispatchAttempts(t *testing.T) {
	t.Run("ST-SUP-RP01-LC22-T01 The attempt count grows with each dispatch and a success keeps it", func(t *testing.T) {
		db, _ := testDatabase(t)
		clock := newClock()
		q := newQueue(t, db, clock)
		retried, unknown, stopped := message("billing", "a"), message("nowhere", "b"), message("billing", "c")
		w := newWorker(t, q, nil)
		var calls atomic.Int32
		register(t, w, Destination{Name: "billing", Handler: func(context.Context, *sql.Tx, Delivery) error {
			if calls.Add(1) == 1 {
				return NotSent(errors.New("dial failed"))
			}
			return nil
		}})
		enqueue(t, db, q, retried)
		cycle(t, w)
		clock.Advance(time.Hour)
		cycle(t, w)
		if row := readRow(t, db, retried.ID); row.State != Done || row.Attempts != 2 {
			t.Fatalf("the retried message is %+v", row)
		}
		enqueue(t, db, q, unknown)
		cycle(t, w)
		if row := readRow(t, db, unknown.ID); row.State != Pending || row.Attempts != 0 {
			t.Fatalf("the message of an unknown destination is %+v", row)
		}
		if _, err := admin(t, db).Exec(`UPDATE stego_outbox.messages SET available_at = available_at + interval '1 day'
			WHERE id = $1`, unknown.ID); err != nil {
			t.Fatal(err)
		}
		enqueue(t, db, q, stopped)
		stopping, cancel := context.WithCancel(context.Background())
		cancel()
		if n, err := w.cycle(context.Background(), stopping); err != nil || n != 1 {
			t.Fatalf("the cycle claimed %d: %v", n, err)
		}
		if row := readRow(t, db, stopped.ID); row.State != Pending || row.Attempts != 0 || calls.Load() != 2 {
			t.Fatalf("the message of a stopping worker is %+v", row)
		}
	})

	t.Run("ST-SUP-RP01-LC23-T02 A must-land message is not parked for a transient failure", func(t *testing.T) {
		db, _ := testDatabase(t)
		clock := newClock()
		q := newQueue(t, db, clock)
		land, later, other := message("authz", "doc:1"), message("authz", "doc:1"), message("billing", "x")
		enqueue(t, db, q, land, later, other)
		reader, provider := metricReader()
		w := newWorker(t, q, func(c *WorkerConfig) { c.MaxAttempts = 3; c.Meter = provider })
		answers := func() Handler {
			var n atomic.Int32
			return func(context.Context, *sql.Tx, Delivery) error {
				switch k := n.Add(1); {
				case k <= 5:
					return NotSent(errors.New("down"))
				case k <= 10:
					return TransientRefusal(errors.New("503"))
				}
				return nil
			}
		}
		register(t, w, Destination{Name: "authz", Idempotent: true, MustLand: true, Handler: answers()})
		register(t, w, Destination{Name: "billing", Handler: answers()})
		var age float64
		for i := 0; i < 40 && readRow(t, db, land.ID).State != Done; i++ {
			clock.Advance(time.Hour)
			cycle(t, w)
			cycle(t, w)
			if row := readRow(t, db, land.ID); row.State == Parked {
				t.Fatalf("the must-land message parked: %+v", row)
			}
			if readRow(t, db, land.ID).State != Done {
				if l := readRow(t, db, later.ID); l.State != Pending || l.Attempts != 0 {
					t.Fatalf("the later message of the key moved: %+v", l)
				}
				if next := gaugeValues(t, reader)["authz"]; next <= age {
					t.Fatalf("outbox.oldest.age of authz went from %v to %v", age, next)
				} else {
					age = next
				}
			}
		}
		if row := readRow(t, db, land.ID); row.State != Done || row.Attempts != 11 {
			t.Fatalf("the must-land message is %+v", row)
		}
		if row := readRow(t, db, other.ID); row.State != Parked || row.Attempts != 3 {
			t.Fatalf("the other message is %+v", row)
		}
	})
}

func TestDispatchStateHook(t *testing.T) {
	t.Run("ST-SUP-RP01-LC25-T01 The state hook runs in the state change's transaction", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, newClock())
		delivered, parked, failing := message("authz", "a"), message("authz", "b"), message("authz", "c")
		delivered.Group, parked.Group, failing.Group = "rev-1", "rev-2", "rev-3"
		enqueue(t, db, q, delivered, parked, failing)
		w := resolvingWorker(t, q, &fakeDecider{allow: true})
		register(t, w, Destination{Name: "authz", Idempotent: true, MustLand: true,
			Handler: func(_ context.Context, _ *sql.Tx, d Delivery) error {
				if d.ID == parked.ID {
					return PermanentRefusal(errors.New("bad tuple"))
				}
				return nil
			},
			StateHook: func(ctx context.Context, tx *sql.Tx, c StateChange) error {
				if c.ID == failing.ID {
					return errors.New("the hook refuses")
				}
				_, err := tx.ExecContext(ctx, `INSERT INTO hook_rows (message_id, group_id, state, tenant_id) VALUES ($1, $2, $3, $4)`,
					c.ID, c.Group, string(c.State), c.Tenant.ID())
				return err
			}})
		for i := 0; i < 3; i++ {
			cycle(t, w)
		}
		resolve(t, db, w, parked.ID, NotActed)
		hooks := func(id uuid.UUID) string {
			rows, err := admin(t, db).Query(`SELECT group_id || ':' || state FROM hook_rows WHERE message_id = $1 ORDER BY state`, id)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var got []string
			for rows.Next() {
				var v string
				if err := rows.Scan(&v); err != nil {
					t.Fatal(err)
				}
				got = append(got, v)
			}
			return strings.Join(got, ",")
		}
		if got := hooks(delivered.ID); got != "rev-1:done" {
			t.Fatalf("the delivered message's hook rows are %q", got)
		}
		if got := hooks(parked.ID); got != "rev-2:parked,rev-2:pending" {
			t.Fatalf("the parked message's hook rows are %q", got)
		}
		// The failed hook rolled the move to done back; the message waits out a backoff in outcome unknown.
		if row := readRow(t, db, failing.ID); row.State != OutcomeUnknown || row.FailureCode != "hook-failed" ||
			hooks(failing.ID) != "" || w.Stats().StateHookFailures != 1 {
			t.Fatalf("the failing hook left %+v", row)
		}
	})

	hookCase := func(t *testing.T, hook StateHook) {
		db, _ := testDatabase(t)
		clock := newClock()
		q := newQueue(t, db, clock)
		m, other := message("authz", "a"), message("mail", "b")
		enqueue(t, db, q, m)
		reader, provider := metricReader()
		events := &eventExporter{}
		logs := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(events)))
		w := newWorker(t, q, func(c *WorkerConfig) { c.Meter = provider; c.Logger = logs })
		register(t, w, Destination{Name: "authz", Idempotent: true, MustLand: true, StateHook: hook,
			Handler: func(context.Context, *sql.Tx, Delivery) error { return nil }})
		register(t, w, Destination{Name: "mail", Handler: func(context.Context, *sql.Tx, Delivery) error { return nil }})
		var lastDue time.Time
		for i := 1; i <= 3; i++ {
			if n := cycle(t, w); n != 1 {
				t.Fatalf("round %d claimed %d messages", i, n)
			}
			row := readRow(t, db, m.ID)
			if row.State != OutcomeUnknown || row.FailureCode != "hook-failed" || !row.AvailableAt.After(clock.Now()) {
				t.Fatalf("round %d left %+v", i, row)
			}
			if n := cycle(t, w); n != 0 {
				t.Fatalf("round %d: the message was claimed again before its backoff", i)
			}
			if !row.AvailableAt.After(lastDue) {
				t.Fatalf("round %d: the due time did not move on", i)
			}
			lastDue = row.AvailableAt
			clock.Set(row.AvailableAt)
		}
		if got := w.Stats().StateHookFailures; got != 3 {
			t.Fatalf("%d hook failures are counted", got)
		}
		found := collect(t, reader, "outbox.hook.failures")
		if len(found) != 1 || found[0].Data.(metricdata.Sum[int64]).DataPoints[0].Value != 3 {
			t.Fatalf("outbox.hook.failures is %+v", found)
		}
		events.mu.Lock()
		alerts := 0
		for _, r := range events.records {
			if r.EventName() == "outbox.hook_failed" {
				alerts++
			}
		}
		events.mu.Unlock()
		if alerts != 3 {
			t.Fatalf("%d hook alerts are raised", alerts)
		}
		// The worker keeps running and delivers another destination's message.
		enqueue(t, db, q, other)
		for i := 0; i < 3 && readRow(t, db, other.ID).State != Done; i++ {
			cycle(t, w)
		}
		if readRow(t, db, other.ID).State != Done {
			t.Fatal("the worker stopped after the hook failures")
		}
	}

	t.Run("ST-SUP-RP01-LC25-T01 A state hook that always fails backs off and raises an alert", func(t *testing.T) {
		hookCase(t, func(context.Context, *sql.Tx, StateChange) error { return errors.New("the record store is down") })
	})

	t.Run("ST-SUP-RP01-LC25-T01 A state hook that panics is a hook failure", func(t *testing.T) {
		hookCase(t, func(context.Context, *sql.Tx, StateChange) error { panic("the hook broke") })
	})
}

func TestDispatchPanic(t *testing.T) {
	t.Run("ST-SUP-RP01-LC27-T01 A panicking handler leaves the message in outcome unknown", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, newClock())
		first, second := message("billing", "charge:1"), message("mail", "user:1")
		enqueue(t, db, q, first)
		w := newWorker(t, q, nil)
		register(t, w, Destination{Name: "billing", Handler: func(context.Context, *sql.Tx, Delivery) error {
			panic("password=hunter2")
		}})
		register(t, w, Destination{Name: "mail", Handler: func(context.Context, *sql.Tx, Delivery) error { return nil }})
		cycle(t, w)
		if row := readRow(t, db, first.ID); row.State != OutcomeUnknown || row.FailureCode != "handler-panic" {
			t.Fatalf("the panic left %+v", row)
		}
		if _, err := admin(t, db).Exec(`UPDATE stego_outbox.messages SET available_at = available_at + interval '1 day'
			WHERE id = $1`, first.ID); err != nil {
			t.Fatal(err)
		}
		enqueue(t, db, q, second)
		cycle(t, w)
		var text string
		if err := admin(t, db).QueryRow(`SELECT row_to_json(m)::text FROM stego_outbox.messages AS m WHERE id = $1`, first.ID).Scan(&text); err != nil || strings.Contains(text, "hunter2") {
			t.Fatalf("the row holds the panic value: %s %v", text, err)
		}
		if readRow(t, db, second.ID).State != Done {
			t.Fatal("the worker did not deliver after the panic")
		}
	})
}
