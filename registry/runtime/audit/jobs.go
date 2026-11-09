package audit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The tables and functions of the audit record. Each table is tenant-owned (ST-STD-TXN-R01).
const (
	// EventsTable holds the audit rows, partitioned by month on recorded_at.
	EventsTable = "audit_event"
	// HeadsTable holds the head of each tenant's chain: its seq, hash and last recorded_at. Record locks it.
	HeadsTable = "audit_chain_heads"
	// BoundariesTable holds the last anchored boundary of each tenant's chain (ST-SUP-RP05-LC09).
	BoundariesTable = "audit_anchor_boundaries"
	// CheckpointsTable holds the retention checkpoints of each tenant's chain (ST-SUP-RP05-LC11).
	CheckpointsTable = "audit_retention_checkpoints"
	// MaintainFunction is the owner function that maintains the partitions (ST-SUP-RP05-LC10).
	MaintainFunction = "audit_maintain"
	partitionPrefix  = EventsTable + "_p"
)

var plainName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// Config is what the DDL text needs. The table owner runs the text in the migration job, as OwnerRole, so the
// tables and the functions belong to that role (ST-SUP-RP04-LC06).
type Config struct {
	// Schema holds the audit tables; the empty schema is public.
	Schema string
	// RuntimeRole is the role of the running service.
	RuntimeRole string
	// RetentionMonths is the audit-log retention when the migration runs: audit_maintain drops each partition
	// whose whole month ends before the start of the current month minus this many months.
	RetentionMonths int

	// clock is the SQL expression of the current time in audit_maintain; tests set a fixed date.
	clock string
}

// DDL returns the statements that create the audit record (ST-SUP-RP05-LC10, ST-SUP-RP05-LC11): the events table
// partitioned by month on recorded_at with no default partition; the tables audit_chain_heads,
// audit_anchor_boundaries and audit_retention_checkpoints; a trigger that raises on UPDATE, DELETE and TRUNCATE on
// the events table, on each of its partitions and on the checkpoints; row-level security enabled and forced on the
// four tables with one policy on the transaction-local tenant setting; the grants of the runtime role
// (ST-STD-TXN-R06); and the owner function audit_maintain(), SECURITY DEFINER with a fixed search_path and a lock
// timeout, with EXECUTE for the runtime role only.
func DDL(c Config) (string, error) {
	if c.Schema == "" {
		c.Schema = "public"
	}
	if !plainName.MatchString(c.Schema) || !plainName.MatchString(c.RuntimeRole) {
		return "", errors.New("audit: the schema and the runtime role must be plain lower-case names")
	}
	if c.RetentionMonths < 1 {
		return "", errors.New("audit: the retention is less than one month")
	}
	if c.clock == "" {
		c.clock = "now()"
	}
	return strings.NewReplacer(
		"{s}", c.Schema,
		"{role}", c.RuntimeRole,
		"{retention}", strconv.Itoa(c.RetentionMonths),
		"{clock}", c.clock,
		"{events}", EventsTable,
		"{heads}", HeadsTable,
		"{bounds}", BoundariesTable,
		"{checks}", CheckpointsTable,
		"{maintain}", MaintainFunction,
	).Replace(ddlText), nil
}

const ddlText = `CREATE TABLE {s}.{events} (
  tenant_id text NOT NULL,
  seq bigint NOT NULL,
  id uuid NOT NULL,
  occurred_at timestamptz NOT NULL,
  recorded_at timestamptz NOT NULL,
  actor_id text NOT NULL,
  actor_kind text NOT NULL CHECK (actor_kind IN ('user', 'service', 'system', 'impersonated_by')),
  actor_ip text NOT NULL,
  user_agent text NOT NULL,
  action text NOT NULL,
  resource_type text NOT NULL,
  resource_id text NOT NULL,
  outcome text NOT NULL CHECK (outcome IN ('ok', 'denied', 'failed')),
  error_code text NOT NULL,
  request_id text NOT NULL,
  trace_id text NOT NULL,
  span_id text NOT NULL,
  idempotency_key text NOT NULL,
  before jsonb,
  after jsonb,
  patch jsonb,
  service text NOT NULL,
  service_version text NOT NULL,
  declaration_version text NOT NULL,
  prev_hash bytea NOT NULL,
  hash bytea NOT NULL,
  PRIMARY KEY (tenant_id, seq, recorded_at)
) PARTITION BY RANGE (recorded_at);
CREATE TABLE {s}.{heads} (
  tenant_id text PRIMARY KEY,
  seq bigint NOT NULL,
  head bytea NOT NULL,
  last_recorded_at timestamptz NOT NULL
);
CREATE TABLE {s}.{bounds} (
  tenant_id text PRIMARY KEY,
  boundary timestamptz NOT NULL
);
CREATE TABLE {s}.{checks} (
  tenant_id text NOT NULL,
  seq bigint NOT NULL,
  hash bytea NOT NULL,
  partition_name text NOT NULL,
  written_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, seq)
);
CREATE FUNCTION {s}.audit_reject_change() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  RAISE EXCEPTION 'the audit table rejects %', TG_OP USING ERRCODE = 'insufficient_privilege';
END
$$;
CREATE TRIGGER {events}_reject_row BEFORE UPDATE OR DELETE ON {s}.{events}
  FOR EACH ROW EXECUTE FUNCTION {s}.audit_reject_change();
CREATE TRIGGER {events}_reject_statement BEFORE UPDATE OR DELETE OR TRUNCATE ON {s}.{events}
  FOR EACH STATEMENT EXECUTE FUNCTION {s}.audit_reject_change();
CREATE TRIGGER {checks}_reject_row BEFORE UPDATE OR DELETE ON {s}.{checks}
  FOR EACH ROW EXECUTE FUNCTION {s}.audit_reject_change();
CREATE TRIGGER {checks}_reject_statement BEFORE UPDATE OR DELETE OR TRUNCATE ON {s}.{checks}
  FOR EACH STATEMENT EXECUTE FUNCTION {s}.audit_reject_change();
ALTER TABLE {s}.{events} ENABLE ROW LEVEL SECURITY;
ALTER TABLE {s}.{events} FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_wall ON {s}.{events}
  USING (tenant_id = current_setting('app.tenant', true)) WITH CHECK (tenant_id = current_setting('app.tenant', true));
ALTER TABLE {s}.{heads} ENABLE ROW LEVEL SECURITY;
ALTER TABLE {s}.{heads} FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_wall ON {s}.{heads}
  USING (tenant_id = current_setting('app.tenant', true)) WITH CHECK (tenant_id = current_setting('app.tenant', true));
ALTER TABLE {s}.{bounds} ENABLE ROW LEVEL SECURITY;
ALTER TABLE {s}.{bounds} FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_wall ON {s}.{bounds}
  USING (tenant_id = current_setting('app.tenant', true)) WITH CHECK (tenant_id = current_setting('app.tenant', true));
ALTER TABLE {s}.{checks} ENABLE ROW LEVEL SECURITY;
ALTER TABLE {s}.{checks} FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_wall ON {s}.{checks}
  USING (tenant_id = current_setting('app.tenant', true)) WITH CHECK (tenant_id = current_setting('app.tenant', true));
REVOKE ALL ON {s}.{events}, {s}.{heads}, {s}.{bounds}, {s}.{checks} FROM PUBLIC;
GRANT USAGE ON SCHEMA {s} TO {role};
GRANT SELECT, INSERT ON {s}.{events} TO {role};
GRANT SELECT, INSERT, UPDATE ON {s}.{heads}, {s}.{bounds} TO {role};
GRANT SELECT ON {s}.{checks} TO {role};
CREATE FUNCTION {s}.audit_create_partition(month date) RETURNS boolean LANGUAGE plpgsql
  SET search_path = pg_catalog, {s}, pg_temp AS $$
DECLARE
  name text := '{events}_p' || to_char(month, 'YYYYMM');
BEGIN
  IF to_regclass(quote_ident('{s}') || '.' || quote_ident(name)) IS NOT NULL THEN
    RETURN false;
  END IF;
  EXECUTE format('CREATE TABLE %I.%I PARTITION OF %I.{events} FOR VALUES FROM (%L) TO (%L)', '{s}', name, '{s}',
    to_char(month, 'YYYY-MM-DD') || ' 00:00:00+00', to_char(month + interval '1 month', 'YYYY-MM-DD') || ' 00:00:00+00');
  EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE OR TRUNCATE ON %I.%I FOR EACH STATEMENT EXECUTE FUNCTION %I.audit_reject_change()',
    name || '_reject_statement', '{s}', name, '{s}');
  RETURN true;
END
$$;
REVOKE ALL ON FUNCTION {s}.audit_create_partition(date) FROM PUBLIC;
CREATE FUNCTION {s}.{maintain}()
  RETURNS TABLE (step_left boolean, checkpoint_tenant text, checkpoint_seq bigint, checkpoint_hash bytea)
  LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, {s}, pg_temp
  SET lock_timeout = '2s' AS $$
DECLARE
  today date := (({clock}) AT TIME ZONE 'UTC')::date;
  cur date := date_trunc('month', today)::date;
  cutoff timestamp := date_trunc('month', today) - make_interval(months => {retention});
  m date;
  i int;
  done boolean := false;
  old_name text;
  tenants text[] := '{}';
  seqs bigint[] := '{}';
  hashes bytea[] := '{}';
  t record;
  h bytea;
  left_over boolean;
BEGIN
  FOR i IN 0..3 LOOP
    m := (cur + make_interval(months => i))::date;
    IF to_regclass(quote_ident('{s}') || '.' || quote_ident('{events}_p' || to_char(m, 'YYYYMM'))) IS NULL THEN
      PERFORM {s}.audit_create_partition(m);
      done := true;
      EXIT;
    END IF;
  END LOOP;
  IF NOT done THEN
    SELECT c.relname INTO old_name FROM pg_inherits inh JOIN pg_class c ON c.oid = inh.inhrelid
      WHERE inh.inhparent = '{s}.{events}'::regclass AND c.relname ~ '^{events}_p[0-9]{6}$'
        AND to_date(right(c.relname, 6), 'YYYYMM') + interval '1 month' <= cutoff
      ORDER BY c.relname LIMIT 1;
    IF old_name IS NOT NULL THEN
      FOR t IN EXECUTE format('SELECT tenant_id, max(seq) AS seq FROM %I.%I GROUP BY tenant_id ORDER BY tenant_id',
          '{s}', old_name) LOOP
        EXECUTE format('SELECT hash FROM %I.%I WHERE tenant_id = $1 AND seq = $2', '{s}', old_name)
          INTO h USING t.tenant_id, t.seq;
        PERFORM set_config('app.tenant', t.tenant_id, true);
        INSERT INTO {s}.{checks} (tenant_id, seq, hash, partition_name) VALUES (t.tenant_id, t.seq, h, old_name);
        tenants := tenants || t.tenant_id;
        seqs := seqs || t.seq;
        hashes := hashes || h;
      END LOOP;
      EXECUTE format('DROP TABLE %I.%I', '{s}', old_name);
    END IF;
  END IF;
  left_over := false;
  FOR i IN 0..3 LOOP
    m := (cur + make_interval(months => i))::date;
    IF to_regclass(quote_ident('{s}') || '.' || quote_ident('{events}_p' || to_char(m, 'YYYYMM'))) IS NULL THEN
      left_over := true;
    END IF;
  END LOOP;
  IF NOT left_over THEN
    left_over := EXISTS (SELECT 1 FROM pg_inherits inh JOIN pg_class c ON c.oid = inh.inhrelid
      WHERE inh.inhparent = '{s}.{events}'::regclass AND c.relname ~ '^{events}_p[0-9]{6}$'
        AND to_date(right(c.relname, 6), 'YYYYMM') + interval '1 month' <= cutoff);
  END IF;
  IF coalesce(array_length(tenants, 1), 0) = 0 THEN
    RETURN QUERY SELECT left_over, NULL::text, NULL::bigint, NULL::bytea;
  ELSE
    RETURN QUERY SELECT left_over, x.tenant, x.seq, x.hash
      FROM unnest(tenants, seqs, hashes) AS x(tenant, seq, hash);
  END IF;
END
$$;
REVOKE ALL ON FUNCTION {s}.{maintain}() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION {s}.{maintain}() TO {role};
`

func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func partitionName(month time.Time) string {
	return fmt.Sprintf("%s%04d%02d", partitionPrefix, month.Year(), int(month.Month()))
}

// Lease is the controller lease, as the lease runtime gives it (ST-SUP-RP09-LC03). Held tells whether this
// process holds the lease now. Fence fences a write in tx with the holder's token: it fails, with the lease
// runtime's fenced error, when the lease row holds another token, and holds the lease row until tx ends. The jobs
// call Fence as the last statement before COMMIT, so a holder that lost the lease while it worked commits nothing.
type Lease interface {
	Held() bool
	Fence(ctx context.Context, tx Tx) error
}

// EachTenant is the per-tenant iterator of the tenant runtime (ST-SUP-RP04-LC05): it calls fn once per tenant in
// one transaction of that tenant, commits the transaction when fn returns nil and rolls it back otherwise,
// continues with the next tenant after a failure, and reports every failure at the end.
type EachTenant func(ctx context.Context, fn func(ctx context.Context, tenant string, tx Tx) error) error

// AnchorConfig configures the anchor job.
type AnchorConfig struct {
	// Period is the anchor period that the audit-log component sets. The boundaries are its multiples since the
	// Unix epoch.
	Period time.Duration
	// Lease is the controller lease: the job publishes only while it is held, and fences each anchor.
	Lease Lease
	// Each opens one transaction per tenant.
	Each EachTenant
	// Outbox enqueues the anchor messages for the audit sink.
	Outbox Enqueuer
}

// Anchorer is the anchor job (ST-SUP-RP05-LC09). The controller calls Tick with its clock; the job owns no loop.
type Anchorer struct {
	cfg      AnchorConfig
	mu       sync.Mutex
	lastTick time.Time
	missed   map[string]time.Time
	late     atomic.Int64
}

// NewAnchorer returns the anchor job.
func NewAnchorer(cfg AnchorConfig) (*Anchorer, error) {
	switch {
	case cfg.Period <= 0:
		return nil, errors.New("audit: the anchor period is not positive")
	case cfg.Lease == nil:
		return nil, errors.New("audit: the anchor job needs the controller lease")
	case cfg.Each == nil:
		return nil, errors.New("audit: the anchor job needs the per-tenant iterator")
	case cfg.Outbox == nil:
		return nil, errors.New("audit: the anchor job needs an outbox")
	}
	return &Anchorer{cfg: cfg, missed: map[string]time.Time{}}, nil
}

// Boundary returns the last anchor boundary at or before now: a multiple of the period since the Unix epoch.
func (a *Anchorer) Boundary(now time.Time) time.Time {
	p := a.cfg.Period.Nanoseconds()
	n := now.UnixNano()
	return time.Unix(0, n-((n%p)+p)%p).UTC()
}

const (
	selectBoundary = `SELECT boundary FROM ` + BoundariesTable + ` WHERE tenant_id = $1`
	upsertBoundary = `INSERT INTO ` + BoundariesTable + ` (tenant_id, boundary) VALUES ($1, $2)
 ON CONFLICT (tenant_id) DO UPDATE SET boundary = EXCLUDED.boundary`
	selectLast = `SELECT seq, hash FROM ` + EventsTable + ` WHERE tenant_id = $1 ORDER BY seq DESC LIMIT 1`
)

// Tick runs the anchor job once at the time now, while this process holds the controller lease. For each tenant
// whose last anchored boundary is before the due boundary, it publishes at once a periodic anchor with the hash
// of the chain's row with the highest seq and that seq as the count, read from the audit table and never from the
// chain-head row, stores the due boundary, and fences the transaction with the lease. A boundary that it anchors
// after it was due at an earlier Tick, or at the first Tick after this process took the lease, counts as late
// (LateAnchors). A tenant whose due boundary is not anchored makes the job unhealthy until it is anchored. A
// tenant with no stored boundary starts at the due boundary.
func (a *Anchorer) Tick(ctx context.Context, now time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.cfg.Lease.Held() {
		a.lastTick = time.Time{}
		return nil
	}
	due := a.Boundary(now)
	first := a.lastTick.IsZero()
	lastTick := a.lastTick
	a.lastTick = now
	err := a.cfg.Each(ctx, func(ctx context.Context, tenant string, tx Tx) error {
		var stored time.Time
		switch err := tx.QueryRowContext(ctx, selectBoundary, tenant).Scan(&stored); {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx, upsertBoundary, tenant, due); err != nil {
				return a.miss(tenant, due, stmtError(ctx, "anchor boundary write", err))
			}
			if err := a.cfg.Lease.Fence(ctx, tx); err != nil {
				return err
			}
			delete(a.missed, tenant)
			return nil
		case err != nil:
			return a.miss(tenant, due, stmtError(ctx, "anchor boundary read", err))
		}
		if !stored.Before(due) {
			delete(a.missed, tenant)
			return nil
		}
		var an Anchor
		an.Tenant, an.Kind = tenant, AnchorPeriodic
		switch err := tx.QueryRowContext(ctx, selectLast, tenant).Scan(&an.Count, &an.Head); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return a.miss(tenant, due, stmtError(ctx, "chain head read", err))
		default:
			key := "anchor/" + tenant + "/" + timeText(due)
			if err := a.cfg.Outbox.Enqueue(ctx, tx, tenant, SinkDestination, key, anchorMessage(an)); err != nil {
				return a.miss(tenant, due, stmtError(ctx, "anchor enqueue", err))
			}
		}
		if _, err := tx.ExecContext(ctx, upsertBoundary, tenant, due); err != nil {
			return a.miss(tenant, due, stmtError(ctx, "anchor boundary write", err))
		}
		if err := a.cfg.Lease.Fence(ctx, tx); err != nil {
			return err
		}
		if first || !due.After(lastTick) {
			a.late.Add(1)
		}
		// Each boundary between the stored one and the due one was due and is anchored late by this anchor.
		if skipped := int64(due.Sub(stored)/a.cfg.Period) - 1; skipped > 0 {
			a.late.Add(skipped)
		}
		delete(a.missed, tenant)
		return nil
	})
	return err
}

func (a *Anchorer) miss(tenant string, due time.Time, err error) error {
	a.missed[tenant] = due
	return err
}

// LateAnchors returns how many boundaries the job anchored late.
func (a *Anchorer) LateAnchors() int64 { return a.late.Load() }

// Health returns whether the anchor job is healthy and, when it is not, the reason, which names each tenant and
// boundary that is due and not anchored.
func (a *Anchorer) Health() (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.missed) == 0 {
		return true, ""
	}
	var parts []string
	for tenant, b := range a.missed {
		parts = append(parts, "audit anchor missed for the tenant "+tenant+" at the boundary "+timeText(b))
	}
	slices.Sort(parts)
	return false, strings.Join(parts, "; ")
}

// MaintainConfig configures the maintenance loop.
type MaintainConfig struct {
	// Lease is the controller lease: the loop runs only while it is held, and fences each call.
	Lease Lease
	// RunTx opens the short transaction of each call; the maintenance calls give it the empty tenant, for a
	// transaction with no tenant set.
	RunTx RunTx
	// Outbox enqueues the retention anchors for the audit sink.
	Outbox Enqueuer
	// Now is the controller's clock; nil is time.Now.
	Now func() time.Time
}

// Maintainer is the maintenance loop (ST-SUP-RP05-LC10). The controller calls Run; the job owns no loop of its
// own beyond the steps of one run.
type Maintainer struct {
	cfg    MaintainConfig
	mu     sync.Mutex
	reason string
}

// NewMaintainer returns the maintenance loop.
func NewMaintainer(cfg MaintainConfig) (*Maintainer, error) {
	switch {
	case cfg.Lease == nil:
		return nil, errors.New("audit: the maintenance loop needs the controller lease")
	case cfg.RunTx == nil:
		return nil, errors.New("audit: the maintenance loop needs a RunTx")
	case cfg.Outbox == nil:
		return nil, errors.New("audit: the maintenance loop needs an outbox")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Maintainer{cfg: cfg}, nil
}

// maxSteps bounds one run, so a run ends also when the function always reports a step left.
const maxSteps = 64

const (
	callMaintain = `SELECT step_left, checkpoint_tenant, checkpoint_seq, checkpoint_hash FROM ` + MaintainFunction + `()`
	setTenant    = `SELECT set_config('app.tenant', $1, true)`
	partitionOf  = `SELECT EXISTS (SELECT 1 FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
 WHERE i.inhparent = '` + EventsTable + `'::regclass AND c.relname = $1)`
)

// Run calls audit_maintain() while this process holds the controller lease, each call in its own short
// transaction that it fences with the lease, until the function reports no step left. Each call does at most one
// step. For each retention checkpoint that a call writes, Run enqueues a retention anchor in the same
// transaction, with the tenant on the outbox row. Then Run checks that the partition of the next month exists. It
// returns the number of calls.
func (m *Maintainer) Run(ctx context.Context) (int, error) {
	calls := 0
	for calls < maxSteps {
		if !m.cfg.Lease.Held() {
			return calls, nil
		}
		more := false
		err := m.cfg.RunTx(ctx, "", func(ctx context.Context, tx Tx) error {
			rows, err := tx.QueryContext(ctx, callMaintain)
			if err != nil {
				return stmtError(ctx, "maintenance call", err)
			}
			var anchors []Anchor
			for rows.Next() {
				var tenant sql.NullString
				var seq sql.NullInt64
				var hash []byte
				if err := rows.Scan(&more, &tenant, &seq, &hash); err != nil {
					rows.Close()
					return stmtError(ctx, "maintenance call", err)
				}
				if tenant.Valid {
					anchors = append(anchors, Anchor{Tenant: tenant.String, Kind: AnchorRetention, Head: hash, Count: seq.Int64})
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return stmtError(ctx, "maintenance call", err)
			}
			for _, an := range anchors {
				if _, err := tx.ExecContext(ctx, setTenant, an.Tenant); err != nil {
					return stmtError(ctx, "tenant setting", err)
				}
				key := "retention/" + an.Tenant + "/" + strconv.FormatInt(an.Count, 10)
				if err := m.cfg.Outbox.Enqueue(ctx, tx, an.Tenant, SinkDestination, key, anchorMessage(an)); err != nil {
					return stmtError(ctx, "retention anchor enqueue", err)
				}
			}
			return m.cfg.Lease.Fence(ctx, tx)
		})
		calls++
		if err != nil {
			return calls, err
		}
		if !more {
			break
		}
	}
	return calls, m.Check(ctx)
}

// Check looks for the partition of the next month and makes the loop unhealthy, with a reason that names the
// month, when it is missing.
func (m *Maintainer) Check(ctx context.Context) error {
	next := monthStart(m.cfg.Now()).AddDate(0, 1, 0)
	var ok bool
	err := m.cfg.RunTx(ctx, "", func(ctx context.Context, tx Tx) error {
		if err := tx.QueryRowContext(ctx, partitionOf, partitionName(next)).Scan(&ok); err != nil {
			return stmtError(ctx, "partition check", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if ok {
		m.reason = ""
	} else {
		m.reason = "the audit partition for " + next.Format("2006-01") + " is missing"
	}
	return nil
}

// Health returns whether the partition of the next month exists, as the last Run or Check found, and the reason
// when it does not.
func (m *Maintainer) Health() (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reason == "", m.reason
}
