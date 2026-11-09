package query

import (
	"context"
	"errors"
	"fmt"

	"stego.dev/runtime/authz"
)

// errForeignValue is the error of a call that takes a loaded value that this handle did not load.
var errForeignValue = errors.New("query: the value was not loaded through this handle")

// Authz is the authorization part of a query handle (ST-SUP-RP11-LC05). It decides only through the authorization
// runtime's handle, which is bound to the same transaction, tenant and principal: each decision has the principal as
// subject and runs at the consistency floor of ST-SUP-RP06-LC09, read through the bound transaction. It offers no
// other decision path, no bypass and no relationship write.
type Authz struct {
	core *core
}

// Evaluate decides action on the resource of a value that this handle loaded, with the row token that the read
// stored in it, so a fill cannot give a token of its own. A deny is false with a nil error. An entity with no
// resource type is not protected and is refused here, since it has no permission to decide.
func (a *Authz) Evaluate(ctx context.Context, action string, value LoadedValue) (bool, error) {
	c := a.core
	if err := c.life.enter(); err != nil {
		return false, err
	}
	defer c.life.leave()
	if value == nil {
		return false, errForeignValue
	}
	origin, e, id, token := value.row()
	if origin != c || e == nil {
		return false, errForeignValue
	}
	if e.spec.ResourceType == "" {
		return false, errors.New("query: the entity " + e.spec.Name + " has no resource type to decide on")
	}
	result, err := c.authz.Evaluate(ctx, action, authz.Resource{Type: e.spec.ResourceType, ID: id}, token, nil)
	if err != nil {
		return false, err
	}
	return result.Allowed, nil
}

// allowRead decides the read permission that the entity declares on the row id, whose token is token, at the floor
// (ST-SUP-RP11-LC06). An entity with no resource type is not protected, and its rows need no decision.
func (c *core) allowRead(ctx context.Context, e *entity, id string, token authz.Token) (bool, error) {
	if e.spec.ResourceType == "" {
		return true, nil
	}
	result, err := c.authz.Evaluate(ctx, e.spec.ReadPermission, authz.Resource{Type: e.spec.ResourceType, ID: id}, token, nil)
	if err != nil {
		return false, err
	}
	return result.Allowed, nil
}

// Cursor is the position of a list: the zero Cursor is its start, and a ListResult gives the next one.
type Cursor struct {
	after string
	page  authz.PageToken
}

// ListRequest is one page of a list: at most Limit allowed values after the cursor. A Limit of 0 is 100; the most
// is 1000.
type ListRequest struct {
	Limit int
	After Cursor
}

// ListResult is one page of a list: the allowed values in the order of their key, and when More is true the cursor
// of the next page.
type ListResult[T any] struct {
	Items []*Loaded[T]
	Next  Cursor
	More  bool
}

// MaxLimit is the most values that one list page returns.
const MaxLimit = 1000

// List reads one page of the entity's rows in the tenant (ST-SUP-RP11-LC03, ST-SUP-RP11-LC07). For a protected
// entity it holds only the rows that the principal may see on the query's declared permission for a list query,
// and on the permission that the entity declares for list otherwise: for an entity with a small expected
// cardinality, the provider's resource search gives the rows; otherwise List pages the table and filters each
// page with one batch evaluation. Each search and each batch runs at least as fresh as the revocation floor read
// at that page through the bound transaction (ST-SUP-RP06-LC04, ST-SUP-RP06-LC09). Each read of personal data is
// audited before List returns (ST-SUP-RP11-LC08).
func (r *Repository[T]) List(ctx context.Context, request ListRequest) (ListResult[T], error) {
	c := r.core
	if err := c.life.enter(); err != nil {
		return ListResult[T]{}, err
	}
	defer c.life.leave()
	limit := request.Limit
	switch {
	case limit == 0:
		limit = 100
	case limit < 0 || limit > MaxLimit:
		return ListResult[T]{}, fmt.Errorf("query: a list page holds 1 to %d values", MaxLimit)
	}
	e := &r.entity.e
	if e.spec.ResourceType != "" && e.spec.Search && c.authz != nil {
		result, err := r.searchPage(ctx, limit, request.After)
		if !errors.Is(err, authz.ErrNoSearch) {
			return result, err
		}
	}
	return r.tablePage(ctx, limit, request.After)
}

// permission is the permission of a list of the entity: the query's declared permission for a list query, and the
// entity's list permission otherwise.
func (r *Repository[T]) permission() string {
	if r.core.query.listPermission != "" {
		return r.core.query.listPermission
	}
	return r.entity.e.spec.ListPermission
}

// tablePage pages the table by its key and keeps the allowed rows of each page, until it holds limit values or the
// table ends.
func (r *Repository[T]) tablePage(ctx context.Context, limit int, after Cursor) (ListResult[T], error) {
	c := r.core
	e := &r.entity.e
	var list *authz.List
	if e.spec.ResourceType != "" {
		list = c.authz.List(r.permission(), nil)
	}
	statement := "SELECT " + e.selectList + " FROM " + e.table + " WHERE " + TenantColumn + " = $1 AND " + e.spec.Key +
		" > $2 ORDER BY " + e.spec.Key + " LIMIT $3"
	var result ListResult[T]
	key := after.after
	for {
		rows, err := c.tx.QueryContext(ctx, statement, c.tenant.ID(), key, limit)
		if err != nil {
			return ListResult[T]{}, fmt.Errorf("query: list %s: %w", e.spec.Name, err)
		}
		page, tokens, err := readRows(rows, e)
		if err != nil {
			return ListResult[T]{}, err
		}
		if len(page) == 0 {
			return result, nil
		}
		ids := make([]string, len(page))
		for i, row := range page {
			if ids[i], err = row.Text(e.spec.Key); err != nil {
				return ListResult[T]{}, err
			}
		}
		key = ids[len(ids)-1]
		allowed := map[string]bool{}
		if list != nil {
			candidates := make([]authz.Row, len(page))
			for i := range page {
				candidates[i] = authz.Row{Resource: authz.Resource{Type: e.spec.ResourceType, ID: ids[i]}, Token: tokens[i]}
			}
			kept, err := list.Page(ctx, candidates)
			if err != nil {
				return ListResult[T]{}, err
			}
			for _, row := range kept {
				allowed[row.Resource.ID] = true
			}
		}
		for i, row := range page {
			if list != nil && !allowed[ids[i]] {
				continue
			}
			loaded, err := r.load(row, tokens[i])
			if err != nil {
				return ListResult[T]{}, err
			}
			if err := c.audited(ctx, e, loaded.id, row); err != nil {
				return ListResult[T]{}, err
			}
			result.Items = append(result.Items, loaded)
			if len(result.Items) == limit {
				result.Next, result.More = Cursor{after: ids[i]}, true
				return result, nil
			}
		}
		if len(page) < limit {
			return result, nil
		}
	}
}

// searchPage asks the provider for one page of the resources the principal may see and reads their rows in the
// tenant. A resource of another tenant has no row here and is left out.
func (r *Repository[T]) searchPage(ctx context.Context, limit int, after Cursor) (ListResult[T], error) {
	c := r.core
	e := &r.entity.e
	found, err := c.authz.Search(ctx, r.permission(), e.spec.ResourceType, nil, authz.Page{Token: after.page, Limit: limit})
	if err != nil {
		return ListResult[T]{}, err
	}
	var result ListResult[T]
	if !found.Next.IsZero() {
		result.Next, result.More = Cursor{page: found.Next}, true
	}
	if len(found.Resources) == 0 {
		return result, nil
	}
	ids := make([]string, 0, len(found.Resources))
	for _, resource := range found.Resources {
		if resource.Type == e.spec.ResourceType {
			ids = append(ids, resource.ID)
		}
	}
	rows, err := c.tx.QueryContext(ctx, "SELECT "+e.selectList+" FROM "+e.table+" WHERE "+TenantColumn+" = $1 AND "+
		e.spec.Key+" = ANY($2) ORDER BY "+e.spec.Key, c.tenant.ID(), ids)
	if err != nil {
		return ListResult[T]{}, fmt.Errorf("query: list %s: %w", e.spec.Name, err)
	}
	page, tokens, err := readRows(rows, e)
	if err != nil {
		return ListResult[T]{}, err
	}
	for i, row := range page {
		loaded, err := r.load(row, tokens[i])
		if err != nil {
			return ListResult[T]{}, err
		}
		if err := c.audited(ctx, e, loaded.id, row); err != nil {
			return ListResult[T]{}, err
		}
		result.Items = append(result.Items, loaded)
	}
	return result, nil
}
