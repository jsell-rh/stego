package command

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestAuthzPart(t *testing.T) {
	e := newEnv(t)
	cmd := e.command("reserve-and-charge", nil, e.orders, e.reservationE, e.chargeE)
	e.exec(`INSERT INTO orders (tenant_id, id, status, authz_token) VALUES ('t1', 'o5', 'open', 't5'), ('t1', 'o0', 'open', '')`)

	t.Run("ST-SUP-RP10-LC04-T01 A fill's decision uses the principal and the row token", func(t *testing.T) {
		h, _, end := e.bind("t1", u7(t), cmd)
		defer end()
		o, err := repo(t, h, e.orders).Get(e.ctx, "o5")
		if err != nil {
			t.Fatal(err)
		}
		e.decision.taken()
		allowed, err := h.Authz().Evaluate(e.ctx, "reserve", o, nil)
		if err != nil || !allowed {
			t.Fatalf("Evaluate gave %t, %v", allowed, err)
		}
		calls := e.decision.taken()
		if len(calls) != 1 {
			t.Fatalf("the port got %d calls, want one", len(calls))
		}
		token, isToken := calls[0].Consistency.Token()
		if calls[0].Subject != "user:u-7" || calls[0].Action != "reserve" || calls[0].Resource != "order:o5" ||
			!isToken || token.Text() != "t5" {
			t.Fatalf("the port got %+v, want subject user:u-7 at least as fresh as t5", calls[0])
		}
	})

	t.Run("ST-SUP-RP10-LC04-T02 The handle has no bypass", func(t *testing.T) {
		part := reflect.TypeFor[*Authz]()
		for i := range part.NumMethod() {
			name := strings.ToLower(part.Method(i).Name)
			for _, word := range []string{"bypass", "skip", "admin", "system"} {
				if strings.Contains(name, word) {
					t.Errorf("the authorization part has the method %s", part.Method(i).Name)
				}
			}
		}
		h, _, end := e.bind("t1", u7(t), cmd)
		defer end()
		e.decision.taken()
		// The part has no consistency of its own to give: an evaluation without a loaded value has none and is
		// refused before the port, and a row with no token and a zero floor is decided fresh, never with no
		// consistency.
		if _, err := h.Authz().Evaluate(e.ctx, "reserve", nil, nil); !errors.Is(err, ErrNoResource) {
			t.Fatalf("an evaluation with no loaded value gave %v", err)
		}
		if calls := e.decision.taken(); len(calls) != 0 {
			t.Fatalf("the refused evaluation reached the port: %+v", calls)
		}
		o, err := repo(t, h, e.orders).Get(e.ctx, "o0")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.Authz().Evaluate(e.ctx, "reserve", o, nil); err != nil {
			t.Fatal(err)
		}
		for _, call := range e.decision.taken() {
			if call.Consistency.IsZero() || (call.Resource == "order:o0" && !call.Consistency.IsFresh()) {
				t.Fatalf("the port got the consistency %s for %s", call.Consistency, call.Resource)
			}
		}
	})
}
