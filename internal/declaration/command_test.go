package declaration

import (
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/parser"
)

// compositionFaults reads a declaration as stego validate reads it and gives the faults of its composition with
// the facts given, keeping only those of the rules named.
func compositionFaults(t *testing.T, service string, facts Facts, rules ...string) []ValidationError {
	t.Helper()
	declared, undeclared, err := parser.ReadServiceDeclaration([]byte(service), "service.yaml")
	if err != nil || len(undeclared) != 0 {
		t.Fatalf("read the declaration: %v %+v\n%s", err, undeclared, service)
	}
	if own := Validate(declared, undeclared...).All(); len(own) != 0 {
		t.Fatalf("the declaration has other faults: %+v", own)
	}
	keep := map[string]bool{}
	for _, rule := range rules {
		keep[rule] = true
	}
	var faults []ValidationError
	for _, fault := range ValidateComposition(declared, facts) {
		if len(keep) == 0 || keep[fault.Rule] {
			faults = append(faults, fault)
		}
	}
	return faults
}

// commandFacts are the facts of ST-DECL-TD02 with a fill of the command slot for each name given, and the outbox
// destination billing that an active component declares.
func commandFacts(fills ...string) Facts {
	facts := Facts{FillSlots: map[string]string{}, Destinations: map[string]bool{"billing": true}}
	for _, fill := range fills {
		facts.FillSlots[fill] = CommandSlot
	}
	return facts
}

// command writes one command of ST-DECL-TD02 with the lines given after its name.
func command(name string, lines ...string) string {
	return "  - name: " + name + "\n    " + strings.Join(lines, "\n    ") + "\n"
}

// standardCommand is a command that writes Todo, bound to the fill reserve-and-charge.
func standardCommand(name string, lines ...string) string {
	return command(name, append([]string{"writes: [Todo]", "fill: reserve-and-charge", "route: POST /orders/{orderId}/" + name}, lines...)...)
}

// wantFault checks that faults holds exactly one fault and that its message names each part.
func wantFault(t *testing.T, faults []ValidationError, parts ...string) {
	t.Helper()
	if len(faults) != 1 {
		t.Fatalf("want one fault, got %d: %+v", len(faults), faults)
	}
	for _, part := range parts {
		if !strings.Contains(faults[0].Message, part) {
			t.Errorf("the fault does not name %s: %s", part, faults[0].Message)
		}
	}
}

func wantNoFault(t *testing.T, faults []ValidationError) {
	t.Helper()
	if len(faults) != 0 {
		t.Fatalf("want no fault, got %+v", faults)
	}
}

func TestCommandNames(t *testing.T) {
	facts := commandFacts("reserve-and-charge")
	t.Run("ST-DECL-INV37-T Command names that are used twice are refused", func(t *testing.T) {
		service := td02 + "commands:\n" + standardCommand("reserve-and-charge") + command("reserve-and-charge", "writes: [Todo]", "fill: reserve-and-charge", "route: POST /orders/{orderId}/release")
		wantFault(t, compositionFaults(t, service, facts, "command-name"), `"reserve-and-charge"`)
	})
	t.Run("ST-DECL-INV37-T Command names that differ are accepted", func(t *testing.T) {
		service := td02 + "commands:\n" + standardCommand("reserve-and-charge") + standardCommand("release")
		wantNoFault(t, compositionFaults(t, service, facts))
	})
}

func TestCommandWrites(t *testing.T) {
	facts := commandFacts("reserve-and-charge")
	with := func(writes string) string {
		return td02 + "commands:\n" + command("reserve-and-charge", "writes: "+writes, "fill: reserve-and-charge", "route: POST /reservations")
	}
	t.Run("ST-DECL-INV38-T A command that writes Todo is accepted", func(t *testing.T) {
		wantNoFault(t, compositionFaults(t, with("[Todo]"), facts))
	})
	t.Run("ST-DECL-INV38-T A command that writes no entity is refused", func(t *testing.T) {
		wantFault(t, compositionFaults(t, with("[]"), facts, "command-writes"), `"reserve-and-charge"`)
	})
	t.Run("ST-DECL-INV38-T A command that writes an undeclared entity is refused", func(t *testing.T) {
		faults := compositionFaults(t, with("[Gadget]"), facts, "command-writes")
		wantFault(t, faults, `"reserve-and-charge"`, `"Gadget"`)
		if faults[0].Found != "Gadget" || faults[0].Path != "commands[0].writes[0]" {
			t.Errorf("the fault does not point at Gadget: %+v", faults[0])
		}
	})
}

func TestCommandFill(t *testing.T) {
	facts := commandFacts("reserve-and-charge", "release")
	facts.FillSlots["policy"] = "rest-api.before_create"
	with := func(fill string) string {
		return td02 + "commands:\n" + command("reserve-and-charge", "writes: [Todo]", fill, "route: POST /reservations")
	}
	t.Run("ST-DECL-INV39-T A command with a fill of the command slot is accepted", func(t *testing.T) {
		wantNoFault(t, compositionFaults(t, with("fill: reserve-and-charge"), facts))
	})
	for _, c := range []struct{ name, fill, names string }{
		{"no fill", "fill: []", "no fill"},
		{"two fills", "fill: [reserve-and-charge, release]", `"release"`},
		{"a fill of before_create", "fill: policy", `"policy"`},
		{"a fill the project does not hold", "fill: missing", `"missing"`},
	} {
		t.Run("ST-DECL-INV39-T A command with "+c.name+" is refused", func(t *testing.T) {
			wantFault(t, compositionFaults(t, with(c.fill), facts, "command-fill"), `"reserve-and-charge"`, c.names)
		})
	}
}

func TestCommandEffects(t *testing.T) {
	facts := commandFacts("reserve-and-charge")
	with := func(effects ...string) string {
		return td02 + "commands:\n" + standardCommand("reserve-and-charge", "effects:\n      - "+strings.Join(effects, "\n      - "))
	}
	t.Run("ST-DECL-INV42-T A command effect for a declared destination is accepted", func(t *testing.T) {
		wantNoFault(t, compositionFaults(t, with("{destination: billing, name: charge-requested}"), facts))
	})
	for _, c := range []struct {
		name    string
		effects []string
		names   string
	}{
		{"an unknown destination", []string{"{destination: nowhere, name: charge-requested}"}, `"nowhere"`},
		{"a repeated name", []string{"{destination: billing, name: charge-requested}", "{destination: billing, name: charge-requested}"}, `"charge-requested"`},
		{"the destination authz", []string{"{destination: authz, name: holder}"}, "only through the authorization handle"},
		{"the destination audit", []string{"{destination: audit, name: entry}"}, "only through the audit runtime"},
	} {
		t.Run("ST-DECL-INV42-T A command effect with "+c.name+" is refused", func(t *testing.T) {
			wantFault(t, compositionFaults(t, with(c.effects...), facts, "command-effect"), `"reserve-and-charge"`, c.names)
		})
	}
}

func TestOperationRefusals(t *testing.T) {
	facts := commandFacts("reserve-and-charge")
	with := func(refusals ...string) string {
		return td02 + "commands:\n" + standardCommand("reserve-and-charge", "refusals:\n      - "+strings.Join(refusals, "\n      - "))
	}
	shortStock := "{name: short_stock, status: 409, code: reservation.short_stock}"
	t.Run("ST-DECL-INV56-T A command refusal with a known status and code is accepted", func(t *testing.T) {
		wantNoFault(t, compositionFaults(t, with(shortStock), facts))
	})
	for _, c := range []struct {
		name     string
		refusals []string
		names    string
	}{
		{"a repeated name", []string{shortStock, "{name: short_stock, status: 409, code: reservation.other}"}, `"short_stock"`},
		{"a repeated code", []string{shortStock, "{name: other, status: 409, code: reservation.short_stock}"}, "reservation.short_stock"},
		{"the status 500", []string{"{name: short_stock, status: 500, code: reservation.short_stock}"}, "500"},
		{"the status 418", []string{"{name: short_stock, status: 418, code: reservation.short_stock}"}, "418"},
		{"the code internal", []string{"{name: short_stock, status: 409, code: internal}"}, `"internal"`},
		{"the code conflict", []string{"{name: short_stock, status: 409, code: conflict}"}, `"conflict"`},
		{"the name ShortStock", []string{"{name: ShortStock, status: 409, code: reservation.short_stock}"}, `"ShortStock"`},
	} {
		t.Run("ST-DECL-INV56-T A command refusal with "+c.name+" is refused", func(t *testing.T) {
			wantFault(t, compositionFaults(t, with(c.refusals...), facts, "operation-refusal"), `"reserve-and-charge"`, c.names)
		})
	}
}

func TestCommandRetry(t *testing.T) {
	facts := commandFacts("reserve-and-charge")
	with := func(lines ...string) string {
		return td02 + "commands:\n" + standardCommand("reserve-and-charge", lines...)
	}
	t.Run("ST-DECL-INV80-T A command with the values left out gets 0 and 2s, marked as defaults", func(t *testing.T) {
		declared := readDeclaration(t, with())
		got := map[string]Setting{}
		for _, setting := range ResolvedSettings(declared) {
			got[setting.Path] = setting
		}
		if s := got["commands[0].retry_on_conflict"]; s.Value != 0 || !s.Default {
			t.Errorf("retry on conflict is %+v, want the default 0", s)
		}
		if s := got["commands[0].lock_timeout"]; s.Value != "2s" || !s.Default {
			t.Errorf("lock timeout is %+v, want the default 2s", s)
		}
		declared = readDeclaration(t, with("retry_on_conflict: 2", "lock_timeout: 500ms"))
		for _, setting := range ResolvedSettings(declared) {
			got[setting.Path] = setting
		}
		if s := got["commands[0].retry_on_conflict"]; s.Value != 2 || s.Default {
			t.Errorf("retry on conflict is %+v, want the stated 2", s)
		}
		if s := got["commands[0].lock_timeout"]; s.Value != "500ms" || s.Default {
			t.Errorf("lock timeout is %+v, want the stated 500ms", s)
		}
	})
	for _, accepted := range [][]string{nil, {"retry_on_conflict: 0"}, {"retry_on_conflict: 3"}, {"lock_timeout: 100ms"}, {"lock_timeout: 10s"}} {
		t.Run("ST-DECL-INV80-T A command with "+strings.Join(append([]string{"the values left out"}, accepted...), ", ")+" is accepted", func(t *testing.T) {
			wantNoFault(t, compositionFaults(t, with(accepted...), facts))
		})
	}
	for _, c := range []struct{ line, value string }{
		{"retry_on_conflict: 4", "4"}, {"retry_on_conflict: -1", "-1"}, {"retry_on_conflict: 1.5", "1.5"},
		{"lock_timeout: 50ms", "50ms"}, {"lock_timeout: 11s", "11s"}, {"lock_timeout: soon", "soon"},
	} {
		t.Run("ST-DECL-INV80-T A command with "+c.line+" is refused", func(t *testing.T) {
			faults := compositionFaults(t, with(c.line), facts)
			wantFault(t, faults, `"reserve-and-charge"`, `"`+c.value+`"`)
			if faults[0].Found != c.value {
				t.Errorf("the fault found %q, want %q", faults[0].Found, c.value)
			}
		})
	}
}

func TestQueryRefusals(t *testing.T) {
	facts := queryFacts()
	with := func(refusal string) string {
		return td02 + "commands:\n" + standardCommand("reserve-and-charge", "refusals:\n      - {name: short_stock, status: 409, code: reservation.short_stock}") +
			"queries:\n" + standardQuery("open-reservations", "refusals:\n      - "+refusal)
	}
	t.Run("ST-DECL-INV56-T A command and a query with known refusals are accepted", func(t *testing.T) {
		wantNoFault(t, compositionFaults(t, with("{name: order_unknown, status: 404, code: order.unknown}"), facts))
	})
	t.Run("ST-DECL-INV56-T A query refusal with the code conflict is refused", func(t *testing.T) {
		wantFault(t, compositionFaults(t, with("{name: order_unknown, status: 404, code: conflict}"), facts, "operation-refusal"), `"open-reservations"`, `"conflict"`)
	})
}
