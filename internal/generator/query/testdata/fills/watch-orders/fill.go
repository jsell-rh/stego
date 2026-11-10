// Package watchorders is the fill of the stream query watch-orders of the test service: it lists the Orders that the
// caller may browse and sends each one; Send sends only the Orders that the caller may view.
package watchorders

import (
	"context"

	"example.com/shop/out/queries"
)

// Fill sends each Order that the list gives.
func Fill(ctx context.Context, req queries.WatchOrdersRequest, s *queries.WatchOrdersStream) error {
	repo, err := queries.WatchOrdersOrders(s.Handle())
	if err != nil {
		return err
	}
	request := queries.ListRequest{}
	for {
		page, err := repo.List(ctx, request)
		if err != nil {
			return err
		}
		for _, o := range page.Items {
			if err := s.Send(ctx, o); err != nil {
				return err
			}
		}
		if !page.More {
			return nil
		}
		request.After = page.Next
	}
}
