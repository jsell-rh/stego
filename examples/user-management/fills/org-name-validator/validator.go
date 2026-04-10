package org_name_validator

import (
	"context"
	"net/http"
	"strings"

	"github.com/example/service/out/slots"
)

// OrgNameValidator rejects organization names containing reserved words.
// Demonstrates short-circuit chain semantics: returns halt=true with a 400
// status when the name is invalid, stopping the chain before provisioning.
type OrgNameValidator struct{}

// New returns a new OrgNameValidator that implements the BeforeCreateSlot interface.
func New() *OrgNameValidator {
	return &OrgNameValidator{}
}

// Evaluate checks that the organization name does not contain reserved words.
func (v *OrgNameValidator) Evaluate(_ context.Context, req *slots.BeforeCreateRequest) (*slots.SlotResult, error) {
	if req.Input == nil {
		return &slots.SlotResult{Ok: true}, nil
	}
	name := req.Input.Fields["name"]
	reserved := []string{"system", "internal", "admin", "root"}
	lower := strings.ToLower(name)
	for _, r := range reserved {
		if strings.Contains(lower, r) {
			return &slots.SlotResult{
				Ok:           false,
				Halt:         true,
				StatusCode:   int32(http.StatusBadRequest),
				ErrorMessage: "organization name must not contain reserved word: " + r,
			}, nil
		}
	}
	return &slots.SlotResult{Ok: true}, nil
}
