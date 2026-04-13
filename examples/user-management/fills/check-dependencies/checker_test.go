package check_dependencies

import (
	"context"
	"net/http"
	"testing"

	"github.com/example/service/out/slots"
)

func TestAllowDeletionWhenNoDependents(t *testing.T) {
	c := New()
	result, err := c.Evaluate(context.Background(), &slots.BeforeDeleteRequest{
		Entity:   "Organization",
		EntityID: "org-123",
		Caller:   &slots.Identity{Role: "admin"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Ok {
		t.Error("expected deletion to be allowed when no dependents exist")
	}
}

func TestRejectDeletionWhenDependentsExist(t *testing.T) {
	result, err := EvaluateWithDependents(context.Background(), &slots.BeforeDeleteRequest{
		Entity:   "Organization",
		EntityID: "org-456",
		Caller:   &slots.Identity{Role: "admin"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Ok {
		t.Error("expected deletion to be rejected when dependents exist")
	}
	if result.StatusCode != int32(http.StatusConflict) {
		t.Errorf("expected 409 status code, got %d", result.StatusCode)
	}
	if result.ErrorMessage == "" {
		t.Error("expected non-empty error message")
	}
}
