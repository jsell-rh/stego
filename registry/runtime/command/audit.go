package command

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/authz"
)

// ErrorCodeInternal is the fixed error code of a command that fails with an internal error.
const ErrorCodeInternal = "internal"

// actorKind is the audit actor kind of a principal: a service account is a service, any other subject a user.
func actorKind(p authz.Principal) audit.ActorKind {
	if p.Subject().Type == authz.ServiceAccountType {
		return audit.ActorService
	}
	return audit.ActorUser
}

// event is the audit event of one resource of the attempt, with the action of the command or the worker and the
// principal as the actor.
func (s *state) event(resourceType, id string) audit.Event {
	cfg := s.rt.cfg
	return audit.Event{
		TenantID: s.tenant.ID(), ActorID: s.principal.Subject().String(), ActorKind: actorKind(s.principal),
		ActorIP: audit.Untrusted(s.request.ActorIP), UserAgent: audit.Untrusted(s.request.UserAgent),
		Action: s.action, Operation: s.action, OnBehalfOf: s.onBehalfOf, ResourceType: resourceType, ResourceID: id,
		Outcome:   audit.OutcomeOK,
		RequestID: s.request.RequestID, TraceID: s.traceID, SpanID: s.spanID, IdempotencyKey: s.request.Key,
		Service: cfg.Service, ServiceVersion: cfg.ServiceVersion, DeclarationVersion: cfg.DeclarationVersion,
	}
}

// buffer keeps the audit event of one write, with its states redacted by field sensitivity, for the one batch that
// Run records just before COMMIT (ST-SUP-RP10-LC06).
func (s *state) buffer(resourceType string, fields audit.Fields, id string, before, after map[string]any) error {
	e := s.event(resourceType, id)
	if err := e.SetStates(s.rt.cfg.AuditSettings, fields, before, after); err != nil {
		return s.breakWith(internalf("the audit state of a row of %s cannot be encoded", resourceType))
	}
	s.events = append(s.events, e)
	return nil
}

// auditRead writes the audit row of a read of one resource of an entity with a pii field, in its own short
// transaction on the audit connections, before the read returns (ST-SUP-RP10-LC13, ST-SUP-RP11-LC08,
// ST-SUP-RP10-LC29). When the write fails, the read returns no result.
func (s *state) auditRead(ctx context.Context, pii bool, resourceType, id string) error {
	if !pii {
		return nil
	}
	if _, err := s.rt.cfg.Audit.RecordRead(ctx, s.rt.cfg.AuditDB, s.tenant, s.event(resourceType, id)); err != nil {
		return fmt.Errorf("command: the audit row of a read of %s was not written, so the read returns nothing: %w",
			resourceType, err)
	}
	return nil
}

// recordEvents writes the buffered events of the attempt with one call of the audit runtime's Record in the bound
// transaction: the last statement before COMMIT, so the chain lock is the last lock the attempt takes.
func (s *state) recordEvents(ctx context.Context) error {
	if len(s.events) == 0 {
		return nil
	}
	if err := s.setLockTimeout(ctx); err != nil {
		return s.fail(ctx, "audit record", err)
	}
	s.rt.stepped("record", s.tx)
	if _, err := s.rt.cfg.Audit.Record(ctx, s.tx, s.tenant, s.events); err != nil {
		return s.fail(ctx, "audit record", err)
	}
	return nil
}

// ErrorCode is the fixed audit error code of an error that Run, RunWorker or RunConsumer returns: conflict for a
// conflict after the last attempt, commit_outcome_unknown for a COMMIT whose outcome is not known, and internal for
// any other error. A declared refusal of the fill has its own declared code, which the generated code gives.
func ErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrCommitUnknown):
		return audit.ErrorCodeCommitOutcomeUnknown
	case errors.Is(err, ErrConflict):
		return audit.ErrorCodeConflict
	default:
		return ErrorCodeInternal
	}
}

// RecordOutcome writes the audit row of a denied or failed command in its own short transaction on the audit
// connections, after the command's transaction rolled back or when its COMMIT has an outcome that is not known
// (ST-SUP-RC32-OC07, ST-SUP-RP10-LC25): the action is the command's name and the actor the request's principal. When
// the audit runtime's RecordOutcome fails, it logs the event's action, resource type, outcome and error code, with no
// field value, counts the failure (ST-SUP-RP10-LC06) and returns the error.
func (rt *Runtime) RecordOutcome(ctx context.Context, req Request, action, resourceType, resourceID string,
	outcome audit.Outcome, code string) error {
	if rt == nil {
		return errors.New("command: no runtime")
	}
	cfg := rt.cfg
	span := trace.SpanContextFromContext(ctx)
	e := audit.Event{
		TenantID: req.Tenant.ID(), ActorID: req.Principal.Subject().String(), ActorKind: actorKind(req.Principal),
		ActorIP: audit.Untrusted(req.ActorIP), UserAgent: audit.Untrusted(req.UserAgent), Action: action, Operation: action,
		ResourceType: resourceType, ResourceID: resourceID, Outcome: outcome, ErrorCode: code,
		RequestID: req.RequestID, IdempotencyKey: req.Key,
		Service: cfg.Service, ServiceVersion: cfg.ServiceVersion, DeclarationVersion: cfg.DeclarationVersion,
	}
	if span.IsValid() {
		e.TraceID, e.SpanID = span.TraceID().String(), span.SpanID().String()
	}
	if _, err := cfg.Audit.RecordOutcome(ctx, cfg.AuditDB, req.Tenant, e); err != nil {
		rt.outcomeFailures.Add(1)
		rt.logOutcomeFailure(ctx, e)
		return err
	}
	return nil
}

// OutcomeFailures returns how many outcome rows RecordOutcome could not write.
func (rt *Runtime) OutcomeFailures() int64 { return rt.outcomeFailures.Load() }

// logOutcomeFailure logs the metadata of an outcome row that was not written: its action, resource type, outcome and
// error code, and no field value and no error text (ST-SUP-RP10-LC06).
func (rt *Runtime) logOutcomeFailure(ctx context.Context, e audit.Event) {
	if rt.logger == nil {
		return
	}
	var record otellog.Record
	record.SetSeverity(otellog.SeverityError)
	record.SetEventName("command.audit_outcome.failed")
	record.SetBody(attribute.StringValue("the audit row of a command outcome was not written"))
	record.AddAttributes(
		attribute.String("stego.action", e.Action),
		attribute.String("stego.resource_type", e.ResourceType),
		attribute.String("stego.outcome", string(e.Outcome)),
		attribute.String("stego.error_code", e.ErrorCode),
	)
	rt.logger.Emit(ctx, record)
}

// checkAuditPool checks the pool of the audit connections (ST-SUP-RP10-LC29): a pool of its own, apart from the
// main pool, that can hold at least two connections, so a read audit row of a fill that holds a main connection
// never waits for a main connection.
func checkAuditPool(main, auditDB *sql.DB) error {
	if auditDB == nil {
		return errors.New("command: the runtime needs the pool of the audit connections")
	}
	if auditDB == main {
		return errors.New("command: the audit connections need a pool of their own, apart from the main pool")
	}
	if n := auditDB.Stats().MaxOpenConnections; n != 0 && n < 2 {
		return fmt.Errorf("command: the pool of the audit connections holds at most %d connection; it needs at least two", n)
	}
	return nil
}
