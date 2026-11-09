package outbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"stego.dev/runtime/tenant"
)

// The limits of the queue (ST-SUP-RC18-OC02).
const (
	// MaxPayloadBytes is the largest payload of one message.
	MaxPayloadBytes = 64 << 10
	// MaxBatchSize is the most messages of one Enqueue call, and the largest limit of a claim or a list.
	MaxBatchSize = 32
	// MaxKeyBytes is the longest resource key and outgoing idempotency key, in bytes.
	MaxKeyBytes = 255
	// MaxResourceKeyBytes is the longest resource key, in bytes.
	MaxResourceKeyBytes = 256
)

// operationTimeout bounds each statement of the queue.
const operationTimeout = 5 * time.Second

// Migration is the DDL text of the queue table stego_outbox.messages. A service applies it with its own
// migrations. The table holds no error text and no copy of a payload other than the payload itself
// (ST-SUP-RP01-LC06): failure_code is a short fixed label. state is one of the dispatch states of ST-SUP-RP01-LC08.
// attempts and conflicts are two counts (ST-SUP-RC18-OC29). tenant_id is the tenant of the staging transaction
// (ST-SUP-RC18-OC28), and outgoing_key is the idempotency key that every attempt sends (ST-SUP-RP01-LC10).
const Migration = `CREATE SCHEMA IF NOT EXISTS stego_outbox;

CREATE TABLE IF NOT EXISTS stego_outbox.messages (
	id uuid PRIMARY KEY,
	sequence bigint GENERATED ALWAYS AS IDENTITY UNIQUE NOT NULL,
	tenant_id text NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
	destination text NOT NULL CHECK (destination ~ '^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$'),
	resource_key text NOT NULL CHECK (octet_length(resource_key) BETWEEN 1 AND 256),
	kind text NOT NULL CHECK (kind ~ '^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$'),
	payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object' AND octet_length(payload::text) <= 131072),
	outgoing_key text NOT NULL CHECK (octet_length(outgoing_key) BETWEEN 1 AND 255),
	principal text NOT NULL DEFAULT '' CHECK (octet_length(principal) <= 255),
	origin text NOT NULL DEFAULT '' CHECK (octet_length(origin) <= 255),
	state text NOT NULL DEFAULT 'pending' CHECK (state IN
		('pending', 'dispatching', 'outcome_unknown', 'reconciling', 'done', 'parked', 'discarded')),
	created_at timestamptz NOT NULL,
	available_at timestamptz NOT NULL,
	attempts bigint NOT NULL DEFAULT 0 CHECK (attempts >= 0),
	conflicts bigint NOT NULL DEFAULT 0 CHECK (conflicts >= 0),
	lookups bigint NOT NULL DEFAULT 0 CHECK (lookups >= 0),
	lease_token uuid,
	lease_until timestamptz,
	dispatched_at timestamptz,
	failure_code text NOT NULL DEFAULT '' CHECK (octet_length(failure_code) <= 128),
	parked_from text CHECK (parked_from IN ('dispatching', 'outcome_unknown', 'reconciling')),
	parked_at timestamptz,
	done_at timestamptz,
	resolved_by text CHECK (octet_length(resolved_by) <= 255),
	resolved_at timestamptz,
	resolution text CHECK (resolution IN ('acted', 'not_acted', 'discarded')),
	CHECK ((lease_token IS NULL) = (lease_until IS NULL)),
	CHECK ((state = 'parked') = (parked_from IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS messages_open_order ON stego_outbox.messages (destination, resource_key, sequence)
	WHERE state NOT IN ('done', 'discarded');
CREATE INDEX IF NOT EXISTS messages_due ON stego_outbox.messages (available_at, sequence)
	WHERE state IN ('pending', 'dispatching', 'outcome_unknown', 'reconciling');
`

var labelPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

// Message is one message to enqueue. ID is its stable identity. OutgoingKey is the idempotency key that every
// attempt sends to the destination: the key that idempotency.Derive gives for the message's inputs, or, when it is
// empty, the message identity (ST-SUP-RP01-LC10). Principal and Origin are the staging principal and the name of
// the staging command or worker, for a destination that a worker consumes (ST-SUP-RP10-LC16); both may be empty.
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

// Receipt names one claim of a message: the message, the lease token of the claim, and the state the message was
// in when the receipt was given.
type Receipt struct {
	ID    uuid.UUID
	Token uuid.UUID
	State State
}

// Delivery is one claimed message. Tenant is read from the tenant column of the claimed row through the tenant
// runtime's row reader (ST-SUP-RP04-LC01), never from the payload.
type Delivery struct {
	Message
	Tenant       tenant.Tenant
	Receipt      Receipt
	Attempts     int64
	Conflicts    int64
	Lookups      int64
	LeaseUntil   time.Time
	DispatchedAt time.Time
}

// Clock is the clock of the queue. A nil Clock is the database clock, clock_timestamp(); a test passes a clock that
// it advances. A lease.Clock satisfies it.
type Clock interface {
	Now() time.Time
}

// Queue is the queue table of one database.
type Queue struct {
	db    *sql.DB
	clock Clock
}

// New returns the queue of db. clock may be nil: the queue then uses the database clock.
func New(db *sql.DB, clock Clock) (*Queue, error) {
	if db == nil {
		return nil, errors.New("outbox: the queue needs a database")
	}
	return &Queue{db: db, clock: clock}, nil
}

// now is the first argument of every statement that reads the time: the clock's time, or NULL for the database
// clock. Every statement reads it as COALESCE($1::timestamptz, clock_timestamp()).
func (q *Queue) now() any {
	if q == nil || q.clock == nil {
		return nil
	}
	return q.clock.Now()
}

const nowCTE = `now AS (SELECT COALESCE($1::timestamptz, pg_catalog.clock_timestamp()) AS t)`

// Check verifies that the queue columns and the delivery permissions are there. It changes no row.
func (q *Queue) Check(ctx context.Context) error {
	if ctx == nil || q == nil || q.db == nil {
		return errors.New("outbox: the check needs a context and a database")
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	rows, err := q.db.QueryContext(ctx, `SELECT id, sequence, tenant_id, destination, resource_key, kind, payload,
		outgoing_key, principal, origin, state, created_at, available_at, attempts, conflicts, lookups, lease_token,
		lease_until, dispatched_at, failure_code, parked_from, parked_at, done_at, resolved_by, resolved_at, resolution
		FROM stego_outbox.messages WHERE false`)
	if err != nil {
		return canceled(ctx, errors.New("outbox: the queue table is not there"))
	}
	if err := rows.Close(); err != nil {
		return canceled(ctx, errors.New("outbox: the queue table check failed"))
	}
	var permitted bool
	err = q.db.QueryRowContext(ctx, `SELECT
		pg_catalog.has_table_privilege('stego_outbox.messages', 'SELECT') AND
		pg_catalog.has_table_privilege('stego_outbox.messages', 'UPDATE') AND
		pg_catalog.has_table_privilege('stego_outbox.messages', 'DELETE')`).Scan(&permitted)
	if err != nil || !permitted {
		return canceled(ctx, errors.New("outbox: the delivery permissions are not there"))
	}
	return nil
}

// ValidateMessage checks one message with no database access (ST-SUP-RC18-OC02).
func ValidateMessage(message Message) error { return validateMessage(message) }

// Enqueue writes messages inside the caller's transaction tx (ST-SUP-RP01-LC01). It checks every payload before it
// sends a statement, then takes a transaction-scoped advisory lock for each destination and resource key in a
// stable order before it allocates a sequence (ST-SUP-RC18-OC13), and writes each message with the tenant of the
// transaction, which tenant.Begin set. Each message sends a notice through pg_notify, which PostgreSQL delivers only
// after the commit, so no notice precedes it. Call Enqueue once for each transaction with the whole batch and
// commit only when it succeeds; a rollback removes the messages and their notices with the application write.
func (q *Queue) Enqueue(ctx context.Context, tx *sql.Tx, messages ...Message) error {
	if tx == nil {
		return errors.New("outbox: Enqueue needs the caller's transaction")
	}
	if len(messages) == 0 || len(messages) > MaxBatchSize {
		return fmt.Errorf("outbox: a batch holds 1 to %d messages", MaxBatchSize)
	}
	keys := make(map[int64]bool)
	seen := make(map[uuid.UUID]bool)
	for _, message := range messages {
		if err := validateMessage(message); err != nil {
			return err
		}
		if seen[message.ID] {
			return errors.New("outbox: a message ID is twice in the batch")
		}
		seen[message.ID] = true
		keys[lockKey(message.Destination, message.ResourceKey)] = true
	}
	ordered := make([]int64, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	var tenantID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT pg_catalog.current_setting('app.tenant', true)`).Scan(&tenantID); err != nil {
		return canceled(ctx, fmt.Errorf("outbox: read the tenant of the transaction: %w", err))
	}
	if tenantID.String == "" {
		return errors.New("outbox: the transaction holds no tenant; open it with tenant.Begin")
	}
	for _, key := range ordered {
		if _, err := tx.ExecContext(ctx, `SELECT pg_catalog.pg_advisory_xact_lock($1)`, key); err != nil {
			return canceled(ctx, err)
		}
	}
	for _, message := range messages {
		outgoing := message.OutgoingKey
		if outgoing == "" {
			outgoing = message.ID.String()
		}
		if _, err := tx.ExecContext(ctx, `WITH `+nowCTE+`
			INSERT INTO stego_outbox.messages (id, tenant_id, destination, resource_key, kind, payload, outgoing_key,
				principal, origin, created_at, available_at)
			SELECT $2, $3, $4, $5, $6, $7::jsonb, $8, $9, $10, now.t, now.t FROM now`,
			q.now(), message.ID, tenantID.String, message.Destination, message.ResourceKey, message.Kind,
			string(message.Payload), outgoing, message.Principal, message.Origin); err != nil {
			return canceled(ctx, err)
		}
		notice, err := encodeEvent(Event{ID: message.ID, TenantID: tenantID.String, Destination: message.Destination,
			ResourceKey: message.ResourceKey, Kind: message.Kind})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `SELECT pg_catalog.pg_notify($1, $2)`, NoticeChannel, notice); err != nil {
			return canceled(ctx, err)
		}
	}
	return nil
}

func lockKey(destination, resourceKey string) int64 {
	hash := sha256.Sum256([]byte("stego-outbox\x00" + destination + "\x00" + resourceKey))
	return int64(binary.BigEndian.Uint64(hash[:8]))
}

// claimStatement first moves each dispatching message whose lease has expired to outcome unknown, then claims the
// messages that are due, one for each key: the earliest message of the key that is not done or discarded, in
// pending, outcome unknown or reconciling, with no live lease (ST-SUP-RP01-LC15, ST-STD-TXN-R14). Both parts lock
// with SKIP LOCKED, so two workers never claim one message (ST-SUP-RC18-OC14). The two parts read one snapshot, so a
// message that the first part moves still holds back its key in the second, and the key is claimed in the next
// claim, not in this one. A claim of a pending message moves it to dispatching and counts an attempt; a claim of a
// message in outcome unknown or reconciling keeps its state, and the worker then takes its next step.
const claimStatement = `WITH ` + nowCTE + `,
expired AS (
	UPDATE stego_outbox.messages AS x
	SET state = 'outcome_unknown', lease_token = NULL, lease_until = NULL, failure_code = 'lease-expired',
		available_at = now.t, conflicts = 0
	FROM now
	WHERE x.id IN (SELECT e.id FROM stego_outbox.messages AS e, now
		WHERE e.state = 'dispatching' AND e.lease_until <= now.t
		ORDER BY e.sequence LIMIT 32 FOR UPDATE OF e SKIP LOCKED)
	RETURNING x.id
),
ready AS (
	SELECT m.id FROM stego_outbox.messages AS m, now
	WHERE m.state IN ('pending', 'outcome_unknown', 'reconciling')
		AND m.available_at <= now.t
		AND (m.lease_until IS NULL OR m.lease_until <= now.t)
		AND NOT EXISTS (SELECT 1 FROM stego_outbox.messages AS earlier
			WHERE earlier.destination = m.destination AND earlier.resource_key = m.resource_key
				AND earlier.sequence < m.sequence AND earlier.state NOT IN ('done', 'discarded'))
	ORDER BY m.sequence LIMIT $2 FOR UPDATE OF m SKIP LOCKED
)
UPDATE stego_outbox.messages AS m
SET lease_token = $3, lease_until = now.t + $4 * interval '1 millisecond',
	state = CASE WHEN m.state = 'pending' THEN 'dispatching' ELSE m.state END,
	attempts = CASE WHEN m.state = 'pending' THEN m.attempts + 1 ELSE m.attempts END,
	dispatched_at = CASE WHEN m.state = 'pending' THEN now.t ELSE m.dispatched_at END
FROM ready, now
WHERE m.id = ready.id
RETURNING m.tenant_id, m.id, m.destination, m.resource_key, m.kind, m.payload, m.outgoing_key, m.principal,
	m.origin, m.state, m.attempts, m.conflicts, m.lookups, m.lease_until, m.dispatched_at`

// Claim leases at most limit messages for lease, one for each key, and returns them. It returns no delivery when
// the statement, a row scan or the row iteration fails (ST-SUP-RC18-OC08); the database may then still hold the
// leases that it wrote, and they expire as any lease does (ST-SUP-RC18-OC09). A cancellation of ctx is returned as
// such (ST-SUP-RC18-OC24).
func (q *Queue) Claim(ctx context.Context, limit int, lease time.Duration) ([]Delivery, error) {
	if limit < 1 || limit > MaxBatchSize || lease < time.Second || lease > 5*time.Minute {
		return nil, errors.New("outbox: the claim limit or the lease is out of its bounds")
	}
	if q == nil || q.db == nil {
		return nil, errors.New("outbox: the queue needs a database")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	token, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	rows, err := q.db.QueryContext(ctx, claimStatement, q.now(), limit, token, lease.Milliseconds())
	if err != nil {
		return nil, canceled(ctx, err)
	}
	defer rows.Close()
	var deliveries []Delivery
	for rows.Next() {
		var d Delivery
		var state string
		var dispatched sql.NullTime
		t, err := tenant.ScanRows(rows, &d.ID, &d.Destination, &d.ResourceKey, &d.Kind, &d.Payload, &d.OutgoingKey,
			&d.Principal, &d.Origin, &state, &d.Attempts, &d.Conflicts, &d.Lookups, &d.LeaseUntil, &dispatched)
		if err != nil {
			return nil, canceled(ctx, err)
		}
		d.Tenant = t
		d.DispatchedAt = dispatched.Time
		d.Receipt = Receipt{ID: d.ID, Token: token, State: State(state)}
		deliveries = append(deliveries, d)
	}
	if err := rows.Err(); err != nil {
		return nil, canceled(ctx, err)
	}
	return deliveries, nil
}

// change is what one move of a claimed message writes besides its new state.
type change struct {
	delay       time.Duration // the wait until the message is due again
	dueAt       *time.Time    // the time the message is due again; it takes the place of delay
	attempts    int64         // added to the attempt count
	conflict    bool          // true counts one conflict in a row; false sets the count to zero
	lookup      bool          // true counts one status lookup
	failureCode string
	lease       time.Duration // for a move to dispatching: the new lease
}

// moveStatement moves a claimed message from the state of its receipt to $4, only while the receipt's lease is
// live. A move to dispatching keeps the lease and renews it; every other move ends it. It returns the seconds from
// the enqueue to now, for the delivery delay (ST-SUP-RP01-LC07).
const moveStatement = `WITH ` + nowCTE + `
UPDATE stego_outbox.messages AS m SET
	state = $4,
	lease_token = CASE WHEN $4 = 'dispatching' THEN m.lease_token END,
	lease_until = CASE WHEN $4 = 'dispatching' THEN now.t + $12 * interval '1 millisecond' END,
	available_at = COALESCE($6::timestamptz, now.t + $5 * interval '1 millisecond'),
	attempts = GREATEST(m.attempts + $7, 0),
	conflicts = CASE WHEN $8 THEN m.conflicts + 1 ELSE 0 END,
	lookups = CASE WHEN $9 THEN m.lookups + 1 ELSE m.lookups END,
	dispatched_at = CASE WHEN $4 = 'dispatching' THEN now.t ELSE m.dispatched_at END,
	failure_code = $10,
	parked_from = CASE WHEN $4 = 'parked' THEN $11 END,
	parked_at = CASE WHEN $4 = 'parked' THEN now.t END,
	done_at = CASE WHEN $4 = 'done' THEN now.t ELSE m.done_at END
FROM now
WHERE m.id = $2 AND m.lease_token = $3 AND m.lease_until > now.t AND m.state = $11
RETURNING pg_catalog.date_part('epoch', now.t - m.created_at)::float8, m.attempts, m.conflicts, m.lookups`

// moved is the result of one move.
type moved struct {
	delay     time.Duration // from the enqueue to the move
	attempts  int64
	conflicts int64
	lookups   int64
}

// move moves the claimed message of receipt to the state to (ST-SUP-RP01-LC08). It refuses a transition that the
// state machine does not allow, with an error that names both states, and changes nothing. It returns false with
// no error when the lease of the receipt is no longer live, so a late result never counts (ST-SUP-RP01-LC02). On
// success it sets the receipt's state to the new state.
func (q *Queue) move(ctx context.Context, receipt *Receipt, to State, c change) (bool, moved, error) {
	if receipt == nil || receipt.ID == uuid.Nil || receipt.Token == uuid.Nil {
		return false, moved{}, errors.New("outbox: the receipt is not valid")
	}
	if !labelPattern.MatchString(c.failureCode) && c.failureCode != "" {
		return false, moved{}, errors.New("outbox: the failure code is not a short label")
	}
	if c.delay < 0 || c.delay > 24*time.Hour {
		return false, moved{}, errors.New("outbox: the delay is out of its bounds")
	}
	if err := Transition(receipt.State, to); err != nil {
		return false, moved{}, err
	}
	if err := ctx.Err(); err != nil {
		return false, moved{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	var due any
	if c.dueAt != nil {
		due = *c.dueAt
	}
	var m moved
	var seconds float64
	err := q.db.QueryRowContext(ctx, moveStatement, q.now(), receipt.ID, receipt.Token, string(to),
		c.delay.Milliseconds(), due, c.attempts, c.conflict, c.lookup, c.failureCode, string(receipt.State),
		c.lease.Milliseconds()).Scan(&seconds, &m.attempts, &m.conflicts, &m.lookups)
	if errors.Is(err, sql.ErrNoRows) {
		return false, moved{}, q.refusedMove(ctx, receipt.ID, to)
	}
	if err != nil {
		return false, moved{}, canceled(ctx, err)
	}
	m.delay = time.Duration(seconds * float64(time.Second))
	receipt.State = to
	return true, m, nil
}

// refusedMove finds why a move changed no row: a message whose stored state does not allow the transition gives an
// error that names both states (ST-SUP-RP01-LC08); a message whose lease is gone gives no error.
func (q *Queue) refusedMove(ctx context.Context, id uuid.UUID, to State) error {
	var state string
	err := q.db.QueryRowContext(ctx, `SELECT state FROM stego_outbox.messages WHERE id = $1`, id).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return canceled(ctx, err)
	}
	if err := Transition(State(state), to); err != nil {
		return err
	}
	return nil
}

// release ends the lease of a claimed message and keeps its state, for a message that this worker cannot take a
// step for now, such as one whose destination it has no registration for.
func (q *Queue) release(ctx context.Context, receipt Receipt, delay time.Duration, failureCode string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	result, err := q.db.ExecContext(ctx, `WITH `+nowCTE+`
		UPDATE stego_outbox.messages AS m SET lease_token = NULL, lease_until = NULL,
			available_at = now.t + $4 * interval '1 millisecond', failure_code = $5
		FROM now WHERE m.id = $2 AND m.lease_token = $3 AND m.lease_until > now.t`,
		q.now(), receipt.ID, receipt.Token, delay.Milliseconds(), failureCode)
	return changed(ctx, result, err)
}

// Acknowledge moves a delivered message to done, only while the receipt's lease is live. False means the lease is
// gone; the message can then be delivered again with the same identity and outgoing key.
func (q *Queue) Acknowledge(ctx context.Context, receipt Receipt) (bool, error) {
	ok, _, err := q.move(ctx, &receipt, Done, doneChange(receipt.State))
	return ok, err
}

// doneChange is the change of a move to done. The attempt count holds the attempts that did not succeed, the one
// in flight included, so a move to done from dispatching takes back the attempt that succeeded.
func doneChange(from State) change {
	if from == Dispatching {
		return change{attempts: -1}
	}
	return change{}
}

// Retry moves a claimed message back to pending after a known failure, due again after delay, with the short
// failure code failureCode, never an error text that can hold a credential or the payload (ST-SUP-RP01-LC06). It
// keeps the message's identity, payload and outgoing key, and its attempt count (ST-SUP-RC18-OC04). It never
// removes a message. False means the lease is gone.
func (q *Queue) Retry(ctx context.Context, receipt Receipt, delay time.Duration, failureCode string) (bool, error) {
	if !labelPattern.MatchString(failureCode) {
		return false, errors.New("outbox: the failure code is not a short label")
	}
	ok, _, err := q.move(ctx, &receipt, Pending, change{delay: delay, failureCode: failureCode})
	return ok, err
}

// BlockedMessage is one key whose earliest open message cannot be claimed now: it is leased, waits for its retry
// or its next step, or is parked. It holds no payload and no error text.
type BlockedMessage struct {
	Destination  string
	ResourceKey  string
	State        State
	Attempts     int64
	FailureCode  string
	AvailableAt  time.Time
	BlockedSince time.Time
}

// Blocked lists the keys that cannot progress now, oldest first, once each (ST-SUP-RP01-LC06): a key whose earliest
// message that is not done or discarded is leased, parked, or waits out a delay after a failure. A key that is
// ready to claim is not listed. limit is 1 to MaxBatchSize.
func (q *Queue) Blocked(ctx context.Context, limit int) ([]BlockedMessage, error) {
	if limit < 1 || limit > MaxBatchSize {
		return nil, errors.New("outbox: the blocked limit is out of its bounds")
	}
	if q == nil || q.db == nil {
		return nil, errors.New("outbox: the queue needs a database")
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	rows, err := q.db.QueryContext(ctx, `WITH `+nowCTE+`
		SELECT m.destination, m.resource_key, m.state, m.attempts, m.failure_code, m.available_at, m.created_at
		FROM stego_outbox.messages AS m, now
		WHERE m.state NOT IN ('done', 'discarded')
			AND NOT EXISTS (SELECT 1 FROM stego_outbox.messages AS earlier
				WHERE earlier.destination = m.destination AND earlier.resource_key = m.resource_key
					AND earlier.sequence < m.sequence AND earlier.state NOT IN ('done', 'discarded'))
			AND (m.state IN ('dispatching', 'outcome_unknown', 'reconciling', 'parked')
				OR (m.lease_until IS NOT NULL AND m.lease_until > now.t)
				OR (m.failure_code <> '' AND m.available_at > now.t))
		ORDER BY m.sequence LIMIT $2`, q.now(), limit)
	if err != nil {
		return nil, canceled(ctx, err)
	}
	defer rows.Close()
	var blocked []BlockedMessage
	for rows.Next() {
		var item BlockedMessage
		var state string
		if err := rows.Scan(&item.Destination, &item.ResourceKey, &state, &item.Attempts, &item.FailureCode,
			&item.AvailableAt, &item.BlockedSince); err != nil {
			return nil, canceled(ctx, err)
		}
		item.State = State(state)
		blocked = append(blocked, item)
	}
	if err := rows.Err(); err != nil {
		return nil, canceled(ctx, err)
	}
	return blocked, nil
}

// DeadLetter is one parked message that waits for a person (ST-SUP-RP01-LC16). It holds no payload.
type DeadLetter struct {
	ID          uuid.UUID
	Destination string
	ResourceKey string
	Kind        string
	ParkedFrom  State
	Attempts    int64
	FailureCode string
	CreatedAt   time.Time
	ParkedAt    time.Time
}

// DeadLetters lists the parked messages, oldest first, and no message in another state (ST-SUP-RP01-LC16).
func (q *Queue) DeadLetters(ctx context.Context, limit int) ([]DeadLetter, error) {
	if limit < 1 || limit > MaxBatchSize {
		return nil, errors.New("outbox: the dead letter limit is out of its bounds")
	}
	if q == nil || q.db == nil {
		return nil, errors.New("outbox: the queue needs a database")
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	rows, err := q.db.QueryContext(ctx, `SELECT id, destination, resource_key, kind, parked_from, attempts,
		failure_code, created_at, parked_at
		FROM stego_outbox.messages WHERE state = 'parked' ORDER BY sequence LIMIT $1`, limit)
	if err != nil {
		return nil, canceled(ctx, err)
	}
	defer rows.Close()
	var dead []DeadLetter
	for rows.Next() {
		var item DeadLetter
		var from string
		if err := rows.Scan(&item.ID, &item.Destination, &item.ResourceKey, &item.Kind, &from, &item.Attempts,
			&item.FailureCode, &item.CreatedAt, &item.ParkedAt); err != nil {
			return nil, canceled(ctx, err)
		}
		item.ParkedFrom = State(from)
		dead = append(dead, item)
	}
	if err := rows.Err(); err != nil {
		return nil, canceled(ctx, err)
	}
	return dead, nil
}

// purge removes at most limit parked messages that were parked before olderThan, only of the destinations in
// purgeable, and never one that left outcome unknown or reconciling (ST-SUP-RP01-LC16). A discarded message is not
// parked, so purge never removes one.
func (q *Queue) purge(ctx context.Context, limit int, olderThan time.Duration, purgeable []string) (int64, error) {
	if limit < 1 || limit > MaxBatchSize || olderThan < 0 || olderThan > 90*24*time.Hour {
		return 0, errors.New("outbox: the purge limits are out of their bounds")
	}
	names, err := json.Marshal(purgeable)
	if err != nil {
		return 0, err
	}
	if purgeable == nil {
		names = []byte("[]")
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	result, err := q.db.ExecContext(ctx, `WITH `+nowCTE+`
		DELETE FROM stego_outbox.messages WHERE id IN (
			SELECT m.id FROM stego_outbox.messages AS m, now
			WHERE m.state = 'parked' AND m.parked_from = 'dispatching'
				AND m.parked_at <= now.t - $3 * interval '1 millisecond'
				AND m.destination IN (SELECT pg_catalog.jsonb_array_elements_text($4::jsonb))
			ORDER BY m.sequence LIMIT $2 FOR UPDATE OF m SKIP LOCKED)`,
		q.now(), limit, olderThan.Milliseconds(), string(names))
	if err != nil {
		return 0, canceled(ctx, err)
	}
	return result.RowsAffected()
}

// PurgeDone removes at most limit messages that reached done before olderThan. A done message holds back no key and
// keeps no rule; its row only serves the delivery status of a while. A discarded message stays.
func (q *Queue) PurgeDone(ctx context.Context, limit int, olderThan time.Duration) (int64, error) {
	if limit < 1 || limit > MaxBatchSize || olderThan < 0 || olderThan > 90*24*time.Hour {
		return 0, errors.New("outbox: the purge limits are out of their bounds")
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	result, err := q.db.ExecContext(ctx, `WITH `+nowCTE+`
		DELETE FROM stego_outbox.messages WHERE id IN (
			SELECT m.id FROM stego_outbox.messages AS m, now
			WHERE m.state = 'done' AND m.done_at <= now.t - $3 * interval '1 millisecond'
			ORDER BY m.sequence LIMIT $2 FOR UPDATE OF m SKIP LOCKED)`,
		q.now(), limit, olderThan.Milliseconds())
	if err != nil {
		return 0, canceled(ctx, err)
	}
	return result.RowsAffected()
}

func changed(ctx context.Context, result sql.Result, err error) (bool, error) {
	if err != nil {
		return false, canceled(ctx, err)
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

// canceled returns the cancellation of ctx in the place of err when ctx ended, so a canceled queue operation
// returns the cancellation as such (ST-SUP-RC18-OC24).
func canceled(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil && err != nil {
		return ctxErr
	}
	return err
}

func validateMessage(message Message) error {
	if message.ID == uuid.Nil || !labelPattern.MatchString(message.Destination) || !labelPattern.MatchString(message.Kind) {
		return errors.New("outbox: the message identity, destination or kind is not valid")
	}
	if !validText(message.ResourceKey, MaxResourceKeyBytes) || message.ResourceKey == "" {
		return errors.New("outbox: the resource key is not valid")
	}
	if message.OutgoingKey != "" && !validText(message.OutgoingKey, MaxKeyBytes) {
		return errors.New("outbox: the outgoing idempotency key is not valid")
	}
	if !validText(message.Principal, MaxKeyBytes) || !validText(message.Origin, MaxKeyBytes) {
		return errors.New("outbox: the staging principal or origin is not valid")
	}
	if len(message.Payload) == 0 || len(message.Payload) > MaxPayloadBytes || !utf8.Valid(message.Payload) {
		return errors.New("outbox: the payload size or encoding is not valid")
	}
	decoder := json.NewDecoder(bytes.NewReader(message.Payload))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("outbox: the payload is not one JSON object")
	}
	if err := validateJSONContainer(decoder, '{', 1); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("outbox: the payload has data after the object")
	}
	return nil
}

// validText accepts valid UTF-8 of at most limit bytes with no NUL, so the text reaches a text column whole.
func validText(value string, limit int) bool {
	return len(value) <= limit && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func validateJSONContainer(decoder *json.Decoder, opener json.Delim, depth int) error {
	if depth > 32 {
		return errors.New("outbox: the payload nests deeper than 32 levels")
	}
	keys := make(map[string]bool)
	for decoder.More() {
		if opener == '{' {
			token, err := decoder.Token()
			if err != nil {
				return errors.New("outbox: the payload is not valid JSON")
			}
			key, ok := token.(string)
			if !ok || keys[key] || strings.ContainsRune(key, 0) {
				return errors.New("outbox: the payload has a duplicate or invalid member")
			}
			keys[key] = true
		}
		token, err := decoder.Token()
		if err != nil {
			return errors.New("outbox: the payload is not valid JSON")
		}
		if text, ok := token.(string); ok && strings.ContainsRune(text, 0) {
			// PostgreSQL keeps no NUL in a jsonb text value.
			return errors.New("outbox: the payload holds a NUL character")
		}
		if delimiter, ok := token.(json.Delim); ok {
			if delimiter != '{' && delimiter != '[' {
				return errors.New("outbox: the payload is not valid JSON")
			}
			if err := validateJSONContainer(decoder, delimiter, depth+1); err != nil {
				return err
			}
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return errors.New("outbox: the payload is not valid JSON")
	}
	if opener == '{' && closing != json.Delim('}') || opener == '[' && closing != json.Delim(']') {
		return errors.New("outbox: the payload is not valid JSON")
	}
	return nil
}
