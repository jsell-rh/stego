// Package getordersummary is the fill of the query get-order-summary of the test service: it reads the one Order of
// the request.
package getordersummary

import (
	"context"

	"example.com/shop/out/queries"
)

// Fill reads the Order of the request.
func Fill(ctx context.Context, req queries.GetOrderSummaryRequest, h *queries.GetOrderSummaryHandle) (queries.GetOrderSummaryResponse, error) {
	repo, err := queries.GetOrderSummaryOrders(h)
	if err != nil {
		return nil, err
	}
	order, err := repo.Get(ctx, req.OrderID)
	if err != nil {
		return nil, err
	}
	return order.Value(), nil
}
