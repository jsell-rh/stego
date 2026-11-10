package domainprobe

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

// catalogOf generates ctx and returns observations.json.
func catalogOf(t *testing.T, ctx gen.Context) []byte {
	t.Helper()
	files, _, err := new(Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Path == "probes/observations.json" {
			return f.Bytes()
		}
	}
	t.Fatal("the generator emitted no probes/observations.json")
	return nil
}

func TestCatalog(t *testing.T) {
	t.Run("ST-SUP-RC34-OC07-T01 The observation catalog lists each observation and stays the same", func(t *testing.T) {
		first, second := catalogOf(t, reservationContext()), catalogOf(t, reservationContext())
		if !bytes.Equal(first, second) {
			t.Fatalf("two generations of one declaration give different catalogs:\n%s\n%s", first, second)
		}
		var catalog struct {
			Observations []map[string]json.RawMessage `json:"observations"`
		}
		if err := json.Unmarshal(first, &catalog); err != nil {
			t.Fatal(err)
		}
		want := []struct{ name, signal, kind, severity, fields, value, emitters string }{
			{"reservation_refused", "domain.reservation_refused", "event", "warn",
				`[{"name":"sku","type":"entity","entity":"Reservation","field":"sku"},{"name":"reason","type":"enum","values":["short","expired"]}]`,
				`null`, `["reserve-and-charge"]`},
			{"reservation_wait", "domain.reservation_wait", "histogram", "info", `[]`, `{"name":"wait","unit":"ms"}`, `["reservations.before_create"]`},
			{"reservations_refused", "domain.reservations_refused", "counter", "warn",
				`[{"name":"reason","type":"enum","values":["short","expired"]}]`, `null`, `["reserve-and-charge"]`},
		}
		if len(catalog.Observations) != len(want) {
			t.Fatalf("the catalog has %d entries, want %d:\n%s", len(catalog.Observations), len(want), first)
		}
		compact := func(raw json.RawMessage) string {
			var b bytes.Buffer
			if err := json.Compact(&b, raw); err != nil {
				t.Fatal(err)
			}
			return b.String()
		}
		for i, w := range want {
			e := catalog.Observations[i]
			got := []string{compact(e["name"]), compact(e["signal"]), compact(e["kind"]), compact(e["severity"]),
				compact(e["fields"]), compact(e["value"]), compact(e["emitted_by"])}
			exp := []string{`"` + w.name + `"`, `"` + w.signal + `"`, `"` + w.kind + `"`, `"` + w.severity + `"`, w.fields, w.value, w.emitters}
			for j := range got {
				if got[j] != exp[j] {
					t.Errorf("entry %d: got %s, want %s", i, got[j], exp[j])
				}
			}
			if _, ok := e["outcome_measured"]; !ok {
				t.Errorf("entry %d has no outcome_measured", i)
			}
			if string(e["counts"]) != `"attempt"` {
				t.Errorf("entry %d counts %s, want attempt", i, e["counts"])
			}
		}
	})
}
