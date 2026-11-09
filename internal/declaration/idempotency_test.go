package declaration

import (
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/parser"
	"github.com/jsell-rh/stego/internal/types"
)

// td02Idempotency is ST-DECL-TD02 with patch added to todos and idempotency required on the operations given.
func td02Idempotency(t *testing.T, requiredOn string) *types.ServiceDeclaration {
	t.Helper()
	data := strings.Replace(td02Fields, "%s", "      - {name: title, type: string, min_length: 1, max_length: 255}\n      - {name: done, type: bool, default: false}", 1)
	data = strings.Replace(data, "operations: [create, read, update, delete, list]", "operations: [create, read, update, delete, list, patch]\n    patchable: [title, done]", 1)
	data += "idempotency:\n  required_on: [" + requiredOn + "]\n"
	service, undeclared, err := parser.ReadServiceDeclaration([]byte(data), "service.yaml")
	if err != nil || len(undeclared) != 0 {
		t.Fatalf("read the declaration: %v %v", err, undeclared)
	}
	return service
}

// idempotencyFaults gives the faults of the idempotency rule that checking finds in the declaration.
func idempotencyFaults(service *types.ServiceDeclaration) []ValidationError {
	var faults []ValidationError
	for _, fault := range Validate(service).All() {
		if fault.Rule == "idempotency-required-on" {
			faults = append(faults, fault)
		}
	}
	return faults
}

func TestIdempotencyRequiredOn(t *testing.T) {
	t.Run("ST-DECL-INV25-T Idempotency required on read, list, update or delete, or not on create, is refused", func(t *testing.T) {
		for _, accepted := range []string{"create", "create, patch"} {
			if faults := Validate(td02Idempotency(t, accepted)).All(); len(faults) != 0 {
				t.Errorf("required on %s: want no fault, got %+v", accepted, faults)
			}
		}
		faults := idempotencyFaults(td02Idempotency(t, "patch"))
		if len(faults) != 1 || !strings.Contains(faults[0].Message, "create is required") || faults[0].Path != "idempotency.required_on" {
			t.Errorf("required on patch only: want one fault that says create is required, got %+v", faults)
		}
		for _, refused := range []struct{ operation, method string }{{"read", "GET"}, {"list", "GET"}, {"update", "PUT"}, {"delete", "DELETE"}} {
			faults := idempotencyFaults(td02Idempotency(t, "create, "+refused.operation))
			if len(faults) != 1 {
				t.Errorf("required on create and %s: want one fault, got %+v", refused.operation, faults)
				continue
			}
			fault := faults[0]
			if fault.Found != refused.operation || fault.Path != "idempotency.required_on[1]" ||
				!strings.Contains(fault.Message, `"`+refused.operation+`"`) || !strings.Contains(fault.Message, refused.method+" ignores the key") {
				t.Errorf("required on create and %s: the fault does not name %s and %s: %+v", refused.operation, refused.operation, refused.method, fault)
			}
			if strings.Join(fault.Allowed, ",") != "create,patch" {
				t.Errorf("required on create and %s: the fault allows %v, want create and patch", refused.operation, fault.Allowed)
			}
		}
	})
}

func TestIdempotencyOnIgnoredMethod(t *testing.T) {
	t.Run("ST-DECL-SP08-T Required on with read, list, update or delete satisfies it, and create or patch does not", func(t *testing.T) {
		for _, satisfies := range []string{"create, read", "create, list", "create, update", "create, delete"} {
			if !IdempotencyOnIgnoredMethod(td02Idempotency(t, satisfies)) {
				t.Errorf("required on %s does not satisfy the specification", satisfies)
			}
		}
		for _, not := range []string{"create", "create, patch"} {
			if IdempotencyOnIgnoredMethod(td02Idempotency(t, not)) {
				t.Errorf("required on %s satisfies the specification", not)
			}
		}
	})
}
