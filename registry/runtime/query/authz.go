package query

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/authz"
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
	origin, e, id, token := rowOf(value)
	if origin != c || e == nil {
		return false, errForeignValue
	}
	if e.spec.ResourceType == "" {
		return false, errors.New("query: the entity " + e.spec.Name + " has no resource type to decide on")
	}
	resource, err := c.resource(e, id)
	if err != nil {
		return false, err
	}
	result, err := c.authz.Evaluate(ctx, action, resource, token, nil)
	if err != nil {
		return false, err
	}
	return result.Allowed, nil
}

// resource is the resource at the provider of the row id of entity e in the handle's tenant: its ID is the
// tenant-qualified form that the authorization runtime builds (ST-SUP-RP06-LC22, ST-SUP-RC29-OC22).
func (c *core) resource(e *entity, id string) (authz.Resource, error) {
	return authz.NewResource(c.tenant, e.spec.ResourceType, id)
}

// allowRead decides the read permission that the entity declares on the row id, whose token is token, at the floor
// (ST-SUP-RP11-LC06). An entity with no resource type is not protected, and its rows need no decision.
func (c *core) allowRead(ctx context.Context, e *entity, id string, token authz.Token) (bool, error) {
	if e.spec.ResourceType == "" {
		return true, nil
	}
	resource, err := c.resource(e, id)
	if err != nil {
		return false, err
	}
	result, err := c.authz.Evaluate(ctx, e.spec.ReadPermission, resource, token, nil)
	if err != nil {
		return false, err
	}
	return result.Allowed, nil
}

// Cursor is the position of a list: the zero Cursor is its start, and a ListResult gives the next one. A list in an
// order of declared fields holds the position of each order field too, and the order it is of (ST-SUP-RP11-LC18).
type Cursor struct {
	after   string
	started bool
	page    authz.PageToken
	// order is the text form of the order of the list, and values the encoded position of each order field, in that
	// order (encodePosition).
	order  string
	values string
	// filter is the digest of the filter of the list (filterDigest), so a cursor is refused for another filter
	// (ST-SUP-RP11-LC20).
	filter string
}

// ListRequest is one page of a list: at most Limit allowed values after the cursor. A Limit of 0 is 100; the most
// is 1000. Order is the order of the list: the declared fields that the query gives, each ascending or descending,
// and then the key, ascending, as the last tie-breaker; an empty Order is the order of the key (ST-SUP-RP11-LC18).
type ListRequest struct {
	Limit int
	After Cursor
	Order []OrderBy
	// Where keeps only the rows whose declared fields equal the given values, all of them (ST-SUP-RP11-LC20). Each
	// field is a column of the entity's Spec and each value goes to the statement as a parameter, never as text.
	Where []Equal
}

// Equal is one condition of the filter of a list: the column Field of the entity's Spec equals Value. Value is a
// string, a bool, an int, int32 or int64, or a float64; NULL matches no condition.
type Equal struct {
	Field string
	Value any
}

// MaxFilter is the most conditions of the filter of a list.
const MaxFilter = 8

// checkFilter refuses, before any row is read, a filter of more than MaxFilter conditions, a field that the entity
// does not declare or that the filter names twice, and a value of another type (ST-SUP-RP11-LC20). Its error names
// the field and not the value.
func (e *entity) checkFilter(where []Equal) error {
	if len(where) > MaxFilter {
		return fmt.Errorf("%w: a list of %s filters on at most %d fields", ErrBadRequest, e.spec.Name, MaxFilter)
	}
	seen := map[string]bool{}
	for _, w := range where {
		if !slices.Contains(e.spec.Columns, w.Field) || seen[w.Field] || e.spec.Fields[w.Field] != "" {
			return fmt.Errorf("%w: the entity %s declares no filter field %q that is not pii or secret, or the filter names it twice", ErrBadRequest, e.spec.Name, w.Field)
		}
		seen[w.Field] = true
		switch v := w.Value.(type) {
		case string:
			if strings.ContainsRune(v, 0) {
				return fmt.Errorf("%w: the filter value of the field %s of %s holds a NUL", ErrBadRequest, w.Field, e.spec.Name)
			}
		case bool, int, int32, int64, float64:
		default:
			return fmt.Errorf("%w: the filter value of the field %s of %s has a type that a filter does not take", ErrBadRequest, w.Field, e.spec.Name)
		}
	}
	return nil
}

// filterDigest is the text that a cursor holds of a filter: a SHA-256 digest of its conditions in field order, each
// with the type and the value, so that a cursor of one filter is refused for another, and the cursor stays short.
func filterDigest(where []Equal) string {
	if len(where) == 0 {
		return ""
	}
	sorted := slices.Clone(where)
	slices.SortFunc(sorted, func(a, b Equal) int { return strings.Compare(a.Field, b.Field) })
	h := sha256.New()
	for _, w := range sorted {
		fmt.Fprintf(h, "%d:%s%d:%T%d:%v", len(w.Field), w.Field, len(fmt.Sprintf("%T", w.Value)), w.Value, len(fmt.Sprint(w.Value)), w.Value)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// padKey is the key of the resource of the padding decision of a list call that read no row: no row has it.
const padKey = "00000000-0000-0000-0000-000000000000"

// filterSQL gives the predicate of a filter, " AND field = $n" for each condition, with its parameters numbered from
// first, and the values; with keys that are not nil, also " AND key = ANY($n)", so only the rows of those keys are
// read (ST-SUP-RP11-LC20).
func (e *entity) filterSQL(where []Equal, keys []string, first int) (string, []any) {
	var b strings.Builder
	args := make([]any, 0, len(where)+1)
	for i, w := range where {
		fmt.Fprintf(&b, " AND %s = $%d", w.Field, first+i)
		args = append(args, w.Value)
	}
	if keys != nil {
		fmt.Fprintf(&b, " AND %s = ANY($%d)", e.spec.Key, first+len(where))
		args = append(args, keys)
	}
	return b.String(), args
}

// MaxSearchKeys is the most keys that an ordered list of an entity of small expected cardinality takes from the
// resource search (ST-SUP-RP11-LC20).
const MaxSearchKeys = 10000

// ErrTooManyKeys is the error of an ordered list of an entity of small expected cardinality whose resource search
// gives more than MaxSearchKeys keys: the entity is not small, and the list fails closed (ST-SUP-RP11-LC20).
var ErrTooManyKeys = errors.New("query: the resource search gives more keys than an entity of small expected cardinality may have")

// overBound is the warning of a list over MaxSearchKeys: at most one log line in each overBoundEvery, which counts
// the lists over the bound since the last line, so a caller who repeats such a list cannot flood the log.
var overBound = &boundWarning{every: overBoundEvery, now: time.Now}

// overBoundEvery is the shortest time between two warnings of a list over MaxSearchKeys.
const overBoundEvery = time.Minute

// boundWarning logs a list over the bound at most once in each period every, with the count of lists since the
// last line.
type boundWarning struct {
	mu    sync.Mutex
	every time.Duration
	now   func() time.Time
	last  time.Time
	count int
}

func (b *boundWarning) warn(ctx context.Context, entity string) {
	b.mu.Lock()
	b.count++
	now := b.now()
	if !b.last.IsZero() && now.Sub(b.last) < b.every {
		b.mu.Unlock()
		return
	}
	count := b.count
	b.last, b.count = now, 0
	b.mu.Unlock()
	slog.WarnContext(ctx, "query: an entity of small expected cardinality has more visible rows than the bound; declare it large",
		"entity", entity, "bound", MaxSearchKeys, "lists", count)
}

// searchKeys gives every key that the resource search gives for the list permission, page by page, and refuses more
// than MaxSearchKeys with ErrTooManyKeys, which it logs with the entity and the bound so that an operator sees a
// wrong cardinality declaration. It never falls back to paging the table.
func (r *Repository[T]) searchKeys(ctx context.Context) ([]string, error) {
	c := r.core
	e := &r.entity.e
	keys := []string{}
	var token authz.PageToken
	for {
		found, err := c.authz.Search(ctx, r.permission(), e.spec.ResourceType, nil, authz.Page{Token: token, Limit: MaxLimit})
		if errors.Is(err, authz.ErrNoSearch) {
			return nil, err
		}
		if err != nil {
			return nil, ErrUndecided
		}
		keys = append(keys, found.Keys...)
		if len(keys) > MaxSearchKeys {
			overBound.warn(ctx, e.spec.Name)
			return nil, fmt.Errorf("%w: the entity %s, more than %d keys", ErrTooManyKeys, e.spec.Name, MaxSearchKeys)
		}
		if found.Next.IsZero() {
			return keys, nil
		}
		token = found.Next
	}
}

// OrderBy is one field of the order of a list: a column of the entity's Spec, and its direction. NULL values come
// after every other value, in either direction.
type OrderBy struct {
	Field      string
	Descending bool
}

// orderText is the text form of an order, which a cursor holds so that it is refused for another order.
func orderText(order []OrderBy) string {
	var b strings.Builder
	for _, o := range order {
		b.WriteString(o.Field)
		if o.Descending {
			b.WriteString(" desc,")
		} else {
			b.WriteString(" asc,")
		}
	}
	return b.String()
}

// ListResult is one page of a list: the allowed values in the order of their key, and when More is true the cursor
// of the next page. A list by resource search orders each page by key, and its pages follow the provider's order of
// the tenant-qualified resource IDs (ST-SUP-RP06-LC22), which is not the order of the keys. More can be true on a page with fewer values than its limit, or none, when the call read its
// ScanLimit rows: the fill goes on with Next.
type ListResult[T any] struct {
	Items []Loaded[T]
	Next  Cursor
	More  bool
}

// MaxLimit is the most values that one list page returns.
const MaxLimit = 1000

// ScanLimit is the most rows of the table that one list call reads, and MinChunk the fewest rows of one read
// (ST-SUP-RP11-LC07).
const (
	ScanLimit = 1000
	MinChunk  = 100
)

// List reads one page of the entity's rows in the tenant (ST-SUP-RP11-LC03, ST-SUP-RP11-LC07). For a protected
// entity it holds only the rows that the principal may see on the query's declared permission for a list query,
// and on the permission that the entity declares for list otherwise: for an entity with a small expected
// cardinality, the provider's resource search gives the rows, also for a filtered list; otherwise, and for a list in
// an order of declared fields, List pages the table and filters each page with one batch evaluation. Each search and each batch runs at least as fresh as the revocation floor read
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
	if err := e.checkFilter(request.Where); err != nil {
		return ListResult[T]{}, err
	}
	filter := filterDigest(request.Where)
	if (request.After.started || !request.After.page.IsZero()) && request.After.filter != filter {
		return ListResult[T]{}, ErrBadCursor
	}
	if len(request.Order) > 0 {
		// An order of declared fields reads the table in that order, with the key last (ST-SUP-RP11-LC18); a resource
		// search has the provider's order, so it is not used.
		if err := e.checkOrder(request.Order); err != nil {
			return ListResult[T]{}, err
		}
		if request.After.started && request.After.order != orderText(request.Order) {
			return ListResult[T]{}, ErrBadCursor
		}
		if _, err := decodePosition(request.After.values, len(request.Order), request.After.started); err != nil {
			return ListResult[T]{}, err
		}
		// An entity of small expected cardinality reads only the rows of the keys that the resource search gives, in
		// the order of the list (ST-SUP-RP11-LC20).
		var keys []string
		if e.spec.ResourceType != "" && e.spec.Search && c.authz != nil {
			found, err := r.searchKeys(ctx)
			if err != nil && !errors.Is(err, authz.ErrNoSearch) {
				return ListResult[T]{}, err
			}
			keys = found
		}
		return r.tablePage(ctx, limit, request.After, request.Order, request.Where, keys)
	}
	if request.After.order != "" {
		return ListResult[T]{}, ErrBadCursor
	}
	// An entity of small expected cardinality lists through the provider's resource search, also with a filter: the
	// search gives the rows that the principal may see, and the filter keeps those that match, so the work of a call
	// does not grow with hidden rows that match (ST-SUP-RP11-LC20, ST-SUP-Q111 c). A provider with no search pages
	// the table.
	if e.spec.ResourceType != "" && e.spec.Search && c.authz != nil {
		result, err := r.searchPage(ctx, limit, request.After, request.Where)
		if !errors.Is(err, authz.ErrNoSearch) {
			return result, err
		}
	}
	return r.tablePage(ctx, limit, request.After, nil, request.Where, nil)
}

// MaxOrder is the most fields of the order of a list.
const MaxOrder = 4

// checkOrder refuses an order field that the entity does not declare, a field given twice and more than MaxOrder
// fields, before any row is read (ST-SUP-RP11-LC18).
func (e *entity) checkOrder(order []OrderBy) error {
	if len(order) > MaxOrder {
		return fmt.Errorf("query: a list of %s orders by at most %d fields", e.spec.Name, MaxOrder)
	}
	seen := map[string]bool{}
	for _, o := range order {
		if !slices.Contains(e.spec.Columns, o.Field) || seen[o.Field] || e.spec.Fields[o.Field] != "" {
			return fmt.Errorf("%w: the entity %s declares no order field %q, or the order names it twice", ErrBadRequest, e.spec.Name, o.Field)
		}
		seen[o.Field] = true
	}
	return nil
}

// orderedStatement gives the statement of one chunk of a list in order, and its arguments after the tenant: the
// rows after the position values and key, or from the start when started is false, in the order of the fields and
// then of the key, with NULL values last in either direction. A position value that is NULL has no row after it on
// its field but the rows that are NULL there too.
func (e *entity) orderedStatement(order []OrderBy, started bool, values []any, key string, size int, where []Equal, keys []string) (string, []any) {
	var by []string
	for _, o := range order {
		if o.Descending {
			by = append(by, o.Field+" DESC NULLS LAST")
		} else {
			by = append(by, o.Field+" ASC NULLS LAST")
		}
	}
	by = append(by, e.spec.Key+" ASC")
	args := []any{}
	param := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args)+1)
	}
	after := ""
	if started {
		var alternatives, equal []string
		for i, o := range order {
			if values[i] == nil {
				// Only NULL is at or after NULL, which is last.
				equal = append(equal, o.Field+" IS NULL")
				continue
			}
			p := param(values[i])
			cmp := " > "
			if o.Descending {
				cmp = " < "
			}
			after := "(" + o.Field + " IS NULL OR " + o.Field + cmp + p + ")"
			alternatives = append(alternatives, "("+strings.Join(append(slices.Clone(equal), after), " AND ")+")")
			equal = append(equal, o.Field+" = "+p)
		}
		last := e.spec.Key + " > " + param(key)
		alternatives = append(alternatives, "("+strings.Join(append(equal, last), " AND ")+")")
		after = " AND (" + strings.Join(alternatives, " OR ") + ")"
	}
	filter, filterArgs := e.filterSQL(where, keys, len(args)+2)
	args = append(args, filterArgs...)
	limit := param(size)
	tail := " ORDER BY " + strings.Join(by, ", ") + " LIMIT " + limit
	if keys != nil {
		// The rows of the searched keys are read by key first, at most MaxSearchKeys of them, and only then put in
		// order, so the planner cannot walk an index of the order field and test each row it passes, hidden rows
		// included, against the keys: the work depends on the visible rows only (ST-SUP-RP11-LC20).
		return "WITH v AS MATERIALIZED (SELECT * FROM " + e.table + " WHERE " + TenantColumn + " = $1" + filter + ") SELECT " +
			e.selectList + " FROM v WHERE true" + after + tail, args
	}
	return "SELECT " + e.selectList + " FROM " + e.table + " WHERE " + TenantColumn + " = $1" + filter + after + tail, args
}

// position gives the encoded position values of a row on the fields of order, which a cursor of the list holds:
// for each value a tag and its bytes, so the cursor gives it back with its type.
func position(row Row, order []OrderBy) (string, error) {
	var b []byte
	for _, o := range order {
		v, _ := row.Value(o.Field)
		switch x := v.(type) {
		case nil:
			b = append(b, 'n')
		case string:
			b = appendText(append(b, 's'), x)
		case []byte:
			b = appendText(append(b, 'b'), string(x))
		case int64:
			b = binary.BigEndian.AppendUint64(append(b, 'i'), uint64(x))
		case float64:
			b = binary.BigEndian.AppendUint64(append(b, 'f'), math.Float64bits(x))
		case bool:
			if x {
				b = append(b, 't')
			} else {
				b = append(b, 'F')
			}
		case time.Time:
			text, err := x.MarshalBinary()
			if err != nil {
				return "", err
			}
			b = appendText(append(b, 'T'), string(text))
		default:
			return "", fmt.Errorf("query: the order field %s holds a value of a type that a cursor cannot hold", o.Field)
		}
	}
	return string(b), nil
}

// decodePosition reads the n position values that position encoded, and refuses with ErrBadCursor a text that does
// not hold exactly n. A cursor that has not started holds none.
func decodePosition(text string, n int, started bool) ([]any, error) {
	if !started {
		if text != "" {
			return nil, ErrBadCursor
		}
		return nil, nil
	}
	b := []byte(text)
	values := make([]any, 0, n)
	for len(b) > 0 {
		tag := b[0]
		b = b[1:]
		switch tag {
		case 'n':
			values = append(values, nil)
		case 't', 'F':
			values = append(values, tag == 't')
		case 'i', 'f':
			if len(b) < 8 {
				return nil, ErrBadCursor
			}
			u := binary.BigEndian.Uint64(b)
			b = b[8:]
			if tag == 'i' {
				values = append(values, int64(u))
			} else {
				values = append(values, math.Float64frombits(u))
			}
		case 's', 'b', 'T':
			text, rest, ok := readText(b)
			if !ok {
				return nil, ErrBadCursor
			}
			b = rest
			switch tag {
			case 's':
				values = append(values, text)
			case 'b':
				values = append(values, []byte(text))
			default:
				var t time.Time
				if err := t.UnmarshalBinary([]byte(text)); err != nil {
					return nil, ErrBadCursor
				}
				values = append(values, t)
			}
		default:
			return nil, ErrBadCursor
		}
	}
	if len(values) != n {
		return nil, ErrBadCursor
	}
	return values, nil
}

// permission is the permission of a list of the entity: the query's declared permission when the query lists this
// entity, and the entity's list permission otherwise.
func (r *Repository[T]) permission() string {
	if q := r.core.query; q.listEntity == &r.entity.e {
		return q.listPermission
	}
	return r.entity.e.spec.ListPermission
}

// tablePage pages the table by its key, or in order and then by its key, and keeps the allowed rows of each page,
// until it holds limit values or the table ends.
func (r *Repository[T]) tablePage(ctx context.Context, limit int, after Cursor, order []OrderBy, where []Equal, keys []string) (ListResult[T], error) {
	c := r.core
	e := &r.entity.e
	var list *authz.List
	if e.spec.ResourceType != "" {
		list = c.authz.List(r.permission(), nil)
	}
	// The first page has no key predicate, so a key of any type, such as uuid, needs no lowest value.
	// The filter's values are the parameters after the tenant ($2 on), and then come the key and the limit.
	filter, filterArgs := e.filterSQL(where, keys, 2)
	n := len(filterArgs) + 2
	first := "SELECT " + e.selectList + " FROM " + e.table + " WHERE " + TenantColumn + " = $1" + filter + " ORDER BY " +
		e.spec.Key + " LIMIT $" + strconv.Itoa(n)
	next := "SELECT " + e.selectList + " FROM " + e.table + " WHERE " + TenantColumn + " = $1" + filter + " AND " +
		e.spec.Key + " > $" + strconv.Itoa(n) + " ORDER BY " + e.spec.Key + " LIMIT $" + strconv.Itoa(n+1)
	digest := filterDigest(where)
	// One call reads at most ScanLimit rows, in chunks of at least MinChunk, so a principal who may see few rows
	// never makes one call read the whole table (ST-SUP-RP11-LC07).
	chunk := max(limit, MinChunk)
	var result ListResult[T]
	key, started, values := after.after, after.started, after.values
	ordered := orderText(order)
	if len(order) == 0 {
		ordered = ""
	}
	scanned := 0
	// A protected list makes at least one batch decision in each call, also when no row of the table matches, so
	// the cost of a call does not tell whether rows that the principal may not see match its filter
	// (ST-SUP-RP11-LC20).
	batches := 0
	pad := func() error {
		if list == nil || batches > 0 {
			return nil
		}
		resource, err := c.resource(e, padKey)
		if err != nil {
			return err
		}
		_, err = list.Page(ctx, []authz.Row{{Resource: resource}})
		return err
	}
	for {
		size := min(chunk, ScanLimit-scanned)
		var rows *sql.Rows
		var err error
		switch {
		case len(order) > 0:
			at, err := decodePosition(values, len(order), started)
			if err != nil {
				return ListResult[T]{}, err
			}
			statement, args := e.orderedStatement(order, started, at, key, size, where, keys)
			rows, err = c.tx.QueryContext(ctx, statement, append([]any{c.tenant.ID()}, args...)...)
		case started:
			rows, err = c.tx.QueryContext(ctx, next, append(append([]any{c.tenant.ID()}, filterArgs...), key, size)...)
		default:
			rows, err = c.tx.QueryContext(ctx, first, append(append([]any{c.tenant.ID()}, filterArgs...), size)...)
		}
		if err != nil {
			return ListResult[T]{}, fmt.Errorf("query: list %s: %w", e.spec.Name, err)
		}
		page, tokens, err := readRows(rows, e)
		if err != nil {
			return ListResult[T]{}, err
		}
		if len(page) == 0 {
			if err := pad(); err != nil {
				return ListResult[T]{}, ErrUndecided
			}
			return result, nil
		}
		scanned += len(page)
		ids := make([]string, len(page))
		for i, row := range page {
			if ids[i], err = row.Text(e.spec.Key); err != nil {
				return ListResult[T]{}, err
			}
		}
		positions := make([]string, len(page))
		if len(order) > 0 {
			for i, row := range page {
				if positions[i], err = position(row, order); err != nil {
					return ListResult[T]{}, err
				}
			}
			values = positions[len(page)-1]
		}
		key, started = ids[len(ids)-1], true
		allowed := map[string]bool{}
		if list != nil {
			candidates := make([]authz.Row, len(page))
			for i := range page {
				resource, err := c.resource(e, ids[i])
				if err != nil {
					return ListResult[T]{}, err
				}
				candidates[i] = authz.Row{Resource: resource, Token: tokens[i]}
			}
			batches++
			kept, err := list.Page(ctx, candidates)
			if err != nil {
				// Each row of personal data that the page read and could not decide has its failed read row
				// (ST-SUP-RP11-LC16).
				for _, id := range ids {
					if err := c.auditedFailure(ctx, e, id, audit.ErrorCodeUndecided); err != nil {
						return ListResult[T]{}, err
					}
				}
				return ListResult[T]{}, ErrUndecided
			}
			// The kept rows are mapped back to their keys by position in candidates, never by parsing an ID.
			byID := make(map[string]string, len(candidates))
			for i, candidate := range candidates {
				byID[candidate.Resource.ID] = ids[i]
			}
			for _, row := range kept {
				allowed[byID[row.Resource.ID]] = true
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
			if err := c.audited(ctx, e, loaded.ID(), row); err != nil {
				return ListResult[T]{}, err
			}
			result.Items = append(result.Items, loaded)
			if len(result.Items) == limit {
				result.Next, result.More = Cursor{after: ids[i], started: true, order: ordered, values: positions[i], filter: digest}, true
				return result, nil
			}
		}
		if len(page) < size {
			if err := pad(); err != nil {
				return ListResult[T]{}, ErrUndecided
			}
			return result, nil
		}
		if scanned >= ScanLimit {
			// The scan budget of the call is spent: the next call goes on after the last row read.
			result.Next, result.More = Cursor{after: key, started: true, order: ordered, values: values, filter: digest}, true
			return result, nil
		}
	}
}

// searchPage asks the provider for one page of the resources the principal may see and reads their rows in the
// tenant. The authorization handle maps each result back to its row key, in Keys, only when it is of this tenant and
// this resource type, and drops every other result (ST-SUP-RP06-LC22).
func (r *Repository[T]) searchPage(ctx context.Context, limit int, after Cursor, where []Equal) (ListResult[T], error) {
	c := r.core
	e := &r.entity.e
	found, err := c.authz.Search(ctx, r.permission(), e.spec.ResourceType, nil, authz.Page{Token: after.page, Limit: limit})
	if errors.Is(err, authz.ErrNoSearch) {
		return ListResult[T]{}, err
	}
	if err != nil {
		// A provider that fails gives the same error as an undecided read.
		return ListResult[T]{}, ErrUndecided
	}
	var result ListResult[T]
	if !found.Next.IsZero() {
		result.Next, result.More = Cursor{page: found.Next, filter: filterDigest(where)}, true
	}
	if len(found.Keys) == 0 {
		return result, nil
	}
	ids := found.Keys
	// The filter reads only the rows of the search, so a page may hold fewer values than the limit and still have
	// more after it.
	filter, filterArgs := e.filterSQL(where, nil, 3)
	rows, err := c.tx.QueryContext(ctx, "SELECT "+e.selectList+" FROM "+e.table+" WHERE "+TenantColumn+" = $1 AND "+
		e.spec.Key+" = ANY($2)"+filter+" ORDER BY "+e.spec.Key, append([]any{c.tenant.ID(), ids}, filterArgs...)...)
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
		if err := c.audited(ctx, e, loaded.ID(), row); err != nil {
			return ListResult[T]{}, err
		}
		result.Items = append(result.Items, loaded)
	}
	return result, nil
}
