package query

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

func TestListOrder(t *testing.T) {
	t.Run("ST-SUP-RP11-LC18-T01 A list orders by a declared field with the key last and pages through the sealed cursor", func(t *testing.T) {
		e := newEnv(t)
		// Each order whose key ends in 3 is denied: the list keeps only allowed rows, in order.
		denied := func(r authz.EvaluationRequest) bool { return !strings.HasSuffix(keyOf(r.Resource), "3") }
		rt := e.servedRuntime(t, &fakeDecision{decide: denied})
		orders := orderEntity(t, false)
		type row struct {
			id    string
			total int64
		}
		var want []row
		for i := range 250 {
			id := fmt.Sprintf("o-%03d", (i*37)%250)
			total := int64(i % 9)
			e.insert(t, "INSERT INTO app.orders (tenant_id, id, total) VALUES ('t1', $1, $2)", id, total)
			if !strings.HasSuffix(id, "3") {
				want = append(want, row{id, total})
			}
		}
		e.insert(t, "INSERT INTO app.orders (tenant_id, id, total) VALUES ('t2', 'o-000', 100)")
		slices.SortFunc(want, func(a, b row) int {
			if a.total != b.total {
				return int(b.total - a.total)
			}
			return strings.Compare(a.id, b.id)
		})
		q := mustListQuery(t, "list-orders", orders, "view")
		order := []OrderBy{{Field: "total", Descending: true}}
		tn, p := mustTenant(t, "t1"), mustPrincipal(t, "user:u-7")
		var got []row
		text, pages := "", 0
		for {
			after, err := rt.ParseCursor(q, tn, p, text)
			if err != nil {
				t.Fatalf("page %d: the cursor of the page before does not parse: %v", pages, err)
			}
			page, err := Run(context.Background(), rt, q, bound(t, "t1", "user:u-7", q.Name()), struct{}{},
				func(ctx context.Context, _ struct{}, h *Handle[noProbe]) (ListResult[Order], error) {
					repo, err := Repo(h, orders)
					if err != nil {
						return ListResult[Order]{}, err
					}
					return repo.List(ctx, ListRequest{Limit: 7, After: after, Order: order})
				})
			if err != nil {
				t.Fatal(err)
			}
			pages++
			if len(page.Items) > 7 {
				t.Fatalf("page %d holds %d values, want at most 7", pages, len(page.Items))
			}
			for _, item := range page.Items {
				v := item.Value()
				got = append(got, row{v.ID, v.Total})
			}
			if !page.More {
				break
			}
			if text, err = rt.CursorText(q, tn, p, page.Next); err != nil {
				t.Fatal(err)
			}
			if pages > 100 {
				t.Fatal("the list does not end")
			}
		}
		if !slices.Equal(got, want) {
			t.Fatalf("the list gave %d rows, want %d in the order of total descending and then the key:\ngot  %v\nwant %v", len(got), len(want), got, want)
		}
		// A page holds the values, the next cursor and whether there is more: no total.
		for i := range reflect.TypeFor[ListResult[Order]]().NumField() {
			if name := reflect.TypeFor[ListResult[Order]]().Field(i).Name; name != "Items" && name != "Next" && name != "More" {
				t.Fatalf("a list page holds the field %s, want only Items, Next and More", name)
			}
		}

		// Two fields in two directions, the first one with NULL values, which come last.
		noted, err := NewEntity(Spec{Name: "order", Schema: "app", Table: "orders", Key: "id", Columns: []string{"id", "total", "note"},
			ResourceType: "order", ReadPermission: "view", ListPermission: "view"},
			func(r Row) (Order, error) {
				id, err := r.Text("id")
				if err != nil {
					return Order{}, err
				}
				total, err := r.Int("total")
				return Order{ID: id, Total: total}, err
			})
		if err != nil {
			t.Fatal(err)
		}
		e.insert(t, `UPDATE app.orders SET note = CASE WHEN total % 3 = 0 THEN NULL ELSE convert_to((total % 2)::text, 'UTF8') END WHERE tenant_id = 't1'`)
		notes := map[int64]string{}
		for total := int64(0); total < 9; total++ {
			if total%3 != 0 {
				notes[total] = fmt.Sprint(total % 2)
			}
		}
		twoWant := slices.Clone(want)
		slices.SortFunc(twoWant, func(a, b row) int {
			na, aok := notes[a.total]
			nb, bok := notes[b.total]
			switch {
			case aok != bok && aok:
				return -1
			case aok != bok:
				return 1
			case na != nb:
				return strings.Compare(na, nb)
			case a.total != b.total:
				return int(b.total - a.total)
			}
			return strings.Compare(a.id, b.id)
		})
		q2 := mustListQuery(t, "list-noted-orders", noted, "view")
		two := []OrderBy{{Field: "note"}, {Field: "total", Descending: true}}
		var twoGot []row
		var next Cursor
		for range 100 {
			page, err := Run(context.Background(), rt, q2, bound(t, "t1", "user:u-7", q2.Name()), struct{}{},
				func(ctx context.Context, _ struct{}, h *Handle[noProbe]) (ListResult[Order], error) {
					repo, err := Repo(h, noted)
					if err != nil {
						return ListResult[Order]{}, err
					}
					return repo.List(ctx, ListRequest{Limit: 11, After: next, Order: two})
				})
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range page.Items {
				v := item.Value()
				twoGot = append(twoGot, row{v.ID, v.Total})
			}
			if !page.More {
				break
			}
			next = page.Next
		}
		if !slices.Equal(twoGot, twoWant) {
			t.Fatalf("the list by note and total descending gave\ngot  %v\nwant %v", twoGot, twoWant)
		}

		// An order field that the entity does not declare is refused before any statement.
		rec := &statements{}
		tx, err := tenant.Begin(context.Background(), e.traced(t, rec), tn, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback() })
		h, err := bind(context.Background(), rt, tx, tn, p, q, noProbe{})
		if err != nil {
			t.Fatal(err)
		}
		repo, err := Repo(h, orders)
		if err != nil {
			t.Fatal(err)
		}
		before := len(rec.all())
		for name, by := range map[string][]OrderBy{
			"a field the entity does not declare": {{Field: "note"}},
			"a field that is not a column name":   {{Field: "total; DROP TABLE app.orders"}},
			"a field given twice":                 {{Field: "total"}, {Field: "total", Descending: true}},
		} {
			if _, err := repo.List(context.Background(), ListRequest{Order: by}); !errors.Is(err, ErrBadRequest) {
				t.Errorf("an order by %s gave %v, want ErrBadRequest", name, err)
			}
		}
		if got := rec.all()[before:]; len(got) != 0 {
			t.Fatalf("the refused orders ran %d statements, want none: %v", len(got), got)
		}
		// A cursor of the order by total is refused for a list in the order of the key, and the other way round.
		first, err := repo.List(context.Background(), ListRequest{Limit: 2, Order: order})
		if err != nil || !first.More {
			t.Fatalf("the first ordered page gave %v", err)
		}
		if _, err := repo.List(context.Background(), ListRequest{Limit: 2, After: first.Next}); !errors.Is(err, ErrBadCursor) {
			t.Fatalf("a cursor of the order by total in a list by key gave %v, want ErrBadCursor", err)
		}
		if _, err := repo.List(context.Background(), ListRequest{Limit: 2, After: first.Next, Order: []OrderBy{{Field: "total"}}}); !errors.Is(err, ErrBadCursor) {
			t.Fatalf("a cursor of the order by total descending in a list by total ascending gave %v, want ErrBadCursor", err)
		}
		plain, err := repo.List(context.Background(), ListRequest{Limit: 2})
		if err != nil || !plain.More {
			t.Fatalf("the first page by key gave %v", err)
		}
		if _, err := repo.List(context.Background(), ListRequest{Limit: 2, After: plain.Next, Order: order}); !errors.Is(err, ErrBadCursor) {
			t.Fatalf("a cursor of the key order in a list by total gave %v, want ErrBadCursor", err)
		}
	})
}
