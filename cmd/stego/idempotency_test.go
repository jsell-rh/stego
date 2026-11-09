package main

import (
	"strings"
	"testing"
)

// td02WithPatch is ST-DECL-TD02 with patch added to todos.
var td02WithPatch = strings.Replace(td02, "operations: [create, read, update, delete, list]\n",
	"operations: [create, read, update, delete, list, patch]\n    patchable: [title, done]\n", 1)

// withIdempotency gives the declaration with idempotency required on the operations given.
func withIdempotency(service, requiredOn string) string {
	return service + "idempotency:\n  required_on: [" + requiredOn + "]\n"
}

func TestDeclareIdempotentOperations(t *testing.T) {
	t.Run("ST-DECL-UC07-R01-T01 Idempotency that is not required on create is refused", func(t *testing.T) {
		document, run := validateJSON(t, td02Project(t, withIdempotency(td02WithPatch, "patch")))
		if run.code == 0 || document.Accepted {
			t.Fatalf("idempotency required on patch only was accepted:\n%s", run.stdout)
		}
		found := false
		for _, fault := range document.Faults {
			found = found || (strings.HasPrefix(fault.Path, "idempotency.required_on") && strings.Contains(fault.Message, "Required on") && strings.Contains(fault.Message, "create is required"))
		}
		if !found {
			t.Errorf("no fault names Required on and says that create is required:\n%s", run.stdout)
		}
	})

	t.Run("ST-DECL-UC07-R01-T02 Idempotency required on create, or on create and patch, is accepted", func(t *testing.T) {
		for name, service := range map[string]string{
			"create":           withIdempotency(td02, "create"),
			"create and patch": withIdempotency(td02WithPatch, "create, patch"),
		} {
			document, run := validateJSON(t, td02Project(t, service))
			if run.code != 0 || !document.Accepted || len(document.Faults) != 0 {
				t.Errorf("idempotency required on %s was refused: exit %d\n%s%s", name, run.code, run.stdout, run.stderr)
			}
		}
	})

	for _, refused := range []struct{ id, operation, says string }{
		{"ST-DECL-UC07-R02-T01 Idempotency required on read is refused, naming read", "read", ""},
		{"ST-DECL-UC07-R02-T02 Idempotency required on list is refused, naming list", "list", ""},
		{"ST-DECL-UC07-R02-T03 Idempotency required on delete is refused, naming delete", "delete", ""},
		{"ST-DECL-UC07-R02-T04 Idempotency required on update is refused, naming update", "update", "PUT ignores the key"},
	} {
		t.Run(refused.id, func(t *testing.T) {
			document, run := validateJSON(t, td02Project(t, withIdempotency(td02, "create, "+refused.operation)))
			if run.code == 0 || document.Accepted {
				t.Fatalf("idempotency required on create and %s was accepted:\n%s", refused.operation, run.stdout)
			}
			found := false
			for _, fault := range document.Faults {
				found = found || (strings.HasPrefix(fault.Path, "idempotency.required_on") && fault.Found == refused.operation &&
					strings.Contains(fault.Message, "Required on") && strings.Contains(fault.Message, `"`+refused.operation+`"`) &&
					strings.Contains(fault.Message, refused.says))
			}
			if !found {
				t.Errorf("no fault names %s in Required on%s:\n%s", refused.operation, map[bool]string{true: " and says " + refused.says}[refused.says != ""], run.stdout)
			}
		})
	}
}
