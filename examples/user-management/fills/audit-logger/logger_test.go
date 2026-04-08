package audit_logger

import (
	"context"
	"testing"

	"github.com/example/user-management/out/slots"
)

func TestAuditLoggerReturnsOk(t *testing.T) {
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
}
