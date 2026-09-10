package tracing_test

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"example.com/records/tracing"
	"example.com/records/web"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type privateCallbackError struct{}

func (privateCallbackError) Error() string { panic("private error was formatted") }

func TestHTTPClientWireCompletion(t *testing.T) {
	r, recorder, output := tracing.HTTPClientTestRuntime(t)
	var mu sync.Mutex
	var wire []http.Header
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		wire = append(wire, req.Header.Clone())
		mu.Unlock()
		switch req.URL.Path {
		case "/private-denied":
			w.WriteHeader(403)
		case "/private-failed":
			w.WriteHeader(503)
		case "/private-redirect":
			w.Header().Set("Location", "https://private-redirect.invalid")
			w.WriteHeader(302)
		case "/private-large":
			w.Write([]byte(strings.Repeat("x", web.MaxResponseBytes+1)))
		case "/private-stream":
			w.Write([]byte("private-frame\n"))
			w.(http.Flusher).Flush()
			<-req.Context().Done()
		case "/private-eof":
			w.Write([]byte("private-frame\n"))
		case "/private-timeout":
			<-req.Context().Done()
		default:
			w.Write([]byte("private-response"))
		}
	}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	server.StartTLS()
	defer server.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := web.New(web.Options{BaseURL: server.URL, CAFile: ca, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx := r.Context(context.Background())
	original := http.Header{"Authorization": {"private-token"}, "traceparent": {"private-parent"}, "tracestate": {"private-state"}, "baggage": {"private-baggage"}}
	attr := func(span sdktrace.ReadOnlySpan, key string) attribute.Value {
		for _, a := range span.Attributes() {
			if string(a.Key) == key {
				return a.Value
			}
		}
		return attribute.Value{}
	}
	check := func(before int, kind, outcome string, status int) {
		t.Helper()
		spans := recorder.Ended()
		if len(spans) != before+1 {
			t.Fatal("request did not finish once", len(spans), before)
		}
		span := spans[before]
		if span.SpanKind() != trace.SpanKindClient || attr(span, "error.type").AsString() != kind || attr(span, "outcome").AsString() != outcome || attr(span, "http.response.status_code").AsInt64() != int64(status) {
			t.Fatal("wrong request outcome", span.Attributes())
		}
		if (span.Status().Code == codes.Error) != (kind != "") {
			t.Fatal("wrong request span status")
		}
	}
	for _, item := range []struct {
		path, kind string
		status     int
		failure    bool
	}{{"/private-ok?secret=private-query", "", 200, false}, {"/private-denied", "403", 403, false}, {"/private-failed", "503", 503, false}, {"/private-redirect", "redirect", 302, true}, {"/private-large", "response", 200, true}, {"/private-timeout", "deadline", 0, true}} {
		before := len(recorder.Ended())
		response, err := client.Do(ctx, "GET", item.path, original, nil)
		if (err != nil) != item.failure {
			t.Fatal("request result changed", item.path)
		}
		if !item.failure && response.StatusCode != item.status {
			t.Fatal("response status changed")
		}
		outcome := "success"
		if item.kind != "" {
			outcome = "failure"
		}
		check(before, item.kind, outcome, item.status)
	}
	before := len(recorder.Ended())
	_, err = client.Do(ctx, "private-method", "/private-ok", nil, nil)
	if err == nil {
		t.Fatal("invalid method accepted")
	}
	check(before, "invalid_request", "failure", 0)
	before = len(recorder.Ended())
	_, err = client.Do(ctx, "GET", "//private-origin", nil, nil)
	if err == nil {
		t.Fatal("invalid path accepted")
	}
	check(before, "invalid_request", "failure", 0)
	before = len(recorder.Ended())
	callbackErr := privateCallbackError{}
	_, err = client.Stream(ctx, "/private-eof", original, func(context.Context, []byte) error { return callbackErr })
	if !errors.Is(err, callbackErr) {
		t.Fatal("callback error changed")
	}
	check(before, "callback", "failure", 200)
	before = len(recorder.Ended())
	frames := 0
	_, err = client.Stream(ctx, "/private-eof", original, func(context.Context, []byte) error { frames++; return nil })
	if err != nil || frames != 1 {
		t.Fatal("stream completion changed")
	}
	check(before, "", "success", 200)
	before = len(recorder.Ended())
	streamCtx, cancel := context.WithCancel(ctx)
	entered := make(chan trace.SpanContext, 1)
	done := make(chan error, 1)
	go func() {
		_, err := client.Stream(streamCtx, "/private-stream", original, func(ctx context.Context, _ []byte) error {
			entered <- trace.SpanContextFromContext(ctx)
			<-ctx.Done()
			return ctx.Err()
		})
		done <- err
	}()
	select {
	case sc := <-entered:
		if !sc.IsValid() {
			t.Fatal("callback has no span")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not start")
	}
	time.Sleep(250 * time.Millisecond)
	if len(recorder.Ended()) != before {
		t.Fatal("HTTP span ended before the stream callback")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("stream cancellation changed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not stop")
	}
	check(before, "", "canceled", 200)
	for _, exit := range []bool{false, true} {
		before = len(recorder.Ended())
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			defer func() { recover() }()
			client.Stream(ctx, "/private-eof", original, func(context.Context, []byte) error {
				if exit {
					runtime.Goexit()
				}
				panic("private-panic")
			})
		}()
		select {
		case <-stopped:
		case <-time.After(3 * time.Second):
			t.Fatal("callback abort did not return")
		}
		check(before, "aborted", "failure", 200)
	}
	mu.Lock()
	headers := append([]http.Header(nil), wire...)
	mu.Unlock()
	if len(headers) != 11 {
		t.Fatal("unexpected HTTP wire count", len(headers))
	}
	for _, h := range headers {
		if h.Get("Authorization") != "private-token" || len(h.Get("Traceparent")) != 55 || h.Get("Baggage") != "" || h.Get("Tracestate") != "" {
			t.Fatal("wire headers changed or lack trace context")
		}
	}
	if original["traceparent"][0] != "private-parent" || original["baggage"][0] != "private-baggage" {
		t.Fatal("caller headers changed")
	}
	r.Close()
	if strings.Contains(output.String(), "private-") {
		t.Fatal("private HTTP data reached local logs")
	}
}
