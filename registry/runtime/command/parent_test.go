package command

import (
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/registry/runtime/authz"
)

// The entities of the reference test: an order that refers to a customer and a product, and a node of a tree whose
// parent is optional (ST-DECL-EN02).
type (
	product     struct{ ID, Name string }
	placedOrder struct{ ID, CustomerID, ProductID string }
	node        struct{ ID, ParentID string }
)

const referenceDDL = `
CREATE TABLE products (tenant_id text NOT NULL, id text NOT NULL, name text NOT NULL,
  version bigint NOT NULL DEFAULT 1, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id));
CREATE TABLE placed_orders (tenant_id text NOT NULL, id text NOT NULL, customer_id text NOT NULL, product_id text NOT NULL,
  version bigint NOT NULL DEFAULT 1, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id));
CREATE TABLE nodes (tenant_id text NOT NULL, id text NOT NULL, parent_id text NOT NULL,
  version bigint NOT NULL DEFAULT 1, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id));
`

// referenceEntities makes the tables and the entities of the reference test: PlacedOrder refers to Customer by
// customer_id with the permission place_order and the relation customer, and to Product by product_id with the
// permission view and the relation product; Node refers to its optional parent Node by parent_id with the permission
// add_child and the relation parent.
func referenceEntities(e *env) (*Entity[product], *Entity[placedOrder], *Entity[node]) {
	t := e.t
	t.Helper()
	e.exec(referenceDDL)
	for _, table := range []string{"products", "placed_orders", "nodes"} {
		e.exec("ALTER TABLE "+table+" ENABLE ROW LEVEL SECURITY", "ALTER TABLE "+table+" FORCE ROW LEVEL SECURITY",
			"CREATE POLICY "+table+"_tenant ON "+table+" USING (tenant_id = current_setting('app.tenant', true)) WITH CHECK (tenant_id = current_setting('app.tenant', true))",
			"GRANT SELECT, INSERT, UPDATE, DELETE ON "+table+" TO "+e.roles[0])
	}
	products, err := NewEntity(spec("Product", "product", "products", []string{"name"},
		func(s Scanner) (product, Meta, error) {
			var v product
			var m Meta
			err := s.Scan(&v.ID, &v.Name, &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		func(v product) []any { return []any{v.ID, v.Name} },
		func(v product) map[string]any { return map[string]any{"id": v.ID, "name": v.Name} }))
	if err != nil {
		t.Fatal(err)
	}
	orderSpec := spec("PlacedOrder", "order", "placed_orders", []string{"customer_id", "product_id"},
		func(s Scanner) (placedOrder, Meta, error) {
			var v placedOrder
			var m Meta
			err := s.Scan(&v.ID, &v.CustomerID, &v.ProductID, &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		func(v placedOrder) []any { return []any{v.ID, v.CustomerID, v.ProductID} },
		func(v placedOrder) map[string]any {
			return map[string]any{"id": v.ID, "customer_id": v.CustomerID, "product_id": v.ProductID}
		})
	orderSpec.Parent = []Reference[placedOrder]{
		{Entity: "Customer", ResourceType: "customer", ID: func(v placedOrder) string { return v.CustomerID },
			Permission: "place_order", Relation: "customer"},
		{Entity: "Product", ResourceType: "product", ID: func(v placedOrder) string { return v.ProductID },
			Permission: "view", Relation: "product"},
	}
	orders, err := NewEntity(orderSpec)
	if err != nil {
		t.Fatal(err)
	}
	nodeSpec := spec("Node", "node", "nodes", []string{"parent_id"},
		func(s Scanner) (node, Meta, error) {
			var v node
			var m Meta
			err := s.Scan(&v.ID, &v.ParentID, &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		func(v node) []any { return []any{v.ID, v.ParentID} },
		func(v node) map[string]any { return map[string]any{"id": v.ID, "parent_id": v.ParentID} })
	nodeSpec.Parent = []Reference[node]{{Entity: "Node", ResourceType: "node", ID: func(v node) string { return v.ParentID },
		Permission: "add_child", Relation: "parent"}}
	nodes, err := NewEntity(nodeSpec)
	if err != nil {
		t.Fatal(err)
	}
	return products, orders, nodes
}

// tuples gives the relationship touches and deletes that the outbox holds for the relay in tenant t1, each as
// op resource_type#relation@subject_type:subject_key with the tenant part of each ID dropped, in the order of the
// outbox.
func (e *env) tuples() []string {
	e.t.Helper()
	rows, err := e.admin.QueryContext(e.ctx, `SELECT p->>'op', p->'tuple'->>'resource_type', p->'tuple'->>'resource_id',
  p->'tuple'->>'relation', p->'tuple'->>'subject_type', p->'tuple'->>'subject_id'
 FROM (SELECT convert_from(payload, 'UTF8')::jsonb AS p, created_at, id FROM stego_outbox.messages
 WHERE tenant_id = 't1' AND destination = 'authz') m ORDER BY created_at, id`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var op, resourceType, resourceID, relation, subjectType, subjectID string
		if err := rows.Scan(&op, &resourceType, &resourceID, &relation, &subjectType, &subjectID); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, op+" "+resourceType+":"+keyOf(resourceID)+"#"+relation+"@"+subjectType+":"+keyOf(subjectID))
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// keyOf is the key of a tenant-qualified resource ID of the tenant t1, or the ID with a mark when it is not one.
func keyOf(id string) string {
	tenantPart, keyPart, ok := strings.Cut(id, "/")
	tenantID, err1 := base64.RawURLEncoding.DecodeString(tenantPart)
	key, err2 := base64.RawURLEncoding.DecodeString(keyPart)
	if !ok || err1 != nil || err2 != nil || string(tenantID) != "t1" {
		return "UNQUALIFIED(" + id + ")"
	}
	return string(key)
}

func TestReferences(t *testing.T) {
	t.Run("ST-SUP-RP10-LC05-T12 A create decides the declared permission of each set reference and writes its relation", func(t *testing.T) {
		e := newEnv(t)
		products, orders, nodes := referenceEntities(e)
		e.exec(`INSERT INTO customers (tenant_id, id, name, email) VALUES ('t1', 'c1', 'C1', 'c1@example.com'), ('t1', 'c2', 'C2', 'c2@example.com')`,
			`INSERT INTO products (tenant_id, id, name) VALUES ('t1', 'p1', 'P1')`,
			`INSERT INTO nodes (tenant_id, id, parent_id) VALUES ('t1', 'n0', '')`)
		allowAll := []string{
			relation(t, "user:u-7", "view", "t1", "customer", "c1"), relation(t, "user:u-7", "view", "t1", "customer", "c2"),
			relation(t, "user:u-7", "view", "t1", "product", "p1"), relation(t, "user:u-7", "view", "t1", "order", "po1"),
			relation(t, "user:u-7", "place_order", "t1", "customer", "c1"), relation(t, "user:u-7", "place_order", "t1", "customer", "c2"),
			relation(t, "user:u-7", "place", "t1", "order", "po1"),
		}
		place := e.command("place", func(s *CommandSpec[testProbe]) {
			s.Permission, s.Resource = "place", &Resource{Type: "service", Service: "orders"}
		}, orders, e.customerE, products, nodes)
		run := func(key string, fill func(ctx context.Context, h *Handle[testProbe]) error) error {
			_, err := e.run(place, e.request("t1", u7(t), key, `{}`), fillOf(fill))
			return err
		}
		create := func(id, customerID string) func(ctx context.Context, h *Handle[testProbe]) error {
			return func(ctx context.Context, h *Handle[testProbe]) error {
				c, err := repo(t, h, e.customerE).Get(ctx, customerID)
				if err != nil {
					return err
				}
				p, err := repo(t, h, products).Get(ctx, "p1")
				if err != nil {
					return err
				}
				_, err = repo(t, h, orders).Create(ctx, placedOrder{ID: id, CustomerID: customerID, ProductID: "p1"}, c, p)
				return err
			}
		}

		// The first create: place on the service, place_order on customer:c1 and view on product:p1.
		e.decision.setRelations(append(allowAll, relation(t, "user:u-7", "place", "t1", "service", "orders"))...)
		before := e.tuples()
		e.decision.taken()
		if err := run("k-lc05-t12-a", create("po1", "c1")); err != nil {
			t.Fatal(err)
		}
		var decided []string
		for _, d := range e.decision.taken() {
			decided = append(decided, d.Action+" "+d.Resource)
		}
		for _, want := range []string{"place service:orders", "place_order customer:c1", "view product:p1"} {
			if !slices.Contains(decided, want) {
				t.Fatalf("the create was decided as %v, want %s among them", decided, want)
			}
		}
		if e.count(`SELECT count(*) FROM placed_orders WHERE id = 'po1'`) != 1 {
			t.Fatal("the create did not commit")
		}
		added := e.tuples()[len(before):]
		if !slices.Equal(added, []string{"touch order:po1#customer@customer:c1", "touch order:po1#product@product:p1"}) {
			t.Fatalf("the create enqueued the tuples %v, want order#customer@customer:c1 and order#product@product:p1", added)
		}

		// The second create: place_order is denied on c1, so the create is not found and writes nothing.
		denied := slices.DeleteFunc(slices.Clone(allowAll), func(r string) bool {
			return r == relation(t, "user:u-7", "place_order", "t1", "customer", "c1")
		})
		e.decision.setRelations(append(denied, relation(t, "user:u-7", "place", "t1", "service", "orders"))...)
		before = e.tuples()
		if err := run("k-lc05-t12-b", create("po2", "c1")); !errors.Is(err, ErrNotFound) {
			t.Fatalf("the create with place_order denied on c1 gave %v, want not found", err)
		}
		if e.count(`SELECT count(*) FROM placed_orders WHERE id = 'po2'`) != 0 || len(e.tuples()) != len(before) ||
			e.keyRows("k-lc05-t12-b") != 0 {
			t.Fatal("the denied create left a row, a tuple or a key row")
		}

		// A tree row whose optional parent is not set: no reference is decided and no relation is written.
		e.decision.setRelations(append(allowAll, relation(t, "user:u-7", "place", "t1", "service", "orders"))...)
		before = e.tuples()
		e.decision.taken()
		if err := run("k-lc05-t12-c", func(ctx context.Context, h *Handle[testProbe]) error {
			_, err := repo(t, h, nodes).Create(ctx, node{ID: "n1"}, nil)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if taken := e.decision.taken(); len(taken) != 1 || taken[0].Resource != "service:orders" {
			t.Fatalf("the tree row was decided as %+v, want only the command's decision on service:orders", taken)
		}
		if len(e.tuples()) != len(before) {
			t.Fatal("the tree row with no parent wrote a relation")
		}

		// The move of po1 to Customer c2: place_order on c2, and the relation moves.
		e.decision.setRelations(append(allowAll, relation(t, "user:u-7", "place", "t1", "service", "orders"))...)
		before = e.tuples()
		e.decision.taken()
		if err := run("k-lc05-t12-d", func(ctx context.Context, h *Handle[testProbe]) error {
			o, err := repo(t, h, orders).GetForUpdate(ctx, "po1")
			if err != nil {
				return err
			}
			c2, err := repo(t, h, e.customerE).Get(ctx, "c2")
			if err != nil {
				return err
			}
			v := o.Value()
			v.CustomerID = "c2"
			_, err = repo(t, h, orders).Update(ctx, o, v, c2)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		decided = nil
		for _, d := range e.decision.taken() {
			decided = append(decided, d.Action+" "+d.Resource)
		}
		if !slices.Contains(decided, "place_order customer:c2") {
			t.Fatalf("the move was decided as %v, want place_order on customer:c2", decided)
		}
		if moved := e.tuples()[len(before):]; !slices.Equal(moved, []string{"delete order:po1#customer@customer:c1", "touch order:po1#customer@customer:c2"}) {
			t.Fatalf("the move enqueued %v, want the delete of the old relation and the touch of the new one", moved)
		}

		// A fill cannot write a relation of a reference itself: only the runtime does.
		if err := run("k-lc05-t12-e", func(ctx context.Context, h *Handle[testProbe]) error {
			o, err := repo(t, h, orders).GetForUpdate(ctx, "po1")
			if err != nil {
				return err
			}
			if _, err := h.Authz().Relate(ctx, o, orderTuple(t, "po1", "customer", "c1")); err == nil {
				t.Error("the fill wrote the relation of a reference")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}

// orderTuple is the tuple order:<id>#<relation>@customer:<customerID> of the tenant t1.
func orderTuple(t *testing.T, id, relation, customerID string) authz.Tuple {
	t.Helper()
	r, err := authz.NewResource(mustTenant(t, "t1"), "order", id)
	if err != nil {
		t.Fatal(err)
	}
	subject, err := authz.ResourceID(mustTenant(t, "t1"), customerID)
	if err != nil {
		t.Fatal(err)
	}
	return authz.Tuple{Resource: r, Relation: relation, Subject: authz.Subject{Type: "customer", ID: subject}}
}
