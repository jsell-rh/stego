package probe

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"stego.dev/runtime/otel"
)

// emitCalls emits n events in calls of at most HoldLimit emissions, each a fill that returns, then commits.
func emitCalls(r *Runtime, event *Observation, n int) {
	for n > 0 {
		call := r.Bind(context.Background(), "t1", "user:u-7", command)
		for i := 0; i < min(n, HoldLimit); i++ {
			call.Probe().Emit(event, Text("A-1"), Enum("short"))
		}
		n -= min(n, HoldLimit)
		call.Returned()
		call.Commit()
	}
}

func TestProbeSinks(t *testing.T) {
	t.Run("ST-SUP-RP12-LC06-T01 OTLP is on only with a collector endpoint", func(t *testing.T) {
		c := startCollector(t)
		endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
		certificate := os.Getenv("OTEL_EXPORTER_OTLP_CERTIFICATE")
		event := reservationRefused(t, CountsAttempt)
		for _, withEndpoint := range []bool{false, true} {
			if withEndpoint {
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
				t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", certificate)
			} else {
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
				t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", "")
			}
			r := newProbe(t, newTelemetry(t), "", nil)
			if (r.otlp != nil) != withEndpoint {
				t.Fatalf("with an endpoint %v the probe has an OTLP sink %v", withEndpoint, r.otlp != nil)
			}
			emitCalls(r, event, 1)
			if err := r.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := 0
			if withEndpoint {
				want = 1
			}
			if got := len(c.logRecords()); got != want {
				t.Fatalf("with an endpoint %v the collector holds %d log records, want %d", withEndpoint, got, want)
			}
		}
	})

	t.Run("ST-SUP-RP12-LC08-T01 A blocked sink does not block or fail the emission", func(t *testing.T) {
		clearEnvironment(t)
		event := reservationRefused(t, CountsAttempt)
		// The fill's response does not depend on its probe. The test record asks for three times the queue size
		// from a fill; one call holds at most HoldLimit emissions (ST-SUP-RP12-LC20), so the fills are 48 calls.
		run := func(r *Runtime) []string {
			var responses []string
			for i := 0; i < 3*QueueLimit/HoldLimit; i++ {
				call := r.Bind(context.Background(), "t1", "user:u-7", command)
				fill := func(p *Probe) string {
					for range HoldLimit {
						p.Emit(event, Text("A-1"), Enum("short"))
					}
					return "reserved"
				}
				responses = append(responses, fill(call.Probe()))
				call.Returned()
				call.Commit()
			}
			return responses
		}
		off := newProbe(t, newTelemetry(t), "", nil)
		want := run(off)
		stdout := &lines{}
		r := newProbe(t, newTelemetry(t), "json", stdout)
		release := stdout.hold()
		defer release()
		emitCalls(r, event, 1)
		stdout.waitEntered(t)
		done := make(chan []string)
		go func() { done <- run(r) }()
		select {
		case got := <-done:
			if !slices.Equal(got, want) {
				t.Fatal("the responses differ with the console sink held")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the fills did not return while the console writer was held")
		}
		if d := r.Delivery(); d.ConsoleDropped == 0 || d.ProbeDropped != 0 {
			t.Fatalf("the delivery counts are %+v, want console drops and no probe drop", d)
		}
	})

	t.Run("ST-SUP-RP12-LC12-T01 Close drains the console queue inside the shared shutdown budget", func(t *testing.T) {
		clearEnvironment(t)
		event := reservationRefused(t, CountsAttempt)
		stdout := &lines{}
		r := newProbe(t, newTelemetry(t), "json", stdout)
		release := stdout.hold()
		emitCalls(r, event, 500)
		stdout.waitEntered(t)
		time.AfterFunc(100*time.Millisecond, release)
		if err := r.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := len(stdout.all()); got != 500 {
			t.Fatalf("Close returned after %d lines, want all 500", got)
		}

		c := startCollector(t)
		c.hold.Store(true)
		stdout = &lines{}
		r = newProbe(t, newTelemetry(t), "json", stdout)
		release = stdout.hold()
		defer release()
		emitCalls(r, event, 10)
		stdout.waitEntered(t)
		begin := time.Now()
		err := r.Close(context.Background())
		elapsed := time.Since(begin)
		if err == nil {
			t.Fatal("Close reported a full drain with standard output and the collector held")
		}
		if elapsed > otel.ShutdownTimeout+time.Second {
			t.Fatalf("Close took %v, more than the one shared limit of %v", elapsed, otel.ShutdownTimeout)
		}
		before := r.Delivery().ProbeDropped
		call := r.Bind(context.Background(), "t1", "user:u-7", command)
		call.Probe().Emit(event, Text("A-1"), Enum("short"))
		call.Commit()
		if got := r.Delivery().ProbeDropped; got != before+1 {
			t.Fatalf("the drop count after Close is %d, want %d", got, before+1)
		}
	})

	t.Run("ST-SUP-RP12-LC16-T01 Domain observations reach the console only when the environment turns it on", func(t *testing.T) {
		event := reservationRefused(t, CountsAttempt)
		cases := []struct {
			name, setting string
			collector     bool
			want          string
		}{
			{"no settings", "", false, ""},
			{"json", "json", false, `{"timestamp":"`},
			{"logfmt", "logfmt", false, "timestamp="},
			{"off", "off", false, ""},
			{"collector", "", true, ""},
		}
		for _, tc := range cases {
			clearEnvironment(t)
			if tc.collector {
				startCollector(t)
			}
			if tc.setting != "" {
				t.Setenv(consoleSetting, tc.setting)
			}
			stdout := &lines{}
			r, err := start(newTelemetry(t), nil, os.Getenv(consoleSetting), stdout)
			if err != nil {
				t.Fatal(err)
			}
			emitCalls(r, event, 1)
			r.Close(context.Background())
			got := stdout.all()
			switch {
			case tc.want == "" && len(got) != 0:
				t.Fatalf("%s: the console holds %q, want no line", tc.name, got)
			case tc.want != "" && (len(got) != 1 || !strings.HasPrefix(got[0], tc.want) || !strings.Contains(got[0], "domain.reservation_refused")):
				t.Fatalf("%s: the console holds %q, want one line that starts with %q", tc.name, got, tc.want)
			}
		}
		clearEnvironment(t)
		t.Setenv(consoleSetting, "yaml")
		_, err := start(newTelemetry(t), nil, os.Getenv(consoleSetting), &lines{})
		if err == nil || strings.Contains(err.Error(), "yaml") || err.Error() != "probe: STEGO_PROBE_CONSOLE must be json, logfmt or off" {
			t.Fatalf("the start with yaml returned %v, want the fixed error text", err)
		}
	})

	t.Run("ST-SUP-RP12-LC19-T01 Drops and write failures are counted by sink", func(t *testing.T) {
		c := startCollector(t)
		event := reservationRefused(t, CountsAttempt)
		stdout := &lines{}
		r := newProbe(t, newTelemetry(t), "json", stdout)
		release := stdout.hold()
		emitCalls(r, event, 1)
		stdout.waitEntered(t)
		emitCalls(r, event, 3*QueueLimit)
		stdout.fails.Store(1)
		release()
		waitFor(t, func() bool { return r.Delivery().ConsoleFailed == 1 })
		if err := r.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		metrics := c.lastMetrics()
		for name, want := range map[string]int64{"domain.probe.dropped": 2 * QueueLimit, "domain.probe.failed": 1} {
			m := metrics[name]
			if m == nil || m.GetSum() == nil {
				t.Fatalf("no counter %s", name)
			}
			var found bool
			for _, p := range m.GetSum().DataPoints {
				if got := attributeKeys(p.Attributes); !slices.Equal(got, []string{"sink"}) {
					t.Fatalf("%s has the labels %v, want sink only", name, got)
				}
				if slices.Contains(attributes(p.Attributes), "sink=console") {
					found = true
					if p.GetAsInt() != want {
						t.Fatalf("%s with sink=console is %d, want %d", name, p.GetAsInt(), want)
					}
				}
			}
			if !found {
				t.Fatalf("%s has no series for sink=console", name)
			}
		}
	})

	t.Run("ST-SUP-RP12-LC19-T01 Two calls on one server span each keep their span events on their own call span", func(t *testing.T) {
		c := startCollector(t)
		event := reservationRefused(t, CountsAttempt)
		r := newProbe(t, newTelemetry(t), "", nil)
		// A conflict reruns the command on the same server span: each attempt is a call of its own.
		ctx, span := recordingSpan(t, r.telemetry)
		for _, commit := range []bool{false, true} {
			call := r.Bind(ctx, "t1", "user:u-7", command)
			for range HoldLimit {
				call.Probe().Emit(event, Text("A-1"), Enum("short"))
			}
			call.Returned()
			if commit {
				call.Commit()
			} else {
				call.Rollback()
			}
		}
		span.End()
		if err := r.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if d := r.Delivery(); d.OTLPDropped != 0 {
			t.Fatalf("the delivery counts are %+v, want no otlp drop: each call has its own span", d)
		}
		var calls int
		for _, s := range c.spans() {
			if s.Name == CallSpanName {
				calls++
				if len(s.Events) != otel.SpanEventLimit {
					t.Fatalf("a call span holds %d events, want %d", len(s.Events), otel.SpanEventLimit)
				}
			} else if len(s.Events) != 0 {
				t.Fatal("an event went on the server span")
			}
		}
		if calls != 2 || len(c.logRecords()) != 2*HoldLimit {
			t.Fatalf("%d call spans and %d log records, want 2 and %d", calls, len(c.logRecords()), 2*HoldLimit)
		}
		m := c.lastMetrics()["domain.probe.dropped"]
		var found bool
		for _, p := range m.GetSum().DataPoints {
			if slices.Contains(attributes(p.Attributes), "sink=otlp") {
				found = p.GetAsInt() == 0
			}
		}
		if !found {
			t.Fatal("domain.probe.dropped has no series sink=otlp of 0")
		}
	})

	t.Run("ST-SUP-RP12-LC20-T01 A refusal that rolls back is sent, and committed work only after a commit", func(t *testing.T) {
		refusedEvent := reservationRefused(t, CountsAttempt)
		refusedCounter := reservationsRefused(t, CountsAttempt)
		made := reservationsMade(t)
		for _, commit := range []bool{false, true} {
			recorder := NewRecorder(nil)
			call := recorder.Bind(context.Background(), "t1", "user:u-7", command)
			call.Probe().Emit(refusedEvent, Text("A-1"), Enum("short"))
			call.Probe().Emit(refusedCounter, Enum("short"))
			call.Probe().Emit(made)
			if len(recorder.All()) != 0 {
				t.Fatal("the recorder holds an emission before the call ended")
			}
			call.Returned()
			if commit {
				call.Commit()
			} else {
				call.Rollback()
			}
			var got []string
			for _, e := range recorder.All() {
				got = append(got, e.String())
			}
			want := []string{"reservation_refused{sku=A-1 reason=short stego.outcome=rolled_back}", "reservations_refused{reason=short stego.outcome=rolled_back}"}
			if commit {
				want = []string{"reservation_refused{sku=A-1 reason=short stego.outcome=committed}", "reservations_refused{reason=short stego.outcome=committed}", "reservations_made{}"}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("with a commit %v the recorder holds %v, want %v", commit, got, want)
			}
		}
		recorder := NewRecorder(nil)
		call := recorder.Bind(context.Background(), "t1", "user:u-7", command)
		for range HoldLimit + 1 {
			call.Probe().Emit(refusedEvent, Text("A-1"), Enum("short"))
		}
		call.Commit()
		if len(recorder.All()) != HoldLimit || recorder.Dropped() != 1 {
			t.Fatalf("the last call sent %d records with %d drops, want %d and 1", len(recorder.All()), recorder.Dropped(), HoldLimit)
		}
	})
}
