package ordercreated

import (
	"context"
	"testing"

	"example.com/shop/out/slots"
)

func TestFill(t *testing.T) {
	t.Run("ST-SUP-RP14-LC04-T02 The after slot fill of orders accepts each order", func(t *testing.T) {
		result, err := New().Evaluate(context.Background(), &slots.AfterCreateRequest{Entity: "Order", PersistedFields: map[string]string{"sku": "a1"}})
		if err != nil || !result.Ok {
			t.Fatalf("the fill gave %+v, %v; want it to accept", result, err)
		}
	})
}
