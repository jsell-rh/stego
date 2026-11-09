package probe

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"stego.dev/runtime/lease"
	"stego.dev/runtime/otel"
)

// The schema backfill observations are what a backfill of the lease runtime takes.
var _ lease.Observer = Backfill{}

func TestProbeBuiltins(t *testing.T) {
	t.Run("ST-SUP-RP12-LC21-T01 The service events are built-in observations on the probe's sinks", func(t *testing.T) {
		c := startCollector(t)
		stdout := &lines{}
		r := newProbe(t, newTelemetry(t), "", stdout)
		r.Service(context.Background(), ServiceReady)
		r.Service(context.Background(), ServiceFailed)
		if err := r.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"service.ready": "INFO", "service.failed": "ERROR"}
		got := stdout.all()
		if len(got) != 2 {
			t.Fatalf("the console holds %q, want two lines", got)
		}
		for _, line := range got {
			var decoded map[string]any
			if err := json.Unmarshal([]byte(line), &decoded); err != nil {
				t.Fatal(err)
			}
			name, _ := decoded["event.name"].(string)
			if want[name] == "" || decoded["severity"] != want[name] || decoded["fields"] != nil {
				t.Fatalf("the console line is %q", line)
			}
		}
		records := c.logRecords()
		if len(records) != 2 {
			t.Fatalf("the collector holds %d log records, want 2", len(records))
		}
		for _, record := range records {
			if want[record.EventName] != record.SeverityText || len(record.Attributes) != 0 {
				t.Fatalf("the log record is %s %s with %v", record.EventName, record.SeverityText, attributes(record.Attributes))
			}
		}
		_, err := Declare(Spec{Name: "service.ready", Kind: Event, Severity: Info, EmittedBy: []string{command}})
		if err == nil || !strings.Contains(err.Error(), "service.ready") || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("the declaration of service.ready returned %v, want a refusal by name", err)
		}
		// The worker observations carry the worker name and the fixed outcome only.
		builtins, err := NewBuiltins([]string{"sync-partner-orders"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		recorder := NewRecorder(builtins)
		worker := recorder.Worker("sync-partner-orders")
		worker.RunFailed(context.Background(), WorkerFenced)
		worker.TickSkipped()
		worker.TenantUnreached()
		recorder.Worker("undeclared").TickSkipped()
		var kept []string
		for _, e := range recorder.All() {
			kept = append(kept, e.Name+" "+e.Severity.String()+" "+e.String())
		}
		if !slices.Equal(kept, []string{
			"worker.run.failed ERROR worker.run.failed{worker=sync-partner-orders outcome=fenced}",
			"worker.tick.skipped WARN worker.tick.skipped{worker=sync-partner-orders}",
			"worker.tenant.unreached WARN worker.tenant.unreached{worker=sync-partner-orders}",
		}) || recorder.Dropped() != 1 {
			t.Fatalf("the recorder holds %v with %d drops", kept, recorder.Dropped())
		}
	})

	t.Run("ST-SUP-RP12-LC22-T01 Built-in service events are on the console in every setting", func(t *testing.T) {
		if mode := os.Getenv(childSetting); mode != "" && strings.HasPrefix(mode, "service") {
			var c *collector
			if mode == "service-collector" {
				c = startCollector(t)
			}
			telemetry, err := otel.New("orders")
			if err != nil {
				os.Exit(2)
			}
			r, err := Start(telemetry, nil)
			if err != nil {
				os.Exit(3)
			}
			r.Service(unsampled(), ServiceReady)
			r.Close(context.Background())
			if c != nil {
				count := 0
				for _, record := range c.logRecords() {
					if record.EventName == "service.ready" {
						count++
					}
				}
				os.WriteFile(os.Getenv("STEGO_PROBE_TEST_RESULT"), []byte(strconv.Itoa(count)), 0o600)
			}
			os.Exit(0)
		}
		result := filepath.Join(t.TempDir(), "result")
		sc := unsampledSpanContext()
		var form []string
		for _, settings := range [][]string{
			{childSetting + "=service"},
			{childSetting + "=service", consoleSetting + "=off"},
			{childSetting + "=service-collector", "STEGO_PROBE_TEST_RESULT=" + result},
			{childSetting + "=service", consoleSetting + "=logfmt"},
		} {
			stdout, _ := runChild(t, "^TestProbeBuiltins$/^ST-SUP-RP12-LC22-T01_", settings...)
			lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
			if len(lines) != 1 {
				t.Fatalf("%v: the child wrote %q, want one line", settings, stdout)
			}
			var decoded map[string]any
			if err := json.Unmarshal([]byte(lines[0]), &decoded); err != nil {
				t.Fatalf("%v: the line %q is not JSON: %v", settings, lines[0], err)
			}
			var keys []string
			for k := range decoded {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			if form == nil {
				form = keys
			}
			if !slices.Equal(keys, form) || !slices.Equal(keys, []string{"event.name", "service.instance.id", "service.name", "severity", "span_id", "timestamp", "trace_id"}) {
				t.Fatalf("%v: the line has the keys %v, want the fixed form %v", settings, keys, form)
			}
			if decoded["event.name"] != "service.ready" || decoded["trace_id"] != sc.TraceID().String() {
				t.Fatalf("%v: the line is %q", settings, lines[0])
			}
		}
		if got, err := os.ReadFile(result); err != nil || string(got) != "1" {
			t.Fatalf("the collector got %q log records service.ready (%v), want 1", got, err)
		}
	})

	t.Run("ST-SUP-RP12-LC23-T01 The schema backfill observations are built in and their names are reserved", func(t *testing.T) {
		builtins, err := NewBuiltins(nil, []SchemaChange{{ID: "user-email-rename", Open: true}, {ID: "orders-retype", Open: false}})
		if err != nil {
			t.Fatal(err)
		}
		if got := builtins.Changes(); !slices.Equal(got, []string{"user-email-rename"}) {
			t.Fatalf("the enum of the field change is %v, want the open change alone", got)
		}
		recorder := NewRecorder(builtins)
		var observer lease.Observer = recorder.Backfill()
		observer.BackfillRows("user-email-rename", 200)
		observer.BackfillFailedRows("user-email-rename", 2)
		observer.BackfillDuplicateGroups("user-email-rename", 1)
		observer.BackfillCompleted("user-email-rename")
		observer.BackfillStalled("user-email-rename")
		observer.BackfillRows("orders-retype", 5)
		var kept []string
		for _, e := range recorder.All() {
			kept = append(kept, e.Kind.String()+" "+e.Severity.String()+" "+e.String())
		}
		want := []string{
			"counter INFO schema.backfill.rows{change=user-email-rename value=200}",
			"counter WARN schema.backfill.failed_rows{change=user-email-rename value=2}",
			"counter WARN schema.backfill.duplicate_groups{change=user-email-rename value=1}",
			"event INFO schema.backfill.completed{change=user-email-rename}",
			"event ERROR schema.backfill.stalled{change=user-email-rename}",
		}
		if !slices.Equal(kept, want) || recorder.Dropped() != 1 {
			t.Fatalf("the recorder holds %v with %d drops, want %v and the finished change dropped", kept, recorder.Dropped(), want)
		}
		for _, name := range []string{"schema.backfill.rows", "schema.backfill.stalled"} {
			if _, err := Declare(Spec{Name: name, Kind: Counter, Severity: Info, EmittedBy: []string{command}}); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("the declaration of %s returned %v, want a refusal by name", name, err)
			}
		}
	})
}
