// Package getordertotal is the fill of the query get-order-total of the test service: it reads the one Order of the
// request, which the caller may browse, and returns its total.
package getordertotal

import (
	"context"

	"example.com/shop/out/queries"
)

// Total is the response of get-order-total.
type Total struct {
	ID    string `json:"id"`
	Total int64  `json:"total"`
}

// Fill reads the Order of the request and returns its total.
func Fill(ctx context.Context, req queries.GetOrderTotalRequest, h *queries.GetOrderTotalHandle) (queries.GetOrderTotalResponse, error) {
	repo, err := queries.GetOrderTotalOrders(h)
	if err != nil {
		return nil, err
	}
	order, err := repo.Get(ctx, req.OrderID)
	if err != nil {
		return nil, err
	}
	v := order.Value()
	return Total{ID: v.ID, Total: v.Total}, nil
}
