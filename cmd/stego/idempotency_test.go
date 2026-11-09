package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/declaration"
)

// td02WithPatch is ST-DECL-TD02 with patch added to todos.
var td02WithPatch = strings.Replace(td02, "operations: [create, read, update, delete, list]\n",
	"operations: [create, read, update, delete, list, patch]\n    patchable: [title, done]\n", 1)

// withIdempotency gives the declaration with idempotency required on the operations given.
func withIdempotency(service, requiredOn string) string {
	return service + "idempotency:\n  required_on: [" + requiredOn + "]\n"
}

// reportedSetting gives the setting at the path in the settings that stego validate --json reports.
func reportedSetting(t *testing.T, document declaration.FaultsDocument, path string) declaration.Setting {
	t.Helper()
	for _, setting := range document.Settings {
		if setting.Path == path {
			return setting
		}
	}
	t.Fatalf("the reported settings hold no %s: %+v", path, document.Settings)
	return declaration.Setting{}
}

func TestDeclareIdempotentOperations(t *testing.T) {
	t.Run("ST-DECL-UC07-R03-T01 Idempotency with no retention gets 24h", func(t *testing.T) {
		document, run := validateJSON(t, td02Project(t, withIdempotency(td02, "create")))
		if run.code != 0 || !document.Accepted {
			t.Fatalf("idempotency required on create with no retention was refused: exit %d\n%s%s", run.code, run.stdout, run.stderr)
		}
		if retention := reportedSetting(t, document, "idempotency.retention"); retention.Value != "24h" || !retention.Default {
			t.Errorf("want the retention 24h, marked as the default, got %+v", retention)
		}
	})

	t.Run("ST-DECL-UC07-FL-T An author declares idempotency, and checking accepts it with create among the required operations", func(t *testing.T) {
		service := withIdempotency(td02WithPatch, "create, patch") + "  retention: 48h\n  fingerprint: body\n"
		document, run := validateJSON(t, td02Project(t, service))
		if run.code != 0 || !document.Accepted || len(document.Faults) != 0 {
			t.Fatalf("the idempotency declaration was refused: exit %d\n%s%s", run.code, run.stdout, run.stderr)
		}
		want := map[string]declaration.Setting{
			"idempotency.required_on": {Path: "idempotency.required_on", Value: []any{"create", "patch"}},
			"idempotency.retention":   {Path: "idempotency.retention", Value: "48h"},
			"idempotency.fingerprint": {Path: "idempotency.fingerprint", Value: "body"},
		}
		for path, setting := range want {
			if got := reportedSetting(t, document, path); !reflect.DeepEqual(got, setting) {
				t.Errorf("%s: want %+v, got %+v", path, setting, got)
			}
		}
	})

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

	t.Run("ST-DECL-UC07-R02-T01 Idempotency required on read is refused, naming read", func(t *testing.T) {
		refusedKeyOperation(t, "read", "")
	})
	t.Run("ST-DECL-UC07-R02-T02 Idempotency required on list is refused, naming list", func(t *testing.T) {
		refusedKeyOperation(t, "list", "")
	})
	t.Run("ST-DECL-UC07-R02-T03 Idempotency required on delete is refused, naming delete", func(t *testing.T) {
		refusedKeyOperation(t, "delete", "")
	})
	t.Run("ST-DECL-UC07-R02-T04 Idempotency required on update is refused, naming update", func(t *testing.T) {
		refusedKeyOperation(t, "update", "PUT ignores the key")
	})
}

// refusedKeyOperation runs stego validate on ST-DECL-TD02 with idempotency required on create and the operation,
// and expects a refusal with a fault that names the operation in Required on and says what says gives.
func refusedKeyOperation(t *testing.T, operation, says string) {
	t.Helper()
	document, run := validateJSON(t, td02Project(t, withIdempotency(td02, "create, "+operation)))
	if run.code == 0 || document.Accepted {
		t.Fatalf("idempotency required on create and %s was accepted:\n%s", operation, run.stdout)
	}
	for _, fault := range document.Faults {
		if strings.HasPrefix(fault.Path, "idempotency.required_on") && fault.Found == operation &&
			strings.Contains(fault.Message, "Required on") && strings.Contains(fault.Message, `"`+operation+`"`) &&
			strings.Contains(fault.Message, says) {
			return
		}
	}
	t.Errorf("no fault names %s in Required on and says %q:\n%s", operation, says, run.stdout)
}
