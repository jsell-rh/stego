package user_change_notifier

import (
	"context"
	"testing"

	"github.com/example/service/out/slots"
)

func TestNotifier(t *testing.T) {
	t.Run("ST-SUP-RC20-OC34-T06 The fill user-change-notifier of the example user-management-rhsso accepts each change of a user", func(t *testing.T) {
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
	})
}
