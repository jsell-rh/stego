package authz

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"

	"github.com/jsell-rh/stego/registry/runtime/outbox"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// DestinationName is the name of the outbox destination of the relay.
const DestinationName = "authz"

// DefaultAlertBound is the delivery alert bound when the relay is built with none (ST-SUP-RP06-LC16).
const DefaultAlertBound = 5 * time.Second

// DefaultMarker is the fixed marker relationship whose TOUCH raises the revocation floor (ST-SUP-RP06-LC13). The
// service's schema declares it: definition stego_floor { relation marker: stego_floor }. Its IDs are not
// tenant-qualified (ST-SUP-RP06-LC22): the marker is one relationship of the whole service, not a row of a tenant,
// it holds no tenant data, no evaluation or search names it, and it grants no permission; only the relay writes it.
var DefaultMarker = Tuple{Resource: Resource{Type: "stego_floor", ID: "floor"}, Relation: "marker",
	Subject: Subject{Type: "stego_floor", ID: "floor"}}

// Owner names the table of a protected entity, whose rows hold the token of their resource in authz_token: the key
// part of the tenant-qualified resource ID (ST-SUP-RP06-LC22) is in the column Key, the tenant in tenant_id. An empty Schema is the relay's schema.
type Owner struct {
	Schema string
	Table  string
	Key    string
}

// RelayConfig is what the relay needs.
type RelayConfig struct {
	// Name is the name of the relay's service account, such as authz-relay.
	Name string
	// Relationships is the relationships port, which only the relay holds.
	Relationships Relationships
	// DB is the database of the floor, as the relay role, which raises the floor in a transaction of its own.
	DB *sql.DB
	// Schema is the schema of the tables that Migration creates.
	Schema string
	// Marker is the fixed marker relationship that the relay TOUCHes to raise the floor. The zero Tuple is
	// DefaultMarker.
	Marker Tuple
	// Owners maps a resource type to the table of its rows. A tuple of a resource type with no owner, such as a
	// group, stores its token on no row.
	Owners map[string]Owner
	// Meter gives the alert metric of a late revocation. Nil records no metric.
	Meter metric.MeterProvider
	// Logger gives the alert event of a late revocation. Nil emits no event.
	Logger otellog.LoggerProvider
}

// RelayOption is an option of NewRelay.
type RelayOption func(*Relay) error

// WithAlertBound sets the delivery alert bound, which must be positive (ST-SUP-RP06-LC16).
func WithAlertBound(bound time.Duration) RelayOption {
	return func(r *Relay) error {
		if bound <= 0 {
			return fmt.Errorf("authz: the delivery alert bound %s is not positive", bound)
		}
		r.bound = bound
		return nil
	}
}

type ownerTable struct {
	table string
	key   string
}

// Relay is the outbox destination authz (ST-SUP-RP06-LC06). It writes each tuple to the provider with its
// relationships port, raises the service's revocation floor to the returned token in a short transaction of its
// own, stores the token on the owning row and on the tuple of its revocation, and only then reports the delivery
// done. The outbox worker delivers its entries in order per resource key and parks one that the provider refuses.
type Relay struct {
	principal     Principal
	relationships Relationships
	db            *sql.DB
	schema        string
	owners        map[string]ownerTable
	marker        Tuple
	bound         time.Duration

	alerts           metric.Int64Counter
	readFailures     metric.Int64Counter
	uncountedChanges metric.Int64Counter
	notCounted       atomic.Int64
	logger           otellog.Logger
	failures         atomic.Int64

	mu    sync.Mutex
	above bool

	// afterFloor runs after the floor is raised and before the delivery is done. Only a test sets it.
	afterFloor func() error
}

// NewRelay returns the relay of cfg. It refuses a configuration that lacks a part, and a delivery alert bound that
// is not positive.
func NewRelay(cfg RelayConfig, options ...RelayOption) (*Relay, error) {
	principal, err := System(cfg.Name)
	if err != nil {
		return nil, err
	}
	switch {
	case cfg.Relationships == nil:
		return nil, errors.New("authz: the relay has no relationships port")
	case cfg.DB == nil:
		return nil, errors.New("authz: the relay has no database")
	}
	schema, err := quoteName("schema", cfg.Schema)
	if err != nil {
		return nil, err
	}
	owners := make(map[string]ownerTable, len(cfg.Owners))
	for resourceType, owner := range cfg.Owners {
		if err := validType("resource", resourceType); err != nil {
			return nil, err
		}
		ownerSchema := schema
		if owner.Schema != "" {
			if ownerSchema, err = quoteName("owner schema", owner.Schema); err != nil {
				return nil, err
			}
		}
		table, err := quoteName("owner table", owner.Table)
		if err != nil {
			return nil, err
		}
		key, err := quoteName("owner key column", owner.Key)
		if err != nil {
			return nil, err
		}
		owners[resourceType] = ownerTable{table: ownerSchema + "." + table, key: key}
	}
	marker := cfg.Marker
	if marker == (Tuple{}) {
		marker = DefaultMarker
	}
	if err := marker.valid(); err != nil {
		return nil, err
	}
	r := &Relay{principal: principal, relationships: cfg.Relationships, db: cfg.DB, schema: schema, owners: owners,
		marker: marker, bound: DefaultAlertBound}
	for _, option := range options {
		if option == nil {
			continue
		}
		if err := option(r); err != nil {
			return nil, err
		}
	}
	if cfg.Meter != nil {
		r.alerts, err = cfg.Meter.Meter(instrumentation).Int64Counter("authz.revocation.late",
			metric.WithDescription("The times the oldest authz outbox message became older than the delivery alert bound."))
		if err != nil {
			return nil, err
		}
		r.readFailures, err = cfg.Meter.Meter(instrumentation).Int64Counter("authz.oldest_age.read_failures",
			metric.WithDescription("The reads of the oldest age of the authz outbox messages that failed."))
		if err != nil {
			return nil, err
		}
		r.uncountedChanges, err = cfg.Meter.Meter(instrumentation).Int64Counter("authz.revocation.uncounted",
			metric.WithDescription("The state changes of authz messages that the revocation record did not count."))
		if err != nil {
			return nil, err
		}
	}
	if cfg.Logger != nil {
		r.logger = cfg.Logger.Logger(instrumentation)
	}
	return r, nil
}

const instrumentation = "github.com/jsell-rh/stego/registry/runtime/authz"

// Principal returns the relay's principal, service_account:<name>.
func (r *Relay) Principal() Principal { return r.principal }

// AlertBound returns the delivery alert bound.
func (r *Relay) AlertBound() time.Duration { return r.bound }

// Destination returns the registration of the relay on the outbox worker (ST-SUP-RP06-LC06, ST-SUP-RP01-LC25): the
// destination authz, idempotent and must land, with Deliver as its handler and Hook as its state hook.
func (r *Relay) Destination() outbox.Destination {
	return outbox.Destination{Name: DestinationName, Idempotent: true, MustLand: true, Handler: r.Deliver, StateHook: r.Hook}
}

// Hook is the relay's state hook (ST-SUP-RP06-LC18): in tx, the transaction of the outbox in which a message of the
// revocation changes state, it calls the function authz_revocation_state, which counts the change on the
// revocation's record only when the message is in that state, and on the last done tuple stores the floor as it
// reads it in tx. An error rolls the state change back.
func (r *Relay) Hook(ctx context.Context, tx *sql.Tx, change outbox.StateChange) error {
	if tx == nil {
		return errors.New("authz: the state hook has no transaction")
	}
	if change.Destination != DestinationName {
		return errors.New("authz: the state hook got a message of another destination")
	}
	revocation, err := uuid.Parse(change.Group)
	if err != nil {
		return errors.New("authz: the message names no revocation")
	}
	var counted bool
	if err := tx.QueryRowContext(ctx, "SELECT "+r.schema+".authz_revocation_state($1::uuid, $2::uuid, $3::text)",
		change.ID, revocation, string(change.State)).Scan(&counted); err != nil {
		return fixed("authz: the revocation record cannot be changed", err)
	}
	if !counted {
		r.uncounted(ctx, change.State)
	}
	return nil
}

// UncountedChanges returns the number of state changes that the state function did not count.
func (r *Relay) UncountedChanges() int64 { return r.notCounted.Load() }

// uncounted counts a state change that the state function did not count, because the outbox row is not in the
// reported state or the revocation does not know the message, and raises one alert event and metric that name the
// destination authz and the state: such a change is never dropped silently.
func (r *Relay) uncounted(ctx context.Context, state outbox.State) {
	r.notCounted.Add(1)
	attributes := []attribute.KeyValue{attribute.String("destination", DestinationName), attribute.String("state", string(state))}
	if r.uncountedChanges != nil {
		r.uncountedChanges.Add(ctx, 1, metric.WithAttributes(attributes...))
	}
	if r.logger != nil {
		var record otellog.Record
		record.SetEventName("authz.revocation.uncounted")
		record.SetSeverity(otellog.SeverityWarn)
		record.SetSeverityText("WARN")
		record.SetBody(attribute.StringValue("a state change of an authz message is not counted on its revocation"))
		record.AddAttributes(attributes...)
		r.logger.Emit(ctx, record)
	}
}

// Deliver is the outbox handler of the relay. It lands one tuple write (ST-SUP-RP06-LC13): in a short transaction
// of its own it locks the floor row, writes the tuple, touch or delete, together with a TOUCH of the fixed marker
// relationship in one WriteRelationships request, sets the floor to that request's one written_at token and commits;
// only then does it store the same token on the owning row in tx, the worker's transaction of the delivery's
// tenant, which commits with done. It refuses, as a permanent refusal, a payload it cannot read and a write the
// provider refuses, so the outbox parks the entry (ST-SUP-RP06-LC06); any other error may pass on a retry, which is
// safe because touch and delete are idempotent.
func (r *Relay) Deliver(ctx context.Context, tx *sql.Tx, delivery outbox.Delivery) error {
	if delivery.Tenant.IsZero() {
		return outbox.PermanentRefusal(tenant.ErrZero)
	}
	if tx == nil {
		return errors.New("authz: the delivery has no transaction")
	}
	message, err := readPayload(delivery.Payload, delivery.Tenant)
	if err != nil {
		return outbox.PermanentRefusal(err)
	}
	tuple := message.Tuple.tuple()
	// Every resource ID of the tuple names the tenant of the outbox row, which the staging transaction wrote from its
	// own tenant; a tuple of another tenant, or one that is not tenant-qualified, is never written (ST-SUP-RP06-LC22).
	// The filtered delete of a relation has no subject.
	var key string
	if message.Operation == OperationDeleteRelation {
		key, err = keyIn(delivery.Tenant, tuple.Resource.ID)
	} else {
		key, err = tupleKey(delivery.Tenant, tuple)
	}
	if err != nil {
		return outbox.PermanentRefusal(fmt.Errorf("authz: the tuple %s is refused: %w", tuple.Resource.Type+"#"+tuple.Relation, err))
	}
	if resourceKey(tuple.Resource.Type, key) != delivery.ResourceKey {
		return outbox.PermanentRefusal(errors.New("authz: the relay payload is for another resource key"))
	}
	token, err := r.writeWithFloor(ctx, message.Operation, tuple)
	if err != nil {
		return err
	}
	if r.afterFloor != nil {
		if err := r.afterFloor(); err != nil {
			return err
		}
	}
	return r.storeToken(ctx, tx, delivery, tuple, key, token)
}

// writeWithFloor writes the tuple and the marker in one request under the floor lock and raises the floor to the
// request's written_at token, in one short transaction of its own. The marker write commits after the lock is
// granted, in the same request as the tuple, so the token is at least as fresh as the tuple's write and as every
// earlier floor, on every datastore: the floor never falls, and no two tokens are compared. No head read is used.
// The lock is held for that one provider call only.
func (r *Relay) writeWithFloor(ctx context.Context, operation Operation, tuple Tuple) (Token, error) {
	floorTx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Token{}, fixed("authz: the floor transaction cannot begin", err)
	}
	defer func() { _ = floorTx.Rollback() }()
	var current string
	if err := floorTx.QueryRowContext(ctx, "SELECT token FROM "+r.schema+".authz_floor WHERE only_row FOR UPDATE").Scan(&current); err != nil {
		return Token{}, fixed("authz: the floor row cannot be locked", err)
	}
	touches := []Tuple{r.marker}
	var deletes []Filter
	switch operation {
	case OperationTouch:
		touches = append([]Tuple{tuple}, r.marker)
	case OperationDelete:
		deletes = []Filter{FilterOf(tuple)}
	case OperationDeleteRelation:
		// A filtered delete cannot go in one request with the marker. It is made first, under the floor lock, and
		// the marker TOUCH after it gives the token, which is then at least as fresh as the delete.
		filter, err := RelationFilter(tuple.Resource, tuple.Relation)
		if err != nil {
			return Token{}, outbox.PermanentRefusal(err)
		}
		if _, err := r.relationships.Delete(ctx, []Filter{filter}, nil); err != nil {
			if IsRefused(err) {
				return Token{}, outbox.PermanentRefusal(fmt.Errorf("authz: the provider refused the %s of %s: %w", operation, tuple.Resource.Type+"#"+tuple.Relation, err))
			}
			return Token{}, fmt.Errorf("authz: the %s of %s failed: %w", operation, tuple.Resource.Type+"#"+tuple.Relation, err)
		}
	}
	token, err := r.relationships.Write(ctx, touches, deletes, nil)
	if err != nil {
		if IsRefused(err) {
			return Token{}, outbox.PermanentRefusal(fmt.Errorf("authz: the provider refused the %s of %s: %w", operation, tuple.Name(), err))
		}
		return Token{}, fmt.Errorf("authz: the %s of %s failed: %w", operation, tuple.Name(), err)
	}
	if token.IsZero() {
		return Token{}, fmt.Errorf("authz: the provider gave no token for the %s of %s", operation, tuple.Name())
	}
	if _, err := floorTx.ExecContext(ctx, "UPDATE "+r.schema+".authz_floor SET token = $1, sequence = sequence + 1 WHERE only_row",
		token.Text()); err != nil {
		return Token{}, fixed("authz: the floor cannot be written", err)
	}
	if err := floorTx.Commit(); err != nil {
		return Token{}, fixed("authz: the floor transaction cannot commit", err)
	}
	return token, nil
}

// Check TOUCHes the marker relationship once, so a service whose schema does not declare it, definition stego_floor
// { relation marker: stego_floor }, does not start. The service's own code calls it at start-up, before it runs the
// outbox worker.
func (r *Relay) Check(ctx context.Context) error {
	if _, err := r.relationships.Touch(ctx, []Tuple{r.marker}, nil); err != nil {
		return fmt.Errorf("authz: the provider refuses the floor marker %s, so the relay does not start: %w", r.marker.Name(), err)
	}
	return nil
}

// payloadVersionKeys is the version of the relay payload before ST-SUP-RP06-LC22: its resource ID, and the subject
// ID of a subject set, are the bare keys of the outbox row's tenant.
const payloadVersionKeys = 1

// readPayload reads the payload of a delivery of tenant t. A payload of version 1 holds bare keys: readPayload
// upgrades it to the tenant-qualified form of t, the tenant of the outbox row, so a message enqueued before
// ST-SUP-RP06-LC22 is delivered and never parks its resource key.
func readPayload(body []byte, t tenant.Tenant) (payload, error) {
	var message payload
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&message); err != nil {
		return payload{}, errors.New("authz: the relay payload does not decode")
	}
	if decoder.More() {
		return payload{}, errors.New("authz: the relay payload holds more than one value")
	}
	switch message.Version {
	case payloadVersion:
	case payloadVersionKeys:
		if t.IsZero() {
			return payload{}, tenant.ErrZero
		}
		message.Version = payloadVersion
		message.Tuple.ResourceID = qualify(t.ID(), message.Tuple.ResourceID)
		if message.Tuple.SubjectRelation != "" {
			message.Tuple.SubjectID = qualify(t.ID(), message.Tuple.SubjectID)
		}
	default:
		return payload{}, errors.New("authz: the relay payload has an unknown version")
	}
	tuple := message.Tuple.tuple()
	switch message.Operation {
	case OperationTouch, OperationDelete:
		if err := tuple.valid(); err != nil {
			return payload{}, err
		}
	case OperationDeleteRelation:
		// The filtered delete names one resource and one relation, and no subject (ST-SUP-Q118).
		if tuple.Subject != (Subject{}) || tuple.SubjectRelation != "" {
			return payload{}, errors.New("authz: the relay payload of a filtered delete names a subject")
		}
		if _, err := RelationFilter(tuple.Resource, tuple.Relation); err != nil {
			return payload{}, err
		}
	default:
		return payload{}, errors.New("authz: the relay payload has an unknown operation")
	}
	return message, nil
}

// storeToken stores the write token on the owning row in tx, the worker's transaction of the delivery's tenant: the
// row of key, the key part of the tuple's tenant-qualified resource ID.
func (r *Relay) storeToken(ctx context.Context, tx *sql.Tx, delivery outbox.Delivery, tuple Tuple, key string, token Token) error {
	owner, ok := r.owners[tuple.Resource.Type]
	if !ok {
		return nil
	}
	if _, err := tx.ExecContext(ctx, "UPDATE "+owner.table+" SET authz_token = $1 WHERE "+owner.key+
		" = $2 AND tenant_id = $3", token.Text(), key, delivery.Tenant.ID()); err != nil {
		return fmt.Errorf("authz: the token of %s cannot be stored on its row", tuple.Name())
	}
	return nil
}

// Observe takes one reading of the gauge outbox.oldest.age{destination=authz} (ST-SUP-RP01-LC07) and raises one
// alert event and metric that name the destination authz when the age crosses the delivery alert bound upward. It
// raises none while the age stays above the bound, and the next alert needs the age back at or below the bound
// first (ST-SUP-RP06-LC14).
func (r *Relay) Observe(ctx context.Context, oldest time.Duration) {
	r.cross(ctx, oldest > r.bound, false)
}

// ReadFailures returns the number of reads of the oldest age that failed in Watch.
func (r *Relay) ReadFailures() int64 { return r.failures.Load() }

// cross records whether the age is above the bound, and alerts on an upward crossing. A failed read counts as
// above the bound: a broken gauge cannot hide a late revocation.
func (r *Relay) cross(ctx context.Context, above, failed bool) {
	r.mu.Lock()
	crossed := above && !r.above
	r.above = above
	r.mu.Unlock()
	if !crossed {
		return
	}
	attributes := []attribute.KeyValue{attribute.String("destination", DestinationName)}
	if r.alerts != nil {
		r.alerts.Add(ctx, 1, metric.WithAttributes(attributes...))
	}
	if r.logger != nil {
		var record otellog.Record
		record.SetEventName("authz.revocation.late")
		record.SetSeverity(otellog.SeverityWarn)
		record.SetSeverityText("WARN")
		body := "a revocation is not delivered within the delivery alert bound"
		if failed {
			body = "the oldest age of the authz outbox messages cannot be read"
		}
		record.SetBody(attribute.StringValue(body))
		record.AddAttributes(attribute.String("destination", DestinationName),
			attribute.Float64("bound_seconds", r.bound.Seconds()), attribute.Bool("read_failed", failed))
		r.logger.Emit(ctx, record)
	}
}

// OldestAge reads the age of the oldest authz outbox message that is not done, as the outbox runtime reports it.
type OldestAge func(ctx context.Context) (time.Duration, error)

// Watch reads oldest every interval and observes it, until ctx ends. A read that fails is counted and alerts as an
// age above the bound (ST-SUP-RP06-LC14). The service's own code runs it beside the outbox worker.
func (r *Relay) Watch(ctx context.Context, interval time.Duration, oldest OldestAge) error {
	if interval <= 0 {
		return errors.New("authz: the watch interval is not positive")
	}
	if oldest == nil {
		return errors.New("authz: no reader of the oldest age")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.check(ctx, oldest)
	}
}

// check takes one reading of oldest. A read that fails because ctx ended is the end of the watch, which Watch
// returns, not a failed read of the gauge.
func (r *Relay) check(ctx context.Context, oldest OldestAge) {
	age, err := oldest(ctx)
	if err != nil && ctx.Err() != nil {
		return
	}
	if err != nil {
		r.failures.Add(1)
		if r.readFailures != nil {
			r.readFailures.Add(ctx, 1, metric.WithAttributes(attribute.String("destination", DestinationName)))
		}
		r.cross(ctx, true, true)
		return
	}
	r.Observe(ctx, age)
}

// TableFloor is the revocation floor that the relay keeps in the table authz_floor.
type TableFloor struct {
	db     *sql.DB
	schema string
}

// NewTableFloor returns the floor of the table authz_floor in schema.
func NewTableFloor(db *sql.DB, schema string) (*TableFloor, error) {
	if db == nil {
		return nil, errors.New("authz: no database")
	}
	quoted, err := quoteName("schema", schema)
	if err != nil {
		return nil, err
	}
	return &TableFloor{db: db, schema: quoted}, nil
}

// Current reads the floor: the token of the newest tuple write that the relay delivered, or the zero token.
func (f *TableFloor) Current(ctx context.Context) (Token, error) {
	var text string
	if err := f.db.QueryRowContext(ctx, "SELECT token FROM "+f.schema+".authz_floor WHERE only_row").Scan(&text); err != nil {
		return Token{}, fixed("authz: the revocation floor cannot be read", err)
	}
	return ParseToken(text)
}
