package check_dependencies

import (
	"context"
	"net/http"

	"github.com/example/service/out/slots"
)

// DependencyChecker prevents deletion of organizations that have active
// resources (users, settings). In production this would query the database;
// here we demonstrate the before_delete gate pattern.
type DependencyChecker struct{}

// New returns a new DependencyChecker that implements the BeforeDeleteSlot interface.
func New() *DependencyChecker {
	return &DependencyChecker{}
}

// Evaluate rejects deletion when the entity has known dependents.
// A real implementation would query the store for child records.
func (c *DependencyChecker) Evaluate(_ context.Context, req *slots.BeforeDeleteRequest) (*slots.SlotResult, error) {
	if req.EntityID == "" {
		return &slots.SlotResult{Ok: true}, nil
	}
	// Placeholder: in a real service, this would check for active users/settings
	// belonging to this organization. Here we always allow, demonstrating
	// the gate contract.
	return &slots.SlotResult{Ok: true}, nil
}

// EvaluateWithDependents is a helper showing the rejection path for tests.
func EvaluateWithDependents(_ context.Context, req *slots.BeforeDeleteRequest) (*slots.SlotResult, error) {
	return &slots.SlotResult{
		Ok:           false,
		StatusCode:   int32(http.StatusConflict),
		ErrorMessage: "cannot delete organization " + req.EntityID + ": active resources exist",
	}, nil
}
