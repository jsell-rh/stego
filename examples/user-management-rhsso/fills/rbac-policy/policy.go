package rbac_policy

import (
	"context"

	"github.com/example/service/out/slots"
)

// RBACPolicy enforces role-based access control on create operations.
type RBACPolicy struct{}

// New returns a new RBACPolicy that implements the BeforeCreateSlot interface.
func New() *RBACPolicy {
	return &RBACPolicy{}
}

// Evaluate checks that the caller has permission to create entities.
func (p *RBACPolicy) Evaluate(_ context.Context, req *slots.BeforeCreateRequest) (*slots.SlotResult, error) {
	if req.Caller == nil || req.Caller.Role == "" {
		return &slots.SlotResult{Ok: false, ErrorMessage: "authentication required"}, nil
	}
	return &slots.SlotResult{Ok: true}, nil
}
