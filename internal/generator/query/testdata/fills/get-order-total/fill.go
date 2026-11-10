// Package getordertotal is the fill of the query get-order-total of the test service, which declares browse on its
// Order. The handler decides browse on the requested Order before the fill; the fill's repository reads on the read
// permission view of Order. The fill also reads the Order Peek, so the service test sees which permission that read
// decides.
package getordertotal

import (
	"context"
	"errors"
	"sync/atomic"

	"example.com/shop/out/queries"
)

// Peek is the key of the other Order that the fill reads: o2 of the service test, which u-7 may browse and may not
// view.
const Peek = "00000000-0000-4000-8000-000000000002"

// Answer is the response of get-order-total: the requested ID, and found or not found for the read of Peek.
type Answer struct {
	ID   string `json:"id"`
	Peek string `json:"peek"`
}

var calls atomic.Int64

// Calls counts the calls of the fill, for the service test.
func Calls() int64 { return calls.Load() }

// Fill reads Peek through its repository and answers.
func Fill(ctx context.Context, req queries.GetOrderTotalRequest, h *queries.GetOrderTotalHandle) (queries.GetOrderTotalResponse, error) {
	calls.Add(1)
	repo, err := queries.GetOrderTotalOrders(h)
	if err != nil {
		return nil, err
	}
	answer := Answer{ID: req.OrderID, Peek: "found"}
	if _, err := repo.Get(ctx, Peek); errors.Is(err, queries.ErrNotFound) {
		answer.Peek = "not found"
	} else if err != nil {
		return nil, err
	}
	return answer, nil
}
