package command

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"

	"stego.dev/runtime/audit"
	"stego.dev/runtime/authz"
	"stego.dev/runtime/idempotency"
	"stego.dev/runtime/otel"
	"stego.dev/runtime/outbox"
	"stego.dev/runtime/probe"
	"stego.dev/runtime/tenant"
)

// The timings of ST-SUP-RP10-LC25 and ST-SUP-RP10-LC28.
const (
	// DefaultLockTimeout is the lock timeout of a command that declares none, and of a worker.
	DefaultLockTimeout = 2 * time.Second
	// LockMargin is the time that a lock wait leaves before the deadline.
	LockMargin = 50 * time.Millisecond
	// RerunStep is the longest random wait before the first rerun; the n-th rerun waits at most n times it.
	RerunStep = 50 * time.Millisecond
	// RerunRoom is the least time left before the deadline after the wait, for a rerun to start.
	RerunRoom = 100 * time.Millisecond
)

// The operation span names of the command runtime, which the generated code registers when it starts the telemetry
// runtime: one span for each attempt of Run, each run of RunWorker and each message of RunConsumer.
const (
	AttemptSpanName  = "command.attempt"
	WorkerSpanName   = "command.worker"
	ConsumerSpanName = "command.consumer"
)

// SpanNames are the operation span names of the command runtime.
func SpanNames() []string { return []string{AttemptSpanName, WorkerSpanName, ConsumerSpanName} }

// ErrCommitUnknown is the error of a call whose COMMIT has an outcome that is not known, such as when the connection
// is lost during it or the deadline cancels it (ST-SUP-RP10-LC25, ST-SUP-RP10-LC28). It is an internal error: Run
// does not run the fill again, and the handler writes the outcome row commit_outcome_unknown.
var ErrCommitUnknown = &internalError{text: "the outcome of COMMIT is not known"}

// ErrDenied is the error of Run when the principal may not run the command on the resource that the request names:
// the authorization handle denied the command's declared permission on it (ST-SUP-RP10-LC31). Run did not call the
// fill and wrote no key row; the handler answers it with 403 and writes the outcome row denied.
var ErrDenied = errors.New("command: the principal may not run the command on its resource")

// errPanic is the error of a fill that panicked. It holds no part of the panic value (ST-SUP-RP10-LC12).
var errPanic = &internalError{text: "the fill panicked"}

// Probes binds a probe call of a fill: the probe runtime or, in a fill test, its Recorder.
type Probes interface {
	Bind(ctx context.Context, tenant, principal, operation string) *probe.Call
}

// Config is what the command runtime of a service needs.
type Config struct {
	// DB is the main pool, which tenant.Open returned (ST-SUP-RP10-LC30).
	DB *sql.DB
	// AuditDB is the pool of the audit connections, apart from DB, which only the short audit transactions use
	// (ST-SUP-RP10-LC29). tenant.Open returned it too.
	AuditDB *sql.DB
	// Authz binds the authorization handle of each transaction (ST-SUP-RP10-LC04).
	Authz *authz.Runtime
	// Audit writes the audit rows (ST-SUP-RP10-LC06), and AuditSettings are the service's audit settings.
	Audit         *audit.Writer
	AuditSettings audit.Settings
	// Outbox is the outbox queue of the runtime role, in which the handle stages its effects (ST-SUP-RP10-LC07).
	Outbox *outbox.Queue
	// Probes binds the probe call of each attempt (ST-SUP-RP10-LC14).
	Probes Probes
	// Entities are the entities of the service. FromRequest makes a loaded value only of one of them
	// (ST-SUP-RP10-LC32).
	Entities []Declared
	// Telemetry starts the operation spans of SpanNames, or nil for none.
	Telemetry *otel.Runtime
	// Logger gives the log of an outcome row that was not written (ST-SUP-RP10-LC06). Nil logs nothing.
	Logger otellog.LoggerProvider
	// Service, ServiceVersion and DeclarationVersion name the service in its audit rows.
	Service, ServiceVersion, DeclarationVersion string
}

// Runtime is the command runtime of one service.
type Runtime struct {
	cfg    Config
	logger otellog.Logger
	// slots is set only by New: a Runtime that New did not make, such as new(Runtime), gives a slot key that
	// FromRequest refuses (ST-SUP-RP10-LC32).
	slots           *slotSeal
	outcomeFailures atomic.Int64
	// recorder and beforeCommit are set only by the tests of the package: the statement recorder, and a step just
	// before COMMIT.
	recorder     func(query string, args []any)
	beforeCommit func(ctx context.Context, tx *sql.Tx) error
	// rerun, when set by a test, gets the random wait before each rerun and the time left after it.
	rerun func(n int, wait, left time.Duration)
	// step, when set by a test, runs right after lock_timeout is set before a statement of the wrapper, the enqueue,
	// the audit record and COMMIT.
	step func(name string, tx *sql.Tx)
}

// New checks cfg and returns the command runtime. It refuses a configuration that lacks a part, a main pool or an
// audit pool that does not close a connection that comes back with a tenant set, as a pool that tenant.Open
// returned does, which tenant.Opened tells (ST-SUP-RP10-LC30, ST-SUP-RP04-LC08), an audit pool that is the main pool or holds fewer than two connections
// (ST-SUP-RP10-LC29), and a telemetry runtime that does not register the names of SpanNames.
func New(ctx context.Context, cfg Config) (*Runtime, error) {
	switch {
	case cfg.DB == nil:
		return nil, errors.New("command: the runtime needs the main pool")
	case cfg.Authz == nil || cfg.Audit == nil || cfg.Outbox == nil || cfg.Probes == nil:
		return nil, errors.New("command: the runtime needs the authorization runtime, the audit writer, the outbox queue and the probes")
	case cfg.Service == "":
		return nil, errors.New("command: the runtime needs the service name")
	}
	if err := checkAuditPool(cfg.DB, cfg.AuditDB); err != nil {
		return nil, err
	}
	if !tenant.Opened(cfg.DB) || !tenant.Opened(cfg.AuditDB) {
		return nil, errors.New("command: the main pool and the audit pool must be pools that tenant.Open returned")
	}
	if cfg.Telemetry != nil {
		for _, name := range SpanNames() {
			if !cfg.Telemetry.Registered(name) {
				return nil, fmt.Errorf("command: the telemetry runtime does not register the span name %s", name)
			}
		}
	}
	if cfg.AuditSettings.Level == "" {
		cfg.AuditSettings.Level = audit.LevelChange
	}
	rt := &Runtime{cfg: cfg, slots: &slotSeal{entities: map[Declared]bool{}}}
	for _, e := range cfg.Entities {
		if e == nil {
			return nil, errors.New("command: the runtime declares a nil entity")
		}
		if err := e.register(rt); err != nil {
			return nil, err
		}
		rt.slots.entities[e] = true
	}
	if cfg.Logger != nil {
		rt.logger = cfg.Logger.Logger("stego.dev/runtime/command")
	}
	return rt, nil
}

func (rt *Runtime) stepped(name string, tx *sql.Tx) {
	if rt.step != nil {
		rt.step(name, tx)
	}
}

func (rt *Runtime) record(query string, args []any) {
	if rt.recorder != nil {
		rt.recorder(query, args)
	}
}

// span starts the operation span name of one attempt or run, or none without a telemetry runtime.
func (rt *Runtime) span(ctx context.Context, name string) (context.Context, otel.Finish) {
	if rt.cfg.Telemetry == nil {
		return ctx, func(otel.Outcome, bool, bool) {}
	}
	spanCtx, finish, err := rt.cfg.Telemetry.StartOperation(ctx, otel.Child, name)
	if err != nil {
		return ctx, func(otel.Outcome, bool, bool) {}
	}
	return spanCtx, finish
}

// CommandSpec is what the generated code declares of one command.
type CommandSpec[P any] struct {
	// Name is the command's name: the action of its audit rows and the operation of its probe.
	Name string
	// Writes are the entities that the command declares it writes (ST-SUP-RP10-LC02).
	Writes []Declared
	// Destinations are the outbox destinations of its effects (ST-SUP-RP10-LC07).
	Destinations []Destination
	// Relations are the relations whose tuples its fill may write (ST-SUP-RP06-LC10).
	Relations []authz.Relation
	// RetryOnConflict is the declared Retry on conflict: the most reruns after a conflict (ST-DECL-EN18).
	RetryOnConflict int
	// LockTimeout is the declared Lock timeout, or zero for DefaultLockTimeout (ST-SUP-RP10-LC28).
	LockTimeout time.Duration
	// Probe wraps the probe of one call in the command's typed probe (ST-SUP-RP10-LC14).
	Probe func(*probe.Probe) P
	// Permission is the command's declared permission, which Run decides on Resource before the fill
	// (ST-SUP-RP10-LC31). A command with a permission declares its resource, and one without declares neither.
	Permission string
	// Resource is the resource on which Run decides Permission: for a command that creates, the parent that the
	// new resource goes under.
	Resource *Resource
}

// Resource is the generated declaration of the resource of a command (ST-SUP-RP10-LC31): its resource type at the
// provider, the statement that reads the authorization token of its row, and the check of an ID.
type Resource struct {
	// Type is the resource type at the provider, such as order.
	Type string
	// Token reads the authz_token of the resource row: the tenant is $1 and the ID $2. It gives one text column, or
	// no row when the tenant holds no such row.
	Token string
	// ValidID tells whether id is a key of the resource, such as a uuid.
	ValidID func(id string) bool
}

// Command is one checked command declaration.
type Command[P any] struct {
	spec         CommandSpec[P]
	writes       map[Declared]bool
	destinations map[string]Destination
}

// NewCommand checks the declaration of one command.
func NewCommand[P any](spec CommandSpec[P]) (*Command[P], error) {
	if spec.Name == "" || spec.Probe == nil {
		return nil, errors.New("command: a command needs a name and its probe")
	}
	if spec.RetryOnConflict < 0 || spec.LockTimeout < 0 {
		return nil, fmt.Errorf("command: %s declares a negative Retry on conflict or Lock timeout", spec.Name)
	}
	writes, err := checkWrites(spec.Name, spec.Writes)
	if err != nil {
		return nil, err
	}
	destinations, err := checkDestinations(spec.Name, spec.Destinations)
	if err != nil {
		return nil, err
	}
	if err := checkResource(spec.Name, spec.Permission, spec.Resource); err != nil {
		return nil, err
	}
	return &Command[P]{spec: spec, writes: writes, destinations: destinations}, nil
}

func checkWrites(owner string, declared []Declared) (map[Declared]bool, error) {
	writes := make(map[Declared]bool, len(declared))
	for _, e := range declared {
		if e == nil {
			return nil, fmt.Errorf("command: %s declares a nil entity", owner)
		}
		writes[e] = true
	}
	return writes, nil
}

// checkResource refuses a permission without its resource, a resource without a permission, and a resource that
// lacks a part. Its token statement, with its comments removed, must be one SELECT with no subselect, take exactly
// the placeholders $1, the tenant, and $2, the ID, so $12 does not pass for $1, and lock the row it reads FOR SHARE,
// also for the parent of a create, so the revocation's UPDATE of the row's token waits until the command's
// transaction ends (ST-SUP-RP10-LC31). FOR KEY SHARE does not block that UPDATE and is refused.
func checkResource(owner, permission string, r *Resource) error {
	switch {
	case permission == "" && r == nil:
		return nil
	case permission == "" || r == nil:
		return fmt.Errorf("command: %s declares a permission without its resource, or a resource without a permission", owner)
	case r.Type == "" || r.ValidID == nil:
		return fmt.Errorf("command: the resource of %s needs its type and ValidID", owner)
	}
	statement := stripComments(r.Token)
	if len(selectWord.FindAllString(statement, -1)) != 1 {
		return fmt.Errorf("command: the token statement of the resource of %s is not one SELECT with no subselect", owner)
	}
	seen := map[string]bool{}
	for _, m := range placeholder.FindAllStringSubmatch(statement, -1) {
		seen[m[1]] = true
	}
	if len(seen) != 2 || !seen["1"] || !seen["2"] {
		return fmt.Errorf("command: the token statement of the resource of %s does not take exactly the tenant as $1 and the ID as $2", owner)
	}
	if !shareLock.MatchString(statement) {
		return fmt.Errorf("command: the token statement of the resource of %s does not lock the row FOR SHARE", owner)
	}
	return nil
}

// placeholder matches one placeholder of a statement, selectWord the word SELECT, and shareLock a statement that
// ends with FOR SHARE. FOR KEY SHARE does not block the UPDATE of a token, so it does not count.
var (
	placeholder  = regexp.MustCompile(`\$([0-9]+)`)
	selectWord   = regexp.MustCompile(`(?i)\bSELECT\b`)
	shareLock    = regexp.MustCompile(`(?i)\bFOR\s+SHARE\s*;?\s*$`)
	lineComment  = regexp.MustCompile(`--[^\n]*`)
	blockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
)

// stripComments removes the comments of a statement, so a comment cannot look like a lock clause or a placeholder.
func stripComments(statement string) string {
	return lineComment.ReplaceAllString(blockComment.ReplaceAllString(statement, " "), " ")
}

// Request is what Run knows of one command request, from the generated handler.
type Request struct {
	// Tenant is the tenant of the verified identity, and Principal the principal that the handler decided.
	Tenant    tenant.Tenant
	Principal authz.Principal
	// Key is the inbound idempotency key, and Fingerprint the request's fingerprint (ST-SUP-RP07-LC05).
	Key         string
	Fingerprint string
	// Body is the request body, which Run decodes again for each attempt (ST-SUP-RP10-LC25).
	Body []byte
	// Resource is the ID of the resource that the request names, on which Run decides the command's permission
	// (ST-SUP-RP10-LC31). A command with no permission ignores it.
	Resource string
	// RequestID, ActorIP and UserAgent go to the audit rows.
	RequestID          string
	ActorIP, UserAgent string
	Retention          time.Duration
}

// Result is the answer of Run.
type Result struct {
	// Response is the response to send; when Problem is not empty, only its status is set.
	Response idempotency.Response
	// Problem is idempotency.ProblemInProgress or idempotency.ProblemReused when the wrapper answered without the
	// fill.
	Problem string
	// Replayed is true when Response is the stored response of a completed key.
	Replayed bool
	// Attempts is the number of calls of the fill.
	Attempts int
}

// Fill is a command fill as the generated code adapts it: it gets the decoded request and the handle, and gives
// the response to send.
type Fill[Req, P any] func(ctx context.Context, request Req, h *Handle[P]) (idempotency.Response, error)

// Run calls fill for the request req of the command cmd, inside the idempotency wrapper, in a transaction that it
// opens with tenant.Begin and ends with tenant.Commit or a rollback (ST-SUP-RP10-LC09, ST-SUP-RP10-LC11,
// ST-SUP-RP10-LC30). Each attempt decodes a copy of the body again with decode, binds a new handle, and calls the
// fill once; after the fill it enqueues the staged effects and records the buffered audit events as one batch, and
// commits only when the fill returns no error and met no conflict (ST-SUP-RP10-LC06, ST-SUP-RP10-LC07). A conflict
// rolls the attempt back and runs the fill again, after a random wait, while fewer reruns than the command's Retry
// on conflict have run and the deadline leaves room (ST-SUP-RP10-LC25, ST-SUP-RP10-LC28).
//
// Before the idempotency wrapper, and so before the fill and before any key row, each attempt decides the command's
// declared permission on the resource that the request names through the bound authorization handle, at the floor
// of ST-SUP-RP06-LC09 and the row's token, which it reads in the attempt's transaction; a deny ends Run with
// ErrDenied (ST-SUP-RP10-LC31).
//
// Run returns the fill's own error as it is, ErrDenied for a deny, ErrConflict after the last attempt, ErrCommitUnknown when the outcome of
// COMMIT is not known, and an internal error for a panic, which carries no part of the panic value
// (ST-SUP-RP10-LC12), and for every other fault.
func Run[Req, P any](ctx context.Context, rt *Runtime, cmd *Command[P], req Request,
	decode func(body []byte) (Req, error), fill Fill[Req, P]) (Result, error) {
	if rt == nil || cmd == nil || decode == nil || fill == nil {
		return Result{}, errors.New("command: Run needs the runtime, the command, the decoder and the fill")
	}
	if req.Tenant.IsZero() || req.Principal.IsZero() {
		return Result{}, errors.New("command: Run needs the tenant and the principal")
	}
	if req.RequestID == "" {
		req.RequestID = uuid.NewString()
	}
	for reruns := 0; ; reruns++ {
		result, err, conflict := runAttempt(ctx, rt, cmd, req, reruns, decode, fill)
		result.Attempts = reruns + 1
		if !conflict || reruns >= cmd.spec.RetryOnConflict || !waitRerun(ctx, rt, reruns+1) {
			return result, err
		}
	}
}

// waitRerun waits a random time of at most n times RerunStep before the n-th rerun, capped so at least RerunRoom is
// left before the deadline, and reports whether the rerun may start: it may not when less than RerunRoom is left
// after the wait (ST-SUP-RP10-LC28).
func waitRerun(ctx context.Context, rt *Runtime, n int) bool {
	wait := rand.N(time.Duration(n)*RerunStep + 1)
	deadline, hasDeadline := ctx.Deadline()
	if hasDeadline {
		room := time.Until(deadline) - RerunRoom
		if room <= 0 {
			return false
		}
		wait = min(wait, room)
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
	}
	if rt.rerun != nil {
		left := time.Duration(-1)
		if hasDeadline {
			left = time.Until(deadline)
		}
		rt.rerun(n, wait, left)
	}
	return !hasDeadline || time.Until(deadline) >= RerunRoom
}

// fillFailure is the error of the wrapper's handler, which Wrap returns as it is: the outcome of the attempt.
type fillFailure struct {
	err      error
	conflict bool
}

func (f *fillFailure) Error() string { return f.err.Error() }

// runAttempt runs one attempt in a transaction of its own and reports whether it ended in a conflict that a rerun
// may change.
func runAttempt[Req, P any](ctx context.Context, rt *Runtime, cmd *Command[P], req Request, reruns int,
	decode func([]byte) (Req, error), fill Fill[Req, P]) (result Result, err error, conflict bool) {
	ctx, finish := rt.span(ctx, AttemptSpanName)
	outcome := otel.OutcomeRolledBack
	defer func() { finish(outcome, reruns > 0, err != nil && !conflict) }()

	tx, err := tenant.Begin(ctx, rt.cfg.DB, req.Tenant, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		if isConflict(ctx, err) {
			return Result{}, &conflictError{text: "the transaction did not begin before the deadline"}, true
		}
		return Result{}, internalf("the transaction did not begin"), false
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
			// The attempt's audit record rolled back with it, and it holds no chain lock any more, so a read of a
			// rerun may write its own read row.
			rt.cfg.Audit.Forget(req.RequestID)
		}
	}()
	s := newState(ctx, rt, tx, req.Tenant, req.Principal, modeCommand, cmd.spec.Name, cmd.writes, cmd.destinations)
	s.request = req
	s.commandPermission = cmd.spec.Permission
	s.lockTimeout = cmd.spec.LockTimeout
	if s.lockTimeout == 0 {
		s.lockTimeout = DefaultLockTimeout
	}
	s.inbound = commandInbound(req.Key)
	// The first lock_timeout comes right after the tenant setting and before the key insert (ST-SUP-RP10-LC28).
	if err := s.setLockTimeout(ctx); err != nil {
		if isConflict(ctx, err) {
			return Result{}, s.fail(ctx, "lock timeout", err), true
		}
		return Result{}, internalf("the lock timeout was not set"), false
	}
	if err := s.checkIsolation(ctx); err != nil {
		return Result{}, err, false
	}
	if s.az, err = rt.cfg.Authz.Bind(tx, req.Tenant, req.Principal, cmd.spec.Relations); err != nil {
		return Result{}, internalf("the authorization handle was not bound"), false
	}
	if err := s.decideCommand(ctx, cmd.spec.Permission, cmd.spec.Resource, req.Resource); err != nil {
		return Result{}, err, errors.Is(err, ErrConflict)
	}
	call := rt.cfg.Probes.Bind(ctx, req.Tenant.ID(), req.Principal.Subject().String(), cmd.spec.Name)
	defer call.Rollback()
	h := &Handle[P]{st: s, probe: cmd.spec.Probe(call.Probe())}

	wrapped, err := idempotency.Wrap(ctx, wrapTx{s: s}, idempotency.Request{Tenant: req.Tenant.ID(),
		Principal: req.Principal.Subject().String(), Key: req.Key, Fingerprint: req.Fingerprint,
		Retention: req.Retention}, func(ctx context.Context) (idempotency.Response, error) {
		request, err := decode(bytes.Clone(req.Body))
		if err != nil {
			s.close()
			call.Returned()
			return idempotency.Response{}, &fillFailure{err: err}
		}
		response, err := callFill(ctx, s, call, func(ctx context.Context) (idempotency.Response, error) {
			return fill(ctx, request, h)
		})
		if err != nil {
			return idempotency.Response{}, err
		}
		return response, nil
	})
	var failure *fillFailure
	switch {
	case errors.As(err, &failure):
		return Result{}, failure.err, failure.conflict
	case err != nil:
		// A statement of the wrapper itself failed: an internal error, never a conflict (ST-SUP-RP10-LC25).
		return Result{}, internalf("the idempotency wrapper failed"), false
	case wrapped.Problem != "" || wrapped.Replayed:
		return Result{Response: wrapped.Response, Problem: wrapped.Problem, Replayed: wrapped.Replayed}, nil, false
	case !wrapped.Commit:
		// A transient response of the fill: the wrapper stored nothing, and the attempt rolls back.
		return Result{Response: wrapped.Response}, nil, false
	}
	if err := commit(ctx, s); err != nil {
		return Result{}, err, errors.Is(err, ErrConflict)
	}
	committed = true
	outcome = otel.OutcomeCommitted
	call.Commit()
	return Result{Response: wrapped.Response}, nil, false
}

// decideCommand decides the command's declared permission on the resource id with the attempt's authorization
// handle, before the idempotency wrapper (ST-SUP-RP10-LC31). It reads and share-locks the resource row through the
// bound transaction FOR SHARE, after lock_timeout, so no revocation can change its token before the commit, and decides at
// the revocation floor with the same consistency whether or not the row exists, so a missing row and a denied one
// cost the same calls, take the same time and give the same answer. An ID that is not a key of the resource is
// denied without a statement. A command with no permission decides nothing.
func (s *state) decideCommand(ctx context.Context, permission string, r *Resource, id string) error {
	if r == nil {
		return nil
	}
	if id == "" || !r.ValidID(id) {
		return ErrDenied
	}
	if err := s.setLockTimeout(ctx); err != nil {
		return s.fail(ctx, "lock timeout", err)
	}
	var token sql.NullString
	err := s.queryRow(ctx, r.Token, s.tenant.ID(), id).Scan(&token)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return s.fail(ctx, "read the token of the resource", err)
	}
	// The row's token is not given to the decision: a missing row has none, and the floor alone gives one
	// consistency for both. While the floor is zero, the decision is fresh, which is at least as fresh as any
	// token of the row.
	_ = token
	allowed, err := s.evaluate(ctx, s.az, permission, r.Type, id, "", nil)
	switch {
	case errors.Is(err, ErrNotFound):
		// The ID makes no resource ID at the provider (ST-SUP-RP06-LC22): no principal may act on it.
		return ErrDenied
	case errors.Is(err, ErrConflict) || errors.Is(err, ErrInternal):
		return err
	case err != nil:
		return internalf("the permission of the command was not decided")
	}
	if !allowed {
		return ErrDenied
	}
	return nil
}

// callFill calls fill, ends the handle and the probe call when it returns or panics, and then, when the call met no
// conflict and no fault, enqueues the staged effects and records the audit events. It gives the outcome of the call
// as a *fillFailure.
func callFill[R any](ctx context.Context, s *state, call *probe.Call, fill func(context.Context) (R, error)) (R, error) {
	var zero R
	ended := false
	defer func() {
		if !ended {
			// The fill ended its goroutine with runtime.Goexit, as t.FailNow does, which recover does not see: the
			// handle still closes, and the attempt's deferred rollback runs as for a panic.
			s.close()
			call.Returned()
		}
	}()
	// The fill runs on a goroutine of its own, so a fill that ends through runtime.Goexit, which recover does not
	// see, ends only that goroutine and counts as a panic (ST-SUP-RP10-LC12).
	type outcome struct {
		result   R
		err      error
		panicked bool
	}
	done := make(chan outcome, 1)
	go func() {
		o := outcome{panicked: true}
		defer func() {
			_ = recover()
			done <- o
		}()
		r, err := fill(ctx)
		o = outcome{result: r, err: err}
	}()
	o := <-done
	result, err, panicked := o.result, o.err, o.panicked
	ended = true
	s.close()
	call.Returned()
	switch {
	case panicked:
		return zero, &fillFailure{err: errPanic}
	case s.conflict != nil:
		// A fill that ignores a conflict cannot commit (ST-SUP-RP10-LC25).
		return zero, &fillFailure{err: s.conflict, conflict: true}
	case s.broken != nil:
		return zero, &fillFailure{err: s.broken}
	case err != nil:
		return zero, &fillFailure{err: err, conflict: errors.Is(err, ErrConflict)}
	}
	if err := s.enqueue(ctx); err != nil {
		return zero, &fillFailure{err: err, conflict: errors.Is(err, ErrConflict)}
	}
	if err := s.recordEvents(ctx); err != nil {
		return zero, &fillFailure{err: err, conflict: errors.Is(err, ErrConflict)}
	}
	return result, nil
}

// commit commits the attempt's transaction with tenant.Commit (ST-SUP-RP10-LC30) after it sets lock_timeout for the
// statements of COMMIT (ST-SUP-RP10-LC28). A conflict SQLSTATE from COMMIT is a conflict; a context error, a lost
// connection or any error with no SQLSTATE is an outcome that is not known (ST-SUP-RP10-LC25); any other error is an
// internal error after a rollback. An error of tenant.Commit before COMMIT is sent is a conflict when it is a
// conflict or a cancel, and an internal error otherwise: the transaction did not commit. A run whose context ended
// does not commit: database/sql refuses COMMIT on a transaction whose context is done.
func commit(ctx context.Context, s *state) error {
	if s.rt.beforeCommit != nil {
		if err := s.rt.beforeCommit(ctx, s.tx); err != nil {
			return internalf("the step before COMMIT failed")
		}
	}
	if err := s.setLockTimeout(ctx); err != nil {
		if isConflict(ctx, err) {
			return s.fail(ctx, "lock timeout", err)
		}
		return internalf("the lock timeout was not set before COMMIT")
	}
	s.rt.stepped("commit", s.tx)
	err := tenant.Commit(ctx, s.tx, s.tenant)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, tenant.ErrTenantChanged):
		return internalf("the tenant setting of the transaction changed, so it was rolled back")
	case errors.Is(err, tenant.ErrNotCommitted) && isConflict(ctx, err):
		return &conflictError{text: fmt.Sprintf("the step before COMMIT ended with SQLSTATE %q or the deadline", sqlState(err))}
	case errors.Is(err, tenant.ErrNotCommitted):
		return internalf("the step before COMMIT failed with SQLSTATE %q", sqlState(err))
	case conflictStates[sqlState(err)]:
		return &conflictError{text: fmt.Sprintf("COMMIT ended with SQLSTATE %q", sqlState(err))}
	case commitKnown(sqlState(err)):
		return internalf("COMMIT ended with SQLSTATE %q", sqlState(err))
	default:
		return ErrCommitUnknown
	}
}

// commitKnown reports whether a COMMIT that failed with the SQLSTATE code surely did not take effect: the server
// answered with an error of the transaction, not of the connection, the server or the system.
func commitKnown(code string) bool {
	if code == "" || code == "40003" {
		return false
	}
	switch code[:2] {
	case "08", "53", "57", "58", "XX":
		return false
	}
	return true
}

// checkIsolation refuses a transaction that does not run at READ COMMITTED: a locked read must see the row that
// another transaction committed after this one began, and the idempotency wrapper expects it (ST-SUP-RP07-LC03).
func (s *state) checkIsolation(ctx context.Context) error {
	var level string
	if err := s.queryRow(ctx, `SELECT pg_catalog.current_setting('transaction_isolation')`).Scan(&level); err != nil {
		return s.fail(ctx, "isolation check", err)
	}
	if level != "read committed" {
		return s.breakWith(internalf("the transaction runs at %s, not at read committed", level))
	}
	return nil
}

// errDeadline is the error of a worker run that reached its deadline under a lease: it rolls back, and the tenant
// runs again in the next tick (ST-SUP-RP10-LC22).
var errDeadline = &conflictError{text: "the run reached its deadline before the lease can expire"}

// newState makes the state of one attempt.
func newState(ctx context.Context, rt *Runtime, tx *sql.Tx, t tenant.Tenant, p authz.Principal, m mode, action string,
	writes map[Declared]bool, destinations map[string]Destination) *state {
	s := &state{rt: rt, tx: tx, tenant: t, principal: p, mode: m, action: action, writes: writes,
		destinations: destinations, stagedK: map[string]bool{}, written: map[rowKey]int64{}}
	if deadline, ok := ctx.Deadline(); ok {
		s.deadline = deadline
	}
	if span := trace.SpanContextFromContext(ctx); span.IsValid() {
		s.traceID, s.spanID = span.TraceID().String(), span.SpanID().String()
	}
	return s
}

// wrapTx is the transaction that Run gives the idempotency wrapper: it sets lock_timeout before each statement of
// the wrapper (ST-SUP-RP10-LC28), and it cannot end the transaction.
type wrapTx struct {
	s *state
}

func (w wrapTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if err := w.s.setLockTimeout(ctx); err != nil {
		return nil, err
	}
	w.s.rt.stepped("wrapper", w.s.tx)
	return w.s.exec(ctx, query, args...)
}

func (w wrapTx) QueryRowContext(ctx context.Context, query string, args ...any) idempotency.Row {
	if err := w.s.setLockTimeout(ctx); err != nil {
		return errRow{err: err}
	}
	w.s.rt.stepped("wrapper", w.s.tx)
	return w.s.queryRow(ctx, query, args...)
}

func (w wrapTx) Rollback() error {
	return errors.New("command: the idempotency wrapper does not end the transaction")
}

// errRow is a row whose statement did not run.
type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

// statusOK is the status of a consumed message's stored response.
const statusOK = http.StatusOK
