package declaration

import (
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/parser"
	"github.com/jsell-rh/stego/internal/types"
)

// td02Composition reads ST-DECL-TD02 with the fields given for Todo, and the text given added at the end.
func td02Composition(t *testing.T, fields, added string) *types.ServiceDeclaration {
	t.Helper()
	data := strings.Replace(td02Fields, "%s", "      - {name: title, type: string, min_length: 1, max_length: 255}\n      - {name: done, type: bool, default: false}"+fields, 1) + added
	service, undeclared, err := parser.ReadServiceDeclaration([]byte(data), "service.yaml")
	if err != nil || len(undeclared) != 0 {
		t.Fatalf("read the declaration: %v %v", err, undeclared)
	}
	return service
}

// provisionerWithout is the controller of the composition tests, with the field given left out.
func provisionerWithout(without string) string {
	fields := []string{"name: provisioner", "sources: [scan, watch]", "resync: 10m", "replicas: 2", "lease_ttl: 15s", "tenants: each"}
	var kept []string
	for _, field := range fields {
		if !strings.HasPrefix(field, without+":") {
			kept = append(kept, field)
		}
	}
	return "controllers:\n  - {" + strings.Join(kept, ", ") + "}\n"
}

// timeFields are the two time fields that bound the range of a no_overlap invariant, and the field owner_id that
// scopes it.
const timeFields = "\n      - {name: starts_at, type: timestamp}\n      - {name: ends_at, type: timestamp}\n      - {name: owner_id, type: string}"

// knownInvariants are the three declared invariants of the known kinds, each with the fields its kind needs.
var knownInvariants = map[string]string{
	"unique":     "{name: unique_open_title, kind: unique, on: Todo, by: [title], where: done = false}",
	"no_overlap": "{name: no_double_booking, kind: no_overlap, on: Todo, range: 'starts_at, ends_at', by: [owner_id]}",
	"sum_limit":  "{name: within_budget, kind: sum_limit, on: Todo, parent: budget.limit}",
}

// ruleFaults gives the faults of the rule given that checking finds in the declaration.
func ruleFaults(service *types.ServiceDeclaration, rule string) []ValidationError {
	var faults []ValidationError
	for _, fault := range Validate(service).All() {
		if fault.Rule == rule {
			faults = append(faults, fault)
		}
	}
	return faults
}

func TestModesAndMixins(t *testing.T) {
	const inv28 = "ST-DECL-INV28-T An unknown mode and kafka-events without durable-events are refused, and event-publisher is accepted with a warning"
	for _, accepted := range []string{"modes: [api, worker]\n", "", "mixins: [kafka-events, durable-events]\n"} {
		t.Run(inv28+": accepted "+strings.TrimSpace(accepted), func(t *testing.T) {
			if faults := Validate(td02Composition(t, "", accepted)).All(); len(faults) != 0 {
				t.Errorf("want no fault, got %+v", faults)
			}
		})
	}
	t.Run(inv28+": the modes api and batch", func(t *testing.T) {
		faults := ruleFaults(td02Composition(t, "", "modes: [api, batch]\n"), "mode")
		if len(faults) != 1 || faults[0].Path != "modes[1]" || faults[0].Found != "batch" || strings.Join(faults[0].Allowed, ",") != "api,worker,controller" {
			t.Errorf("want one fault that names batch and the allowed modes, got %+v", faults)
		}
	})
	t.Run(inv28+": the mixin kafka-events alone", func(t *testing.T) {
		faults := ruleFaults(td02Composition(t, "", "mixins: [kafka-events]\n"), "mixin-requirement")
		if len(faults) != 1 || !strings.Contains(faults[0].Message, "durable-events") || faults[0].Found != "kafka-events" {
			t.Errorf("want one fault that names durable-events, got %+v", faults)
		}
	})

	for _, c := range []struct {
		name, added string
		want        bool
	}{
		{"the modes api and batch", "modes: [api, batch]\n", true},
		{"the modes api, worker and controller", "modes: [api, worker, controller]\n", false},
		{"the mode worker only", "modes: [worker]\n", false},
		{"no modes", "", false},
	} {
		t.Run("ST-DECL-SP14-T A mode other than api, worker or controller satisfies it: "+c.name, func(t *testing.T) {
			if got := ModeUnknown(td02Composition(t, "", c.added)); got != c.want {
				t.Errorf("want %v, got %v", c.want, got)
			}
		})
	}
	for _, c := range []struct {
		name, added string
		want        bool
	}{
		{"kafka-events alone", "mixins: [kafka-events]\n", true},
		{"kafka-events and durable-events", "mixins: [kafka-events, durable-events]\n", false},
		{"durable-events alone", "mixins: [durable-events]\n", false},
		{"no mixin", "", false},
	} {
		t.Run("ST-DECL-SP15-T kafka-events without durable-events satisfies it: "+c.name, func(t *testing.T) {
			if got := MixinRequirementUnmet(td02Composition(t, "", c.added)); got != c.want {
				t.Errorf("want %v, got %v", c.want, got)
			}
		})
	}
}

func TestControllerLease(t *testing.T) {
	const inv29 = "ST-DECL-INV29-T A controller with no lease TTL, sources, resync or replicas, or an unknown source, is refused"
	t.Run(inv29+": the controller provisioner is accepted", func(t *testing.T) {
		if faults := Validate(td02Composition(t, "", provisionerWithout(""))).All(); len(faults) != 0 {
			t.Errorf("want no fault, got %+v", faults)
		}
	})
	for _, c := range []struct{ without, path string }{
		{"lease_ttl", "controllers[0].lease_ttl"}, {"sources", "controllers[0].sources"}, {"resync", "controllers[0].resync"}, {"replicas", "controllers[0].replicas"},
	} {
		t.Run(inv29+": no "+c.without, func(t *testing.T) {
			faults := ruleFaults(td02Composition(t, "", provisionerWithout(c.without)), "controller")
			if len(faults) != 1 || faults[0].Path != c.path || !strings.Contains(faults[0].Message, `"provisioner"`) {
				t.Errorf("want one fault at %s that names provisioner, got %+v", c.path, faults)
			}
		})
	}
	t.Run(inv29+": the source poll", func(t *testing.T) {
		faults := ruleFaults(td02Composition(t, "", strings.Replace(provisionerWithout(""), "[scan, watch]", "[scan, poll]", 1)), "controller")
		if len(faults) != 1 || faults[0].Path != "controllers[0].sources[1]" || faults[0].Found != "poll" {
			t.Errorf("want one fault that names the source poll, got %+v", faults)
		}
	})
	t.Run(inv29+": a lease TTL that is not a duration", func(t *testing.T) {
		faults := ruleFaults(td02Composition(t, "", strings.Replace(provisionerWithout(""), "lease_ttl: 15s", "lease_ttl: forever", 1)), "controller")
		if len(faults) != 1 || faults[0].Found != "forever" {
			t.Errorf("want one fault that names forever, got %+v", faults)
		}
	})

	for _, c := range []struct {
		name, added string
		want        bool
	}{
		{"no lease TTL", provisionerWithout("lease_ttl"), true},
		{"the source poll", strings.Replace(provisionerWithout(""), "[scan, watch]", "[poll]", 1), true},
		{"tenants none", strings.Replace(provisionerWithout(""), "tenants: each", "tenants: none", 1), true},
		{"sources scan and watch, a lease TTL and tenants each", provisionerWithout(""), false},
	} {
		t.Run("ST-DECL-SP17-T A controller with no lease TTL, a source other than scan or watch, or tenants other than each satisfies it: "+c.name, func(t *testing.T) {
			if got := ControllerWithoutLease(td02Composition(t, "", c.added)); got != c.want {
				t.Errorf("want %v, got %v", c.want, got)
			}
		})
	}
}

func TestDeclaredInvariants(t *testing.T) {
	const inv30 = "ST-DECL-INV30-T A declared invariant of an unknown kind, or without the fields its kind needs, is refused"
	for kind, invariant := range knownInvariants {
		t.Run(inv30+": "+kind+" with its fields is accepted", func(t *testing.T) {
			if faults := Validate(td02Composition(t, timeFields, "invariants:\n  - "+invariant+"\n")).All(); len(faults) != 0 {
				t.Errorf("want no fault, got %+v", faults)
			}
		})
	}
	for _, c := range []struct{ name, invariant, path, names string }{
		{"the kind exclusive", "{name: one_open, kind: exclusive, on: Todo}", "invariants[0].kind", "exclusive"},
		{"unique with no by", "{name: unique_title, kind: unique, on: Todo}", "invariants[0].by", "needs by"},
		{"no_overlap with no range", "{name: no_double_booking, kind: no_overlap, on: Todo, by: [owner_id]}", "invariants[0].range", "needs range"},
		{"sum_limit with no parent", "{name: within_budget, kind: sum_limit, on: Todo}", "invariants[0].parent", "needs parent"},
	} {
		t.Run(inv30+": "+c.name, func(t *testing.T) {
			faults := ruleFaults(td02Composition(t, timeFields, "invariants:\n  - "+c.invariant+"\n"), "declared-invariant")
			if len(faults) != 1 || faults[0].Path != c.path || !strings.Contains(faults[0].Message, c.names) || !strings.Contains(faults[0].Message, `"`+strings.Split(strings.TrimPrefix(c.invariant, "{name: "), ",")[0]+`"`) {
				t.Errorf("want one fault at %s that names the invariant and %s, got %+v", c.path, c.names, faults)
			}
			if c.path == "invariants[0].kind" && strings.Join(faults[0].Allowed, ",") != "unique,no_overlap,sum_limit" {
				t.Errorf("want unique, no_overlap and sum_limit allowed, got %v", faults[0].Allowed)
			}
		})
	}
	t.Run(inv30+": the rule exclusive written as a binding on todos at before_create is accepted", func(t *testing.T) {
		binding := "slots:\n  - collection: todos\n    slot: before_create\n    gate: [one-open]\n"
		if faults := Validate(td02Composition(t, "", binding)).All(); len(faults) != 0 {
			t.Errorf("want no fault, got %+v", faults)
		}
	})
	t.Run(inv30+": a where that is not field = value, or names no field of the entity, is refused", func(t *testing.T) {
		for where, found := range map[string]string{
			"done = false; DROP TABLE todos": "",
			"done = false or 1 = 1":          "",
			"done = false or title = 'x'":    "",
			"title = 'x''y'":                 "",
			"closed = true":                  "closed",
		} {
			invariant := "{name: unique_open_title, kind: unique, on: Todo, by: [title], where: \"" + where + "\"}"
			faults := ruleFaults(td02Composition(t, "", "invariants:\n  - "+invariant+"\n"), "declared-invariant")
			if len(faults) != 1 || faults[0].Path != "invariants[0].where" || !strings.Contains(faults[0].Message, found) {
				t.Errorf("where %s: want one fault at the where, got %+v", where, faults)
			}
		}
	})

	for _, c := range []struct {
		name, invariant string
		want            bool
	}{
		{"the kind exclusive", "{name: one_open, kind: exclusive, on: Todo}", true},
		{"unique with no by", "{name: unique_title, kind: unique, on: Todo}", true},
		{"no_overlap with no range", "{name: no_double_booking, kind: no_overlap, on: Todo, by: [owner_id]}", true},
		{"sum_limit with no parent", "{name: within_budget, kind: sum_limit, on: Todo}", true},
		{"unique with on and by", knownInvariants["unique"], false},
		{"no_overlap with on, range and by", knownInvariants["no_overlap"], false},
		{"sum_limit with on and parent", knownInvariants["sum_limit"], false},
	} {
		t.Run("ST-DECL-SP16-T A declared invariant of another kind, or without the fields its kind needs, satisfies it: "+c.name, func(t *testing.T) {
			if got := InvariantWithoutEmission(td02Composition(t, timeFields, "invariants:\n  - "+c.invariant+"\n")); got != c.want {
				t.Errorf("want %v, got %v", c.want, got)
			}
		})
	}
}
