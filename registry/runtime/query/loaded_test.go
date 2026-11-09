package query

import (
	"context"
	"errors"
	"testing"
)

func TestLoaded(t *testing.T) {
	t.Run("ST-SUP-RP11-LC12-T01 A fill gets a loaded value only from the query handle", func(t *testing.T) {
		e := newEnv(t)
		e.insert(t, "INSERT INTO app.reservations (tenant_id, id, sku) VALUES ('t1', 'r-1', 'sku-77')")
		e.insert(t, "INSERT INTO app.orders (tenant_id, id, total) VALUES ('t1', 'o-1', 5)")
		port := &fakeDecision{decide: allowAll}
		rt := e.runtimeOf(t, port)
		reservations, orders := reservationEntity(t), orderEntity(t, false)
		q := mustQuery(t, "open-reservations", reservations, orders)
		got, err := Run(context.Background(), rt, q, bound(t, "t1", "user:u-7", "open-reservations"), "r-1",
			func(ctx context.Context, id string, h *Handle[noProbe]) (*Loaded[Reservation], error) {
				repo, err := Repo(h, reservations)
				if err != nil {
					return nil, err
				}
				return repo.Get(ctx, id)
			})
		if err != nil {
			t.Fatal(err)
		}
		if got.Value().SKU != "sku-77" || got.ID() != "r-1" {
			t.Errorf("the loaded value holds %+v, want the stored sku sku-77 of r-1", got.Value())
		}

		// A value that a fill makes itself, with a composite literal or as a copy of a loaded value, is refused by
		// every operation that takes a loaded value, before the decision port.
		var refusals map[string]error
		var sent int
		_, err = RunStream(context.Background(), rt, q, bound(t, "t1", "user:u-7", "open-reservations"), struct{}{}, orders,
			func(Order) error { sent++; return nil },
			func(ctx context.Context, _ struct{}, s *Stream[Order, noProbe]) error {
				repo, err := Repo(s.Handle(), orders)
				if err != nil {
					return err
				}
				loaded, err := repo.Get(ctx, "o-1")
				if err != nil {
					return err
				}
				made := &Loaded[Order]{}
				copied := *loaded
				before := len(port.recorded())
				_, evalMade := s.Handle().Authz().Evaluate(ctx, "view", made)
				_, evalCopy := s.Handle().Authz().Evaluate(ctx, "view", &copied)
				refusals = map[string]error{
					"Evaluate of a composite literal": evalMade,
					"Evaluate of a copy":              evalCopy,
					"Send of a composite literal":     s.Send(ctx, made),
					"Send of a copy":                  s.Send(ctx, &copied),
				}
				if len(port.recorded()) != before {
					return errors.New("a value that the fill made reached the decision port")
				}
				// The control: the value that the handle loaded is sent.
				return s.Send(ctx, loaded)
			})
		if err != nil {
			t.Fatal(err)
		}
		for name, err := range refusals {
			if !errors.Is(err, errForeignValue) {
				t.Errorf("%s gives %v, want errForeignValue", name, err)
			}
		}
		if sent != 1 {
			t.Errorf("%d messages sent, want only the 1 of the loaded value", sent)
		}
	})
}
