package browser

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	telemetry "example.com/browser-test/out/tracing"
)

func TestBrowserStartupDiagnostics(t *testing.T) {
	if mode := os.Getenv("STEGO_TEST_STARTUP_CHILD"); mode != "" {
		for _, name := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_CERTIFICATE", "STEGO_OTEL_TOKEN_FILE", "OTEL_EXPORTER_OTLP_HEADERS"} {
			t.Setenv(name, "")
		}
		t.Setenv("OTEL_TRACES_EXPORTER", "none")
		t.Setenv("OTEL_LOGS_EXPORTER", "none")
		t.Setenv("OTEL_METRICS_EXPORTER", "none")
		r, err := telemetry.NewRuntime()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		db, err := sql.Open("pgx", "postgres://private-user:private-password@127.0.0.1:1/private-database")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		t.Setenv("STEGO_HTTP_TLS_CERT", "")
		t.Setenv("STEGO_HTTP_TLS_KEY", "")
		t.Setenv("STEGO_BROWSER_ORIGIN", "private-invalid-origin")
		t.Setenv("STEGO_BROWSER_API_URL", "https://api.example.test")
		t.Setenv("STEGO_BROWSER_ISSUER", "https://identity.example.test")
		t.Setenv("STEGO_BROWSER_SESSION_KEY_FILE", filepath.Join(t.TempDir(), "private-missing-key"))
		if mode != "transport" {
			t.Setenv("STEGO_HTTP_TLS_CERT", "private-cert-path")
			t.Setenv("STEGO_HTTP_TLS_KEY", "private-key-path")
		}
		if mode == "keys" || mode == "schema" {
			t.Setenv("STEGO_BROWSER_ORIGIN", "https://console.example.test")
		}
		ctx := context.Background()
		if mode == "schema" {
			key := filepath.Join(t.TempDir(), "private-key")
			if err := os.WriteFile(key, []byte(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("STEGO_BROWSER_SESSION_KEY_FILE", key)
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			cancel()
		}

		if mode == "success" || mode == "discovery" {
			f := setup(t)
			db = f.db
			for name, value := range map[string]string{"STEGO_BROWSER_ORIGIN": f.options.Origin, "STEGO_BROWSER_API_URL": f.options.Upstream, "STEGO_BROWSER_API_CA_FILE": f.options.UpstreamCA, "STEGO_BROWSER_ISSUER": f.options.Issuer, "STEGO_BROWSER_ISSUER_CA_FILE": f.options.IssuerCA, "STEGO_BROWSER_CLIENT_ID": f.options.ClientID, "STEGO_BROWSER_CLIENT_SECRET_FILE": f.options.SecretFile, "STEGO_BROWSER_SESSION_KEY_FILE": f.options.KeyFile} {
				t.Setenv(name, value)
			}
			if mode == "discovery" {
				t.Setenv("STEGO_BROWSER_CLIENT_SECRET_FILE", filepath.Join(t.TempDir(), "private-missing-secret"))
			}
		}
		backend, err := NewBrowserBackendWithTelemetry(r, ctx, db)
		if mode == "success" {
			if err != nil || backend == nil {
				t.Fatal("valid startup failed")
			}
			backend.Close()
			return
		}
		if backend != nil || err == nil {
			t.Fatal("invalid startup succeeded")
		}
		if mode == "schema" && !errors.Is(err, context.Canceled) {
			t.Fatal("schema startup lost cancellation")
		}
		return
	}
	modes := []string{"transport", "configuration", "keys", "schema"}
	if os.Getenv("STEGO_TEST_POSTGRES_DSN") != "" {
		modes = append(modes, "success", "discovery")
	} else if os.Getenv("STEGO_REQUIRE_POSTGRES") == "1" {
		t.Fatal("database fixture is required")
	}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBrowserStartupDiagnostics$", "-test.count=1")
			cmd.Env = append(os.Environ(), "STEGO_TEST_STARTUP_CHILD="+mode)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("startup diagnostic child failed: %v", err)
			}
			if bytes.Contains(output, []byte("private-")) || bytes.Contains(output, []byte(secret)) {
				t.Fatal("startup diagnostics contain private input")
			}
			stages := []string{"browser.transport"}
			if mode != "transport" {
				stages = append(stages, "browser.configuration")
			}
			if mode == "keys" || mode == "schema" || mode == "success" || mode == "discovery" {
				stages = append(stages, "browser.session_keys")
			}
			if mode == "schema" || mode == "success" || mode == "discovery" {
				stages = append(stages, "browser.session_schema")
			}
			if mode == "success" || mode == "discovery" {
				stages = append(stages, "browser.upstream", "browser.discovery")
			}
			if mode == "success" {
				stages = append(stages, "browser.authorization", "browser.routes")
			}
			var records []map[string]any
			for _, line := range bytes.Split(output, []byte{'\n'}) {
				if len(line) == 0 || line[0] != '{' {
					continue
				}
				var value map[string]any
				if json.Unmarshal(line, &value) != nil {
					t.Fatal("invalid diagnostic JSON")
				}
				if value["event.name"] == "startup.step.completed" {
					records = append(records, value)
				}
			}
			if len(records) != len(stages) {
				t.Fatal("missing or repeated startup stage", len(records), len(stages))
			}
			for i, record := range records {
				outcome := "success"
				if i == len(stages)-1 && mode != "success" {
					outcome = "failure"
					if mode == "schema" {
						outcome = "canceled"
					}
				}
				if record["startup.stage"] != stages[i] || record["outcome"] != outcome {
					t.Fatal("incorrect startup stage or outcome")
				}
				for key := range record {
					switch key {
					case "startup.stage", "error.type", "timestamp", "severity", "service.name", "service.instance.id", "event.name", "message", "outcome", "duration_seconds":
					default:
						t.Fatal("unexpected diagnostic field", key)
					}
				}
			}
		})
	}
}

func TestBrowserStartupPreservesErrorsAndPanic(t *testing.T) {
	for _, cause := range []error{nil, context.Canceled, context.DeadlineExceeded, errors.New("private-provider-response")} {
		called := 0
		ctx, cancel := context.WithCancel(context.Background())
		got := startupStep(ctx, telemetry.StartupBrowserDiscovery, func(actual context.Context) error {
			called++
			if actual != ctx {
				t.Fatal("unbound startup replaced the caller context")
			}
			return cause
		})
		cancel()
		if got != cause || called != 1 {
			t.Fatal("startup changed the result or repeated work")
		}
	}
	marker := "private-panic"
	func() {
		defer func() {
			if recover() != marker {
				t.Fatal("startup changed the panic")
			}
		}()
		_ = startupStep(context.Background(), telemetry.StartupBrowserDiscovery, func(context.Context) error { panic(marker) })
	}()
	if _, err := NewBrowserBackendWithTelemetry(nil, context.Background(), nil); err == nil || strings.Contains(err.Error(), marker) {
		t.Fatal("missing runtime was accepted")
	}
}
