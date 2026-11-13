package command

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// relationshipEntity is the order entity of the reference tests with two declared relationships and no reference:
// owner from the caller, a user, and buyer from the field customer_id, a customer of this service.
func relationshipEntity(e *env, callerType string) *Entity[placedOrder] {
	e.t.Helper()
	_, orders, _ := referenceEntities(e)
	s := orders.spec
	s.Parent = nil
	s.Relationships = []Relationship[placedOrder]{{Relation: "owner", SubjectType: callerType},
		{Relation: "buyer", SubjectType: "customer", Field: func(v placedOrder) string { return v.CustomerID }, FieldName: "customer_id", Resource: true}}
	entity, err := NewEntity(s)
	if err != nil {
		e.t.Fatal(err)
	}
	return entity
}

func TestDeclaredRelationships(t *testing.T) {
	t.Run("ST-SUP-RP10-LC05-T13 The runtime writes the declared relationships of a row on create, update and delete", func(t *testing.T) {
		e := newEnv(t)
		orders := relationshipEntity(e, "user")
		// A command with a permission: one with none may not set a field subject (ST-SUP-RP10-LC05-T20).
		make := placeCommand(e, orders)
		run := func(key string, fill func(ctx context.Context, h *Handle[testProbe]) error) error {
			_, err := e.run(make, e.request("t1", u7(t), key, `{}`), fillOf(fill))
			return err
		}
		before := e.tuples()
		if err := run("k-t13-create", func(ctx context.Context, h *Handle[testProbe]) error {
			_, err := repo(t, h, orders).Create(ctx, placedOrder{ID: "r1", CustomerID: "c1", ProductID: "p1"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		added := e.tuples()[len(before):]
		if want := []string{"touch order:r1#owner@user:UNQUALIFIED(u-7)", "touch order:r1#buyer@customer:c1"}; !slices.Equal(added, want) {
			t.Fatalf("the create enqueued %v, want %v", added, want)
		}
		// An update that keeps the subject writes no tuple; one that changes it is refused (ST-SUP-RP10-LC05-T19).
		before = e.tuples()
		if err := run("k-t13-same", func(ctx context.Context, h *Handle[testProbe]) error {
			r := repo(t, h, orders)
			from, err := r.GetForUpdate(ctx, "r1")
			if err != nil {
				return err
			}
			_, err = r.Update(ctx, from, placedOrder{ID: "r1", CustomerID: "c1", ProductID: "p2"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if added = e.tuples()[len(before):]; len(added) != 0 {
			t.Fatalf("an update that keeps the subject enqueued %v, want nothing", added)
		}
		// A delete deletes the field-sourced tuple.
		before = e.tuples()
		if err := run("k-t13-delete", func(ctx context.Context, h *Handle[testProbe]) error {
			r := repo(t, h, orders)
			from, err := r.GetForDelete(ctx, "r1")
			if err != nil {
				return err
			}
			return r.Delete(ctx, from)
		}); err != nil {
			t.Fatal(err)
		}
		added = e.tuples()[len(before):]
		if want := []string{"delete order:r1#buyer@customer:c1"}; !slices.Equal(added, want) {
			t.Fatalf("the delete enqueued %v, want %v", added, want)
		}
	})
	t.Run("ST-SUP-RP10-LC05-T14 A create whose caller is not of the relationship's subject type is refused and writes nothing", func(t *testing.T) {
		e := newEnv(t)
		orders := relationshipEntity(e, "group")
		make := placeCommand(e, orders)
		before := e.tuples()
		_, err := e.run(make, e.request("t1", u7(t), "k-t14", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			_, err := repo(t, h, orders).Create(ctx, placedOrder{ID: "r2", CustomerID: "c1", ProductID: "p1"})
			return err
		}))
		if err == nil || !strings.Contains(err.Error(), "the relationship owner takes a group") {
			t.Fatalf("a create by a user for a group relationship gave %v, want the refusal", err)
		}
		if e.count(`SELECT count(*) FROM placed_orders WHERE id = 'r2'`) != 0 || len(e.tuples()) != len(before) {
			t.Fatal("the refused create wrote a row or a tuple")
		}
	})
	t.Run("ST-SUP-RP10-LC05-T19 Neither a caller with no permission nor an editor can change a field subject", func(t *testing.T) {
		e := newEnv(t)
		orders := relationshipEntity(e, "user")
		if _, err := e.run(placeCommand(e, orders), e.request("t1", u7(t), "k-t19-create", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			_, err := repo(t, h, orders).Create(ctx, placedOrder{ID: "m1", CustomerID: "c1", ProductID: "p1"})
			return err
		})); err != nil {
			t.Fatal(err)
		}
		// The editor may edit the row: its command declares edit on the order, which the fake port allows.
		editor := e.command("edit", func(s *CommandSpec[testProbe]) {
			s.Permission, s.Resource = "edit", placedOrderResource()
		}, orders)
		for _, c := range []struct {
			name string
			cmd  *Command[testProbe]
		}{{"no permission", e.command("public-edit", nil, orders)}, {"editor", editor}} {
			before := e.tuples()
			req := e.request("t1", person(t, "user:u-9"), "k-t19-"+c.name, `{}`)
			req.Resource = "m1"
			calls := 0
			_, err := e.run(c.cmd, req, fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
				r := repo(t, h, orders)
				from, err := r.GetForUpdate(ctx, "m1")
				if err != nil {
					return err
				}
				// The fill drops the refusal and writes again: the handle stays broken.
				_, _ = r.Update(ctx, from, placedOrder{ID: "m1", CustomerID: "c9", ProductID: "p1"})
				if _, err := r.Update(ctx, from, placedOrder{ID: "m1", CustomerID: "c1", ProductID: "p2"}); err != nil {
					calls++
				}
				return nil
			}))
			o := OutcomeOf(err)
			if !errors.Is(err, ErrInvalid) || o.Status != http.StatusBadRequest || o.Code != ErrorCodeInvalid || o.Field != "customer_id" || calls != 1 {
				t.Fatalf("%s: the move of customer_id gave %v (%+v, later write refused %d), want ErrInvalid, 400 invalid naming customer_id", c.name, err, o, calls)
			}
			var subject, product string
			if err := e.admin.QueryRowContext(e.ctx, `SELECT customer_id, product_id FROM placed_orders WHERE id = 'm1'`).Scan(&subject, &product); err != nil {
				t.Fatal(err)
			}
			if subject != "c1" || product != "p1" || len(e.tuples()) != len(before) {
				t.Fatalf("%s: the refused move left %s/%s and the tuples %v", c.name, subject, product, e.tuples()[len(before):])
			}
		}
	})
	t.Run("ST-SUP-RP10-LC05-T20 A refused relationship that the fill drops commits nothing", func(t *testing.T) {
		e := newEnv(t)
		orders := ownerEntity(e)
		before := e.tuples()
		audits := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = 't1'`)
		for _, c := range []struct {
			name, subject string
			cmd           *Command[testProbe]
			want          error
		}{
			{"a hostile subject", "*", placeCommand(e, orders), ErrInvalid},
			{"a field subject in a command that declares no permission", "u-2", e.command("public-make", nil, orders), ErrInternal},
		} {
			_, err := e.run(c.cmd, e.request("t1", u7(t), "k-t20-"+c.name, `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
				r := repo(t, h, orders)
				_, _ = r.Create(ctx, placedOrder{ID: "d1", CustomerID: c.subject, ProductID: "p1"})
				_, _ = r.Create(ctx, placedOrder{ID: "d2", ProductID: "p1"})
				return nil
			}))
			if !errors.Is(err, c.want) {
				t.Fatalf("%s: the run gave %v, want %v", c.name, err, c.want)
			}
		}
		if n := e.count(`SELECT count(*) FROM placed_orders`); n != 0 {
			t.Fatalf("the runs committed %d rows", n)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = 't1'`); n != audits || len(e.tuples()) != len(before) {
			t.Fatalf("the runs committed %d audit rows and the tuples %v", n-audits, e.tuples()[len(before):])
		}
		// A failure of the authorization handle after the row statement breaks the handle too: here the handle is
		// bound with no relation, so the touch of the owner tuple fails after the insert.
		cmd := placeCommand(e, orders)
		h, tx, end := e.bind("t1", u7(t), cmd)
		defer end()
		var err error
		if h.st.az, err = e.authz.Bind(tx, mustTenant(t, "t1"), u7(t), nil); err != nil {
			t.Fatal(err)
		}
		h.st.commandPermission = cmd.spec.Permission
		r := repo(t, h, orders)
		if _, err := r.Create(e.ctx, placedOrder{ID: "d3", CustomerID: "u-2", ProductID: "p1"}); !errors.Is(err, ErrInternal) || !strings.Contains(err.Error(), "was not enqueued") {
			t.Fatalf("a create whose tuple was not enqueued gave %v, want an internal error", err)
		}
		if _, err := r.Create(e.ctx, placedOrder{ID: "d4", ProductID: "p1"}); !errors.Is(err, ErrInternal) || !errors.Is(h.st.broken, ErrInternal) {
			t.Fatalf("a write after a tuple that was not enqueued gave %v, want the handle broken", err)
		}
	})
	t.Run("ST-SUP-RP10-LC05-T21 A hostile field subject is refused with 400 before the row is written", func(t *testing.T) {
		e := newEnv(t)
		orders := ownerEntity(e)
		other := base64.RawURLEncoding.EncodeToString([]byte("t2")) + "/" + base64.RawURLEncoding.EncodeToString([]byte("u-9"))
		before := e.tuples()
		for i, subject := range []string{"*", "user:*", other, "a b", "u-9#member", "\u00e9", strings.Repeat("u", 1100)} {
			_, err := e.run(placeCommand(e, orders), e.request("t1", u7(t), "k-t21-"+string(rune('a'+i)), `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
				_, err := repo(t, h, orders).Create(ctx, placedOrder{ID: "h" + string(rune('a'+i)), CustomerID: subject, ProductID: "p1"})
				return err
			}))
			if o := OutcomeOf(err); o.Status != http.StatusBadRequest || o.Code != ErrorCodeInvalid || o.Field != "customer_id" || strings.Contains(err.Error(), subject) {
				t.Fatalf("the subject %.20q gave %v (%+v), want 400 invalid naming customer_id and not the value", subject, err, o)
			}
		}
		if n := e.count(`SELECT count(*) FROM placed_orders`); n != 0 || len(e.tuples()) != len(before) {
			t.Fatalf("the refused creates wrote %d rows and the tuples %v", n, e.tuples()[len(before):])
		}
	})
}

// ownerEntity is the order entity of the reference tests with no Parent and one relationship, owner from the field
// customer_id, a user (not a resource type of this service).
func ownerEntity(e *env) *Entity[placedOrder] {
	e.t.Helper()
	_, orders, _ := referenceEntities(e)
	s := orders.spec
	s.Parent = nil
	s.Relationships = []Relationship[placedOrder]{{Relation: "owner", SubjectType: "user",
		Field: func(v placedOrder) string { return v.CustomerID }, FieldName: "customer_id"}}
	entity, err := NewEntity(s)
	if err != nil {
		e.t.Fatal(err)
	}
	return entity
}

// placeCommand is a command that writes orders and declares the permission place on the service-level resource,
// which the fake port allows.
func placeCommand(e *env, orders *Entity[placedOrder]) *Command[testProbe] {
	e.t.Helper()
	return e.command("place", func(s *CommandSpec[testProbe]) {
		s.Permission, s.Resource = "place", &Resource{Type: "service", Service: "orders"}
	}, orders)
}

// placedOrderResource is the resource of a command on one placed order.
func placedOrderResource() *Resource {
	return &Resource{Type: "order", Token: `SELECT authz_token FROM placed_orders WHERE tenant_id = $1 AND id = $2 FOR SHARE`,
		ValidID: func(id string) bool { return id != "" }}
}
