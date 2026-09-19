package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"example.com/sso-test/tracing"
)

func TestSSOStartupTelemetry(t *testing.T) {
	ssoFixture(t)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "OTEL_") || name == "STEGO_OTEL_TOKEN_FILE" {
			t.Setenv(name, "")
		}
	}
	output, err := os.CreateTemp(t.TempDir(), "signals-")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	previous := os.Stderr
	os.Stderr = output
	defer func() { os.Stderr = previous }()
	runtime, err := tracing.NewRuntime()
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	ctx := context.Background()
	if h, err := NewJWTHandlerWithTelemetry(nil, ctx); err == nil {
		h.Stop()
		t.Fatal("startup accepted a missing runtime")
	}
	if h, err := NewJWTHandlerWithTelemetry(runtime, nil); err == nil {
		h.Stop()
		t.Fatal("startup accepted a missing context")
	}
	h, err := NewJWTHandlerWithTelemetry(runtime, ctx)
	if err != nil {
		t.Fatal(err)
	}
	h.Stop()
	h.Stop()
	// The handler must not close the process-owned runtime.
	_, finish := tracing.TraceAuthKeyRefresh(runtime.Context(ctx), "https")
	finish("success")
	file := os.Getenv("JWK_CERT_FILE")
	document, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("private-provider-response"), 0600); err != nil {
		t.Fatal(err)
	}
	if h, err := NewJWTHandlerWithTelemetry(runtime, ctx); err == nil {
		h.Stop()
		t.Fatal("startup accepted an invalid key document")
	}
	if err := os.WriteFile(file, document, 0600); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if h, err := NewJWTHandlerWithTelemetry(runtime, canceled); err == nil {
		h.Stop()
		t.Fatal("startup ignored cancellation")
	}
	expired, done := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer done()
	if h, err := NewJWTHandlerWithTelemetry(runtime, expired); err == nil {
		h.Stop()
		t.Fatal("startup ignored its deadline")
	}
	runtime.Close()
	if _, err := output.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(output)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private-provider-response", file, "issuer.example/realm"} {
		if bytes.Contains(data, []byte(private)) {
			t.Fatal("startup telemetry contains private data")
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	for i, outcome := range []string{"success", "success", "failure", "canceled", "deadline"} {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatal("startup telemetry is missing", err)
		}
		source := "file"
		if i == 1 {
			source = "https"
		}
		if record["event.name"] != "auth.keys.refresh.completed" || record["outcome"] != outcome || record["operation"] != source {
			t.Fatal("wrong startup telemetry", record)
		}
	}
	var extra map[string]any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatal("unexpected startup telemetry", err)
	}
}
