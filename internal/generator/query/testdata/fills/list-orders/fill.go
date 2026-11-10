// Package listorders is the fill of the query list-orders of the test service: one page of the Orders that the
// caller may view.
package listorders

import (
	"context"
	"sync/atomic"

	"example.com/shop/out/queries"
)

var (
	calls atomic.Int64
	enter atomic.Pointer[func()]
)

// Calls counts the calls of the fill, for the service test.
func Calls() int64 { return calls.Load() }

// OnEnter sets a function that the fill calls first, for the service test.
func OnEnter(f func()) { enter.Store(&f) }

// Fill lists one page of Orders after the request's cursor.
func Fill(ctx context.Context, req queries.ListOrdersRequest, h *queries.ListOrdersHandle) (queries.ListOrdersResponse, error) {
	calls.Add(1)
	if f := enter.Load(); f != nil && *f != nil {
		(*f)()
	}
	repo, err := queries.ListOrdersOrders(h)
	if err != nil {
		return queries.ListOrdersResponse{}, err
	}
	page, err := repo.List(ctx, queries.ListRequest{Limit: req.Limit, After: req.After})
	if err != nil {
		return queries.ListOrdersResponse{}, err
	}
	items := []queries.Order{}
	for _, o := range page.Items {
		items = append(items, o.Value())
	}
	res := queries.ListOrdersResponse{Items: items}
	if page.More {
		res.Next = page.Next
	}
	return res, nil
}
