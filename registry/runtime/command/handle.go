package command

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/internal/loaded"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
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
	// ErrInvalid matches every refusal of a value that the request gave for a field, such as a change of a field
	// that holds the subject of a declared relationship, which only a create sets, or a subject that no tuple can
	// hold (ST-SUP-RP10-LC05, ST-SUP-Q118). Once a handle meets one, every later method returns it, Run commits
	// nothing and runs the fill no more, and OutcomeOf answers it with 400 and the code invalid, naming the field.
	ErrInvalid = errors.New("command: invalid value")
	// ErrNoParent is the error of a worker's create of a row whose value sets a reference to an entity with a
	// resource type while the call gives no loaded parent to decide on (ST-SUP-RP10-LC17): the run log names its
	// cause create_without_parent and the entity. A create that sets no such reference is decided on the tenant
	// resource (ST-SUP-Q164 (b)).
	ErrNoParent = errors.New("command: a worker's create needs the parent resource to decide on")
	// ErrUnavailable matches every internal error of a database that cannot serve now, such as a refused or lost
	// connection or a server that shuts down (tenant.Unavailable). OutcomeOf answers it with 503 and the code
	// unavailable, with Retry-After, and Unavailable over gRPC, so the client tries again later (walkthrough 10, B7).
	ErrUnavailable = errors.New("command: the database cannot serve now")
)

// invalidError is a refusal of a field's value: it matches ErrInvalid and names the declared field and a fixed
// reason, never the value.
type invalidError struct{ field, reason string }

func (e *invalidError) Error() string {
	return "command: invalid value of " + e.field + ": " + e.reason
}
func (e *invalidError) Is(target error) bool { return target == ErrInvalid }

// The fixed reasons of an invalidError.
const (
	reasonCreateOnly = "the field holds the subject of a relationship, which only a create sets"
	reasonSubject    = "the field names no subject that a relationship can hold"
)

// internalError is an internal error: it matches ErrInternal and holds no field value and no database text.
// statement is true for a statement that the database failed, such as for an undefined column, a statement timeout
// or a lost connection, which the fill did not cause: a consumer does not count it for the poison bound
// (ST-SUP-RC35-OC06).
//
// data is true for a statement that fails on the values of the row, such as for a foreign key or a check (SQLSTATE
// classes 22 and 23): a consumer counts it for the poison bound with the cause class data.
//
// unavailable is true for a database that cannot serve now (tenant.Unavailable): it matches ErrUnavailable too.
type internalError struct {
	text        string
	statement   bool
	data        bool
	unavailable bool
}

func (e *internalError) Error() string { return "command: internal error: " + e.text }
func (e *internalError) Is(target error) bool {
	return target == ErrInternal || (e.unavailable && target == ErrUnavailable)
}

// unavailablef is the internal error of a step that failed because the database cannot serve now.
func unavailablef(format string, args ...any) error {
	return &internalError{text: fmt.Sprintf(format, args...), statement: true, unavailable: true}
}

func internalf(format string, args ...any) error {
	return &internalError{text: fmt.Sprintf(format, args...)}
}

// noParentError is ErrNoParent for the entity that it names, a declared name.
type noParentError struct{ entity string }

func (e *noParentError) Error() string        { return ErrNoParent.Error() + ": " + e.entity }
func (e *noParentError) Is(target error) bool { return target == ErrNoParent }

// NoParentEntity gives the entity of an ErrNoParent in err, and false when err is not one.
func NoParentEntity(err error) (string, bool) {
	var noParent *noParentError
	if errors.As(err, &noParent) {
		return noParent.entity, true
	}
	return "", false
}

// conflictError is a conflict: it matches ErrConflict and names the step and the SQLSTATE, never a database text.
type conflictError struct {
	text string
	// unique is true for a unique violation, which a consumer counts as an attempt (uniqueConflict).
	unique bool
	// sealSwitch is true for a statement that failed because the contract step of a seal dropped a clear column, after
	// which the entity takes its statements after the contract (ST-SUP-RP10-LC49).
	sealSwitch bool
	// stalePlan is true for a statement that the server refused because a change of its table made the prepared
	// statement of the connection stale (tenant.StalePlan); the pools drop their prepared statements (ST-SUP-RP04-LC11).
	stalePlan bool
	// key is the declared fields of the unique key that a unique violation broke, or empty when the entity does not
	// declare the index (ST-SUP-RP10-LC48). It names no value.
	key string
}

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

// conflictStates are the SQLSTATE values that are a conflict (ST-SUP-RP10-LC25): serialization failure, deadlock,
// lock not available, which a lock timeout raises, and unique violation, which a concurrent write of the same unique
// value raises: a rerun sees the other row.
var conflictStates = map[string]bool{"40001": true, "40P01": true, "55P03": true, "23505": true}

// uniqueConflict reports whether err is the conflict of a unique violation. A rerun of an HTTP command sees the other
// row, but a consumer that writes the same value again gets the same violation on each dispatch, so RunConsumer
// counts it as an attempt and not as a conflict (ST-SUP-RP10-LC25, ST-SUP-RP10-LC26).
func uniqueConflict(err error) bool {
	var c *conflictError
	return errors.As(err, &c) && c.unique
}

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
// (ST-SUP-RP10-LC24) and the text form of the authorization token, empty while the relay has written none.
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
	// Check is the generated check of the declared constraints of the entity's fields: min_length, max_length,
	// pattern, min, max and the values of an enum, of an optional field only when it is set. It gives the declared
	// name of the first field whose value breaks one and a fixed reason, or ok. The reason never holds the value.
	// Create and Update call it before any decision and any statement, and refuse a value that breaks one with
	// ErrInvalid naming the field, which OutcomeOf answers 400 invalid, or InvalidArgument over gRPC (walkthrough 10,
	// B4). Nil checks nothing: the table still refuses the value, as Constraints states.
	Check func(value T) (field, reason string, ok bool)
	// Constraints gives the declared field of each CHECK and foreign key of the entity's table, by the name of the
	// constraint, and of each column, by the name of the column. A statement that a CHECK (SQLSTATE 23514), a NOT NULL
	// (23502) or a foreign key (23503) refuses is ErrInvalid naming that field, so a value that Check does not cover
	// is answered 400 and never 500 (walkthrough 10, B4).
	Constraints map[string]string
	// Unique gives the declared fields of the key of each unique index of the entity's table, by the name of the
	// index, joined by a comma and a space. A create or an update that a unique violation (SQLSTATE 23505) of one of
	// them ends is a conflict that names those fields, which OutcomeOf answers 409 with the field, never a value
	// (ST-SUP-RP10-LC48). Nil names no key.
	Unique map[string]string
	// Parent is the list of the references of a row to the rows of other entities, which the generated code reads
	// from a value, such as Order and value.OrderID, in the order that the declaration gives them; empty for an
	// entity that has none. A create, a move and the work on a child take the loaded value of each reference that
	// is set, and nothing else (ST-SUP-RP10-LC05).
	Parent []Reference[T]
	// Within reads each field of a value that a reference of this entity, or of another entity to this one, is held
	// within (Reference.Within), by its declared name, such as project_id and value.ProjectID; an empty string is a
	// field that is not set (ST-SUP-RP10-LC37).
	Within map[string]func(value T) string
	// Referrers are the composite keys of the references of other entities to this one that are held within a field
	// of this entity (ST-SUP-RP10-LC37).
	Referrers []Referrer
	// Relationships are the declared relationships of the entity (ST-DECL-UC06-R04), which the runtime writes as it
	// writes the relations of the references (ST-SUP-RP10-LC05, ST-SUP-Q118): a create touches the tuple of each one
	// whose subject is set, an update that changes a field subject deletes the old tuple and touches the new one,
	// and a delete deletes each field-sourced tuple. They need a resource type on the entity.
	Relationships []Relationship[T]
	// Relations are the relations that the schema declares on ResourceType, which the generator reads from the
	// service's schema: a delete of a row clears each of them on the row's resource, whatever wrote the tuple
	// (ST-SUP-Q118). The handle is bound with them, but a fill relates only its own relations.
	Relations []string
	// Sealed is true when the entity has a readable secret field, which Values gives as SealAt and Scan reads into
	// OpenAt: a runtime with no key ring refuses the entity (ST-SUP-RP10-LC44).
	Sealed bool
	// ReadOnly marks the read-only declaration of an entity that a worker reads (ST-SUP-RP10-LC53, decision
	// ST-SUP-Q186 (a)): it has only Select, Scan, Clone and ValidID, its value holds no secret field, and only ReadRepo
	// reaches it, with Get and List.
	ReadOnly bool
	// EventData gives the declared event fields of a value by their declared names, and EventVersion their version
	// from 1: the change event of each write of the entity then holds them as data and data_version
	// (ST-SUP-RP10-LC55, decision ST-SUP-Q184 (c)). Nil is an event with no field value.
	EventData    func(value T) map[string]any
	EventVersion int
	// AfterSeal is the entity after the contract step of each of its open seals, with the sealed column alone of each
	// readable secret whose seal step is open, or nil for an entity with no open seal: the runtime takes it when a
	// statement fails because the contract dropped the clear columns (ST-SUP-RP10-LC49).
	AfterSeal *AfterSeal[T]
	// Tenant is true when the schema gives ResourceType the relation tenant of the subject type tenant: a create then
	// touches the tuple <resource type>:<row>#tenant@tenant:<tenant resource ID> (authz.TenantResource), so a
	// permission such as sync = tenant->worker_<name> reaches the rows of that tenant only. Only the runtime writes
	// this relation, and a delete clears it (ST-SUP-RP10-LC05, ST-SUP-Q132). It needs a resource type on the entity.
	Tenant bool
}

// Relationship is one declared relationship of an entity: the relation of the entity's resource type, the subject
// type, and where the subject comes from.
type Relationship[T any] struct {
	// Relation is the relation, such as owner, and SubjectType the type of its subject, such as user.
	Relation, SubjectType string
	// Field reads the subject's ID from a value, such as value.OwnerID; an empty ID writes no tuple. Nil takes the
	// subject from the caller, the principal of the handle, which a create writes and an update never changes; a
	// caller of another subject type than SubjectType is refused, so a tuple that the schema refuses is never
	// enqueued. A field subject is set at create only: an update that changes it is refused with ErrInvalid, and a
	// command that declares no permission may not set it (ST-SUP-Q118).
	Field func(value T) string
	// FieldName is the declared name of the field that Field reads, such as owner_id, which a refusal names; it is
	// required with Field.
	FieldName string
	// Resource is true when SubjectType is a resource type of this service, so the subject's ID is tenant-qualified
	// as the ID of a referenced row is (ST-SUP-RP06-LC22).
	Resource bool
}

// Reference is one reference of a row of an entity to a row of another entity, as the declaration states it
// (ST-DECL-EN02): the referenced entity, the field that holds the referenced ID, the permission that a create and a
// move decide on the referenced row, and the relation that the runtime writes for the reference (ST-SUP-RP10-LC05).
type Reference[T any] struct {
	// Entity is the name of the referenced entity, such as Customer, and ResourceType its resource type, such as
	// customer, or empty for an entity with none.
	Entity, ResourceType string
	// ID reads the ID of the referenced row from a value, such as value.CustomerID. An empty ID is an optional
	// reference that is not set: it is not decided and has no relation.
	ID func(value T) string
	// Permission is the permission that the declaration names for the reference, such as place_order, which a
	// command decides on the referenced row when it creates the row or moves the reference. Empty is the command's
	// own permission; a worker always decides its own permission (ST-SUP-RP10-LC17).
	Permission string
	// Relation is the relation of the entity's resource type that names the referenced row, such as customer, in
	// the service's own schema: the runtime writes the tuple <resource type>:<row>#<Relation>@<referenced type>:<ID>
	// when the row is created or the reference moves, and deletes the old one on a move. Empty writes none. It
	// needs a resource type on the entity.
	Relation string
	// Field is the declared name of the reference, such as sprint_id, which a refusal names. Within is the declared
	// field that the row and the referenced row must have equal, such as project_id, or empty for a reference with no
	// within; both entities read it through their EntitySpec.Within. Key is the name of the composite foreign key
	// that holds the same rule in the database (ST-SUP-RP10-LC37, ST-SUP-RC01-OC46).
	Field, Within, Key string
	// Check is the name of the CHECK that holds the within field set while the reference is set, for a within field
	// that may be NULL (ST-SUP-RC01-OC46), or empty. A write that it refuses (SQLSTATE 23514) is ErrInvalid naming the
	// reference, as a write that Key refuses is.
	Check string
}

// Referrer is the composite foreign key of a reference of another entity to this one that is held within a field
// of this entity (ST-SUP-RP10-LC37): Key is the name of the key, Field the reference as Entity.field, such as
// Ticket.sprint_id, and Within the field of this entity, such as project_id. A write of this entity that the key
// refuses, such as an update that moves the row to another project while a ticket refers to it, is ErrInvalid.
type Referrer struct {
	Key, Field, Within string
}

// reference is one set reference of a value: the referenced entity and ID, and the declared permission and
// relation.
type reference struct {
	entity, resourceType, id, permission, relation string
	// field, within and key are the Field, Within and Key of the declared reference (ST-SUP-RP10-LC37).
	field, within, key string
}

// references gives the set references of value, in the order of the declaration.
func (e *Entity[T]) references(value T) []reference {
	var refs []reference
	for _, r := range e.spec.Parent {
		if id := r.ID(value); id != "" {
			refs = append(refs, reference{entity: r.Entity, resourceType: r.ResourceType, id: id, permission: r.Permission,
				relation: r.Relation, field: r.Field, within: r.Within, key: r.Key})
		}
	}
	return refs
}

// relations are the relations that the runtime writes or clears for the rows of the entity (relationNames), which the
// authorization handle of a command or a worker that writes it is bound with besides the fill's own.
func (e *Entity[T]) relations() []authz.Relation {
	if e.spec.ResourceType == "" {
		return nil
	}
	var out []authz.Relation
	for _, name := range e.relationNames() {
		out = append(out, authz.Relation{ResourceType: e.spec.ResourceType, Name: name})
	}
	return out
}

// runtimeRelations gives the relations that only the runtime writes for the rows of the entity: the relation of each
// declared relationship and of each parent reference (ST-SUP-Q118). A command that declares one of them gets no
// fill relation of it, so a fill cannot move an owner after create or relate a row to a parent with no permission
// on the parent.
func (e *Entity[T]) runtimeRelations() []authz.Relation {
	if e.spec.ResourceType == "" {
		return nil
	}
	var out []authz.Relation
	for _, rel := range e.spec.Relationships {
		out = append(out, authz.Relation{ResourceType: e.spec.ResourceType, Name: rel.Relation})
	}
	for _, ref := range e.spec.Parent {
		out = append(out, authz.Relation{ResourceType: e.spec.ResourceType, Name: ref.Relation})
	}
	if e.spec.Tenant {
		out = append(out, authz.Relation{ResourceType: e.spec.ResourceType, Name: authz.TenantRelation})
	}
	return out
}

// declaredTuple is the tuple of one declared relationship of a row before the row is written: its relation and its
// checked subject. The resource is the row's, whose key the row statement gives.
type declaredTuple struct {
	relation string
	subject  authz.Subject
}

// subjects builds and checks, before the row statement, the tuple of each declared relationship of value whose
// subject is set: for create, the caller's too; otherwise only the field-sourced ones, since the caller of an update
// or a delete is not the subject that the create wrote (ST-SUP-Q118). A refusal breaks the handle, so a fill that
// drops it commits nothing: a caller of another subject type and a create in a command that declares no permission
// that sets a field subject are internal errors, which the generators refuse; a field subject that no tuple can
// hold, such as *, a text with a character that a provider ID cannot hold or the resource ID of another tenant, is
// ErrInvalid naming the field.
func (r *Repository[T]) subjects(value T, create bool) ([]declaredTuple, error) {
	s, e := r.st, r.entity
	var tuples []declaredTuple
	for _, rel := range e.spec.Relationships {
		var subject authz.Subject
		switch {
		case rel.Field == nil && !create:
			continue
		case rel.Field == nil:
			subject = s.principal.Subject()
			if subject.Type != rel.SubjectType {
				return nil, s.breakWith(internalf("the caller of the create of %s is a %s, and the relationship %s takes a %s", e.spec.Name, subject.Type, rel.Relation, rel.SubjectType))
			}
		default:
			subjectID := rel.Field(value)
			if subjectID == "" {
				continue
			}
			if create && s.mode == modeCommand && s.commandPermission == "" {
				return nil, s.breakWith(internalf("the command %s declares no permission and sets the subject of the relationship %s of %s, which the generators refuse", s.action, rel.Relation, e.spec.Name))
			}
			if rel.Resource {
				qualified, err := authz.ResourceID(s.tenant, subjectID)
				if err != nil {
					return nil, s.breakWith(&invalidError{field: rel.FieldName, reason: reasonSubject})
				}
				subjectID = qualified
			}
			subject = authz.Subject{Type: rel.SubjectType, ID: subjectID}
			if err := authz.CheckSubject(s.tenant, subject); err != nil {
				return nil, s.breakWith(&invalidError{field: rel.FieldName, reason: reasonSubject})
			}
		}
		tuples = append(tuples, declaredTuple{relation: rel.Relation, subject: subject})
	}
	// The row relates to its tenant at create, so a worker's tenant relation reaches it (ST-SUP-Q132).
	if create && e.spec.Tenant {
		resource, err := authz.TenantResource(s.tenant)
		if err != nil {
			return nil, s.breakWith(internalf("the tenant resource of a row of %s was not built", e.spec.Name))
		}
		tuples = append(tuples, declaredTuple{relation: authz.TenantRelation, subject: authz.Subject{Type: resource.Type, ID: resource.ID}})
	}
	return tuples, nil
}

// relateDeclared enqueues, through the authorization handle and after the row statement of the row id, op of each
// tuple of tuples: a touch on create and a delete on delete (ST-SUP-Q118). Each failure breaks the handle, so Run
// commits nothing, also when the fill drops the error.
func (r *Repository[T]) relateDeclared(ctx context.Context, id string, tuples []declaredTuple,
	op func(*authz.Handle, context.Context, authz.Tuple) (authz.Revocation, error)) error {
	s, e := r.st, r.entity
	if len(tuples) == 0 {
		return nil
	}
	resource, err := authz.NewResource(s.tenant, e.spec.ResourceType, id)
	if err != nil {
		return s.breakWith(internalf("the row of %s has a key that is not a resource key", e.spec.Name))
	}
	for _, t := range tuples {
		if err := s.setLockTimeout(ctx); err != nil {
			return s.fail(ctx, "relate", err)
		}
		if _, err := op(s.az, ctx, authz.Tuple{Resource: resource, Relation: t.relation, Subject: t.subject}); err != nil {
			return s.afterWrite(ctx, "relate", err)
		}
	}
	return nil
}

// afterWrite maps an error of the authorization handle after the row statement of a write: a database error or a
// cancel is a statement failure, and every other error is an internal error. Both break the handle, so the row is
// never committed without its tuples and its audit row (ST-SUP-Q118).
func (s *state) afterWrite(ctx context.Context, step string, err error) error {
	if sqlState(err) != "" || ctx.Err() != nil {
		return s.fail(ctx, step, err)
	}
	return s.breakWith(internalf("the %s of a tuple of the written row was not enqueued", step))
}

// unrelateAll enqueues, through the authorization handle in the transaction of the delete of the row id, after the
// row statement, the filtered delete of every tuple of the row's resource in each relation of the entity's resource
// type, whatever its subject (ST-SUP-Q118, delete path 1, and M3 of the security review of PR #272): each relation
// that the schema declares on the type (EntitySpec.Relations), so also a tuple that a fill or an operator wrote, and
// each relation of a declared relationship and of a reference. Each filter names exactly the row's resource type,
// its tenant-qualified ID and the one relation, so the tuples of every other row stay. Each failure breaks the
// handle.
func (r *Repository[T]) unrelateAll(ctx context.Context, id string) error {
	s, e := r.st, r.entity
	if e.spec.ResourceType == "" {
		return nil
	}
	relations := e.relationNames()
	if len(relations) == 0 {
		return nil
	}
	resource, err := authz.NewResource(s.tenant, e.spec.ResourceType, id)
	if err != nil {
		return s.breakWith(internalf("the row of %s has a key that is not a resource key", e.spec.Name))
	}
	for _, relation := range relations {
		if err := s.setLockTimeout(ctx); err != nil {
			return s.fail(ctx, "relate", err)
		}
		if _, err := s.az.UnrelateAll(ctx, resource, relation); err != nil {
			return s.afterWrite(ctx, "relate", err)
		}
	}
	return nil
}

// relationNames gives, once each and in a fixed order, the relations of the entity's resource type that a delete
// of a row clears: the schema's, then those of the declared relationships and the references.
func (e *Entity[T]) relationNames() []string {
	var names []string
	add := func(name string) {
		if name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	for _, name := range e.spec.Relations {
		add(name)
	}
	for _, rel := range e.spec.Relationships {
		add(rel.Relation)
	}
	for _, ref := range e.spec.Parent {
		add(ref.Relation)
	}
	if e.spec.Tenant {
		add(authz.TenantRelation)
	}
	return names
}

// Entity is the repository declaration of one entity, which NewEntity checks once.
type Entity[T any] struct {
	spec EntitySpec[T]
	// contracted is set once the contract step of the open seals of the entity dropped their clear columns: the
	// statements, the scan and the values of AfterSeal then run (ST-SUP-RP10-LC49).
	contracted atomic.Bool
	// switchedAt is the count of sealSwitches at the switch of the entity.
	switchedAt atomic.Int64
	// recheckAt is the time, in Unix nanoseconds, of the next catalog check of recheckSeal while contracted is set.
	recheckAt atomic.Int64
	// strayLogged is set once recheckSeal logged a clear column with no seal checkpoint.
	strayLogged atomic.Bool
	// owner is the runtime that registered the entity in its Config.Entities; an entity registers with one runtime
	// only.
	owner *Runtime
}

// Declared is an entity that a command or a worker declares it writes: an *Entity of any type.
type Declared interface {
	entityName() string
	readOnly() bool
	register(rt *Runtime) error
	relations() []authz.Relation
	runtimeRelations() []authz.Relation
	recheckSeal(ctx context.Context, db *sql.DB)
}

func (e *Entity[T]) entityName() string { return e.spec.Name }

// registerMu guards the owner of every entity.
var registerMu sync.Mutex

// register makes rt the one runtime of the entity, and refuses an entity that another runtime registered.
func (e *Entity[T]) register(rt *Runtime) error {
	registerMu.Lock()
	defer registerMu.Unlock()
	if e.owner != nil && e.owner != rt {
		return fmt.Errorf("command: the entity %s is registered with another runtime", e.spec.Name)
	}
	e.owner = rt
	return nil
}

// Name is the entity's name.
func (e *Entity[T]) Name() string { return e.spec.Name }

// sealedEntity tells whether the entity has a readable secret field, which needs the key ring.
func (e *Entity[T]) sealedEntity() bool { return e.spec.Sealed }

// NewEntity checks the declaration of one entity and makes its repository declaration. It refuses a missing name,
// statement or function, a statement that does not take the tenant as $1, a locked read without its lock clause,
// and a negative LockAtMost.
func NewEntity[T any](spec EntitySpec[T]) (*Entity[T], error) {
	if spec.Name == "" {
		return nil, errors.New("command: an entity needs a name")
	}
	// The resource type tenant is the tenant resource of the worker tuples: a row of it whose key is its tenant's ID
	// would be that resource, and its delete would clear the tuple of each worker (ST-SUP-RC35-OC13, ST-SUP-Q132).
	if spec.ResourceType == authz.TenantType {
		return nil, fmt.Errorf("command: the entity %s has the resource type %s, which is reserved for the tenant resource that holds the tuple of each worker; give it another resource type", spec.Name, authz.TenantType)
	}
	if (spec.EventData == nil) != (spec.EventVersion == 0) || spec.EventVersion < 0 {
		return nil, fmt.Errorf("command: the entity %s needs EventData with an EventVersion from 1, or neither", spec.Name)
	}
	if spec.ReadOnly {
		return newReadOnly(spec)
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
	if err := spec.AfterSeal.check(spec.Name); err != nil {
		return nil, err
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
	for i, r := range spec.Parent {
		if r.Entity == "" || r.ID == nil {
			return nil, fmt.Errorf("command: a reference of %s needs the referenced entity and the reader of its ID", spec.Name)
		}
		// A reference held within a field names itself and its key, and the entity reads the field (ST-SUP-RP10-LC37).
		if r.Within != "" && (r.Field == "" || r.Key == "" || spec.Within[r.Within] == nil) {
			return nil, fmt.Errorf("command: the reference of %s to %s is held within %s, which needs its Field, its Key and a reader of %s in Within", spec.Name, r.Entity, r.Within, r.Within)
		}
		if r.Relation != "" {
			if spec.ResourceType == "" || r.ResourceType == "" {
				return nil, fmt.Errorf("command: the reference of %s to %s writes a relation, which needs a resource type on both", spec.Name, r.Entity)
			}
			if _, err := authz.ParseRelation(spec.ResourceType + "#" + r.Relation); err != nil {
				return nil, fmt.Errorf("command: the relation of the reference of %s to %s: %w", spec.Name, r.Entity, err)
			}
		}
		for _, other := range spec.Parent[:i] {
			if r.Relation != "" && other.Relation == r.Relation {
				return nil, fmt.Errorf("command: two references of %s write the relation %s", spec.Name, r.Relation)
			}
		}
	}
	for i, r := range spec.Relationships {
		if spec.ResourceType == "" || r.SubjectType == "" {
			return nil, fmt.Errorf("command: the relationship %s of %s needs a resource type on the entity and a subject type", r.Relation, spec.Name)
		}
		if _, err := authz.ParseRelation(spec.ResourceType + "#" + r.Relation); err != nil {
			return nil, fmt.Errorf("command: the relationship of %s: %w", spec.Name, err)
		}
		if (r.Field == nil) != (r.FieldName == "") {
			return nil, fmt.Errorf("command: the relationship %s of %s needs the field name with the field, and only then", r.Relation, spec.Name)
		}
		if (r.Field == nil && r.Resource) || slices.ContainsFunc(spec.Relationships[:i], func(o Relationship[T]) bool { return o.Relation == r.Relation }) ||
			slices.ContainsFunc(spec.Parent, func(o Reference[T]) bool { return o.Relation == r.Relation }) {
			return nil, fmt.Errorf("command: the relationship %s of %s is given twice, also writes a reference's relation, or takes a caller as a resource", r.Relation, spec.Name)
		}
	}
	if spec.Tenant && (spec.ResourceType == "" || slices.ContainsFunc(spec.Relationships, func(o Relationship[T]) bool { return o.Relation == authz.TenantRelation }) ||
		slices.ContainsFunc(spec.Parent, func(o Reference[T]) bool { return o.Relation == authz.TenantRelation })) {
		return nil, fmt.Errorf("command: the relation tenant of %s needs a resource type on the entity, and no relationship or reference may write it", spec.Name)
	}
	for _, name := range spec.Relations {
		if spec.ResourceType == "" {
			return nil, fmt.Errorf("command: the schema relations of %s need a resource type on the entity", spec.Name)
		}
		if _, err := authz.ParseRelation(spec.ResourceType + "#" + name); err != nil {
			return nil, fmt.Errorf("command: a schema relation of %s: %w", spec.Name, err)
		}
	}
	spec.Parent = slices.Clone(spec.Parent)
	spec.Relationships = slices.Clone(spec.Relationships)
	spec.Relations = slices.Clone(spec.Relations)
	return &Entity[T]{spec: spec}, nil
}

// newReadOnly checks the read-only declaration of an entity that a worker reads (ST-SUP-RP10-LC53): only Select, which
// takes the tenant as $1, and Scan, Clone and ValidID; no write statement, no write function, no secret, no relation
// and no lock.
func newReadOnly[T any](spec EntitySpec[T]) (*Entity[T], error) {
	if !strings.Contains(spec.Select, "$1") || spec.Scan == nil || spec.Clone == nil || spec.ValidID == nil {
		return nil, fmt.Errorf("command: the read-only entity %s needs a Select that takes the tenant as $1, Scan, Clone and ValidID", spec.Name)
	}
	if spec.SelectForUpdate != "" || spec.SelectForDelete != "" || spec.Insert != "" || spec.Update != "" || spec.Delete != "" ||
		spec.Values != nil || spec.State != nil || spec.KeyChanged != nil || spec.Sealed || len(spec.Relationships) > 0 ||
		len(spec.Relations) > 0 || spec.Tenant || spec.LockAtMost != 0 || spec.EventData != nil {
		return nil, fmt.Errorf("command: the read-only entity %s has a statement other than Select, a write function, a sealed field, a relation, events or a Lock at most", spec.Name)
	}
	if spec.ResourceType != "" && spec.ReadPermission == "" {
		return nil, fmt.Errorf("command: the entity %s has a resource type and no read permission", spec.Name)
	}
	for _, r := range spec.Parent {
		if r.Entity == "" || r.ID == nil {
			return nil, fmt.Errorf("command: a reference of %s needs the referenced entity and the reader of its ID", spec.Name)
		}
	}
	spec.Parent = slices.Clone(spec.Parent)
	return &Entity[T]{spec: spec}, nil
}

// eventData gives the declared event fields of a value and their version, or nil for an entity that declares none
// (ST-SUP-RP10-LC55).
func (e *Entity[T]) eventData(value T) *eventData {
	if e.spec.EventData == nil {
		return nil
	}
	return &eventData{fields: e.spec.EventData(value), version: e.spec.EventVersion}
}

// readOnly tells whether the entity is the read-only declaration of an entity that a worker reads.
func (e *Entity[T]) readOnly() bool { return e.spec.ReadOnly }

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
//
// The generator emits each list of a command's or a worker's declaration in its contract, with its statements built
// from the entity of the release and its Values (ST-SUP-RC35-OC15, ST-SUP-RC32-OC16), and the CommandSpec.Lists or
// WorkerSpec.Lists of the declaration names it; the generated upsert key of a collection is a list of its upsert
// command too. Since runtime v0.127.0 every handle takes only a list that its declaration declares, so no fill gives
// SQL text (ST-SUP-RP10-LC38): declare the list under lists: of the command or the worker.
type ListSpec struct {
	Name string
	Read string
	Lock string
	// ReadAfterSeal and LockAfterSeal are Read and Lock after the contract step of each open seal of the entity, with
	// the sealed column alone, which the runtime takes once the contract dropped the clear columns; empty for an
	// entity with no open seal (ST-SUP-RP10-LC49).
	ReadAfterSeal, LockAfterSeal string
	// Values are the declared values of the filter, for the parameters from $3 in their order: each a constant of
	// the field type, or the time of the run. A list with Values takes no filter values from the fill: the handle
	// binds them (ST-DECL-INV94, ST-SUP-RC35-OC15). DeclaredValues makes it; a pointer keeps ListSpec comparable.
	Values *ListValues
}

// ListValues are the declared values of the filter of a list, in the order of their parameters.
type ListValues struct{ values []ListValue }

// DeclaredValues gives the declared values of a list filter, for ListSpec.Values.
func DeclaredValues(values ...ListValue) *ListValues {
	return &ListValues{values: slices.Clone(values)}
}

// all gives the values; a nil ListValues has none.
func (v *ListValues) all() []ListValue {
	if v == nil {
		return nil
	}
	return v.values
}

// ListValue is one declared value of a list filter: Constant, the time of the run when RunTime is true, which is the
// due tick time of a worker run and the time at the start of a consumer run, or the parameter Param, which the fill
// gives at call time through the generated function of the list (List.With). The handle binds each value as a SQL
// parameter, never as text (ST-SUP-RP10-LC38).
type ListValue struct {
	Constant any
	RunTime  bool
	// Param is the declared name of a parameter, such as order_id; empty for a constant or the time of the run.
	Param string
	// Key tells that the parameter holds the key of a row, such as a reference: the handle checks it with the key
	// check of the entity and gives an empty list for a value that the check refuses (ST-SUP-RP10-LC38).
	Key bool
	// KeyOf is the kind of key of the entity whose key the parameter holds, such as the referenced entity of a
	// reference field: KeyUUID checks it with ValidUUID. Empty takes the key check of the listed entity, as code of a
	// runtime before v0.131.0 has it (security review of PR #416, sec-416b). It is a kind, not a function, so
	// ListValue stays comparable (ST-STD-RUNTIME-R03).
	KeyOf KeyKind
	// Parent is the entity of the parent row of a list over a child entity, such as Order: the fill gives that loaded
	// row through With, and the handle decides on it before it reads a row and binds its ID (ST-SUP-RP10-LC38).
	Parent string
}

// KeyKind is the kind of key of an entity, which a key parameter of a list is checked by.
type KeyKind string

// KeyUUID is the key of an entity whose keys are UUIDs, as every generated entity has: ValidUUID checks it.
const KeyUUID KeyKind = "uuid"

// List is one checked list of an entity, or that list with its parameters bound (With).
type List[T any] struct {
	entity *Entity[T]
	spec   ListSpec
	// binding is the binding that With made, or nil for the list that NewList made. A pointer keeps List comparable.
	binding *listBinding[T]
}

// listBinding is the binding of a list: the list that NewList made, the bound parameters, and why they do not fit
// the declared parameters.
type listBinding[T any] struct {
	declared *List[T]
	params   []any
	bad      string
}

// AnyList is a list of any entity, which CommandSpec.Lists and WorkerSpec.Lists hold. Only a *List made by NewList
// is one.
type AnyList interface {
	listEntity() Declared
	listName() string
	listBound() bool
	listNil() bool
	// listHeld gives the list that the handle looks up and the copy of its declaration that the handle holds it to.
	listHeld() (AnyList, listCopy)
}

// listCopy is a copy of the declaration of a list: its entity, its name, its statements and its values. The handle
// takes a declared list only while it still has this declaration, so a fill that overwrites the list through its
// pointer, such as *commands.PlaceOrderOfOrder = *command.Must(command.NewList(...)), gets a list that the handle
// refuses (security review of PR #421, L1).
type listCopy struct {
	entity                       Declared
	name, read, lock             string
	readAfterSeal, lockAfterSeal string
	values                       []ListValue
}

func (c listCopy) same(o listCopy) bool {
	return c.entity == o.entity && c.name == o.name && c.read == o.read && c.lock == o.lock &&
		c.readAfterSeal == o.readAfterSeal && c.lockAfterSeal == o.lockAfterSeal && slices.Equal(c.values, o.values)
}

func (l *List[T]) listCopy() listCopy {
	return listCopy{entity: l.entity, name: l.spec.Name, read: l.spec.Read, lock: l.spec.Lock, readAfterSeal: l.spec.ReadAfterSeal,
		lockAfterSeal: l.spec.LockAfterSeal, values: slices.Clone(l.spec.Values.all())}
}

func (l *List[T]) listHeld() (AnyList, listCopy) { return l, l.listCopy() }

// heldList is a declared list with the copy of its declaration that Snapshot took.
type heldList struct {
	AnyList
	held listCopy
}

func (h heldList) listHeld() (AnyList, listCopy) { return h.AnyList, h.held }

// Snapshot gives the declared lists for CommandSpec.Lists or WorkerSpec.Lists, each with a copy of its declaration
// taken now. The generated contract package calls it when it starts, before the init of any fill runs, so a fill
// that later overwrites a list through its pointer gets a list that the handle refuses (ST-SUP-RP10-LC38, security
// review of PR #421, L1). A nil list stays nil, and the spec refuses it.
func Snapshot(lists ...AnyList) []AnyList {
	out := make([]AnyList, len(lists))
	for i, l := range lists {
		if l == nil || l.listNil() {
			out[i] = l
			continue
		}
		list, held := l.listHeld()
		out[i] = heldList{AnyList: list, held: held}
	}
	return out
}

func (l *List[T]) listEntity() Declared { return l.entity }
func (l *List[T]) listName() string     { return l.spec.Name }
func (l *List[T]) listBound() bool      { return l.binding != nil }
func (l *List[T]) listNil() bool        { return l == nil }

// root gives the list that NewList made.
func (l *List[T]) root() *List[T] {
	if l.binding != nil {
		return l.binding.declared
	}
	return l
}

// With gives the list l with its parameters bound, in the order of the Param values of its Values. The generated
// function of a declared list with parameters calls it with typed Go values, such as the ID of a request or a field
// of a message (ST-SUP-RP10-LC38). The handle binds each one as a SQL parameter, never as text, and refuses, as an
// internal error, a binding whose count does not fit or whose value is not a bool, a string, an int32, an int64, a
// float32, a float64 or a time.Time, or a pointer to one, which is the Go value of an optional field and binds NULL
// when it is nil.
func (l *List[T]) With(params ...any) *List[T] {
	if l == nil {
		return nil
	}
	root := l.root()
	binding := &listBinding[T]{declared: root, params: slices.Clone(params)}
	bound := &List[T]{entity: root.entity, spec: root.spec, binding: binding}
	var slots []ListValue
	for _, v := range root.spec.Values.all() {
		if v.Param != "" || v.Parent != "" {
			slots = append(slots, v)
		}
	}
	if len(params) != len(slots) {
		binding.bad = fmt.Sprintf("it takes %d parameters and got %d", len(slots), len(params))
		return bound
	}
	for i, p := range params {
		if slots[i].Parent != "" {
			if ref, ok := p.(Ref); !ok || ref == nil || refOf(ref).entity != slots[i].Parent {
				binding.bad = fmt.Sprintf("its parameter %d is not a loaded row of %s", i+1, slots[i].Parent)
				return bound
			}
			continue
		}
		switch p.(type) {
		case bool, string, int32, int64, float32, float64, time.Time,
			*bool, *string, *int32, *int64, *float32, *float64, *time.Time:
		default:
			binding.bad = fmt.Sprintf("its parameter %d is not a bool, a string, an int32, an int64, a float32, a float64 or a time", i+1)
			return bound
		}
	}
	return bound
}

// NewList checks one list of e. It refuses a list whose statements do not take the tenant as $1 and whose Lock does
// not order by the key and lock FOR NO KEY UPDATE. A list with a Lock needs an entity that declares Lock at most.
//
// Every handle takes only a list that the generated CommandSpec.Lists or WorkerSpec.Lists of its declaration
// declares, so a list that a fill makes itself is refused at its use (ST-SUP-RP10-LC38). A value is at most one of a
// constant, the time of the run and a parameter, and each parameter name is given once.
func NewList[T any](e *Entity[T], spec ListSpec) (*List[T], error) {
	if e == nil || spec.Name == "" || !strings.Contains(spec.Read, "$1") {
		return nil, errors.New("command: a list needs its entity, a name and a Read statement that takes the tenant as $1")
	}
	// The statements after the seal contract come with an entity with an open seal, and in the same shape.
	if (spec.ReadAfterSeal != "") != (e.spec.AfterSeal != nil) || (spec.LockAfterSeal != "") != (spec.Lock != "" && e.spec.AfterSeal != nil) ||
		(spec.ReadAfterSeal != "" && !strings.Contains(spec.ReadAfterSeal, "$1")) {
		return nil, fmt.Errorf("command: the list %s gives its statements after the seal contract only for an entity with an open seal, and each takes the tenant as $1", spec.Name)
	}
	for _, lock := range []string{spec.Lock, spec.LockAfterSeal} {
		if lock == "" {
			continue
		}
		upper := strings.ToUpper(lock)
		if !strings.Contains(lock, "$1") || !strings.Contains(upper, "ORDER BY") ||
			!strings.Contains(upper, "FOR NO KEY UPDATE") {
			return nil, fmt.Errorf("command: the Lock of the list %s does not take the tenant as $1, order by the key and lock FOR NO KEY UPDATE", spec.Name)
		}
	}
	if spec.Lock != "" {
		if e.spec.LockAtMost == 0 || e.spec.ReadOnly {
			return nil, fmt.Errorf("command: the list %s locks rows of %s, which declares no Lock at most or is read-only", spec.Name, e.spec.Name)
		}
	}
	params := map[string]bool{}
	for i, v := range spec.Values.all() {
		kinds := 0
		for _, set := range []bool{v.Constant != nil, v.RunTime, v.Param != "", v.Parent != ""} {
			if set {
				kinds++
			}
		}
		if kinds > 1 {
			return nil, fmt.Errorf("command: the value %d of the list %s is more than one of a constant, the time of the run, a parameter and a parent", i+1, spec.Name)
		}
		if v.Parent != "" && !e.isChild() {
			return nil, fmt.Errorf("command: the list %s takes a parent row, and %s is not a child entity", spec.Name, e.spec.Name)
		}
		if v.Param != "" {
			if params[v.Param] {
				return nil, fmt.Errorf("command: the list %s declares the parameter %s twice", spec.Name, v.Param)
			}
			params[v.Param] = true
		}
	}
	if spec.Values != nil {
		spec.Values = DeclaredValues(spec.Values.all()...)
	}
	return &List[T]{entity: e, spec: spec}, nil
}

// checkLists gives the lists that the command or worker owner declares, by list. It refuses a nil list, a list with
// bound parameters, which is not the declared list, and a list of an entity that the owner does not write.
func checkLists(owner string, lists []AnyList, writes ...map[Declared]bool) (map[AnyList]listCopy, error) {
	out := make(map[AnyList]listCopy, len(lists))
	for _, l := range lists {
		if l == nil || l.listNil() {
			return nil, fmt.Errorf("command: %s declares a nil list", owner)
		}
		if l.listBound() {
			return nil, fmt.Errorf("command: %s declares the list %s with bound parameters; declare the list itself", owner, l.listName())
		}
		declared := false
		for _, set := range writes {
			declared = declared || set[l.listEntity()]
		}
		if !declared {
			return nil, fmt.Errorf("command: %s declares the list %s of %s, which it does not write", owner, l.listName(), l.listEntity().entityName())
		}
		list, held := l.listHeld()
		out[list] = held
	}
	return out, nil
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
	entityRef    any
	resourceType string
	id           string
	token        string
	lock         lockMode
	// value is the loaded value itself, which a within check reads (ST-SUP-RP10-LC37).
	value Ref
}

func refOf(r Ref) rowRef {
	ref, ok := loaded.Of(r)
	if !ok {
		return rowRef{}
	}
	st, _ := ref.Owner.(*state)
	return rowRef{st: st, entity: ref.EntityName, entityRef: ref.Entity, resourceType: ref.ResourceType, id: ref.ID,
		token: ref.Token, lock: lockMode(ref.Lock), value: r}
}

// newLoaded makes the loaded value of one row that the handle state s read or wrote.
func newLoaded[T any](value T, meta Meta, e *Entity[T], s *state, lock lockMode) Loaded[T] {
	return loaded.New(value, e.spec.Clone, loaded.Ref{Owner: s, Entity: e, EntityName: e.spec.Name, ResourceType: e.spec.ResourceType,
		ID: meta.ID, Version: meta.Version, Token: meta.Token, Lock: uint8(lock)})
}

// isChild reports whether the entity is a child: it has a Parent and no resource type, so it takes its authorization
// from its parent (ST-SUP-RP10-LC05).
func (r *Repository[T]) isChild() bool { return r.entity.isChild() }

func (e *Entity[T]) isChild() bool { return len(e.spec.Parent) > 0 && e.spec.ResourceType == "" }

// parentsOf gives the set references of the loaded value ref, for a child entity, and false for any other entity or
// a value of another entity.
func (e *Entity[T]) parentsOf(ref Ref) ([]reference, bool) {
	l, ok := ref.(Loaded[T])
	if !ok || !e.isChild() {
		return nil, false
	}
	value, _, ok := loaded.Fields(l)
	if !ok {
		return nil, false
	}
	return e.references(value), true
}

// nonNil drops the nil values of parents, so Create(ctx, value, nil) gives no parent.
func nonNil(parents []Ref) []Ref {
	out := make([]Ref, 0, len(parents))
	for _, p := range parents {
		if p != nil {
			out = append(out, p)
		}
	}
	return out
}

// matchParents finds, for each of refs, the loaded value of this handle among parents that is the referenced row,
// the same entity and the same ID. It refuses, as an internal error, a reference with no loaded value and a loaded
// value of another handle; used marks each parent that a reference takes.
func (s *state) matchParents(verb, entity string, refs []reference, parents []Ref, used []bool) ([]rowRef, error) {
	out := make([]rowRef, len(refs))
	for i, ref := range refs {
		found := -1
		for j, p := range parents {
			if pr := refOf(p); pr.st == s && pr.entity == ref.entity && pr.id == ref.id && pr.resourceType == ref.resourceType {
				found = j
				break
			}
		}
		if found < 0 {
			for _, p := range parents {
				if refOf(p).st != s {
					return nil, s.breakWith(internalf("a parent of the %s of %s is a loaded value of another transaction", verb, entity))
				}
			}
			return nil, s.breakWith(internalf("the %s of %s needs the loaded value of each row that the row refers to", verb, entity))
		}
		used[found] = true
		out[i] = refOf(parents[found])
	}
	return out, nil
}

// allUsed refuses a parent that no reference took.
func (s *state) allUsed(verb, entity string, used []bool) error {
	for _, u := range used {
		if !u {
			return s.breakWith(internalf("the %s of %s takes a parent that the row does not refer to", verb, entity))
		}
	}
	return nil
}

// decideReference decides one set reference on its loaded referenced row p (ST-SUP-RP10-LC05): a command decides the
// permission that the declaration names for the reference, or its own permission when it names none; a worker
// decides its own permission (ST-SUP-RP10-LC17). A deny is not found.
func (s *state) decideReference(ctx context.Context, ref reference, p rowRef) error {
	if s.mode == modeCommand && ref.permission != "" {
		if p.resourceType == "" {
			return nil
		}
		ok, err := s.evaluate(ctx, s.az, ref.permission, p.resourceType, p.id, p.token, nil)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: %s", ErrNotFound, p.entity)
		}
		return nil
	}
	return s.decideWrite(ctx, p.resourceType, p.entity, p.id, p.token)
}

// decideParents decides, for a locked read, a list, an update or a delete of a child, on each row that each of
// values refers to, as a create does (ST-SUP-RP10-LC05, ST-SUP-RP10-LC17): parents must hold the loaded value of each
// such row, and nothing else. Each row and permission is decided once. An entity that is not a child takes no
// parent. A deny is not found.
func (r *Repository[T]) decideParents(ctx context.Context, verb string, parents []Ref, values ...T) error {
	s, e := r.st, r.entity
	parents = nonNil(parents)
	if !r.isChild() {
		if len(parents) > 0 {
			return s.breakWith(internalf("the %s of %s takes no parent: it is not a child entity", verb, e.spec.Name))
		}
		return nil
	}
	used := make([]bool, len(parents))
	decided := map[reference]bool{}
	for _, value := range values {
		refs := e.references(value)
		rows, err := s.matchParents(verb, e.spec.Name, refs, parents, used)
		if err != nil {
			return err
		}
		for i, ref := range refs {
			if decided[ref] {
				continue
			}
			decided[ref] = true
			if err := s.decideReference(ctx, ref, rows[i]); err != nil {
				return err
			}
		}
	}
	return s.allUsed(verb, e.spec.Name, used)
}

// decideMove decides the references of an update. A child decides on each row it refers to before and after the
// update (decideParents). Any other entity with references, such as one with a resource type of its own, decides on
// each reference that the update moves, on the new referenced row: parents must hold its loaded value and nothing
// else; an update that moves no reference takes no parent (ST-SUP-RP10-LC05, ST-SUP-RP10-LC17). It gives the moved
// references and their new rows.
func (r *Repository[T]) decideMove(ctx context.Context, parents []Ref, from, value T) ([]moved, error) {
	s, e := r.st, r.entity
	if r.isChild() || len(e.spec.Parent) == 0 {
		if err := r.decideParents(ctx, "update", parents, from, value); err != nil {
			return nil, err
		}
		// A child holds the loaded value of each row that it refers to, so each reference held within a field is
		// compared with its row (ST-SUP-RP10-LC37).
		refs := e.references(value)
		if len(refs) == 0 {
			return nil, nil
		}
		rows, err := s.matchParents("update", e.spec.Name, refs, nonNil(parents), make([]bool, len(nonNil(parents))))
		if err != nil {
			return nil, err
		}
		for i, ref := range refs {
			if err := r.checkWithin(value, ref, rows[i]); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	parents = nonNil(parents)
	var moves []moved
	var refs, held []reference
	for _, ref := range e.spec.Parent {
		oldID, newID := ref.ID(from), ref.ID(value)
		if oldID == newID {
			// A reference that stays while the field that it is held within moves is compared with its row too, so
			// parents must hold that row (ST-SUP-RP10-LC37).
			if ref.Within != "" && newID != "" && e.withinMoved(ref.Within, from, value) {
				held = append(held, reference{entity: ref.Entity, resourceType: ref.ResourceType, id: newID,
					permission: ref.Permission, relation: ref.Relation, field: ref.Field, within: ref.Within, key: ref.Key})
			}
			continue
		}
		m := moved{entity: ref.Entity, resourceType: ref.ResourceType, relation: ref.Relation, oldID: oldID, newID: newID}
		moves = append(moves, m)
		if newID != "" {
			refs = append(refs, reference{entity: ref.Entity, resourceType: ref.ResourceType, id: newID,
				permission: ref.Permission, relation: ref.Relation, field: ref.Field, within: ref.Within, key: ref.Key})
		}
	}
	if len(moves) == 0 && len(held) == 0 {
		if len(parents) > 0 {
			return nil, s.breakWith(internalf("an update of %s that keeps its references takes no parent", e.spec.Name))
		}
		return nil, nil
	}
	used := make([]bool, len(parents))
	rows, err := s.matchParents("move", e.spec.Name, refs, parents, used)
	if err != nil {
		return nil, err
	}
	heldRows, err := s.matchParents("move", e.spec.Name, held, parents, used)
	if err != nil {
		return nil, err
	}
	if err := s.allUsed("move", e.spec.Name, used); err != nil {
		return nil, err
	}
	for i, ref := range refs {
		if err := s.decideReference(ctx, ref, rows[i]); err != nil {
			return nil, err
		}
	}
	// A held reference stays, so it is not decided again; it is only compared (ST-SUP-RP10-LC05, ST-SUP-RP10-LC37).
	checks, checked := slices.Concat(refs, held), slices.Concat(rows, heldRows)
	for i, ref := range checks {
		if err := r.checkWithin(value, ref, checked[i]); err != nil {
			return nil, err
		}
	}
	return moves, nil
}

// reasonWithin is the fixed reason of a refused reference held within a field (ST-SUP-RP10-LC37): it names the
// declared field and never a value.
func reasonWithin(within string) string {
	return "the referenced row has another " + within + "; the reference is held within " + within
}

// reasonReferred is the fixed reason of a refused write of a row that another entity's reference is held within
// (ST-SUP-RP10-LC37).
func reasonReferred(referrer string) string {
	return "a row refers to this row through " + referrer + ", which is held within this field"
}

// withinMoved tells whether an update from before to after changes the field within, or whether the entity cannot
// read it, which checkWithin refuses.
func (e *Entity[T]) withinMoved(within string, before, after T) bool {
	read := e.spec.Within[within]
	return read == nil || read(before) != read(after)
}

// withinReader is an entity that reads a within field of its own loaded values; every *Entity[T] is one.
type withinReader interface {
	withinOf(ref Ref, field string) (string, bool)
}

// withinOf reads the field of a loaded value of the entity through EntitySpec.Within.
func (e *Entity[T]) withinOf(ref Ref, field string) (string, bool) {
	l, ok := ref.(Loaded[T])
	if !ok {
		return "", false
	}
	value, _, ok := loaded.Fields(l)
	read := e.spec.Within[field]
	if !ok || read == nil {
		return "", false
	}
	return read(value), true
}

// checkWithin compares, for a reference held within a field, the field of value with the field of the loaded
// referenced row p, before the row statement (ST-SUP-RP10-LC37). A difference is ErrInvalid that names the
// reference and writes nothing; a field of value that is not set is not compared, as the composite key does not
// check it (MATCH SIMPLE). An entity that cannot read the field is a fault of the generated code, an internal error.
func (r *Repository[T]) checkWithin(value T, ref reference, p rowRef) error {
	if ref.within == "" {
		return nil
	}
	s, e := r.st, r.entity
	read := e.spec.Within[ref.within]
	if read == nil {
		return s.breakWith(internalf("the entity %s does not read the field %s that its reference %s is held within", e.spec.Name, ref.within, ref.field))
	}
	mine := read(value)
	if mine == "" {
		return nil
	}
	reader, ok := p.entityRef.(withinReader)
	if !ok {
		return s.breakWith(internalf("the row that %s.%s refers to is no loaded value of this handle", e.spec.Name, ref.field))
	}
	theirs, ok := reader.withinOf(p.value, ref.within)
	if !ok {
		return s.breakWith(internalf("the entity %s does not read the field %s that %s.%s is held within", p.entity, ref.within, e.spec.Name, ref.field))
	}
	if theirs != mine {
		return s.breakWith(&invalidError{field: ref.field, reason: reasonWithin(ref.within)})
	}
	return nil
}

// failWrite is fail for the row statement of a create or an update of the entity: a foreign key violation (SQLSTATE
// 23503) of the composite key of a reference held within a field, from a race or from a write of the referenced
// row, and a violation (SQLSTATE 23514) of the CHECK of its within field, is ErrInvalid naming the reference, never an
// internal error and never a run again (ST-SUP-RP10-LC37). The
// key of a reference of this entity names the reference; the key of a reference of another entity to this one
// names the field of this entity that it is held within.
func (r *Repository[T]) failWrite(ctx context.Context, step string, err error) error {
	s, e := r.st, r.entity
	if refused := r.failConstraint(err); refused != nil && !r.withinConstraint(err) {
		return s.breakWith(refused)
	}
	var pg *pgconn.PgError
	// A CHECK of the within field (SQLSTATE 23514) refuses a set reference with no within field, as from a worker
	// that writes the row outside the collection of the scope (ST-SUP-RC01-OC46).
	if sqlState(err) == "23514" && errors.As(err, &pg) && pg.ConstraintName != "" {
		for _, ref := range e.spec.Parent {
			if ref.Within != "" && ref.Check == pg.ConstraintName {
				return s.breakWith(&invalidError{field: ref.Field, reason: reasonWithin(ref.Within)})
			}
		}
	}
	if sqlState(err) == "23503" && errors.As(err, &pg) && pg.ConstraintName != "" {
		for _, ref := range e.spec.Parent {
			if ref.Key == pg.ConstraintName {
				return s.breakWith(&invalidError{field: ref.Field, reason: reasonWithin(ref.Within)})
			}
		}
		for _, k := range e.spec.Referrers {
			if k.Key == pg.ConstraintName {
				return s.breakWith(&invalidError{field: k.Within, reason: reasonReferred(k.Field)})
			}
		}
	}
	return r.failUnique(ctx, step, err)
}

// failUnique is fail for the row statement of a create or an update, which keeps the declared fields of the unique key
// that a unique violation (SQLSTATE 23505) broke in the conflict, when the entity declares the index in Unique
// (ST-SUP-RP10-LC48). The first conflict of the transaction is the one that Run returns, so only it takes the key.
func (r *Repository[T]) failUnique(ctx context.Context, step string, err error) error {
	failed := r.st.failOf(ctx, r.entity, step, err)
	var pg *pgconn.PgError
	var c *conflictError
	if sqlState(err) == "23505" && errors.As(err, &pg) && errors.As(failed, &c) && c.unique && c.key == "" {
		c.key = r.entity.spec.Unique[pg.ConstraintName]
	}
	return failed
}

// UniqueKey gives the declared fields of the unique key that the conflict err broke, or empty for any other error and
// for a unique violation of an index that the entity does not declare (ST-SUP-RP10-LC48).
func UniqueKey(err error) string {
	var c *conflictError
	if errors.As(err, &c) && c.unique {
		return c.key
	}
	return ""
}

// reasonConstraint is the fixed reason of a value that a CHECK, a NOT NULL or a foreign key of the entity's table
// refused: the value breaks a constraint that the declaration gives the field.
const reasonConstraint = "the value breaks a declared constraint of the field"

// checkFields runs the generated Check of the declared constraints of the fields on value, before any decision and
// any statement, and refuses a value that breaks one with ErrInvalid naming the field (walkthrough 10, B4).
func (r *Repository[T]) checkFields(value T) error {
	if r.entity.spec.Check == nil {
		return nil
	}
	field, reason, ok := r.entity.spec.Check(value)
	if ok {
		return nil
	}
	if reason == "" {
		reason = reasonConstraint
	}
	return r.st.breakWith(&invalidError{field: field, reason: reason})
}

// withinConstraint reports whether err is the violation of a key or a CHECK of a reference held within a field,
// which failWrite answers with its own reason (ST-SUP-RP10-LC37).
func (r *Repository[T]) withinConstraint(err error) bool {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.ConstraintName == "" {
		return false
	}
	for _, ref := range r.entity.spec.Parent {
		if ref.Within != "" && (ref.Check == pg.ConstraintName || ref.Key == pg.ConstraintName) {
			return true
		}
	}
	for _, k := range r.entity.spec.Referrers {
		if k.Key == pg.ConstraintName {
			return true
		}
	}
	return false
}

// failConstraint gives the ErrInvalid of a statement that a CHECK (SQLSTATE 23514), a NOT NULL (23502) or a foreign
// key (23503) of the entity's table refused, naming the declared field that Constraints gives for the constraint, or
// for the column of a NOT NULL; and nil for every other error and for a constraint that Constraints does not name. A unique violation (23505) stays a conflict. The log of the failed statement names its class, as fail
// does for every statement (walkthrough 10, B4).
func (r *Repository[T]) failConstraint(err error) error {
	code := sqlState(err)
	if code != "23514" && code != "23502" && code != "23503" {
		return nil
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return nil
	}
	constraints := r.entity.spec.Constraints
	field := constraints[pg.ConstraintName]
	if field == "" && code == "23502" {
		field = constraints[pg.ColumnName]
	}
	if field == "" {
		// A constraint that the declaration does not give a field, such as a key that a release added by hand,
		// stays an internal error that names no field (ST-SUP-RP10-LC37).
		return nil
	}
	if ok, held := statementLog.Allow(r.st.action + "/invalid/" + code[:2]); ok {
		slog.Warn("command: a statement refused a value of a field", "action", r.st.action, "entity", r.entity.spec.Name,
			"field", field, "cause", causeData, "sqlstate_class", code[:2], "held_back", held)
	}
	return &invalidError{field: field, reason: reasonConstraint}
}

// moved is one reference that an update moves: the referenced entity, its resource type, the relation, and the old
// and the new referenced ID.
type moved struct {
	entity, resourceType, relation, oldID, newID string
}

// checkCreate takes the loaded value of each set reference of a new row of entity from parents (ST-SUP-RP10-LC05). A
// command must give each one; a worker may give none, and then decides nothing on them, or each one. It refuses, as
// an internal error, a parent that the row does not refer to, a parent of another handle, and a parent for an entity
// that has no references.
func (r *Repository[T]) checkCreate(refs []reference, parents []Ref) ([]rowRef, error) {
	s, e := r.st, r.entity
	switch {
	case len(e.spec.Parent) == 0 && len(parents) > 0:
		return nil, s.breakWith(internalf("a create of %s takes no parent: the entity has none", e.spec.Name))
	case len(parents) == 0 && len(refs) > 0 && s.mode != modeCommand:
		return nil, nil
	}
	used := make([]bool, len(parents))
	rows, err := s.matchParents("create", e.spec.Name, refs, parents, used)
	if err != nil {
		return nil, err
	}
	return rows, s.allUsed("create", e.spec.Name, used)
}

// relate writes the relation of each reference of a row of the entity that has one (ST-SUP-RP10-LC05): a touch of
// <resource type>:<row>#<relation>@<referenced type>:<referenced ID> through the authorization handle, so the relay
// writes it after the commit; and, for a moved reference, the delete of the old tuple. A referenced row with no
// resource type has no relation.
func (r *Repository[T]) relate(ctx context.Context, id string, touch, drop []reference) error {
	s, e := r.st, r.entity
	if e.spec.ResourceType == "" {
		return nil
	}
	write := func(ref reference, op func(*authz.Handle, context.Context, authz.Tuple) (authz.Revocation, error)) error {
		if ref.relation == "" || ref.resourceType == "" {
			return nil
		}
		resource, err := authz.NewResource(s.tenant, e.spec.ResourceType, id)
		if err != nil {
			return s.breakWith(internalf("the row of %s has a key that is not a resource key", e.spec.Name))
		}
		subject, err := authz.ResourceID(s.tenant, ref.id)
		if err != nil {
			return s.breakWith(internalf("a reference of %s has a key that is not a resource key", e.spec.Name))
		}
		if err := s.setLockTimeout(ctx); err != nil {
			return s.fail(ctx, "relate", err)
		}
		if _, err := op(s.az, ctx, authz.Tuple{Resource: resource, Relation: ref.relation,
			Subject: authz.Subject{Type: ref.resourceType, ID: subject}}); err != nil {
			return s.afterWrite(ctx, "relate", err)
		}
		return nil
	}
	for _, ref := range drop {
		if err := write(ref, (*authz.Handle).Unrelate); err != nil {
			return err
		}
	}
	for _, ref := range touch {
		if err := write(ref, (*authz.Handle).Relate); err != nil {
			return err
		}
	}
	return nil
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
	// notGranted is set when a statement failed with SQLSTATE 42501: the role of the transaction lacks a privilege,
	// which a dispatch again does not change (decision on ST-SUP-Q141).
	notGranted bool
	// sealSwitches is the count of the switches of entities to their statements after a seal contract when the
	// attempt began: a statement of an entity that switched later can still be one of the statements before the
	// contract (ST-SUP-RP10-LC49; security review of PR #452, L2).
	sealSwitches int64
	// logged is set when a log line of ST-SUP-RP10-LC39 named the cause of the call's failure: a failed statement, or
	// an internal error that breakWith logged. Run then writes no command.fill.failed line (ST-SUP-RP10-LC51).
	logged bool

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
	// reads are the read-only entities that a worker declares it reads (ST-SUP-RP10-LC53).
	reads map[Declared]bool
	// attempts are the counts that the call resets when it commits, by row, and chargeFailed is set when a charge of a
	// check of a counted value did not commit (ST-SUP-RP10-LC54).
	attempts     map[attemptKey]*attemptRow
	chargeFailed bool
	// checks count the checks of each stored value of a counted row in this attempt, and charges are the checks that
	// the attempts of the call charged, which a rerun does not charge again for the same stored value (security
	// review of PR #451, second review, I1; of PR #479, L2).
	checks       map[chargeKey]int
	charges      *charges
	destinations map[string]Destination
	// lists are the lists that the command or the worker declares, which its handle takes only (ST-SUP-RP10-LC38).
	lists map[AnyList]listCopy
	// runTime is the time of the run that a list value RunTime binds: the due tick of a worker run, the start of a
	// command attempt or the start of a consumer run.
	runTime time.Time
	// ownRelations are the relations that the fill may write itself; the handle is bound with the relations of the
	// references too, which only the runtime writes (ST-SUP-RP10-LC05).
	ownRelations map[authz.Relation]bool
	// inbound gives the inbound key of an effect for the entity ID (ST-SUP-RP10-LC08, ST-SUP-RP10-LC21).
	inbound func(id string) string
	staged  []outbox.Message
	stagedK map[string]bool
	// effects counts the effects that the fill staged, which a call holds at most outbox.MaxBatchSize of.
	effects int
	// changeEvents are the change events of the written entities by entity name, or nil when the declaration gives
	// none; changes counts the change events of one name for one row in the call (ST-SUP-RP10-LC36).
	changeEvents map[string]ChangeEvents
	changes      map[string]int
	events       []audit.Event
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
	// A sealed value that does not open is an internal error of the runtime, not of a statement (ST-SUP-RP10-LC44).
	if sealedFault(err) {
		return s.breakWith(err)
	}
	code := sqlState(err)
	if tenant.StalePlan(err) {
		tenant.ResetStatements()
		if s.conflict == nil {
			s.conflict = &conflictError{text: step + " ran a prepared statement that a change of its table made stale", stalePlan: true}
		}
		return s.conflict
	}
	if isConflict(ctx, err) {
		if s.conflict == nil {
			s.conflict = &conflictError{text: fmt.Sprintf("%s ended with SQLSTATE %q or the deadline", step, code), unique: code == "23505"}
		}
		return s.conflict
	}
	if code == notGrantedState {
		s.notGranted = true
	}
	// A data exception (class 22) or an integrity constraint violation (class 23, but for 23505, which is a
	// conflict) comes from the values that the fill writes, so it comes again in each attempt; any other SQLSTATE,
	// such as of a connection (08), of the resources (53), of an operator intervention or a timeout (57) or of a
	// rolling schema change (42), and an error with no SQLSTATE, is a failed statement (decision of the orchestrator
	// on the re-review of PR #411).
	data := strings.HasPrefix(code, "22") || strings.HasPrefix(code, "23")
	// The log names the action, the step, the fixed cause class and the SQLSTATE class, and no value and no database
	// text, so a call that ends with 500 internal is never an error that no log explains (walkthrough 7, B1;
	// ST-SUP-RP10-LC39).
	cause, class := statementCause(err, code)
	s.logged = true
	if ok, held := statementLog.Allow(s.action + "/" + cause + "/" + class); ok {
		slog.ErrorContext(ctx, "command: a statement failed", "action", s.action, "step", step, "cause", cause,
			"sqlstate_class", class, "held_back", held)
	}
	return s.breakWith(&internalError{text: fmt.Sprintf("the statement %s failed with SQLSTATE %q", step, code), statement: !data, data: data,
		unavailable: !data && tenant.Unavailable(err)})
}

// statementLog holds back the log of a failed statement to one line for each action, cause class and SQLSTATE class
// in each authz.LogEvery.
var statementLog = authz.NewCauseLog(nil)

// The fixed cause classes of a failed statement in its log (ST-SUP-RP10-LC39): data for the values of the row
// (SQLSTATE classes 22 and 23), not_granted for a privilege that the role lacks (42501), schema for a statement that
// does not fit the database, such as an undefined column (the rest of class 42), statement for any other SQLSTATE,
// scan for a row whose columns do not fit the generated Scan of the entity, and other for an error with no SQLSTATE,
// such as a lost connection.
const (
	causeData       = "data"
	causeNotGranted = "not_granted"
	causeSchema     = "schema"
	causeStatement  = "statement"
	causeScan       = "scan"
	causeOther      = "other"
)

// statementCause gives the fixed cause class and the SQLSTATE class, the first two characters of code or none, of the
// error err of a statement.
func statementCause(err error, code string) (cause, class string) {
	if len(code) < 2 {
		var scan *scanError
		if errors.As(err, &scan) {
			return causeScan, "none"
		}
		return causeOther, "none"
	}
	class = code[:2]
	if strings.Trim(class, "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
		class = "xx"
	}
	switch {
	case class == "22" || class == "23":
		return causeData, class
	case code == notGrantedState:
		return causeNotGranted, class
	case class == "42":
		return causeSchema, class
	}
	return causeStatement, class
}

// scanError is the error of a row that the generated Scan of an entity could not read, such as a row with another
// number of columns than the Scan reads. It has no SQLSTATE.
type scanError struct{ err error }

func (e *scanError) Error() string { return "command: the row does not fit the scan of the entity" }
func (e *scanError) Unwrap() error { return e.err }

// scanFailed marks err of a Scan of an entity, which is not sql.ErrNoRows, as a scanError when it has no SQLSTATE: an
// error of the statement itself, which a row read gives at its Scan, keeps its SQLSTATE.
func scanFailed(err error) error {
	if err == nil || sqlState(err) != "" || errors.Is(err, sql.ErrNoRows) || sealedFault(err) {
		return err
	}
	return &scanError{err: err}
}

// notGrantedState is the SQLSTATE insufficient_privilege.
const notGrantedState = "42501"

// breakWith records an internal error that a rerun cannot change. The first internal error of a call that is not a
// failed statement, which fail logs, writes one error log line with the action and its fixed text, which names only
// declared names, such as the list that the declaration does not declare, so a 500 internal of a fill that a release
// broke is diagnosable (walkthrough 7, B1; ST-SUP-RP10-LC39).
func (s *state) breakWith(err error) error {
	if s.broken == nil {
		s.broken = err
		var internal *internalError
		if errors.As(err, &internal) && !internal.statement && !internal.data {
			// A sealed value that does not open keeps its class: Run logs it as sealed (ST-SUP-RP10-LC51).
			s.logged = s.logged || !sealedFault(err)
			if ok, held := statementLog.Allow(s.action + "/internal/" + internal.text); ok {
				slog.Error("command: the call has an internal error", "action", s.action, "cause", "internal",
					"reason", internal.text, "held_back", held)
			}
		}
	}
	return s.broken
}

// setLockTimeout sets lock_timeout in the transaction to the smaller of the declared lock timeout and the time left
// before the deadline less 50 ms, and to at least 1 ms (ST-SUP-RP10-LC28).
func (s *state) setLockTimeout(ctx context.Context) error {
	d := s.lockTimeout
	if !s.deadline.IsZero() {
		if left := s.deadline.Sub(s.rt.clock().Now()) - LockMargin; left < d {
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
	if e == nil || e.spec.ReadOnly || !h.st.writes[e] {
		name := "<nil>"
		if e != nil {
			name = e.spec.Name
		}
		return nil, fmt.Errorf("command: %s does not declare that it writes %s", h.st.action, name)
	}
	return &Repository[T]{st: h.st, entity: e}, nil
}

// Reader is the read-only repository of an entity that a worker reads (ST-SUP-RP10-LC53, decision ST-SUP-Q186 (a)):
// it reads with no lock, and has no create, update, delete or locked read. Its value type holds no secret field.
type Reader[T any] struct {
	r *Repository[T]
}

// ReadRepo returns the read-only repository of e in h. It refuses, with an error that names the entity and with no
// statement, an entity that the handle's worker does not declare it reads.
func ReadRepo[T, P any](h *Handle[P], e *Entity[T]) (*Reader[T], error) {
	if h == nil || h.st == nil {
		return nil, ErrClosed
	}
	if err := h.st.enter(); err != nil {
		return nil, err
	}
	defer h.st.leave()
	if e == nil || !e.spec.ReadOnly || !h.st.reads[e] {
		name := "<nil>"
		if e != nil {
			name = e.spec.Name
		}
		return nil, fmt.Errorf("command: %s does not declare that it reads %s", h.st.action, name)
	}
	return &Reader[T]{r: &Repository[T]{st: h.st, entity: e}}, nil
}

// Get reads the row of id with no lock, as Repository.Get does: a row that the tenant does not hold or that the
// worker may not read is not found, and a read of an entity with a pii field is audited.
func (r *Reader[T]) Get(ctx context.Context, id string) (Loaded[T], error) {
	if r == nil || r.r == nil {
		return nil, ErrClosed
	}
	return r.r.Get(ctx, id)
}

// List reads the allowed rows of the declared list l with the filter values args, at most limit of them, with no
// lock, as Repository.List does.
func (r *Reader[T]) List(ctx context.Context, l *List[T], limit int, args ...any) ([]Loaded[T], error) {
	if r == nil || r.r == nil {
		return nil, ErrClosed
	}
	return r.r.List(ctx, l, limit, args...)
}

// Get reads the row of id with no lock. It returns ErrNotFound with no data for a row that the tenant does not hold
// or that the principal may not read (ST-SUP-RP10-LC03, ST-SUP-RP10-LC13), and audits the read of an entity with a
// pii field (ST-SUP-RP11-LC08).
func (r *Repository[T]) Get(ctx context.Context, id string) (Loaded[T], error) {
	if err := r.st.enter(); err != nil {
		return nil, err
	}
	defer r.st.leave()
	return r.read(ctx, id, lockNone, nil)
}

// GetForUpdate reads the row of id, decides the read on it, and only then locks it with FOR NO KEY UPDATE until the
// transaction ends, so a concurrent insert of a child row does not wait (ST-SUP-RP10-LC23). A denied row is not
// locked and is not found. When the row's authorization token changed between the read and the lock, it decides
// again and releases the lock of a row that is now denied.
func (r *Repository[T]) GetForUpdate(ctx context.Context, id string, parent ...Ref) (Loaded[T], error) {
	if err := r.st.enter(); err != nil {
		return nil, err
	}
	defer r.st.leave()
	return r.read(ctx, id, lockUpdate, parent)
}

// GetForDelete is GetForUpdate with FOR UPDATE, for a fill that deletes the row or changes a column of one of its
// keys (ST-SUP-RP10-LC23, ST-SUP-RP10-LC24).
func (r *Repository[T]) GetForDelete(ctx context.Context, id string, parent ...Ref) (Loaded[T], error) {
	if err := r.st.enter(); err != nil {
		return nil, err
	}
	defer r.st.leave()
	return r.read(ctx, id, lockDelete, parent)
}

func (r *Repository[T]) read(ctx context.Context, id string, lock lockMode, parents []Ref) (Loaded[T], error) {
	s, e := r.st, r.entity
	var value T
	var meta Meta
	err := sql.ErrNoRows
	if e.spec.ValidID(id) {
		st := e.statements()
		value, meta, err = st.scan(s.opening(s.queryRow(ctx, st.selectRow, s.tenant.ID(), id)))
		err = scanFailed(err)
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
		return nil, s.failOf(ctx, e, "read "+e.spec.Name, err)
	}
	if err := s.decideRead(ctx, e.spec.ResourceType, e.spec.ReadPermission, e.spec.Name, meta); err != nil {
		return nil, err
	}
	if lock != lockNone {
		if err := s.decideCommandOn(ctx, e.spec.ResourceType, e.spec.Name, meta.ID, meta.Token); err != nil {
			return nil, err
		}
		if err := r.decideParents(ctx, "locked read", parents, value); err != nil {
			return nil, err
		}
		statement := e.statements().forUpdate
		if lock == lockDelete {
			statement = e.statements().forDelete
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
	value, meta, err := e.statements().scan(s.opening(s.queryRow(ctx, statement, s.tenant.ID(), id)))
	err = scanFailed(err)
	if errors.Is(err, sql.ErrNoRows) {
		if err := s.release(ctx, step); err != nil {
			return zero, Meta{}, err
		}
		return zero, Meta{}, fmt.Errorf("%w: %s", ErrNotFound, e.spec.Name)
	}
	if err != nil {
		return zero, Meta{}, s.failOf(ctx, e, step, err)
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
	args, bound, empty, err := r.listArgs(ctx, l, args)
	if err != nil || empty {
		return nil, err
	}
	// A child entity has no row decision of its own: its list reads only the rows of its decided parent
	// (ST-SUP-RP10-LC38, security review of PR #416, M1).
	if r.isChild() && bound == nil {
		return nil, r.st.breakWith(internalf("the list %s of the child entity %s takes its parent row, which decides it", l.spec.Name, r.entity.spec.Name))
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
func (r *Repository[T]) ListForUpdate(ctx context.Context, l *List[T], parent Ref, args ...any) ([]Loaded[T], error) {
	if err := r.st.enter(); err != nil {
		return nil, err
	}
	defer r.st.leave()
	s, e := r.st, r.entity
	if l == nil || l.entity != e || l.spec.Lock == "" || e.spec.LockAtMost == 0 {
		return nil, fmt.Errorf("command: %s has no locked list of that name: the entity declares no Lock at most", e.spec.Name)
	}
	args, bound, empty, err := r.listArgs(ctx, l, args)
	if err != nil || empty {
		return nil, err
	}
	if bound != nil {
		// The parent of the list is the parent of the call; a fill that gives another one breaks the call.
		if parent != nil && refOf(parent) != refOf(bound) {
			return nil, s.breakWith(internalf("the locked list %s of %s takes the parent of its binding", l.spec.Name, e.spec.Name))
		}
		parent = bound
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
	// A child entity takes its authorization from its parent: every row of the list must name parent, and the
	// call decides on parent once (ST-SUP-RP10-LC05).
	var parents []Ref
	if parent != nil {
		parents = []Ref{parent}
	}
	for _, row := range allowed {
		if err := r.decideParents(ctx, "locked list", parents, row.value); err != nil {
			return nil, err
		}
	}
	if len(allowed) == 0 && parent != nil && !r.isChild() {
		return nil, s.breakWith(internalf("a locked list of %s takes no parent", e.spec.Name))
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
		locked, err := r.scanAll(ctx, step, l.lockStatement(), append([]any{s.tenant.ID(), ids}, args...))
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

// listArgs gives the filter values of the list l: its declared Values in their order, with the time of the run in
// place of each RunTime value, the bound parameters of With in place of each Param value and the ID of the bound
// parent row in place of a Parent value. Every handle refuses, as an internal error, a list that its CommandSpec.Lists
// or WorkerSpec.Lists does not declare, filter values from the fill, a list with parameters that With did not bind, a
// binding that does not fit and a parent row that this handle did not read (ST-SUP-RP10-LC38). It decides the
// permission of the reference on the parent row before any row is read, so a caller that may act on one parent never
// lists the rows of another; a deny is not found. empty is true when a key parameter fails the key check of the
// entity: the list then has no row and runs no statement. bound is the parent row, or nil.
func (r *Repository[T]) listArgs(ctx context.Context, l *List[T], args []any) (values []any, bound Ref, empty bool, err error) {
	s := r.st
	if held, ok := s.lists[AnyList(l.root())]; !ok || !held.same(l.root().listCopy()) {
		kind, place := "command", "command contract"
		if s.mode == modeWorker || s.mode == modeConsumer {
			kind, place = "worker", "worker contract"
		}
		return nil, nil, false, s.breakWith(internalf("the %s %s reads the list %s of %s, which its declaration does not declare: since runtime "+
			"v0.127.0 a fill gives no SQL text, so declare the list under lists: of the %s and use the generated "+
			"list of the %s (ST-SUP-RP10-LC38)", kind, s.action, l.spec.Name, r.entity.spec.Name, kind, place))
	}
	// A declared list takes no filter values from the fill, also when its filter has no value, such as a filter of
	// is_set terms only (security review of PR #411, L3): a value that the fill gives at call time is a declared
	// parameter, which With binds.
	if len(args) > 0 {
		return nil, nil, false, s.breakWith(internalf("the list %s of %s declares its filter values, so the fill gives none; a declared "+
			"parameter goes through the generated function of the list", l.spec.Name, r.entity.spec.Name))
	}
	declared := l.spec.Values.all()
	params := 0
	for _, v := range declared {
		if v.Param != "" || v.Parent != "" {
			params++
		}
	}
	switch {
	case params > 0 && l.binding == nil:
		return nil, nil, false, s.breakWith(internalf("the list %s of %s takes %d parameters: give them through the generated function "+
			"of the list, which binds them", l.spec.Name, r.entity.spec.Name, params))
	case l.binding != nil && l.binding.bad != "":
		return nil, nil, false, s.breakWith(internalf("the binding of the list %s of %s does not fit: %s", l.spec.Name, r.entity.spec.Name, l.binding.bad))
	}
	values = make([]any, len(declared))
	next := 0
	for i, v := range declared {
		switch {
		case v.RunTime:
			if s.runTime.IsZero() {
				return nil, nil, false, s.breakWith(internalf("the list %s of %s takes the time of the run, which this call does not have", l.spec.Name, r.entity.spec.Name))
			}
			values[i] = s.runTime
		case v.Parent != "":
			parent := l.binding.params[next].(Ref)
			next++
			if err := r.decideListParent(ctx, l, v.Parent, parent); err != nil {
				return nil, nil, false, err
			}
			bound, values[i] = parent, refOf(parent).id
		case v.Param != "":
			values[i] = l.binding.params[next]
			next++
			if v.Key && !r.validKey(v.KeyOf, values[i]) {
				empty = true
			}
		default:
			values[i] = v.Constant
		}
	}
	return values, bound, empty, nil
}

// decideListParent decides the permission of the reference of a child entity on its parent row p, a loaded row of
// the entity parent that this handle read, as a write of the child decides it (ST-SUP-RP10-LC05, ST-SUP-RP10-LC38).
func (r *Repository[T]) decideListParent(ctx context.Context, l *List[T], parent string, p Ref) error {
	s, e := r.st, r.entity
	pr := refOf(p)
	if pr.st != s || pr.entity != parent {
		return s.breakWith(internalf("the list %s of %s takes a loaded row of %s that this handle read", l.spec.Name, e.spec.Name, parent))
	}
	for _, ref := range e.spec.Parent {
		if ref.Entity == parent {
			return s.decideReference(ctx, reference{entity: ref.Entity, resourceType: ref.ResourceType, id: pr.id,
				permission: ref.Permission, relation: ref.Relation}, pr)
		}
	}
	return s.breakWith(internalf("the list %s of %s takes a parent row of %s, which the entity does not refer to", l.spec.Name, e.spec.Name, parent))
}

// validKey checks a key parameter with the key check of the kind of key of the entity whose key it holds, or else
// the key check of the listed entity: a string, or a pointer to one that is set.
func (r *Repository[T]) validKey(kind KeyKind, value any) bool {
	valid := r.entity.spec.ValidID
	if kind == KeyUUID {
		valid = ValidUUID
	}
	if valid == nil {
		return true
	}
	switch v := value.(type) {
	case string:
		return valid(v)
	case *string:
		return v == nil || valid(*v)
	}
	return false
}

// scanned is one row that a list read.
type scanned[T any] struct {
	value T
	meta  Meta
}

func (r *Repository[T]) readList(ctx context.Context, l *List[T], limit int, args []any) ([]scanned[T], error) {
	return r.scanAll(ctx, "read the list "+l.spec.Name, l.readStatement(), append([]any{r.st.tenant.ID(), limit}, args...))
}

func (r *Repository[T]) scanAll(ctx context.Context, step, statement string, args []any) ([]scanned[T], error) {
	s := r.st
	rows, err := s.query(ctx, statement, args...)
	if err != nil {
		return nil, s.failOf(ctx, r.entity, step, err)
	}
	defer rows.Close()
	var out []scanned[T]
	scan := r.entity.statements().scan
	for rows.Next() {
		value, meta, err := scan(s.opening(rows))
		if err != nil {
			return nil, s.failOf(ctx, r.entity, step, scanFailed(err))
		}
		out = append(out, scanned[T]{value: value, meta: meta})
	}
	if err := rows.Err(); err != nil {
		return nil, s.failOf(ctx, r.entity, step, err)
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

// Create inserts value and returns it as stored, with the version the insert gave it. The parent must be the
// loaded value of this handle that the entity's Parent reads from value, the same entity and the same ID, and a
// command must give it whenever the entity has a parent; an entity with no parent takes none. A command's create
// decides the command's permission on the parent, and one with no parent is covered by the decision before the fill
// (ST-SUP-RP10-LC05); a worker's create decides the worker's permission on parent, the loaded
// parent resource that the new row goes under, before the write (ST-SUP-RP10-LC17). The audit row of the write is
// buffered for Run (ST-SUP-RP10-LC06).
func (r *Repository[T]) Create(ctx context.Context, value T, parents ...Ref) (Loaded[T], error) {
	if err := r.st.enter(); err != nil {
		return nil, err
	}
	defer r.st.leave()
	s, e := r.st, r.entity
	if err := r.checkFields(value); err != nil {
		return nil, err
	}
	parents = nonNil(parents)
	refs := e.references(value)
	rows, err := r.checkCreate(refs, parents)
	if err != nil {
		return nil, err
	}
	if s.mode != modeCommand && e.spec.ResourceType != "" && len(rows) == 0 {
		// A value that sets a reference to an entity with a resource type has a parent, which must be given and
		// decided: a create with no loaded parent for it is refused, so no row is written under a parent that no
		// one decided on (ST-SUP-RP10-LC17; security review of PR #434, M1).
		for _, ref := range refs {
			if ref.resourceType != "" {
				if ok, held := statementLog.Allow(s.action + "/create_without_parent/" + e.spec.Name); ok {
					slog.WarnContext(ctx, "command: a worker's create sets a reference and gives no parent to decide on", "action", s.action,
						"entity", e.spec.Name, "cause", "create_without_parent", "held_back", held)
				}
				return nil, &noParentError{entity: e.spec.Name}
			}
		}
		// A worker's create with no parent is decided on the tenant resource of the run, with the worker's permission
		// and, in a consumer, a second time with the staging principal (ST-SUP-RP10-LC17, ST-SUP-Q164 (b)).
		if err := s.decideWrite(ctx, authz.TenantType, e.spec.Name, s.tenant.ID(), ""); err != nil {
			return nil, err
		}
	}
	// A create decides on each row that the new row refers to, and an optional reference that is not set is not
	// decided (ST-SUP-RP10-LC05, ST-SUP-RP10-LC17).
	for i, p := range rows {
		if err := s.decideReference(ctx, refs[i], p); err != nil {
			return nil, err
		}
	}
	// A reference held within a field is compared with its loaded referenced row before the row statement
	// (ST-SUP-RP10-LC37). A worker's create that gives no parent is held by the composite key alone.
	for i, p := range rows {
		if err := r.checkWithin(value, refs[i], p); err != nil {
			return nil, err
		}
	}
	// Every declared tuple is built and checked before the row statement (ST-SUP-Q118).
	tuples, err := r.subjects(value, true)
	if err != nil {
		return nil, err
	}
	st := e.statements()
	values, err := s.sealArgs(ctx, st.values(value))
	if err != nil {
		return nil, s.breakWith(err)
	}
	row, err := s.lockedRow(ctx, "create "+e.spec.Name, st.insert, append([]any{s.tenant.ID()}, values...)...)
	if err != nil {
		return nil, err
	}
	stored, meta, err := st.scan(s.opening(row))
	if err != nil {
		return nil, r.failWrite(ctx, "create "+e.spec.Name, scanFailed(err))
	}
	if len(rows) > 0 {
		if err := r.relate(ctx, meta.ID, refs, nil); err != nil {
			return nil, err
		}
	}
	if err := r.relateDeclared(ctx, meta.ID, tuples, (*authz.Handle).Relate); err != nil {
		return nil, err
	}
	if err := s.buffer(e.auditType(), e.spec.Fields, meta.ID, nil, e.spec.State(stored)); err != nil {
		return nil, err
	}
	if err := s.stageChange(e.spec.Name, e.spec.ResourceType, meta.ID, ActionCreated, e.eventData(stored)); err != nil {
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
func (r *Repository[T]) Update(ctx context.Context, from Loaded[T], value T, parents ...Ref) (Loaded[T], error) {
	if err := r.st.enter(); err != nil {
		return nil, err
	}
	defer r.st.leave()
	s, e := r.st, r.entity
	from2, err := r.check(from, "update")
	if err != nil {
		return nil, err
	}
	if err := r.checkFields(value); err != nil {
		return nil, err
	}
	if e.spec.KeyChanged(from2.value, value) && from2.lock != lockDelete {
		return nil, s.breakWith(internalf("an update of a key column of %s needs a value that GetForDelete locked", e.spec.Name))
	}
	if err := s.decideWrite(ctx, e.spec.ResourceType, e.spec.Name, from2.meta.ID, from2.meta.Token); err != nil {
		return nil, err
	}
	// A child decides on its parent, and an update that moves a reference on the new referenced row too.
	moves, err := r.decideMove(ctx, parents, from2.value, value)
	if err != nil {
		return nil, err
	}
	// The subject of a field-sourced relationship is set at create and does not change (ST-SUP-Q118).
	for _, rel := range e.spec.Relationships {
		if rel.Field != nil && rel.Field(from2.value) != rel.Field(value) {
			return nil, s.breakWith(&invalidError{field: rel.FieldName, reason: reasonCreateOnly})
		}
	}
	step := "update " + e.spec.Name
	st := e.statements()
	values, err := s.sealArgs(ctx, st.values(value))
	if err != nil {
		return nil, s.breakWith(err)
	}
	row, err := s.lockedRow(ctx, step, st.update,
		append([]any{s.tenant.ID(), from2.meta.ID, from2.meta.Version}, values...)...)
	if err != nil {
		return nil, err
	}
	stored, meta, err := st.scan(s.opening(row))
	err = scanFailed(err)
	if errors.Is(err, sql.ErrNoRows) {
		s.conflict = &conflictError{text: "the version of a row of " + e.spec.Name + " moved"}
		return nil, s.conflict
	}
	if err != nil {
		return nil, r.failWrite(ctx, step, err)
	}
	// A moved reference moves its relation: the old tuple goes and the new one is written.
	var touch, drop []reference
	for _, m := range moves {
		if m.oldID != "" {
			drop = append(drop, reference{entity: m.entity, resourceType: m.resourceType, id: m.oldID, relation: m.relation})
		}
		if m.newID != "" {
			touch = append(touch, reference{entity: m.entity, resourceType: m.resourceType, id: m.newID, relation: m.relation})
		}
	}
	if err := r.relate(ctx, meta.ID, touch, drop); err != nil {
		return nil, err
	}
	if err := s.buffer(e.auditType(), e.spec.Fields, meta.ID, e.spec.State(from2.value), e.spec.State(stored)); err != nil {
		return nil, err
	}
	if err := s.stageChange(e.spec.Name, e.spec.ResourceType, meta.ID, ActionUpdated, e.eventData(stored)); err != nil {
		return nil, err
	}
	s.written[rowKey{e.spec.Name, meta.ID}] = meta.Version
	return newLoaded(stored, meta, e, s, max(from2.lock, lockUpdate)), nil
}

// Delete removes the row of from, a loaded value of this handle, only while the row still has from's version, as
// Update states (ST-SUP-RP10-LC24).
func (r *Repository[T]) Delete(ctx context.Context, from Loaded[T], parent ...Ref) error {
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
	if err := r.decideParents(ctx, "delete", parent, from2.value); err != nil {
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
	if err := r.unrelateAll(ctx, from2.meta.ID); err != nil {
		return err
	}
	if err := s.buffer(e.auditType(), e.spec.Fields, from2.meta.ID, e.spec.State(from2.value), nil); err != nil {
		return err
	}
	if err := s.stageChange(e.spec.Name, e.spec.ResourceType, from2.meta.ID, ActionDeleted, e.eventData(from2.value)); err != nil {
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
