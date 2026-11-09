package authz

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"

	"stego.dev/runtime/tenant"
)

// DestinationName is the name of the outbox destination of the relay.
const DestinationName = "authz"

// DefaultAlertBound is the delivery alert bound when the relay is built with none (ST-SUP-RP06-LC16).
const DefaultAlertBound = 5 * time.Second

// Delivery is one claimed outbox message for the relay: its identity, the tenant of its row, its resource key and
// its payload. Its fields are those that the outbox runtime's delivery holds (ST-SUP-RP01-LC03); the service's own
// code passes them from the outbox handler to Deliver. The outbox package is not imported here.
type Delivery struct {
	ID          uuid.UUID
	Tenant      tenant.Tenant
	ResourceKey string
	Payload     json.RawMessage
}

// Registration is how the relay is registered on the outbox worker (ST-SUP-RP01-LC14): the destination authz,
// idempotent and must land, with Deliver as its handler.
type Registration struct {
	Name       string
	Idempotent bool
	MustLand   bool
	Handler    func(ctx context.Context, delivery Delivery) error
}

// Owner names the table of a protected entity, whose rows hold the token of their resource in authz_token: the
// resource ID is in the column Key, the tenant in tenant_id. An empty Schema is the relay's schema.
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
	// DB is the database of the floor, the revocation tables and the protected entities, as the relay role.
	DB *sql.DB
	// Schema is the schema of the tables that Migration creates.
	Schema string
	// Owners maps a resource type to the table of its rows. A tuple of a resource type with no owner, such as a
	// group, stores its token on no row.
	Owners map[string]Owner
	// Permanent marks an error as a permanent refusal for the outbox runtime, which parks the entry and raises its
	// alert (ST-SUP-RP01-LC12). The service's own code passes the outbox runtime's PermanentRefusal.
	Permanent func(error) error
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
	permanent     func(error) error
	bound         time.Duration

	alerts metric.Int64Counter
	logger otellog.Logger

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
	case cfg.Permanent == nil:
		return nil, errors.New("authz: the relay has no mark for a permanent refusal")
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
	r := &Relay{principal: principal, relationships: cfg.Relationships, db: cfg.DB, schema: schema, owners: owners,
		permanent: cfg.Permanent, bound: DefaultAlertBound}
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
	}
	if cfg.Logger != nil {
		r.logger = cfg.Logger.Logger(instrumentation)
	}
	return r, nil
}

const instrumentation = "stego.dev/runtime/authz"

// Principal returns the relay's principal, service_account:<name>.
func (r *Relay) Principal() Principal { return r.principal }

// AlertBound returns the delivery alert bound.
func (r *Relay) AlertBound() time.Duration { return r.bound }

// Destination returns the registration of the relay on the outbox worker: the destination authz, idempotent and
// must land (ST-SUP-RP06-LC06).
func (r *Relay) Destination() Registration {
	return Registration{Name: DestinationName, Idempotent: true, MustLand: true, Handler: r.Deliver}
}

// Deliver lands one tuple write. It refuses, as a permanent refusal, a payload it cannot read and a write the
// provider refuses; any other error may pass on a retry, which is safe because touch and delete are idempotent.
func (r *Relay) Deliver(ctx context.Context, delivery Delivery) error {
	if delivery.Tenant.IsZero() {
		return r.permanent(tenant.ErrZero)
	}
	message, err := readPayload(delivery)
	if err != nil {
		return r.permanent(err)
	}
	tuple := message.Tuple.tuple()
	var token Token
	switch message.Operation {
	case OperationTouch:
		token, err = r.relationships.Touch(ctx, []Tuple{tuple}, nil)
	case OperationDelete:
		token, err = r.relationships.Delete(ctx, []Filter{FilterOf(tuple)}, nil)
	}
	if err != nil {
		if IsRefused(err) {
			return r.permanent(fmt.Errorf("authz: the provider refused %s of %s: %w", message.Operation, tuple, err))
		}
		return fmt.Errorf("authz: %s %s: %w", message.Operation, tuple, err)
	}
	if token.IsZero() {
		return fmt.Errorf("authz: the provider gave no token for %s of %s", message.Operation, tuple)
	}
	if err := r.raiseFloor(ctx, token); err != nil {
		return err
	}
	if r.afterFloor != nil {
		if err := r.afterFloor(); err != nil {
			return err
		}
	}
	return r.storeToken(ctx, delivery, tuple, token)
}

func readPayload(delivery Delivery) (payload, error) {
	var message payload
	decoder := json.NewDecoder(bytes.NewReader(delivery.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&message); err != nil {
		return payload{}, fmt.Errorf("authz: the relay payload does not decode: %w", err)
	}
	if decoder.More() {
		return payload{}, errors.New("authz: the relay payload holds more than one value")
	}
	if message.Version != payloadVersion {
		return payload{}, fmt.Errorf("authz: the relay payload has the version %d", message.Version)
	}
	if message.Operation != OperationTouch && message.Operation != OperationDelete {
		return payload{}, fmt.Errorf("authz: the relay payload has the operation %q", message.Operation)
	}
	tuple := message.Tuple.tuple()
	if err := tuple.valid(); err != nil {
		return payload{}, err
	}
	if tuple.Resource.String() != delivery.ResourceKey {
		return payload{}, errors.New("authz: the relay payload is for another resource key")
	}
	return message, nil
}

// raiseFloor raises the revocation floor to token with one statement, in a transaction of its own, which only
// raises the revision: a concurrent relay that holds a fresher token makes it change nothing, and the statement
// waits at most for the one statement of the other relay (ST-SUP-RP06-LC13).
func (r *Relay) raiseFloor(ctx context.Context, token Token) error {
	if _, err := r.db.ExecContext(ctx, "UPDATE "+r.schema+".authz_floor SET token = $1, revision = $2::numeric "+
		"WHERE only_row AND (revision IS NULL OR revision < $2::numeric)", token.Text(), token.Revision()); err != nil {
		return fmt.Errorf("authz: raise the revocation floor: %w", err)
	}
	return nil
}

// storeToken stores token on the owning row and on the tuple of its revocation, in a transaction of the delivery's
// tenant.
func (r *Relay) storeToken(ctx context.Context, delivery Delivery, tuple Tuple, token Token) error {
	tx, err := tenant.Begin(ctx, r.db, delivery.Tenant, nil)
	if err != nil {
		return fmt.Errorf("authz: store the token: %w", err)
	}
	if owner, ok := r.owners[tuple.Resource.Type]; ok {
		if _, err := tx.ExecContext(ctx, "UPDATE "+owner.table+" SET authz_token = $1 WHERE "+owner.key+
			" = $2 AND tenant_id = $3", token.Text(), tuple.Resource.ID, delivery.Tenant.ID()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("authz: store the token on the row of %s: %w", tuple.Resource, err)
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE "+r.schema+".authz_revocation_tuples SET token = $1, revision = $2::numeric, "+
		"delivered_at = pg_catalog.now() WHERE message_id = $3 AND tenant_id = $4 "+
		"AND (revision IS NULL OR revision < $2::numeric)",
		token.Text(), token.Revision(), delivery.ID, delivery.Tenant.ID()); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("authz: store the token on the tuple of its revocation: %w", err)
	}
	if err := tenant.Commit(ctx, tx, delivery.Tenant); err != nil {
		return fmt.Errorf("authz: store the token: %w", err)
	}
	return nil
}

// Observe takes one reading of the gauge outbox.oldest.age{destination=authz} (ST-SUP-RP01-LC07) and raises one
// alert event and metric that name the destination authz when the age crosses the delivery alert bound upward. It
// raises none while the age stays above the bound, and the next alert needs the age back at or below the bound
// first (ST-SUP-RP06-LC14).
func (r *Relay) Observe(ctx context.Context, oldest time.Duration) {
	r.mu.Lock()
	above := oldest > r.bound
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
		record.SetBody(attribute.StringValue("a revocation is not delivered within the delivery alert bound"))
		record.AddAttributes(attribute.String("destination", DestinationName),
			attribute.Float64("bound_seconds", r.bound.Seconds()), attribute.Float64("oldest_seconds", oldest.Seconds()))
		r.logger.Emit(ctx, record)
	}
}

// OldestAge reads the age of the oldest authz outbox message that is not done, as the outbox runtime reports it.
type OldestAge func(ctx context.Context) (time.Duration, error)

// Watch reads oldest every interval and observes it, until ctx ends. The service's own code runs it beside the
// outbox worker.
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
		age, err := oldest(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		r.Observe(ctx, age)
	}
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
		return Token{}, fmt.Errorf("authz: read the revocation floor: %w", err)
	}
	return ParseToken(text)
}
