package org_name_validator

import (
	"context"
	"net/http"
	"testing"

	"github.com/example/service/out/slots"
)

func TestRejectsReservedName(t *testing.T) {
	v := New()
	result, err := v.Evaluate(context.Background(), &slots.BeforeCreateRequest{
		Input: &slots.CreateRequest{
			Entity: "Organization",
			Fields: map[string]string{"name": "system-ops"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Ok {
		t.Error("expected Ok=false for reserved name")
	}
	if !result.Halt {
		t.Error("expected Halt=true for short-circuit chain")
	}
	if result.StatusCode != int32(http.StatusBadRequest) {
		t.Errorf("expected status 400, got %d", result.StatusCode)
	}
}

func TestAllowsValidName(t *testing.T) {
	v := New()
	result, err := v.Evaluate(context.Background(), &slots.BeforeCreateRequest{
		Input: &slots.CreateRequest{
			Entity: "Organization",
			Fields: map[string]string{"name": "acme-corp"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Ok {
		t.Error("expected Ok=true for valid name")
	}
}
