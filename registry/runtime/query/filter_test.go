package query

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
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
		// An entity whose list uses the resource search pages the table when the provider has no search.
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

func TestSmallListSearch(t *testing.T) {
	t.Run("ST-SUP-RP11-LC20-T05 A filtered list of an entity of small expected cardinality never pages the table, so its work does not tell whether hidden rows match", func(t *testing.T) {
		e := newEnv(t)
		// The principal may see the orders o-00 to o-09; o-10 to o-29 are hidden. Allowed rows of total 4 are the even
		// ones; each hidden row has total 7.
		var allowed, want []string
		for i := range 30 {
			id, total := fmt.Sprintf("o-%02d", i), int64(7)
			if i < 10 {
				allowed = append(allowed, id)
				total = int64(4 + i%2)
				if i%2 == 0 {
					want = append(want, id)
				}
			}
			e.insert(t, "INSERT INTO app.orders (tenant_id, id, total) VALUES ('t1', $1, $2)", id, total)
		}
		// The search gives the allowed orders, in pages of the requested limit.
		port := &fakeDecision{decide: func(authz.EvaluationRequest) bool { return false }, search: func(r authz.SearchRequest) authz.SearchResult {
			from := 0
			if cursor, _, ok := r.Page.Token.CursorFor("fake"); ok {
				fmt.Sscan(cursor, &from)
			}
			to := min(from+r.Page.Limit, len(allowed))
			var result authz.SearchResult
			for _, id := range allowed[from:to] {
				result.Resources = append(result.Resources, resourceIn(t, "t1", "order", id))
			}
			if to < len(allowed) {
				result.Next = authz.NewPageToken("fake", fmt.Sprint(to), authz.Fresh())
			}
			return result
		}}
		rt := e.servedRuntime(t, port)
		searched := orderEntity(t, true)
		q := mustListQuery(t, "list-searched-orders", searched, "view")
		tn, p := mustTenant(t, "t1"), mustPrincipal(t, "user:u-7")
		// pages lists through every page with the filter on one traced transaction, and gives the IDs, the statements
		// that read the table and the number of searches.
		pages := func(where []Equal) (ids []string, reads []statement, searches int) {
			rec := &statements{}
			tx, err := tenant.Begin(context.Background(), e.traced(t, rec), tn, &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			h, err := bind(context.Background(), rt, tx, tn, p, q, noProbe{})
			if err != nil {
				t.Fatal(err)
			}
			repo, err := Repo(h, searched)
			if err != nil {
				t.Fatal(err)
			}
			port.mu.Lock()
			asked := len(port.asked)
			port.mu.Unlock()
			after := Cursor{}
			for range 20 {
				page, err := repo.List(context.Background(), ListRequest{Limit: 3, After: after, Where: where})
				if err != nil {
					t.Fatal(err)
				}
				for _, item := range page.Items {
					ids = append(ids, item.ID())
				}
				if !page.More {
					break
				}
				after = page.Next
			}
			for _, s := range rec.all() {
				if strings.Contains(s.sql, "app.orders") {
					reads = append(reads, s)
				}
			}
			port.mu.Lock()
			defer port.mu.Unlock()
			return ids, reads, len(port.asked) - asked
		}
		got, reads, searches := pages([]Equal{{Field: "total", Value: int64(4)}})
		if !slices.Equal(got, want) {
			t.Errorf("the filtered list gave %v, want %v", got, want)
		}
		for _, s := range reads {
			if !strings.Contains(s.sql, "= ANY(") {
				t.Errorf("the filtered list of a small entity paged the table: %s", s.sql)
			}
		}
		port.mu.Lock()
		batches := len(port.batches)
		port.mu.Unlock()
		if batches != 0 || searches == 0 {
			t.Errorf("the filtered list made %d batch decisions and %d searches, want none and at least one", batches, searches)
		}
		// The work of a filter that only hidden rows match is the work of a filter that no row matches.
		hiddenIDs, hiddenReads, hiddenSearches := pages([]Equal{{Field: "total", Value: int64(7)}})
		noneIDs, noneReads, noneSearches := pages([]Equal{{Field: "total", Value: int64(8)}})
		if len(hiddenIDs) != 0 || len(noneIDs) != 0 || len(hiddenReads) != len(noneReads) || hiddenSearches != noneSearches {
			t.Errorf("a filter that only hidden rows match gave %v in %d reads and %d searches, one that no row matches %v in %d reads and %d searches; want none in the same work",
				hiddenIDs, len(hiddenReads), hiddenSearches, noneIDs, len(noneReads), noneSearches)
		}
	})
}

func TestSmallListOrder(t *testing.T) {
	t.Run("ST-SUP-RP11-LC20-T06 An ordered list of an entity of small expected cardinality reads only the searched keys, and more keys than the bound fail closed", func(t *testing.T) {
		e := newEnv(t)
		// The principal may see o-10 to o-14; hidden rows sort before and after them, by key and by total.
		var allowed []string
		for i := range 25 {
			id := fmt.Sprintf("o-%02d", i)
			if i >= 10 && i < 15 {
				allowed = append(allowed, id)
			}
			e.insert(t, "INSERT INTO app.orders (tenant_id, id, total) VALUES ('t1', $1, $2)", id, int64(i%3))
		}
		var keys []string
		port := &fakeDecision{decide: func(authz.EvaluationRequest) bool { return true }, search: func(r authz.SearchRequest) authz.SearchResult {
			from := 0
			if cursor, _, ok := r.Page.Token.CursorFor("fake"); ok {
				fmt.Sscan(cursor, &from)
			}
			to := min(from+r.Page.Limit, len(keys))
			var result authz.SearchResult
			for _, id := range keys[from:to] {
				result.Resources = append(result.Resources, resourceIn(t, "t1", "order", id))
			}
			if to < len(keys) {
				result.Next = authz.NewPageToken("fake", fmt.Sprint(to), authz.Fresh())
			}
			return result
		}}
		rt := e.servedRuntime(t, port)
		searched := orderEntity(t, true)
		q := mustListQuery(t, "list-searched-orders", searched, "view")
		tn, p := mustTenant(t, "t1"), mustPrincipal(t, "user:u-7")
		list := func(request ListRequest) (ListResult[Order], []statement, error) {
			rec := &statements{}
			tx, err := tenant.Begin(context.Background(), e.traced(t, rec), tn, &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			h, err := bind(context.Background(), rt, tx, tn, p, q, noProbe{})
			if err != nil {
				t.Fatal(err)
			}
			repo, err := Repo(h, searched)
			if err != nil {
				t.Fatal(err)
			}
			page, err := repo.List(context.Background(), request)
			return page, rec.all(), err
		}
		// Every decision allows, so only the searched keys keep the hidden rows out.
		keys = allowed
		var got []string
		after := Cursor{}
		for range 10 {
			page, seen, err := list(ListRequest{Limit: 2, After: after, Order: []OrderBy{{Field: "total", Descending: true}}, Where: []Equal{{Field: "total", Value: int64(1)}}})
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range seen {
				if strings.Contains(s.sql, "app.orders") && !strings.Contains(s.sql, "= ANY(") {
					t.Errorf("the ordered list of a small entity paged the table: %s", s.sql)
				}
			}
			for _, item := range page.Items {
				got = append(got, item.ID())
			}
			if !page.More {
				break
			}
			after = page.Next
		}
		if want := []string{"o-10", "o-13"}; !slices.Equal(got, want) {
			t.Errorf("the ordered list of total 1 gave %v, want the visible %v", got, want)
		}
		// One key over the bound fails closed, with an error that names the entity and the bound.
		keys = make([]string, MaxSearchKeys+1)
		for i := range keys {
			keys[i] = fmt.Sprintf("k-%05d", i)
		}
		_, seen, err := list(ListRequest{Order: []OrderBy{{Field: "total"}}})
		if !errors.Is(err, ErrTooManyKeys) || !strings.Contains(err.Error(), "entity order") || !strings.Contains(err.Error(), strconv.Itoa(MaxSearchKeys)) {
			t.Errorf("an ordered list with %d searched keys gave %v, want ErrTooManyKeys that names the entity and the bound", len(keys), err)
		}
		for _, s := range seen {
			if strings.Contains(s.sql, "app.orders") {
				t.Errorf("the refused list read the table: %s", s.sql)
			}
		}
		// The bound itself is taken.
		keys = keys[:MaxSearchKeys]
		if _, _, err := list(ListRequest{Order: []OrderBy{{Field: "total"}}}); err != nil {
			t.Errorf("an ordered list with %d searched keys gave %v, want it taken", len(keys), err)
		}
	})
}

func TestSmallListOrderWork(t *testing.T) {
	t.Run("ST-SUP-RP11-LC20-T07 The work of an ordered list of an entity of small expected cardinality depends neither on where hidden rows sort nor on whether they match its filter", func(t *testing.T) {
		e := newEnv(t)
		// An index of the order field, which the planner walks when it may test the keys row by row.
		e.insert(t, "CREATE INDEX orders_total ON app.orders (tenant_id, total)")
		// In both tenants the caller sees the same 5000 orders; 200000 hidden orders sort before them by total in t1,
		// and after them in t2.
		var visible []string
		for i := range 5000 {
			visible = append(visible, fmt.Sprintf("v-%05d", i))
		}
		e.insert(t, "INSERT INTO app.orders (tenant_id, id, total) SELECT t, 'v-' || lpad(g::text, 5, '0'), 1000000 + g FROM generate_series(0, 4999) g, (VALUES ('t1'), ('t2')) AS x(t)")
		e.insert(t, "INSERT INTO app.orders (tenant_id, id, total) SELECT 't1', 'h-' || g, g FROM generate_series(1, 200000) g")
		e.insert(t, "INSERT INTO app.orders (tenant_id, id, total) SELECT 't2', 'h-' || g, 2000000 + g FROM generate_series(1, 200000) g")
		// For a filter on the indexed field: the same visible orders, all of total 7, in t3 with 200000 hidden orders
		// of total 7 that match the filter, and in t4 with 200000 hidden orders of total 8 that do not.
		e.insert(t, "INSERT INTO app.orders (tenant_id, id, total) SELECT t, 'v-' || lpad(g::text, 5, '0'), 7 FROM generate_series(0, 4999) g, (VALUES ('t3'), ('t4')) AS x(t)")
		e.insert(t, "INSERT INTO app.orders (tenant_id, id, total) SELECT 't3', 'h-' || g, 7 FROM generate_series(1, 200000) g")
		e.insert(t, "INSERT INTO app.orders (tenant_id, id, total) SELECT 't4', 'h-' || g, 8 FROM generate_series(1, 200000) g")
		e.insert(t, "VACUUM ANALYZE app.orders")
		var current string
		port := &fakeDecision{decide: func(authz.EvaluationRequest) bool { return true }, search: func(r authz.SearchRequest) authz.SearchResult {
			from := 0
			if cursor, _, ok := r.Page.Token.CursorFor("fake"); ok {
				fmt.Sscan(cursor, &from)
			}
			to := min(from+r.Page.Limit, len(visible))
			var result authz.SearchResult
			for _, id := range visible[from:to] {
				result.Resources = append(result.Resources, resourceIn(t, current, "order", id))
			}
			if to < len(visible) {
				result.Next = authz.NewPageToken("fake", fmt.Sprint(to), authz.Fresh())
			}
			return result
		}}
		rt := e.servedRuntime(t, port)
		searched := orderEntity(t, true)
		q := mustListQuery(t, "list-searched-orders", searched, "view")
		p := mustPrincipal(t, "user:u-7")
		// buffers lists the first page in the order of total as tenant id, and gives its IDs and the shared buffers
		// that the ordered statement of the list reads, from EXPLAIN (ANALYZE, BUFFERS) of the same statement.
		buffers := func(id string, where []Equal) ([]string, int) {
			current = id
			tn := mustTenant(t, id)
			rec := &statements{}
			tx, err := tenant.Begin(context.Background(), e.traced(t, rec), tn, &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			h, err := bind(context.Background(), rt, tx, tn, p, q, noProbe{})
			if err != nil {
				t.Fatal(err)
			}
			repo, err := Repo(h, searched)
			if err != nil {
				t.Fatal(err)
			}
			page, err := repo.List(context.Background(), ListRequest{Limit: 4, Order: []OrderBy{{Field: "total"}}, Where: where})
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, item := range page.Items {
				ids = append(ids, item.ID())
			}
			for _, s := range rec.all() {
				if !strings.Contains(s.sql, "app.orders") || !strings.Contains(s.sql, "ORDER BY") {
					continue
				}
				var plan []struct {
					Plan struct {
						Hit  int `json:"Shared Hit Blocks"`
						Read int `json:"Shared Read Blocks"`
					} `json:"Plan"`
				}
				var text []byte
				if err := e.admin.QueryRowContext(context.Background(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+s.sql, s.args...).Scan(&text); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(text, &plan); err != nil || len(plan) != 1 {
					t.Fatalf("the plan %s: %v", text, err)
				}
				return ids, plan[0].Plan.Hit + plan[0].Plan.Read
			}
			t.Fatal("the list ran no ordered statement")
			return nil, 0
		}
		beforeIDs, before := buffers("t1", nil)
		afterIDs, after := buffers("t2", nil)
		want := []string{"v-00000", "v-00001", "v-00002", "v-00003"}
		if !slices.Equal(beforeIDs, want) || !slices.Equal(afterIDs, want) {
			t.Errorf("the first pages were %v and %v, want %v in both tenants", beforeIDs, afterIDs, want)
		}
		if before > 2*after+50 {
			t.Errorf("the ordered list read %d buffers with hidden rows before the visible ones and %d with them after; the work depends on where hidden rows sort", before, after)
		}
		// A filter on an indexed field: hidden rows that match it cost no more than hidden rows that do not.
		seven := []Equal{{Field: "total", Value: int64(7)}}
		matchIDs, match := buffers("t3", seven)
		noneIDs, none := buffers("t4", seven)
		if !slices.Equal(matchIDs, want) || !slices.Equal(noneIDs, want) {
			t.Errorf("the filtered first pages were %v and %v, want %v in both tenants", matchIDs, noneIDs, want)
		}
		if match > 2*none+50 {
			t.Errorf("the filtered ordered list read %d buffers with hidden rows that match the filter and %d with hidden rows that do not; the work depends on hidden matches", match, none)
		}
	})
}

func TestOverBoundWarning(t *testing.T) {
	t.Run("ST-SUP-RP11-LC20-T08 The warning of a list over the bound is logged at most once a minute, with the count of lists", func(t *testing.T) {
		var out strings.Builder
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&out, nil)))
		t.Cleanup(func() { slog.SetDefault(previous) })
		clock := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
		b := &boundWarning{every: time.Minute, now: func() time.Time { return clock }}
		for range 1000 {
			b.warn(context.Background(), "order")
		}
		clock = clock.Add(time.Minute)
		b.warn(context.Background(), "order")
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if len(lines) != 2 || !strings.Contains(lines[0], "entity=order") || !strings.Contains(lines[0], "bound=10000") ||
			!strings.HasSuffix(lines[0], "lists=1") || !strings.HasSuffix(lines[1], "lists=1000") {
			t.Errorf("1001 lists over the bound in a minute and one after it logged %d lines:\n%s\nwant one with lists=1 and one with lists=1000", len(lines), out.String())
		}
	})
}
