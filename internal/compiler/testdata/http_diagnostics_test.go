package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHTTPDiagnosticsWithoutTelemetry(t *testing.T) {
	for _, abort := range []bool{false, true} {
		listener := testListener(t)
		var output bytes.Buffer
		server := stegoHTTPServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/panic" {
				if abort {
					panic(http.ErrAbortHandler)
				}
				panic("private-http-fault")
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		diagnostics := server.ErrorLog.Writer().(*stegoHTTPDiagnostics)
		diagnostics.output = &output
		if diagnostics.queue != nil {
			t.Fatal("unused logger started a worker")
		}
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { result <- stegoServeHTTP(ctx, listener, server, time.Second) }()
		transport := &http.Transport{DisableKeepAlives: true}
		client := &http.Client{Transport: transport, Timeout: time.Second}
		response, err := client.Get("http://" + listener.Addr().String() + "/panic")
		if err == nil {
			response.Body.Close()
			t.Error("panic did not abort request")
		}
		response, err = client.Get("http://" + listener.Addr().String() + "/ready")
		if err != nil {
			t.Error("server did not recover", err)
		} else {
			response.Body.Close()
			if response.StatusCode != 204 {
				t.Error("invalid recovery status")
			}
		}
		transport.CloseIdleConnections()
		cancel()
		if err := waitResult(t, result); err != nil {
			t.Fatal(err)
		}
		if abort {
			if output.Len() != 0 {
				t.Fatal("ErrAbortHandler produced a diagnostic")
			}
			continue
		}
		if strings.Contains(output.String(), "private-http-fault") || strings.Contains(output.String(), "goroutine") || strings.Contains(output.String(), ".go:") {
			t.Fatal("server exposed private diagnostics")
		}
		var record map[string]any
		if err := json.Unmarshal(output.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		if len(record) != 4 || record["event.name"] != "http.server.diagnostic" || record["severity"] != "ERROR" {
			t.Fatal("invalid fallback diagnostic", record)
		}
	}
}

type blockedHTTPOutput struct {
	once             sync.Once
	entered, release chan struct{}
}

func (w *blockedHTTPOutput) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(data), nil
}
func TestHTTPDiagnosticQueueAndShutdownBounds(t *testing.T) {
	output := &blockedHTTPOutput{entered: make(chan struct{}), release: make(chan struct{})}
	logger := stegoNewHTTPErrorLog(output)
	diagnostics := logger.Writer().(*stegoHTTPDiagnostics)
	logger.Print("private-first-diagnostic")
	<-output.entered
	for range 512 {
		logger.Print("private-next-diagnostic")
	}
	if diagnostics.dropped.Load() != 256 {
		t.Fatal("queue did not retain its bound", diagnostics.dropped.Load())
	}
	returned := make(chan struct{})
	go func() { diagnostics.close(); close(returned) }()
	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Error("diagnostic close blocked")
	}
	logger.Print("private-after-close")
	close(output.release)
	select {
	case <-diagnostics.done:
	case <-time.After(3 * time.Second):
		t.Fatal("diagnostic worker did not stop")
	}
	if diagnostics.dropped.Load() != 257 {
		t.Fatal("closed logger accepted output")
	}
}

type failedHTTPOutput struct{}

func (failedHTTPOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func TestHTTPDiagnosticWriteFailureDoesNotRetry(t *testing.T) {
	logger := stegoNewHTTPErrorLog(failedHTTPOutput{})
	diagnostics := logger.Writer().(*stegoHTTPDiagnostics)
	for range 4 {
		logger.Print("private")
	}
	diagnostics.close()
	if diagnostics.failures.Load() != 4 {
		t.Fatal("output failures were retried or lost")
	}
	logger = stegoNewHTTPErrorLog(io.Discard)
	diagnostics = logger.Writer().(*stegoHTTPDiagnostics)
	diagnostics.close()
	if diagnostics.queue != nil {
		t.Fatal("unused close started a worker")
	}
}
