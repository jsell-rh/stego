// Package listcharges is the fill of the query list-charges of the test service: one page of the Charges that the
// caller may view. card_last4 is pii, so each Charge read has its read audit row.
package listcharges

import (
	"context"

	"example.com/shop/out/queries"
)

// Fill lists one page of Charges.
func Fill(ctx context.Context, req queries.ListChargesRequest, h *queries.ListChargesHandle) (queries.ListChargesResponse, error) {
	repo, err := queries.ListChargesCharges(h)
	if err != nil {
		return queries.ListChargesResponse{}, err
	}
	page, err := repo.List(ctx, queries.ListRequest{Limit: req.Limit, After: req.After})
	if err != nil {
		return queries.ListChargesResponse{}, err
	}
	items := []queries.Charge{}
	for _, c := range page.Items {
		items = append(items, c.Value())
	}
	res := queries.ListChargesResponse{Items: items}
	if page.More {
		res.Next = page.Next
	}
	return res, nil
}
