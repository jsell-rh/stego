package send_welcome

import (
	"context"
	"testing"

	"github.com/example/service/out/slots"
)

func TestWelcomeSenderSuccess(t *testing.T) {
	w := New()
	result, err := w.Evaluate(context.Background(), &slots.AfterCreateRequest{
		Entity:          "User",
		PersistedFields: map[string]string{"email": "user@example.com", "role": "member"},
		Caller:          &slots.Identity{Role: "admin"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Ok {
		t.Error("expected welcome sender to succeed")
	}
}

func TestWelcomeSenderEmptyEmail(t *testing.T) {
	w := New()
	result, err := w.Evaluate(context.Background(), &slots.AfterCreateRequest{
		Entity:          "User",
		PersistedFields: map[string]string{"role": "member"},
		Caller:          &slots.Identity{Role: "admin"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Ok {
		t.Error("expected welcome sender to succeed even with empty email")
	}
}
