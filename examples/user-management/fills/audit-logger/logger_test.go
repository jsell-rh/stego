package audit_logger

import (
	"context"
	"testing"

	"github.com/example/service/out/slots"
)

func TestAuditLogger(t *testing.T) {
	t.Run("ST-SUP-RC20-OC34-T05 The fill audit-logger of the example user-management accepts each change of a user", func(t *testing.T) {
		a := New()
		result, err := a.Evaluate(context.Background(), &slots.OnEntityChangedRequest{
			Entity: "User",
			Action: "update",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !result.Ok {
			t.Error("expected ok result")
		}
	})
}
