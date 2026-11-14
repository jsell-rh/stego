package command

import (
	"context"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/registry/runtime/authz"
)

// TestRuntimeRelations proves that a command which declares the relation of a reference or of a declared
// relationship of an entity it writes gives its fill no way to write it: only the runtime writes those relations
// (ST-SUP-Q118; security review of PR 288, M1, defence in depth behind the generators).
func TestRuntimeRelations(t *testing.T) {
	t.Run("ST-SUP-RP10-LC05-T23 A fill cannot relate or unrelate the relation of a reference that its command declares", func(t *testing.T) {
		e := newEnv(t)
		products, orders, nodes := referenceEntities(e)
		e.exec(`INSERT INTO customers (tenant_id, id, name, email) VALUES ('t1', 'c1', 'C1', 'c1@example.com'), ('t1', 'c2', 'C2', 'c2@example.com')`,
			`INSERT INTO products (tenant_id, id, name) VALUES ('t1', 'p1', 'P1')`)
		e.decision.setRelations(
			relation(t, "user:u-7", "view", "t1", "customer", "c1"), relation(t, "user:u-7", "view", "t1", "product", "p1"),
			relation(t, "user:u-7", "view", "t1", "order", "po1"), relation(t, "user:u-7", "place_order", "t1", "customer", "c1"),
			relation(t, "user:u-7", "place", "t1", "order", "po1"), relation(t, "user:u-7", "place", "t1", "service", "orders"))
		// u-7 has no view and no place_order on customer:c2.
		place := e.command("place", func(s *CommandSpec[testProbe]) {
			s.Permission, s.Resource = "place", &Resource{Type: "service", Service: "orders"}
			s.Relations = []authz.Relation{{ResourceType: "order", Name: "customer"}}
		}, orders, e.customerE, products, nodes)
		run := func(key string, fill func(ctx context.Context, h *Handle[testProbe]) error) error {
			_, err := e.run(place, e.request("t1", u7(t), key, `{}`), fillOf(fill))
			return err
		}
		if err := run("k-a", func(ctx context.Context, h *Handle[testProbe]) error {
			c, err := repo(t, h, e.customerE).Get(ctx, "c1")
			if err != nil {
				return err
			}
			p, err := repo(t, h, products).Get(ctx, "p1")
			if err != nil {
				return err
			}
			_, err = repo(t, h, orders).Create(ctx, placedOrder{ID: "po1", CustomerID: "c1", ProductID: "p1"}, c, p)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		before := e.tuples()
		err := run("k-b", func(ctx context.Context, h *Handle[testProbe]) error {
			o, err := repo(t, h, orders).GetForUpdate(ctx, "po1")
			if err != nil {
				return err
			}
			if _, err := h.Authz().Unrelate(ctx, o, orderTuple(t, "po1", "customer", "c1")); err != nil {
				return err
			}
			_, err = h.Authz().Relate(ctx, o, orderTuple(t, "po1", "customer", "c2"))
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "does not declare the relation order#customer") {
			t.Errorf("a fill that moves the reference relation to c2 gave %v, want a refusal that names order#customer", err)
		}
		if after := e.tuples(); len(after) != len(before) {
			t.Errorf("the refused fill enqueued %v", after[len(before):])
		}
	})
	t.Run("ST-SUP-RP10-LC05-T23 A fill cannot relate or unrelate the relation of a declared relationship that its command declares", func(t *testing.T) {
		e := newEnv(t)
		_, orders, _ := referenceEntities(e)
		s := orders.spec
		s.Parent = nil
		s.Relationships = []Relationship[placedOrder]{{Relation: "creator", SubjectType: "user"},
			{Relation: "owner", SubjectType: "user", Field: func(v placedOrder) string { return v.CustomerID }, FieldName: "customer_id"}}
		owned, err := NewEntity(s)
		if err != nil {
			t.Fatal(err)
		}
		e.decision.setRelations(relation(t, "user:u-7", "place", "t1", "order", "o1"), relation(t, "user:u-7", "view", "t1", "order", "o1"),
			relation(t, "user:u-7", "place", "t1", "service", "orders"))
		make := e.command("make", func(s *CommandSpec[testProbe]) {
			s.Permission, s.Resource = "place", &Resource{Type: "service", Service: "orders"}
			s.Relations = []authz.Relation{{ResourceType: "order", Name: "owner"}, {ResourceType: "order", Name: "creator"}}
		}, owned)
		if _, err := e.run(make, e.request("t1", u7(t), "k-c", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			_, err := repo(t, h, owned).Create(ctx, placedOrder{ID: "o1", CustomerID: "u-1", ProductID: "p1"})
			return err
		})); err != nil {
			t.Fatal(err)
		}
		before := e.tuples()
		res, _ := authz.NewResource(mustTenant(t, "t1"), "order", "o1")
		_, err = e.run(make, e.request("t1", u7(t), "k-d", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			o, err := repo(t, h, owned).GetForUpdate(ctx, "o1")
			if err != nil {
				return err
			}
			if _, err := h.Authz().Unrelate(ctx, o, authz.Tuple{Resource: res, Relation: "owner", Subject: authz.Subject{Type: "user", ID: "u-1"}}); err != nil {
				return err
			}
			if _, err := h.Authz().Unrelate(ctx, o, authz.Tuple{Resource: res, Relation: "creator", Subject: authz.Subject{Type: "user", ID: "u-7"}}); err != nil {
				return err
			}
			_, err = h.Authz().Relate(ctx, o, authz.Tuple{Resource: res, Relation: "owner", Subject: authz.Subject{Type: "user", ID: "u-9"}})
			return err
		}))
		if err == nil || !strings.Contains(err.Error(), "does not declare the relation order#owner") {
			t.Errorf("a fill that moves the field owner gave %v, want a refusal that names order#owner", err)
		}
		if after := e.tuples(); len(after) != len(before) {
			t.Errorf("the refused fill enqueued %v", after[len(before):])
		}
		// The caller relationship is the runtime's too: a fill that unrelates the creator alone is refused.
		_, err = e.run(make, e.request("t1", u7(t), "k-e", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			o, err := repo(t, h, owned).GetForUpdate(ctx, "o1")
			if err != nil {
				return err
			}
			_, err = h.Authz().Unrelate(ctx, o, authz.Tuple{Resource: res, Relation: "creator", Subject: authz.Subject{Type: "user", ID: "u-7"}})
			return err
		}))
		if err == nil || !strings.Contains(err.Error(), "does not declare the relation order#creator") {
			t.Errorf("a fill that unrelates the caller relationship gave %v, want a refusal that names order#creator", err)
		}
		if after := e.tuples(); len(after) != len(before) {
			t.Errorf("the refused fills enqueued %v", after[len(before):])
		}
	})
}
