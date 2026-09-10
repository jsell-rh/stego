package tracing

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"
)

func TestServiceLogsLocalAndOTLP(t *testing.T) {
	for _, mode := range []string{"local", "otlp", "logs-none"} {
		t.Run(mode, func(t *testing.T) {
			var collector *traceCollector
			if mode == "local" {
				traceEnvironment(t)
			} else {
				collector = collectorFixture(t, false)
			}
			if mode == "logs-none" {
				t.Setenv("OTEL_LOGS_EXPORTER", "none")
			}
			t.Setenv("OTEL_SERVICE_NAME", "records-test")
			before := slog.Default()
			var output bytes.Buffer
			runtime, err := newRuntime(&output)
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()
			if slog.Default() != before {
				t.Fatal("runtime changed the process logger")
			}
			traceID, _ := trace.TraceIDFromHex("11111111111111111111111111111111")
			spanID, _ := trace.SpanIDFromHex("2222222222222222")
			ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID}))
			// A context value cannot become a log field.
			type privateKey struct{}
			ctx = context.WithValue(ctx, privateKey{}, "private-credential")
			if !runtime.LogServiceEvent(ctx, ServiceReady) || !runtime.LogServiceEvent(ctx, ServiceFailed) {
				t.Fatal("declared event rejected")
			}
			if runtime.LogServiceEvent(ctx, ServiceEvent(255)) {
				t.Fatal("unknown event accepted")
			}
			runtime.Close()
			if runtime.LogServiceEvent(ctx, ServiceReady) {
				t.Fatal("closed runtime accepted an event")
			}
			decoder := json.NewDecoder(&output)
			for _, event := range []ServiceEvent{ServiceReady, ServiceFailed} {
				var record localRecord
				if err := decoder.Decode(&record); err != nil {
					t.Fatal(err)
				}
				name, message, severity, _ := event.description()
				if record.Service != "records-test" || record.Event != name || record.Message != message || record.Severity != severity || record.Time.IsZero() || record.TraceID != traceID.String() || record.SpanID != spanID.String() {
					t.Fatal("incorrect local event", record)
				}
			}
			var extra localRecord
			if err := decoder.Decode(&extra); err != io.EOF {
				t.Fatal("unexpected local event", err)
			}
			if mode != "otlp" {
				if runtime.service.logger != nil {
					t.Fatal("disabled export created a service logger")
				}
				return
			}
			count := 0
			timer := time.NewTimer(time.Second)
			defer timer.Stop()
			for count < 2 {
				select {
				case batch := <-collector.logs:
					data, _ := proto.Marshal(batch)
					if strings.Contains(string(data), "private-credential") {
						t.Fatal("log exposed a context value")
					}
					for _, resource := range batch.ResourceLogs {
						if len(resource.Resource.Attributes) != 2 || value(resource.Resource.Attributes, "service.instance.id").GetStringValue() != runtime.instance || value(resource.Resource.Attributes, "service.name").GetStringValue() != "records-test" {
							t.Fatal("incorrect service identity")
						}
						for _, scope := range resource.ScopeLogs {
							if scope.Scope.Name != "stego/service" {
								t.Fatal("incorrect service log scope")
							}
							for _, record := range scope.LogRecords {
								if count >= 2 {
									t.Fatal("unexpected exported event")
								}
								name, message, severity, _ := []ServiceEvent{ServiceReady, ServiceFailed}[count].description()
								if record.EventName != name || record.Body.GetStringValue() != message || record.SeverityText != severity || hex.EncodeToString(record.TraceId) != traceID.String() || hex.EncodeToString(record.SpanId) != spanID.String() || len(record.Attributes) != 0 {
									t.Fatal("incorrect exported event", record)
								}
								count++
							}
						}
					}
				case <-timer.C:
					t.Fatal("service logs were not exported")
				}
			}
		})
	}
}

type blockedLogWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockedLogWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}

func TestBlockedServiceLogOutputIsBounded(t *testing.T) {
	collectorFixture(t, true)
	writer := &blockedLogWriter{entered: make(chan struct{}), release: make(chan struct{})}
	runtime, err := newRuntime(writer)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		close(writer.release)
		runtime.Close()
		select {
		case <-runtime.service.local.done:
		case <-time.After(time.Second):
			t.Error("released log worker did not stop")
		}
	}()
	runtime.LogServiceEvent(context.Background(), ServiceReady)
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("local writer did not start")
	}
	started := time.Now()
	for i := 0; i < QueueSize*3; i++ {
		runtime.LogServiceEvent(context.Background(), ServiceFailed)
	}
	if time.Since(started) > time.Second {
		t.Fatal("log queue blocked the caller")
	}
	if runtime.LocalLogDrops() != QueueSize*2 {
		t.Fatal("local queue is not bounded", runtime.LocalLogDrops())
	}
	runtime.service.lifecycle = true
	started = time.Now()
	runtime.Close()
	if elapsed := time.Since(started); elapsed < ShutdownTimeout || elapsed > ShutdownTimeout+time.Second {
		t.Fatal("shared shutdown budget not enforced", elapsed)
	}
	if runtime.LocalLogDrops() != QueueSize*2+2 {
		t.Fatal("final events bypassed the queue bound")
	}
	if runtime.LogServiceEvent(context.Background(), ServiceFailed) {
		t.Fatal("closed queue accepted an event")
	}
}

type failedLogWriter struct{}

func (failedLogWriter) Write([]byte) (int, error) { return 0, errors.New("private-output-error") }
func TestServiceLogWriteFailureAndConcurrentClose(t *testing.T) {
	traceEnvironment(t)
	runtime, err := newRuntime(failedLogWriter{})
	if err != nil {
		t.Fatal(err)
	}
	runtime.LogServiceEvent(context.Background(), ServiceReady)
	var callers sync.WaitGroup
	for i := 0; i < 8; i++ {
		callers.Add(1)
		go func() {
			defer callers.Done()
			for j := 0; j < 100; j++ {
				runtime.LogServiceEvent(context.Background(), ServiceFailed)
			}
		}()
	}
	callers.Add(1)
	go func() { defer callers.Done(); runtime.Close() }()
	callers.Wait()
	runtime.Close()
	if runtime.LocalLogFailures() == 0 {
		t.Fatal("output failure was not counted")
	}
	select {
	case <-runtime.service.local.done:
	default:
		t.Fatal("local writer did not stop")
	}
}

func TestServiceIdentityIsValidatedWithoutCollector(t *testing.T) {
	traceEnvironment(t)
	for _, service := range []string{"private\ncredential", strings.Repeat("x", 129), "name=value"} {
		t.Setenv("OTEL_SERVICE_NAME", service)
		if _, err := NewRuntime(); err == nil || strings.Contains(err.Error(), service) {
			t.Fatal("invalid identity accepted or exposed")
		}
	}
}

func BenchmarkServiceLogs(b *testing.B) {
	traceEnvironment(b)
	runtime, err := newRuntime(io.Discard)
	if err != nil {
		b.Fatal(err)
	}
	defer runtime.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runtime.LogServiceEvent(context.Background(), ServiceReady)
	}
	b.StopTimer()
	b.ReportMetric(float64(runtime.LocalLogDrops())/float64(b.N), "drops/op")
}
