package send_welcome

import (
	"context"
	"log"

	"github.com/example/service/out/slots"
)

// WelcomeSender sends a welcome notification after a user is created.
// Demonstrates the after_create fan-out pattern for post-creation side effects.
type WelcomeSender struct{}

// New returns a new WelcomeSender that implements the AfterCreateSlot interface.
func New() *WelcomeSender {
	return &WelcomeSender{}
}

// Evaluate logs a welcome message for the newly created user. In production,
// this would send an email, push notification, or enqueue a message.
func (w *WelcomeSender) Evaluate(_ context.Context, req *slots.AfterCreateRequest) (*slots.SlotResult, error) {
	// The email address is personal data, so the debug log names only the
	// entity. A real sender reads the address and hands it to its delivery
	// channel, never to a log.
	log.Printf("WELCOME: sending welcome (entity=%s)", req.Entity)
	return &slots.SlotResult{Ok: true}, nil
}
