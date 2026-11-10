package query

import (
	"context"
	"testing"

	"github.com/jsell-rh/stego/registry/runtime/probe"
)

// openReservationsProbe is the typed probe that the generator emits for the query open-reservations: one method per
// observation, on the bound probe of the call.
type openReservationsProbe struct {
	p   *probe.Probe
	obs *probe.Observation
}

func (p openReservationsProbe) ReservationLookupEmpty() { p.p.Emit(p.obs) }

func TestProbe(t *testing.T) {
	t.Run("ST-SUP-RP11-LC11-T01 The query handle gives the fill its bound probe, which goes quiet once the fill returns", func(t *testing.T) {
		obs, err := probe.Declare(probe.Spec{Name: "reservation_lookup_empty", Kind: probe.Event, Severity: probe.Info,
			EmittedBy: []string{"open-reservations"}})
		if err != nil {
			t.Fatal(err)
		}
		rec := probe.NewRecorder(nil)
		call := rec.Bind(context.Background(), "t1", "user:u-7", "open-reservations")
		// The transaction is a test double: the probe does not touch storage.
		rt := unitRuntime(t, &fakeDecision{decide: allowAll})
		rt.db = unitDB(t, "")
		emit, emitted := make(chan struct{}), make(chan struct{})
		rt.afterFill = func() {
			// The fill returned; the transaction has not ended yet. The goroutine emits now.
			close(emit)
			<-emitted
		}
		q := mustQuery(t, "open-reservations", reservationEntity(t))
		b := Bound[openReservationsProbe]{Tenant: mustTenant(t, "t1"), Principal: mustPrincipal(t, "user:u-7"), Call: call,
			Probe: func(p *probe.Probe) openReservationsProbe { return openReservationsProbe{p: p, obs: obs} }}
		_, err = Run(context.Background(), rt, q, b, struct{}{},
			func(ctx context.Context, _ struct{}, h *Handle[openReservationsProbe]) (struct{}, error) {
				h.Probe().ReservationLookupEmpty()
				kept := h.Probe()
				go func() {
					<-emit
					kept.ReservationLookupEmpty()
					close(emitted)
				}()
				return struct{}{}, nil
			})
		if err != nil {
			t.Fatal(err)
		}
		got := rec.Emissions("reservation_lookup_empty")
		if len(got) != 1 {
			t.Fatalf("%d emissions recorded, want 1: %v", len(got), got)
		}
		if got[0].Tenant != "t1" || got[0].Principal != "user:u-7" || got[0].Operation != "open-reservations" {
			t.Errorf("the emission is bound to %s, %s, %s; want t1, user:u-7, open-reservations", got[0].Tenant, got[0].Principal, got[0].Operation)
		}
		if n := rec.Dropped(); n != 1 {
			t.Errorf("%d emissions dropped, want the 1 of the goroutine", n)
		}
	})

	t.Run("ST-SUP-RP11-LC11-T01 A probe call bound to another query, tenant or principal is refused", func(t *testing.T) {
		rt := unitRuntime(t, &fakeDecision{decide: allowAll})
		rt.db = unitDB(t, "")
		q := mustQuery(t, "open-reservations", reservationEntity(t))
		for _, other := range [][3]string{{"t2", "user:u-7", "open-reservations"}, {"t1", "user:u-8", "open-reservations"}, {"t1", "user:u-7", "other"}} {
			call := probe.NewRecorder(nil).Bind(context.Background(), other[0], other[1], other[2])
			b := Bound[noProbe]{Tenant: mustTenant(t, "t1"), Principal: mustPrincipal(t, "user:u-7"), Call: call, Probe: newNoProbe}
			ran := false
			_, err := Run(context.Background(), rt, q, b, struct{}{}, func(context.Context, struct{}, *Handle[noProbe]) (struct{}, error) {
				ran = true
				return struct{}{}, nil
			})
			if err == nil || ran {
				t.Errorf("a probe call bound to %v: Run gives %v and runs the fill %v; want a refusal", other, err, ran)
			}
		}
	})
}
