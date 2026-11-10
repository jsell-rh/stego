// Package auditoutbox joins the audit runtime (ST-SUP-RP05) to the outbox runtime (ST-SUP-RP01): the adapter that
// enqueues each audit sink message on the outbox queue in the caller's transaction, and the outbox destination of
// the audit sink, in the form of authz.Relay.Destination, so no generated service writes either (ST-SUP-RP05-LC14).
// It is a package apart from audit because the outbox runtime's own tests use the audit Writer.
//
// It is a library, not a framework (ST-STD-RUNTIME-R01): generated code calls its functions and types, and the
// package calls back into no generated code and owns no main loop.
package auditoutbox

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"

	"github.com/google/uuid"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// Kind is the kind of each outbox message of the audit sink.
const Kind = "audit.sink"

// maxKey is the longest resource key and outgoing key that an audit sink message carries as it is; a longer key,
// such as an anchor key of a long tenant ID, is carried as its SHA-256.
const maxKey = 255

// Queue is the writer-to-outbox adapter: the audit.Enqueuer that the Writer, the Anchorer and the Maintainer take,
// on the outbox queue of github.com/jsell-rh/stego/registry/runtime/outbox. Each sink message goes in the caller's transaction as a message
// of the destination audit-sink that is idempotent and must land, with the key of the event or the anchor as its
// outgoing key.
type Queue struct {
	queue *outbox.Queue
}

var _ audit.Enqueuer = (*Queue)(nil)

// PayloadLimits are the payload limits that the outbox queue of the adapter takes: audit-sink up to
// audit.MaxSinkMessage. The generated wiring gives them as outbox.Options.PayloadLimits.
func PayloadLimits() map[string]int {
	return map[string]int{audit.SinkDestination: audit.MaxSinkMessage}
}

// NewQueue returns the adapter of the outbox queue q. It refuses a queue whose payload limit for audit-sink is
// below audit.MaxSinkMessage, so an event that the Writer takes never fails at the enqueue.
func NewQueue(q *outbox.Queue) (*Queue, error) {
	if q == nil {
		return nil, errors.New("auditoutbox: the adapter needs the outbox queue")
	}
	if q.PayloadLimit(audit.SinkDestination) < audit.MaxSinkMessage {
		return nil, errors.New("auditoutbox: the outbox queue does not take audit sink messages of 1 MiB; give it PayloadLimits")
	}
	return &Queue{queue: q}, nil
}

// Enqueue writes one sink message in tx, which must be the *sql.Tx that tenant.Begin opened for the tenant tenantID:
// the outbox writes the transaction's tenant on the row and refuses a transaction with none. The message ID is the
// event ID for an event, so one event is one message; an anchor gets a new ID.
func (q *Queue) Enqueue(ctx context.Context, tx audit.Tx, tenantID, destination, key string, payload []byte) error {
	sqlTx, ok := tx.(*sql.Tx)
	if !ok {
		return errors.New("auditoutbox: the adapter needs the *sql.Tx of the caller")
	}
	if tenantID == "" || destination != audit.SinkDestination || key == "" {
		return errors.New("auditoutbox: the adapter takes only an audit sink message with its tenant and key")
	}
	id, err := uuid.Parse(key)
	if err != nil {
		id = uuid.New()
	}
	bounded := key
	if len(bounded) > maxKey {
		sum := sha256.Sum256([]byte(key))
		bounded = "sha256:" + hex.EncodeToString(sum[:])
	}
	return q.queue.Enqueue(ctx, sqlTx, outbox.Message{ID: id, Destination: audit.SinkDestination, ResourceKey: bounded,
		Kind: Kind, Payload: payload, OutgoingKey: bounded, Idempotent: true, MustLand: true})
}

// Sink is the outbox destination of the audit sink.
type Sink struct {
	destination *audit.Destination
}

// NewSink returns the outbox destination that delivers to the sink of d.
func NewSink(d *audit.Destination) (*Sink, error) {
	if d == nil {
		return nil, errors.New("auditoutbox: the destination needs the audit sink destination")
	}
	return &Sink{destination: d}, nil
}

// Destination returns the registration of the audit sink on the outbox worker, in the form of
// authz.Relay.Destination: the destination audit-sink, idempotent and must land, with Deliver as its handler.
func (s *Sink) Destination() outbox.Destination {
	return outbox.Destination{Name: audit.SinkDestination, Idempotent: true, MustLand: true, Handler: s.Deliver}
}

// Deliver is the outbox handler of the audit sink. It refuses for good, so the outbox parks the message, a payload
// that audit.Destination cannot read and one that names a tenant other than the tenant of its outbox row; an error
// of the sink is retried.
func (s *Sink) Deliver(ctx context.Context, _ *sql.Tx, delivery outbox.Delivery) error {
	if delivery.Tenant.IsZero() {
		return outbox.PermanentRefusal(tenant.ErrZero)
	}
	err := s.destination.DeliverFor(ctx, delivery.Tenant.ID(), delivery.Payload)
	if errors.Is(err, audit.ErrBadMessage) {
		return outbox.PermanentRefusal(err)
	}
	return err
}
