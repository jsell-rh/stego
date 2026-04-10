package org_provisioner

import (
	"context"
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
}
