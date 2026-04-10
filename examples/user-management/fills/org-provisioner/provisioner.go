package org_provisioner

import (
	"context"
	"log"
	"net/http"
	"strings"

	"github.com/example/service/out/slots"
)

// OrgProvisioner performs side-effects when an organization is created,
// such as setting up default resources. Runs after validation in the
// before_create chain.
//
// Demonstrates halt semantics: when the organization name starts with
// "noop-", the provisioner returns Ok=true, Halt=true, StatusCode=204.
// This signals "the operation succeeded but nothing needs to be created"
// — the handler stops the pipeline and returns 204 No Content without
// persisting the entity. This is the canonical halt pattern (analogous
// to discard-stale-generation in the spec).
type OrgProvisioner struct{}

// New returns a new OrgProvisioner that implements the BeforeCreateSlot interface.
func New() *OrgProvisioner {
	return &OrgProvisioner{}
}

// Evaluate logs the provisioning action. In production, this would create
// default settings, namespaces, or external resources for the organization.
// Names starting with "noop-" trigger a halt with 204, simulating an
// already-provisioned organization that does not need creation.
func (p *OrgProvisioner) Evaluate(_ context.Context, req *slots.BeforeCreateRequest) (*slots.SlotResult, error) {
	name := ""
	if req.Input != nil {
		name = req.Input.Fields["name"]
	}
	if strings.HasPrefix(name, "noop-") {
		log.Printf("organization %q already provisioned; halting pipeline", name)
		return &slots.SlotResult{
			Ok:         true,
			Halt:       true,
			StatusCode: int32(http.StatusNoContent),
		}, nil
	}
	log.Printf("provisioning resources for organization: %s", name)
	return &slots.SlotResult{Ok: true}, nil
}
