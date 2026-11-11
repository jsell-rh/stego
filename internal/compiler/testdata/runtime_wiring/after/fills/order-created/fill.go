// Package ordercreated is the fill of orders.after_create of the test service: the outbox worker runs it after the
// create committed (ST-SUP-RC20-OC34), and it accepts each order.
package ordercreated

import (
	"context"

	"example.com/shop/out/slots"
)

// OrderCreated accepts each created order.
type OrderCreated struct{}

// New gives the fill of orders.after_create.
func New() *OrderCreated { return &OrderCreated{} }

// Evaluate accepts the created order.
func (*OrderCreated) Evaluate(context.Context, *slots.AfterCreateRequest) (*slots.SlotResult, error) {
	return &slots.SlotResult{Ok: true}, nil
}
