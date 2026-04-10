package org_provisioner

import (
	"context"
	"net/http"
	"testing"

	"github.com/example/service/out/slots"
)

func TestProvisionerReturnsOk(t *testing.T) {
	p := New()
	result, err := p.Evaluate(context.Background(), &slots.BeforeCreateRequest{
		Input: &slots.CreateRequest{
			Entity: "Organization",
			Fields: map[string]string{"name": "acme-corp"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Ok {
		t.Error("expected Ok=true from provisioner")
	}
	if result.Halt {
		t.Error("expected Halt=false for normal provisioning")
	}
}

func TestProvisionerHaltsForNoopPrefix(t *testing.T) {
	p := New()
	result, err := p.Evaluate(context.Background(), &slots.BeforeCreateRequest{
		Input: &slots.CreateRequest{
			Entity: "Organization",
			Fields: map[string]string{"name": "noop-test-org"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Ok {
		t.Error("expected Ok=true for halt (success, not rejection)")
	}
	if !result.Halt {
		t.Error("expected Halt=true for noop- prefix")
	}
	if result.StatusCode != int32(http.StatusNoContent) {
		t.Errorf("expected StatusCode=204, got %d", result.StatusCode)
	}
}
