package query

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"stego.dev/runtime/audit"
	"stego.dev/runtime/authz"
	"stego.dev/runtime/tenant"
)

// ErrClosed is the error of every method of a handle, of its repositories and of its authorization part after the
// fill returned or panicked (ST-SUP-RP11-LC09). Such a call reads nothing and decides nothing.
var ErrClosed = errors.New("query: the handle is used after its fill returned")

// ErrNotFound is the error of a read of a row that does not exist in the tenant, or that the principal may not
// read: a deny is not found, with no data from the row (ST-SUP-RP11-LC06).
var ErrNotFound = errors.New("query: not found")

// ErrNotRead is the error of Repo for an entity that the query does not declare it reads (ST-SUP-RP11-LC02).
var ErrNotRead = errors.New("query: the query does not read this entity")

// TenantColumn is the tenant column of every tenant-owned table. Each read adds the tenant predicate on it
// (ST-STD-TXN-R01), in addition to the row-level security policy of the table.
const TenantColumn = "tenant_id"

// DefaultTokenColumn is the column of the authorization token of a protected row (ST-SUP-RP06-LC04).
const DefaultTokenColumn = "authz_token"

var plainName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// Spec is the declaration of one entity as the generated code gives it to NewEntity: where its rows are, which
// columns its loaded value carries, how it is authorized and which of its fields are personal data.
type Spec struct {
	// Name is the entity name of the declaration, such as reservation. A query names the entities it reads by it.
	Name string
	// Schema and Table name the tenant-owned table of the entity. An empty schema is public.
	Schema string
	Table  string
	// Key is the column of the entity's ID, which is unique in the tenant. It is one of Columns.
	Key string
	// Columns are the stored columns that the loaded value carries, in the order the generated load reads them.
	Columns []string
	// ResourceType is the authorization resource type of the entity. Empty means the entity is not protected: a
	// declaration that binds an authorization provider gives every entity one (ST-STD-TXN-R18).
	ResourceType string
	// ReadPermission is the permission that the entity declares for read; ListPermission the one for list.
	ReadPermission string
	ListPermission string
	// TokenColumn is the column of the row's authorization token; empty is DefaultTokenColumn.
	TokenColumn string
	// Fields are the sensitivities of the entity's fields by column name. An entity with a pii field is audited on
	// each read (ST-SUP-RP11-LC08).
	Fields audit.Fields
	// Search is true for an entity with a small expected cardinality: its list filters with a resource search of
	// the provider. Otherwise a list pages the table and filters each page with one batch evaluation
	// (ST-SUP-RP11-LC07).
	Search bool
}

// Row is one stored row as the generated load function reads it: the value of each column of the entity's Spec.
// Its values are those of the database/sql driver: string, int64, float64, bool, time.Time, []byte or nil.
type Row struct {
	columns []string
	values  []any
}

// Value returns the value of the column, and false when the entity's Spec does not name it.
func (r Row) Value(column string) (any, bool) {
	for i, name := range r.columns {
		if name == column {
			return r.values[i], true
		}
	}
	return nil, false
}

// Text returns the value of a text column; NULL is the empty text.
func (r Row) Text(column string) (string, error) {
	v, ok := r.Value(column)
	if !ok {
		return "", fmt.Errorf("query: the row has no column %s", column)
	}
	switch x := v.(type) {
	case nil:
		return "", nil
	case string:
		return x, nil
	case []byte:
		return string(x), nil
	default:
		return "", fmt.Errorf("query: the column %s is not text", column)
	}
}

// Int returns the value of an integer column; NULL is 0.
func (r Row) Int(column string) (int64, error) {
	v, ok := r.Value(column)
	if !ok {
		return 0, fmt.Errorf("query: the row has no column %s", column)
	}
	switch x := v.(type) {
	case nil:
		return 0, nil
	case int64:
		return x, nil
	default:
		return 0, fmt.Errorf("query: the column %s is not an integer", column)
	}
}

// Bool returns the value of a bool column; NULL is false.
func (r Row) Bool(column string) (bool, error) {
	v, ok := r.Value(column)
	if !ok {
		return false, fmt.Errorf("query: the row has no column %s", column)
	}
	switch x := v.(type) {
	case nil:
		return false, nil
	case bool:
		return x, nil
	default:
		return false, fmt.Errorf("query: the column %s is not a bool", column)
	}
}

// Time returns the value of a time column; NULL is the zero time.
func (r Row) Time(column string) (time.Time, error) {
	v, ok := r.Value(column)
	if !ok {
		return time.Time{}, fmt.Errorf("query: the row has no column %s", column)
	}
	switch x := v.(type) {
	case nil:
		return time.Time{}, nil
	case time.Time:
		return x, nil
	default:
		return time.Time{}, fmt.Errorf("query: the column %s is not a time", column)
	}
}

// state is the state of an entity for the audit row of a read: each column and its value.
func (r Row) state() map[string]any {
	m := make(map[string]any, len(r.columns))
	for i, name := range r.columns {
		m[name] = r.values[i]
	}
	return m
}

// entity is the part of an entity's declaration that does not depend on its Go type.
type entity struct {
	spec       Spec
	table      string // schema.table
	selectList string // the columns, then the token column of a protected entity
	pii        bool
}

// Readable is an entity that a query can declare it reads: an *Entity of any type. Its method is unexported, so only
// NewEntity makes one.
type Readable interface {
	info() *entity
}

// Entity is the declaration of one entity of type T, which NewEntity makes once for the generated code. Its load
// function is the generated code's own, which builds a T from a stored row; the handle calls it only on a row that
// it read through its bound transaction.
type Entity[T any] struct {
	e    entity
	load func(Row) (T, error)
}

func (e *Entity[T]) info() *entity { return &e.e }

// NewEntity checks spec and makes the declaration of an entity of type T, whose rows load gives as values of T.
func NewEntity[T any](spec Spec, load func(Row) (T, error)) (*Entity[T], error) {
	if load == nil {
		return nil, errors.New("query: an entity needs its load function")
	}
	if spec.Name == "" {
		return nil, errors.New("query: an entity needs a name")
	}
	if spec.Schema == "" {
		spec.Schema = "public"
	}
	if spec.TokenColumn == "" {
		spec.TokenColumn = DefaultTokenColumn
	}
	for _, name := range []string{spec.Schema, spec.Table, spec.Key, spec.TokenColumn} {
		if !plainName.MatchString(name) {
			return nil, fmt.Errorf("query: the entity %s names %q, which is not a plain lower-case name", spec.Name, name)
		}
	}
	seen := map[string]bool{}
	for _, column := range spec.Columns {
		if !plainName.MatchString(column) || seen[column] || column == TenantColumn {
			return nil, fmt.Errorf("query: the entity %s has the column %q twice, as the tenant column or not as a plain lower-case name", spec.Name, column)
		}
		seen[column] = true
	}
	if !seen[spec.Key] {
		return nil, fmt.Errorf("query: the key of the entity %s is not one of its columns", spec.Name)
	}
	if spec.ResourceType != "" && (spec.ReadPermission == "" || spec.ListPermission == "") {
		return nil, fmt.Errorf("query: the protected entity %s needs a read and a list permission", spec.Name)
	}
	selectList := strings.Join(spec.Columns, ", ")
	if spec.ResourceType != "" {
		selectList += ", " + spec.TokenColumn
	}
	pii := false
	for _, s := range spec.Fields {
		if s == audit.PII {
			pii = true
		}
	}
	spec.Columns = append([]string(nil), spec.Columns...)
	return &Entity[T]{e: entity{spec: spec, table: spec.Schema + "." + spec.Table, selectList: selectList, pii: pii},
		load: load}, nil
}

// Name is the entity name of the declaration.
func (e *Entity[T]) Name() string { return e.e.spec.Name }

// Loaded is an entity value that a read through a handle loaded: the stored field values of one row of type T, its
// ID and the token of the row. It has no exported field and no exported constructor, and the generated loaded type is
// a pointer to it, such as type LoadedReservation = *query.Loaded[Reservation], which no composite literal and no
// conversion from another type can make; so a fill can get a loaded value only from the handle
// (ST-SUP-RP11-LC12). A value that the handle did not load, such as one that new or a composite literal makes, and a
// copy of a loaded value carry no handle, and every operation that takes a loaded value refuses them.
type Loaded[T any] struct {
	self   *Loaded[T]
	value  T
	origin *core
	entity *entity
	id     string
	token  authz.Token
}

// newLoaded is the one constructor of a loaded value, for the reads of the handle.
func newLoaded[T any](origin *core, e *entity, id string, token authz.Token, value T) *Loaded[T] {
	l := &Loaded[T]{value: value, origin: origin, entity: e, id: id, token: token}
	l.self = l
	return l
}

// Value returns a copy of the stored field values.
func (l *Loaded[T]) Value() T {
	if l == nil {
		var zero T
		return zero
	}
	return l.value
}

// ID returns the entity's ID.
func (l *Loaded[T]) ID() string {
	if l == nil {
		return ""
	}
	return l.id
}

// row returns what the authorization part needs of the value: the handle that loaded it, its entity, its ID and
// its row token.
func (l *Loaded[T]) row() (*core, *entity, string, authz.Token) {
	// A copy of a loaded value, such as one that *l makes, is not the value that the handle loaded.
	if l == nil || l.self != l {
		return nil, nil, "", authz.Token{}
	}
	return l.origin, l.entity, l.id, l.token
}

// LoadedValue is a loaded value of any entity type, as the authorization part takes it. Only *Loaded implements it.
type LoadedValue interface {
	row() (*core, *entity, string, authz.Token)
}

// life is the lifetime of one handle: open while the fill runs, then closed for good (ST-SUP-RP11-LC09). Each call
// of the handle holds the read lock for its whole run, so close waits for a call that runs and then refuses every
// later call.
type life struct {
	mu     sync.RWMutex
	closed bool
}

func (l *life) enter() error {
	l.mu.RLock()
	if l.closed {
		l.mu.RUnlock()
		return ErrClosed
	}
	return nil
}

func (l *life) leave() { l.mu.RUnlock() }

func (l *life) close() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
}

// core is the part of a handle that does not depend on the probe type: the bound transaction, tenant and principal,
// the query, the authorization handle and the audit of the reads of personal data.
type core struct {
	life      life
	tx        *sql.Tx
	tenant    tenant.Tenant
	principal authz.Principal
	query     *Query
	reads     map[*entity]bool
	authz     *authz.Handle
	audit     *auditor
	requestID string
}

// Handle is the query handle that a query fill gets (ST-SUP-RP11-LC01): bound to one READ ONLY store transaction,
// one tenant and one principal, which it shows for reading only. It reaches storage only through the tenant-scoped
// repository reads of the entities its query declares it reads (Repo), decides only through its authorization part
// (Authz), gives the fill its query's typed probe (Probe), and writes nothing. It holds no exported field, and no
// method takes or returns a database value.
type Handle[P any] struct {
	core  *core
	probe P
	authz *Authz
}

// Tenant returns the tenant of the handle.
func (h *Handle[P]) Tenant() tenant.Tenant { return h.core.tenant }

// Principal returns the principal of the handle.
func (h *Handle[P]) Principal() authz.Principal { return h.core.principal }

// Probe returns the typed probe that the generated code built for the query and bound to the handle's span context,
// tenant, principal and query name (ST-SUP-RP11-LC11). It emits nothing once the fill returned.
func (h *Handle[P]) Probe() P { return h.probe }

// Authz returns the authorization part of the handle (ST-SUP-RP11-LC05).
func (h *Handle[P]) Authz() *Authz { return h.authz }

// bind makes the handle of tx, a READ ONLY transaction of tenant t that tenant.Begin opened, for principal p and the
// query q. It refuses a transaction that is not read only before the fill gets it (ST-SUP-RP11-LC04).
func bind[P any](ctx context.Context, rt *Runtime, tx *sql.Tx, t tenant.Tenant, p authz.Principal, q *Query, typed P) (*Handle[P], error) {
	switch {
	case tx == nil:
		return nil, errors.New("query: no transaction")
	case t.IsZero():
		return nil, tenant.ErrZero
	case p.IsZero():
		return nil, errors.New("query: no principal")
	case q == nil:
		return nil, errors.New("query: no query")
	}
	var readOnly string
	if err := tx.QueryRowContext(ctx, "SELECT pg_catalog.current_setting('transaction_read_only')").Scan(&readOnly); err != nil {
		return nil, fmt.Errorf("query: read the access mode of the transaction: %w", err)
	}
	if readOnly != "on" {
		return nil, errors.New("query: the transaction of a query is not READ ONLY")
	}
	var isolation string
	if err := tx.QueryRowContext(ctx, "SELECT pg_catalog.current_setting('transaction_isolation')").Scan(&isolation); err != nil {
		return nil, fmt.Errorf("query: read the isolation of the transaction: %w", err)
	}
	if isolation != "read committed" {
		return nil, errors.New("query: the transaction of a query is not READ COMMITTED")
	}
	c := &core{tx: tx, tenant: t, principal: p, query: q, reads: map[*entity]bool{}}
	for _, r := range q.reads {
		e := r.info()
		c.reads[e] = true
		if e.spec.ResourceType != "" && rt.authz == nil {
			return nil, fmt.Errorf("query: the query %s reads the protected entity %s and the runtime has no authorization", q.name, e.spec.Name)
		}
		if e.pii && rt.audit == nil {
			return nil, fmt.Errorf("query: the query %s reads the entity %s, which holds pii, and the runtime has no audit", q.name, e.spec.Name)
		}
	}
	if rt.authz != nil {
		handle, err := rt.authz.Bind(tx, t, p, nil)
		if err != nil {
			return nil, err
		}
		c.authz = handle
	}
	if rt.audit != nil {
		c.audit = &auditor{rt: rt, done: map[string]bool{}}
	}
	return &Handle[P]{core: c, probe: typed, authz: &Authz{core: c}}, nil
}

// Repository is the tenant-scoped read of one entity through one handle. Its methods read only in the handle's
// bound transaction, with the tenant predicate, decide each row through the handle's authorization part and audit
// each read of personal data.
type Repository[T any] struct {
	core   *core
	entity *Entity[T]
}

// Repo returns the repository of the entity e through the handle h. It refuses, with ErrNotRead and before any
// statement, an entity that the query does not declare it reads (ST-SUP-RP11-LC02).
func Repo[T, P any](h *Handle[P], e *Entity[T]) (*Repository[T], error) {
	if h == nil || h.core == nil || e == nil {
		return nil, errors.New("query: no handle or no entity")
	}
	if err := h.core.life.enter(); err != nil {
		return nil, err
	}
	defer h.core.life.leave()
	if !h.core.reads[&e.e] {
		return nil, fmt.Errorf("%w: %s", ErrNotRead, e.e.spec.Name)
	}
	return &Repository[T]{core: h.core, entity: e}, nil
}

// scanRow reads the current row of rows into a Row of the entity and the row token of a protected entity.
func scanRow(rows *sql.Rows, e *entity) (Row, authz.Token, error) {
	values := make([]any, len(e.spec.Columns))
	dest := make([]any, 0, len(values)+1)
	for i := range values {
		dest = append(dest, &values[i])
	}
	var token sql.NullString
	if e.spec.ResourceType != "" {
		dest = append(dest, &token)
	}
	if err := rows.Scan(dest...); err != nil {
		return Row{}, authz.Token{}, fmt.Errorf("query: read a row of %s: %w", e.spec.Name, err)
	}
	for i, v := range values {
		if b, ok := v.([]byte); ok {
			values[i] = append([]byte(nil), b...)
		}
	}
	t, err := authz.ParseToken(token.String)
	if err != nil {
		return Row{}, authz.Token{}, fmt.Errorf("query: the token of a row of %s: %w", e.spec.Name, err)
	}
	return Row{columns: e.spec.Columns, values: values}, t, nil
}

// load runs the generated load function on a row read through the handle and makes its loaded value.
func (r *Repository[T]) load(row Row, token authz.Token) (*Loaded[T], error) {
	id, err := row.Text(r.entity.e.spec.Key)
	if err != nil {
		return nil, err
	}
	value, err := r.entity.load(row)
	if err != nil {
		return nil, fmt.Errorf("query: load a row of %s: %w", r.entity.e.spec.Name, err)
	}
	return newLoaded(r.core, &r.entity.e, id, token, value), nil
}

// Get reads the entity whose ID is id (ST-SUP-RP11-LC03, ST-SUP-RP11-LC06). It reads the row in the bound
// transaction with the tenant predicate, then, for a protected entity, decides the entity's read permission on it
// at the floor through the authorization handle; a row that is not there and a deny are both ErrNotFound, with no
// data from the row. A read of an entity with a pii field writes its audit row before Get returns the value
// (ST-SUP-RP11-LC08).
func (r *Repository[T]) Get(ctx context.Context, id string) (*Loaded[T], error) {
	c := r.core
	if err := c.life.enter(); err != nil {
		return nil, err
	}
	defer c.life.leave()
	e := &r.entity.e
	rows, err := c.tx.QueryContext(ctx, "SELECT "+e.selectList+" FROM "+e.table+" WHERE "+TenantColumn+" = $1 AND "+
		e.spec.Key+" = $2", c.tenant.ID(), id)
	if err != nil {
		return nil, fmt.Errorf("query: read %s: %w", e.spec.Name, err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("query: read %s: %w", e.spec.Name, err)
		}
		return nil, ErrNotFound
	}
	row, token, err := scanRow(rows, e)
	if err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("query: read %s: %w", e.spec.Name, err)
	}
	// The decision is on the stored key of the row, not on the text the fill gave.
	stored, err := row.Text(e.spec.Key)
	if err != nil {
		return nil, err
	}
	allowed, err := c.allowRead(ctx, e, stored, token)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, ErrNotFound
	}
	loaded, err := r.load(row, token)
	if err != nil {
		return nil, err
	}
	if err := c.audited(ctx, e, loaded.id, row); err != nil {
		return nil, err
	}
	return loaded, nil
}

// readRows reads and closes rows.
func readRows(rows *sql.Rows, e *entity) ([]Row, []authz.Token, error) {
	defer rows.Close()
	var page []Row
	var tokens []authz.Token
	for rows.Next() {
		row, token, err := scanRow(rows, e)
		if err != nil {
			return nil, nil, err
		}
		page = append(page, row)
		tokens = append(tokens, token)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("query: list %s: %w", e.spec.Name, err)
	}
	return page, tokens, nil
}
