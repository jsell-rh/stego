package admin_creation_policy

import (
	"context"

	"github.com/example/service/out/slots"
)

// AdminCreationPolicy prevents non-admins from creating admin users.
type AdminCreationPolicy struct{}

// New returns a new AdminCreationPolicy that implements the BeforeCreateSlot interface.
func New() *AdminCreationPolicy {
	return &AdminCreationPolicy{}
}

// Evaluate checks that only admins can create admin users.
func (p *AdminCreationPolicy) Evaluate(_ context.Context, req *slots.BeforeCreateRequest) (*slots.SlotResult, error) {
	if req.Input != nil && req.Input.Fields["role"] == "admin" {
		if req.Caller == nil || req.Caller.Role != "admin" {
			return &slots.SlotResult{Ok: false, ErrorMessage: "only admins can create admins"}, nil
		}
	}
	return &slots.SlotResult{Ok: true}, nil
}
