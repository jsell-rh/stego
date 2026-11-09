package audit

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// The tables of the audit record.
const (
	// EventsTable holds the audit rows, partitioned by month on recorded_at.
	EventsTable = "audit_events"
	// HeadsTable holds the head hash and the row count of each service chain.
	HeadsTable = "audit_chain_heads"
	// partitionPrefix starts the name of each monthly partition, such as audit_events_p202604.
	partitionPrefix = EventsTable + "_p"
	rejectFunction  = "audit_reject_change"
)

var roleName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// DDL returns the statements that create the audit tables (ST-SUP-RP05-LC10): the events table partitioned by
// month on recorded_at, the chain heads table, a trigger function that raises, triggers that call it on UPDATE,
// DELETE and TRUNCATE, and SELECT and INSERT on the events table for the runtime role. The table owner runs it, in
// the migration. Maintain creates the partitions, each with its own TRUNCATE trigger.
func DDL(runtimeRole string) (string, error) {
	if !roleName.MatchString(runtimeRole) {
		return "", fmt.Errorf("audit: %q is not a plain role name", runtimeRole)
	}
	return strings.NewReplacer("{events}", EventsTable, "{heads}", HeadsTable, "{reject}", rejectFunction,
		"{role}", runtimeRole).Replace(ddlText), nil
}

const ddlText = `CREATE TABLE {events} (
  seq bigint NOT NULL,
  id uuid NOT NULL,
  occurred_at timestamptz NOT NULL,
  recorded_at timestamptz NOT NULL,
  tenant_id text NOT NULL,
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
  PRIMARY KEY (id, recorded_at)
) PARTITION BY RANGE (recorded_at);
CREATE INDEX {events}_chain ON {events} (service, seq);
CREATE TABLE {heads} (
  service text PRIMARY KEY,
  head bytea NOT NULL,
  row_count bigint NOT NULL
);
CREATE FUNCTION {reject}() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'the audit table rejects %', TG_OP USING ERRCODE = 'insufficient_privilege';
END
$$;
CREATE TRIGGER {events}_reject_row BEFORE UPDATE OR DELETE ON {events}
  FOR EACH ROW EXECUTE FUNCTION {reject}();
CREATE TRIGGER {events}_reject_statement BEFORE UPDATE OR DELETE OR TRUNCATE ON {events}
  FOR EACH STATEMENT EXECUTE FUNCTION {reject}();
REVOKE ALL ON {events} FROM PUBLIC;
GRANT SELECT, INSERT ON {events} TO {role};
GRANT SELECT, INSERT, UPDATE ON {heads} TO {role};
`

func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func partitionName(month time.Time) string {
	return fmt.Sprintf("%s%04d%02d", partitionPrefix, month.Year(), int(month.Month()))
}

// Maintain is the partition maintenance job (ST-SUP-RP05-LC10). In tx, which belongs to the table owner, it
// creates the partitions of the month of now and of the next month when they are missing, each with a trigger
// that rejects UPDATE, DELETE and TRUNCATE on the partition itself, and drops every partition whose whole month
// ends before the retention: with a retention of 12 months in March 2026, the partitions before March 2025 go. It
// drops whole partitions and deletes no row.
func Maintain(ctx context.Context, tx Tx, now time.Time, retentionMonths int) error {
	if retentionMonths < 1 {
		return fmt.Errorf("audit: the retention is %d months, not one month or more", retentionMonths)
	}
	current := monthStart(now)
	for _, month := range []time.Time{current, current.AddDate(0, 1, 0)} {
		if err := createPartition(ctx, tx, month); err != nil {
			return err
		}
	}
	cutoff := current.AddDate(0, -retentionMonths, 0)
	parts, err := partitions(ctx, tx)
	if err != nil {
		return err
	}
	for _, p := range parts {
		if !p.month.AddDate(0, 1, 0).After(cutoff) {
			if _, err := tx.Exec(ctx, "DROP TABLE "+p.name); err != nil {
				return fmt.Errorf("audit: drop the partition %s: %w", p.name, err)
			}
		}
	}
	return nil
}

func createPartition(ctx context.Context, tx Tx, month time.Time) error {
	name := partitionName(month)
	stmt := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`, name,
		EventsTable, month.Format("2006-01-02 15:04:05Z07:00"), month.AddDate(0, 1, 0).Format("2006-01-02 15:04:05Z07:00"))
	if _, err := tx.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("audit: create the partition %s: %w", name, err)
	}
	trigger := fmt.Sprintf(`CREATE OR REPLACE TRIGGER %s_reject_statement BEFORE UPDATE OR DELETE OR TRUNCATE ON %s
 FOR EACH STATEMENT EXECUTE FUNCTION %s()`, name, name, rejectFunction)
	if _, err := tx.Exec(ctx, trigger); err != nil {
		return fmt.Errorf("audit: create the reject trigger of %s: %w", name, err)
	}
	return nil
}

type partition struct {
	name  string
	month time.Time
}

const selectPartitions = `SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
 WHERE i.inhparent = '` + EventsTable + `'::regclass`

func partitions(ctx context.Context, tx Tx) ([]partition, error) {
	rows, err := tx.Query(ctx, selectPartitions)
	if err != nil {
		return nil, fmt.Errorf("audit: list the partitions: %w", err)
	}
	defer rows.Close()
	var out []partition
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("audit: list the partitions: %w", err)
		}
		month, err := time.Parse("200601", strings.TrimPrefix(name, partitionPrefix))
		if err != nil || !strings.HasPrefix(name, partitionPrefix) {
			continue
		}
		out = append(out, partition{name: name, month: month})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit: list the partitions: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].month.Before(out[j].month) })
	return out, nil
}

// Lease tells whether this process holds the controller lease now. The lease runtime gives it.
type Lease interface {
	Held() bool
}

// AnchorConfig configures the anchor job.
type AnchorConfig struct {
	// Period is the anchor period that the audit-log component sets.
	Period time.Duration
	// Lease is the controller lease: the job publishes only while it is held.
	Lease Lease
	// RunTx opens the transaction in which the job reads the chain heads and enqueues the anchors.
	RunTx RunTx
	// Outbox enqueues the anchor messages for the audit sink.
	Outbox Enqueuer
}

// Anchorer is the anchor publisher (ST-SUP-RP05-LC09). The controller calls Tick with its clock; the job owns no
// loop.
type Anchorer struct {
	cfg    AnchorConfig
	mu     sync.Mutex
	next   time.Time
	reason string
}

// NewAnchorer returns the anchor job.
func NewAnchorer(cfg AnchorConfig) (*Anchorer, error) {
	switch {
	case cfg.Period <= 0:
		return nil, errors.New("audit: the anchor period is not positive")
	case cfg.Lease == nil:
		return nil, errors.New("audit: the anchor job needs the controller lease")
	case cfg.RunTx == nil:
		return nil, errors.New("audit: the anchor job needs a RunTx")
	case cfg.Outbox == nil:
		return nil, errors.New("audit: the anchor job needs an outbox")
	}
	return &Anchorer{cfg: cfg}, nil
}

const selectHeads = `SELECT service, head, row_count FROM ` + HeadsTable + ` ORDER BY service`

// Tick publishes one anchor for each anchor period that ended at or before now, while this process holds the
// controller lease: one outbox message per service chain with its head hash and row count. A period whose anchor
// cannot be enqueued is not passed over silently: the job becomes unhealthy with a reason that names the missed
// period, until an anchor is published again. Without the lease the job publishes nothing; the first Tick under
// the lease starts the first period.
func (a *Anchorer) Tick(ctx context.Context, now time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.cfg.Lease.Held() {
		a.next = time.Time{}
		return nil
	}
	if a.next.IsZero() {
		a.next = now.Add(a.cfg.Period)
		return nil
	}
	var errs []error
	for !a.next.After(now) {
		end := a.next
		a.next = a.next.Add(a.cfg.Period)
		if !a.cfg.Lease.Held() {
			a.next = time.Time{}
			break
		}
		if err := a.publish(ctx, end); err != nil {
			a.reason = fmt.Sprintf("audit anchor missed for the period that ended at %s: %v", timeText(end), err)
			errs = append(errs, errors.New(a.reason))
			continue
		}
		if len(errs) == 0 {
			a.reason = ""
		}
	}
	return errors.Join(errs...)
}

func (a *Anchorer) publish(ctx context.Context, end time.Time) error {
	return a.cfg.RunTx(ctx, func(ctx context.Context, tx Tx) error {
		rows, err := tx.Query(ctx, selectHeads)
		if err != nil {
			return fmt.Errorf("read the chain heads: %w", err)
		}
		var anchors []Anchor
		for rows.Next() {
			var an Anchor
			if err := rows.Scan(&an.Head.Service, &an.Head.Hash, &an.Count); err != nil {
				rows.Close()
				return fmt.Errorf("read the chain heads: %w", err)
			}
			anchors = append(anchors, an)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read the chain heads: %w", err)
		}
		for _, an := range anchors {
			key := "anchor/" + an.Head.Service + "/" + timeText(end)
			if err := a.cfg.Outbox.Enqueue(ctx, tx, SinkDestination, key, anchorMessage(an)); err != nil {
				return fmt.Errorf("enqueue the anchor of %s: %w", an.Head.Service, err)
			}
		}
		return nil
	})
}

// Health returns whether the anchor job is healthy and, when it is not, the reason, which names the missed
// anchor period.
func (a *Anchorer) Health() (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reason == "", a.reason
}
