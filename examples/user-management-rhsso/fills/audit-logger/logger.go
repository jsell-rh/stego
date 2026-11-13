package audit_logger

import (
	"context"
	"log"

	"github.com/example/service/out/slots"
)

// AuditLogger records entity change events for audit purposes.
type AuditLogger struct{}

// New returns a new AuditLogger that implements the OnEntityChangedSlot interface.
func New() *AuditLogger {
	return &AuditLogger{}
}

// Evaluate logs the entity change for audit trail. In production, this would
// write to a persistent audit log store.
func (a *AuditLogger) Evaluate(_ context.Context, req *slots.OnEntityChangedRequest) (*slots.SlotResult, error) {
	log.Printf("AUDIT: entity=%s action=%s", req.Entity, req.Action)
	return &slots.SlotResult{Ok: true}, nil
}
