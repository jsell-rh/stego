package user_change_notifier

import (
	"context"
	"testing"

	"github.com/example/user-management/out/slots"
)

func TestNotifierReturnsOk(t *testing.T) {
	n := New()
	result, err := n.Evaluate(context.Background(), &slots.OnEntityChangedRequest{
		Entity: "User",
		Action: "create",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Ok {
		t.Error("expected ok result")
	}
}
