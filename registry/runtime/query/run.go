package query

import (
	"context"
	"crypto/cipher"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/probe"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// Config is what the query runtime of a service needs.
type Config struct {
	// DB is the store pool of the runtime role. It must be a pool that tenant.Open returned (ST-SUP-RP11-LC14).
	DB *sql.DB
	// AuditDB is the pool of the audit connections (ST-SUP-RP10-LC29), apart from DB, which only the short audit
	// transactions of the reads of personal data use. It must be a pool that tenant.Open returned. It may be nil only
	// when Audit is nil.
	AuditDB *sql.DB
	// Audit writes the read audit rows. It may be nil only for a service whose queries read no entity with a pii
	// field; Run refuses a query that reads one.
	Audit *audit.Writer
	// AuditSettings are the service's audit settings: at the change level a read row holds the redacted state.
	AuditSettings audit.Settings
	// Authz is the authorization runtime. It may be nil only for a service that binds no authorization provider;
	// Run refuses a query that reads a protected entity.
	Authz *authz.Runtime
	// Service is the service name that each audit row carries.
	Service string
	// CursorKey is the key of the MAC of the text form of a list cursor (ST-SUP-RP11-LC15): at least MinCursorKey
	// random bytes, the same in every replica of the service, and secret. Without it, a cursor has no text form.
	CursorKey []byte
}

// Runtime is the query runtime of one service: it runs query fills, each in a READ ONLY transaction of its own.
type Runtime struct {
	db       *sql.DB
	auditDB  *sql.DB
	audit    *audit.Writer
	settings audit.Settings
	authz    *authz.Runtime
	service  string
	dropped  atomic.Uint64

	cursorAEAD cipher.AEAD

	// afterFill, when a test sets it, runs after the fill returned and the handle closed, before Run ends the
	// transaction.
	afterFill func()
}

// New returns the query runtime of cfg. It refuses a store pool or an audit pool that tenant.Open did not return,
// so a connection that comes back to the pool with a tenant set is closed (ST-SUP-RP11-LC14, ST-SUP-RP04-LC04).
func New(cfg Config) (*Runtime, error) {
	if cfg.DB == nil {
		return nil, errors.New("query: no store pool")
	}
	if !tenant.Opened(cfg.DB) {
		return nil, errors.New("query: the store pool was not opened with tenant.Open")
	}
	if cfg.Audit != nil {
		if cfg.AuditDB == nil {
			return nil, errors.New("query: the audit writer needs the audit connections")
		}
		if !tenant.Opened(cfg.AuditDB) {
			return nil, errors.New("query: the audit pool was not opened with tenant.Open")
		}
		if cfg.AuditDB == cfg.DB {
			return nil, errors.New("query: the audit connections must be a pool apart from the store pool")
		}
		if cfg.Service == "" {
			return nil, errors.New("query: the audit rows need the service name")
		}
	}
	if len(cfg.CursorKey) > 0 && len(cfg.CursorKey) < MinCursorKey {
		return nil, fmt.Errorf("query: the cursor key holds fewer than %d bytes", MinCursorKey)
	}
	if len(cfg.CursorKey) > 0 && cfg.Service == "" {
		// The service is associated data of each cursor: without it, two services with one key read each other's.
		return nil, errors.New("query: a cursor key needs the service name")
	}
	rt := &Runtime{db: cfg.DB, auditDB: cfg.AuditDB, audit: cfg.Audit, settings: cfg.AuditSettings, authz: cfg.Authz,
		service: cfg.Service}
	if len(cfg.CursorKey) > 0 {
		aead, err := newCursorAEAD(cfg.CursorKey)
		if err != nil {
			return nil, err
		}
		rt.cursorAEAD = aead
	}
	return rt, nil
}

// DroppedSends counts the messages of server streams that a deny dropped (ST-SUP-RP11-LC13).
func (rt *Runtime) DroppedSends() uint64 { return rt.dropped.Load() }

// Query is the declaration of one query as the generated code gives it to Run: its name, the entities it reads and,
// for a list query, the entity it lists with the query's declared permission.
type Query struct {
	name           string
	reads          []Readable
	listEntity     *entity
	listPermission string
}

// NewQuery makes the declaration of the query named name, which reads the entities reads. A list of one of them
// decides the permission that the entity declares for list.
func NewQuery(name string, reads ...Readable) (*Query, error) {
	if name == "" {
		return nil, errors.New("query: a query needs a name")
	}
	seen := map[string]bool{}
	for _, r := range reads {
		if r == nil || r.info() == nil {
			return nil, fmt.Errorf("query: the query %s reads a nil entity", name)
		}
		if seen[r.info().spec.Name] {
			return nil, fmt.Errorf("query: the query %s reads the entity %s twice", name, r.info().spec.Name)
		}
		seen[r.info().spec.Name] = true
	}
	return &Query{name: name, reads: append([]Readable(nil), reads...)}, nil
}

// NewListQuery makes the declaration of the list query named name, which lists the entity lists on its declared
// permission and also reads the entities reads. A list of lists decides permission; a list of another entity decides
// the permission that the entity declares for list (ST-SUP-RP11-LC07).
func NewListQuery(name string, lists Readable, permission string, reads ...Readable) (*Query, error) {
	if lists == nil || lists.info() == nil || permission == "" {
		return nil, fmt.Errorf("query: the list query %s needs the entity it lists and its permission", name)
	}
	q, err := NewQuery(name, append([]Readable{lists}, reads...)...)
	if err != nil {
		return nil, err
	}
	q.listEntity, q.listPermission = lists.info(), permission
	return q, nil
}

// Name is the query name.
func (q *Query) Name() string { return q.name }

// Bound is what the generated handler of one call gives Run: the verified tenant and principal, the probe call that
// it bound to the span context, the tenant, the principal and the query name, the constructor of the query's typed
// probe, which Run calls on that call's Probe, so the fill's probe is always the one of this call, and the request
// ID that the audit rows carry.
type Bound[P any] struct {
	Tenant    tenant.Tenant
	Principal authz.Principal
	Call      *probe.Call
	Probe     func(*probe.Probe) P
	RequestID string
}

// Fill is a query fill: it gets the decoded request and the query handle, and returns the response.
type Fill[Req, Res, P any] func(ctx context.Context, req Req, h *Handle[P]) (Res, error)

// errPanic is the error of a fill that panicked or did not return normally, such as one that called
// runtime.Goexit. It does not carry the panic value (ST-SUP-RP11-LC10).
var errPanic = errors.New("query: the fill panicked")

// txOptions are the options of the transaction of a query: READ ONLY, at READ COMMITTED, so each statement and each
// floor read sees the newest committed revocation.
var txOptions = &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelReadCommitted}

// StatementTimeout is the most time that one statement of a query runs, and statementTimeoutSQL its setting in the
// query's transaction.
const (
	StatementTimeout    = 5 * time.Second
	statementTimeoutSQL = "SET LOCAL statement_timeout = '5s'"
)

// Run runs one query: it opens a READ ONLY transaction of the tenant at READ COMMITTED with tenant.Begin, binds the
// handle, calls the fill once, and makes the handle and its probe invalid as soon as the fill returns, panics or
// leaves through runtime.Goexit, before it ends the transaction (ST-SUP-RP11-LC09, ST-SUP-RP11-LC11). When the fill
// returns an error or does not return normally, Run rolls the transaction back and returns no result, with an error
// that does not carry the panic value (ST-SUP-RP11-LC10). Otherwise it commits with tenant.Commit
// (ST-SUP-RP11-LC14) and returns the result. Each read of personal data has its audit row before Run returns, and a
// failed audit write gives no result (ST-SUP-RP11-LC08).
func Run[Req, Res, P any](ctx context.Context, rt *Runtime, q *Query, b Bound[P], req Req, fill Fill[Req, Res, P]) (Res, error) {
	var zero Res
	if fill == nil {
		return zero, errors.New("query: no fill")
	}
	var res Res
	err := runFill(ctx, rt, q, b, func(ctx context.Context, h *Handle[P]) error {
		var err error
		res, err = fill(ctx, req, h)
		return err
	})
	if err != nil {
		return zero, err
	}
	return res, nil
}

// runFill is the part of Run and RunStream that does not depend on the request and the response: the transaction,
// the handle, the call of the fill in body, the end of the handle and of the probe, the audit check and the end of
// the transaction. The end of the handle, of the probe and of a transaction that did not commit is in a deferred
// function, so it also runs when the fill leaves through runtime.Goexit.
func runFill[P any](ctx context.Context, rt *Runtime, q *Query, b Bound[P], body func(context.Context, *Handle[P]) error) error {
	switch {
	case rt == nil:
		return errors.New("query: no runtime")
	case q == nil:
		return errors.New("query: no query")
	case b.Call == nil || b.Probe == nil:
		return errors.New("query: no probe call or no probe constructor")
	}
	bound := b.Call.Probe()
	if bound.Tenant() != b.Tenant.ID() || bound.Principal() != b.Principal.Subject().String() || bound.Operation() != q.name {
		b.Call.Returned()
		b.Call.Rollback()
		return errors.New("query: the probe call is not bound to the tenant, the principal and the name of the query")
	}
	tx, err := tenant.Begin(ctx, rt.db, b.Tenant, txOptions)
	if err != nil {
		b.Call.Returned()
		b.Call.Rollback()
		return err
	}
	// Each statement of the query ends within StatementTimeout, so a list with a filter that matches few rows of a
	// large table cannot hold the connection (ST-SUP-RP11-LC07, ST-SUP-RP11-LC20).
	if _, err := tx.ExecContext(ctx, statementTimeoutSQL); err != nil {
		_ = tx.Rollback()
		b.Call.Returned()
		b.Call.Rollback()
		return err
	}
	var h *Handle[P]
	committed := false
	defer func() {
		if h != nil {
			h.core.life.close()
		}
		b.Call.Returned()
		if !committed {
			_ = tx.Rollback()
			b.Call.Rollback()
		}
	}()
	h, err = bind(ctx, rt, tx, b.Tenant, b.Principal, q, b.Probe(bound))
	if err != nil {
		return err
	}
	h.core.requestID = b.RequestID
	fillErr := callFill(ctx, h, body)
	// The handle and the probe end as soon as the fill returns or panics, before the transaction ends.
	h.core.life.close()
	b.Call.Returned()
	if rt.afterFill != nil {
		rt.afterFill()
	}
	if fillErr == nil {
		fillErr = h.core.audit.failure()
	}
	if fillErr != nil {
		return fillErr
	}
	if err := tenant.Commit(ctx, tx, b.Tenant); err != nil {
		// tenant.Commit rolled back or tried to commit; the deferred rollback finds the transaction done.
		return err
	}
	committed = true
	b.Call.Commit()
	return nil
}

// callFill calls the fill in body on a goroutine of its own and waits for it. It turns a panic, or a body that does
// not return normally, such as one that calls runtime.Goexit, into errPanic, which does not carry the panic value;
// because only the fill's goroutine ends, Run still returns (ST-SUP-RP11-LC10).
func callFill[P any](ctx context.Context, h *Handle[P], body func(context.Context, *Handle[P]) error) error {
	done := make(chan error, 1)
	go func() {
		normal := false
		var err error
		defer func() {
			if r := recover(); r != nil || !normal {
				err = errPanic
			}
			done <- err
		}()
		err = body(ctx, h)
		normal = true
	}()
	return <-done
}

// auditor writes the read audit rows of one handle: one row for each resource of an entity with a pii field that
// the fill read, each in its own short transaction on the audit connections (ST-SUP-RP11-LC08, ST-SUP-RP05-LC02).
type auditor struct {
	rt     *Runtime
	mu     sync.Mutex
	done   map[string]bool
	failed error
}

// failure is the first audit write that failed, which makes the query give no result.
func (a *auditor) failure() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.failed
}

// auditedFailure writes the read audit row of a read of the resource id of e that failed with the fixed error code
// code (ST-SUP-RP11-LC16), when e holds a pii field: the action read, the outcome failed, the code and no state,
// so the row holds no error text and no data of the row. A failed write makes Run give no result, as for audited.
func (c *core) auditedFailure(ctx context.Context, e *entity, id, code string) error {
	if !e.pii {
		return nil
	}
	a := c.audit
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failed != nil {
		return a.failed
	}
	key := e.spec.Name + "\x00" + id + "\x00" + code
	if a.done[key] {
		return nil
	}
	event := c.readEvent(e, id)
	event.ErrorCode = code
	if _, err := a.rt.audit.RecordRead(ctx, a.rt.auditDB, c.tenant, event); err != nil {
		a.failed = fmt.Errorf("query: the read audit row of %s: %w", e.spec.Name, err)
		return a.failed
	}
	a.done[key] = true
	return nil
}

// readEvent is the read audit event of the resource id of e: the principal as the actor and the query name as the
// operation.
func (c *core) readEvent(e *entity, id string) audit.Event {
	subject := c.principal.Subject()
	kind := audit.ActorUser
	if subject.Type == authz.ServiceAccountType {
		kind = audit.ActorService
	}
	resourceType := e.spec.ResourceType
	if resourceType == "" {
		resourceType = e.spec.Name
	}
	return audit.Event{ActorID: subject.String(), ActorKind: kind, Operation: c.query.name, ResourceType: resourceType,
		ResourceID: id, Service: c.audit.rt.service, RequestID: c.requestID}
}

// audited writes the read audit row of the resource id of e, whose stored row is row, when e holds a pii field and
// the handle has not written one for that resource yet. The row has the action read, the principal as the actor, the
// query name as the operation and, at the change level, the redacted state. A failed write is returned and makes Run give no result, also when
// the fill does not return the error.
func (c *core) audited(ctx context.Context, e *entity, id string, row Row) error {
	if !e.pii {
		return nil
	}
	a := c.audit
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failed != nil {
		return a.failed
	}
	key := e.spec.Name + "\x00" + id
	if a.done[key] {
		return nil
	}
	event := c.readEvent(e, id)
	state := row.state()
	if err := event.SetStates(a.rt.settings, e.spec.Fields, state, state); err != nil {
		a.failed = fmt.Errorf("query: the read audit row of %s: %w", e.spec.Name, err)
		return a.failed
	}
	if _, err := a.rt.audit.RecordRead(ctx, a.rt.auditDB, c.tenant, event); err != nil {
		a.failed = fmt.Errorf("query: the read audit row of %s: %w", e.spec.Name, err)
		return a.failed
	}
	a.done[key] = true
	return nil
}
