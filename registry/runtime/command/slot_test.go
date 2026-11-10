package command

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSlotLoaded(t *testing.T) {
	t.Run("ST-SUP-RP10-LC32-T01 Only slot code makes a loaded value from a request", func(t *testing.T) {
		e := newEnv(t)
		request := reservation{ID: "r-new", OrderID: "o1", SKU: "A-1"}
		l, err := FromRequest(e.rt.SlotKey(), e.reservationE, request)
		if err != nil {
			t.Fatal(err)
		}
		if got := l.Value(); got != request {
			t.Fatalf("the loaded value holds %+v, want the request's fields %+v", got, request)
		}
		request.SKU = "changed"
		if got := l.Value().SKU; got != "A-1" {
			t.Fatalf("a change to the request entity changed the loaded value to %q", got)
		}
		if _, err := FromRequest((*Runtime)(nil).SlotKey(), e.reservationE, request); !errors.Is(err, ErrNoSlotKey) {
			t.Fatalf("the key of no runtime gave %v, want ErrNoSlotKey", err)
		}
		if _, err := FromRequest(new(Runtime).SlotKey(), e.reservationE, request); !errors.Is(err, ErrNoSlotKey) {
			t.Fatalf("the key of a runtime that New did not make gave %v, want ErrNoSlotKey", err)
		}
		// An entity that a fill or another module builds with NewEntity is not an entity of this runtime, even with
		// the same declaration.
		outside, err := NewEntity(e.reservationE.spec)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := FromRequest(e.rt.SlotKey(), outside, request); !errors.Is(err, ErrNotRegistered) {
			t.Fatalf("an entity that the runtime did not register gave %v, want ErrNotRegistered", err)
		}
		// An entity registers with one runtime only.
		other := e.rt.cfg
		other.Entities = []Declared{e.reservationE}
		if _, err := New(e.ctx, other); err == nil {
			t.Fatal("a second runtime registered an entity of the first")
		}
		var zero slotKey
		if _, err := FromRequest(zero, e.reservationE, request); !errors.Is(err, ErrNoSlotKey) {
			t.Fatalf("the zero key gave %v, want ErrNoSlotKey", err)
		}

		// No handle takes the value as a row it read: not for a write, an effect or a decision.
		e.exec(`INSERT INTO orders (tenant_id, id, status, authz_token) VALUES ('t1', 'o1', 'open', 't1')`)
		cmd := e.command("reserve", nil, e.reservationE, e.orders)
		h, _, end := e.bind("t1", u7(t), cmd)
		if _, err := h.Authz().Evaluate(e.ctx, "reserve", l, nil); !errors.Is(err, ErrNoResource) {
			t.Fatalf("a decision on the request's loaded value gave %v, want ErrNoResource", err)
		}
		if err := h.Effects().Stage(e.ctx, "billing", "reserved", l, []byte(`{}`)); !errors.Is(err, ErrInternal) {
			t.Fatalf("an effect for the request's loaded value gave %v, want an internal error", err)
		}
		end()
		h, _, end = e.bind("t1", u7(t), cmd)
		defer end()
		if _, err := repo(t, h, e.reservationE).Update(context.Background(), l, l.Value()); !errors.Is(err, ErrInternal) {
			t.Fatalf("an update from the request's loaded value gave %v, want an internal error", err)
		}

		// A fill can neither call FromRequest nor build a loaded value in a way that compiles; the generated slot
		// code of the fixture module, which holds the runtime, compiles.
		entity := "(*command.Entity[gen.Reservation])(nil)"
		for name, c := range map[string]struct {
			imports   []string
			src, want string
		}{
			"a literal of the key":       {nil, "\nvar _, _ = command.FromRequest(command.slotKey{}, " + entity + ", gen.Reservation{})\n", "slotKey"},
			"a nil key":                  {nil, "\nvar _, _ = command.FromRequest(nil, " + entity + ", gen.Reservation{})\n", "cannot use nil"},
			"a variable of the key type": {nil, "\nvar k command.slotKey\n", "slotKey"},
			"the loaded constructor": {[]string{"github.com/jsell-rh/stego/registry/runtime/internal/loaded"},
				"\nvar _ = loaded.New(gen.Reservation{}, nil, loaded.Ref{})\n", "use of internal package"},
			"a composite literal": {nil, "\nvar _ = gen.LoadedReservation{}\n", "invalid composite literal type"},
		} {
			err := buildFillWith(t, c.imports, c.src)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s in a fill gave %v, want a compile error with %q", name, err, c.want)
			}
		}
		if err := buildFill(t, "\nfunc sku(r gen.LoadedReservation) string { return r.Value().SKU }\nvar _ = gen.Slots.BeforeCreate\n"); err != nil {
			t.Fatalf("the control fill and the generated slot code do not compile: %v", err)
		}
	})
}
