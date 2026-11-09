package authz

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"stego.dev/runtime/internal/seal"
	"stego.dev/runtime/outbox"
	"stego.dev/runtime/tenant"
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
	// Floor gives the revocation floor to Port, for a caller with no transaction of its own, such as a TableFloor.
	// A handle reads the floor through its own transaction instead.
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
}

// New returns the authorization runtime of cfg. It refuses a configuration that lacks a part.
func New(cfg Config) (*Runtime, error) {
	port, err := NewPort(cfg.Provider, cfg.Floor)
	if err != nil {
		return nil, err
	}
	switch {
	case cfg.DB == nil:
		return nil, errors.New("authz: no database")
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
	return r, nil
}

// Port returns the runtime's decision entry point, for a worker that decides with no transaction of its own, such
// as the outbox worker's Resolve (ST-SUP-RP01-LC17). It holds no relationships port.
func (r *Runtime) Port() *Port { return r.port }

// Decide is the decision port of the outbox worker's Resolve (ST-SUP-RP01-LC17): may the subject of request, written
// type:id, do the permission on the resource, written type:id. It decides at the floor (ST-SUP-RP06-LC09), which it
// reads through the Floor of the configuration, as a worker with no transaction of its own does. A deny is false
// with no error.
func (r *Runtime) Decide(ctx context.Context, request outbox.DecisionRequest) (bool, error) {
	subjectType, subjectID, ok := strings.Cut(request.Subject, ":")
	if !ok {
		return false, errors.New("authz: the subject of the resolution is not type:id")
	}
	resourceType, resourceID, ok := strings.Cut(request.Resource, ":")
	if !ok {
		return false, errors.New("authz: the resource of the resolution is not type:id")
	}
	floor, err := r.port.currentFloor(ctx)
	if err != nil {
		return false, err
	}
	result, err := r.port.evaluate(ctx, EvaluationRequest{
		Subject:     Subject{Type: subjectType, ID: subjectID},
		Action:      Action{Name: request.Permission},
		Resource:    Resource{Type: resourceType, ID: resourceID},
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
// outbox, for the relations it was bound with. It exposes no provider, no relationships port and no field.
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

// floor reads the revocation floor through the handle's own transaction, never through a second connection of the
// pool (ST-SUP-RP06-LC09).
func (h *Handle) floor(ctx context.Context) (Token, error) {
	var text string
	if err := h.tx.QueryRowContext(ctx, "SELECT token FROM "+h.runtime.schema+".authz_floor WHERE only_row").Scan(&text); err != nil {
		return Token{}, errors.New("authz: the revocation floor cannot be read in the transaction")
	}
	return ParseToken(text)
}

// Evaluate decides action on resource, whose row holds rowToken, with the principal as subject, at least as fresh
// as the revocation floor; only while the floor is zero, at least as fresh as the row token; and fresh when the row
// has no token either (ST-SUP-RP06-LC09). A deny is a result with Allowed false and a nil error.
func (h *Handle) Evaluate(ctx context.Context, action string, resource Resource, rowToken Token, c Context) (EvaluationResult, error) {
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

// Search returns one page of the resources of resourceType that the principal may do action on, at least as fresh
// as the revocation floor at this page, or ErrNoSearch when the provider does not search. A later page keeps the
// first page's consistency in its page token, and the provider filters its results at this page's floor
// (ST-SUP-RP06-LC05).
func (h *Handle) Search(ctx context.Context, action, resourceType string, c Context, page Page) (SearchResult, error) {
	floor, err := h.floor(ctx)
	if err != nil {
		return SearchResult{}, err
	}
	return h.runtime.port.search(ctx, SearchRequest{Subject: h.principal.subject, Action: Action{Name: action},
		ResourceType: resourceType, Context: c, Consistency: atFloor(floor, Token{}), Page: page}, Token{})
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

// TenantAction is the action of a Provision or Retire request.
type TenantAction string

const (
	// Provision is the action of a Provision request.
	Provision TenantAction = "provision"
	// Retire is the action of a Retire request.
	Retire TenantAction = "retire"
)

// tenantDecision is the decision that tenant.Requested takes. Only DecideTenant makes one, after an allow, with the
// mark of the internal seal constructor.
type tenantDecision struct {
	seal.DecisionMark
}

// DecideTenant decides the permission of action, provision or retire, on resource at the floor, for a request that
// names the tenant tenantID (ST-SUP-RP06-LC17). On an allow it returns the decision that tenant.Requested takes,
// whose mark the internal seal constructor makes with exactly that tenant ID, and true; on a deny it returns no
// decision and false.
func (h *Handle) DecideTenant(ctx context.Context, action TenantAction, tenantID string, resource Resource, c Context) (tenant.Decision, bool, error) {
	if action != Provision && action != Retire {
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
	return tenantDecision{DecisionMark: seal.NewDecisionMark(tenantID)}, true, nil
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

// payloadVersion is the version of the relay payload.
const payloadVersion = 1

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
		ResourceKey: tuple.Resource.String(),
		Kind:        "authz." + string(operation),
		Payload:     body,
		OutgoingKey: messageID.String(),
		Group:       revocation.String(),
		Principal:   h.principal.subject.String(),
		MustLand:    true,
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
		return RevocationStatus{}, errors.New("authz: the revocation record cannot be read")
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

// migrationText creates the revocation floor and the revocation records (ST-SUP-RP06-LC21). The floor holds one row
// and no tenant: the runtime role may read it, and only the relay role may change it. The revocation records are
// tenant-owned, with row-level security enabled and forced: the runtime role may read them and insert a new
// revocation and its tuples, never a count or a token, and the counts and the token change only through the
// function authz_revocation_state, which the relay role owns and the outbox state hook calls. The function counts a
// change only when the outbox message it names is in the state the hook reports, so a caller cannot mark a
// revocation complete. It reads the state of the message in the table stego_outbox.messages of the outbox runtime,
// for which the text gives the relay role a SELECT policy of the transaction's tenant; Status never reads the outbox.
const migrationText = `CREATE TABLE {{schema}}.authz_floor (
	only_row boolean PRIMARY KEY DEFAULT true CHECK (only_row),
	token text NOT NULL DEFAULT '' CHECK (octet_length(token) <= 4096),
	sequence bigint NOT NULL DEFAULT 0
);
INSERT INTO {{schema}}.authz_floor (only_row) VALUES (true);
REVOKE ALL ON {{schema}}.authz_floor FROM PUBLIC;
GRANT SELECT ON {{schema}}.authz_floor TO {{runtime_role}}, {{relay_role}};
GRANT UPDATE (token, sequence) ON {{schema}}.authz_floor TO {{relay_role}};

CREATE TABLE {{schema}}.authz_revocations (
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
CREATE POLICY authz_revocations_tenant ON {{schema}}.authz_revocations
	USING (tenant_id = pg_catalog.current_setting('app.tenant', true))
	WITH CHECK (tenant_id = pg_catalog.current_setting('app.tenant', true));
REVOKE ALL ON {{schema}}.authz_revocations FROM PUBLIC;
GRANT SELECT ON {{schema}}.authz_revocations TO {{runtime_role}}, {{relay_role}};
GRANT INSERT (id, tenant_id, principal, tuple_count) ON {{schema}}.authz_revocations TO {{runtime_role}};
GRANT UPDATE (tuple_count, done, parked, discarded, token, completed_at) ON {{schema}}.authz_revocations TO {{relay_role}};

CREATE TABLE {{schema}}.authz_revocation_tuples (
	message_id uuid PRIMARY KEY,
	revocation_id uuid NOT NULL REFERENCES {{schema}}.authz_revocations (id),
	tenant_id text NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
	state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'done', 'parked', 'discarded'))
);
CREATE INDEX authz_revocation_tuples_revocation ON {{schema}}.authz_revocation_tuples (revocation_id);
ALTER TABLE {{schema}}.authz_revocation_tuples ENABLE ROW LEVEL SECURITY;
ALTER TABLE {{schema}}.authz_revocation_tuples FORCE ROW LEVEL SECURITY;
CREATE POLICY authz_revocation_tuples_tenant ON {{schema}}.authz_revocation_tuples
	USING (tenant_id = pg_catalog.current_setting('app.tenant', true))
	WITH CHECK (tenant_id = pg_catalog.current_setting('app.tenant', true));
REVOKE ALL ON {{schema}}.authz_revocation_tuples FROM PUBLIC;
GRANT SELECT ON {{schema}}.authz_revocation_tuples TO {{runtime_role}}, {{relay_role}};
GRANT INSERT (message_id, revocation_id, tenant_id) ON {{schema}}.authz_revocation_tuples TO {{runtime_role}};
GRANT UPDATE (state) ON {{schema}}.authz_revocation_tuples TO {{relay_role}};

GRANT USAGE ON SCHEMA stego_outbox TO {{relay_role}};
GRANT SELECT (id, tenant_id, group_id, state) ON stego_outbox.messages TO {{relay_role}};
CREATE POLICY authz_relay_reads_state ON stego_outbox.messages FOR SELECT TO {{relay_role}}
	USING (tenant_id = pg_catalog.current_setting('app.tenant', true));

CREATE FUNCTION {{schema}}.authz_revocation_state(p_message uuid, p_revocation uuid, p_state text) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE
	v_tenant text := pg_catalog.current_setting('app.tenant', true);
	v_old text;
	v_count bigint;
BEGIN
	IF p_state IS NULL OR p_state NOT IN ('pending', 'done', 'parked', 'discarded') OR v_tenant IS NULL OR v_tenant = '' THEN
		RETURN false;
	END IF;
	PERFORM 1 FROM stego_outbox.messages AS m
		WHERE m.id = p_message AND m.state = p_state AND m.group_id = p_revocation::text AND m.tenant_id = v_tenant;
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
ALTER FUNCTION {{schema}}.authz_revocation_state(uuid, uuid, text) OWNER TO {{relay_role}};
REVOKE ALL ON FUNCTION {{schema}}.authz_revocation_state(uuid, uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION {{schema}}.authz_revocation_state(uuid, uuid, text) TO {{runtime_role}}, {{relay_role}};
{{hook_grants}}`

// Migration returns the SQL text that creates the revocation floor authz_floor, the revocation records and the
// function of the outbox state hook in schema, for the migration job to run as the owner role, which must be able
// to give the function to the relay role, after the migration of the outbox runtime, whose table it reads. runtimeRole is the role the service runs as; relayRole is the role of the
// relay, the only role that may raise the floor and change a revocation's counts. It refuses one role for both
// (ST-SUP-RP06-LC21). hookRoles are the other roles that call the outbox state hook, such as the role of the outbox
// worker and the role that runs Resolve: each may execute the state function, in the transaction of the change.
func Migration(schema, runtimeRole, relayRole string, hookRoles ...string) (string, error) {
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
	var grants strings.Builder
	for _, role := range hookRoles {
		quoted, err := quoteName("hook role", role)
		if err != nil {
			return "", err
		}
		grants.WriteString("GRANT EXECUTE ON FUNCTION " + quotedSchema + ".authz_revocation_state(uuid, uuid, text) TO " + quoted + ";\n")
	}
	return strings.NewReplacer("{{schema}}", quotedSchema, "{{runtime_role}}", quotedRuntime,
		"{{relay_role}}", quotedRelay, "{{hook_grants}}", grants.String()).Replace(migrationText), nil
}
