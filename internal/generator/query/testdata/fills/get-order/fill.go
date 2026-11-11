// Package getorder is the fill of the query get-order of the test service: it reads the one Order of the request
// and returns its value.
package getorder

import (
	"context"
	"errors"
	"sync/atomic"

	"example.com/shop/out/queries"
)

var calls atomic.Int64

// Calls counts the calls of the fill, for the service test.
func Calls() int64 { return calls.Load() }

// Fill reads the Order of the request.
func Fill(ctx context.Context, req queries.GetOrderRequest, h *queries.GetOrderHandle) (queries.GetOrderResponse, error) {
	calls.Add(1)
	repo, err := queries.GetOrderOrders(h)
	if err != nil {
		return nil, err
	}
	// A row that went away after the decision before the fill is not found, which the handler answers 404, as it
	// answers the declared refusal order_unknown with 404 order.unknown (ST-SUP-RP11-LC17).
	order, err := repo.Get(ctx, req.OrderID)
	if errors.Is(err, queries.ErrNotFound) {
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	// An order whose status is lost is the declared refusal order_unknown, and one whose status is broken an error
	// whose text is private (ST-SUP-RC33-OC07).
	switch order.Value().Status {
	case "lost":
		return nil, queries.GetOrderOrderUnknown()
	case "broken":
		return nil, errors.New("the fill failed for a@example.com in select 1")
	}
	return order.Value(), nil
}
