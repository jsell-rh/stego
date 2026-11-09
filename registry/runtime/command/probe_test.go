package command

import (
	"context"
	"database/sql"
	"testing"

	"stego.dev/runtime/idempotency"
	"stego.dev/runtime/probe"
)

// emit is the one method of the test probe that the fills call, as a generated probe method does.
func (p testProbe) emit(o *probe.Observation, values ...probe.Value) { p.p.Emit(o, values...) }

// observations are the declared observations of the tests, on a recorder.
type observations struct {
	probes                         *probe.Recorder
	refused, deactivated, reserved *probe.Observation
}

// newObservations declares the observations of the tests for the emitters, on a new recorder.
func newObservations(t *testing.T, emitters ...string) *observations {
	t.Helper()
	declare := func(spec probe.Spec) *probe.Observation {
		spec.EmittedBy = emitters
		o, err := probe.Declare(spec)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	return &observations{
		probes:      probe.NewRecorder(nil),
		refused:     declare(probe.Spec{Name: "deactivation_refused", Kind: probe.Counter, Severity: probe.Info, Counts: probe.CountsAttempt}),
		deactivated: declare(probe.Spec{Name: "user_deactivated", Kind: probe.Event, Severity: probe.Info, Counts: probe.CountsCommitted}),
		reserved:    declare(probe.Spec{Name: "reservation_refused", Kind: probe.Event, Severity: probe.Info}),
	}
}

func TestProbe(t *testing.T) {
	t.Run("ST-SUP-RP10-LC14-T01 The command handle gives the fill its bound probe, which goes quiet once the fill returns", func(t *testing.T) {
		e := newEnv(t)
		obs := newObservations(t, "reserve-and-charge")
		rt := e.runtime(func(c *Config) { c.Probes = obs.probes })
		returned, emitted := make(chan struct{}), make(chan struct{})
		rt.beforeCommit = func(context.Context, *sql.Tx) error {
			close(returned)
			<-emitted
			return nil
		}
		cmd := e.command("reserve-and-charge", nil, e.reservationE)
		if _, err := e.runOn(rt, cmd, e.request("t1", u7(t), "k-probe", `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
			p := h.Probe()
			p.emit(obs.reserved)
			go func() {
				<-returned
				p.emit(obs.reserved)
				close(emitted)
			}()
			return ok, nil
		}); err != nil {
			t.Fatal(err)
		}
		got := obs.probes.Emissions("reservation_refused")
		if len(got) != 1 || got[0].Tenant != "t1" || got[0].Principal != "user:u-7" || got[0].Operation != "reserve-and-charge" {
			t.Fatalf("the recorder holds %v, want one emission of t1, user:u-7 and reserve-and-charge", got)
		}
		if obs.probes.Dropped() != 1 {
			t.Fatalf("%d emissions dropped, want the goroutine's one", obs.probes.Dropped())
		}
	})
}
