package declaration

import (
	"slices"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/parser"
	"github.com/jsell-rh/stego/internal/types"
)

// td02 is the accepted rest-crud declaration of ST-DECL-TD02, which says nothing of tenancy or audit.
const td02 = `kind: service
name: todo-svc
archetype: rest-crud
language: go
base_path: /api/todo/v1
entities:
  - name: Todo
    fields:
      - {name: title, type: string, min_length: 1, max_length: 255}
      - {name: done, type: bool, default: false}
collections:
  todos:
    entity: Todo
    operations: [create, read, update, delete, list]
`

// readTD02 reads ST-DECL-TD02 with the text added at its end, and fails when reading finds a fault.
func readTD02(t *testing.T, added string) *types.ServiceDeclaration {
	t.Helper()
	service, undeclared, err := parser.ReadServiceDeclaration([]byte(td02+added), "service.yaml")
	if err != nil || len(undeclared) != 0 {
		t.Fatalf("read the declaration: %v %+v", err, undeclared)
	}
	return service
}

// faultsOf checks the declaration and gives every fault it has.
func faultsOf(service *types.ServiceDeclaration) []ValidationError {
	return Validate(service).All()
}

func TestTenancyAndAuditDefaults(t *testing.T) {
	t.Run("ST-DECL-INV18-T TD02 is accepted with pooled tenancy and audit at level change", func(t *testing.T) {
		service := readTD02(t, "")
		if faults := faultsOf(service); len(faults) != 0 {
			t.Fatalf("TD02 is refused: %+v", faults)
		}
		want := map[string]any{"tenancy.kind": "pooled", "tenancy.key": "tenant_id", "tenancy.claim": "tenant_id", "audit.enabled": true, "audit.level": "change"}
		for path, value := range want {
			i := slices.IndexFunc(ResolvedSettings(service), func(s Setting) bool { return s.Path == path })
			if i < 0 || ResolvedSettings(service)[i].Value != value || !ResolvedSettings(service)[i].Default {
				t.Errorf("want %s %v marked as a default, got %+v", path, value, ResolvedSettings(service))
			}
		}
	})
	refused := []struct{ name, added, path string }{
		{"tenancy kind single with no reason", "tenancy: {kind: single}\n", "tenancy.reason"},
		{"audit off with no reason", "audit: {enabled: false}\n", "audit.reason"},
	}
	for _, c := range refused {
		t.Run("ST-DECL-INV18-T "+c.name+" is refused, naming the setting and that a reason is required", func(t *testing.T) {
			faults := faultsOf(readTD02(t, c.added))
			if len(faults) != 1 || faults[0].Path != c.path || !strings.Contains(faults[0].Message, "must give a reason") {
				t.Fatalf("want one fault at %s that a reason is required, got %+v", c.path, faults)
			}
		})
	}
	accepted := []struct{ name, added string }{
		{"tenancy kind single with a reason", "tenancy: {kind: single, reason: Internal tool for one organisation}\n"},
		{"audit off with a reason", "audit: {enabled: false, reason: Stateless proxy with no data of its own}\n"},
	}
	for _, c := range accepted {
		t.Run("ST-DECL-INV18-T "+c.name+" is accepted", func(t *testing.T) {
			if faults := faultsOf(readTD02(t, c.added)); len(faults) != 0 {
				t.Fatalf("want no fault, got %+v", faults)
			}
		})
	}
}

func TestDefaultLeftWithoutReason(t *testing.T) {
	cases := []struct {
		name, added string
		satisfies   bool
	}{
		{"tenancy kind single and no reason", "tenancy: {kind: single}\n", true},
		{"audit off and no reason", "audit: {enabled: false}\n", true},
		{"tenancy kind single and a reason", "tenancy: {kind: single, reason: one customer per deployment}\n", false},
		{"audit off and a reason", "audit: {enabled: false, reason: no personal data}\n", false},
		{"tenancy pooled and audit on and no reason", "tenancy: {kind: pooled, key: org_id, claim: org.id}\naudit: {enabled: true, level: change}\n", false},
		{"TD02 unchanged", "", false},
	}
	for _, c := range cases {
		t.Run("ST-DECL-SP01-T "+c.name, func(t *testing.T) {
			if got := DefaultLeftWithoutReason(readTD02(t, c.added)); got != c.satisfies {
				t.Fatalf("DefaultLeftWithoutReason = %v, want %v", got, c.satisfies)
			}
		})
	}
}
