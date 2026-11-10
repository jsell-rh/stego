package command

import (
	"context"
	"errors"
	"testing"

	"stego.dev/runtime/idempotency"
)

// orderResource is the generated resource declaration of a command on an order.
func orderResource() *Resource {
	return &Resource{Type: "order", Token: `SELECT authz_token FROM orders WHERE tenant_id = $1 AND id = $2`,
		ValidID: func(id string) bool { return id != "" }}
}

func TestCommandPermission(t *testing.T) {
	t.Run("ST-SUP-RP10-LC31-T01 A denied command does not reach the fill", func(t *testing.T) {
		e := newEnv(t)
		e.exec(`INSERT INTO orders (tenant_id, id, status, authz_token) VALUES ('t1', 'o5', 'open', 't5')`)
		cmd := e.command("reserve", func(s *CommandSpec[testProbe]) {
			s.Permission, s.Resource = "reserve", orderResource()
		}, e.reservationE)
		// Only user:u-7 may reserve on order o5.
		e.decision.setAllow(func(subject, action, resource string) bool {
			return subject == "user:u-7" && action == "reserve" && resource == "order:o5"
		})
		calls := 0
		fill := func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
			calls++
			_, err := repo(t, h, e.reservationE).Create(ctx, reservation{ID: "r-" + h.Principal().Subject().ID,
				OrderID: "o5", SKU: "sku-1"}, nil)
			return ok, err
		}

		denied := e.request("t1", person(t, "user:u-9"), "k-lc31-denied", `{}`)
		denied.Resource = "o5"
		e.decision.taken()
		if _, err := e.run(cmd, denied, fill); !errors.Is(err, ErrDenied) {
			t.Fatalf("the denied command gave %v, want ErrDenied", err)
		}
		if calls != 0 {
			t.Fatalf("the denied command called the fill %d times", calls)
		}
		if n := e.keyRows("k-lc31-denied"); n != 0 {
			t.Fatalf("the denied command wrote %d key rows", n)
		}
		if n := e.count(`SELECT count(*) FROM reservations`); n != 0 {
			t.Fatalf("the denied command wrote %d reservations", n)
		}
		taken := e.decision.taken()
		if len(taken) != 1 || taken[0].Subject != "user:u-9" || taken[0].Action != "reserve" || taken[0].Resource != "order:o5" {
			t.Fatalf("the denied command asked the port %+v, want one decision of reserve on order:o5", taken)
		}

		allowed := e.request("t1", u7(t), "k-lc31-allowed", `{}`)
		allowed.Resource = "o5"
		if _, err := e.run(cmd, allowed, fill); err != nil {
			t.Fatalf("the allowed command gave %v", err)
		}
		if calls != 1 {
			t.Fatalf("the allowed command called the fill %d times, want once", calls)
		}
		if n := e.keyRows("k-lc31-allowed"); n != 1 {
			t.Fatalf("the allowed command wrote %d key rows, want one", n)
		}
		taken = e.decision.taken()
		if len(taken) != 1 {
			t.Fatalf("the allowed command asked the port %d times, want once: %+v", len(taken), taken)
		}
		token, isToken := taken[0].Consistency.Token()
		if taken[0].Subject != "user:u-7" || taken[0].Resource != "order:o5" || !isToken || token.Text() != "t5" {
			t.Fatalf("the allowed command was decided as %+v, want user:u-7 on order:o5 at least as fresh as t5", taken[0])
		}

		// The permission is revoked: a replay of the completed key is decided too, before the wrapper reads the
		// key, so the stored response is not given.
		e.decision.setAllow(func(string, string, string) bool { return false })
		replay := e.request("t1", u7(t), "k-lc31-allowed", `{}`)
		replay.Resource = "o5"
		if result, err := e.run(cmd, replay, fill); !errors.Is(err, ErrDenied) || result.Replayed || calls != 1 {
			t.Fatalf("a revoked replay gave %+v, %v with %d fill calls, want ErrDenied and no stored response", result, err, calls)
		}
		// A missing order is decided on a zero token, and the provider, which holds no relation of it, denies it: the
		// same answer as a denied one.
		missing := e.request("t1", u7(t), "k-lc31-missing", `{}`)
		missing.Resource = "o404"
		e.decision.taken()
		if _, err := e.run(cmd, missing, fill); !errors.Is(err, ErrDenied) || calls != 1 {
			t.Fatalf("a missing order gave %v with %d fill calls, want ErrDenied", err, calls)
		}
		if taken := e.decision.taken(); len(taken) != 1 || taken[0].Resource != "order:o404" {
			t.Fatalf("a missing order asked the port %+v, want one decision", taken)
		}
		if _, err := NewCommand(CommandSpec[testProbe]{Name: "half", Probe: newTestProbe, Permission: "reserve"}); err == nil {
			t.Fatal("a command with a permission and no resource was accepted")
		}
	})
}
