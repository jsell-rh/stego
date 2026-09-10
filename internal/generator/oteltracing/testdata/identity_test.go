package tracing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

var instancePattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func checkResourceIdentity(t *testing.T, attrs []*commonpb.KeyValue, service, instance string) {
	t.Helper()
	if len(attrs) != 2 || value(attrs, "service.name").GetStringValue() != service || value(attrs, "service.instance.id").GetStringValue() != instance || !instancePattern.MatchString(instance) {
		t.Fatal("invalid telemetry resource identity", attrs)
	}
}

type failedEntropy struct{}

func (failedEntropy) Read([]byte) (int, error) { return 0, errors.New("private entropy error") }
func TestInstanceIdentityFormatAndEntropyFailure(t *testing.T) {
	data := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	reader := bytes.NewReader(data)
	got, err := newInstanceID(reader)
	if err != nil || got != "00010203-0405-4607-8809-0a0b0c0d0e0f" || reader.Len() != 1 {
		t.Fatal("invalid UUID encoding", got, err)
	}
	for _, source := range []io.Reader{bytes.NewReader(data[:15]), failedEntropy{}} {
		value, err := newInstanceID(source)
		if value != "" || err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal("entropy failure produced identity or private error", value, err)
		}
	}
}
func TestConcurrentAndReplacementRuntimesHaveDistinctIdentities(t *testing.T) {
	traceEnvironment(t)
	ids := make(chan string, 32)
	var group sync.WaitGroup
	for i := 0; i < cap(ids); i++ {
		group.Go(func() {
			runtime, err := newRuntime(io.Discard)
			if err != nil {
				t.Error(err)
				return
			}
			id := runtime.instance
			runtime.Close()
			if runtime.instance != id {
				t.Error("close changed runtime identity")
			}
			ids <- id
		})
	}
	group.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		if !instancePattern.MatchString(id) || seen[id] {
			t.Fatal("runtime identity is invalid or repeated", id)
		}
		seen[id] = true
	}
	if len(seen) != 32 {
		t.Fatal("runtime creation failed")
	}
}
func TestAllSignalsAndLocalLogsShareRuntimeIdentity(t *testing.T) {
	sink := collectorFixture(t, false)
	var output bytes.Buffer
	runtime, err := newRuntime(&output)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	runtime.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/records", nil))
	runtime.LogServiceEvent(context.Background(), ServiceReady)
	runtime.Close()
	var local localRecord
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &local); err != nil || local.Instance != runtime.instance {
		t.Fatal("local log identity differs", err)
	}
	var traces, logs, metrics bool
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for !traces || !logs || !metrics {
		select {
		case batch := <-sink.received:
			for _, item := range batch.ResourceSpans {
				checkResourceIdentity(t, item.Resource.Attributes, "records", runtime.instance)
			}
			traces = true
		case batch := <-sink.logs:
			for _, item := range batch.ResourceLogs {
				checkResourceIdentity(t, item.Resource.Attributes, "records", runtime.instance)
			}
			logs = true
		case batch := <-sink.metrics:
			for _, item := range batch.ResourceMetrics {
				checkResourceIdentity(t, item.Resource.Attributes, "records", runtime.instance)
			}
			metrics = true
		case <-timer.C:
			t.Fatal("one telemetry signal has no identity evidence")
		}
	}
}
