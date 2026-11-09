package declaration

import (
	"fmt"
	"strings"
	"testing"
)

// reservations is ST-DECL-TD02 with the entity Reservation (fields id uuid, sku string, qty int, holder_email string
// with sensitivity pii, card_token string with sensitivity secret) and the command reserve-and-charge that writes
// it. The declaration format gives each entity its id, so the entity does not list it, and qty is an int32.
const reservations = `kind: service
name: todo-svc
archetype: rest-crud
language: go
base_path: /api/todo/v1
entities:
  - name: Todo
    fields:
      - {name: title, type: string, min_length: 1, max_length: 255}
      - {name: done, type: bool, default: false}
  - name: Reservation
    fields:
      - {name: sku, type: string}
      - {name: qty, type: int32}
      - {name: holder_email, type: string, sensitivity: pii}
      - {name: card_token, type: string, sensitivity: secret}
collections:
  todos:
    entity: Todo
    operations: [create, read, update, delete, list]
  reservations:
    entity: Reservation
    operations: [create, read]
slots:
  - collection: reservations
    slot: before_create
    gate: [policy]
commands:
  - name: reserve-and-charge
    writes: [Reservation]
    fill: reserve-and-charge
    route: POST /orders/{orderId}/reservations
`

// observations gives the declaration of reservations with the observations given, each as the lines of one item.
func observations(items ...string) string {
	return reservations + "observations:\n" + strings.Join(items, "")
}

// observation writes one observation with the lines given.
func observation(lines ...string) string {
	return "  - " + strings.Join(lines, "\n    ") + "\n"
}

// refused is the event reservation_refused, emitted by reserve-and-charge, with the lines given.
func refused(lines ...string) string {
	return observation(append([]string{"name: reservation_refused", "kind: event", "severity: warn", "emitted_by: [reserve-and-charge]"}, lines...)...)
}

func observationFaults(t *testing.T, service string, rules ...string) []ValidationError {
	t.Helper()
	return compositionFaults(t, service, commandFacts("reserve-and-charge"), rules...)
}

func TestObservationNames(t *testing.T) {
	t.Run("ST-DECL-INV48-T An observation reservation_refused is accepted", func(t *testing.T) {
		wantNoFault(t, observationFaults(t, observations(refused())))
	})
	t.Run("ST-DECL-INV48-T Two observations named reservation_refused are refused", func(t *testing.T) {
		wantFault(t, observationFaults(t, observations(refused(), refused()), "observation-name"), `"reservation_refused"`)
	})
	for _, name := range []string{"ReservationRefused", "9_refused", "reservation.refused", strings.Repeat("r", 64)} {
		t.Run("ST-DECL-INV48-T The observation name "+name+" is refused", func(t *testing.T) {
			service := observations(observation("name: "+name, "kind: event", "severity: warn", "emitted_by: [reserve-and-charge]"))
			wantFault(t, observationFaults(t, service, "observation-name"), `"`+name+`"`)
		})
	}
}

func TestObservationKind(t *testing.T) {
	with := func(kind, severity string) string {
		lines := []string{"name: reservation_refused", kind, "emitted_by: [reserve-and-charge]"}
		if severity != "" {
			lines = append(lines, severity)
		}
		return observations(observation(lines...))
	}
	t.Run("ST-DECL-INV49-T An event of severity warn is accepted", func(t *testing.T) {
		wantNoFault(t, observationFaults(t, with("kind: event", "severity: warn")))
	})
	for _, c := range []struct{ name, kind, severity, names string }{
		{"the kind gauge", "kind: gauge", "severity: warn", `"gauge"`},
		{"the severity fatal", "kind: event", "severity: fatal", `"fatal"`},
		{"no severity", "kind: event", "", "severity"},
	} {
		t.Run("ST-DECL-INV49-T An observation with "+c.name+" is refused", func(t *testing.T) {
			wantFault(t, observationFaults(t, with(c.kind, c.severity), "observation-kind", "observation-severity"), `"reservation_refused"`, c.names)
		})
	}
}

func TestObservationValue(t *testing.T) {
	wait := func(value string) string {
		lines := []string{"name: reservation_wait", "kind: histogram", "severity: info", "emitted_by: [reserve-and-charge]", "counts: committed"}
		if value != "" {
			lines = append(lines, value)
		}
		return observations(observation(lines...))
	}
	t.Run("ST-DECL-INV50-T A histogram with the value wait: ms is accepted", func(t *testing.T) {
		wantNoFault(t, observationFaults(t, wait(`value: "wait: ms"`)))
	})
	for _, c := range []struct{ name, service, names string }{
		{"a histogram with no value", wait(""), `"reservation_wait"`},
		{"a histogram with the unit minutes", wait(`value: "wait: minutes"`), "wait: minutes"},
		{"a histogram with two values", wait(`value: "wait: ms, size: By"`), "exactly one value"},
		{"an event with the value wait: ms", observations(refused(`value: "wait: ms"`)), `"reservation_refused"`},
	} {
		t.Run("ST-DECL-INV50-T "+c.name+" is refused", func(t *testing.T) {
			wantFault(t, observationFaults(t, c.service, "observation-value"), c.names)
		})
	}
}

// fields writes the fields of an observation as YAML items.
func fields(items ...string) string {
	var lines []string
	for _, item := range items {
		name, typ, _ := strings.Cut(item, ": ")
		lines = append(lines, fmt.Sprintf("  - {name: %s, type: %q}", name, typ))
	}
	return "fields:\n    " + strings.Join(lines, "\n    ")
}

func TestObservationFields(t *testing.T) {
	good := []string{"sku: Reservation.sku", "reason: enum(short | expired)", "qty: measure(1, 1..1000)", "retried: bool"}
	t.Run("ST-DECL-INV51-T An event with an entity field, an enum, a measure and a bool is accepted", func(t *testing.T) {
		wantNoFault(t, observationFaults(t, observations(refused(fields(good...)))))
	})
	var many []string
	for i := 0; i < 33; i++ {
		many = append(many, fmt.Sprintf("v%d", i))
	}
	for _, c := range []struct {
		name   string
		fields []string
		names  string
	}{
		{"a field note: string", []string{"note: string"}, `"note"`},
		{"qty: int", []string{"qty: int"}, `"qty"`},
		{"qty: measure(1)", []string{"qty: measure(1)"}, `"qty"`},
		{"qty: measure(minutes, 1..10)", []string{"qty: measure(minutes, 1..10)"}, `"qty"`},
		{"qty: measure(1, 10..1)", []string{"qty: measure(1, 10..1)"}, `"qty"`},
		{"sku: Reservation.colour", []string{"sku: Reservation.colour"}, `"sku"`},
		{"sku: Gadget.sku", []string{"sku: Gadget.sku"}, `"sku"`},
		{"a field named tenant", []string{"tenant: bool"}, `"tenant"`},
		{"two fields named reason", []string{"reason: bool", "reason: enum(short | expired)"}, `"reason"`},
		{"nine fields", []string{"a: bool", "b: bool", "c: bool", "d: bool", "e: bool", "f: bool", "g: bool", "h: bool", "i: bool"}, "9 fields"},
		{"reason: enum()", []string{"reason: enum()"}, `"reason"`},
		{"an enum of 33 values", []string{"reason: enum(" + strings.Join(many, " | ") + ")"}, `"reason"`},
	} {
		t.Run("ST-DECL-INV51-T An observation with "+c.name+" is refused", func(t *testing.T) {
			wantFault(t, observationFaults(t, observations(refused(fields(c.fields...))), "observation-field"), `"reservation_refused"`, c.names)
		})
	}
}

func TestObservationPrivacy(t *testing.T) {
	t.Run("ST-DECL-INV52-T An event with the field sku: Reservation.sku is accepted", func(t *testing.T) {
		wantNoFault(t, observationFaults(t, observations(refused(fields("sku: Reservation.sku")))))
	})
	for _, c := range []struct{ field, entityField string }{
		{"email: Reservation.holder_email", "Reservation.holder_email"},
		{"token: Reservation.card_token", "Reservation.card_token"},
	} {
		t.Run("ST-DECL-INV52-T An event with the field "+c.field+" is refused", func(t *testing.T) {
			name, _, _ := strings.Cut(c.field, ":")
			wantFault(t, observationFaults(t, observations(refused(fields(c.field))), "observation-private-field"), `"reservation_refused"`, `"`+name+`"`, c.entityField)
		})
	}
	t.Run("ST-DECL-INV52-T The event is accepted again when the private field is removed", func(t *testing.T) {
		wantNoFault(t, observationFaults(t, observations(refused())))
	})
}

func TestObservationSeries(t *testing.T) {
	enum := func(name string, n int) string {
		values := make([]string, n)
		for i := range values {
			values[i] = fmt.Sprintf("v%d", i)
		}
		return name + ": enum(" + strings.Join(values, " | ") + ")"
	}
	counter := func(counts string, items ...string) string {
		lines := []string{"name: reservations_refused", "kind: counter", "severity: warn", "emitted_by: [reserve-and-charge]"}
		if counts != "" {
			lines = append(lines, "counts: "+counts)
		}
		return observations(observation(append(lines, fields(items...))...))
	}
	for _, c := range []struct{ name, service string }{
		{"the field reason: enum(short | expired)", counter("", "reason: enum(short | expired)")},
		{"Counts committed and 126 combinations", counter("committed", enum("a", 7), enum("b", 9), "c: bool")},
	} {
		t.Run("ST-DECL-INV53-T A counter with "+c.name+" is accepted", func(t *testing.T) {
			wantNoFault(t, observationFaults(t, c.service))
		})
	}
	for _, c := range []struct{ name, service, names string }{
		{"sku: Reservation.sku", counter("", "sku: Reservation.sku"), `"sku"`},
		{"qty: measure(1, 1..1000)", counter("", "qty: measure(1, 1..1000)"), `"qty"`},
		{"Counts attempt and 252 series", counter("attempt", enum("a", 7), enum("b", 9), "c: bool"), "252"},
		{"Counts committed and 128 series", counter("committed", enum("a", 8), enum("b", 8), "c: bool"), "128"},
	} {
		t.Run("ST-DECL-INV53-T A counter with "+c.name+" is refused", func(t *testing.T) {
			wantFault(t, observationFaults(t, c.service, "observation-series"), `"reservations_refused"`, c.names)
		})
	}
}

func TestObservationEmitters(t *testing.T) {
	with := func(emitters string) string {
		return observations(observation("name: reservation_refused", "kind: event", "severity: warn", "emitted_by: "+emitters))
	}
	for _, emitters := range []string{"[reserve-and-charge]", "[reservations.before_create]"} {
		t.Run("ST-DECL-INV54-T An event emitted by "+emitters+" is accepted", func(t *testing.T) {
			wantNoFault(t, observationFaults(t, with(emitters)))
		})
	}
	for _, c := range []struct{ name, emitters, names string }{
		{"nothing", "[]", "no emitter"},
		{"release-stock, which the declaration does not declare", "[release-stock]", `"release-stock"`},
		{"reserve-and-charge twice", "[reserve-and-charge, reserve-and-charge]", `"reserve-and-charge"`},
	} {
		t.Run("ST-DECL-INV54-T An event emitted by "+c.name+" is refused", func(t *testing.T) {
			wantFault(t, observationFaults(t, with(c.emitters), "observation-emitter"), `"reservation_refused"`, c.names)
		})
	}
}

func TestObservationCounts(t *testing.T) {
	t.Run("ST-DECL-INV55-T An event with no Counts is accepted, and counts each attempt", func(t *testing.T) {
		service := observations(refused())
		wantNoFault(t, observationFaults(t, service))
		declared := readDeclaration(t, service)
		if counts := ObservationCounts(declared.Observations[0]); counts != "attempt" {
			t.Errorf("the event counts %q, want attempt", counts)
		}
		found := false
		for _, setting := range ResolvedSettings(declared) {
			if setting.Path == "observations[0].counts" {
				found = true
				if setting.Value != "attempt" || !setting.Default {
					t.Errorf("checking reports the counts %+v, want the default attempt", setting)
				}
			}
		}
		if !found {
			t.Error("checking reports no counts of the event")
		}
	})
	for _, c := range []struct{ name, service string }{
		{"an event with Counts attempt and the outcome measured CCS-BO02", observations(refused("counts: attempt", "outcome_measured: CCS-BO02"))},
		{"a counter with Counts committed and the outcome measured CCS-BO01", observations(observation("name: reservations_made", "kind: counter", "severity: info", "emitted_by: [reserve-and-charge]", "counts: committed", "outcome_measured: CCS-BO01"))},
	} {
		t.Run("ST-DECL-INV55-T "+c.name+" is accepted", func(t *testing.T) {
			wantNoFault(t, observationFaults(t, c.service))
		})
	}
	for _, c := range []struct{ name, service string }{
		{"an event with Counts always", observations(refused("counts: always"))},
		{"an event with the outcome measured CCS-BO02 and no Counts", observations(refused("outcome_measured: CCS-BO02"))},
	} {
		t.Run("ST-DECL-INV55-T "+c.name+" is refused", func(t *testing.T) {
			wantFault(t, observationFaults(t, c.service, "observation-counts"), `"reservation_refused"`)
		})
	}
}
