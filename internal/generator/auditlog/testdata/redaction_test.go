package auditlog

import (
	"encoding/json"
	"testing"

	"stego.dev/runtime/audit"
)

// states runs the redacting path of the audit runtime with the emitted settings and the emitted fields of an
// entity, and returns the before and after of the event.
func states(t *testing.T, fields audit.Fields, before, after map[string]any) (map[string]any, map[string]any) {
	t.Helper()
	var e audit.Event
	if err := e.SetStates(Settings, fields, before, after); err != nil {
		t.Fatal(err)
	}
	var b, a map[string]any
	if err := json.Unmarshal(e.Before.JSON(), &b); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(e.After.JSON(), &a); err != nil {
		t.Fatal(err)
	}
	return b, a
}

func TestRedaction(t *testing.T) {
	t.Run("ST-SUP-RC28-OC01-T01 The emitted redaction replaces every pii field in before and after", func(t *testing.T) {
		before, after := states(t, Fields["Customer"],
			map[string]any{"name": "Ada", "plan": "free", "api_token": "s3cr3t"},
			map[string]any{"name": "Grace", "plan": "gold", "api_token": "s3cr3t"})
		if before["name"] != audit.RedactionMarker || after["name"] != audit.RedactionMarker {
			t.Fatalf("name is %v before and %v after, want the redaction marker", before["name"], after["name"])
		}
		if before["plan"] != "free" || after["plan"] != "gold" {
			t.Fatalf("plan is %v before and %v after, want the real values", before["plan"], after["plan"])
		}
		// The second pii field, on a second entity, is redacted too.
		before, after = states(t, VendorFields, map[string]any{"label": "Acme", "contact": "ada@example.com"},
			map[string]any{"label": "Acme", "contact": "grace@example.com"})
		if before["contact"] != audit.RedactionMarker || after["contact"] != audit.RedactionMarker || after["label"] != "Acme" {
			t.Fatalf("Vendor before %v after %v, want contact redacted and label kept", before, after)
		}
		if PII["Plan"] || !PII["Customer"] || !PII["Vendor"] {
			t.Fatalf("PII = %v, want Customer and Vendor only", PII)
		}
	})
}
