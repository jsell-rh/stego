package org_provisioner

import (
	"context"
	"log"

	"github.com/example/service/out/slots"
)

// OrgProvisioner performs side-effects when an organization is created,
// such as setting up default resources. Runs after validation in the
// before_create chain.
type OrgProvisioner struct{}

// New returns a new OrgProvisioner that implements the BeforeCreateSlot interface.
func New() *OrgProvisioner {
	return &OrgProvisioner{}
}

// Evaluate logs the provisioning action. In production, this would create
// default settings, namespaces, or external resources for the organization.
func (p *OrgProvisioner) Evaluate(_ context.Context, req *slots.BeforeCreateRequest) (*slots.SlotResult, error) {
	name := ""
	if req.Input != nil {
		name = req.Input.Fields["name"]
	}
	log.Printf("provisioning resources for organization: %s", name)
	return &slots.SlotResult{Ok: true}, nil
}
