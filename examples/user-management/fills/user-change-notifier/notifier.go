package user_change_notifier

import (
	"context"
	"log"

	"github.com/example/user-management/out/slots"
)

// UserChangeNotifier sends notifications when user entities change.
type UserChangeNotifier struct{}

// New returns a new UserChangeNotifier that implements the OnEntityChangedSlot interface.
func New() *UserChangeNotifier {
	return &UserChangeNotifier{}
}

// Evaluate logs the entity change event. In production, this would send a
// notification via email, webhook, or message queue.
func (n *UserChangeNotifier) Evaluate(_ context.Context, req *slots.OnEntityChangedRequest) (*slots.SlotResult, error) {
	log.Printf("entity changed: %s action=%s", req.Entity, req.Action)
	return &slots.SlotResult{Ok: true}, nil
}
