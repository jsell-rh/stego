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

	"github.com/jsell-rh/stego/registry/runtime/internal/catalogfirst"
	"github.com/jsell-rh/stego/registry/runtime/lease"
	"github.com/jsell-rh/stego/registry/runtime/manifest"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
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
	// RecordSchema is the schema of the audit record that the writer, the chain check and the anchorer name each
	// table with, so no relation of the same name earlier on the search path stands in for it (ST-STD-TXN-R19). The
	// migration job makes the record there, as Config.Schema does when it is empty.
	RecordSchema = "public"
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
	// AnchorPeriod is the anchor period of the audit-log component: a stored anchor boundary may be at most one
	// period ahead of the database clock.
	AnchorPeriod time.Duration
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
// timeout, with EXECUTE for the runtime role only. The text runs again with no error on a database that it made:
// each table is made only when it is missing, each function is replaced, and row-level security, each trigger and
// each policy are read in the catalog first and made again only when they differ, so the migration job can run on
// every release (ST-SUP-RP05-LC10) and a run with nothing to change takes no strong lock on a table (ST-SUP-RP14-LC03). Run it in one
// transaction.
func DDL(c Config) (string, error) {
	if c.Schema == "" {
		c.Schema = "public"
	}
	if !plainName.MatchString(c.Schema) || !plainName.MatchString(c.RuntimeRole) {
		return "", errors.New("audit: the schema and the runtime role must be plain lower-case names")
	}
	if c.AnchorPeriod < time.Second {
		return "", errors.New("audit: the anchor period is less than one second")
	}
	if c.RetentionMonths < 1 {
		return "", errors.New("audit: the retention is less than one month")
	}
	if c.clock == "" {
		c.clock = "now()"
	}
	return render(c), nil
}

// render gives the DDL text of c with each of its names and values.
func render(c Config) string {
	if c.clock == "" {
		c.clock = "now()"
	}
	return strings.NewReplacer(
		"{s}", c.Schema,
		"{role}", c.RuntimeRole,
		"{retention}", strconv.Itoa(c.RetentionMonths),
		"{period}", strconv.FormatInt(int64(c.AnchorPeriod/time.Second), 10)+" seconds",
		"{clock}", c.clock,
		"{events}", EventsTable,
		"{heads}", HeadsTable,
		"{bounds}", BoundariesTable,
		"{checks}", CheckpointsTable,
		"{maintain}", MaintainFunction,
	).Replace(ddlText)
}

// tenantWall is the expression of the policy tenant_wall of each audit table, as pg_get_expr gives it.
const tenantWall = catalogfirst.TenantForm

// walled gives the row-level security and the policy tenant_wall of the audit table table, each read in the catalog
// first, so a run of the migration job that changes nothing takes no lock on the table that conflicts with a write
// of the running release; a missing or changed policy is made again. The triggers of the audit tables are read in
// the catalog first in the same way.
func walled(table string) string {
	relation := "'{s}." + table + "'"
	return catalogfirst.Unless(catalogfirst.RowSecurity(relation), "ALTER TABLE {s}."+table+" ENABLE ROW LEVEL SECURITY;\nALTER TABLE {s}."+table+" FORCE ROW LEVEL SECURITY;") +
		catalogfirst.Unless(catalogfirst.Policy{Relation: relation, Name: "tenant_wall", Command: catalogfirst.EveryCommand, Using: tenantWall, Check: tenantWall}.Holds(),
			"DROP POLICY IF EXISTS tenant_wall ON {s}."+table+";\nCREATE POLICY tenant_wall ON {s}."+table+"\n"+
				"  USING (tenant_id = current_setting('app.tenant', true)) WITH CHECK (tenant_id = current_setting('app.tenant', true));")
}

// Manifest gives what the code needs of the audit record that DDL makes in the schema of c (ST-SUP-RC01-OC12):
// the four tables with row-level security enabled and forced and their policy tenant_wall, and the triggers that
// reject a change of the events table and of the checkpoints and that guard the anchor boundaries, each with the
// source of its function as DDL makes it for the anchor period of c. The service runtime checks it before the
// service serves, so an audit record that a change of the database left open to an edit, or to another tenant,
// stops the start.
func Manifest(c Config) (manifest.Manifest, error) {
	s := c.Schema
	if s == "" {
		s = "public"
	}
	if !plainName.MatchString(s) {
		return manifest.Manifest{}, errors.New("audit: the schema must be a plain lower-case name")
	}
	if c.AnchorPeriod < time.Second {
		return manifest.Manifest{}, errors.New("audit: the anchor period is less than one second")
	}
	// The source of audit_maintain holds the retention, so the manifest needs the retention of the migration.
	if c.RetentionMonths < 1 {
		return manifest.Manifest{}, errors.New("audit: the retention is less than one month")
	}
	c.Schema = s
	text := render(c)
	function := func(name string) (manifest.Function, error) {
		head := "CREATE OR REPLACE FUNCTION " + s + "." + name + "() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$"
		_, rest, found := strings.Cut(text, head)
		source, _, closed := strings.Cut(rest, "$$")
		if !found || !closed {
			return manifest.Function{}, fmt.Errorf("audit: the DDL has no trigger function %s", name)
		}
		return manifest.Function{Name: s + "." + name, Language: "plpgsql", Source: source, Config: []string{"search_path=pg_catalog, pg_temp"}}, nil
	}
	reject, err := function("audit_reject_change")
	if err != nil {
		return manifest.Manifest{}, err
	}
	guard, err := function("audit_boundary_guard")
	if err != nil {
		return manifest.Manifest{}, err
	}
	wall := []manifest.Policy{{Name: "tenant_wall", Command: catalogfirst.EveryCommand, Using: tenantWall, WithCheck: tenantWall}}
	rejecting := func(table string) []manifest.Trigger {
		return []manifest.Trigger{
			{Name: table + "_reject_row", Timing: "BEFORE", Row: true, Events: []string{"UPDATE", "DELETE"}, Function: reject},
			{Name: table + "_reject_statement", Timing: "BEFORE", Events: []string{"UPDATE", "DELETE", "TRUNCATE"}, Function: reject},
		}
	}
	// audit_maintain is the SECURITY DEFINER function that makes and drops the partitions (ST-SUP-RP05-LC10).
	maintainHead := "CREATE OR REPLACE FUNCTION " + s + "." + MaintainFunction + "()"
	_, rest, found := strings.Cut(text, maintainHead)
	_, body, opened := strings.Cut(rest, "AS $$")
	source, _, closed := strings.Cut(body, "$$")
	if !found || !opened || !closed {
		return manifest.Manifest{}, fmt.Errorf("audit: the DDL has no function %s", MaintainFunction)
	}
	// EXECUTE goes to the runtime role only, which runs the maintenance loop.
	if !plainName.MatchString(c.RuntimeRole) {
		return manifest.Manifest{}, errors.New("audit: the manifest needs the runtime role, a plain lower-case name")
	}
	maintain := manifest.Function{Name: s + "." + MaintainFunction, Language: "plpgsql", Source: source, SecurityDefiner: true,
		Config: []string{"search_path=pg_catalog, pg_temp", "lock_timeout=2s"}, Execute: []string{c.RuntimeRole},
		Returns: "TABLE(step_left boolean, checkpoint_tenant text, checkpoint_seq bigint, checkpoint_hash bytea)"}
	return manifest.Manifest{Functions: []manifest.Function{maintain}, Tables: []manifest.Table{
		{Schema: s, Name: EventsTable, Partitioned: true, RowSecurity: true, Policies: wall, Triggers: rejecting(EventsTable),
			// The primary key of the DDL: on a table with row-level security, each unique index is the manifest's.
			Indexes: []manifest.Index{{Name: EventsTable + "_pkey", Columns: []string{"tenant_id", "seq", "recorded_at"}}},
			// The check constraints of the DDL: on a table with row-level security, each one is the manifest's.
			Constraints: []manifest.Constraint{
				{Name: EventsTable + "_actor_kind_check", Kind: manifest.KindCheck,
					Definition: "CHECK ((actor_kind = ANY (ARRAY['user'::text, 'service'::text, 'system'::text, 'impersonated_by'::text])))"},
				{Name: EventsTable + "_outcome_check", Kind: manifest.KindCheck, Definition: "CHECK ((outcome = ANY (ARRAY['ok'::text, 'denied'::text, 'failed'::text])))"},
				{Name: EventsTable + "_hash_version_check", Kind: manifest.KindCheck, Definition: "CHECK ((hash_version = ANY (ARRAY[1, 2, 3])))"},
				{Name: EventsTable + "_check", Kind: manifest.KindCheck, Definition: "CHECK (((hash_version >= 2) OR (operation = ''::text)))"},
				{Name: EventsTable + "_check1", Kind: manifest.KindCheck, Definition: "CHECK (((hash_version = 3) OR (on_behalf_of = ''::text)))"},
			},
			// The columns with a default of the DDL; Record writes every other column.
			Columns: []manifest.Column{{Name: "operation", Type: "text", Default: "''::text"}, {Name: "on_behalf_of", Type: "text", Default: "''::text"},
				{Name: "hash_version", Type: "smallint", Default: "1"}},
			// audit_create_partition gives each partition a statement trigger of its own; the row triggers are clones.
			PartitionTriggers: []manifest.Trigger{{Name: "_reject_statement", Timing: "BEFORE", Events: []string{"UPDATE", "DELETE", "TRUNCATE"},
				Function: reject}}},
		{Schema: s, Name: HeadsTable, RowSecurity: true, Policies: wall, Indexes: []manifest.Index{{Name: HeadsTable + "_pkey", Columns: []string{"tenant_id"}}}},
		{Schema: s, Name: BoundariesTable, RowSecurity: true, Policies: wall, Indexes: []manifest.Index{{Name: BoundariesTable + "_pkey", Columns: []string{"tenant_id"}}},
			Triggers: []manifest.Trigger{
				{Name: BoundariesTable + "_guard", Timing: "BEFORE", Row: true, Events: []string{"INSERT", "UPDATE"}, Function: guard}}},
		{Schema: s, Name: CheckpointsTable, RowSecurity: true, Policies: wall, Triggers: rejecting(CheckpointsTable),
			Indexes: []manifest.Index{{Name: CheckpointsTable + "_pkey", Columns: []string{"tenant_id", "seq"}}},
			Columns: []manifest.Column{{Name: "written_at", Type: "timestamp with time zone", Default: "now()"}}},
	}}, nil
}

var ddlText = `CREATE TABLE IF NOT EXISTS {s}.{events} (
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
  operation text NOT NULL DEFAULT '',
  on_behalf_of text NOT NULL DEFAULT '',
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
  hash_version smallint NOT NULL DEFAULT 1 CHECK (hash_version IN (1, 2, 3)),
  prev_hash bytea NOT NULL,
  hash bytea NOT NULL,
  CHECK (hash_version >= 2 OR operation = ''),
  CHECK (hash_version = 3 OR on_behalf_of = ''),
  PRIMARY KEY (tenant_id, seq, recorded_at)
) PARTITION BY RANGE (recorded_at);
CREATE TABLE IF NOT EXISTS {s}.{heads} (
  tenant_id text PRIMARY KEY,
  seq bigint NOT NULL,
  head bytea NOT NULL,
  last_recorded_at timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS {s}.{bounds} (
  tenant_id text PRIMARY KEY,
  boundary timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS {s}.{checks} (
  tenant_id text NOT NULL,
  seq bigint NOT NULL,
  hash bytea NOT NULL,
  partition_name text NOT NULL,
  written_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, seq)
);
CREATE OR REPLACE FUNCTION {s}.audit_reject_change() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  RAISE EXCEPTION 'the audit table rejects %', TG_OP USING ERRCODE = 'insufficient_privilege';
END
$$;
` + catalogfirst.Unless(catalogfirst.Trigger{Relation: "'{s}.{events}'", Name: "{events}_reject_row", Function: "'{s}.audit_reject_change()'",
	Type: catalogfirst.Before | catalogfirst.Row | catalogfirst.Update | catalogfirst.Delete}.Holds(), `CREATE OR REPLACE TRIGGER {events}_reject_row BEFORE UPDATE OR DELETE ON {s}.{events}
  FOR EACH ROW EXECUTE FUNCTION {s}.audit_reject_change();`) + `
` + catalogfirst.Unless(catalogfirst.Trigger{Relation: "'{s}.{events}'", Name: "{events}_reject_statement", Function: "'{s}.audit_reject_change()'",
	Type: catalogfirst.Before | catalogfirst.Update | catalogfirst.Delete | catalogfirst.Truncate}.Holds(), `CREATE OR REPLACE TRIGGER {events}_reject_statement BEFORE UPDATE OR DELETE OR TRUNCATE ON {s}.{events}
  FOR EACH STATEMENT EXECUTE FUNCTION {s}.audit_reject_change();`) + `
CREATE OR REPLACE FUNCTION {s}.audit_boundary_guard() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  IF TG_OP = 'UPDATE' AND NEW.boundary < OLD.boundary THEN
    RAISE EXCEPTION 'an anchor boundary only grows' USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.boundary > now() + interval '{period}' THEN
    RAISE EXCEPTION 'an anchor boundary is at most one period ahead of now' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END
$$;
` + catalogfirst.Unless(catalogfirst.Trigger{Relation: "'{s}.{bounds}'", Name: "{bounds}_guard", Function: "'{s}.audit_boundary_guard()'",
	Type: catalogfirst.Before | catalogfirst.Row | catalogfirst.Insert | catalogfirst.Update}.Holds(), `CREATE OR REPLACE TRIGGER {bounds}_guard BEFORE INSERT OR UPDATE ON {s}.{bounds}
  FOR EACH ROW EXECUTE FUNCTION {s}.audit_boundary_guard();`) + `
` + catalogfirst.Unless(catalogfirst.Trigger{Relation: "'{s}.{checks}'", Name: "{checks}_reject_row", Function: "'{s}.audit_reject_change()'",
	Type: catalogfirst.Before | catalogfirst.Row | catalogfirst.Update | catalogfirst.Delete}.Holds(), `CREATE OR REPLACE TRIGGER {checks}_reject_row BEFORE UPDATE OR DELETE ON {s}.{checks}
  FOR EACH ROW EXECUTE FUNCTION {s}.audit_reject_change();`) + `
` + catalogfirst.Unless(catalogfirst.Trigger{Relation: "'{s}.{checks}'", Name: "{checks}_reject_statement", Function: "'{s}.audit_reject_change()'",
	Type: catalogfirst.Before | catalogfirst.Update | catalogfirst.Delete | catalogfirst.Truncate}.Holds(), `CREATE OR REPLACE TRIGGER {checks}_reject_statement BEFORE UPDATE OR DELETE OR TRUNCATE ON {s}.{checks}
  FOR EACH STATEMENT EXECUTE FUNCTION {s}.audit_reject_change();`) + `
` + walled("{events}") + `` + walled("{heads}") + `` + walled("{bounds}") + `` + walled("{checks}") + `REVOKE ALL ON {s}.{events}, {s}.{heads}, {s}.{bounds}, {s}.{checks} FROM PUBLIC;
GRANT USAGE ON SCHEMA {s} TO {role};
GRANT SELECT, INSERT ON {s}.{events} TO {role};
GRANT SELECT, INSERT, UPDATE ON {s}.{heads}, {s}.{bounds} TO {role};
GRANT SELECT ON {s}.{checks} TO {role};
CREATE OR REPLACE FUNCTION {s}.audit_create_partition(month date) RETURNS boolean LANGUAGE plpgsql
  SET search_path = pg_catalog, pg_temp AS $$
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
CREATE OR REPLACE FUNCTION {s}.{maintain}()
  RETURNS TABLE (step_left boolean, checkpoint_tenant text, checkpoint_seq bigint, checkpoint_hash bytea)
  LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, pg_temp
  SET lock_timeout = '2s' AS $$
DECLARE
  saved text := current_setting('app.tenant', true);
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
  -- The caller's transaction ends with the tenant setting it had (ST-SUP-RP04-LC03).
  PERFORM set_config('app.tenant', coalesce(saved, ''), true);
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

// LeaseOf returns the controller lease that this process holds now, or nil when it holds none. The controller
// gives it from its lease.Client loop (ST-SUP-RP09).
type LeaseOf func() *lease.Held

// held returns the held lease, or nil when there is none or it stopped.
func held(of LeaseOf) *lease.Held {
	if of == nil {
		return nil
	}
	h := of()
	if h == nil || h.Err() != nil {
		return nil
	}
	return h
}

// TenantsOf returns the tenants to visit, such as tenant.List of the service's schema (ST-SUP-RP04-LC07).
type TenantsOf func(ctx context.Context) ([]tenant.Tenant, error)

// AnchorConfig configures the anchor job.
type AnchorConfig struct {
	// Period is the anchor period that the audit-log component sets. The boundaries are its multiples since the
	// Unix epoch.
	Period time.Duration
	// Lease is the controller lease: the job publishes only while it is held, and fences each anchor.
	Lease LeaseOf
	// Tenants lists the tenants of the service.
	Tenants TenantsOf
	// DB is the runtime role's pool: the job opens one transaction per tenant with tenant.Begin.
	DB *sql.DB
	// Outbox enqueues the anchor messages for the audit sink.
	Outbox Enqueuer
}

// Anchorer is the anchor job (ST-SUP-RP05-LC09). The controller calls Tick with its clock; the job owns no loop.
type Anchorer struct {
	cfg     AnchorConfig
	mu      sync.Mutex
	missed  map[string]string
	late    atomic.Int64
	skipped atomic.Int64
}

// NewAnchorer returns the anchor job.
func NewAnchorer(cfg AnchorConfig) (*Anchorer, error) {
	switch {
	case cfg.Period <= 0:
		return nil, errors.New("audit: the anchor period is not positive")
	case cfg.Lease == nil:
		return nil, errors.New("audit: the anchor job needs the controller lease")
	case cfg.Tenants == nil || cfg.DB == nil:
		return nil, errors.New("audit: the anchor job needs the tenants and the database")
	case cfg.Outbox == nil:
		return nil, errors.New("audit: the anchor job needs an outbox")
	}
	return &Anchorer{cfg: cfg, missed: map[string]string{}}, nil
}

// Boundary returns the last anchor boundary at or before now: a multiple of the period since the Unix epoch.
func (a *Anchorer) Boundary(now time.Time) time.Time {
	p := a.cfg.Period.Nanoseconds()
	n := now.UnixNano()
	return time.Unix(0, n-((n%p)+p)%p).UTC()
}

const (
	selectBoundary = `SELECT boundary FROM ` + RecordSchema + "." + BoundariesTable + ` WHERE tenant_id = $1`
	upsertBoundary = `INSERT INTO ` + RecordSchema + "." + BoundariesTable + ` (tenant_id, boundary) VALUES ($1, $2)
 ON CONFLICT (tenant_id) DO UPDATE SET boundary = EXCLUDED.boundary`
	selectLast = `SELECT seq, hash FROM ` + RecordSchema + "." + EventsTable + ` WHERE tenant_id = $1 ORDER BY seq DESC LIMIT 1`
)

// tickResult is what one tenant's transaction of a Tick did; it counts only once the commit succeeded.
type tickResult struct {
	late, skipped int64
	future        bool
}

// Tick runs the anchor job once at the time now, while this process holds the controller lease. It visits each
// tenant in a transaction of its own, which tenant.Begin opens and tenant.Commit commits (ST-SUP-RP04-LC03,
// ST-SUP-RP04-LC05), and goes on after a tenant fails. For each tenant whose last anchored boundary is before the
// due boundary, or that has no stored boundary, it publishes at once a periodic anchor with the hash of the
// chain's row with the highest seq and that seq as the count, read from the audit table and never from the
// chain-head row, stores the due boundary, and fences the transaction with the lease (lease.Held.Fence) as its
// last statement. A chain with no row is not anchored, and its due boundary is stored. When more than one boundary
// is due, it publishes one anchor for the latest, and each earlier due boundary, which gets no anchor of its own,
// counts as skipped (SkippedBoundaries). An anchor published after the next boundary is due, on the job's clock,
// counts as late (LateAnchors). A tenant whose due boundary is not anchored, or whose stored boundary is more than one
// period after the due one, makes the job unhealthy; a tenant's miss clears only after its transaction commits. A
// process that does not hold the lease clears its misses: the holder reports them.
//
// The job does not use tenant.Each: Each gives its function a tenant.Querier, which hides the *sql.Tx that
// lease.Held.Fence takes.
func (a *Anchorer) Tick(ctx context.Context, now time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	h := held(a.cfg.Lease)
	if h == nil {
		clear(a.missed)
		return nil
	}
	tenants, err := a.cfg.Tenants(ctx)
	if err != nil {
		return err
	}
	due := a.Boundary(now)
	start := time.Now()
	var errs []error
	for _, t := range tenants {
		// The time of this anchor on the job's clock: the tick's time plus the time the tick ran so far.
		at := now.Add(time.Since(start))
		res, err := a.anchorOne(ctx, h, t, due, at)
		tid := t.ID()
		switch {
		case err != nil:
			a.missed[tid] = "audit anchor missed for the tenant " + tid + " at the boundary " + timeText(due)
			errs = append(errs, err)
		case res.future:
			a.missed[tid] = "the stored anchor boundary of the tenant " + tid + " is more than one period after " + timeText(due)
		default:
			delete(a.missed, tid)
			a.late.Add(res.late)
			a.skipped.Add(res.skipped)
		}
		if errors.Is(err, lease.ErrFenced) || errors.Is(err, lease.ErrStopped) {
			break
		}
	}
	return errors.Join(errs...)
}

func (a *Anchorer) anchorOne(ctx context.Context, h *lease.Held, t tenant.Tenant, due, at time.Time) (tickResult, error) {
	var res tickResult
	tx, err := tenant.Begin(ctx, a.cfg.DB, t, nil)
	if err != nil {
		return res, err
	}
	rollback := func(err error) (tickResult, error) {
		_ = tx.Rollback()
		return tickResult{}, err
	}
	tid := t.ID()
	var stored time.Time
	hasStored := true
	switch err := tx.QueryRowContext(ctx, selectBoundary, tid).Scan(&stored); {
	case errors.Is(err, sql.ErrNoRows):
		hasStored = false
	case err != nil:
		return rollback(stmtError(ctx, "anchor boundary read", err))
	}
	if hasStored && stored.After(due.Add(a.cfg.Period)) {
		res.future = true
		return res, tenant.Commit(ctx, tx, t)
	}
	if hasStored && !stored.Before(due) {
		return res, tenant.Commit(ctx, tx, t)
	}
	an := Anchor{Tenant: tid, Kind: AnchorPeriodic}
	switch err := tx.QueryRowContext(ctx, selectLast, tid).Scan(&an.Count, &an.Head); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return rollback(stmtError(ctx, "chain head read", err))
	default:
		key := "anchor/" + tid + "/" + timeText(due)
		if err := a.cfg.Outbox.Enqueue(ctx, tx, tid, SinkDestination, key, anchorMessage(an)); err != nil {
			return rollback(stmtError(ctx, "anchor enqueue", err))
		}
		if !at.Before(due.Add(a.cfg.Period)) {
			res.late = 1
		}
		if hasStored {
			if n := int64(due.Sub(stored)/a.cfg.Period) - 1; n > 0 {
				res.skipped = n
			}
		}
	}
	if _, err := tx.ExecContext(ctx, upsertBoundary, tid, due); err != nil {
		return rollback(stmtError(ctx, "anchor boundary write", err))
	}
	if _, err := h.Fence(ctx, tx); err != nil {
		return rollback(err)
	}
	return res, tenant.Commit(ctx, tx, t)
}

// LateAnchors returns how many boundaries the job anchored late.
func (a *Anchorer) LateAnchors() int64 { return a.late.Load() }

// SkippedBoundaries returns how many boundaries the job passed over, because a later boundary was due when it
// anchored.
func (a *Anchorer) SkippedBoundaries() int64 { return a.skipped.Load() }

// Health returns whether the anchor job is healthy and, when it is not, the reason, which names each tenant whose
// due boundary is not anchored or whose stored boundary is in the future.
func (a *Anchorer) Health() (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.missed) == 0 {
		return true, ""
	}
	parts := make([]string, 0, len(a.missed))
	for _, reason := range a.missed {
		parts = append(parts, reason)
	}
	slices.Sort(parts)
	return false, strings.Join(parts, "; ")
}

// MaintainConfig configures the maintenance loop.
type MaintainConfig struct {
	// Lease is the controller lease: the loop runs only while it is held, and fences each call.
	Lease LeaseOf
	// DB is the runtime role's pool; each call is a transaction on it with no tenant set.
	DB *sql.DB
	// Outbox enqueues the retention anchors for the audit sink.
	Outbox Enqueuer
	// Now is the controller's clock; nil is time.Now.
	Now func() time.Time
	// Schema is the schema of the audit record, as Config.Schema; empty is public.
	Schema string
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
	case cfg.DB == nil:
		return nil, errors.New("audit: the maintenance loop needs the database")
	case cfg.Outbox == nil:
		return nil, errors.New("audit: the maintenance loop needs an outbox")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Schema == "" {
		cfg.Schema = "public"
	}
	if !plainName.MatchString(cfg.Schema) {
		return nil, errors.New("audit: the schema must be a plain lower-case name")
	}
	return &Maintainer{cfg: cfg}, nil
}

// maintainCall is the call of audit_maintain in the schema of the audit record, by its schema, so no function of the
// same name earlier on the search path runs in its place.
func maintainCall(schema string) string {
	return `SELECT step_left, checkpoint_tenant, checkpoint_seq, checkpoint_hash FROM ` + schema + `.` + MaintainFunction + `()`
}

// maxSteps bounds one run, so a run ends also when the function always reports a step left.
const maxSteps = 64

const (
	setTenant    = `SELECT pg_catalog.set_config('app.tenant', $1, true)`
	readTenant   = `SELECT coalesce(pg_catalog.current_setting('app.tenant', true), '')`
	leaseTimeout = `SET LOCAL lock_timeout = '2s'`
	// partitionOf tells whether the events table of the schema {s} has the partition $1.
	partitionOf = `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_inherits i JOIN pg_catalog.pg_class c ON c.oid = i.inhrelid
 WHERE i.inhparent = '{s}.` + EventsTable + `'::pg_catalog.regclass AND c.relname = $1)`
)

// ErrTenantLeft is the error of a maintenance transaction that would commit with a tenant set.
var ErrTenantLeft = errors.New("audit: the maintenance transaction ends with a tenant set, so it is rolled back")

// noTenant runs fn in a transaction of db with no tenant set, and commits it only when the tenant setting is still
// empty, as tenant.Commit checks a tenant's transaction (ST-SUP-RP04-LC03).
func noTenant(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return stmtError(ctx, "begin", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	var current string
	if err := tx.QueryRowContext(ctx, readTenant).Scan(&current); err != nil {
		_ = tx.Rollback()
		return stmtError(ctx, "tenant check", err)
	}
	if current != "" {
		_ = tx.Rollback()
		return ErrTenantLeft
	}
	if err := tx.Commit(); err != nil {
		return stmtError(ctx, "commit", err)
	}
	return nil
}

// Run calls audit_maintain() while this process holds the controller lease, each call in its own short
// transaction with no tenant, until the function reports no step left. Each transaction first takes the lease row
// lock (lease.Held.Fence) under a lock timeout of 2 s, before the function takes its locks on the partitions, so
// a lost lease fails the call before it holds ACCESS EXCLUSIVE on a partition; it fences again as its last
// statement. For each retention checkpoint that a call writes, Run enqueues a retention anchor in the same
// transaction, with the tenant set to the checkpoint's tenant, and sets the tenant back to empty before the commit,
// which checks it. Then Run checks that the partition of the next month exists. It returns the number of calls.
func (m *Maintainer) Run(ctx context.Context) (int, error) {
	calls := 0
	for calls < maxSteps {
		h := held(m.cfg.Lease)
		if h == nil {
			return calls, nil
		}
		more := false
		err := noTenant(ctx, m.cfg.DB, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, leaseTimeout); err != nil {
				return stmtError(ctx, "lock timeout", err)
			}
			if _, err := h.Fence(ctx, tx); err != nil {
				return err
			}
			rows, err := tx.QueryContext(ctx, maintainCall(m.cfg.Schema))
			if err != nil {
				return stmtError(ctx, "maintenance call", err)
			}
			var anchors []Anchor
			for rows.Next() {
				var tn sql.NullString
				var seq sql.NullInt64
				var hash []byte
				if err := rows.Scan(&more, &tn, &seq, &hash); err != nil {
					rows.Close()
					return stmtError(ctx, "maintenance call", err)
				}
				if tn.Valid {
					anchors = append(anchors, Anchor{Tenant: tn.String, Kind: AnchorRetention, Head: hash, Count: seq.Int64})
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
			if len(anchors) > 0 {
				if _, err := tx.ExecContext(ctx, setTenant, ""); err != nil {
					return stmtError(ctx, "tenant setting", err)
				}
			}
			_, err = h.Fence(ctx, tx)
			return err
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
	if err := noTenant(ctx, m.cfg.DB, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, strings.ReplaceAll(partitionOf, "{s}", m.cfg.Schema), partitionName(next)).Scan(&ok); err != nil {
			return stmtError(ctx, "partition check", err)
		}
		return nil
	}); err != nil {
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
