package command

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"stego.dev/runtime/audit"
	"stego.dev/runtime/authz"
	"stego.dev/runtime/internal/loaded"
	"stego.dev/runtime/outbox"
	"stego.dev/runtime/tenant"
)

// The errors of the handle. A fill tells them apart with errors.Is.
var (
	// ErrConflict is the error of a write whose loaded value carries a version that the row no longer has, and of a
	// statement that ends with the SQLSTATE 40001, 40P01 or 55P03 or with the deadline (ST-SUP-RP10-LC24,
	// ST-SUP-RP10-LC25, ST-SUP-RP10-LC28). Once a handle meets it, every later method returns it, and Run treats the
	// call as a conflict whatever the fill returns.
	ErrConflict = errors.New("command: conflict")
	// ErrNotFound is the error of a read of a row that the tenant does not hold, or that the principal may not read
	// (ST-SUP-RP10-LC13): the two cases give the same answer and no data of the row.
	ErrNotFound = errors.New("command: not found")
	// ErrRefused is the error of a worker handle's read or write that the decision port denies (ST-SUP-RP10-LC17).
	ErrRefused = errors.New("command: the decision port refused the resource")
	// ErrClosed is the error of every method of a handle after its fill returned or panicked (ST-SUP-RP10-LC10).
	ErrClosed = errors.New("command: the handle is not valid after its fill returns")
	// ErrInternal matches every internal error of the runtime: a fault of the fill or of the generated code that a
	// rerun cannot change, such as a loaded value of another attempt (ST-SUP-RP10-LC24). Once a handle meets one,
	// every later method returns it, and Run ends the call with it and runs the fill no more.
	ErrInternal = errors.New("command: internal error")
)

// internalError is an internal error: it matches ErrInternal and holds no field value and no database text.
type internalError struct{ text string }

func (e *internalError) Error() string        { return "command: internal error: " + e.text }
func (e *internalError) Is(target error) bool { return target == ErrInternal }

func internalf(format string, args ...any) error {
	return &internalError{text: fmt.Sprintf(format, args...)}
}

// conflictError is a conflict: it matches ErrConflict and names the step and the SQLSTATE, never a database text.
type conflictError struct{ text string }

func (e *conflictError) Error() string        { return "command: conflict: " + e.text }
func (e *conflictError) Is(target error) bool { return target == ErrConflict }

// deniedError is a worker handle's deny: it matches ErrRefused (ST-SUP-RP10-LC17) and, for a read, also ErrNotFound,
// so a locked read of a denied row is not found as ST-SUP-RP10-LC23 states.
type deniedError struct {
	entity string
	read   bool
	// staging is true when the staging principal of a consumer was denied, and its service account allowed.
	staging bool
}

func (e *deniedError) Error() string {
	return "command: the decision port refused the resource of " + e.entity
}

func (e *deniedError) Is(target error) bool {
	return target == ErrRefused || (e.read && target == ErrNotFound)
}

// conflictStates are the SQLSTATE values that are a conflict (ST-SUP-RP10-LC25): serialization failure, deadlock and
// lock not available, which a lock timeout raises.
var conflictStates = map[string]bool{"40001": true, "40P01": true, "55P03": true}

// sqlState returns the SQLSTATE of a database error, as the PostgreSQL drivers and the audit runtime give it.
func sqlState(err error) string {
	var coded interface{ SQLState() string }
	if errors.As(err, &coded) {
		return coded.SQLState()
	}
	return ""
}

// isConflict reports whether err of a statement before COMMIT is a conflict: a conflict SQLSTATE, or a cancel or
// another context error, also when the driver closed the connection on the cancel (ST-SUP-RP10-LC28).
func isConflict(ctx context.Context, err error) bool {
	return conflictStates[sqlState(err)] || ctx.Err() != nil || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

// Scanner reads the columns of one row. *sql.Row and *sql.Rows satisfy it.
type Scanner interface {
	Scan(dest ...any) error
}

// Meta is what the generated scan of an entity reads from a row besides its fields: the ID, the resource version
// (ST-SUP-RC01-OC07) and the text form of the authorization token, empty while the relay has written none.
type Meta struct {
	ID      string
	Version int64
	Token   string
}

// EntitySpec is what the generated code declares of one entity that a command or a worker writes: its names, its
// permissions, the statements of its repository and the functions that read and write its values. The statements
// vary with the declaration, so they are generated (ST-STD-RUNTIME-R05); the runtime runs them in the order and
// with the checks that its contracts state, and never builds SQL text.
//
// Every statement takes the tenant of the transaction as $1, so its tenant predicate tenant_id = $1 and the
// row-level security policy both hold (ST-SUP-RP10-LC03):
//   - Select, SelectForUpdate and SelectForDelete read the row of ID $2, with no lock, with FOR NO KEY UPDATE and with
//     FOR UPDATE (ST-SUP-RP10-LC23);
//   - Insert writes a row from $2 on, the values that Values gives, and returns the row;
//   - Update writes the row of ID $2 only while its version is $3, from $4 on the values that Values gives, raises its
//     version by one and returns the row (ST-SUP-RP10-LC24);
//   - Delete removes the row of ID $2 only while its version is $3.
//
// Each statement that returns a row returns the columns that Scan reads.
type EntitySpec[T any] struct {
	// Name is the entity's name, such as Reservation. Errors name it.
	Name string
	// ResourceType is the authorization resource type of the entity, such as reservation, or empty for an entity
	// that has none: only an entity with a resource type is decided (ST-SUP-RP11-LC06). Its audit rows name it as
	// their resource type, and Name when it is empty.
	ResourceType string
	// ReadPermission and ListPermission are the permissions that the entity declares for read and for list.
	ReadPermission string
	ListPermission string
	// LockAtMost is the declared bound of a locked list, or zero when the entity declares none (ST-DECL-EN02).
	LockAtMost int
	// Fields are the sensitive fields of the entity, which its audit rows redact (ST-SUP-RP05-LC03).
	Fields audit.Fields
	// PII is true when the entity holds a pii field: each read of one of its rows is audited (ST-SUP-RP11-LC08).
	PII bool

	Select, SelectForUpdate, SelectForDelete, Insert, Update, Delete string

	// Scan reads one row of the statements' columns.
	Scan func(row Scanner) (T, Meta, error)
	// Values gives the column values of an insert or an update, in the order of the statement's parameters.
	Values func(value T) []any
	// State gives the entity state of a value for its audit row.
	State func(value T) map[string]any
	// KeyChanged reports whether an update from before to after changes a column of the primary key, of a unique
	// index or of a key that a foreign key references (ST-SUP-RP10-LC24).
	KeyChanged func(before, after T) bool
	// Clone gives a deep copy of a value, which a loaded value's Value returns (ST-SUP-RP10-LC15). The generator gives
	// one for every entity: a copy of the struct for one with no slice, map or pointer, and a deep copy otherwise.
	Clone func(value T) T
	// ValidID is the generated check of a key: it reports whether id has the form of the entity's key type, such as a
	// uuid, so a malformed key is not found after the same decision and never fails a statement.
	ValidID func(id string) bool
}

// Entity is the repository declaration of one entity, which NewEntity checks once.
type Entity[T any] struct {
	spec EntitySpec[T]
}

// Declared is an entity that a command or a worker declares it writes: an *Entity of any type.
type Declared interface {
	entityName() string
}

func (e *Entity[T]) entityName() string { return e.spec.Name }

// Name is the entity's name.
func (e *Entity[T]) Name() string { return e.spec.Name }

// NewEntity checks the declaration of one entity and makes its repository declaration. It refuses a missing name,
// statement or function, a statement that does not take the tenant as $1, a locked read without its lock clause,
// and a negative LockAtMost.
func NewEntity[T any](spec EntitySpec[T]) (*Entity[T], error) {
	if spec.Name == "" {
		return nil, errors.New("command: an entity needs a name")
	}
	statements := map[string]string{"Select": spec.Select, "SelectForUpdate": spec.SelectForUpdate,
		"SelectForDelete": spec.SelectForDelete, "Insert": spec.Insert, "Update": spec.Update, "Delete": spec.Delete}
	for name, text := range statements {
		if !strings.Contains(text, "$1") {
			return nil, fmt.Errorf("command: the statement %s of %s does not take the tenant as $1", name, spec.Name)
		}
	}
	if !strings.Contains(strings.ToUpper(spec.SelectForUpdate), "FOR NO KEY UPDATE") {
		return nil, fmt.Errorf("command: the statement SelectForUpdate of %s does not lock FOR NO KEY UPDATE", spec.Name)
	}
	upper := strings.ToUpper(spec.SelectForDelete)
	if !strings.Contains(upper, "FOR UPDATE") || strings.Contains(upper, "NO KEY") {
		return nil, fmt.Errorf("command: the statement SelectForDelete of %s does not lock FOR UPDATE", spec.Name)
	}
	if spec.Scan == nil || spec.Values == nil || spec.State == nil || spec.KeyChanged == nil || spec.Clone == nil ||
		spec.ValidID == nil {
		return nil, fmt.Errorf("command: the entity %s needs Scan, Values, State, KeyChanged, Clone and ValidID", spec.Name)
	}
	if spec.LockAtMost < 0 {
		return nil, fmt.Errorf("command: the entity %s declares a negative Lock at most", spec.Name)
	}
	if spec.ResourceType != "" && spec.ReadPermission == "" {
		return nil, fmt.Errorf("command: the entity %s has a resource type and no read permission", spec.Name)
	}
	return &Entity[T]{spec: spec}, nil
}

// resourceType is the audit resource type of the entity.
func (e *Entity[T]) auditType() string {
	if e.spec.ResourceType != "" {
		return e.spec.ResourceType
	}
	return e.spec.Name
}

// ListSpec is one generated list of an entity. Read reads the matching rows with no lock: $1 is the tenant, $2 the
// most rows to return, and the list's filter values follow from $3. Lock locks matching rows: $1 is the tenant, $2
// the IDs to lock as a text array, which the statement casts to the type of the key, such as $2::uuid[], and the same filter values follow from $3; it applies the filter again, orders by
// the primary key and locks FOR NO KEY UPDATE (ST-SUP-RP10-LC27). Both return the columns of the entity's Scan.
type ListSpec struct {
	Name string
	Read string
	Lock string
}

// List is one checked list of an entity.
type List[T any] struct {
	entity *Entity[T]
	spec   ListSpec
}

// NewList checks one list of e. It refuses a list whose statements do not take the tenant as $1 and whose Lock does
// not order by the key and lock FOR NO KEY UPDATE. A list with a Lock needs an entity that declares Lock at most.
func NewList[T any](e *Entity[T], spec ListSpec) (*List[T], error) {
	if e == nil || spec.Name == "" || !strings.Contains(spec.Read, "$1") {
		return nil, errors.New("command: a list needs its entity, a name and a Read statement that takes the tenant as $1")
	}
	if spec.Lock != "" {
		upper := strings.ToUpper(spec.Lock)
		if !strings.Contains(spec.Lock, "$1") || !strings.Contains(upper, "ORDER BY") ||
			!strings.Contains(upper, "FOR NO KEY UPDATE") {
			return nil, fmt.Errorf("command: the Lock of the list %s does not take the tenant as $1, order by the key and lock FOR NO KEY UPDATE", spec.Name)
		}
		if e.spec.LockAtMost == 0 {
			return nil, fmt.Errorf("command: the list %s locks rows of %s, which declares no Lock at most", spec.Name, e.spec.Name)
		}
	}
	return &List[T]{entity: e, spec: spec}, nil
}

// lockMode is the lock that a loaded value's read took.
type lockMode uint8

const (
	lockNone lockMode = iota
	lockUpdate
	lockDelete
)

// Loaded is the loaded type of the entity T, such as LoadedReservation = command.Loaded[Reservation]: a pointer to a
// value that only a handle makes (ST-SUP-RP10-LC15). It has no exported field and no constructor, and no other type
// converts to it. The query handle gives the same type, from the internal package loaded, so a generated probe
// method takes the loaded value of either handle.
type Loaded[T any] = *loaded.Value[T]

// Ref is a loaded value of any entity, which an effect or a decision names (ST-SUP-RP10-LC08).
type Ref = loaded.Any

// rowRef is what a Ref gives the runtime: the row's resource, its token and the handle state that made it. The
// state of a value that another handle, such as a query handle, made is nil here.
type rowRef struct {
	st           *state
	entity       string
	resourceType string
	id           string
	token        string
}

func refOf(r Ref) rowRef {
	ref, ok := loaded.Of(r)
	if !ok {
		return rowRef{}
	}
	st, _ := ref.Owner.(*state)
	return rowRef{st: st, entity: ref.EntityName, resourceType: ref.ResourceType, id: ref.ID, token: ref.Token}
}

// newLoaded makes the loaded value of one row that the handle state s read or wrote.
func newLoaded[T any](value T, meta Meta, e *Entity[T], s *state, lock lockMode) Loaded[T] {
	return loaded.New(value, e.spec.Clone, loaded.Ref{Owner: s, Entity: e, EntityName: e.spec.Name, ResourceType: e.spec.ResourceType,
		ID: meta.ID, Version: meta.Version, Token: meta.Token, Lock: uint8(lock)})
}

// view is a loaded value as this package reads it.
type view[T any] struct {
	value  T
	meta   Meta
	entity any
	st     *state
	lock   lockMode
	ok     bool
}

func viewOf[T any](l Loaded[T]) view[T] {
	value, ref, ok := loaded.Fields(l)
	st, _ := ref.Owner.(*state)
	return view[T]{value: value, meta: Meta{ID: ref.ID, Version: ref.Version, Token: ref.Token}, entity: ref.Entity,
		st: st, lock: lockMode(ref.Lock), ok: ok}
}

// resourceKey is the outbox resource key of the row's messages: its resource type, or its entity name, and its ID.
func (r rowRef) resourceKey() string {
	kind := r.resourceType
	if kind == "" {
		kind = r.entity
	}
	return kind + ":" + r.id
}

// mode is the kind of run that a handle serves.
type mode uint8

const (
	modeCommand mode = iota + 1
	modeWorker
	modeConsumer
)

// rowKey names one row of one entity in the versions that a handle wrote.
type rowKey struct {
	entity string
	id     string
}

// state is the one state of an attempt that the handle and its parts share. Every method holds its lock for the whole
// call, so the end of the fill waits for a call that runs and every later call does nothing (ST-SUP-RP10-LC10).
type state struct {
	mu       sync.Mutex
	closed   bool
	conflict error
	broken   error

	rt        *Runtime
	tx        *sql.Tx
	tenant    tenant.Tenant
	principal authz.Principal
	mode      mode
	// action is the name of the command or the worker: the action of its audit rows.
	action string
	// az decides as the principal; in a consumer, staging decides as the staging principal with stagingPermission.
	az                *authz.Handle
	staging           *authz.Handle
	stagingPermission string
	// onBehalfOf is the staging principal of a consumer, which its audit rows name (ST-SUP-RP10-LC16).
	onBehalfOf string
	// permission is a worker's declared permission (ST-SUP-RP10-LC17).
	permission string
	// commandPermission is the command's declared permission, which a command's locked read and write decide too
	// (ST-SUP-RP10-LC05); empty for a command without one and for a worker.
	commandPermission string
	writes            map[Declared]bool
	destinations      map[string]Destination
	// inbound gives the inbound key of an effect for the entity ID (ST-SUP-RP10-LC08, ST-SUP-RP10-LC21).
	inbound func(id string) string
	staged  []outbox.Message
	stagedK map[string]bool
	events  []audit.Event
	// written holds the version of each row that a write of this attempt left; a deleted row holds -1.
	written     map[rowKey]int64
	deadline    time.Time
	lockTimeout time.Duration
	request     Request
	traceID     string
	spanID      string
}

// enter starts one method call: it takes the state's lock and fails when the handle is closed, met a conflict or met
// an internal error. The caller calls leave when enter returns nil.
func (s *state) enter() error {
	s.mu.Lock()
	switch {
	case s.closed:
		s.mu.Unlock()
		return ErrClosed
	case s.conflict != nil:
		err := s.conflict
		s.mu.Unlock()
		return err
	case s.broken != nil:
		err := s.broken
		s.mu.Unlock()
		return err
	}
	return nil
}

func (s *state) leave() { s.mu.Unlock() }

// close makes the handle invalid. It waits for a call that runs.
func (s *state) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// fail records the error of a statement: a conflict (ST-SUP-RP10-LC25) or an internal error, which the state keeps,
// so every later method and Run see it. It holds no database text.
func (s *state) fail(ctx context.Context, step string, err error) error {
	code := sqlState(err)
	if isConflict(ctx, err) {
		if s.conflict == nil {
			s.conflict = &conflictError{text: fmt.Sprintf("%s ended with SQLSTATE %q or the deadline", step, code)}
		}
		return s.conflict
	}
	return s.breakWith(internalf("the statement %s failed with SQLSTATE %q", step, code))
}

// breakWith records an internal error that a rerun cannot change.
func (s *state) breakWith(err error) error {
	if s.broken == nil {
		s.broken = err
	}
	return s.broken
}

// setLockTimeout sets lock_timeout in the transaction to the smaller of the declared lock timeout and the time left
// before the deadline less 50 ms, and to at least 1 ms (ST-SUP-RP10-LC28).
func (s *state) setLockTimeout(ctx context.Context) error {
	d := s.lockTimeout
	if !s.deadline.IsZero() {
		if left := time.Until(s.deadline) - 50*time.Millisecond; left < d {
			d = left
		}
	}
	if d < time.Millisecond {
		d = time.Millisecond
	}
	_, err := s.exec(ctx, setLockTimeout, fmt.Sprintf("%dms", d.Milliseconds()))
	return err
}

const setLockTimeout = `SELECT pg_catalog.set_config('lock_timeout', $1, true)`

// exec, queryRow and query run one statement in the bound transaction and tell the runtime's statement recorder,
// which only the tests of the package set.
func (s *state) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	s.rt.record(query, args)
	return s.tx.ExecContext(ctx, query, args...)
}

func (s *state) queryRow(ctx context.Context, query string, args ...any) *sql.Row {
	s.rt.record(query, args)
	return s.tx.QueryRowContext(ctx, query, args...)
}

func (s *state) query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	s.rt.record(query, args)
	return s.tx.QueryContext(ctx, query, args...)
}

// lockStatement runs a locking or writing statement: it sets lock_timeout first (ST-SUP-RP10-LC28).
func (s *state) lockedRow(ctx context.Context, step, query string, args ...any) (*sql.Row, error) {
	if err := s.setLockTimeout(ctx); err != nil {
		return nil, s.fail(ctx, step, err)
	}
	return s.queryRow(ctx, query, args...), nil
}

// Handle is the command handle that a fill gets: bound to one transaction, one tenant and one principal, which it
// reports and does not change (ST-SUP-RP10-LC01). It reaches storage only through the repositories of the entities
// that its command or worker declares it writes (ST-SUP-RP10-LC02), and it has no transaction control
// (ST-SUP-RP10-LC09). P is the typed probe that the generated code built for the command or the worker
// (ST-SUP-RP10-LC14). Once the fill returns or panics, every method fails and does nothing (ST-SUP-RP10-LC10).
type Handle[P any] struct {
	st    *state
	probe P
}

// Tenant returns the tenant of the handle.
func (h *Handle[P]) Tenant() tenant.Tenant { return h.st.tenant }

// Principal returns the principal of the handle: the command's principal, or a worker's service account.
func (h *Handle[P]) Principal() authz.Principal { return h.st.principal }

// Probe returns the fill's own probe, bound to the handle's span context, tenant, principal and command or worker
// name. It emits nothing once the fill returns or panics (ST-SUP-RP10-LC14).
func (h *Handle[P]) Probe() P { return h.probe }

// Authz returns the handle's authorization part (ST-SUP-RP10-LC04).
func (h *Handle[P]) Authz() *Authz { return &Authz{st: h.st} }

// Effects returns the handle's effect stager (ST-SUP-RP10-LC07).
func (h *Handle[P]) Effects() *Effects { return &Effects{st: h.st} }

// Repository is the tenant-scoped repository of one declared entity in one handle's transaction.
type Repository[T any] struct {
	st     *state
	entity *Entity[T]
}

// Repo returns the repository of e in h. It refuses, with an error that names the entity and with no statement, an
// entity that the handle's command or worker does not declare it writes (ST-SUP-RP10-LC02, ST-SUP-RP10-LC05).
func Repo[T, P any](h *Handle[P], e *Entity[T]) (*Repository[T], error) {
	if h == nil || h.st == nil {
		return nil, ErrClosed
	}
	if err := h.st.enter(); err != nil {
		return nil, err
	}
	defer h.st.leave()
	if e == nil || !h.st.writes[e] {
		name := "<nil>"
		if e != nil {
			name = e.spec.Name
		}
		return nil, fmt.Errorf("command: %s does not declare that it writes %s", h.st.action, name)
	}
	return &Repository[T]{st: h.st, entity: e}, nil
}

// Get reads the row of id with no lock. It returns ErrNotFound with no data for a row that the tenant does not hold
// or that the principal may not read (ST-SUP-RP10-LC03, ST-SUP-RP10-LC13), and audits the read of an entity with a
// pii field (ST-SUP-RP11-LC08).
func (r *Repository[T]) Get(ctx context.Context, id string) (Loaded[T], error) {
	if err := r.st.enter(); err != nil {
		return nil, err
	}
	defer r.st.leave()
	return r.read(ctx, id, lockNone)
}

// GetForUpdate reads the row of id, decides the read on it, and only then locks it with FOR NO KEY UPDATE until the
// transaction ends, so a concurrent insert of a child row does not wait (ST-SUP-RP10-LC23). A denied row is not
// locked and is not found. When the row's authorization token changed between the read and the lock, it decides
// again and releases the lock of a row that is now denied.
func (r *Repository[T]) GetForUpdate(ctx context.Context, id string) (Loaded[T], error) {
	if err := r.st.enter(); err != nil {
		return nil, err
	}
	defer r.st.leave()
	return r.read(ctx, id, lockUpdate)
}

// GetForDelete is GetForUpdate with FOR UPDATE, for a fill that deletes the row or changes a column of one of its
// keys (ST-SUP-RP10-LC23, ST-SUP-RP10-LC24).
func (r *Repository[T]) GetForDelete(ctx context.Context, id string) (Loaded[T], error) {
	if err := r.st.enter(); err != nil {
		return nil, err
	}
	defer r.st.leave()
	return r.read(ctx, id, lockDelete)
}

func (r *Repository[T]) read(ctx context.Context, id string, lock lockMode) (Loaded[T], error) {
	s, e := r.st, r.entity
	var value T
	var meta Meta
	err := sql.ErrNoRows
	if e.spec.ValidID(id) {
		value, meta, err = e.spec.Scan(s.queryRow(ctx, e.spec.Select, s.tenant.ID(), id))
	}
	if errors.Is(err, sql.ErrNoRows) {
		// A row that does not exist is decided as one that exists, so a deny and a missing row give the same
		// answer, and a failed decision the same error.
		if err := s.decideRead(ctx, e.spec.ResourceType, e.spec.ReadPermission, e.spec.Name, Meta{ID: id}); err != nil &&
			!errors.Is(err, ErrNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s", ErrNotFound, e.spec.Name)
	}
	if err != nil {
		return nil, s.fail(ctx, "read "+e.spec.Name, err)
	}
	if err := s.decideRead(ctx, e.spec.ResourceType, e.spec.ReadPermission, e.spec.Name, meta); err != nil {
		return nil, err
	}
	if lock != lockNone {
		if err := s.decideCommandOn(ctx, e.spec.ResourceType, e.spec.Name, meta.ID, meta.Token); err != nil {
			return nil, err
		}
		statement := e.spec.SelectForUpdate
		if lock == lockDelete {
			statement = e.spec.SelectForDelete
		}
		value, meta, err = lockOne(ctx, s, e, statement, id, meta)
		if err != nil {
			return nil, err
		}
	}
	if err := s.auditRead(ctx, e.spec.PII, e.auditType(), meta.ID); err != nil {
		return nil, err
	}
	return newLoaded(value, meta, e, s, lock), nil
}

// lockOne locks the row of id inside a savepoint. When its token moved since the read of first, it decides again and
// rolls the savepoint back, which releases the lock, for a row that is now denied (ST-SUP-RP10-LC23).
func lockOne[T any](ctx context.Context, s *state, e *Entity[T], statement, id string, first Meta) (T, Meta, error) {
	var zero T
	step := "lock " + e.spec.Name
	if err := s.setLockTimeout(ctx); err != nil {
		return zero, Meta{}, s.fail(ctx, step, err)
	}
	if _, err := s.exec(ctx, "SAVEPOINT stego_command_lock"); err != nil {
		return zero, Meta{}, s.fail(ctx, step, err)
	}
	value, meta, err := e.spec.Scan(s.queryRow(ctx, statement, s.tenant.ID(), id))
	if errors.Is(err, sql.ErrNoRows) {
		if err := s.release(ctx, step); err != nil {
			return zero, Meta{}, err
		}
		return zero, Meta{}, fmt.Errorf("%w: %s", ErrNotFound, e.spec.Name)
	}
	if err != nil {
		return zero, Meta{}, s.fail(ctx, step, err)
	}
	if meta.Token != first.Token {
		err := s.decideRead(ctx, e.spec.ResourceType, e.spec.ReadPermission, e.spec.Name, meta)
		if err == nil {
			err = s.decideCommandOn(ctx, e.spec.ResourceType, e.spec.Name, meta.ID, meta.Token)
		}
		if err != nil {
			if _, rbErr := s.exec(ctx, "ROLLBACK TO SAVEPOINT stego_command_lock"); rbErr != nil {
				return zero, Meta{}, s.fail(ctx, step, rbErr)
			}
			if err := s.release(ctx, step); err != nil {
				return zero, Meta{}, err
			}
			return zero, Meta{}, err
		}
	}
	if err := s.release(ctx, step); err != nil {
		return zero, Meta{}, err
	}
	return value, meta, nil
}

func (s *state) release(ctx context.Context, step string) error {
	if _, err := s.exec(ctx, "RELEASE SAVEPOINT stego_command_lock"); err != nil {
		return s.fail(ctx, step, err)
	}
	return nil
}

// List reads the rows of the list l with the filter values args, at most limit of them, and returns the allowed rows
// in the list's order (ST-SUP-RP10-LC13, ST-SUP-RP10-LC17). It locks nothing.
func (r *Repository[T]) List(ctx context.Context, l *List[T], limit int, args ...any) ([]Loaded[T], error) {
	if err := r.st.enter(); err != nil {
		return nil, err
	}
	defer r.st.leave()
	if l == nil || l.entity != r.entity || limit < 1 {
		return nil, fmt.Errorf("command: a list of %s needs its own list and a limit of at least 1", r.entity.spec.Name)
	}
	rows, err := r.readList(ctx, l, limit, args)
	if err != nil {
		return nil, err
	}
	allowed, err := r.allowedRows(ctx, rows, false)
	if err != nil {
		return nil, err
	}
	return r.loadAll(ctx, allowed, lockNone)
}

// ListForUpdate reads the rows of l with no lock and a limit of Lock at most plus one, fails with an internal error
// and locks nothing when more rows than Lock at most match, keeps the rows that the decision port allows, and locks
// only those with l's Lock statement, in primary key order. It decides again each locked row whose token moved and
// leaves out, and unlocks, a row that is now denied (ST-SUP-RP10-LC27). The locks stay until the transaction ends.
func (r *Repository[T]) ListForUpdate(ctx context.Context, l *List[T], args ...any) ([]Loaded[T], error) {
	if err := r.st.enter(); err != nil {
		return nil, err
	}
	defer r.st.leave()
	s, e := r.st, r.entity
	if l == nil || l.entity != e || l.spec.Lock == "" || e.spec.LockAtMost == 0 {
		return nil, fmt.Errorf("command: %s has no locked list of that name: the entity declares no Lock at most", e.spec.Name)
	}
	rows, err := r.readList(ctx, l, e.spec.LockAtMost+1, args)
	if err != nil {
		return nil, err
	}
	if len(rows) > e.spec.LockAtMost {
		return nil, s.breakWith(internalf("the locked list %s of %s matches more than its Lock at most %d rows",
			l.spec.Name, e.spec.Name, e.spec.LockAtMost))
	}
	allowed, err := r.allowedRows(ctx, rows, true)
	if err != nil {
		return nil, err
	}
	first := map[string]string{}
	for _, row := range allowed {
		first[row.meta.ID] = row.meta.Token
	}
	step := "lock the list " + l.spec.Name
	for {
		if len(allowed) == 0 {
			return nil, nil
		}
		if err := s.setLockTimeout(ctx); err != nil {
			return nil, s.fail(ctx, step, err)
		}
		ids := make([]string, len(allowed))
		for i, row := range allowed {
			ids[i] = row.meta.ID
		}
		if _, err := s.exec(ctx, "SAVEPOINT stego_command_lock"); err != nil {
			return nil, s.fail(ctx, step, err)
		}
		locked, err := r.scanAll(ctx, step, l.spec.Lock, append([]any{s.tenant.ID(), ids}, args...))
		if err != nil {
			return nil, err
		}
		var moved []scanned[T]
		for _, row := range locked {
			if row.meta.Token != first[row.meta.ID] {
				moved = append(moved, row)
			}
		}
		denied := map[string]bool{}
		if len(moved) > 0 {
			ok, err := r.decideRows(ctx, moved, true)
			if err != nil {
				return nil, err
			}
			for i, row := range moved {
				if !ok[i] {
					denied[row.meta.ID] = true
				}
			}
		}
		if len(denied) == 0 {
			if err := s.release(ctx, step); err != nil {
				return nil, err
			}
			return r.loadAll(ctx, locked, lockUpdate)
		}
		// A row that is now denied leaves the list, and its lock goes with the savepoint; the other rows are locked
		// again, at their own tokens.
		if _, err := s.exec(ctx, "ROLLBACK TO SAVEPOINT stego_command_lock"); err != nil {
			return nil, s.fail(ctx, step, err)
		}
		if err := s.release(ctx, step); err != nil {
			return nil, err
		}
		allowed = allowed[:0]
		for _, row := range locked {
			if !denied[row.meta.ID] {
				first[row.meta.ID] = row.meta.Token
				allowed = append(allowed, row)
			}
		}
	}
}

// scanned is one row that a list read.
type scanned[T any] struct {
	value T
	meta  Meta
}

func (r *Repository[T]) readList(ctx context.Context, l *List[T], limit int, args []any) ([]scanned[T], error) {
	return r.scanAll(ctx, "read the list "+l.spec.Name, l.spec.Read, append([]any{r.st.tenant.ID(), limit}, args...))
}

func (r *Repository[T]) scanAll(ctx context.Context, step, statement string, args []any) ([]scanned[T], error) {
	s := r.st
	rows, err := s.query(ctx, statement, args...)
	if err != nil {
		return nil, s.fail(ctx, step, err)
	}
	defer rows.Close()
	var out []scanned[T]
	for rows.Next() {
		value, meta, err := r.entity.spec.Scan(rows)
		if err != nil {
			return nil, s.fail(ctx, step, err)
		}
		out = append(out, scanned[T]{value: value, meta: meta})
	}
	if err := rows.Err(); err != nil {
		return nil, s.fail(ctx, step, err)
	}
	return out, nil
}

// allowedRows keeps the rows that the decision port allows, in their order.
func (r *Repository[T]) allowedRows(ctx context.Context, rows []scanned[T], locked bool) ([]scanned[T], error) {
	ok, err := r.decideRows(ctx, rows, locked)
	if err != nil {
		return nil, err
	}
	var allowed []scanned[T]
	for i, row := range rows {
		if ok[i] {
			allowed = append(allowed, row)
		}
	}
	return allowed, nil
}

// decideRows decides the rows of a list. A locked list of a command also decides the command's permission on each
// row that the list permission allows (ST-SUP-RP10-LC05).
func (r *Repository[T]) decideRows(ctx context.Context, rows []scanned[T], locked bool) ([]bool, error) {
	metas := make([]Meta, len(rows))
	for i, row := range rows {
		metas[i] = row.meta
	}
	permission := r.entity.spec.ListPermission
	if permission == "" {
		permission = r.entity.spec.ReadPermission
	}
	s := r.st
	allowed, err := s.decideList(ctx, r.entity.spec.ResourceType, permission, metas)
	if err != nil || !locked || s.mode != modeCommand || s.commandPermission == "" || r.entity.spec.ResourceType == "" {
		return allowed, err
	}
	return s.batch(ctx, s.az, s.commandPermission, r.entity.spec.ResourceType, metas, allowed, allowed)
}

// loadAll audits each read of an entity with a pii field and makes the loaded values.
func (r *Repository[T]) loadAll(ctx context.Context, rows []scanned[T], lock lockMode) ([]Loaded[T], error) {
	out := make([]Loaded[T], 0, len(rows))
	for _, row := range rows {
		if err := r.st.auditRead(ctx, r.entity.spec.PII, r.entity.auditType(), row.meta.ID); err != nil {
			return nil, err
		}
		out = append(out, newLoaded(row.value, row.meta, r.entity, r.st, lock))
	}
	return out, nil
}

// Create inserts value and returns it as stored, with the version the insert gave it. A command's create under a
// parent decides the command's permission on the parent, and one with no parent is covered by the decision before
// the fill (ST-SUP-RP10-LC05); a worker's create decides the worker's permission on parent, the loaded
// parent resource that the new row goes under, before the write (ST-SUP-RP10-LC17). The audit row of the write is
// buffered for Run (ST-SUP-RP10-LC06).
func (r *Repository[T]) Create(ctx context.Context, value T, parent Ref) (Loaded[T], error) {
	if err := r.st.enter(); err != nil {
		return nil, err
	}
	defer r.st.leave()
	s, e := r.st, r.entity
	if s.mode != modeCommand && e.spec.ResourceType != "" {
		if parent == nil {
			return nil, fmt.Errorf("command: a worker's create of %s needs the parent resource to decide on", e.spec.Name)
		}
		p := refOf(parent)
		if p.st != s {
			return nil, s.breakWith(internalf("the parent of a create of %s is a loaded value of another transaction", e.spec.Name))
		}
		if err := s.decideWrite(ctx, p.resourceType, p.entity, p.id, p.token); err != nil {
			return nil, err
		}
	}
	if s.mode == modeCommand && parent != nil {
		// A command's create under a parent decides the command's permission on the parent (ST-SUP-RP10-LC05).
		p := refOf(parent)
		if p.st != s {
			return nil, s.breakWith(internalf("the parent of a create of %s is a loaded value of another transaction", e.spec.Name))
		}
		if err := s.decideWrite(ctx, p.resourceType, p.entity, p.id, p.token); err != nil {
			return nil, err
		}
	}
	row, err := s.lockedRow(ctx, "create "+e.spec.Name, e.spec.Insert, append([]any{s.tenant.ID()}, e.spec.Values(value)...)...)
	if err != nil {
		return nil, err
	}
	stored, meta, err := e.spec.Scan(row)
	if err != nil {
		return nil, s.fail(ctx, "create "+e.spec.Name, err)
	}
	if err := s.buffer(e.auditType(), e.spec.Fields, meta.ID, nil, e.spec.State(stored)); err != nil {
		return nil, err
	}
	s.written[rowKey{e.spec.Name, meta.ID}] = meta.Version
	// The row is new in this transaction, which holds it as strongly as a lock FOR UPDATE.
	return newLoaded(stored, meta, e, s, lockDelete), nil
}

// Update writes value over the row of from, a loaded value of this handle, only while the row still has from's
// version, and returns the row with its new version (ST-SUP-RP10-LC24). A moved version writes nothing, writes no
// audit row and returns ErrConflict. A value of another transaction, a value of this transaction whose row a later
// write changed, and a change of a key column from a value that GetForDelete did not lock are internal errors.
func (r *Repository[T]) Update(ctx context.Context, from Loaded[T], value T) (Loaded[T], error) {
	if err := r.st.enter(); err != nil {
		return nil, err
	}
	defer r.st.leave()
	s, e := r.st, r.entity
	from2, err := r.check(from, "update")
	if err != nil {
		return nil, err
	}
	if e.spec.KeyChanged(from2.value, value) && from2.lock != lockDelete {
		return nil, s.breakWith(internalf("an update of a key column of %s needs a value that GetForDelete locked", e.spec.Name))
	}
	if err := s.decideWrite(ctx, e.spec.ResourceType, e.spec.Name, from2.meta.ID, from2.meta.Token); err != nil {
		return nil, err
	}
	step := "update " + e.spec.Name
	row, err := s.lockedRow(ctx, step, e.spec.Update,
		append([]any{s.tenant.ID(), from2.meta.ID, from2.meta.Version}, e.spec.Values(value)...)...)
	if err != nil {
		return nil, err
	}
	stored, meta, err := e.spec.Scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		s.conflict = &conflictError{text: "the version of a row of " + e.spec.Name + " moved"}
		return nil, s.conflict
	}
	if err != nil {
		return nil, s.fail(ctx, step, err)
	}
	if err := s.buffer(e.auditType(), e.spec.Fields, meta.ID, e.spec.State(from2.value), e.spec.State(stored)); err != nil {
		return nil, err
	}
	s.written[rowKey{e.spec.Name, meta.ID}] = meta.Version
	return newLoaded(stored, meta, e, s, max(from2.lock, lockUpdate)), nil
}

// Delete removes the row of from, a loaded value of this handle, only while the row still has from's version, as
// Update states (ST-SUP-RP10-LC24).
func (r *Repository[T]) Delete(ctx context.Context, from Loaded[T]) error {
	if err := r.st.enter(); err != nil {
		return err
	}
	defer r.st.leave()
	s, e := r.st, r.entity
	from2, err := r.check(from, "delete")
	if err != nil {
		return err
	}
	// A delete takes the strongest lock: a value that GetForUpdate locked would make it stronger later, so it is
	// refused as a key change is (ST-SUP-RP10-LC24).
	if from2.lock == lockUpdate {
		return s.breakWith(internalf("a delete of %s needs a value that GetForDelete locked, not GetForUpdate", e.spec.Name))
	}
	if err := s.decideWrite(ctx, e.spec.ResourceType, e.spec.Name, from2.meta.ID, from2.meta.Token); err != nil {
		return err
	}
	step := "delete " + e.spec.Name
	if err := s.setLockTimeout(ctx); err != nil {
		return s.fail(ctx, step, err)
	}
	result, err := s.exec(ctx, e.spec.Delete, s.tenant.ID(), from2.meta.ID, from2.meta.Version)
	if err != nil {
		return s.fail(ctx, step, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return s.fail(ctx, step, err)
	}
	if n == 0 {
		s.conflict = &conflictError{text: "the version of a row of " + e.spec.Name + " moved"}
		return s.conflict
	}
	if err := s.buffer(e.auditType(), e.spec.Fields, from2.meta.ID, e.spec.State(from2.value), nil); err != nil {
		return err
	}
	s.written[rowKey{e.spec.Name, from2.meta.ID}] = -1
	return nil
}

// check refuses, as an internal error, a loaded value that this handle did not return and one whose row a later write
// of this transaction changed (ST-SUP-RP10-LC24).
func (r *Repository[T]) check(l Loaded[T], verb string) (view[T], error) {
	s, e := r.st, r.entity
	from := viewOf(l)
	if !from.ok || from.st != s || from.entity != any(e) {
		return from, s.breakWith(internalf("the %s of %s takes a loaded value of another transaction", verb, e.spec.Name))
	}
	if version, ok := s.written[rowKey{e.spec.Name, from.meta.ID}]; ok && version != from.meta.Version {
		return from, s.breakWith(internalf("the %s of %s takes a loaded value whose row a later write changed", verb, e.spec.Name))
	}
	return from, nil
}
