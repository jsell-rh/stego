package outbox

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"stego.dev/runtime/tenant"
)

// testConfig is a worker configuration for tests: one step at a time, short waits, and an attempt deadline that a
// test reaches in real time.
func testConfig() WorkerConfig {
	config := DefaultWorkerConfig()
	config.Concurrency = 1
	config.PollInterval = 10 * time.Millisecond
	config.AttemptTimeout = 300 * time.Millisecond
	config.LeaseDuration = 30 * time.Second
	config.RetryMin = 100 * time.Millisecond
	config.RetryMax = 10 * time.Second
	return config
}

func newWorker(t testing.TB, q *Queue, edit func(*WorkerConfig)) *Worker {
	t.Helper()
	config := testConfig()
	if edit != nil {
		edit(&config)
	}
	w, err := NewWorker(q, config)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func register(t testing.TB, w *Worker, d Destination) {
	t.Helper()
	if err := w.Register(d); err != nil {
		t.Fatal(err)
	}
}

// cycle runs one claim and the step of what it claimed, and returns how many messages it claimed.
func cycle(t testing.TB, w *Worker) int {
	t.Helper()
	n, err := w.cycle(context.Background(), context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func startWorker(t *testing.T, w *Worker) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- w.Run(ctx) }()
	t.Cleanup(cancel)
	return cancel, result
}

func await(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("the condition did not become true")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// metricReader is a manual reader of the meter provider that the worker gets.
func metricReader() (*sdkmetric.ManualReader, *sdkmetric.MeterProvider) {
	reader := sdkmetric.NewManualReader()
	return reader, sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
}

func collect(t *testing.T, reader *sdkmetric.ManualReader, name string) []metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var found []metricdata.Metrics
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == name {
				found = append(found, m)
			}
		}
	}
	return found
}

// gaugeValues reads outbox.oldest.age by destination.
func gaugeValues(t *testing.T, reader *sdkmetric.ManualReader) map[string]float64 {
	t.Helper()
	values := map[string]float64{}
	for _, m := range collect(t, reader, "outbox.oldest.age") {
		gauge, ok := m.Data.(metricdata.Gauge[float64])
		if !ok {
			t.Fatalf("outbox.oldest.age is a %T", m.Data)
		}
		for _, point := range gauge.DataPoints {
			destination, _ := point.Attributes.Value("destination")
			values[destination.AsString()] = point.Value
		}
	}
	return values
}

func TestWorkerDelivery(t *testing.T) {
	t.Run("ST-SUP-RP01-LC02-T03 A result after the attempt deadline is not acknowledged", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, newClock())
		m := message("audit", "one")
		enqueue(t, db, q, m)
		w := newWorker(t, q, nil)
		var calls atomic.Int32
		register(t, w, Destination{Name: "audit", Idempotent: true, Handler: func(ctx context.Context, _ *sql.Tx, d Delivery) error {
			if calls.Add(1) == 1 {
				<-ctx.Done()
			}
			return nil
		}})
		cycle(t, w)
		row := readRow(t, db, m.ID)
		if w.Stats().Delivered != 0 || row.State != OutcomeUnknown || row.FailureCode != "delivery-timeout" {
			t.Fatalf("a late success counted: delivered %d, row %+v", w.Stats().Delivered, row)
		}
		cycle(t, w)
		if calls.Load() != 2 || readRow(t, db, m.ID).State != Done || w.Stats().Delivered != 1 {
			t.Fatalf("the message was not sent again: calls %d, row %+v", calls.Load(), readRow(t, db, m.ID))
		}
	})

	t.Run("ST-SUP-RP01-LC03-T03 An unknown destination stays in the queue", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, newClock())
		m := message("nowhere", "one")
		enqueue(t, db, q, m)
		w := newWorker(t, q, nil)
		var calls atomic.Int32
		register(t, w, Destination{Name: "audit", Handler: func(context.Context, *sql.Tx, Delivery) error {
			calls.Add(1)
			return nil
		}})
		cycle(t, w)
		row := readRow(t, db, m.ID)
		if row.State != Pending || row.FailureCode != "destination-unknown" || row.Attempts != 0 || calls.Load() != 0 ||
			w.Stats().UnknownDestinations == 0 {
			t.Fatalf("the unknown destination: row %+v, calls %d, stats %+v", row, calls.Load(), w.Stats())
		}
	})

	t.Run("ST-SUP-RP01-LC06-T01 The emitted worker stores no handler error text", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, newClock())
		m := message("audit", "one")
		enqueue(t, db, q, m)
		w := newWorker(t, q, nil)
		const secret = "password=hunter2"
		register(t, w, Destination{Name: "audit", Handler: func(context.Context, *sql.Tx, Delivery) error {
			return errors.New("the sink refused " + secret)
		}})
		cycle(t, w)
		if row := readRow(t, db, m.ID); row.FailureCode != "delivery-failed" {
			t.Fatalf("the failure code is %q", row.FailureCode)
		}
		var text string
		if err := admin(t, db).QueryRow(`SELECT row_to_json(m)::text FROM stego_outbox.messages AS m WHERE id = $1`, m.ID).Scan(&text); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(text, "hunter2") || strings.Contains(text, "refused") {
			t.Fatalf("the row holds the handler's error text: %s", text)
		}
	})
}

// faultConnector passes each statement to the real database and then loses a result, as a client that loses the
// answer of a statement that committed. With loseCommit false it loses the result of every statement after its
// first row, such as a claim; with loseCommit true it passes every statement through whole, and loses the result of
// each COMMIT after the database committed, such as the commit of an acknowledgement.
type faultConnector struct {
	database   *sql.DB
	loseCommit bool
	failBegin  bool
}

func (c faultConnector) Connect(context.Context) (driver.Conn, error) {
	return &faultConn{faultConnector: c}, nil
}
func (faultConnector) Driver() driver.Driver { return faultDriver{} }

type faultDriver struct{}

func (faultDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use the fault connector")
}

// faultConn routes its statements to the real database, or to the real transaction that it holds.
type faultConn struct {
	faultConnector
	tx *sql.Tx
}

func (*faultConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("no prepare") }
func (c *faultConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *faultConn) BeginTx(ctx context.Context, _ driver.TxOptions) (driver.Tx, error) {
	if c.failBegin {
		return nil, errors.New("the database refuses a transaction")
	}
	tx, err := c.database.BeginTx(context.WithoutCancel(ctx), nil)
	if err != nil {
		return nil, err
	}
	c.tx = tx
	return faultTx{c}, nil
}
func (*faultConn) Close() error { return nil }

type faultTx struct{ c *faultConn }

func (f faultTx) Commit() error {
	err := f.c.tx.Commit()
	f.c.tx = nil
	if err == nil && f.c.loseCommit {
		return errors.New("the connection broke after the commit")
	}
	return err
}

func (f faultTx) Rollback() error {
	err := f.c.tx.Rollback()
	f.c.tx = nil
	return err
}

func (c *faultConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	values := make([]any, len(args))
	for i, value := range args {
		values[i] = value.Value
	}
	var rows *sql.Rows
	var err error
	if c.tx != nil {
		rows, err = c.tx.QueryContext(ctx, query, values...)
	} else {
		rows, err = c.database.QueryContext(ctx, query, values...)
	}
	if err != nil {
		return nil, err
	}
	columns, err := rows.Columns()
	if err != nil {
		_ = rows.Close()
		return nil, err
	}
	return &faultRows{rows: rows, columns: columns, whole: c.loseCommit || c.failBegin}, nil
}

// faultRows gives the first row and then fails, so the client never reads the whole result.
type faultRows struct {
	rows    *sql.Rows
	columns []string
	read    bool
	whole   bool
}

func (r *faultRows) Columns() []string { return r.columns }
func (r *faultRows) Close() error      { return r.rows.Close() }
func (r *faultRows) Next(dest []driver.Value) error {
	if r.read && !r.whole {
		_ = r.rows.Close()
		return errors.New("the connection broke")
	}
	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			return err
		}
		return io.EOF
	}
	values := make([]any, len(dest))
	for i := range dest {
		values[i] = &dest[i]
	}
	if err := r.rows.Scan(values...); err != nil {
		return err
	}
	r.read = true
	return nil
}

func TestWorkerFailures(t *testing.T) {
	t.Run("ST-SUP-RP01-LC04-T01 A database failure drops no message", func(t *testing.T) {
		db, dsn := testDatabase(t)
		m := message("audit", "one")
		enqueue(t, db, newQueue(t, db, nil), m)
		pool, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		w := newWorker(t, newQueue(t, pool, nil), func(c *WorkerConfig) { c.AttemptTimeout = 5 * time.Second })
		entered, release := make(chan struct{}), make(chan struct{})
		register(t, w, Destination{Name: "audit", Handler: func(context.Context, *sql.Tx, Delivery) error {
			close(entered)
			<-release
			return nil
		}})
		cancel, result := startWorker(t, w)
		// The database ends every session of the pool while Run runs and a handler is in flight, and the pool closes:
		// the acknowledgement in the handler's transaction and the next claims fail.
		<-entered
		if _, err := admin(t, db).Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			WHERE usename = $1`, runtimeUser(t, dsn)); err != nil {
			t.Fatal(err)
		}
		if err := pool.Close(); err != nil {
			t.Fatal(err)
		}
		close(release)
		await(t, func() bool { return w.Stats().DatabaseFailures > 0 })
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, not the cancellation", err)
		}
		if w.Stats().Delivered != 0 {
			t.Fatalf("a database failure counted as a delivery: %+v", w.Stats())
		}
		if row := readRow(t, db, m.ID); row.State != Dispatching || !row.Token.Valid {
			t.Fatalf("the message lost its claim: %+v", row)
		}

		// The acknowledgement commits with the handler's transaction, but the result of the COMMIT is lost: the worker
		// counts a database failure, not a delivery.
		n := message("audit", "two")
		enqueue(t, db, newQueue(t, db, nil), n)
		faulty := sql.OpenDB(faultConnector{database: db, loseCommit: true})
		t.Cleanup(func() { _ = faulty.Close() })
		lost := newWorker(t, newQueue(t, faulty, nil), nil)
		register(t, lost, Destination{Name: "audit", Handler: func(context.Context, *sql.Tx, Delivery) error { return nil }})
		if got := cycle(t, lost); got != 1 {
			t.Fatalf("the cycle claimed %d messages", got)
		}
		if stats := lost.Stats(); stats.Delivered != 0 || stats.DatabaseFailures != 1 {
			t.Fatalf("a lost acknowledgement result counted as %+v", stats)
		}
		if row := readRow(t, db, n.ID); row.State != Done {
			t.Fatalf("the acknowledgement did not commit: %+v", row)
		}
	})

	t.Run("ST-SUP-RP01-LC04-T02 Recovery after an unknown claim result keeps the delivery contract", func(t *testing.T) {
		db, _ := testDatabase(t)
		clock := newClock()
		m, n := message("audit", "one"), message("audit", "two")
		enqueue(t, db, newQueue(t, db, clock), m, n)
		faulty := sql.OpenDB(faultConnector{database: db})
		faulty.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = faulty.Close() })
		lost, err := newQueue(t, faulty, clock).Claim(context.Background(), 2, time.Minute)
		if err == nil || lost != nil {
			t.Fatalf("a claim with a lost result returned %v, %v", lost, err)
		}
		old := readRow(t, db, m.ID)
		if old.State != Dispatching || !old.Token.Valid || old.Attempts != 1 {
			t.Fatalf("the lost claim did not keep its lease: %+v", old)
		}
		q := newQueue(t, db, clock)
		if next, err := q.Claim(context.Background(), 2, time.Minute); err != nil || len(next) != 0 {
			t.Fatalf("a message under a live lease was claimed again: %v %v", next, err)
		}
		clock.Advance(2 * time.Minute)
		w := newWorker(t, q, nil)
		var got []Delivery
		register(t, w, Destination{Name: "audit", Idempotent: true, Handler: func(_ context.Context, _ *sql.Tx, d Delivery) error {
			got = append(got, d)
			return nil
		}})
		for i := 0; i < 4; i++ {
			cycle(t, w)
		}
		if len(got) != 2 {
			t.Fatalf("the recovery delivered %d messages", len(got))
		}
		for _, d := range got {
			want := m
			if d.ID == n.ID {
				want = n
			}
			if d.ID != want.ID || d.Kind != want.Kind || string(d.Payload) != `{"version":1}` || d.Attempts != 2 ||
				d.Receipt.Token == old.Token.UUID {
				t.Fatalf("the recovered delivery %+v does not keep the contract", d)
			}
		}
		stale := Receipt{ID: m.ID, Token: old.Token.UUID, State: Dispatching}
		if ok, _ := q.acknowledge(context.Background(), stale); ok {
			t.Fatal("the lost token had an effect")
		}
		if ok, _ := q.retry(context.Background(), stale, 0, "late"); ok || readRow(t, db, m.ID).State != Done {
			t.Fatal("the lost token moved the message")
		}
	})
}

func TestWorkerBounds(t *testing.T) {
	t.Run("ST-SUP-RP01-LC05-T01 The worker holds its concurrency limit and copies its handlers", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, nil)
		for i := 0; i < 8; i++ {
			enqueue(t, db, q, message("audit", uuid.NewString()))
		}
		var active, maximum atomic.Int32
		release := make(chan struct{})
		d := Destination{Name: "audit", Handler: func(ctx context.Context, _ *sql.Tx, _ Delivery) error {
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
		w := newWorker(t, q, func(c *WorkerConfig) { c.Concurrency = 2; c.AttemptTimeout = 5 * time.Second })
		register(t, w, d)
		d.Handler = nil
		_, result := startWorker(t, w)
		await(t, func() bool { return active.Load() == 2 })
		// Twenty poll intervals give an idle worker time to claim a third message while two handlers block.
		time.Sleep(200 * time.Millisecond)
		if n := active.Load(); n != 2 {
			t.Fatalf("%d handlers are active at one time, want 2", n)
		}
		if err := w.Run(context.Background()); err == nil {
			t.Fatal("the worker ran twice")
		}
		close(release)
		await(t, func() bool { return w.Stats().Delivered == 8 })
		w.StopClaiming()
		if err := <-result; err != nil {
			t.Fatalf("Run after StopClaiming returned %v", err)
		}
		if err := w.Finish(context.Background()); err != nil {
			t.Fatal(err)
		}
		if maximum.Load() != 2 || active.Load() != 0 {
			t.Fatalf("the concurrency was %d and %d handlers are still active", maximum.Load(), active.Load())
		}
	})

	t.Run("ST-SUP-RP01-LC05-T04 Shutdown acknowledges a delivery that completed", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, nil)

		// StopClaiming does not cancel a handler that runs: a handler that honours its context ends done.
		m := message("audit", "one")
		enqueue(t, db, q, m)
		entered, stopped := make(chan struct{}), make(chan struct{})
		w := newWorker(t, q, func(c *WorkerConfig) { c.AttemptTimeout = 5 * time.Second })
		var cancelled atomic.Bool
		register(t, w, Destination{Name: "audit", Handler: func(ctx context.Context, _ *sql.Tx, _ Delivery) error {
			close(entered)
			<-stopped
			select {
			case <-ctx.Done():
				cancelled.Store(true)
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
				return nil
			}
		}})
		_, result := startWorker(t, w)
		<-entered
		w.StopClaiming()
		close(stopped)
		bound, stopBound := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopBound()
		if err := w.Finish(bound); err != nil {
			t.Fatal(err)
		}
		if cancelled.Load() {
			t.Fatal("StopClaiming cancelled the handler's context")
		}
		if err := <-result; err != nil {
			t.Fatalf("Run after StopClaiming returned %v", err)
		}
		if w.Stats().Delivered != 1 || readRow(t, db, m.ID).State != Done {
			t.Fatalf("StopClaiming lost a delivery: %+v, %+v", w.Stats(), readRow(t, db, m.ID))
		}

		// The end of the parent context cancels the handler; a success that it returns then is still recorded.
		n := message("audit", "two")
		enqueue(t, db, q, n)
		entered2, exited := make(chan struct{}), make(chan struct{})
		w2 := newWorker(t, q, func(c *WorkerConfig) { c.AttemptTimeout = 5 * time.Second })
		register(t, w2, Destination{Name: "audit", Handler: func(ctx context.Context, _ *sql.Tx, _ Delivery) error {
			close(entered2)
			<-ctx.Done()
			close(exited)
			return nil
		}})
		cancel, result2 := startWorker(t, w2)
		<-entered2
		cancel()
		if err := <-result2; !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, not the cancellation", err)
		}
		select {
		case <-exited:
		default:
			t.Fatal("the worker returned while its handler ran")
		}
		if w2.Stats().Delivered != 1 || readRow(t, db, n.ID).State != Done {
			t.Fatalf("shutdown lost a completed delivery: %+v", w2.Stats())
		}

		// The drain bound of Finish cancels a handler that waits; Finish waits for Run even when Run starts late.
		o := message("audit", "three")
		enqueue(t, db, q, o)
		w3 := newWorker(t, q, func(c *WorkerConfig) { c.AttemptTimeout = 30 * time.Second; c.LeaseDuration = time.Minute })
		entered3 := make(chan struct{})
		register(t, w3, Destination{Name: "audit", Handler: func(ctx context.Context, _ *sql.Tx, _ Delivery) error {
			close(entered3)
			<-ctx.Done()
			return ctx.Err()
		}})
		finished := make(chan error, 1)
		bound, stop := context.WithCancel(context.Background())
		defer stop()
		go func() { finished <- w3.Finish(bound) }()
		select {
		case err := <-finished:
			t.Fatalf("Finish returned %v before Run started", err)
		case <-time.After(50 * time.Millisecond):
		}
		_, result3 := startWorker(t, w3)
		<-entered3
		w3.StopClaiming()
		stop()
		select {
		case err := <-finished:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Finish at its drain bound returned %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Finish did not cancel the handler at its drain bound")
		}
		if err := <-result3; err != nil {
			t.Fatalf("Run returned %v", err)
		}
		if row := readRow(t, db, o.ID); row.State != OutcomeUnknown || w3.Stats().Delivered != 0 {
			t.Fatalf("the canceled handler left %+v", row)
		}
		if count(t, db, "SELECT count(*) FROM stego_outbox.messages WHERE state = 'dispatching'") != 0 {
			t.Fatal("a message stays in dispatching")
		}
	})
}

func TestWorkerMetrics(t *testing.T) {
	t.Run("ST-SUP-RP01-LC07-T01 The delivery delay of a message is recorded under its destination", func(t *testing.T) {
		db, _ := testDatabase(t)
		clock := newClock()
		q := newQueue(t, db, clock)
		enqueue(t, db, q, message("billing", "one"))
		reader, provider := metricReader()
		w := newWorker(t, q, func(c *WorkerConfig) { c.Meter = provider })
		register(t, w, Destination{Name: "billing", Handler: func(context.Context, *sql.Tx, Delivery) error { return nil }})
		clock.Advance(3 * time.Second)
		cycle(t, w)
		found := collect(t, reader, "outbox.delivery.delay")
		if len(found) != 1 || found[0].Unit != "s" {
			t.Fatalf("outbox.delivery.delay is %+v", found)
		}
		histogram, ok := found[0].Data.(metricdata.Histogram[float64])
		if !ok || len(histogram.DataPoints) != 1 {
			t.Fatalf("outbox.delivery.delay holds %+v", found[0].Data)
		}
		point := histogram.DataPoints[0]
		want := attribute.NewSet(attribute.String("destination", "billing"))
		if point.Count != 1 || point.Sum != 3 || !point.Attributes.Equals(&want) {
			t.Fatalf("the observation is count %d, sum %v, labels %v", point.Count, point.Sum, point.Attributes.ToSlice())
		}
	})

	t.Run("ST-SUP-RP01-LC07-T01 A message resolved as acted records its delay", func(t *testing.T) {
		db, _ := testDatabase(t)
		clock := newClock()
		q := newQueue(t, db, clock)
		m := message("billing", "one")
		enqueue(t, db, q, m)
		reader, provider := metricReader()
		w := newWorker(t, q, func(c *WorkerConfig) {
			c.Meter = provider
			c.Decider = &fakeDecider{allow: true}
			c.Auditor = tableAuditor{}
		})
		register(t, w, Destination{Name: "billing", Handler: func(context.Context, *sql.Tx, Delivery) error {
			return PermanentRefusal(errors.New("400"))
		}})
		cycle(t, w)
		clock.Advance(5 * time.Second)
		// A resolution that the caller rolls back records no delay.
		tx, err := tenant.Begin(context.Background(), db, testTenant(t, "tenant-a"), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Resolve(context.Background(), tx, ResolveRequest{ID: m.ID, Subject: "user:ops-1", Resolution: Acted}); err != nil {
			t.Fatal(err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		if found := collect(t, reader, "outbox.delivery.delay"); len(found) != 0 {
			t.Fatalf("a rolled back resolution recorded %+v", found)
		}
		resolve(t, db, w, m.ID, Acted)
		found := collect(t, reader, "outbox.delivery.delay")
		if len(found) != 1 {
			t.Fatalf("outbox.delivery.delay is %+v", found)
		}
		histogram := found[0].Data.(metricdata.Histogram[float64])
		if len(histogram.DataPoints) != 1 || histogram.DataPoints[0].Count != 1 || histogram.DataPoints[0].Sum != 5 {
			t.Fatalf("the observation is %+v", histogram.DataPoints)
		}
	})

	t.Run("ST-SUP-RP01-LC07-T02 The oldest-age gauge shows a destination that does not deliver", func(t *testing.T) {
		db, _ := testDatabase(t)
		clock := newClock()
		q := newQueue(t, db, clock)
		enqueue(t, db, q, message("authz", "document:1"), message("authz", "document:1"), message("billing", "one"))
		reader, provider := metricReader()
		w := newWorker(t, q, func(c *WorkerConfig) { c.Meter = provider; c.AttemptTimeout = 5 * time.Second })
		entered, release := make(chan struct{}, 2), make(chan struct{})
		register(t, w, Destination{Name: "authz", Idempotent: true, MustLand: true, Handler: func(context.Context, *sql.Tx, Delivery) error {
			entered <- struct{}{}
			<-release
			return nil
		}})
		register(t, w, Destination{Name: "billing", Handler: func(context.Context, *sql.Tx, Delivery) error { return nil }})
		blocked := make(chan int, 1)
		go func() { n, _ := w.cycle(context.Background(), context.Background()); blocked <- n }()
		<-entered
		if n := cycle(t, w); n != 1 || readRowByKey(t, db, "billing", "one").State != Done {
			t.Fatal("the billing message was not delivered")
		}
		clock.Advance(7 * time.Second)
		if ages := gaugeValues(t, reader); ages["authz"] != 7 || ages["billing"] != 0 || len(ages) != 2 {
			t.Fatalf("the gauge holds %v while the handler blocks", ages)
		}
		close(release)
		<-blocked
		cycle(t, w)
		if open := count(t, db, "SELECT count(*) FROM stego_outbox.messages WHERE state <> 'done'"); open != 0 {
			t.Fatalf("%d messages are not done", open)
		}
		if ages := gaugeValues(t, reader); ages["authz"] != 0 || ages["billing"] != 0 {
			t.Fatalf("the gauge holds %v after the delivery", ages)
		}
	})
}

func TestWorkerOldestAge(t *testing.T) {
	t.Run("ST-SUP-RP01-LC29-T01 OldestAge gives the age of the oldest message that is not done, and an error on a failed read", func(t *testing.T) {
		db, _ := testDatabase(t)
		clock := newClock()
		q := newQueue(t, db, clock)
		older := message("authz", "document:1")
		enqueue(t, db, q, older)
		clock.Advance(time.Second)
		enqueue(t, db, q, message("billing", "one"))
		clock.Advance(time.Second)
		enqueue(t, db, q, message("authz", "document:2"))
		clock.Advance(time.Second)
		w := newWorker(t, q, nil)
		register(t, w, Destination{Name: "authz", Idempotent: true, MustLand: true, Handler: func(context.Context, *sql.Tx, Delivery) error {
			return nil
		}})
		ctx := context.Background()
		if age, err := w.OldestAge(ctx, "authz"); err != nil || age != 3*time.Second {
			t.Fatalf("OldestAge of authz before the delivery gave %v, %v", age, err)
		}
		if n := cycle(t, w); n != 1 || readRow(t, db, older.ID).State != Done {
			t.Fatal("the older authz message was not delivered")
		}
		for destination, want := range map[string]time.Duration{"authz": time.Second, "billing": 2 * time.Second, "mail": 0} {
			if age, err := w.OldestAge(ctx, destination); err != nil || age != want {
				t.Fatalf("OldestAge of %s gave %v, %v; want %v", destination, age, err, want)
			}
		}
		var role string
		if err := db.QueryRowContext(ctx, `SELECT current_user`).Scan(&role); err != nil {
			t.Fatal(err)
		}
		if _, err := admin(t, db).ExecContext(ctx, `REVOKE EXECUTE ON FUNCTION stego_outbox.oldest(timestamptz) FROM "`+role+`"`); err != nil {
			t.Fatal(err)
		}
		age, err := w.OldestAge(ctx, "authz")
		if err == nil || age != 0 || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("OldestAge with the relay function revoked gave %v, %v", age, err)
		}
	})
}

func readRowByKey(t testing.TB, db *sql.DB, destination, key string) stored {
	t.Helper()
	var id uuid.UUID
	if err := admin(t, db).QueryRow(`SELECT id FROM stego_outbox.messages WHERE destination = $1 AND resource_key = $2
		ORDER BY sequence LIMIT 1`, destination, key).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return readRow(t, db, id)
}

// lazyQueue is a queue on a database handle that never connects, for the tests that need no server.
func lazyQueue(t *testing.T) *Queue {
	t.Helper()
	db, err := sql.Open("pgx", "postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return newQueue(t, db, nil)
}

func TestWorkerRegistration(t *testing.T) {
	nop := func(context.Context, *sql.Tx, Delivery) error { return nil }
	lookup := func(context.Context, LookupRequest) (LookupStatus, error) { return StatusActed, nil }

	t.Run("ST-SUP-RP01-LC14-T01 A destination that does not say it is idempotent counts as not idempotent", func(t *testing.T) {
		w := newWorker(t, lazyQueue(t), nil)
		register(t, w, Destination{Name: "billing", Handler: nop})
		register(t, w, Destination{Name: "mail", Handler: nop, Idempotent: true, Lookup: lookup, SettleTime: time.Second})
		register(t, w, Destination{Name: "partner", Handler: nop, Lookup: lookup, SettleTime: time.Minute})
		register(t, w, Destination{Name: "authz", Handler: nop, Idempotent: true, MustLand: true})
		got := map[string]Registration{}
		for _, r := range w.Registrations() {
			got[r.Name] = r
		}
		if b := got["billing"]; b.Idempotent || b.MustLand || b.HasLookup || len(got) != 4 {
			t.Fatalf("billing is registered as %+v", b)
		}
		if m := got["mail"]; !m.Idempotent || !m.HasLookup || m.MustLand || m.SettleTime != time.Second {
			t.Fatalf("mail is registered as %+v", m)
		}
		if p, a := got["partner"], got["authz"]; p.Idempotent || !p.HasLookup || a.HasLookup || !a.Idempotent || !a.MustLand {
			t.Fatalf("partner and authz are registered as %+v and %+v", p, a)
		}
	})

	t.Run("ST-SUP-RP01-LC14-T02 A destination with no idempotency and no lookup is accepted", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, nil)
		m := message("billing", "charge:1")
		enqueue(t, db, q, m)
		w := newWorker(t, q, nil)
		var calls atomic.Int32
		if err := w.Register(Destination{Name: "billing", Handler: func(context.Context, *sql.Tx, Delivery) error {
			calls.Add(1)
			return nil
		}}); err != nil {
			t.Fatal(err)
		}
		_, result := startWorker(t, w)
		await(t, func() bool { return w.Stats().Delivered == 1 })
		w.StopClaiming()
		if err := <-result; err != nil {
			t.Fatalf("the worker did not start and stop: %v", err)
		}
		if calls.Load() != 1 || readRow(t, db, m.ID).State != Done {
			t.Fatalf("the worker claimed no work: %d calls, %+v", calls.Load(), readRow(t, db, m.ID))
		}
	})

	t.Run("ST-SUP-RP01-LC14-T03 A status lookup without a positive settle time is refused", func(t *testing.T) {
		w := newWorker(t, lazyQueue(t), nil)
		for _, settle := range []time.Duration{0, -time.Second} {
			if err := w.Register(Destination{Name: "billing", Handler: nop, Lookup: lookup, SettleTime: settle}); err == nil {
				t.Fatalf("a status lookup with the settle time %v was accepted", settle)
			}
		}
		if got := w.Registrations(); len(got) != 0 {
			t.Fatalf("a refused destination is registered: %+v", got)
		}
	})
}

// recordingLookup is a status lookup that answers from a list and records each request.
type recordingLookup struct {
	mu       sync.Mutex
	answers  []lookupAnswer
	requests []LookupRequest
	clock    *fakeClock
	times    []time.Time
}

type lookupAnswer struct {
	status LookupStatus
	err    error
}

func (l *recordingLookup) lookup(_ context.Context, request LookupRequest) (LookupStatus, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.requests = append(l.requests, request)
	if l.clock != nil {
		l.times = append(l.times, l.clock.Now())
	}
	if len(l.answers) == 0 {
		return StatusUnknown, nil
	}
	answer := l.answers[0]
	if len(l.answers) > 1 {
		l.answers = l.answers[1:]
	}
	return answer.status, answer.err
}

func (l *recordingLookup) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.requests)
}

// runtimeUser is the role name of a runtime DSN.
func runtimeUser(t testing.TB, dsn string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return u.User.Username()
}

func TestWorkerLimits(t *testing.T) {
	t.Run("ST-SUP-RP01-LC23-T01 The attempts limit defaults to 25 and is bounded", func(t *testing.T) {
		q := lazyQueue(t)
		if DefaultWorkerConfig().MaxAttempts != 25 {
			t.Fatalf("the default attempts limit is %d", DefaultWorkerConfig().MaxAttempts)
		}
		if w := newWorker(t, q, nil); w.config.MaxAttempts != 25 {
			t.Fatalf("a worker with no attempts limit has %d", w.config.MaxAttempts)
		}
		for limit, ok := range map[int64]bool{0: false, 1001: false, 1000: true, 1: true} {
			config := testConfig()
			config.MaxAttempts = limit
			if _, err := NewWorker(q, config); (err == nil) != ok {
				t.Fatalf("the attempts limit %d gave %v", limit, err)
			}
		}
	})
}

func TestWorkerDrain(t *testing.T) {
	t.Run("ST-SUP-RP01-LC26-T01 A handler in flight during StopClaiming ends in done", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, nil)
		m, next := message("audit", "one"), message("audit", "two")
		enqueue(t, db, q, m)
		entered := make(chan struct{})
		var calls atomic.Int32
		w := newWorker(t, q, func(c *WorkerConfig) { c.AttemptTimeout = 5 * time.Second })
		register(t, w, Destination{Name: "audit", Handler: func(ctx context.Context, _ *sql.Tx, _ Delivery) error {
			if calls.Add(1) == 1 {
				close(entered)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
				return nil
			}
		}})
		_, result := startWorker(t, w)
		<-entered
		w.StopClaiming()
		enqueue(t, db, q, next)
		bound, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		if err := w.Finish(bound); err != nil {
			t.Fatal(err)
		}
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		if readRow(t, db, m.ID).State != Done || readRow(t, db, next.ID).State != Pending || calls.Load() != 1 {
			t.Fatalf("after StopClaiming: %+v, %+v, %d calls", readRow(t, db, m.ID), readRow(t, db, next.ID), calls.Load())
		}

		// A second worker gets a drain bound of 50 ms while its handler runs.
		w2 := newWorker(t, q, func(c *WorkerConfig) { c.AttemptTimeout = 30 * time.Second; c.LeaseDuration = time.Minute })
		entered2 := make(chan struct{})
		var cancelled atomic.Bool
		register(t, w2, Destination{Name: "audit", Handler: func(ctx context.Context, _ *sql.Tx, _ Delivery) error {
			close(entered2)
			<-ctx.Done()
			cancelled.Store(true)
			return ctx.Err()
		}})
		_, result2 := startWorker(t, w2)
		<-entered2
		w2.StopClaiming()
		short, stopShort := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer stopShort()
		if err := w2.Finish(short); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Finish at its bound returned %v", err)
		}
		if err := <-result2; err != nil {
			t.Fatal(err)
		}
		if !cancelled.Load() || readRow(t, db, next.ID).State != OutcomeUnknown {
			t.Fatalf("the drain bound left %+v, cancelled %v", readRow(t, db, next.ID), cancelled.Load())
		}
	})
}

func TestWorkerLease(t *testing.T) {
	t.Run("ST-SUP-RP01-LC02-T03 A handler is not called when the lease left is short", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, nil)
		m := message("audit", "one")
		enqueue(t, db, q, m)
		w := newWorker(t, q, nil)
		var calls atomic.Int32
		var deadline time.Time
		reg := Destination{Name: "audit", Handler: func(ctx context.Context, _ *sql.Tx, _ Delivery) error {
			calls.Add(1)
			deadline, _ = ctx.Deadline()
			return nil
		}}
		register(t, w, reg)
		claimed, err := q.Claim(context.Background(), 1, time.Minute)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim: %v %v", claimed, err)
		}
		d := claimed[0]
		// The lease on the local clock holds less than the attempt and the two statements after it.
		d.leaseEnd = time.Now().Add(w.config.AttemptTimeout + 2*operationTimeout - time.Second)
		w.step(context.Background(), d)
		if row := readRow(t, db, m.ID); calls.Load() != 0 || row.State != Pending || row.Attempts != 0 || row.FailureCode != "lease-short" {
			t.Fatalf("a short lease gave %d calls and %+v", calls.Load(), row)
		}
		// With a lease that holds the attempt, the handler's deadline is the attempt deadline or the lease left less
		// the two statements, whichever comes first.
		claimed, err = q.Claim(context.Background(), 1, time.Minute)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim: %v %v", claimed, err)
		}
		d = claimed[0]
		d.leaseEnd = time.Now().Add(w.config.AttemptTimeout + 2*operationTimeout + 200*time.Millisecond)
		end := d.leaseEnd
		w.step(context.Background(), d)
		if calls.Load() != 1 || deadline.After(end.Add(-2*operationTimeout)) || readRow(t, db, m.ID).State != Done {
			t.Fatalf("the handler ran %d times with the deadline %v for a lease to %v", calls.Load(), deadline, end)
		}
	})

	t.Run("ST-SUP-RP01-LC02-T03 A message that another holder finished is a lost lease", func(t *testing.T) {
		db, _ := testDatabase(t)
		q := newQueue(t, db, nil)
		m := message("audit", "one")
		enqueue(t, db, q, m)
		w := newWorker(t, q, nil)
		reg := Destination{Name: "audit", Handler: func(context.Context, *sql.Tx, Delivery) error { return nil }}
		register(t, w, reg)
		claimed, err := q.Claim(context.Background(), 1, time.Minute)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim: %v %v", claimed, err)
		}
		if ok, err := q.acknowledge(context.Background(), claimed[0].Receipt); !ok || err != nil {
			t.Fatalf("acknowledge: %v %v", ok, err)
		}
		if w.apply(context.Background(), nil, claimed[0], reg, Done, change{}) {
			t.Fatal("a done message was moved again")
		}
		if s := w.Stats(); s.LostLeases != 1 || s.DatabaseFailures != 0 {
			t.Fatalf("a done row counted as %+v", s)
		}
	})
}

func TestWorkerPool(t *testing.T) {
	t.Run("ST-SUP-RP01-LC05-T01 A pool with fewer than Concurrency + 2 connections is refused", func(t *testing.T) {
		q := lazyQueue(t)
		q.db.SetMaxOpenConns(3)
		config := testConfig()
		config.Concurrency = 2
		if _, err := NewWorker(q, config); err == nil {
			t.Fatal("a pool of 3 was accepted for a concurrency of 2")
		}
		q.db.SetMaxOpenConns(4)
		if _, err := NewWorker(q, config); err != nil {
			t.Fatalf("a pool of 4: %v", err)
		}
		q.db.SetMaxOpenConns(0)
		if _, err := NewWorker(q, config); err != nil {
			t.Fatalf("a pool with no limit: %v", err)
		}
	})
}

func TestWorkerBeginFailure(t *testing.T) {
	t.Run("ST-SUP-RP01-LC22-T01 A failed begin takes back no attempt", func(t *testing.T) {
		db, _ := testDatabase(t)
		m := message("audit", "one")
		enqueue(t, db, newQueue(t, db, nil), m)
		faulty := sql.OpenDB(faultConnector{database: db, failBegin: true})
		t.Cleanup(func() { _ = faulty.Close() })
		w := newWorker(t, newQueue(t, faulty, nil), nil)
		var calls atomic.Int32
		register(t, w, Destination{Name: "audit", Handler: func(context.Context, *sql.Tx, Delivery) error {
			calls.Add(1)
			return nil
		}})
		if n := cycle(t, w); n != 1 {
			t.Fatalf("the cycle claimed %d", n)
		}
		if row := readRow(t, db, m.ID); calls.Load() != 0 || row.Attempts != 1 || w.Stats().DatabaseFailures == 0 {
			t.Fatalf("a failed begin left %+v with %d calls", row, calls.Load())
		}
	})
}
