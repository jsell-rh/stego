package rbac_policy

import (
	"context"
	"testing"

	"github.com/example/user-management/out/slots"
)

func TestUnauthenticatedCallerIsRejected(t *testing.T) {
	p := New()
	result, err := p.Evaluate(context.Background(), &slots.BeforeCreateRequest{
		Input:  &slots.CreateRequest{Entity: "User"},
		Caller: nil,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Ok {
		t.Error("expected rejection for nil caller")
	}
}

func TestAuthenticatedCallerIsAllowed(t *testing.T) {
	p := New()
	result, err := p.Evaluate(context.Background(), &slots.BeforeCreateRequest{
		Input:  &slots.CreateRequest{Entity: "User"},
		Caller: &slots.Identity{Role: "member"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Ok {
		t.Error("expected authenticated caller to be allowed")
	}
}
