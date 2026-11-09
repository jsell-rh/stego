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
	"stego.dev/runtime/tenant"
)

// Message is one outbox message that the handle enqueues for the relay destination. Its fields are those of the
// outbox runtime's message (ST-SUP-RP01-LC10), in the same order and of the same types, so the service's own code
// converts it to that message type in the Enqueue function it passes to New. The outbox package is not imported
// here: the handle needs only this consumer-side shape.
type Message struct {
	ID          uuid.UUID
	Destination string
	ResourceKey string
	Kind        string
	Payload     json.RawMessage
	OutgoingKey string
	Principal   string
	Origin      string
}

// Enqueue writes one message to the outbox inside tx, the caller's transaction, and never commits it. The service's
// own code passes the outbox runtime's enqueue as this function.
type Enqueue func(ctx context.Context, tx *sql.Tx, message Message) error

// MessageState is the dispatch state of an outbox message that a revocation status reads.
type MessageState string

const (
	// MessageParked is a message that waits for a person (ST-SUP-RP01-LC12).
	MessageParked MessageState = "parked"
	// MessageDiscarded is a message that a person discarded (ST-SUP-RP01-LC13).
	MessageDiscarded MessageState = "discarded"
)

// MessageStates reads the dispatch states of the outbox messages ids. A message that it does not name is in a state
// that is neither parked nor discarded. The service's own code passes a reader of the outbox runtime.
type MessageStates func(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]MessageState, error)

// Config is what the authorization runtime of a service needs.
type Config struct {
	// Provider is the decision provider, such as the SpiceDB provider. The handle never exposes it.
	Provider Decision
	// Floor gives the revocation floor, such as a TableFloor. Nil is a service with no relay.
	Floor Floor
	// DB is the database of the revocation tables, which Status reads in a transaction of its own.
	DB *sql.DB
	// Schema is the schema of the tables that Migration creates.
	Schema string
	// Enqueue writes the relay's outbox messages.
	Enqueue Enqueue
	// States reads the dispatch states of the outbox messages of a revocation.
	States MessageStates
	// Poll is the time between two reads of Wait. Zero is 50 milliseconds.
	Poll time.Duration
}

// Runtime is the authorization runtime of one service. It binds handles and reads revocation statuses.
type Runtime struct {
	port    *Port
	db      *sql.DB
	schema  string
	enqueue Enqueue
	states  MessageStates
	poll    time.Duration
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
	case cfg.Enqueue == nil:
		return nil, errors.New("authz: no outbox enqueue")
	case cfg.States == nil:
		return nil, errors.New("authz: no reader of the outbox message states")
	case cfg.Poll < 0:
		return nil, errors.New("authz: the poll time is negative")
	}
	schema, err := quoteName("schema", cfg.Schema)
	if err != nil {
		return nil, err
	}
	poll := cfg.Poll
	if poll == 0 {
		poll = 50 * time.Millisecond
	}
	return &Runtime{port: port, db: cfg.DB, schema: schema, enqueue: cfg.Enqueue, states: cfg.States, poll: poll}, nil
}

// Port returns the runtime's decision entry point, for a worker that decides with no transaction of its own, such
// as the outbox worker's Resolve (ST-SUP-RP01-LC17). It holds no relationships port.
func (r *Runtime) Port() *Port { return r.port }

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
	relation := Relation{ResourceType: resourceType, Name: name}
	if !ok {
		return Relation{}, fmt.Errorf("authz: the relation %q is not resource_type#relation", text)
	}
	if err := validType("resource", resourceType); err != nil {
		return Relation{}, err
	}
	if err := validType("relation", name); err != nil {
		return Relation{}, err
	}
	return relation, nil
}

// Handle is the authorization handle of one transaction, tenant and principal, which a slot fill gets
// (ST-SUP-RP06-LC09, ST-SUP-RP06-LC10). Its evaluations decide with the principal as subject at the floor: at least
// as fresh as the row token or the service's revocation floor, whichever is fresher. Its Relate and Unrelate only
// enqueue tuples for the relay in the transaction's outbox, for the relations it was bound with. It exposes no
// provider, no relationships port and no field.
type Handle struct {
	runtime    *Runtime
	tx         *sql.Tx
	tenant     tenant.Tenant
	principal  Principal
	allowed    map[Relation]bool
	revocation uuid.UUID
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

// floorOf returns the consistency of a check whose rows have the given tokens: at least as fresh as the freshest of
// them and the floor, or Fresh when none is known.
func floorOf(floor Token, tokens ...Token) Consistency {
	freshest := floor
	for _, token := range tokens {
		freshest = fresher(freshest, token)
	}
	if freshest.IsZero() {
		return Fresh()
	}
	return AtLeast(freshest)
}

// Evaluate decides action on resource, whose row holds rowToken, with the principal as subject, at least as fresh
// as the row token or the revocation floor, whichever is fresher (ST-SUP-RP06-LC09, ST-SUP-RP06-LC13). A deny is a
// result with Allowed false and a nil error.
func (h *Handle) Evaluate(ctx context.Context, action string, resource Resource, rowToken Token, c Context) (EvaluationResult, error) {
	floor, err := h.runtime.port.currentFloor(ctx)
	if err != nil {
		return EvaluationResult{}, err
	}
	return h.runtime.port.evaluate(ctx, EvaluationRequest{
		Subject:     h.principal.subject,
		Action:      Action{Name: action},
		Resource:    resource,
		Context:     c,
		Consistency: floorOf(floor, rowToken),
	}, floor)
}

// Row is a resource that a list reads with the token of its row.
type Row struct {
	Resource Resource
	Token    Token
}

// EvaluateBatch decides action on each row in one batch evaluation, at least as fresh as the freshest row token and
// the revocation floor, and returns one result per row.
func (h *Handle) EvaluateBatch(ctx context.Context, action string, rows []Row, c Context) ([]EvaluationResult, error) {
	floor, err := h.runtime.port.currentFloor(ctx)
	if err != nil {
		return nil, err
	}
	tokens := make([]Token, len(rows))
	for i, row := range rows {
		tokens[i] = row.Token
	}
	return h.batch(ctx, action, rows, c, floor, floorOf(floor, tokens...))
}

func (h *Handle) batch(ctx context.Context, action string, rows []Row, c Context, floor Token, consistency Consistency) ([]EvaluationResult, error) {
	evaluations := make([]EvaluationRequest, len(rows))
	for i, row := range rows {
		evaluations[i] = EvaluationRequest{Subject: h.principal.subject, Action: Action{Name: action},
			Resource: row.Resource, Context: c, Consistency: consistency}
	}
	results, err := h.runtime.port.evaluateBatch(ctx, BatchRequest{Evaluations: evaluations, Semantic: ExecuteAll}, floor)
	if err != nil {
		return nil, err
	}
	if len(results) != len(rows) {
		return nil, fmt.Errorf("authz: the provider answered %d of the %d evaluations of the batch", len(results), len(rows))
	}
	return results, nil
}

// Search returns one page of the resources of resourceType that the principal may do action on, at least as fresh
// as the revocation floor, or ErrNoSearch when the provider does not search.
func (h *Handle) Search(ctx context.Context, action, resourceType string, c Context, page Page) (SearchResult, error) {
	floor, err := h.runtime.port.currentFloor(ctx)
	if err != nil {
		return SearchResult{}, err
	}
	return h.runtime.port.search(ctx, SearchRequest{Subject: h.principal.subject, Action: Action{Name: action},
		ResourceType: resourceType, Context: c, Consistency: floorOf(floor), Page: page}, floor)
}

// List is a list of rows that a query pages through the database and filters by batch evaluation
// (ST-SUP-RP06-LC04). Its first page runs at least as fresh as its rows' tokens and the revocation floor; every
// later page runs at least as fresh as the fresher of the first page's token and the floor at the time of that
// page, so no later page is checked against an older state than the first. A list uses batch evaluation whether
// or not the provider searches.
type List struct {
	handle  *Handle
	action  string
	context Context
	first   Token
	started bool
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
	floor, err := l.handle.runtime.port.currentFloor(ctx)
	if err != nil {
		return nil, err
	}
	tokens := make([]Token, 0, len(rows)+1)
	tokens = append(tokens, l.first)
	for _, row := range rows {
		tokens = append(tokens, row.Token)
	}
	results, err := l.handle.batch(ctx, l.action, rows, l.context, floor, floorOf(floor, tokens...))
	if err != nil {
		return nil, err
	}
	if !l.started {
		l.started = true
		l.first = results[0].Token
	}
	var allowed []Row
	for i, result := range results {
		if result.Allowed {
			allowed = append(allowed, rows[i])
		}
	}
	return allowed, nil
}

// tenantDecision is the decision that tenant.Requested takes. Only DecideTenant makes one, after an allow.
type tenantDecision struct {
	seal.DecisionMark
}

func (tenantDecision) Allowed() bool { return true }

// DecideTenant decides the permission action of a Provision or Retire request on resource, as Evaluate does, at
// the floor (ST-SUP-RP06-LC17). On an allow it returns the decision that tenant.Requested takes, and true; on a
// deny it returns no decision and false. No package outside the runtime module can make the decision.
func (h *Handle) DecideTenant(ctx context.Context, action string, resource Resource, c Context) (tenant.Decision, bool, error) {
	result, err := h.Evaluate(ctx, action, resource, Token{}, c)
	if err != nil {
		return nil, false, err
	}
	if !result.Allowed {
		return nil, false, nil
	}
	return tenantDecision{}, true, nil
}

// Revocation names the one revocation of a transaction: every tuple that the transaction enqueues joins it, touch
// or delete (ST-SUP-RP06-LC12). Its identity is known before the commit.
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

func (h *Handle) write(ctx context.Context, operation Operation, tuple Tuple) (Revocation, error) {
	relation := Relation{ResourceType: tuple.Resource.Type, Name: tuple.Relation}
	if !h.allowed[relation] {
		return Revocation{}, fmt.Errorf("authz: the handle may not write the relation %s", relation)
	}
	if err := tuple.valid(); err != nil {
		return Revocation{}, err
	}
	r := h.runtime
	if h.revocation == uuid.Nil {
		id, err := uuid.NewRandom()
		if err != nil {
			return Revocation{}, fmt.Errorf("authz: make the revocation identity: %w", err)
		}
		if _, err := h.tx.ExecContext(ctx, "INSERT INTO "+r.schema+
			".authz_revocations (id, tenant_id, principal) VALUES ($1, $2, $3)",
			id, h.tenant.ID(), h.principal.subject.String()); err != nil {
			return Revocation{}, fmt.Errorf("authz: record the revocation: %w", err)
		}
		h.revocation = id
	}
	messageID, err := uuid.NewRandom()
	if err != nil {
		return Revocation{}, fmt.Errorf("authz: make the message identity: %w", err)
	}
	body, err := json.Marshal(payload{Version: payloadVersion, Operation: operation, Revocation: h.revocation,
		Tuple: tupleJSON{ResourceType: tuple.Resource.Type, ResourceID: tuple.Resource.ID, Relation: tuple.Relation,
			SubjectType: tuple.Subject.Type, SubjectID: tuple.Subject.ID, SubjectRelation: tuple.SubjectRelation}})
	if err != nil {
		return Revocation{}, fmt.Errorf("authz: encode the tuple: %w", err)
	}
	if _, err := h.tx.ExecContext(ctx, "INSERT INTO "+r.schema+
		".authz_revocation_tuples (message_id, revocation_id, tenant_id, operation, tuple) VALUES ($1, $2, $3, $4, $5)",
		messageID, h.revocation, h.tenant.ID(), string(operation), tuple.String()); err != nil {
		return Revocation{}, fmt.Errorf("authz: record the tuple of the revocation: %w", err)
	}
	if err := r.enqueue(ctx, h.tx, Message{
		ID:          messageID,
		Destination: DestinationName,
		ResourceKey: tuple.Resource.String(),
		Kind:        "authz." + string(operation),
		Payload:     body,
		OutgoingKey: messageID.String(),
		Principal:   h.principal.subject.String(),
	}); err != nil {
		return Revocation{}, fmt.Errorf("authz: enqueue the tuple for the relay: %w", err)
	}
	return Revocation{id: h.revocation}, nil
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
)

// RevocationStatus is the status of a revocation: its identity, its state and, when it is complete, the freshest
// token of its tuples.
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

// Status reads the revocation id for tenant t and principal p, in a read-only transaction of its own. It answers
// ErrRevocationNotFound to any other tenant and principal. The state is failed when a person discarded a tuple of
// the revocation, otherwise parked while a tuple is parked, otherwise pending until the relay landed every tuple,
// and complete with the freshest of their tokens when it did.
func (r *Runtime) Status(ctx context.Context, t tenant.Tenant, p Principal, id uuid.UUID) (RevocationStatus, error) {
	if p.IsZero() {
		return RevocationStatus{}, errors.New("authz: no principal")
	}
	tx, err := tenant.Begin(ctx, r.db, t, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return RevocationStatus{}, err
	}
	ids, tokens, err := r.readRevocation(ctx, tx, t, p, id)
	if err != nil {
		_ = tx.Rollback()
		return RevocationStatus{}, err
	}
	if err := tenant.Commit(ctx, tx, t); err != nil {
		return RevocationStatus{}, err
	}
	states, err := r.states(ctx, ids)
	if err != nil {
		return RevocationStatus{}, fmt.Errorf("authz: read the outbox states of the revocation: %w", err)
	}
	status := RevocationStatus{ID: id, State: RevocationComplete}
	parked, pending := false, false
	var freshest Token
	for i, messageID := range ids {
		switch states[messageID] {
		case MessageDiscarded:
			return RevocationStatus{ID: id, State: RevocationFailed}, nil
		case MessageParked:
			parked = true
		}
		if tokens[i].IsZero() {
			pending = true
		}
		freshest = fresher(freshest, tokens[i])
	}
	switch {
	case parked:
		return RevocationStatus{ID: id, State: RevocationParked}, nil
	case pending:
		return RevocationStatus{ID: id, State: RevocationPending}, nil
	}
	status.Token = freshest
	return status, nil
}

func (r *Runtime) readRevocation(ctx context.Context, tx *sql.Tx, t tenant.Tenant, p Principal, id uuid.UUID) ([]uuid.UUID, []Token, error) {
	var one int
	err := tx.QueryRowContext(ctx, "SELECT 1 FROM "+r.schema+
		".authz_revocations WHERE id = $1 AND tenant_id = $2 AND principal = $3", id, t.ID(), p.subject.String()).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrRevocationNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("authz: read the revocation: %w", err)
	}
	rows, err := tx.QueryContext(ctx, "SELECT message_id, token FROM "+r.schema+
		".authz_revocation_tuples WHERE revocation_id = $1 AND tenant_id = $2 ORDER BY message_id", id, t.ID())
	if err != nil {
		return nil, nil, fmt.Errorf("authz: read the tuples of the revocation: %w", err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	var tokens []Token
	for rows.Next() {
		var messageID uuid.UUID
		var text string
		if err := rows.Scan(&messageID, &text); err != nil {
			return nil, nil, fmt.Errorf("authz: read a tuple of the revocation: %w", err)
		}
		token, err := ParseToken(text)
		if err != nil {
			return nil, nil, err
		}
		ids = append(ids, messageID)
		tokens = append(tokens, token)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("authz: read the tuples of the revocation: %w", err)
	}
	return ids, tokens, nil
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
		return RevocationStatus{}, fmt.Errorf("authz: check that the transaction ended: %w", err)
	}
	r := h.runtime
	for {
		status, err := r.Status(ctx, h.tenant, h.principal, revocation.id)
		if err != nil {
			if ctx.Err() != nil {
				return RevocationStatus{ID: revocation.id, State: RevocationPending}, nil
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
// NUL or is not valid UTF-8.
func quoteName(kind, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("authz: no %s", kind)
	}
	if !cleanText(name) {
		return "", fmt.Errorf("authz: the %s holds a control character or is not valid UTF-8", kind)
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`, nil
}

// migrationText creates the revocation floor and the revocation tables. The floor holds one row and no tenant: the
// runtime role may read it and only the relay role may change it. The two revocation tables are tenant-owned, with
// row-level security enabled and forced on the transaction-local tenant setting.
const migrationText = `CREATE TABLE {{schema}}.authz_floor (
	only_row boolean PRIMARY KEY DEFAULT true CHECK (only_row),
	token text NOT NULL DEFAULT '',
	revision numeric CHECK (revision >= 0)
);
INSERT INTO {{schema}}.authz_floor (only_row) VALUES (true);
REVOKE ALL ON {{schema}}.authz_floor FROM PUBLIC;
GRANT SELECT ON {{schema}}.authz_floor TO {{runtime_role}}, {{relay_role}};
GRANT UPDATE (token, revision) ON {{schema}}.authz_floor TO {{relay_role}};

CREATE TABLE {{schema}}.authz_revocations (
	id uuid PRIMARY KEY,
	tenant_id text NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
	principal text NOT NULL CHECK (octet_length(principal) BETWEEN 1 AND 1200),
	created_at timestamptz NOT NULL DEFAULT pg_catalog.now()
);
ALTER TABLE {{schema}}.authz_revocations ENABLE ROW LEVEL SECURITY;
ALTER TABLE {{schema}}.authz_revocations FORCE ROW LEVEL SECURITY;
CREATE POLICY authz_revocations_tenant ON {{schema}}.authz_revocations
	USING (tenant_id = pg_catalog.current_setting('app.tenant', true))
	WITH CHECK (tenant_id = pg_catalog.current_setting('app.tenant', true));
REVOKE ALL ON {{schema}}.authz_revocations FROM PUBLIC;
GRANT SELECT, INSERT ON {{schema}}.authz_revocations TO {{runtime_role}};
GRANT SELECT ON {{schema}}.authz_revocations TO {{relay_role}};

CREATE TABLE {{schema}}.authz_revocation_tuples (
	message_id uuid PRIMARY KEY,
	revocation_id uuid NOT NULL REFERENCES {{schema}}.authz_revocations (id),
	tenant_id text NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
	operation text NOT NULL CHECK (operation IN ('touch', 'delete')),
	tuple text NOT NULL CHECK (octet_length(tuple) BETWEEN 1 AND 4096),
	token text NOT NULL DEFAULT '',
	revision numeric CHECK (revision >= 0),
	delivered_at timestamptz
);
CREATE INDEX authz_revocation_tuples_revocation ON {{schema}}.authz_revocation_tuples (revocation_id);
ALTER TABLE {{schema}}.authz_revocation_tuples ENABLE ROW LEVEL SECURITY;
ALTER TABLE {{schema}}.authz_revocation_tuples FORCE ROW LEVEL SECURITY;
CREATE POLICY authz_revocation_tuples_tenant ON {{schema}}.authz_revocation_tuples
	USING (tenant_id = pg_catalog.current_setting('app.tenant', true))
	WITH CHECK (tenant_id = pg_catalog.current_setting('app.tenant', true));
REVOKE ALL ON {{schema}}.authz_revocation_tuples FROM PUBLIC;
GRANT SELECT, INSERT ON {{schema}}.authz_revocation_tuples TO {{runtime_role}};
GRANT SELECT, UPDATE (token, revision, delivered_at) ON {{schema}}.authz_revocation_tuples TO {{relay_role}};
`

// Migration returns the SQL text that creates the revocation floor authz_floor and the revocation tables in schema,
// for the migration job to run as the owner role. runtimeRole is the role the service runs as; relayRole is the
// role of the relay, the only role that may raise the floor and store a tuple's token. The two may be the same.
func Migration(schema, runtimeRole, relayRole string) (string, error) {
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
	return strings.NewReplacer("{{schema}}", quotedSchema, "{{runtime_role}}", quotedRuntime,
		"{{relay_role}}", quotedRelay).Replace(migrationText), nil
}
