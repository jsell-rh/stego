package probe

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"go.opentelemetry.io/otel/baggage"
)

type requestPathKey struct{}
type headerKey struct{}

func TestProbeValues(t *testing.T) {
	t.Run("ST-SUP-RP12-LC09-T01 A probe sends no private value and cuts long text", func(t *testing.T) {
		clearEnvironment(t)
		refused := reservationRefused(t, CountsAttempt)
		// 334 euro signs are 1,002 bytes, so a cut at 256 bytes falls inside a rune.
		sku := strings.Repeat("€", 334)
		member, err := baggage.NewMember("session", "baggage-secret")
		if err != nil {
			t.Fatal(err)
		}
		bag, err := baggage.New(member)
		if err != nil {
			t.Fatal(err)
		}
		ctx := baggage.ContextWithBaggage(unsampled(), bag)
		ctx = context.WithValue(ctx, requestPathKey{}, "/v1/reservations/private-path")
		ctx = context.WithValue(ctx, headerKey{}, "Authorization: Bearer private-token")

		recorder := NewRecorder(nil)
		stdout := &lines{}
		r := newProbe(t, newTelemetry(t), "json", stdout)
		for _, call := range []*Call{recorder.Bind(ctx, "t1", "user:u-7", command), r.Bind(ctx, "t1", "user:u-7", command)} {
			call.Probe().Emit(refused, Text(sku), Enum("short"))
			call.Probe().Emit(refused, Text("bad\xff\x00text"), Enum("short"))
			call.Returned()
			call.Rollback()
		}
		if err := r.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		emissions := recorder.Emissions("reservation_refused")
		if len(emissions) != 2 {
			t.Fatalf("the recorder holds %d emissions, want 2", len(emissions))
		}
		got := emissions[0].Fields[0].Value
		if len(got) > TextLimit || !utf8.ValidString(got) || !strings.HasPrefix(sku, got) || len(got) < TextLimit-3 {
			t.Fatalf("the sku is %d bytes, valid %v, want at most %d bytes of valid UTF-8 cut from the value", len(got), utf8.ValidString(got), TextLimit)
		}
		if got := emissions[1].Fields[0].Value; got != "bad��text" {
			t.Fatalf("the cleaned sku is %q, want the invalid byte and the NUL replaced", got)
		}
		text := strings.Join(stdout.all(), "")
		for _, e := range emissions {
			text += " " + e.String() + " " + e.Tenant + " " + e.Principal + " " + e.Operation
		}
		for _, private := range []string{"private-path", "private-token", "Authorization", "baggage-secret", "session", "goroutine", ".go:"} {
			if strings.Contains(text, private) {
				t.Fatalf("a record holds %q: %s", private, text)
			}
		}
		for _, line := range stdout.all() {
			if !strings.Contains(line, `"sku":"`) || strings.Contains(line, strings.Repeat("€", 86)) {
				t.Fatalf("the console line %q does not hold the cut sku", line)
			}
		}
	})
}
