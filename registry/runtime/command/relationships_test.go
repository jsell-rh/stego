package command

import (
	"context"
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
		{Relation: "buyer", SubjectType: "customer", Field: func(v placedOrder) string { return v.CustomerID }, Resource: true}}
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
		// A command with no permission, as a public create is: a fill could relate nothing in it.
		make := e.command("make", nil, orders)
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
		// An update that moves the field subject moves its tuple, and leaves the caller's as the create wrote it.
		before = e.tuples()
		if err := run("k-t13-update", func(ctx context.Context, h *Handle[testProbe]) error {
			r := repo(t, h, orders)
			from, err := r.GetForUpdate(ctx, "r1")
			if err != nil {
				return err
			}
			_, err = r.Update(ctx, from, placedOrder{ID: "r1", CustomerID: "c2", ProductID: "p1"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		added = e.tuples()[len(before):]
		if want := []string{"delete order:r1#buyer@customer:c1", "touch order:r1#buyer@customer:c2"}; !slices.Equal(added, want) {
			t.Fatalf("the update enqueued %v, want %v", added, want)
		}
		// An update that keeps the subject writes no tuple.
		before = e.tuples()
		if err := run("k-t13-same", func(ctx context.Context, h *Handle[testProbe]) error {
			r := repo(t, h, orders)
			from, err := r.GetForUpdate(ctx, "r1")
			if err != nil {
				return err
			}
			_, err = r.Update(ctx, from, placedOrder{ID: "r1", CustomerID: "c2", ProductID: "p2"})
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
		// The field-sourced tuple goes by its exact tuple, and the caller's by the filtered delete of its relation
		// (ST-SUP-Q118, delete path 1), which names no subject.
		if want := []string{"delete order:r1#buyer@customer:c2", "delete_relation order:r1#owner@:UNQUALIFIED()"}; !slices.Equal(added, want) {
			t.Fatalf("the delete enqueued %v, want %v", added, want)
		}
	})
	t.Run("ST-SUP-RP10-LC05-T15 A delete enqueues the filtered delete of each caller-sourced relationship and each reference of the row", func(t *testing.T) {
		e := newEnv(t)
		_, referring, _ := referenceEntities(e)
		s := referring.spec
		s.Relationships = []Relationship[placedOrder]{{Relation: "owner", SubjectType: "user"}}
		withReferences, err := NewEntity(s)
		if err != nil {
			t.Fatal(err)
		}
		// The rows are made through the same table with no Parent, so the create decides no reference.
		s.Parent = nil
		plain, err := NewEntity(s)
		if err != nil {
			t.Fatal(err)
		}
		make := e.command("make", nil, plain)
		drop := e.command("drop", nil, withReferences)
		for _, id := range []string{"r3", "r4"} {
			if _, err := e.run(make, e.request("t1", u7(t), "k-t15-"+id, `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
				_, err := repo(t, h, plain).Create(ctx, placedOrder{ID: id, CustomerID: "c1", ProductID: "p1"})
				return err
			})); err != nil {
				t.Fatal(err)
			}
		}
		before := e.tuples()
		if _, err := e.run(drop, e.request("t1", u7(t), "k-t15-drop", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			r := repo(t, h, withReferences)
			from, err := r.GetForDelete(ctx, "r3")
			if err != nil {
				return err
			}
			return r.Delete(ctx, from)
		})); err != nil {
			t.Fatal(err)
		}
		added := e.tuples()[len(before):]
		want := []string{"delete_relation order:r3#owner@:UNQUALIFIED()", "delete_relation order:r3#customer@:UNQUALIFIED()", "delete_relation order:r3#product@:UNQUALIFIED()"}
		if !slices.Equal(added, want) {
			t.Fatalf("the delete of r3 enqueued %v, want %v: each filter names r3 and one relation, so r4 keeps its tuples", added, want)
		}
	})
	t.Run("ST-SUP-RP10-LC05-T14 A create whose caller is not of the relationship's subject type is refused and writes nothing", func(t *testing.T) {
		e := newEnv(t)
		orders := relationshipEntity(e, "group")
		make := e.command("make", nil, orders)
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
}
