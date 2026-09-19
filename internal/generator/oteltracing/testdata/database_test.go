package tracing

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestAuthKeyRefreshSignals(t *testing.T)    { testDatabaseSignals(t, false, "auth-keys") }
func TestDatabaseSignals(t *testing.T)          { testDatabaseSignals(t, false) }
func TestPostgresOperationSignals(t *testing.T) { testDatabaseSignals(t, true) }
func TestSchemaOperationSignals(t *testing.T)   { testDatabaseSignals(t, true, "schema") }
func TestPrepareOperationSignals(t *testing.T)  { testDatabaseSignals(t, true, "prepare") }
func testDatabaseSignals(t *testing.T, provision bool, known ...string) {
	signal, scopeName, callName, knownCall, event, durationMetric, activeMetric := TraceDatabase, "stego/database", "stego.db.call", "prepare", "db.client.operation.completed", "db.client.operation.duration", "stego.db.active_calls"
	if provision {
		signal = TracePostgresOperation
		scopeName = "stego/postgres-client"
		callName = "operation"
		knownCall = "quarantine"
		event = "postgres.database.completed"
		durationMetric = "stego.postgres.database.duration"
		activeMetric = "stego.postgres.database.active"
	}

	if len(known) == 1 {
		knownCall = known[0]
	}
	if knownCall == "auth-keys" {
		signal, scopeName, callName, knownCall = TraceAuthKeyRefresh, "stego/auth-keys", "stego.auth.keys.source", "https"
		event, durationMetric, activeMetric = "auth.keys.refresh.completed", "stego.auth.keys.refresh.duration", "stego.auth.keys.refresh.active"
	}

	for _, sample := range []string{"1", "0"} {
		t.Run(sample, func(t *testing.T) {
			sink := collectorFixture(t, false)
			t.Setenv("OTEL_TRACES_SAMPLER_ARG", sample)
			var local bytes.Buffer
			r, err := newRuntime(&local)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			expected := map[string]string{}
			var childID, parentID string
			for _, outcome := range []string{"success", "failure", "canceled", "deadline", "aborted", "private-outcome"} {
				ctx, finish := signal(r.Context(context.Background()), "private-call")
				sc := trace.SpanContextFromContext(ctx)
				if !sc.IsValid() {
					t.Fatal("database has no span context")
				}
				if outcome == "success" {
					child, done := signal(ctx, knownCall)
					childID = trace.SpanContextFromContext(child).SpanID().String()
					parentID = sc.SpanID().String()
					done("success")
				}
				finish(outcome)
				finish("failure")
				if outcome == "private-outcome" {
					outcome = "failure"
				}
				expected[sc.SpanID().String()] = outcome
			}
			r.Close()
			check := func(m proto.Message) {
				data, _ := proto.Marshal(m)
				if bytes.Contains(data, []byte("private-")) {
					t.Fatal("database telemetry exposed private data")
				}
			}
			spans := map[string]*tracepb.Span{}
			for len(sink.received) > 0 {
				batch := <-sink.received
				check(batch)
				for _, res := range batch.ResourceSpans {
					for _, scope := range res.ScopeSpans {
						for _, span := range scope.Spans {
							spans[hex.EncodeToString(span.SpanId)] = span
						}
					}
				}
			}
			logs := 0
			for len(sink.logs) > 0 {
				batch := <-sink.logs
				check(batch)
				for _, res := range batch.ResourceLogs {
					for _, scope := range res.ScopeLogs {
						if scope.Scope.Name != scopeName {
							continue
						}
						for _, record := range scope.LogRecords {
							if value(record.Attributes, callName).GetStringValue() == knownCall {
								continue
							}
							id := hex.EncodeToString(record.SpanId)
							outcome, ok := expected[id]
							if !ok || record.EventName != event || value(record.Attributes, "outcome").GetStringValue() != outcome {
								t.Fatal("wrong database completion")
							}
							logs++
							if sample == "1" {
								span := spans[id]
								if span == nil || span.Kind != tracepb.Span_SPAN_KIND_CLIENT || len(span.ParentSpanId) != 0 || !bytes.Equal(span.TraceId, record.TraceId) {
									t.Fatal("database root lost correlation")
								}
								if (span.GetStatus().GetCode() == tracepb.Status_STATUS_CODE_ERROR) != (outcome != "success" && outcome != "canceled") {
									t.Fatal("wrong database error status")
								}
							}
						}
					}
				}
			}
			var total uint64
			active := int64(-1)
			for len(sink.metrics) > 0 {
				batch := <-sink.metrics
				check(batch)
				for _, res := range batch.ResourceMetrics {
					for _, scope := range res.ScopeMetrics {
						if scope.Scope.Name != scopeName {
							continue
						}
						for _, m := range scope.Metrics {
							switch m.Name {
							case durationMetric:
								total = 0
								for _, p := range m.GetHistogram().DataPoints {
									total += p.Count
								}
							case activeMetric:
								for _, p := range m.GetSum().DataPoints {
									active = p.GetAsInt()
								}
							}
						}
					}
				}
			}
			if logs != 6 || total != 7 || active != 0 {
				t.Fatal("database completion missing or repeated", logs, total, active)
			}
			if sample == "1" {
				child := spans[childID]
				if child == nil || hex.EncodeToString(child.ParentSpanId) != parentID {
					t.Fatal("Prepared call lost database parent")
				}
			} else if len(spans) != 0 {
				t.Fatal("unsampled database exported spans")
			}
			if strings.Contains(local.String(), "private-") || strings.Count(local.String(), `"event.name":"`+event+`"`) != 7 {
				t.Fatal("local database completion failed")
			}
		})
	}
}

func TestPostgresOperationLocalAndCollectorFailure(t *testing.T) {
	traceEnvironment(t)
	var local bytes.Buffer
	r, err := newRuntime(&local)
	if err != nil {
		t.Fatal(err)
	}
	_, done := TracePostgresOperation(r.Context(context.Background()), "ensure")
	done("success")
	done("failure")
	r.Close()
	_, done = TracePostgresOperation(r.Context(context.Background()), "delete")
	done("failure")
	if strings.Count(local.String(), `"event.name":"postgres.database.completed"`) != 1 || !strings.Contains(local.String(), `"operation":"ensure"`) {
		t.Fatal("disabled export lost bounded local logging")
	}
	_, done = TracePostgresOperation(nil, "private-operation")
	done("private-outcome")
	_, done = TracePostgresOperation(context.Background(), "private-operation")
	done("private-outcome")

	collectorFixture(t, true)
	t.Setenv("OTEL_LOGS_EXPORTER", "otlp")
	t.Setenv("OTEL_METRICS_EXPORTER", "otlp")
	r, err = newRuntime(io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	start := time.Now()
	for range 16 {
		_, done = TracePostgresOperation(r.Context(context.Background()), "ensure")
		done("success")
	}
	if time.Since(start) > time.Second {
		t.Fatal("blocked collector delayed client completions")
	}
	start = time.Now()
	r.Close()
	if time.Since(start) > ShutdownTimeout+time.Second {
		t.Fatal("blocked collector exceeded the shared close bound")
	}
}

func TestAuthKeyRefreshLocalAndCollectorFailure(t *testing.T) {
	traceEnvironment(t)
	var local bytes.Buffer
	r, err := newRuntime(&local)
	if err != nil {
		t.Fatal(err)
	}
	_, done := TraceAuthKeyRefresh(r.Context(context.Background()), "https")
	done("success")
	done("failure")
	r.Close()
	_, done = TraceAuthKeyRefresh(r.Context(context.Background()), "file")
	done("failure")
	if strings.Count(local.String(), `"event.name":"auth.keys.refresh.completed"`) != 1 || !strings.Contains(local.String(), `"operation":"https"`) {
		t.Fatal("disabled export lost bounded local logging")
	}
	_, done = TraceAuthKeyRefresh(nil, "private-operation")
	done("private-outcome")
	_, done = TraceAuthKeyRefresh(context.Background(), "private-operation")
	done("private-outcome")

	collectorFixture(t, true)
	t.Setenv("OTEL_LOGS_EXPORTER", "otlp")
	t.Setenv("OTEL_METRICS_EXPORTER", "otlp")
	r, err = newRuntime(io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	start := time.Now()
	for range 16 {
		_, done = TraceAuthKeyRefresh(r.Context(context.Background()), "https")
		done("success")
	}
	if time.Since(start) > time.Second {
		t.Fatal("blocked collector delayed client completions")
	}
	start = time.Now()
	r.Close()
	if time.Since(start) > ShutdownTimeout+time.Second {
		t.Fatal("blocked collector exceeded the shared close bound")
	}
}

func BenchmarkDatabaseSignals(b *testing.B) {
	for _, mode := range []string{"unbound", "local", "otlp"} {
		b.Run(mode, func(b *testing.B) {
			var stop chan struct{}
			if mode != "otlp" {
				traceEnvironment(b)
			} else {
				sink := collectorFixture(b, false)
				stop = make(chan struct{})
				defer close(stop)
				go func() {
					for {
						select {
						case <-sink.received:
						case <-sink.logs:
						case <-sink.metrics:
						case <-stop:
							return
						}
					}
				}()
			}
			runtime, err := newRuntime(io.Discard)
			if err != nil {
				b.Fatal(err)
			}
			defer runtime.Close()
			ctx := context.Background()
			if mode != "unbound" {
				ctx = runtime.Context(ctx)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, finish := TraceDatabase(ctx, "query")
				finish("success")
			}
			b.StopTimer()
			b.ReportMetric(float64(runtime.LocalLogDrops())/float64(b.N), "local_drops/op")
		})
	}
}

// Use database/sql's real pool and wait accounting with a driver that has no
// network or database. The application test supplies the PostgreSQL check.
type poolTestConnector struct{}

func (poolTestConnector) Connect(context.Context) (driver.Conn, error) { return poolTestConn{}, nil }
func (poolTestConnector) Driver() driver.Driver                        { return poolTestDriver{} }

type poolTestDriver struct{}

func (poolTestDriver) Open(string) (driver.Conn, error) { return poolTestConn{}, nil }

type poolTestConn struct{}

func (poolTestConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (poolTestConn) Close() error                        { return nil }
func (poolTestConn) Begin() (driver.Tx, error)           { return nil, errors.New("unused") }

func TestDatabasePoolMetricsLifecycle(t *testing.T) {
	sink := collectorFixture(t, false)
	pool := sql.OpenDB(poolTestConnector{})
	defer pool.Close()
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	r, err := NewTracingRuntime(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	collect := func() map[string]*metricpb.Metric {
		t.Helper()
		if err := r.signals.meter.ForceFlush(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case batch := <-sink.metrics:
			for _, resource := range batch.ResourceMetrics {
				if value(resource.Resource.Attributes, "service.instance.id").GetStringValue() != r.instance {
					t.Fatal("pool resource identity differs")
				}
				for _, scope := range resource.ScopeMetrics {
					for _, m := range scope.Metrics {
						if strings.HasPrefix(m.Name, "stego.db.pool.") && scope.Scope.Name != "stego/database" {
							t.Fatal("pool scope differs")
						}
					}
				}
			}
			return metricMap(batch)
		case <-ctx.Done():
			t.Fatal("pool metrics missing")
			return nil
		}
	}
	gauge := func(metrics map[string]*metricpb.Metric, state string) int64 {
		t.Helper()
		m := metrics["stego.db.pool.connections"]
		if m == nil || m.Unit != "{connection}" || len(m.GetGauge().GetDataPoints()) != 2 {
			t.Fatal("pool connection metric differs", m)
		}
		for _, point := range m.GetGauge().GetDataPoints() {
			if len(point.Attributes) != 1 {
				t.Fatal("unexpected pool attributes")
			}
			if value(point.Attributes, "state").GetStringValue() == state {
				return point.GetAsInt()
			}
		}
		t.Fatal("pool state missing", state)
		return -1
	}
	first := collect()
	if gauge(first, "used") != 1 || gauge(first, "idle") != 0 {
		t.Fatal("held connection is not visible")
	}
	limit := first["stego.db.pool.limit"]
	if limit == nil || len(limit.GetGauge().GetDataPoints()) != 1 || limit.GetGauge().DataPoints[0].GetAsInt() != 1 || len(limit.GetGauge().DataPoints[0].Attributes) != 0 {
		t.Fatal("pool limit differs", limit)
	}
	wait, cancelWait := context.WithTimeout(ctx, 25*time.Millisecond)
	_, err = pool.Conn(wait)
	cancelWait()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("pool wait did not expire", err)
	}
	stats := pool.Stats()
	second := collect()
	for _, name := range []string{"stego.db.pool.waits", "stego.db.pool.wait.duration", "stego.db.pool.connections.closed"} {
		m := second[name]
		if m == nil || !m.GetSum().GetIsMonotonic() || m.GetSum().GetAggregationTemporality() != metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE {
			t.Fatal("pool counter differs", m)
		}
	}
	waits := second["stego.db.pool.waits"].GetSum().DataPoints
	duration := second["stego.db.pool.wait.duration"].GetSum().DataPoints
	if len(waits) != 1 || len(duration) != 1 || waits[0].GetAsInt() != stats.WaitCount || stats.WaitCount != 1 || duration[0].GetAsDouble() != stats.WaitDuration.Seconds() || stats.WaitDuration <= 0 || len(waits[0].Attributes) != 0 || len(duration[0].Attributes) != 0 {
		t.Fatal("pool wait totals differ")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	third := collect()
	if gauge(third, "used") != 0 || gauge(third, "idle") != 1 {
		t.Fatal("released connection is not visible")
	}
	pool.SetMaxIdleConns(0)
	final := collect()
	closed := final["stego.db.pool.connections.closed"].GetSum().DataPoints
	if len(closed) != 3 {
		t.Fatal("pool retirement reasons differ")
	}
	for _, point := range closed {
		reason := value(point.Attributes, "reason").GetStringValue()
		if len(point.Attributes) != 1 || reason != "idle_limit" && reason != "idle_time" && reason != "lifetime" {
			t.Fatal("unknown pool retirement reason")
		}
		expected := int64(0)
		if reason == "idle_limit" {
			expected = 1
		}
		if point.GetAsInt() != expected {
			t.Fatal("pool retirement count differs")
		}
	}
	r.Close()
	r.Close()
	// Telemetry must not close the application's pool.
	after, err := pool.Conn(ctx)
	if err != nil {
		t.Fatal("telemetry closed the pool", err)
	}
	after.Close()
}

func TestDatabasePoolMetricsOptionalAndDisabled(t *testing.T) {
	if r, err := NewTracingRuntime(nil, nil); err == nil || r != nil {
		t.Fatal("multiple process pools accepted")
	}
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprint(disabled), func(t *testing.T) {
			if disabled {
				collectorFixture(t, false)
				t.Setenv("OTEL_METRICS_EXPORTER", "none")
			} else {
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			}
			pool := sql.OpenDB(poolTestConnector{})
			defer pool.Close()
			for _, input := range []*sql.DB{nil, pool} {
				r, err := NewTracingRuntime(input)
				if err != nil {
					t.Fatal(err)
				}
				if r.database.poolRegistration != nil {
					t.Fatal("disabled metric callback registered")
				}
				r.Close()
			}
		})
	}
}

func TestDatabasePoolMetricsCollectorFailure(t *testing.T) {
	collectorFixture(t, true)
	pool := sql.OpenDB(poolTestConnector{})
	defer pool.Close()
	pool.SetMaxOpenConns(1)
	r, err := NewTracingRuntime(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	err = r.signals.meter.ForceFlush(ctx)
	cancel()
	if err == nil {
		t.Fatal("blocked collector flush passed")
	}
	before := time.Now()
	r.Close()
	if time.Since(before) > ShutdownTimeout+time.Second {
		t.Fatal("pool metrics delayed shutdown")
	}
	use, cancelUse := context.WithTimeout(context.Background(), time.Second)
	defer cancelUse()
	conn, err := pool.Conn(use)
	if err != nil {
		t.Fatal("collector failure changed pool access", err)
	}
	conn.Close()
}
