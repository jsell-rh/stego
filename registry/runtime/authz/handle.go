package authz

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/jsell-rh/stego/registry/runtime/internal/seal"
	"github.com/jsell-rh/stego/registry/runtime/manifest"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// Clock is the clock of the revocation retention. A lease.Clock satisfies it.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// MinRetention is the shortest retention of a revocation record (ST-SUP-RP06-LC18); it is also the default.
const MinRetention = 30 * 24 * time.Hour

// Config is what the authorization runtime of a service needs.
type Config struct {
	// Provider is the decision provider, such as the SpiceDB provider. The handle never exposes it.
	Provider Decision
	// Floor gives the revocation floor to Port, for a caller with no transaction of its own. Nil is the TableFloor
	// of DB and Schema, so Port never checks below the floor. A handle reads the floor through its own transaction.
	Floor Floor
	// DB is the database of the revocation records, which Status reads in a transaction of its own.
	DB *sql.DB
	// Schema is the schema of the tables that Migration creates.
	Schema string
	// Outbox is the outbox queue of the service, as the runtime role, in which the handle enqueues the relay's
	// tuple writes.
	Outbox *outbox.Queue
	// Poll is the time between two reads of Wait. Zero is 50 milliseconds.
	Poll time.Duration
	// Retention is how long Status answers for a revocation; after it, Status answers expired. Zero is
	// MinRetention, and a shorter one is refused.
	Retention time.Duration
	// Clock is the clock of the retention. Nil is the system clock.
	Clock Clock
	// Meter gives the metric of the search results that a handle drops because they name another tenant
	// (ST-SUP-RP06-LC22). Nil records no metric; DroppedResults counts them still.
	Meter metric.MeterProvider
}

// Runtime is the authorization runtime of one service. It binds handles and reads revocation statuses.
type Runtime struct {
	port      *Port
	db        *sql.DB
	schema    string
	outbox    *outbox.Queue
	poll      time.Duration
	retention time.Duration
	clock     Clock

	droppedMetric metric.Int64Counter
	dropped       atomic.Int64
}

// New returns the authorization runtime of cfg. It refuses a configuration that lacks a part.
func New(cfg Config) (*Runtime, error) {
	if cfg.DB == nil {
		return nil, errors.New("authz: no database")
	}
	floor := cfg.Floor
	if floor == nil {
		table, err := NewTableFloor(cfg.DB, cfg.Schema)
		if err != nil {
			return nil, err
		}
		floor = table
	}
	port, err := NewPort(cfg.Provider, floor)
	if err != nil {
		return nil, err
	}
	switch {
	case cfg.Outbox == nil:
		return nil, errors.New("authz: no outbox queue")
	case cfg.Poll < 0:
		return nil, errors.New("authz: the poll time is negative")
	case cfg.Retention != 0 && cfg.Retention < MinRetention:
		return nil, errors.New("authz: the revocation retention is shorter than 30 days")
	}
	schema, err := quoteName("schema", cfg.Schema)
	if err != nil {
		return nil, err
	}
	r := &Runtime{port: port, db: cfg.DB, schema: schema, outbox: cfg.Outbox, poll: cfg.Poll,
		retention: cfg.Retention, clock: cfg.Clock}
	if r.poll == 0 {
		r.poll = 50 * time.Millisecond
	}
	if r.retention == 0 {
		r.retention = MinRetention
	}
	if r.clock == nil {
		r.clock = systemClock{}
	}
	if cfg.Meter != nil {
		r.droppedMetric, err = cfg.Meter.Meter(instrumentation).Int64Counter("authz.search.dropped",
			metric.WithDescription("The resource search results that name another tenant or are not tenant-qualified, which a handle drops."))
		if err != nil {
			return nil, err
		}
	}
	return r, nil
}

// DroppedResults returns the number of resource search results that the handles of the runtime dropped, because
// their ID is not tenant-qualified or names another tenant than the handle's (ST-SUP-RP06-LC22).
func (r *Runtime) DroppedResults() int64 { return r.dropped.Load() }

// Port returns the runtime's decision entry point, for a worker that decides with no transaction of its own, such
// as the outbox worker's Resolve (ST-SUP-RP01-LC17). It holds no relationships port.
func (r *Runtime) Port() *Port { return r.port }

// Decide is the decision port of the outbox worker's Resolve (ST-SUP-RP01-LC17): may the subject of request, written
// type:id, do the permission on the resource, written type:id, which the outbox names by its key, such as
// outbox_destination:billing. The resource is sent to the provider in the tenant-qualified form of the request's
// tenant, the tenant of the parked message (ST-SUP-RP06-LC22), so a resolve grant in one tenant serves no other. It
// decides at the floor (ST-SUP-RP06-LC09), which it reads through the Floor of the configuration, as a worker with no
// transaction of its own does. A deny is false with no error.
func (r *Runtime) Decide(ctx context.Context, request outbox.DecisionRequest) (bool, error) {
	subjectType, subjectID, ok := strings.Cut(request.Subject, ":")
	if !ok {
		return false, errors.New("authz: the subject of the resolution is not type:id")
	}
	resourceType, key, ok := strings.Cut(request.Resource, ":")
	if !ok {
		return false, errors.New("authz: the resource of the resolution is not type:id")
	}
	resource, err := NewResource(request.Tenant, resourceType, key)
	if err != nil {
		return false, err
	}
	floor, err := r.port.currentFloor(ctx)
	if err != nil {
		return false, err
	}
	result, err := r.port.evaluate(ctx, EvaluationRequest{
		Subject:     Subject{Type: subjectType, ID: subjectID},
		Action:      Action{Name: request.Permission},
		Resource:    resource,
		Consistency: atFloor(floor, Token{}),
	}, Token{})
	if err != nil {
		return false, err
	}
	return result.Allowed, nil
}

// Relation is one relation of a resource type that a handle may write, written resource_type#relation.
type Relation struct {
	ResourceType string
	Name         string
}

// String returns the relation as resource_type#relation.
func (r Relation) String() string { return r.ResourceType + "#" + r.Name }

// ParseRelation reads resource_type#relation.
func ParseRelation(text string) (Relation, error) {
	resourceType, name, ok := strings.Cut(text, "#")
	if !ok {
		return Relation{}, errors.New("authz: the relation is not resource_type#relation")
	}
	if err := validType("resource", resourceType); err != nil {
		return Relation{}, err
	}
	if err := validType("relation", name); err != nil {
		return Relation{}, err
	}
	return Relation{ResourceType: resourceType, Name: name}, nil
}

// Handle is the authorization handle of one transaction, tenant and principal, which a slot fill gets
// (ST-SUP-RP06-LC09, ST-SUP-RP06-LC10). Its evaluations decide with the principal as subject at the floor, which it
// reads through its own transaction. Its Relate and Unrelate only enqueue tuples for the relay in the transaction's
// outbox, for the relations it was bound with. Every resource it takes has the ID of ResourceID in its own tenant,
// and it refuses any other before the provider or the outbox sees it; its Search keeps only the results of its own
// tenant (ST-SUP-RP06-LC22). It exposes no provider, no relationships port and no field.
type Handle struct {
	runtime   *Runtime
	tx        *sql.Tx
	tenant    tenant.Tenant
	principal Principal
	allowed   map[Relation]bool
}

// Bind returns the handle of tx, the open transaction of tenant t, for principal p. allowed is the set of relations
// that the generated code declares the fill writes; Relate and Unrelate refuse every other relation.
func (r *Runtime) Bind(tx *sql.Tx, t tenant.Tenant, p Principal, allowed []Relation) (*Handle, error) {
	switch {
	case tx == nil:
		return nil, errors.New("authz: no transaction")
	case t.IsZero():
		return nil, tenant.ErrZero
	case p.IsZero():
		return nil, errors.New("authz: no principal")
	}
	set := make(map[Relation]bool, len(allowed))
	for _, relation := range allowed {
		if _, err := ParseRelation(relation.String()); err != nil {
			return nil, err
		}
		set[relation] = true
	}
	return &Handle{runtime: r, tx: tx, tenant: t, principal: p, allowed: set}, nil
}

// Principal returns the principal of the handle.
func (h *Handle) Principal() Principal { return h.principal }

// key gives the row key of resource, whose ID must be the tenant-qualified form of the handle's tenant
// (ST-SUP-RP06-LC22).
func (h *Handle) key(resource Resource) (string, error) {
	if err := resource.valid(); err != nil {
		return "", err
	}
	return keyIn(h.tenant, resource.ID)
}

// floor reads the revocation floor through the handle's own transaction, never through a second connection of the
// pool (ST-SUP-RP06-LC09).
func (h *Handle) floor(ctx context.Context) (Token, error) {
	var text string
	if err := h.tx.QueryRowContext(ctx, "SELECT token FROM "+h.runtime.schema+".authz_floor WHERE only_row").Scan(&text); err != nil {
		return Token{}, fixed("authz: the revocation floor cannot be read in the transaction", err)
	}
	return ParseToken(text)
}

// Evaluate decides action on resource, whose row holds rowToken, with the principal as subject, at least as fresh
// as the revocation floor; only while the floor is zero, at least as fresh as the row token; and fresh when the row
// has no token either (ST-SUP-RP06-LC09). A deny is a result with Allowed false and a nil error.
func (h *Handle) Evaluate(ctx context.Context, action string, resource Resource, rowToken Token, c Context) (EvaluationResult, error) {
	if _, err := h.key(resource); err != nil {
		return EvaluationResult{}, err
	}
	floor, err := h.floor(ctx)
	if err != nil {
		return EvaluationResult{}, err
	}
	return h.runtime.port.evaluate(ctx, EvaluationRequest{
		Subject:     h.principal.subject,
		Action:      Action{Name: action},
		Resource:    resource,
		Context:     c,
		Consistency: atFloor(floor, rowToken),
	}, Token{})
}

// Row is a resource that a list reads with the token of its row.
type Row struct {
	Resource Resource
	Token    Token
}

// EvaluateBatch decides action on each row in one batch evaluation, at least as fresh as the floor, or fresh while
// the floor is zero, and returns one result per row.
func (h *Handle) EvaluateBatch(ctx context.Context, action string, rows []Row, c Context) ([]EvaluationResult, error) {
	for _, row := range rows {
		if _, err := h.key(row.Resource); err != nil {
			return nil, err
		}
	}
	floor, err := h.floor(ctx)
	if err != nil {
		return nil, err
	}
	return h.batch(ctx, action, rows, c, atFloor(floor, Token{}))
}

func (h *Handle) batch(ctx context.Context, action string, rows []Row, c Context, consistency Consistency) ([]EvaluationResult, error) {
	evaluations := make([]EvaluationRequest, len(rows))
	for i, row := range rows {
		evaluations[i] = EvaluationRequest{Subject: h.principal.subject, Action: Action{Name: action},
			Resource: row.Resource, Context: c, Consistency: consistency}
	}
	results, err := h.runtime.port.evaluateBatch(ctx, BatchRequest{Evaluations: evaluations, Semantic: ExecuteAll}, Token{})
	if err != nil {
		return nil, err
	}
	if len(results) != len(rows) {
		return nil, errors.New("authz: the provider did not answer every evaluation of the batch")
	}
	return results, nil
}

// maxSearchCalls is the most provider pages that one Handle.Search reads to fill its page.
const maxSearchCalls = 50

// ErrSearchExhausted is the error of Handle.Search when maxSearchCalls provider pages give no result of the handle's
// tenant before the end of the search: an empty page is never returned before the end.
var ErrSearchExhausted = errors.New("authz: the search read its most provider pages and found no resource of the tenant")

// Search returns one page of the resources of resourceType that the principal may do action on, at least as fresh
// as the revocation floor at this page, or ErrNoSearch when the provider does not search. A later page keeps the
// first page's consistency in its page token, and the provider filters its results at this page's floor
// (ST-SUP-RP06-LC05). The page maps each result back to the key of its row, in Keys, only when the result is of
// resourceType and its ID is the tenant-qualified form of the handle's tenant; it drops and counts every other result
// (ST-SUP-RP06-LC22). Because a drop can empty a provider page, Search reads further provider pages until it has
// page.Limit results, or one result when the limit is zero, or the search ends, at most maxSearchCalls pages; so it
// never returns an empty page before the end, and returns ErrSearchExhausted when the bound is reached with no
// result.
func (h *Handle) Search(ctx context.Context, action, resourceType string, c Context, page Page) (SearchResult, error) {
	floor, err := h.floor(ctx)
	if err != nil {
		return SearchResult{}, err
	}
	request := SearchRequest{Subject: h.principal.subject, Action: Action{Name: action}, ResourceType: resourceType,
		Context: c, Consistency: atFloor(floor, Token{}), Page: page}
	var filled SearchResult
	for range maxSearchCalls {
		if page.Limit > 0 {
			request.Page.Limit = page.Limit - len(filled.Resources)
		}
		result, err := h.runtime.port.search(ctx, request, Token{})
		if err != nil {
			return SearchResult{}, err
		}
		mapped := h.runtime.mapSearch(ctx, h.tenant, resourceType, result)
		filled.Resources = append(filled.Resources, mapped.Resources...)
		filled.Keys = append(filled.Keys, mapped.Keys...)
		filled.Next, filled.Token = mapped.Next, mapped.Token
		full := len(filled.Resources) > 0 && (page.Limit == 0 || len(filled.Resources) >= page.Limit)
		if full || filled.Next.IsZero() {
			return filled, nil
		}
		request.Page.Token = filled.Next
	}
	if len(filled.Resources) == 0 {
		return SearchResult{}, ErrSearchExhausted
	}
	return filled, nil
}

// mapSearch keeps the results of one search page that map back to a row of tenant t: a resource of resourceType
// whose ID is the tenant-qualified form of t, with its key in Keys. It drops every other result, one of another
// tenant or one that is not tenant-qualified, and counts it in DroppedResults and the metric authz.search.dropped
// (ST-SUP-RP06-LC22).
func (r *Runtime) mapSearch(ctx context.Context, t tenant.Tenant, resourceType string, result SearchResult) SearchResult {
	mapped := SearchResult{Next: result.Next, Token: result.Token}
	var dropped int64
	for _, resource := range result.Resources {
		key, err := keyIn(t, resource.ID)
		if err != nil || resource.Type != resourceType {
			dropped++
			continue
		}
		mapped.Resources = append(mapped.Resources, resource)
		mapped.Keys = append(mapped.Keys, key)
	}
	if dropped > 0 {
		r.dropped.Add(dropped)
		if r.droppedMetric != nil {
			r.droppedMetric.Add(ctx, dropped, metric.WithAttributes(attribute.String("resource_type", resourceType)))
		}
	}
	return mapped
}

// List is a list of rows that a query pages through the database and filters by batch evaluation
// (ST-SUP-RP06-LC04). Each page runs at least as fresh as the floor read at the time of that page, so no later page
// is checked against an older state than the newest delivered revocation. A list uses batch evaluation whether or
// not the provider searches.
type List struct {
	handle  *Handle
	action  string
	context Context
}

// List starts a list of rows on which the principal may do action.
func (h *Handle) List(action string, c Context) *List {
	return &List{handle: h, action: action, context: c}
}

// Page filters one page of rows and returns the allowed rows in their order.
func (l *List) Page(ctx context.Context, rows []Row) ([]Row, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	results, err := l.handle.EvaluateBatch(ctx, l.action, rows, l.context)
	if err != nil {
		return nil, err
	}
	var allowed []Row
	for i, result := range results {
		if result.Allowed {
			allowed = append(allowed, rows[i])
		}
	}
	return allowed, nil
}

// tenantDecision is the decision that tenant.Requested takes. Only DecideTenant makes one, after an allow, with the
// mark of the internal seal constructor.
type tenantDecision struct {
	seal.DecisionMark
}

// DecideTenant decides the permission of action, provision or retire, on resource at the floor, for a request that
// names the tenant tenantID (ST-SUP-RP06-LC17). Like every evaluation of the handle, resource has the ID of
// ResourceID in the handle's own tenant, the tenant of the principal who asks (ST-SUP-RP06-LC22). On an allow it returns the decision that tenant.Requested takes,
// whose mark the internal seal constructor makes with exactly that tenant ID and that action, so the allow serves
// only the one action on the one tenant, and true; on a deny it returns no decision and false.
func (h *Handle) DecideTenant(ctx context.Context, action tenant.RequestAction, tenantID string, resource Resource, c Context) (tenant.Decision, bool, error) {
	if action != tenant.ProvisionAction && action != tenant.RetireAction {
		return nil, false, errors.New("authz: the tenant action is not provision or retire")
	}
	if tenantID == "" {
		return nil, false, errors.New("authz: the request names no tenant")
	}
	result, err := h.Evaluate(ctx, string(action), resource, Token{}, c)
	if err != nil {
		return nil, false, err
	}
	if !result.Allowed {
		return nil, false, nil
	}
	return tenantDecision{DecisionMark: seal.NewDecisionMark(tenantID, string(action))}, true, nil
}

// Revocation names the one revocation of a transaction: every tuple that the transaction enqueues joins it, touch
// or delete, through any handle bound to the transaction (ST-SUP-RP06-LC12). Its identity is known before the
// commit.
type Revocation struct {
	id uuid.UUID
}

// ID returns the identity of the revocation.
func (r Revocation) ID() uuid.UUID { return r.id }

// RevocationFromID returns the revocation of id, as the revocation status route reads it from its path.
func RevocationFromID(id uuid.UUID) Revocation { return Revocation{id: id} }

// Operation is the write of a tuple at the provider.
type Operation string

const (
	// OperationTouch writes a tuple.
	OperationTouch Operation = "touch"
	// OperationDelete removes a tuple.
	OperationDelete Operation = "delete"
)

// payloadVersion is the version of the relay payload. Version 2 holds tenant-qualified resource IDs
// (ST-SUP-RP06-LC22); the relay upgrades version 1, whose IDs are bare keys, with the outbox row's tenant.
const payloadVersion = 2

// payload is the outbox payload of one tuple write for the relay.
type payload struct {
	Version    int       `json:"v"`
	Operation  Operation `json:"op"`
	Revocation uuid.UUID `json:"revocation"`
	Tuple      tupleJSON `json:"tuple"`
}

type tupleJSON struct {
	ResourceType    string `json:"resource_type"`
	ResourceID      string `json:"resource_id"`
	Relation        string `json:"relation"`
	SubjectType     string `json:"subject_type"`
	SubjectID       string `json:"subject_id"`
	SubjectRelation string `json:"subject_relation,omitempty"`
}

func (t tupleJSON) tuple() Tuple {
	return Tuple{Resource: Resource{Type: t.ResourceType, ID: t.ResourceID}, Relation: t.Relation,
		Subject: Subject{Type: t.SubjectType, ID: t.SubjectID}, SubjectRelation: t.SubjectRelation}
}

// tupleKey gives the row key of the tuple's resource, whose ID must be the tenant-qualified form of t
// (ST-SUP-RP06-LC22). The subject must be of t too when it is a resource: a subject set, a subject with a relation
// such as group#member, must be the tenant-qualified form of t, and a direct subject whose ID has the shape of a
// resource ID, such as the folder of an arrow parent, must be of t. A person's subject ID holds a | and a service
// account's name holds no /, so neither has that shape.
func tupleKey(t tenant.Tenant, tuple Tuple) (string, error) {
	key, err := keyIn(t, tuple.Resource.ID)
	if err != nil {
		return "", err
	}
	if err := subjectIn(t, tuple.Subject, tuple.SubjectRelation); err != nil {
		return "", err
	}
	return key, nil
}

// subjectIn checks that a subject that is a resource is of tenant t: a subject set always, a direct subject when
// its ID has the shape of a tenant-qualified resource ID.
func subjectIn(t tenant.Tenant, subject Subject, relation string) error {
	if _, _, shaped := splitResourceID(subject.ID); relation == "" && !shaped {
		return nil
	}
	_, err := keyIn(t, subject.ID)
	return err
}

// resourceKey is the outbox resource key of a tuple: its resource type and the key of its row, type:key. The outbox
// row holds the tenant, so the key need not.
func resourceKey(resourceType, key string) string { return resourceType + ":" + key }

// Relate enqueues a touch of tuple for the relay in the transaction's outbox and returns the transaction's
// revocation. It does not call the provider. It refuses a relation the handle was not bound with before it writes
// anything (ST-SUP-RP06-LC10).
func (h *Handle) Relate(ctx context.Context, tuple Tuple) (Revocation, error) {
	return h.write(ctx, OperationTouch, tuple)
}

// Unrelate enqueues a delete of tuple for the relay in the transaction's outbox and returns the transaction's
// revocation. It does not call the provider. It refuses a relation the handle was not bound with before it writes
// anything.
func (h *Handle) Unrelate(ctx context.Context, tuple Tuple) (Revocation, error) {
	return h.write(ctx, OperationDelete, tuple)
}

// revocationSetting is the transaction-local setting that holds the identity of the transaction's revocation
// (ST-SUP-RP06-LC12): the first tuple write of any handle sets it, and every later write reads it.
const revocationSetting = "stego_authz.revocation"

// revocation returns the revocation of the handle's transaction, and records it the first time.
func (h *Handle) revocation(ctx context.Context) (uuid.UUID, error) {
	var current sql.NullString
	if err := h.tx.QueryRowContext(ctx, "SELECT pg_catalog.current_setting('"+revocationSetting+"', true)").Scan(&current); err != nil {
		return uuid.Nil, errors.New("authz: the revocation of the transaction cannot be read")
	}
	if current.String != "" {
		id, err := uuid.Parse(current.String)
		if err != nil {
			return uuid.Nil, errors.New("authz: the revocation setting of the transaction is not an identity")
		}
		return id, nil
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return uuid.Nil, errors.New("authz: no revocation identity can be made")
	}
	if _, err := h.tx.ExecContext(ctx, "INSERT INTO "+h.runtime.schema+
		".authz_revocations (id, tenant_id, principal) VALUES ($1, $2, $3)",
		id, h.tenant.ID(), h.principal.subject.String()); err != nil {
		return uuid.Nil, errors.New("authz: the revocation cannot be recorded")
	}
	var set string
	if err := h.tx.QueryRowContext(ctx, "SELECT pg_catalog.set_config('"+revocationSetting+"', $1, true)", id.String()).Scan(&set); err != nil {
		return uuid.Nil, errors.New("authz: the revocation setting of the transaction cannot be set")
	}
	return id, nil
}

func (h *Handle) write(ctx context.Context, operation Operation, tuple Tuple) (Revocation, error) {
	relation := Relation{ResourceType: tuple.Resource.Type, Name: tuple.Relation}
	if !h.allowed[relation] {
		return Revocation{}, fmt.Errorf("authz: the handle may not write the relation %s", relation)
	}
	if err := tuple.valid(); err != nil {
		return Revocation{}, err
	}
	key, err := tupleKey(h.tenant, tuple)
	if err != nil {
		return Revocation{}, err
	}
	revocation, err := h.revocation(ctx)
	if err != nil {
		return Revocation{}, err
	}
	messageID, err := uuid.NewRandom()
	if err != nil {
		return Revocation{}, errors.New("authz: no message identity can be made")
	}
	body, err := json.Marshal(payload{Version: payloadVersion, Operation: operation, Revocation: revocation,
		Tuple: tupleJSON{ResourceType: tuple.Resource.Type, ResourceID: tuple.Resource.ID, Relation: tuple.Relation,
			SubjectType: tuple.Subject.Type, SubjectID: tuple.Subject.ID, SubjectRelation: tuple.SubjectRelation}})
	if err != nil {
		return Revocation{}, errors.New("authz: the tuple cannot be encoded")
	}
	if _, err := h.tx.ExecContext(ctx, "INSERT INTO "+h.runtime.schema+
		".authz_revocation_tuples (message_id, revocation_id, tenant_id) VALUES ($1, $2, $3)",
		messageID, revocation, h.tenant.ID()); err != nil {
		return Revocation{}, errors.New("authz: the tuple of the revocation cannot be recorded")
	}
	if err := h.runtime.outbox.Enqueue(ctx, h.tx, outbox.Message{
		ID:          messageID,
		Destination: DestinationName,
		ResourceKey: resourceKey(tuple.Resource.Type, key),
		Kind:        "authz." + string(operation),
		Payload:     body,
		OutgoingKey: messageID.String(),
		Group:       revocation.String(),
		Principal:   h.principal.subject.String(),
		MustLand:    true,
		Idempotent:  true,
	}); err != nil {
		return Revocation{}, fmt.Errorf("authz: the tuple %s cannot be enqueued for the relay", tuple.Name())
	}
	return Revocation{id: revocation}, nil
}

// RevocationState is the state of a revocation (ST-SUP-RP06-LC15).
type RevocationState string

const (
	// RevocationPending is a revocation with a tuple that the relay has not landed yet.
	RevocationPending RevocationState = "pending"
	// RevocationComplete is a revocation whose every tuple the relay landed.
	RevocationComplete RevocationState = "complete"
	// RevocationParked is a revocation with a parked tuple, which waits for a person.
	RevocationParked RevocationState = "parked"
	// RevocationFailed is a revocation with a tuple that a person discarded.
	RevocationFailed RevocationState = "failed"
	// RevocationExpired is a revocation whose record is older than the retention.
	RevocationExpired RevocationState = "expired"
)

// RevocationStatus is the status of a revocation: its identity, its state and, when it is complete, the floor that
// the transaction in which its last tuple became done read.
type RevocationStatus struct {
	ID    uuid.UUID
	State RevocationState
	Token Token
}

// ErrRevocationNotFound is the error of Status for a revocation that does not exist, or that another tenant or
// another principal made: the two cases give the same answer (ST-SUP-RP06-LC15).
var ErrRevocationNotFound = errors.New("authz: the revocation is not found")

// ErrWaitBeforeCommit is the error of Wait inside the transaction that made the revocation (ST-SUP-RP06-LC12).
var ErrWaitBeforeCommit = errors.New("authz: Wait is refused before the transaction commits")

// Status reads the record of the revocation id (ST-SUP-RP06-LC18) for tenant t and principal p, in a read-only
// transaction of its own, and never the outbox. It answers ErrRevocationNotFound to any other tenant and principal.
// The state is expired after the retention; otherwise failed when a person discarded a tuple, parked while a tuple
// is parked, pending until every tuple is done, and complete with the floor of the last tuple's done transaction.
func (r *Runtime) Status(ctx context.Context, t tenant.Tenant, p Principal, id uuid.UUID) (RevocationStatus, error) {
	if p.IsZero() {
		return RevocationStatus{}, errors.New("authz: no principal")
	}
	tx, err := tenant.Begin(ctx, r.db, t, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return RevocationStatus{}, err
	}
	var count sql.NullInt64
	var done, parked, discarded int64
	var token string
	var created time.Time
	err = tx.QueryRowContext(ctx, "SELECT tuple_count, done, parked, discarded, token, created_at FROM "+r.schema+
		".authz_revocations WHERE id = $1 AND tenant_id = $2 AND principal = $3", id, t.ID(), p.subject.String()).
		Scan(&count, &done, &parked, &discarded, &token, &created)
	if errors.Is(err, sql.ErrNoRows) {
		_ = tx.Rollback()
		return RevocationStatus{}, ErrRevocationNotFound
	}
	if err != nil {
		_ = tx.Rollback()
		return RevocationStatus{}, fixed("authz: the revocation record cannot be read", err)
	}
	if err := tenant.Commit(ctx, tx, t); err != nil {
		return RevocationStatus{}, err
	}
	status := RevocationStatus{ID: id}
	switch {
	case created.Before(r.clock.Now().Add(-r.retention)):
		status.State = RevocationExpired
	case discarded > 0:
		status.State = RevocationFailed
	case parked > 0:
		status.State = RevocationParked
	case !count.Valid || done < count.Int64 || token == "":
		status.State = RevocationPending
	default:
		status.State = RevocationComplete
		if status.Token, err = ParseToken(token); err != nil {
			return RevocationStatus{}, err
		}
	}
	return status, nil
}

// Wait is Status that waits until the state is no longer pending or ctx ends; when ctx ends it returns the pending
// status and no error (ST-SUP-RP06-LC12). It holds no transaction between two reads. It is refused inside the
// transaction that made the revocation, before the commit.
func (h *Handle) Wait(ctx context.Context, revocation Revocation) (RevocationStatus, error) {
	var one int
	err := h.tx.QueryRowContext(ctx, "SELECT 1").Scan(&one)
	if err == nil {
		return RevocationStatus{}, ErrWaitBeforeCommit
	}
	if !errors.Is(err, sql.ErrTxDone) {
		return RevocationStatus{}, errors.New("authz: Wait cannot tell that the transaction ended")
	}
	r := h.runtime
	pending := RevocationStatus{ID: revocation.id, State: RevocationPending}
	for {
		status, err := r.Status(ctx, h.tenant, h.principal, revocation.id)
		if err != nil {
			if ctx.Err() != nil {
				return pending, nil
			}
			return RevocationStatus{}, err
		}
		if status.State != RevocationPending {
			return status, nil
		}
		timer := time.NewTimer(r.poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return status, nil
		case <-timer.C:
		}
	}
}

// quoteName quotes one SQL name, such as a schema, a table or a role, and refuses an empty name or one that holds a
// control character or is not valid UTF-8.
func quoteName(kind, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("authz: no %s", kind)
	}
	if !cleanText(name) {
		return "", fmt.Errorf("authz: the %s holds a control character or is not valid UTF-8", kind)
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`, nil
}

// migrationText creates the revocation floor and the revocation records (ST-SUP-RP06-LC21). It first refuses a
// runtime role that is a member of the relay login role. The floor holds one row and no tenant: the runtime role may
// read it, and the relay login role may update it and nothing else. The revocation records are tenant-owned, with
// row-level security enabled and forced: the runtime role may read them and insert a new revocation and its tuples,
// never a count or a token. The counts and the token change only through the function authz_revocation_state, which
// the NOLOGIN outbox relay role of the service owns, apart from the relay login role, and which the outbox state hook
// calls. The outbox relay role makes both functions itself, under SET LOCAL ROLE, with CREATE on the schema only
// while it makes them, so the owner needs only SET membership in it.
// The function reads the state of the outbox message through the one relay policy of the queue table, and counts a
// change only when the message has the destination authz and is in the state the hook reports; it refuses the state
// done unless session_user is a hook role, and refuses to run under an isolation other than READ COMMITTED. Status
// never reads the outbox.
const migrationText = `DO $do$
DECLARE
	v_role text;
BEGIN
	FOREACH v_role IN ARRAY ARRAY[{{relay_text}}, {{outbox_relay_text}}] || {{hook_array}} LOOP
		IF pg_catalog.pg_has_role({{runtime_text}}, v_role, 'MEMBER') THEN
			RAISE EXCEPTION 'authz: the runtime role % is a member of the role %', {{runtime_text}}, v_role;
		END IF;
	END LOOP;
END $do$;

CREATE TABLE IF NOT EXISTS {{schema}}.authz_floor (
	only_row boolean PRIMARY KEY DEFAULT true CHECK (only_row),
	token text NOT NULL DEFAULT '' CHECK (octet_length(token) <= 4096),
	sequence bigint NOT NULL DEFAULT 0
);
INSERT INTO {{schema}}.authz_floor (only_row) VALUES (true) ON CONFLICT (only_row) DO NOTHING;
REVOKE ALL ON {{schema}}.authz_floor FROM PUBLIC;
GRANT SELECT ON {{schema}}.authz_floor TO {{runtime_role}}, {{relay_role}}, {{outbox_relay}};
GRANT UPDATE (token, sequence) ON {{schema}}.authz_floor TO {{relay_role}};

CREATE TABLE IF NOT EXISTS {{schema}}.authz_revocations (
	id uuid PRIMARY KEY,
	tenant_id text NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
	principal text NOT NULL CHECK (octet_length(principal) BETWEEN 1 AND 1200),
	tuple_count bigint CHECK (tuple_count >= 1),
	done bigint NOT NULL DEFAULT 0 CHECK (done >= 0),
	parked bigint NOT NULL DEFAULT 0 CHECK (parked >= 0),
	discarded bigint NOT NULL DEFAULT 0 CHECK (discarded >= 0),
	token text NOT NULL DEFAULT '' CHECK (octet_length(token) <= 4096),
	created_at timestamptz NOT NULL DEFAULT pg_catalog.now(),
	completed_at timestamptz
);
ALTER TABLE {{schema}}.authz_revocations ENABLE ROW LEVEL SECURITY;
ALTER TABLE {{schema}}.authz_revocations FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS authz_revocations_tenant ON {{schema}}.authz_revocations;
CREATE POLICY authz_revocations_tenant ON {{schema}}.authz_revocations
	USING (tenant_id = pg_catalog.current_setting('app.tenant', true))
	WITH CHECK (tenant_id = pg_catalog.current_setting('app.tenant', true));
REVOKE ALL ON {{schema}}.authz_revocations FROM PUBLIC;
GRANT SELECT ON {{schema}}.authz_revocations TO {{runtime_role}}, {{outbox_relay}};
GRANT INSERT (id, tenant_id, principal, tuple_count) ON {{schema}}.authz_revocations TO {{runtime_role}};
GRANT UPDATE (tuple_count, done, parked, discarded, token, completed_at) ON {{schema}}.authz_revocations TO {{outbox_relay}};

CREATE TABLE IF NOT EXISTS {{schema}}.authz_revocation_tuples (
	message_id uuid PRIMARY KEY,
	revocation_id uuid NOT NULL REFERENCES {{schema}}.authz_revocations (id),
	tenant_id text NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
	state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'done', 'parked', 'discarded'))
);
CREATE INDEX IF NOT EXISTS authz_revocation_tuples_revocation ON {{schema}}.authz_revocation_tuples (revocation_id);
ALTER TABLE {{schema}}.authz_revocation_tuples ENABLE ROW LEVEL SECURITY;
ALTER TABLE {{schema}}.authz_revocation_tuples FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS authz_revocation_tuples_tenant ON {{schema}}.authz_revocation_tuples;
CREATE POLICY authz_revocation_tuples_tenant ON {{schema}}.authz_revocation_tuples
	USING (tenant_id = pg_catalog.current_setting('app.tenant', true))
	WITH CHECK (tenant_id = pg_catalog.current_setting('app.tenant', true));
REVOKE ALL ON {{schema}}.authz_revocation_tuples FROM PUBLIC;
GRANT SELECT ON {{schema}}.authz_revocation_tuples TO {{runtime_role}}, {{outbox_relay}};
GRANT INSERT (message_id, revocation_id, tenant_id) ON {{schema}}.authz_revocation_tuples TO {{runtime_role}};
GRANT UPDATE (state) ON {{schema}}.authz_revocation_tuples TO {{outbox_relay}};
GRANT USAGE ON SCHEMA {{schema}} TO {{outbox_relay}};

-- The outbox relay role makes the two functions itself, so the role that runs the text needs only SET membership in
-- it. CREATE on the schema is given for that alone and taken back below, in the same transaction.
SELECT pg_catalog.set_config('stego.authz_migration_role', CURRENT_USER, true),
	pg_catalog.set_config('stego.authz_relay_create', '', true);
DO $do$ BEGIN
	IF NOT pg_catalog.pg_has_role(CURRENT_USER, {{outbox_relay_text}}, 'SET') THEN
		RAISE EXCEPTION 'authz: the migration role % needs membership in % WITH SET TRUE', CURRENT_USER, {{outbox_relay_text}};
	END IF;
	IF NOT pg_catalog.has_schema_privilege({{outbox_relay_text}}, {{schema_text}}, 'CREATE') THEN
		EXECUTE 'GRANT CREATE ON SCHEMA ' || {{schema_quoted_text}} || ' TO {{outbox_relay}}';
		PERFORM pg_catalog.set_config('stego.authz_relay_create', 'granted', true);
	END IF;
END $do$;
SET LOCAL ROLE {{outbox_relay}};

-- A tuple row must name an authz outbox message of its own revocation, of its tenant, and both the message and the
-- revocation must be written by the same transaction as the row, so the runtime role cannot stall a revocation with
-- extra tuple rows. The check runs at the commit, after Enqueue wrote the message.
CREATE OR REPLACE FUNCTION {{schema}}.authz_revocation_tuple_check() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE
	v_epoch bigint := (pg_catalog.pg_current_xact_id()::text::bigint >> 32) << 32;
BEGIN
	PERFORM 1 FROM stego_outbox.messages AS m, {{schema}}.authz_revocations AS r
		WHERE m.id = NEW.message_id AND m.destination = 'authz' AND m.group_id = NEW.revocation_id::text
			AND m.tenant_id = NEW.tenant_id AND r.id = NEW.revocation_id AND r.tenant_id = NEW.tenant_id
			AND pg_catalog.pg_xact_status((v_epoch | m.xmin::text::bigint)::text::xid8) = 'in progress'
			AND pg_catalog.pg_xact_status((v_epoch | r.xmin::text::bigint)::text::xid8) = 'in progress';
	IF NOT FOUND THEN
		RAISE EXCEPTION 'authz: a tuple row names no authz message of its revocation from the same transaction';
	END IF;
	RETURN NULL;
END
$$;
REVOKE ALL ON FUNCTION {{schema}}.authz_revocation_tuple_check() FROM PUBLIC;

CREATE OR REPLACE FUNCTION {{schema}}.authz_revocation_state(p_message uuid, p_revocation uuid, p_state text) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE
	v_tenant text := pg_catalog.current_setting('app.tenant', true);
	v_old text;
	v_count bigint;
BEGIN
	IF pg_catalog.current_setting('transaction_isolation') <> 'read committed' THEN
		RAISE EXCEPTION 'authz_revocation_state: the transaction is not READ COMMITTED';
	END IF;
	IF p_state = 'done' AND NOT (SESSION_USER = ANY ({{hook_array}}::name[])) THEN
		RAISE EXCEPTION 'authz_revocation_state: only a hook role may report the state done';
	END IF;
	IF p_state IS NULL OR p_state NOT IN ('pending', 'done', 'parked', 'discarded') OR v_tenant IS NULL OR v_tenant = '' THEN
		RETURN false;
	END IF;
	PERFORM 1 FROM stego_outbox.messages AS m
		WHERE m.id = p_message AND m.destination = 'authz' AND m.state = p_state AND m.group_id = p_revocation::text
			AND m.tenant_id = v_tenant;
	IF NOT FOUND THEN
		RETURN false;
	END IF;
	SELECT t.state INTO v_old FROM {{schema}}.authz_revocation_tuples AS t
		WHERE t.message_id = p_message AND t.revocation_id = p_revocation AND t.tenant_id = v_tenant FOR UPDATE;
	IF NOT FOUND OR v_old = p_state OR v_old = 'done' OR v_old = 'discarded' THEN
		RETURN false;
	END IF;
	UPDATE {{schema}}.authz_revocation_tuples SET state = p_state WHERE message_id = p_message;
	SELECT pg_catalog.count(*) INTO v_count FROM {{schema}}.authz_revocation_tuples AS t
		WHERE t.revocation_id = p_revocation;
	UPDATE {{schema}}.authz_revocations AS r SET
		tuple_count = v_count,
		done = r.done + (p_state = 'done')::int,
		parked = r.parked + (p_state = 'parked')::int - (v_old = 'parked')::int,
		discarded = r.discarded + (p_state = 'discarded')::int
		WHERE r.id = p_revocation AND r.tenant_id = v_tenant;
	UPDATE {{schema}}.authz_revocations AS r SET
		token = (SELECT f.token FROM {{schema}}.authz_floor AS f WHERE f.only_row),
		completed_at = pg_catalog.now()
		WHERE r.id = p_revocation AND r.tenant_id = v_tenant AND r.done = r.tuple_count AND r.token = '';
	RETURN true;
END
$$;
REVOKE ALL ON FUNCTION {{schema}}.authz_revocation_state(uuid, uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION {{schema}}.authz_revocation_state(uuid, uuid, text) TO {{runtime_role}};
{{hook_grants}}
-- The role that runs the text makes the trigger, which needs EXECUTE on the trigger function; PostgreSQL calls a
-- trigger function only as a trigger. Then it takes its own role back and the CREATE that it gave. Both role changes
-- are local to the transaction, so the role of the session is the same after it.
DO $do$
DECLARE
	v_role text := pg_catalog.current_setting('stego.authz_migration_role');
BEGIN
	EXECUTE 'GRANT EXECUTE ON FUNCTION ' || {{schema_quoted_text}} || '.authz_revocation_tuple_check() TO ' || pg_catalog.quote_ident(v_role);
	EXECUTE pg_catalog.format('SET LOCAL ROLE %I', v_role);
	IF pg_catalog.current_setting('stego.authz_relay_create') = 'granted' THEN
		EXECUTE 'REVOKE CREATE ON SCHEMA ' || {{schema_quoted_text}} || ' FROM {{outbox_relay}}';
	END IF;
END
$do$;
DROP TRIGGER IF EXISTS authz_revocation_tuple_check ON {{schema}}.authz_revocation_tuples;
CREATE CONSTRAINT TRIGGER authz_revocation_tuple_check AFTER INSERT ON {{schema}}.authz_revocation_tuples
	DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION {{schema}}.authz_revocation_tuple_check();
`

// Migration returns the SQL text that creates the revocation floor authz_floor, the revocation records and the
// function of the outbox state hook in schema, for the migration job to run as the owner role after the migration of
// the outbox runtime, whose table the function reads (ST-SUP-RP06-LC21). The functions belong to outboxRelayRole,
// the NOLOGIN relay role of this service alone, <service role prefix>_outbox_relay, which the outbox migration
// names; the owner role needs the membership WITH SET TRUE that the outbox migration names, and no INHERIT.
// runtimeRole is the role the service runs as; relayRole is the login role of the relay, which may update the floor
// and nothing else; hookRoles are the roles that call the outbox state hook, such as the outbox worker role, and
// only they may report the state done. It refuses a runtime role that equals the relay login role, a relay login
// role that equals a hook role, no hook role, and an outbox relay role that is not a lower-case SQL name ending
// with outbox.RelaySuffix or that equals one of the other roles; the text refuses a runtime role that is a member of
// the relay login role or of the outbox relay role before it creates anything. The text runs again with no error on a database that it made: each table and
// index is made only when it is missing, the floor row only once, each function and trigger is replaced, and each
// policy is dropped before it is made again, so the migration job can run on every release. Run it in one
// transaction.
func Migration(schema, runtimeRole, relayRole, outboxRelayRole string, hookRoles ...string) (string, error) {
	quotedSchema, err := quoteName("schema", schema)
	if err != nil {
		return "", err
	}
	quotedRuntime, err := quoteName("runtime role", runtimeRole)
	if err != nil {
		return "", err
	}
	quotedRelay, err := quoteName("relay role", relayRole)
	if err != nil {
		return "", err
	}
	if runtimeRole == relayRole {
		return "", fmt.Errorf("authz: the runtime role %s and the relay role %s must be two roles", quotedRuntime, quotedRelay)
	}
	if len(hookRoles) == 0 {
		return "", errors.New("authz: the migration needs at least one hook role")
	}
	if !outboxRelayPattern.MatchString(outboxRelayRole) || !strings.HasSuffix(outboxRelayRole, outbox.RelaySuffix) ||
		outboxRelayRole == outbox.RelaySuffix[1:] {
		return "", fmt.Errorf("authz: the outbox relay role %q is not a <service role prefix>%s role", outboxRelayRole, outbox.RelaySuffix)
	}
	for _, role := range append([]string{runtimeRole, relayRole}, hookRoles...) {
		if role == outboxRelayRole {
			return "", fmt.Errorf("authz: the outbox relay role %s must be a role apart from the runtime, relay and hook roles", outboxRelayRole)
		}
	}
	for _, role := range hookRoles {
		if role == runtimeRole {
			return "", fmt.Errorf("authz: the runtime role %s and the hook role %s must be two roles", quotedRuntime, quotedRuntime)
		}
	}
	var grants strings.Builder
	var hooks []string
	for _, role := range hookRoles {
		quoted, err := quoteName("hook role", role)
		if err != nil {
			return "", err
		}
		if role == relayRole {
			return "", fmt.Errorf("authz: the relay role %s and the hook role %s must be two roles", quotedRelay, quoted)
		}
		grants.WriteString("GRANT EXECUTE ON FUNCTION " + quotedSchema + ".authz_revocation_state(uuid, uuid, text) TO " + quoted + ";\n")
		hooks = append(hooks, sqlText(role))
	}
	return strings.NewReplacer("{{schema}}", quotedSchema, "{{runtime_role}}", quotedRuntime,
		"{{relay_role}}", quotedRelay, "{{runtime_text}}", sqlText(runtimeRole), "{{relay_text}}", sqlText(relayRole),
		"{{outbox_relay}}", `"`+outboxRelayRole+`"`, "{{outbox_relay_text}}", sqlText(outboxRelayRole), "{{schema_text}}", sqlText(schema), "{{schema_quoted_text}}", sqlText(quotedSchema),
		"{{hook_array}}", "ARRAY["+strings.Join(hooks, ", ")+"]", "{{hook_grants}}", grants.String()).Replace(migrationText), nil
}

// Manifest gives what the code of the revocation floor and the revocation records needs of the database that
// Migration makes in schema for the same roles (ST-SUP-RP06-LC23, ST-SUP-RP15-LC01): the tables authz_floor,
// authz_revocations and authz_revocation_tuples with their columns, checks, keys and foreign key, row-level security
// with the tenant policy for every role on the two revocation tables, the exact grants of each table and its columns,
// the trigger authz_revocation_tuple_check, and the function authz_revocation_state; both functions are SECURITY
// DEFINER with their fixed search path and owned by outboxRelayRole, and EXECUTE on authz_revocation_state goes to
// runtimeRole and the hook roles only; CREATE on schema goes to no role but its owner and the owner of the manifest.
// schema is a lower-case SQL name that needs no quotes, such as public. Manifest
// refuses the roles that Migration refuses.
func Manifest(schema, runtimeRole, relayRole, outboxRelayRole string, hookRoles ...string) (manifest.Manifest, error) {
	if !outboxRelayPattern.MatchString(schema) {
		return manifest.Manifest{}, fmt.Errorf("authz: %q is not a schema name the manifest accepts", schema)
	}
	text, err := Migration(schema, runtimeRole, relayRole, outboxRelayRole, hookRoles...)
	if err != nil {
		return manifest.Manifest{}, err
	}
	source := func(signature string) (string, error) {
		_, rest, found := strings.Cut(text, "CREATE OR REPLACE FUNCTION \""+schema+"\"."+signature)
		_, body, opened := strings.Cut(rest, "AS $$")
		src, _, closed := strings.Cut(body, "$$")
		if !found || !opened || !closed {
			return "", fmt.Errorf("authz: the migration has no function %s", signature)
		}
		return src, nil
	}
	checkSource, err := source("authz_revocation_tuple_check()")
	if err != nil {
		return manifest.Manifest{}, err
	}
	stateSource, err := source("authz_revocation_state(")
	if err != nil {
		return manifest.Manifest{}, err
	}
	column := func(name, typ string, notNull bool) manifest.Column {
		return manifest.Column{Name: name, Type: typ, NotNull: notNull}
	}
	withDefault := func(c manifest.Column, def string) manifest.Column { c.Default = def; return c }
	check := func(table, column, definition string) manifest.Constraint {
		return manifest.Constraint{Name: table + "_" + column + "_check", Kind: manifest.KindCheck, Definition: definition}
	}
	key := func(table, column string) manifest.Constraint {
		return manifest.Constraint{Name: table + "_pkey", Kind: manifest.KindPrimaryKey, Definition: "PRIMARY KEY (" + column + ")"}
	}
	tenant := "CHECK (((octet_length(tenant_id) >= 1) AND (octet_length(tenant_id) <= 255)))"
	token := "CHECK ((octet_length(token) <= 4096))"
	wall := "(tenant_id = current_setting('app.tenant'::text, true))"
	tenantPolicy := func(table string) []manifest.Policy {
		return []manifest.Policy{{Name: table + "_tenant", Command: manifest.CommandAll, Using: wall, WithCheck: wall}}
	}
	timestamp := "timestamp with time zone"
	config := []string{"search_path=pg_catalog, pg_temp"}
	floor := manifest.Table{Schema: schema, Name: "authz_floor",
		Columns: []manifest.Column{withDefault(column("only_row", "boolean", true), "true"), withDefault(column("token", "text", true), "''::text"),
			withDefault(column("sequence", "bigint", true), "0")},
		Constraints: []manifest.Constraint{key("authz_floor", "only_row"), check("authz_floor", "only_row", "CHECK (only_row)"), check("authz_floor", "token", token)},
		Indexes:     []manifest.Index{{Name: "authz_floor_pkey", Columns: []string{"only_row"}}},
		CheckGrants: true, Grants: []manifest.Grant{
			{Role: runtimeRole, Privileges: []string{"SELECT"}}, {Role: relayRole, Privileges: []string{"SELECT"}},
			{Role: outboxRelayRole, Privileges: []string{"SELECT"}}, {Role: relayRole, Privileges: []string{"UPDATE"}, Columns: []string{"token", "sequence"}},
		}}
	revocations := manifest.Table{Schema: schema, Name: "authz_revocations",
		Columns: []manifest.Column{column("id", "uuid", true), column("tenant_id", "text", true), column("principal", "text", true),
			column("tuple_count", "bigint", false), withDefault(column("done", "bigint", true), "0"), withDefault(column("parked", "bigint", true), "0"),
			withDefault(column("discarded", "bigint", true), "0"), withDefault(column("token", "text", true), "''::text"), withDefault(column("created_at", timestamp, true), "now()"), column("completed_at", timestamp, false)},
		Constraints: []manifest.Constraint{key("authz_revocations", "id"), check("authz_revocations", "tenant_id", tenant),
			check("authz_revocations", "principal", "CHECK (((octet_length(principal) >= 1) AND (octet_length(principal) <= 1200)))"),
			check("authz_revocations", "tuple_count", "CHECK ((tuple_count >= 1))"), check("authz_revocations", "done", "CHECK ((done >= 0))"),
			check("authz_revocations", "parked", "CHECK ((parked >= 0))"), check("authz_revocations", "discarded", "CHECK ((discarded >= 0))"),
			check("authz_revocations", "token", token)},
		Indexes:     []manifest.Index{{Name: "authz_revocations_pkey", Columns: []string{"id"}}},
		RowSecurity: true, Policies: tenantPolicy("authz_revocations"),
		CheckGrants: true, Grants: []manifest.Grant{
			{Role: runtimeRole, Privileges: []string{"SELECT"}}, {Role: outboxRelayRole, Privileges: []string{"SELECT"}},
			{Role: runtimeRole, Privileges: []string{"INSERT"}, Columns: []string{"id", "tenant_id", "principal", "tuple_count"}},
			{Role: outboxRelayRole, Privileges: []string{"UPDATE"}, Columns: []string{"tuple_count", "done", "parked", "discarded", "token", "completed_at"}},
		}}
	tuples := manifest.Table{Schema: schema, Name: "authz_revocation_tuples",
		Columns: []manifest.Column{column("message_id", "uuid", true), column("revocation_id", "uuid", true), column("tenant_id", "text", true),
			withDefault(column("state", "text", true), "'pending'::text")},
		Constraints: []manifest.Constraint{key("authz_revocation_tuples", "message_id"),
			{Name: "authz_revocation_tuples_revocation_id_fkey", Kind: manifest.KindForeignKey,
				Definition: "FOREIGN KEY (revocation_id) REFERENCES " + schema + ".authz_revocations(id)"},
			check("authz_revocation_tuples", "tenant_id", tenant),
			check("authz_revocation_tuples", "state", "CHECK ((state = ANY (ARRAY['pending'::text, 'done'::text, 'parked'::text, 'discarded'::text])))")},
		Indexes:     []manifest.Index{{Name: "authz_revocation_tuples_pkey", Columns: []string{"message_id"}}},
		RowSecurity: true, Policies: tenantPolicy("authz_revocation_tuples"),
		Triggers: []manifest.Trigger{{Name: "authz_revocation_tuple_check", Timing: "AFTER", Row: true, Events: []string{"INSERT"},
			Function: manifest.Function{Name: schema + ".authz_revocation_tuple_check", Language: "plpgsql", Source: checkSource,
				SecurityDefiner: true, Config: config, Owner: outboxRelayRole}}},
		CheckGrants: true, Grants: []manifest.Grant{
			{Role: runtimeRole, Privileges: []string{"SELECT"}}, {Role: outboxRelayRole, Privileges: []string{"SELECT"}},
			{Role: runtimeRole, Privileges: []string{"INSERT"}, Columns: []string{"message_id", "revocation_id", "tenant_id"}},
			{Role: outboxRelayRole, Privileges: []string{"UPDATE"}, Columns: []string{"state"}},
		}}
	state := manifest.Function{Name: schema + ".authz_revocation_state", Args: "uuid, uuid, text", Language: "plpgsql", Source: stateSource,
		SecurityDefiner: true, Config: config, Owner: outboxRelayRole, Execute: append([]string{runtimeRole}, hookRoles...), Returns: "boolean"}
	// The schema may be public, which holds the objects of others, so only its CREATE is checked: no role but its
	// owner and the owner of the manifest can make a function, such as an overload of the state hook, in it.
	return manifest.Manifest{Tables: []manifest.Table{floor, revocations, tuples}, Functions: []manifest.Function{state},
		Schemas: []manifest.Schema{{Name: schema, Shared: true}}, Confined: []string{outboxRelayRole}}, nil
}

// outboxRelayPattern is a lower-case SQL name of at most 63 characters, which needs no quotes.
var outboxRelayPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// sqlText quotes text as an SQL string literal.
func sqlText(text string) string { return "'" + strings.ReplaceAll(text, "'", "''") + "'" }
