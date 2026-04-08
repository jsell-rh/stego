package admin_creation_policy

import (
	"context"
	"testing"

	"github.com/example/user-management/out/slots"
)

func TestNonAdminCannotCreateAdmin(t *testing.T) {
	p := New()
	result, err := p.Evaluate(context.Background(), &slots.BeforeCreateRequest{
		Input:  &slots.CreateRequest{Entity: "User", Fields: map[string]string{"role": "admin"}},
		Caller: &slots.Identity{Role: "member"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Ok {
		t.Error("expected rejection for non-admin creating admin")
	}
	if result.ErrorMessage != "only admins can create admins" {
		t.Errorf("unexpected error message: %s", result.ErrorMessage)
	}
}

func TestAdminCanCreateAdmin(t *testing.T) {
	p := New()
	result, err := p.Evaluate(context.Background(), &slots.BeforeCreateRequest{
		Input:  &slots.CreateRequest{Entity: "User", Fields: map[string]string{"role": "admin"}},
		Caller: &slots.Identity{Role: "admin"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Ok {
		t.Error("expected admin to be allowed to create admin")
	}
}

func TestNonAdminCanCreateMember(t *testing.T) {
	p := New()
	result, err := p.Evaluate(context.Background(), &slots.BeforeCreateRequest{
		Input:  &slots.CreateRequest{Entity: "User", Fields: map[string]string{"role": "member"}},
		Caller: &slots.Identity{Role: "member"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Ok {
		t.Error("expected non-admin to be allowed to create member")
	}
}
