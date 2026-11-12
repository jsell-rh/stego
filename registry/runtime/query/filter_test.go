package query

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/registry/runtime/authz"
)

func TestListFilter(t *testing.T) {
	t.Run("ST-SUP-RP11-LC20-T01 A list keeps the rows whose declared fields equal the filter, with bound values and a cursor of that filter", func(t *testing.T) {
		e := newEnv(t)
		// Each order whose key ends in 3 is denied: the filtered list keeps only allowed rows.
		denied := func(r authz.EvaluationRequest) bool { return !strings.HasSuffix(keyOf(r.Resource), "3") }
		rt := e.servedRuntime(t, &fakeDecision{decide: denied})
		var want []string
		for i := range 250 {
			id := fmt.Sprintf("o-%03d", (i*37)%250)
			total := int64(i % 9)
			e.insert(t, "INSERT INTO app.orders (tenant_id, id, total) VALUES ('t1', $1, $2)", id, total)
			if total == 4 && !strings.HasSuffix(id, "3") {
				want = append(want, id)
			}
		}
		e.insert(t, "INSERT INTO app.orders (tenant_id, id, total) VALUES ('t2', 'o-900', 4)")
		slices.Sort(want)
		tn, p := mustTenant(t, "t1"), mustPrincipal(t, "user:u-7")
		// list runs one page of the list of entity with the request.
		list := func(entity *Entity[Order], q *Query, request ListRequest) (ListResult[Order], error) {
			return Run(context.Background(), rt, q, bound(t, "t1", "user:u-7", q.Name()), struct{}{},
				func(ctx context.Context, _ struct{}, h *Handle[noProbe]) (ListResult[Order], error) {
					repo, err := Repo(h, entity)
					if err != nil {
						return ListResult[Order]{}, err
					}
					return repo.List(ctx, request)
				})
		}
		// all pages through the list with the filter and the order, through the text of each cursor.
		all := func(entity *Entity[Order], q *Query, where []Equal, order []OrderBy) []string {
			var got []string
			text := ""
			for pages := 0; ; pages++ {
				after, err := rt.ParseCursor(q, tn, p, text)
				if err != nil {
					t.Fatalf("page %d: the cursor does not parse: %v", pages, err)
				}
				page, err := list(entity, q, ListRequest{Limit: 3, After: after, Where: where, Order: order})
				if err != nil {
					t.Fatal(err)
				}
				for _, item := range page.Items {
					if item.Value().Total != 4 {
						t.Fatalf("the filtered list gave the order %s with total %d", item.ID(), item.Value().Total)
					}
					got = append(got, item.ID())
				}
				if !page.More {
					return got
				}
				if text, err = rt.CursorText(q, tn, p, page.Next); err != nil {
					t.Fatal(err)
				}
				if pages > 200 {
					t.Fatal("the list does not end")
				}
			}
		}
		orders := orderEntity(t, false)
		q := mustListQuery(t, "list-orders", orders, "view")
		if got := all(orders, q, []Equal{{Field: "total", Value: int64(4)}}, nil); !slices.Equal(got, want) {
			t.Errorf("the filtered list in key order gave %v, want %v", got, want)
		}
		ordered := all(orders, q, []Equal{{Field: "total", Value: 4}}, []OrderBy{{Field: "id", Descending: true}})
		if !slices.Equal(ordered, reversed(want)) {
			t.Errorf("the filtered list in order gave %v, want %v", ordered, reversed(want))
		}
		// An entity whose list uses the resource search pages the table when the list has a filter.
		searched := orderEntity(t, true)
		qs := mustListQuery(t, "list-searched-orders", searched, "view")
		if got := all(searched, qs, []Equal{{Field: "total", Value: int64(4)}}, nil); !slices.Equal(got, want) {
			t.Errorf("the filtered list of a searched entity gave %v, want %v", got, want)
		}

		// A value is a parameter: text that would be SQL matches no row and changes nothing.
		page, err := list(orders, q, ListRequest{Where: []Equal{{Field: "id", Value: "o-001' OR '1'='1"}}})
		if err != nil || len(page.Items) != 0 {
			t.Errorf("a filter value with SQL text gave %d rows and %v, want none", len(page.Items), err)
		}

		// A cursor holds its filter: a cursor of one filter, or of none, is refused for another.
		first, err := list(orders, q, ListRequest{Limit: 2, Where: []Equal{{Field: "total", Value: int64(4)}}})
		if err != nil || !first.More {
			t.Fatalf("the first filtered page gave more %v and %v", first.More, err)
		}
		text, err := rt.CursorText(q, tn, p, first.Next)
		if err != nil {
			t.Fatal(err)
		}
		after, err := rt.ParseCursor(q, tn, p, text)
		if err != nil {
			t.Fatal(err)
		}
		plain, err := list(orders, q, ListRequest{Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		for name, request := range map[string]ListRequest{
			"another value":    {After: after, Where: []Equal{{Field: "total", Value: int64(5)}}},
			"another type":     {After: after, Where: []Equal{{Field: "total", Value: "4"}}},
			"no filter":        {After: after},
			"a cursor of none": {After: plain.Next, Where: []Equal{{Field: "total", Value: int64(4)}}},
		} {
			if _, err := list(orders, q, request); !errors.Is(err, ErrBadCursor) {
				t.Errorf("%s: the list gave %v, want ErrBadCursor", name, err)
			}
		}

		// The filter names declared fields only, each once, at most MaxFilter, with a value of a plain type.
		var many []Equal
		for i := range MaxFilter + 1 {
			many = append(many, Equal{Field: fmt.Sprintf("f%d", i), Value: 1})
		}
		for name, where := range map[string][]Equal{
			"an undeclared field": {{Field: "note", Value: "x"}},
			"the tenant column":   {{Field: TenantColumn, Value: "t2"}},
			"a field twice":       {{Field: "total", Value: 4}, {Field: "total", Value: 5}},
			"a value of bytes":    {{Field: "total", Value: []byte("4")}},
			"a nil value":         {{Field: "total", Value: nil}},
			"too many":            many,
		} {
			if _, err := list(orders, q, ListRequest{Where: where}); !errors.Is(err, ErrBadRequest) || strings.Contains(fmt.Sprint(err), "o-0") {
				t.Errorf("%s: the list gave %v, want ErrBadRequest with no row", name, err)
			}
		}
	})
}

// reversed gives a copy of s in the other order.
func reversed(s []string) []string {
	r := slices.Clone(s)
	slices.Reverse(r)
	return r
}

func TestParseSearch(t *testing.T) {
	t.Run("ST-SUP-RP11-LC20-T02 A list search is read into equality conditions of declared fields only", func(t *testing.T) {
		fields := map[string]FieldKind{"name": KindText, "qty": KindInt64, "small": KindInt32, "weight": KindFloat, "done": KindBool}
		for text, want := range map[string][]Equal{
			"":             nil,
			"name = 'a b'": {{Field: "name", Value: "a b"}},
			"name='it''s'": {{Field: "name", Value: "it's"}},
			"qty = 3 AND done = true and weight = 2.5": {{Field: "qty", Value: int64(3)}, {Field: "done", Value: true}, {Field: "weight", Value: 2.5}},
			"  name = 'x'  ":            {{Field: "name", Value: "x"}},
			"name = 'x'' OR ''1''=''1'": {{Field: "name", Value: "x' OR '1'='1"}},
		} {
			got, err := ParseSearch(text, fields, MaxFilter)
			if err != nil || fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("ParseSearch(%q) = %v, %v; want %v", text, got, err, want)
			}
		}
		many := strings.Repeat("qty = 1 and ", MaxFilter) + "name = 'x'"
		for _, text := range []string{
			"secret = 'x'", "name = 'x' and name = 'y'", "name = 'x' or qty = 1", "name != 'x'", "name = x",
			"qty = 'x'", "qty = 1.5", "done = yes", "name = 'open", "name = 'x' and", "(name = 'x')",
			"name = 'x'; DROP TABLE t", strings.Repeat("a", MaxSearch+1), "name = '\xff'", many,
			"small = 2147483648", "qty = 9223372036854775808", "name = 'a\x00b'",
		} {
			if got, err := ParseSearch(text, fields, MaxFilter); !errors.Is(err, ErrBadRequest) || got != nil || strings.Contains(err.Error(), "DROP") {
				t.Errorf("ParseSearch(%.40q) = %v, %v; want a refusal that holds no part of the text", text, got, err)
			}
		}
		// The room of a list with a scope and implicit fields is smaller.
		if _, err := ParseSearch("name = 'x' and qty = 1", fields, 1); !errors.Is(err, ErrBadRequest) {
			t.Errorf("two conditions with room for one gave %v, want ErrBadRequest", err)
		}
		if got, err := ParseSearch("name = 'x'", fields, 1); err != nil || len(got) != 1 {
			t.Errorf("one condition with room for one gave %v, %v", got, err)
		}
	})
}

func TestListFilterCost(t *testing.T) {
	t.Run("ST-SUP-RP11-LC20-T03 A list call decides as often whether rows it may not see match or not, a sensitive field neither filters nor orders, and each statement has a time limit", func(t *testing.T) {
		e := newEnv(t)
		// Every order of total 7 is denied, and no order has total 8.
		port := &fakeDecision{decide: func(r authz.EvaluationRequest) bool { return !strings.HasSuffix(keyOf(r.Resource), "-7") }}
		rt := e.servedRuntime(t, port)
		for i := range 20 {
			e.insert(t, "INSERT INTO app.orders (tenant_id, id, total) VALUES ('t1', $1, $2)", fmt.Sprintf("o-%02d-%d", i, 7), 7)
		}
		orders := orderEntity(t, false)
		q := mustListQuery(t, "list-orders", orders, "view")
		batchesOf := func(where []Equal) int {
			before := len(port.batches)
			page, err := Run(context.Background(), rt, q, bound(t, "t1", "user:u-7", q.Name()), struct{}{},
				func(ctx context.Context, _ struct{}, h *Handle[noProbe]) (ListResult[Order], error) {
					repo, err := Repo(h, orders)
					if err != nil {
						return ListResult[Order]{}, err
					}
					return repo.List(ctx, ListRequest{Limit: 5, Where: where})
				})
			if err != nil || len(page.Items) != 0 {
				t.Fatalf("the list of %v gave %d rows and %v, want none", where, len(page.Items), err)
			}
			port.mu.Lock()
			defer port.mu.Unlock()
			return len(port.batches) - before
		}
		hidden, none := batchesOf([]Equal{{Field: "total", Value: int64(7)}}), batchesOf([]Equal{{Field: "total", Value: int64(8)}})
		if hidden != 1 || none != 1 {
			t.Errorf("a call whose filter matches only rows that the caller may not see made %d batch decisions, and one that matches no row %d; want 1 and 1", hidden, none)
		}

		// A pii field neither filters nor orders a list: a match would tell its value.
		charges := chargeEntity(t)
		qc := mustListQuery(t, "list-charges", charges, "view")
		for name, request := range map[string]ListRequest{
			"a filter on pii": {Where: []Equal{{Field: "card_last4", Value: "4242"}}},
			"an order on pii": {Order: []OrderBy{{Field: "card_last4"}}},
		} {
			_, err := Run(context.Background(), rt, qc, bound(t, "t1", "user:u-7", qc.Name()), struct{}{},
				func(ctx context.Context, _ struct{}, h *Handle[noProbe]) (ListResult[Charge], error) {
					repo, err := Repo(h, charges)
					if err != nil {
						return ListResult[Charge]{}, err
					}
					return repo.List(ctx, request)
				})
			if !errors.Is(err, ErrBadRequest) {
				t.Errorf("%s gave %v, want ErrBadRequest", name, err)
			}
		}

		// Each statement of the query's transaction ends within the time limit.
		var limit string
		_, err := Run(context.Background(), rt, q, bound(t, "t1", "user:u-7", q.Name()), struct{}{},
			func(ctx context.Context, _ struct{}, h *Handle[noProbe]) (struct{}, error) {
				return struct{}{}, h.core.tx.QueryRowContext(ctx, "SHOW statement_timeout").Scan(&limit)
			})
		if err != nil || limit != "5s" {
			t.Errorf("the statement time limit of a query is %q (%v), want 5s", limit, err)
		}
	})
}
