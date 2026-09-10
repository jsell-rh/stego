package tracing

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
)

func TestHTTPDiagnosticLoggerUsesSafeServiceEvents(t *testing.T) {
	collector := collectorFixture(t, false)
	var output bytes.Buffer
	runtime, err := newRuntime(&output)
	if err != nil {
		t.Fatal(err)
	}
	logger := runtime.HTTPErrorLog()
	logger.Print("private-http-diagnostic\nprivate-stack-and-path")
	runtime.Close()
	var record localRecord
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "private-") || record.Event != "http.server.diagnostic" || record.Instance != runtime.instance || record.Severity != "ERROR" || record.TraceID != "" || record.SpanID != "" {
		t.Fatal("invalid local HTTP diagnostic", record)
	}
	select {
	case batch := <-collector.logs:
		data, _ := proto.Marshal(batch)
		if bytes.Contains(data, []byte("private-")) {
			t.Fatal("HTTP diagnostic exposed raw input")
		}
		count := 0
		for _, resource := range batch.ResourceLogs {
			for _, scope := range resource.ScopeLogs {
				for _, record := range scope.LogRecords {
					count++
					if record.EventName != "http.server.diagnostic" || record.SeverityNumber != 17 || len(record.Attributes) != 0 || len(record.TraceId) != 0 || len(record.SpanId) != 0 {
						t.Fatal("invalid exported HTTP diagnostic")
					}
				}
			}
		}
		if count != 1 {
			t.Fatal("unexpected diagnostic count", count)
		}
	case <-time.After(time.Second):
		t.Fatal("HTTP diagnostic was not exported")
	}
}

func TestHTTPDiagnosticLoggerSharesQueueAndClose(t *testing.T) {
	traceEnvironment(t)
	writer := &blockedLogWriter{entered: make(chan struct{}), release: make(chan struct{})}
	runtime, err := newRuntime(writer)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { close(writer.release); runtime.Close(); <-runtime.service.local.done }()
	logger := runtime.HTTPErrorLog()
	logger.Print("private-first")
	<-writer.entered
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			for range QueueSize {
				logger.Print("private-more")
			}
		})
	}
	group.Wait()
	if runtime.LocalLogDrops() == 0 {
		t.Fatal("HTTP diagnostic bypassed the shared queue bound")
	}
	returned := make(chan struct{})
	go func() { runtime.Close(); close(returned) }()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP diagnostic blocked runtime close")
	}
	logger.Print("private-after-close")
}
