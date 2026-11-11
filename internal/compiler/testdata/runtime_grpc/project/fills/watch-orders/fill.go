// Package watchorders is the fill of the stream query watch-orders of the test service: it lists the orders that the
// caller may view and sends each of them.
package watchorders

import (
	"context"

	"example.com/shop/out/queries"
)

// Fill sends each order of the list.
func Fill(ctx context.Context, _ queries.WatchOrdersRequest, s *queries.WatchOrdersStream) error {
	repo, err := queries.WatchOrdersOrders(s.Handle())
	if err != nil {
		return err
	}
	page, err := repo.List(ctx, queries.ListRequest{})
	if err != nil {
		return err
	}
	for _, order := range page.Items {
		if err := s.Send(ctx, order); err != nil {
			return err
		}
	}
	return nil
}
