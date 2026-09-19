package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"example.com/auth-test/tracing"
	"github.com/golang-jwt/jwt/v5"
)

func TestKeySourceTelemetry(t *testing.T) {
	for _, source := range []string{"file", "https"} {
		t.Run(source, func(t *testing.T) {
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
			ctx := runtime.Context(context.Background())
			key := keyForTest(t)
			document := keyDocument(t, "private-key-id", key)
			config := JWKSConfig{Trust: Config{Issuer: "https://issuer.example", Audience: "example-api"}}
			var setDocument func([]byte)
			if source == "file" {
				config.File = filepath.Join(t.TempDir(), "private-key-path.json")
				setDocument = func(data []byte) {
					if err := os.WriteFile(config.File, data, 0600); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				var content atomic.Value
				content.Store(document)
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(content.Load().([]byte)) }))
				defer server.Close()
				config.URL = server.URL + "/private-key-path"
				config.CAFile = filepath.Join(t.TempDir(), "private-ca.pem")
				if err := os.WriteFile(config.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
					t.Fatal(err)
				}
				setDocument = func(data []byte) { content.Store(data) }
			}
			setDocument(document)
			verifier, err := NewJWKSVerifier(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			defer verifier.Stop()
			now := time.Now()
			verifier.now = func() time.Time { return now }
			token := signed(t, validClaims(), jwt.SigningMethodRS256, key, map[string]any{"kid": "private-key-id"})
			unknown := signed(t, validClaims(), jwt.SigningMethodRS256, key, map[string]any{"kid": "private-unknown-key"})
			if _, err := verifier.Verify(ctx, token); err != nil {
				t.Fatal(err)
			}
			if _, err := verifier.Verify(ctx, unknown); err == nil {
				t.Fatal("unknown key accepted")
			}
			now = now.Add(6 * time.Minute)
			setDocument([]byte("private-provider-response"))
			if _, err := verifier.Verify(ctx, token); err != nil {
				t.Fatal("known key lost its bounded outage window", err)
			}
			if _, err := verifier.Verify(ctx, token); err != nil {
				t.Fatal(err)
			}
			setDocument(document)
			now = now.Add(31 * time.Second)
			if _, err := verifier.Verify(ctx, token); err != nil {
				t.Fatal(err)
			}
			verifier.Stop()
			if _, err := verifier.Verify(ctx, token); err == nil {
				t.Fatal("stopped verifier accepted token")
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			if value, err := NewJWKSVerifier(canceled, config); err == nil {
				value.Stop()
				t.Fatal("canceled startup accepted")
			}
			expired, done := context.WithDeadline(ctx, time.Now().Add(-time.Second))
			defer done()
			if value, err := NewJWKSVerifier(expired, config); err == nil {
				value.Stop()
				t.Fatal("expired startup accepted")
			}
			runtime.Close()
			if _, err := output.Seek(0, 0); err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(output)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(data, []byte("private-")) || bytes.Contains(data, []byte(token)) || bytes.Contains(data, []byte("issuer.example")) {
				t.Fatal("key telemetry contains private data")
			}
			decoder := json.NewDecoder(bytes.NewReader(data))
			for _, outcome := range []string{"success", "failure", "success", "canceled", "deadline"} {
				var record map[string]any
				if err := decoder.Decode(&record); err != nil {
					t.Fatal("missing refresh completion", err)
				}
				if record["event.name"] != "auth.keys.refresh.completed" || record["operation"] != source || record["outcome"] != outcome {
					t.Fatal("wrong refresh completion", record)
				}
			}
			var extra map[string]any
			if err := decoder.Decode(&extra); err != io.EOF {
				t.Fatal("cache hit, cooldown, or stopped verifier emitted extra refresh work", err)
			}
		})
	}
}
