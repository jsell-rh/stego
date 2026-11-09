package declaration

import (
	"slices"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/parser"
	"github.com/jsell-rh/stego/internal/types"
)

// readDeclaration reads a whole declaration, and fails when reading finds a fault.
func readDeclaration(t *testing.T, data string) *types.ServiceDeclaration {
	t.Helper()
	service, undeclared, err := parser.ReadServiceDeclaration([]byte(data), "service.yaml")
	if err != nil || len(undeclared) != 0 {
		t.Fatalf("read the declaration: %v %+v", err, undeclared)
	}
	return service
}

// withTitleSensitivity is TD02 with the sensitivity given on the field title.
func withTitleSensitivity(sensitivity string) string {
	return strings.Replace(td02, "max_length: 255}", "max_length: 255, sensitivity: "+sensitivity+"}", 1)
}

// todosBinding is a binding on todos at before_create with the audit annotations given.
func todosBinding(annotations string) string {
	return "slots:\n  - collection: todos\n    slot: before_create\n    gate: [policy]\n    audit_annotations: " + annotations + "\n"
}

func TestAuditValues(t *testing.T) {
	refused := []struct {
		name, service, found string
		allowed              []string
	}{
		{"audit level verbose", td02 + "audit: {level: verbose}\n", "verbose", []string{"metadata", "change"}},
		{"an audit sink that is no port binding", td02 + "audit: {sink: audit-sink}\n", "audit-sink", nil},
		{"the sensitivity confidential on title", withTitleSensitivity("confidential"), "confidential", []string{"public", "pii", "secret"}},
		{"audit annotations given as a list", td02 + todosBinding("[over-limit]"), "[over-limit]", nil},
		{"audit annotations with a number value", td02 + todosBinding("{reason: 5}"), "5", nil},
	}
	for _, c := range refused {
		t.Run("ST-DECL-INV21-T "+c.name+" is refused, naming the value found", func(t *testing.T) {
			faults := faultsOf(readDeclaration(t, c.service))
			if len(faults) != 1 || faults[0].Found != c.found {
				t.Fatalf("want one fault that names %q, got %+v", c.found, faults)
			}
			if c.allowed != nil && !slices.Equal(faults[0].Allowed, c.allowed) {
				t.Fatalf("the fault allows %v, want %v", faults[0].Allowed, c.allowed)
			}
		})
	}
	t.Run("ST-DECL-INV21-T level metadata, a bound sink, pii on title and the annotations reason=over-limit are accepted", func(t *testing.T) {
		service := withTitleSensitivity("pii") + "overrides:\n  audit-sink: audit-sink-adapter\naudit: {level: metadata, sink: audit-sink}\n" + todosBinding("{reason: over-limit}")
		if faults := faultsOf(readDeclaration(t, service)); len(faults) != 0 {
			t.Fatalf("want no fault, got %+v", faults)
		}
	})
}

func TestAuditValueUnknown(t *testing.T) {
	bound := "overrides:\n  audit-sink: audit-sink-adapter\n"
	cases := []struct {
		name, service string
		satisfies     bool
	}{
		{"audit level verbose", td02 + "audit: {level: verbose}\n", true},
		{"an audit sink that is no port binding", td02 + "audit: {sink: audit-sink}\n", true},
		{"the sensitivity confidential", withTitleSensitivity("confidential"), true},
		{"level metadata", td02 + "audit: {level: metadata}\n", false},
		{"level change", td02 + "audit: {level: change}\n", false},
		{"a bound sink", td02 + bound + "audit: {sink: audit-sink}\n", false},
		{"the sensitivity public", withTitleSensitivity("public"), false},
		{"the sensitivity pii", withTitleSensitivity("pii"), false},
		{"the sensitivity secret", withTitleSensitivity("secret"), false},
	}
	for _, c := range cases {
		t.Run("ST-DECL-SP04-T "+c.name, func(t *testing.T) {
			if got := AuditValueUnknown(readDeclaration(t, c.service)); got != c.satisfies {
				t.Fatalf("AuditValueUnknown = %v, want %v", got, c.satisfies)
			}
		})
	}
}
