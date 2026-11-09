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
	// MaxBatchSize is the most messages of one Enqueue call, and the largest limit of a claim, a list or a purge.
	MaxBatchSize = 32
	// MaxKeyBytes is the longest outgoing idempotency key, group, principal and origin, in bytes.
	MaxKeyBytes = 255
	// MaxResourceKeyBytes is the longest resource key, in bytes.
	MaxResourceKeyBytes = 256
)

// operationTimeout bounds each statement of the queue.
const operationTimeout = 5 * time.Second

// RelayRole is the NOLOGIN role that owns the relay functions, the only code that reads and changes messages of
// more than one tenant (ST-SUP-RP01-LC19).
const RelayRole = "stego_outbox_relay"

var rolePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// Migration returns the DDL text of the queue (ST-SUP-RP01-LC19). The migration job runs it as the table owner, in
// one transaction, as a simple query of several statements runs. The text creates RelayRole when it is missing, and
// makes the relay functions under SET ROLE RelayRole, so they belong to it and a later run replaces them. The owner
// therefore needs membership in RelayRole WITH SET TRUE, INHERIT TRUE, which a superuser or a role with ADMIN on
// RelayRole grants (GRANT stego_outbox_relay TO <owner> WITH SET TRUE, INHERIT TRUE); without it the text stops,
// before it makes the table, with an error that names that grant, and its transaction changes nothing. RelayRole gets CREATE on the schema only while the
// functions are made; the same text revokes it and resets the role.
//
// The table stego_outbox.messages holds the dispatch state, the attempt and conflict counts (ST-SUP-RC18-OC29), the
// tenant (ST-SUP-RC18-OC28), the outgoing idempotency key (ST-SUP-RP01-LC10), the group of the state hook
// (ST-SUP-RP01-LC25), the must_land flag and a short failure code, never an error text (ST-SUP-RP01-LC06).
// Row-level security is enabled and forced. runtimeRole, the request-path role, and workerRole, the role that the
// worker mode connects as, each have the tenant policy on the transaction-local tenant setting, USING and WITH
// CHECK, and SELECT, INSERT and UPDATE under it: Enqueue, Resolve and DeadLetters run in a transaction of one
// tenant. RelayRole has a policy for SELECT, UPDATE and DELETE and none for INSERT, and owns the SECURITY DEFINER
// functions that span tenants: claim, move, release, purge_parked, purge_done, blocked and oldest. Each has
// search_path = pg_catalog, pg_temp, names every object with its schema, checks its arguments and changes no
// setting. Only workerRole gets EXECUTE on them; runtimeRole and PUBLIC get none, so a request path can never claim,
// move or purge a message, nor pass a clock time of its own to a relay function.
func Migration(runtimeRole, workerRole string) (string, error) {
	for _, role := range []string{runtimeRole, workerRole} {
		if !rolePattern.MatchString(role) || role == RelayRole {
			return "", fmt.Errorf("outbox: %q is not a role name the migration accepts", role)
		}
	}
	if runtimeRole == workerRole {
		return "", errors.New("outbox: the worker role must be a role apart from the runtime role")
	}
	text := strings.ReplaceAll(migration, "{{runtime}}", `"`+runtimeRole+`"`)
	return strings.ReplaceAll(text, "{{worker}}", `"`+workerRole+`"`), nil
}

// The relay functions, by their signature, for the grants and the checks.
var relayFunctions = []string{
	"stego_outbox.claim(timestamptz, integer, uuid, bigint)",
	"stego_outbox.move(timestamptz, uuid, uuid, text, text, bigint, timestamptz, bigint, boolean, boolean, text, bigint)",
	"stego_outbox.release(timestamptz, uuid, uuid, bigint, text)",
	"stego_outbox.purge_parked(timestamptz, integer, bigint, jsonb)",
	"stego_outbox.purge_done(timestamptz, integer, bigint)",
	"stego_outbox.blocked(timestamptz, integer)",
	"stego_outbox.oldest(timestamptz)",
}

const migration = `CREATE SCHEMA IF NOT EXISTS stego_outbox;

DO $do$ BEGIN
	CREATE ROLE stego_outbox_relay NOLOGIN;
EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL;
END $do$;

DO $do$ BEGIN
	-- The owner makes the relay functions as stego_outbox_relay, so it needs SET and INHERIT membership in it.
	IF NOT (pg_catalog.pg_has_role(CURRENT_USER, 'stego_outbox_relay', 'SET')
		AND pg_catalog.pg_has_role(CURRENT_USER, 'stego_outbox_relay', 'USAGE')) THEN
		RAISE EXCEPTION 'outbox: the migration role % needs membership in stego_outbox_relay WITH SET TRUE, INHERIT TRUE', CURRENT_USER
			USING HINT = 'GRANT stego_outbox_relay TO ' || pg_catalog.quote_ident(CURRENT_USER) || ' WITH SET TRUE, INHERIT TRUE';
	END IF;
END $do$;

CREATE TABLE IF NOT EXISTS stego_outbox.messages (
	id uuid PRIMARY KEY,
	sequence bigint GENERATED ALWAYS AS IDENTITY UNIQUE NOT NULL,
	tenant_id text NOT NULL CHECK (octet_length(tenant_id) BETWEEN 1 AND 255),
	destination text NOT NULL CHECK (destination ~ '^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$'),
	resource_key text NOT NULL CHECK (octet_length(resource_key) BETWEEN 1 AND 256),
	kind text NOT NULL CHECK (kind ~ '^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$'),
	payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object' AND octet_length(payload::text) <= 131072),
	outgoing_key text NOT NULL CHECK (octet_length(outgoing_key) BETWEEN 1 AND 255),
	group_id text NOT NULL DEFAULT '' CHECK (octet_length(group_id) <= 255),
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
	last_token uuid,
	must_land boolean NOT NULL DEFAULT false,
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

CREATE INDEX IF NOT EXISTS messages_open_order ON stego_outbox.messages (tenant_id, destination, resource_key, sequence)
	WHERE state NOT IN ('done', 'discarded');
CREATE INDEX IF NOT EXISTS messages_due ON stego_outbox.messages (available_at, sequence)
	WHERE state IN ('pending', 'dispatching', 'outcome_unknown', 'reconciling');
CREATE INDEX IF NOT EXISTS messages_done ON stego_outbox.messages (done_at) WHERE state = 'done';

ALTER TABLE stego_outbox.messages ENABLE ROW LEVEL SECURITY;
ALTER TABLE stego_outbox.messages FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS messages_tenant ON stego_outbox.messages;
CREATE POLICY messages_tenant ON stego_outbox.messages TO {{runtime}}, {{worker}}
	USING (tenant_id = pg_catalog.current_setting('app.tenant', true))
	WITH CHECK (tenant_id = pg_catalog.current_setting('app.tenant', true));
DROP POLICY IF EXISTS messages_relay_select ON stego_outbox.messages;
CREATE POLICY messages_relay_select ON stego_outbox.messages FOR SELECT TO stego_outbox_relay USING (true);
DROP POLICY IF EXISTS messages_relay_update ON stego_outbox.messages;
CREATE POLICY messages_relay_update ON stego_outbox.messages FOR UPDATE TO stego_outbox_relay USING (true) WITH CHECK (true);
DROP POLICY IF EXISTS messages_relay_delete ON stego_outbox.messages;
CREATE POLICY messages_relay_delete ON stego_outbox.messages FOR DELETE TO stego_outbox_relay USING (true);

REVOKE ALL ON stego_outbox.messages FROM PUBLIC;
GRANT USAGE ON SCHEMA stego_outbox TO {{runtime}}, {{worker}};
GRANT USAGE, CREATE ON SCHEMA stego_outbox TO stego_outbox_relay;
GRANT SELECT, INSERT, UPDATE ON stego_outbox.messages TO {{runtime}}, {{worker}};
GRANT SELECT, UPDATE, DELETE ON stego_outbox.messages TO stego_outbox_relay;

-- The relay functions are made as their owner, so a later run of the same text replaces them.
SET ROLE stego_outbox_relay;

CREATE OR REPLACE FUNCTION stego_outbox.claim(p_now timestamptz, p_limit integer, p_token uuid, p_lease_ms bigint)
RETURNS TABLE (r_tenant text, r_id uuid, r_destination text, r_resource_key text, r_kind text, r_payload jsonb,
	r_outgoing_key text, r_principal text, r_origin text, r_group text, r_state text, r_attempts bigint,
	r_conflicts bigint, r_lookups bigint, r_lease_until timestamptz, r_dispatched_at timestamptz)
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $fn$
BEGIN
	IF p_limit IS NULL OR p_limit < 1 OR p_limit > 32 THEN
		RAISE EXCEPTION 'outbox: the claim limit is 1 to 32';
	END IF;
	IF p_lease_ms IS NULL OR p_lease_ms < 1000 OR p_lease_ms > 300000 THEN
		RAISE EXCEPTION 'outbox: the lease is 1 s to 5 min';
	END IF;
	IF p_token IS NULL THEN
		RAISE EXCEPTION 'outbox: the claim needs a lease token';
	END IF;
	RETURN QUERY
	WITH now AS (SELECT COALESCE(p_now, pg_catalog.clock_timestamp()) AS t),
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
				WHERE earlier.tenant_id = m.tenant_id AND earlier.destination = m.destination
					AND earlier.resource_key = m.resource_key
					AND earlier.sequence < m.sequence AND earlier.state NOT IN ('done', 'discarded'))
		ORDER BY m.sequence LIMIT p_limit FOR UPDATE OF m SKIP LOCKED
	)
	UPDATE stego_outbox.messages AS m
	SET lease_token = p_token, last_token = p_token, lease_until = now.t + p_lease_ms * interval '1 millisecond',
		state = CASE WHEN m.state = 'pending' THEN 'dispatching' ELSE m.state END,
		attempts = CASE WHEN m.state = 'pending' THEN m.attempts + 1 ELSE m.attempts END,
		dispatched_at = CASE WHEN m.state = 'pending' THEN now.t ELSE m.dispatched_at END
	FROM ready, now
	WHERE m.id = ready.id
	RETURNING m.tenant_id, m.id, m.destination, m.resource_key, m.kind, m.payload, m.outgoing_key, m.principal,
		m.origin, m.group_id, m.state, m.attempts, m.conflicts, m.lookups, m.lease_until, m.dispatched_at;
END
$fn$;

CREATE OR REPLACE FUNCTION stego_outbox.move(p_now timestamptz, p_id uuid, p_token uuid, p_from text, p_to text,
	p_delay_ms bigint, p_due timestamptz, p_attempts bigint, p_conflict boolean, p_lookup boolean, p_code text,
	p_lease_ms bigint)
RETURNS TABLE (r_result text, r_state text, r_delay double precision, r_attempts bigint, r_conflicts bigint,
	r_lookups bigint)
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
	v_now timestamptz := COALESCE(p_now, pg_catalog.clock_timestamp());
	v_tenant text := NULLIF(pg_catalog.current_setting('app.tenant', true), '');
BEGIN
	IF p_attempts IS NULL OR p_attempts NOT IN (-1, 0, 1) THEN
		RAISE EXCEPTION 'outbox: a move adds -1, 0 or 1 attempt';
	END IF;
	IF p_delay_ms IS NULL OR p_delay_ms < 0 OR p_delay_ms > 86400000 THEN
		RAISE EXCEPTION 'outbox: the delay is 0 to 24 h';
	END IF;
	IF p_due IS NOT NULL AND (p_due < v_now - interval '1 day' OR p_due > v_now + interval '2 hours') THEN
		RAISE EXCEPTION 'outbox: the due time is out of its bounds';
	END IF;
	IF p_to = 'dispatching' AND (p_lease_ms IS NULL OR p_lease_ms < 1000 OR p_lease_ms > 300000) THEN
		RAISE EXCEPTION 'outbox: the lease is 1 s to 5 min';
	END IF;
	IF p_code IS NULL OR p_code !~ '^([a-zA-Z0-9][a-zA-Z0-9._-]{0,127})?$' THEN
		RAISE EXCEPTION 'outbox: the failure code is not a short label';
	END IF;
	IF NOT EXISTS (SELECT 1 FROM (VALUES ('pending', 'dispatching'), ('dispatching', 'done'),
		('dispatching', 'pending'), ('dispatching', 'outcome_unknown'), ('dispatching', 'parked'),
		('outcome_unknown', 'dispatching'), ('outcome_unknown', 'reconciling'), ('outcome_unknown', 'parked'),
		('reconciling', 'done'), ('reconciling', 'pending'), ('reconciling', 'parked'),
		('reconciling', 'reconciling')) AS t (f, s) WHERE t.f = p_from AND t.s = p_to) THEN
		RAISE EXCEPTION 'outbox: a message in % cannot move to %', p_from, p_to;
	END IF;
	UPDATE stego_outbox.messages AS m SET
		state = p_to,
		lease_token = CASE WHEN p_to = 'dispatching' THEN m.lease_token END,
		lease_until = CASE WHEN p_to = 'dispatching' THEN v_now + p_lease_ms * interval '1 millisecond' END,
		available_at = COALESCE(p_due, v_now + p_delay_ms * interval '1 millisecond'),
		attempts = GREATEST(m.attempts + p_attempts, 0),
		conflicts = CASE WHEN p_conflict THEN m.conflicts + 1 ELSE 0 END,
		lookups = CASE WHEN p_lookup THEN m.lookups + 1 ELSE m.lookups END,
		dispatched_at = CASE WHEN p_to = 'dispatching' THEN v_now ELSE m.dispatched_at END,
		failure_code = p_code,
		parked_from = CASE WHEN p_to = 'parked' THEN p_from END,
		parked_at = CASE WHEN p_to = 'parked' THEN v_now END,
		done_at = CASE WHEN p_to = 'done' THEN v_now ELSE m.done_at END
	WHERE m.id = p_id AND m.lease_token = p_token AND m.lease_until > v_now AND m.state = p_from
		AND (v_tenant IS NULL OR m.tenant_id = v_tenant)
	RETURNING 'moved', m.state, pg_catalog.date_part('epoch', v_now - m.created_at), m.attempts, m.conflicts,
		m.lookups
	INTO r_result, r_state, r_delay, r_attempts, r_conflicts, r_lookups;
	IF FOUND THEN
		RETURN NEXT;
		RETURN;
	END IF;
	-- Only the holder of the message's last claim learns its state, so the function is no state oracle.
	SELECT 'not_moved', m.state, NULL, m.attempts, m.conflicts, m.lookups
	INTO r_result, r_state, r_delay, r_attempts, r_conflicts, r_lookups
	FROM stego_outbox.messages AS m WHERE m.id = p_id AND m.last_token = p_token
		AND (v_tenant IS NULL OR m.tenant_id = v_tenant);
	IF FOUND THEN
		RETURN NEXT;
	END IF;
	RETURN;
END
$fn$;

CREATE OR REPLACE FUNCTION stego_outbox.release(p_now timestamptz, p_id uuid, p_token uuid, p_delay_ms bigint,
	p_code text)
RETURNS bigint
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
	v_now timestamptz := COALESCE(p_now, pg_catalog.clock_timestamp());
	v_tenant text := NULLIF(pg_catalog.current_setting('app.tenant', true), '');
	v_count bigint;
BEGIN
	IF p_delay_ms IS NULL OR p_delay_ms < 0 OR p_delay_ms > 86400000 THEN
		RAISE EXCEPTION 'outbox: the delay is 0 to 24 h';
	END IF;
	IF p_code IS NULL OR p_code !~ '^([a-zA-Z0-9][a-zA-Z0-9._-]{0,127})?$' THEN
		RAISE EXCEPTION 'outbox: the failure code is not a short label';
	END IF;
	-- A release keeps the state, so it is only for a message whose next step is not a send.
	UPDATE stego_outbox.messages AS m SET lease_token = NULL, lease_until = NULL,
		available_at = v_now + p_delay_ms * interval '1 millisecond', failure_code = p_code
	WHERE m.id = p_id AND m.lease_token = p_token AND m.lease_until > v_now
		AND m.state IN ('outcome_unknown', 'reconciling') AND (v_tenant IS NULL OR m.tenant_id = v_tenant);
	GET DIAGNOSTICS v_count = ROW_COUNT;
	RETURN v_count;
END
$fn$;

CREATE OR REPLACE FUNCTION stego_outbox.purge_parked(p_now timestamptz, p_limit integer, p_older_ms bigint,
	p_destinations jsonb)
RETURNS bigint
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
	v_now timestamptz := COALESCE(p_now, pg_catalog.clock_timestamp());
	v_count bigint;
BEGIN
	IF p_limit IS NULL OR p_limit < 1 OR p_limit > 32 THEN
		RAISE EXCEPTION 'outbox: the purge limit is 1 to 32';
	END IF;
	IF p_older_ms IS NULL OR p_older_ms < 0 THEN
		RAISE EXCEPTION 'outbox: the purge age is not negative';
	END IF;
	IF p_destinations IS NULL OR pg_catalog.jsonb_typeof(p_destinations) <> 'array' THEN
		RAISE EXCEPTION 'outbox: the purge needs a list of destinations';
	END IF;
	DELETE FROM stego_outbox.messages WHERE id IN (
		SELECT m.id FROM stego_outbox.messages AS m
		WHERE m.state = 'parked' AND m.parked_from = 'dispatching' AND NOT m.must_land
			AND m.parked_at <= v_now - p_older_ms * interval '1 millisecond'
			AND m.destination IN (SELECT pg_catalog.jsonb_array_elements_text(p_destinations))
		ORDER BY m.sequence LIMIT p_limit FOR UPDATE OF m SKIP LOCKED);
	GET DIAGNOSTICS v_count = ROW_COUNT;
	RETURN v_count;
END
$fn$;

CREATE OR REPLACE FUNCTION stego_outbox.purge_done(p_now timestamptz, p_limit integer, p_older_ms bigint)
RETURNS bigint
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
	v_now timestamptz := COALESCE(p_now, pg_catalog.clock_timestamp());
	v_count bigint;
BEGIN
	IF p_limit IS NULL OR p_limit < 1 OR p_limit > 32 THEN
		RAISE EXCEPTION 'outbox: the purge limit is 1 to 32';
	END IF;
	IF p_older_ms IS NULL OR p_older_ms < 3600000 THEN
		RAISE EXCEPTION 'outbox: the retention is at least 1 hour';
	END IF;
	DELETE FROM stego_outbox.messages WHERE id IN (
		SELECT m.id FROM stego_outbox.messages AS m
		WHERE m.state = 'done' AND m.done_at <= v_now - p_older_ms * interval '1 millisecond'
		ORDER BY m.done_at LIMIT p_limit FOR UPDATE OF m SKIP LOCKED);
	GET DIAGNOSTICS v_count = ROW_COUNT;
	RETURN v_count;
END
$fn$;

CREATE OR REPLACE FUNCTION stego_outbox.blocked(p_now timestamptz, p_limit integer)
RETURNS TABLE (r_tenant text, r_destination text, r_resource_key text, r_state text, r_attempts bigint,
	r_failure_code text, r_available_at timestamptz, r_created_at timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $fn$
WITH now AS (SELECT COALESCE(p_now, pg_catalog.clock_timestamp()) AS t)
SELECT m.tenant_id, m.destination, m.resource_key, m.state, m.attempts, m.failure_code, m.available_at, m.created_at
FROM stego_outbox.messages AS m, now
WHERE m.state NOT IN ('done', 'discarded')
	AND NOT EXISTS (SELECT 1 FROM stego_outbox.messages AS earlier
		WHERE earlier.tenant_id = m.tenant_id AND earlier.destination = m.destination
			AND earlier.resource_key = m.resource_key
			AND earlier.sequence < m.sequence AND earlier.state NOT IN ('done', 'discarded'))
	AND (m.state IN ('dispatching', 'outcome_unknown', 'reconciling', 'parked')
		OR (m.lease_until IS NOT NULL AND m.lease_until > now.t)
		OR (m.failure_code <> '' AND m.available_at > now.t))
ORDER BY m.sequence LIMIT LEAST(GREATEST(p_limit, 1), 32)
$fn$;

CREATE OR REPLACE FUNCTION stego_outbox.oldest(p_now timestamptz)
RETURNS TABLE (r_destination text, r_age double precision)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $fn$
WITH now AS (SELECT COALESCE(p_now, pg_catalog.clock_timestamp()) AS t)
SELECT m.destination, GREATEST(pg_catalog.date_part('epoch', now.t - pg_catalog.min(m.created_at)), 0)
FROM stego_outbox.messages AS m, now WHERE m.state NOT IN ('done', 'discarded')
GROUP BY m.destination, now.t
$fn$;

DO $do$
DECLARE
	f text;
BEGIN
	FOREACH f IN ARRAY ARRAY[
		'stego_outbox.claim(timestamptz, integer, uuid, bigint)',
		'stego_outbox.move(timestamptz, uuid, uuid, text, text, bigint, timestamptz, bigint, boolean, boolean, text, bigint)',
		'stego_outbox.release(timestamptz, uuid, uuid, bigint, text)',
		'stego_outbox.purge_parked(timestamptz, integer, bigint, jsonb)',
		'stego_outbox.purge_done(timestamptz, integer, bigint)',
		'stego_outbox.blocked(timestamptz, integer)',
		'stego_outbox.oldest(timestamptz)'] LOOP
		EXECUTE 'REVOKE ALL ON FUNCTION ' || f || ' FROM PUBLIC';
		EXECUTE 'REVOKE ALL ON FUNCTION ' || f || ' FROM {{runtime}}';
		EXECUTE 'GRANT EXECUTE ON FUNCTION ' || f || ' TO {{worker}}';
	END LOOP;
END
$do$;

RESET ROLE;
-- CREATE was only for the relay functions: the relay role keeps USAGE.
REVOKE CREATE ON SCHEMA stego_outbox FROM stego_outbox_relay;
`

var labelPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

// Message is one message to enqueue. ID is its stable identity. OutgoingKey is the idempotency key that every
// attempt sends to the destination: the key that idempotency.Derive gives for the message's inputs, or, when it is
// empty, the message identity (ST-SUP-RP01-LC10). Group is the group identity that the destination's state hook
// gets (ST-SUP-RP01-LC25). Principal and Origin are the staging principal and the name of the staging command or
// worker, for a destination that a worker consumes (ST-SUP-RP10-LC16). Group, Principal and Origin may be empty.
type Message struct {
	ID          uuid.UUID
	Destination string
	ResourceKey string
	Kind        string
	Payload     json.RawMessage
	OutgoingKey string
	Group       string
	Principal   string
	Origin      string
	// MustLand is true for a message of a destination that must land, such as authz: the row keeps it, and no purge
	// removes the message while it is parked (ST-SUP-RP01-LC16).
	MustLand bool
}

// Receipt names one claim of a message: the message, the lease token of the claim, and the state the message was
// in when the receipt was given.
type Receipt struct {
	ID    uuid.UUID
	Token uuid.UUID
	State State
}

// Delivery is one claimed message. Tenant is read from the tenant column of the claimed row through the tenant
// runtime's row reader (ST-SUP-RP04-LC01), never from the payload; every later step of the message runs in a
// transaction of that tenant (ST-SUP-RP01-LC19).
type Delivery struct {
	Message
	Tenant       tenant.Tenant
	Receipt      Receipt
	Attempts     int64
	Conflicts    int64
	Lookups      int64
	LeaseUntil   time.Time
	DispatchedAt time.Time
	// leaseEnd is the end of the lease on the local monotonic clock: the time the claim was sent plus the lease, so
	// it is never later than the lease the database holds.
	leaseEnd time.Time
}

// Clock is the clock of the queue. A nil Clock is the database clock, clock_timestamp(); a test passes a clock that
// it advances. A lease.Clock satisfies it.
type Clock interface {
	Now() time.Time
}

// Options are the settings of a queue.
type Options struct {
	// Clock is the clock of the queue; nil is the database clock.
	Clock Clock
	// LiveEvents is true when the service declares a live event source. Only then does Enqueue send a notice
	// (ST-SUP-RP01-LC01); without one, a transaction with effects takes no notify lock at its commit.
	LiveEvents bool
}

// Queue is the queue of one database. Its pool connects as the runtime role of Migration.
type Queue struct {
	db     *sql.DB
	clock  Clock
	notify bool
}

// New returns the queue of db with options.
func New(db *sql.DB, options Options) (*Queue, error) {
	if db == nil {
		return nil, errors.New("outbox: the queue needs a database")
	}
	return &Queue{db: db, clock: options.Clock, notify: options.LiveEvents}, nil
}

// now is the first argument of every statement that reads the time: the clock's time, or NULL for the database
// clock. Every statement and relay function reads it as COALESCE($1, clock_timestamp()).
func (q *Queue) now() any {
	if q == nil || q.clock == nil {
		return nil
	}
	return q.clock.Now()
}

const nowCTE = `now AS (SELECT COALESCE($1::timestamptz, pg_catalog.clock_timestamp()) AS t)`

// Check verifies that the queue table and the relay functions are there and that this role may run them. It
// changes no row.
func (q *Queue) Check(ctx context.Context) error {
	if ctx == nil || q == nil || q.db == nil {
		return errors.New("outbox: the check needs a context and a database")
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	var present bool
	if err := q.db.QueryRowContext(ctx, `SELECT pg_catalog.to_regclass('stego_outbox.messages') IS NOT NULL`).Scan(&present); err != nil || !present {
		return canceled(ctx, errors.New("outbox: the queue table is not there"))
	}
	for _, f := range relayFunctions {
		var permitted bool
		if err := q.db.QueryRowContext(ctx, `SELECT pg_catalog.has_function_privilege($1, 'EXECUTE')`, f).Scan(&permitted); err != nil || !permitted {
			return canceled(ctx, fmt.Errorf("outbox: the relay function %s is not there or may not run", f))
		}
	}
	return nil
}

// ValidateMessage checks one message with no database access (ST-SUP-RC18-OC02).
func ValidateMessage(message Message) error { return validateMessage(message) }

// Enqueue writes messages inside the caller's transaction tx, which tenant.Begin opened (ST-SUP-RP01-LC01,
// ST-SUP-RP01-LC19). It checks every payload before it sends a statement, then takes a transaction-scoped advisory
// lock for each key, the tenant, the destination and the resource key, in a stable order before it allocates a
// sequence (ST-SUP-RC18-OC13, ST-SUP-RP01-LC03), and writes each message with the tenant of the transaction under
// the tenant policy. When the queue serves a live event source, each message sends a notice through pg_notify,
// which PostgreSQL delivers only after the commit. Call Enqueue once for each transaction with the whole batch and
// commit only when it succeeds; a rollback removes the messages and their notices with the application write.
func (q *Queue) Enqueue(ctx context.Context, tx *sql.Tx, messages ...Message) error {
	if tx == nil {
		return errors.New("outbox: Enqueue needs the caller's transaction")
	}
	if len(messages) == 0 || len(messages) > MaxBatchSize {
		return fmt.Errorf("outbox: a batch holds 1 to %d messages", MaxBatchSize)
	}
	seen := make(map[uuid.UUID]bool)
	for _, message := range messages {
		if err := validateMessage(message); err != nil {
			return err
		}
		if seen[message.ID] {
			return errors.New("outbox: a message ID is twice in the batch")
		}
		seen[message.ID] = true
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	var tenantID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT pg_catalog.current_setting('app.tenant', true)`).Scan(&tenantID); err != nil {
		return canceled(ctx, fmt.Errorf("outbox: read the tenant of the transaction: %w", err))
	}
	if tenantID.String == "" {
		return errors.New("outbox: the transaction holds no tenant; open it with tenant.Begin")
	}
	keys := make(map[int64]bool)
	for _, message := range messages {
		keys[lockKey(tenantID.String, message.Destination, message.ResourceKey)] = true
	}
	ordered := make([]int64, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
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
				group_id, principal, origin, must_land, created_at, available_at)
			SELECT $2, $3, $4, $5, $6, $7::jsonb, $8, $9, $10, $11, $12, now.t, now.t FROM now`,
			q.now(), message.ID, tenantID.String, message.Destination, message.ResourceKey, message.Kind,
			string(message.Payload), outgoing, message.Group, message.Principal, message.Origin, message.MustLand); err != nil {
			return canceled(ctx, err)
		}
		if !q.notify {
			continue
		}
		// The notice is the message identity alone: any role that can connect may LISTEN (ST-SUP-RP01-LC21).
		if _, err := tx.ExecContext(ctx, `SELECT pg_catalog.pg_notify($1, $2)`, NoticeChannel, message.ID.String()); err != nil {
			return canceled(ctx, err)
		}
	}
	return nil
}

// lockKey is the advisory lock of one key: the tenant, the destination and the resource key (ST-SUP-RP01-LC03).
func lockKey(tenantID, destination, resourceKey string) int64 {
	hash := sha256.Sum256([]byte("stego-outbox\x00" + tenantID + "\x00" + destination + "\x00" + resourceKey))
	return int64(binary.BigEndian.Uint64(hash[:8]))
}

// Claim leases at most limit messages for lease, one for each key, through the relay function claim, the one
// statement that spans tenants (ST-SUP-RP01-LC19, ST-STD-TXN-R14). The function first moves each dispatching
// message whose lease has expired to outcome unknown, then claims, with SKIP LOCKED, the earliest message of each
// key that is not done or discarded and is due, in pending, outcome unknown or reconciling (ST-SUP-RP01-LC15). Both
// parts read one snapshot, so a key whose message the first part moves is claimed in the next claim, not in this
// one. A claim of a pending message moves it to dispatching and counts an attempt in the same statement
// (ST-SUP-RP01-LC22). Claim returns no delivery when the statement, a row scan or the row iteration fails
// (ST-SUP-RC18-OC08); the database may still hold the leases it wrote, and they expire (ST-SUP-RC18-OC09). A
// cancellation of ctx is returned as such (ST-SUP-RC18-OC24).
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
	sent := time.Now()
	rows, err := q.db.QueryContext(ctx, `SELECT * FROM stego_outbox.claim($1, $2, $3, $4)`, q.now(), limit, token,
		lease.Milliseconds())
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
			&d.Principal, &d.Origin, &d.Group, &state, &d.Attempts, &d.Conflicts, &d.Lookups, &d.LeaseUntil, &dispatched)
		if err != nil {
			return nil, canceled(ctx, err)
		}
		d.Tenant = t
		d.DispatchedAt = dispatched.Time
		d.Receipt = Receipt{ID: d.ID, Token: token, State: State(state)}
		d.leaseEnd = sent.Add(lease)
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

// moved is the result of one move.
type moved struct {
	delay     time.Duration // from the enqueue to the move
	attempts  int64
	conflicts int64
	lookups   int64
}

// querier is the part of a *sql.DB or *sql.Tx that a move uses.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// move moves the claimed message of receipt to the state to through the relay function move, in tx when tx is not
// nil (ST-SUP-RP01-LC08). It refuses a transition that the state machine does not allow, with an error that names
// both states, and changes nothing. It returns false with no error when the lease of the receipt is no longer
// live, so a late result never counts (ST-SUP-RP01-LC02). On success it sets the receipt's state to the new state.
func (q *Queue) move(ctx context.Context, tx *sql.Tx, receipt *Receipt, to State, c change) (bool, moved, error) {
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
	var db querier = q.db
	if tx != nil {
		db = tx
	}
	var due any
	if c.dueAt != nil {
		due = *c.dueAt
	}
	var result, state string
	var seconds sql.NullFloat64
	var m moved
	err := db.QueryRowContext(ctx, `SELECT * FROM stego_outbox.move($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		q.now(), receipt.ID, receipt.Token, string(receipt.State), string(to), c.delay.Milliseconds(), due, c.attempts,
		c.conflict, c.lookup, c.failureCode, c.lease.Milliseconds()).Scan(&result, &state, &seconds, &m.attempts,
		&m.conflicts, &m.lookups)
	if errors.Is(err, sql.ErrNoRows) {
		return false, moved{}, nil
	}
	if err != nil {
		return false, moved{}, canceled(ctx, err)
	}
	if result != "moved" {
		// The stored state does not allow the move: name both states (ST-SUP-RP01-LC08). Otherwise the lease is gone.
		if err := Transition(State(state), to); err != nil {
			return false, moved{}, &movedOnError{err: err}
		}
		return false, moved{}, nil
	}
	m.delay = time.Duration(seconds.Float64 * float64(time.Second))
	receipt.State = to
	return true, m, nil
}

// movedOnError is the refusal of a move because the message moved on from the receipt's state, such as a message
// that another holder finished: the receipt's lease is gone. It names both states (ST-SUP-RP01-LC08).
type movedOnError struct{ err error }

func (e *movedOnError) Error() string { return e.err.Error() }
func (e *movedOnError) Unwrap() error { return e.err }

// release ends the lease of a claimed message and keeps its state, for a message that this worker cannot take a
// step for now, such as one whose destination it has no registration for.
func (q *Queue) release(ctx context.Context, tx *sql.Tx, receipt Receipt, delay time.Duration, failureCode string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	var db querier = q.db
	if tx != nil {
		db = tx
	}
	var n int64
	err := db.QueryRowContext(ctx, `SELECT stego_outbox.release($1, $2, $3, $4, $5)`, q.now(), receipt.ID,
		receipt.Token, delay.Milliseconds(), failureCode).Scan(&n)
	if err != nil {
		return false, canceled(ctx, err)
	}
	return n == 1, nil
}

// acknowledge moves a delivered message to done, only while the receipt's lease is live, outside a tenant
// transaction and with no state hook; only the tests of the package use it. False means the lease is gone. It keeps
// the attempt count (ST-SUP-RP01-LC22).
func (q *Queue) acknowledge(ctx context.Context, receipt Receipt) (bool, error) {
	ok, _, err := q.move(ctx, nil, &receipt, Done, change{})
	return ok, err
}

// retry moves a claimed message back to pending, outside a tenant transaction and with no state hook; only the tests
// of the package use it. It moves it after a known failure, due again after delay, with the short
// failure code failureCode, never an error text that can hold a credential or the payload (ST-SUP-RP01-LC06). It
// keeps the message's identity, payload and outgoing key, and its attempt count (ST-SUP-RC18-OC04). It never
// removes a message. False means the lease is gone.
func (q *Queue) retry(ctx context.Context, receipt Receipt, delay time.Duration, failureCode string) (bool, error) {
	if !labelPattern.MatchString(failureCode) {
		return false, errors.New("outbox: the failure code is not a short label")
	}
	ok, _, err := q.move(ctx, nil, &receipt, Pending, change{delay: delay, failureCode: failureCode})
	return ok, err
}

// BlockedMessage is one key whose earliest open message cannot be claimed now: it is leased, waits for its retry
// or its next step, or is parked. It holds no payload and no error text.
type BlockedMessage struct {
	TenantID     string
	Destination  string
	ResourceKey  string
	State        State
	Attempts     int64
	FailureCode  string
	AvailableAt  time.Time
	BlockedSince time.Time
}

// Blocked lists the keys that cannot progress now, oldest first, once each, through the relay function blocked
// (ST-SUP-RP01-LC06, ST-SUP-RP01-LC19). It is the operator's view of every tenant. limit is 1 to MaxBatchSize.
func (q *Queue) Blocked(ctx context.Context, limit int) ([]BlockedMessage, error) {
	if limit < 1 || limit > MaxBatchSize {
		return nil, errors.New("outbox: the blocked limit is out of its bounds")
	}
	if q == nil || q.db == nil {
		return nil, errors.New("outbox: the queue needs a database")
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	rows, err := q.db.QueryContext(ctx, `SELECT * FROM stego_outbox.blocked($1, $2)`, q.now(), limit)
	if err != nil {
		return nil, canceled(ctx, err)
	}
	defer rows.Close()
	var blocked []BlockedMessage
	for rows.Next() {
		var item BlockedMessage
		var state string
		if err := rows.Scan(&item.TenantID, &item.Destination, &item.ResourceKey, &state, &item.Attempts,
			&item.FailureCode, &item.AvailableAt, &item.BlockedSince); err != nil {
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

// DeadLetters lists the parked messages of the tenant of tx, oldest first, and no message in another state
// (ST-SUP-RP01-LC16). The dead letters are the parked rows of the queue table. It runs under the tenant policy in
// the caller's transaction, which tenant.Begin opened, so it lists no message of another tenant
// (ST-SUP-RP01-LC19).
func (q *Queue) DeadLetters(ctx context.Context, tx *sql.Tx, limit int) ([]DeadLetter, error) {
	if limit < 1 || limit > MaxBatchSize {
		return nil, errors.New("outbox: the dead letter limit is out of its bounds")
	}
	if tx == nil {
		return nil, errors.New("outbox: DeadLetters needs a transaction of one tenant")
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	rows, err := tx.QueryContext(ctx, `SELECT id, destination, resource_key, kind, parked_from, attempts,
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
// purgeable, and never one that left outcome unknown or reconciling, through the relay function purge_parked
// (ST-SUP-RP01-LC16). A discarded message is not parked, so purge never removes one.
func (q *Queue) purge(ctx context.Context, limit int, olderThan time.Duration, purgeable []string) (int64, error) {
	if limit < 1 || limit > MaxBatchSize || olderThan < 0 || olderThan > 90*24*time.Hour {
		return 0, errors.New("outbox: the purge limits are out of their bounds")
	}
	if purgeable == nil {
		purgeable = []string{}
	}
	names, err := json.Marshal(purgeable)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	var n int64
	err = q.db.QueryRowContext(ctx, `SELECT stego_outbox.purge_parked($1, $2, $3, $4::jsonb)`, q.now(), limit,
		olderThan.Milliseconds(), string(names)).Scan(&n)
	return n, canceled(ctx, err)
}

// MinRetention is the shortest retention of a done message (ST-SUP-RP01-LC24).
const MinRetention = time.Hour

// purgeDone removes at most limit messages that reached done before the retention, through the relay function
// purge_done (ST-SUP-RP01-LC24). The function itself keeps every done message younger than MinRetention.
func (q *Queue) purgeDone(ctx context.Context, limit int, retention time.Duration) (int64, error) {
	if limit < 1 || limit > MaxBatchSize || retention < MinRetention {
		return 0, errors.New("outbox: the purge limits are out of their bounds")
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	var n int64
	err := q.db.QueryRowContext(ctx, `SELECT stego_outbox.purge_done($1, $2, $3)`, q.now(), limit,
		retention.Milliseconds()).Scan(&n)
	return n, canceled(ctx, err)
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
	if !validText(message.Principal, MaxKeyBytes) || !validText(message.Origin, MaxKeyBytes) ||
		!validText(message.Group, MaxKeyBytes) {
		return errors.New("outbox: the staging principal, origin or group is not valid")
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
