package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"stego.dev/runtime/otel"
)

// childSetting runs a test again as a child process, which writes to its real standard output.
const childSetting = "STEGO_PROBE_TEST_CHILD"

// runChild runs the subtest pattern of the test binary again as a child process with the settings, and returns its
// standard output and standard error.
func runChild(t *testing.T, pattern string, settings ...string) (string, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run="+pattern, "-test.count=1")
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "OTEL_") || strings.HasPrefix(name, "STEGO_") {
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	cmd.Env = append(cmd.Env, settings...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("the child process failed: %v\n%s\n%s", err, stdout.String(), stderr.String())
	}
	return stdout.String(), stderr.String()
}

// socketCount counts the open sockets of this process.
func socketCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("the process has no /proc/self/fd to count sockets")
	}
	count := 0
	for _, entry := range entries {
		if target, err := os.Readlink("/proc/self/fd/" + entry.Name()); err == nil && strings.HasPrefix(target, "socket:") {
			count++
		}
	}
	return count
}

func TestProbeConsole(t *testing.T) {
	t.Run("ST-SUP-RP12-LC07-T01 The console sink writes one JSON or logfmt line per emission to standard output", func(t *testing.T) {
		if os.Getenv(childSetting) == "console" {
			telemetry, err := otel.New("orders")
			if err != nil {
				os.Exit(2)
			}
			r, err := Start(telemetry, nil)
			if err != nil {
				os.Exit(3)
			}
			call := r.Bind(unsampled(), "t1", "user:u-7", command)
			call.Probe().Emit(reservationRefused(t, CountsAttempt), Text("A-1"), Enum("short"))
			call.Probe().Emit(reservationsRefused(t, CountsAttempt), Enum("expired"))
			call.Probe().Emit(histogram(t, "reservation_wait", Milliseconds), Number(12.5))
			call.Returned()
			call.Commit()
			r.Close(context.Background())
			os.Exit(0)
		}
		sc := unsampledSpanContext()
		common := []string{"timestamp", "severity", "service.name", "service.instance.id", "event.name", "stego.outcome", "trace_id", "span_id", "tenant", "principal", "operation"}
		for _, setting := range []string{"json", "logfmt"} {
			stdout, stderr := runChild(t, "^TestProbeConsole$/^ST-SUP-RP12-LC07-T01_", childSetting+"=console", consoleSetting+"="+setting)
			if stderr != "" {
				t.Fatalf("%s: the child wrote %q to standard error", setting, stderr)
			}
			lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
			if len(lines) != 4 || !strings.Contains(lines[3], `"event.name":"telemetry.runtime.stopped"`) {
				t.Fatalf("%s: the child wrote %d lines, want 3 and the line of telemetry.runtime.stopped: %q", setting, len(lines), stdout)
			}
			lines = lines[:3]
			for i, line := range lines {
				values := map[string]string{}
				if setting == "json" {
					var decoded map[string]any
					if err := json.Unmarshal([]byte(line), &decoded); err != nil {
						t.Fatalf("the line %q is not one JSON object: %v", line, err)
					}
					for k, v := range decoded {
						if fields, ok := v.(map[string]any); ok {
							for name, value := range fields {
								values["fields."+name] = strings.Trim(strings.TrimSpace(jsonText(value)), `"`)
							}
							continue
						}
						values[k] = strings.Trim(jsonText(v), `"`)
					}
				} else {
					for _, pair := range strings.Fields(line) {
						k, v, ok := strings.Cut(pair, "=")
						if !ok {
							t.Fatalf("the logfmt line %q has the pair %q", line, pair)
						}
						values[k] = strings.Trim(v, `"`)
					}
				}
				for _, key := range common {
					if values[key] == "" {
						t.Fatalf("%s: the line %q has no %s", setting, line, key)
					}
				}
				if values["service.name"] != "orders" || values["tenant"] != "t1" || values["principal"] != "user:u-7" || values["operation"] != command || values["trace_id"] != sc.TraceID().String() || values["span_id"] != sc.SpanID().String() {
					t.Fatalf("%s: the line %q has the wrong correlation", setting, line)
				}
				switch i {
				case 0:
					if values["event.name"] != "domain.reservation_refused" || values["severity"] != "WARN" || values["fields.sku"] != "A-1" || values["fields.reason"] != "short" {
						t.Fatalf("%s: the event line is %q", setting, line)
					}
				case 1:
					if values["event.name"] != "domain.reservations_refused" || values["fields.reason"] != "expired" {
						t.Fatalf("%s: the counter line is %q", setting, line)
					}
				case 2:
					if values["event.name"] != "domain.reservation_wait" || values["value"] != "12.5" {
						t.Fatalf("%s: the histogram line is %q", setting, line)
					}
				}
			}
		}
	})

	t.Run("ST-SUP-RP12-LC18-T01 The console sink starts no provider and opens no connection", func(t *testing.T) {
		clearEnvironment(t)
		before := socketCount(t)
		telemetry := newTelemetry(t)
		stdout := &lines{}
		r, err := start(telemetry, nil, "json", stdout)
		if err != nil {
			t.Fatal(err)
		}
		emitCalls(r, reservationRefused(t, CountsAttempt), 10)
		if err := r.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(stdout.all()) != 10 {
			t.Fatalf("the console holds %d lines, want 10", len(stdout.all()))
		}
		if telemetry.TracerProvider() != nil || telemetry.MeterProvider() != nil || telemetry.LoggerProvider() != nil || telemetry.CollectorSet() || r.otlp != nil {
			t.Fatal("a provider or the OTLP sink was started with no collector")
		}
		if after := socketCount(t); after != before {
			t.Fatalf("the process has %d sockets, %d before the probe", after, before)
		}
	})
}

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func unsampledSpanContext() trace.SpanContext { return trace.SpanContextFromContext(unsampled()) }
