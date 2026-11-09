package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"stego.dev/runtime/lease"
	"stego.dev/runtime/modes"
	"stego.dev/runtime/tenant"
)

// The names of the two jobs, as the controller mode registers them and as the health reason names them.
const (
	SnapshotJob = "ledger_snapshot"
	CheckJob    = "ledger_check"
)

// LeaseOf returns the controller lease that this process holds now, or nil when it holds none. The controller
// gives it from its lease.Client loop (ST-SUP-RP09).
type LeaseOf func() *lease.Held

// TenantsOf returns the tenants to visit, such as tenant.List of the service's schema (ST-SUP-RP04-LC07).
type TenantsOf func(ctx context.Context) ([]tenant.Tenant, error)

// JobsConfig configures the snapshot and ledger_check jobs.
type JobsConfig struct {
	// Ledgers are the declared ledgers; each ledger's SnapshotEvery sets its snapshot interval.
	Ledgers []*Ledger
	// Lease is the controller lease: the jobs work only while it is held, and fence each transaction.
	Lease LeaseOf
	// Tenants lists the tenants of the service.
	Tenants TenantsOf
	// DB is the runtime role's pool: the jobs open one transaction per tenant with tenant.Begin.
	DB *sql.DB
}

// Jobs are the snapshot and ledger_check jobs (ST-SUP-RP08-LC08). The controller mode registers Components; the
// jobs own no loop beyond them.
type Jobs struct {
	cfg JobsConfig

	mu       sync.Mutex
	failures []string // the findings of the last check, or its error
}

// NewJobs returns the jobs.
func NewJobs(cfg JobsConfig) (*Jobs, error) {
	switch {
	case len(cfg.Ledgers) == 0:
		return nil, errors.New("ledger: the jobs need the declared ledgers")
	case cfg.Lease == nil:
		return nil, errors.New("ledger: the jobs need the controller lease")
	case cfg.Tenants == nil || cfg.DB == nil:
		return nil, errors.New("ledger: the jobs need the tenants and the database")
	}
	for _, l := range cfg.Ledgers {
		if l == nil {
			return nil, errors.New("ledger: a ledger of the jobs is nil")
		}
	}
	return &Jobs{cfg: cfg}, nil
}

// held returns the held lease, or nil when there is none or it stopped.
func (j *Jobs) held() *lease.Held {
	h := j.cfg.Lease()
	if h == nil || h.Err() != nil {
		return nil
	}
	return h
}

// writeSnapshots inserts one snapshot row for each account of the ledger that has at least $3 posted entries after
// its latest snapshot and below the oldest running transaction ID, with the fencing token $4. The new snapshot is
// the latest one plus those entries.
const writeSnapshots = `WITH x AS (SELECT pg_snapshot_xmin(pg_current_snapshot()) AS xmin),
due AS (
 SELECT a.id, s.amount AS base, s.entry_count AS base_count, sum(` + signed + `) AS delta, count(*) AS n
 FROM {s}.accounts a
 LEFT JOIN LATERAL (SELECT amount, through_xid, entry_count FROM {s}.balance_snapshots b
  WHERE b.tenant_id = a.tenant_id AND b.ledger_id = a.ledger_id AND b.account_id = a.id
  ORDER BY through_xid DESC LIMIT 1) s ON true
 JOIN {s}.entries e ON e.tenant_id = a.tenant_id AND e.ledger_id = a.ledger_id AND e.account_id = a.id AND NOT e.pending
  AND (s.through_xid IS NULL OR e.txid >= s.through_xid) AND e.txid < (SELECT xmin FROM x)
 WHERE a.tenant_id = $1 AND a.ledger_id = $2
 GROUP BY a.id, s.amount, s.entry_count
 HAVING count(*) >= $3
)
INSERT INTO {s}.balance_snapshots (tenant_id, ledger_id, account_id, through_xid, amount, entry_count, fence_token)
SELECT $1, $2, due.id, (SELECT xmin FROM x), coalesce(due.base, 0) + due.delta, coalesce(due.base_count, 0) + due.n, $4
FROM due
RETURNING account_id`

// Snapshot runs the snapshot job once, while this process holds the controller lease: for each tenant, in a
// transaction of its own that tenant.Begin opens, fenced with the lease (lease.Held.Fence) before its first write,
// and that tenant.Commit commits, it writes one balance_snapshots row for each account of each ledger that has at
// least the declared number of posted entries after its latest snapshot (ST-SUP-RP08-LC08). Each row carries the
// holder's fencing token. It returns the number of rows written. A process that does not hold the lease does no
// work. A fenced or stopped lease stops the run with its error, and the tenant's transaction writes nothing.
func (j *Jobs) Snapshot(ctx context.Context) (int, error) {
	h := j.held()
	if h == nil {
		return 0, nil
	}
	tenants, err := j.cfg.Tenants(ctx)
	if err != nil {
		return 0, err
	}
	written := 0
	var errs []error
	for _, t := range tenants {
		n, err := j.snapshotOne(ctx, h, t)
		if err != nil {
			errs = append(errs, fmt.Errorf("ledger snapshot of the tenant %s: %w", t.ID(), err))
			if errors.Is(err, lease.ErrFenced) || errors.Is(err, lease.ErrStopped) {
				break
			}
			continue
		}
		written += n
	}
	return written, errors.Join(errs...)
}

func (j *Jobs) snapshotOne(ctx context.Context, h *lease.Held, t tenant.Tenant) (int, error) {
	tx, err := tenant.Begin(ctx, j.cfg.DB, t, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	token, err := h.Fence(ctx, tx)
	if err != nil {
		return 0, err
	}
	written := 0
	for _, l := range j.cfg.Ledgers {
		every := max(l.every, 1)
		rows, err := tx.QueryContext(ctx, l.q(writeSnapshots), t.ID(), l.id, every, token)
		if err != nil {
			return 0, fmt.Errorf("ledger %s: write the snapshots: %w", l.id, err)
		}
		for rows.Next() {
			written++
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return 0, fmt.Errorf("ledger %s: write the snapshots: %w", l.id, err)
		}
	}
	if err := tenant.Commit(ctx, tx, t); err != nil {
		return 0, err
	}
	return written, nil
}

// The three findings of the check.
const (
	// unbalancedLedgers reads the ledgers whose entries do not sum to zero, debits less credits.
	unbalancedLedgers = `SELECT ledger_id, sum(CASE side WHEN 'debit' THEN amount ELSE -amount END)::text FROM {s}.entries
 WHERE tenant_id = $1 AND ledger_id = ANY(SELECT jsonb_array_elements_text($2::jsonb))
 GROUP BY ledger_id HAVING sum(CASE side WHEN 'debit' THEN amount ELSE -amount END) <> 0 ORDER BY ledger_id`
	// wrongSnapshots reads the snapshots that differ from the recomputed sum of the posted entries they cover.
	wrongSnapshots = `SELECT b.ledger_id, b.account_id, b.through_xid::text, b.amount::text, coalesce((
  SELECT sum(` + signed + `) FROM {s}.entries e WHERE e.tenant_id = b.tenant_id AND e.ledger_id = b.ledger_id
  AND e.account_id = b.account_id AND NOT e.pending AND e.txid < b.through_xid), 0)::text AS recomputed
 FROM {s}.balance_snapshots b JOIN {s}.accounts a ON a.tenant_id = b.tenant_id AND a.ledger_id = b.ledger_id AND a.id = b.account_id
 WHERE b.tenant_id = $1 AND b.ledger_id = ANY(SELECT jsonb_array_elements_text($2::jsonb))
 AND b.amount <> coalesce((SELECT sum(` + signed + `) FROM {s}.entries e WHERE e.tenant_id = b.tenant_id
  AND e.ledger_id = b.ledger_id AND e.account_id = b.account_id AND NOT e.pending AND e.txid < b.through_xid), 0)
 ORDER BY b.ledger_id, b.account_id, b.through_xid`
	// expiredPending reads the pending transfers that are past their expiry and not voided or posted.
	expiredPending = `SELECT ledger_id, id::text, expires_at FROM {s}.transfers
 WHERE tenant_id = $1 AND ledger_id = ANY(SELECT jsonb_array_elements_text($2::jsonb))
 AND kind = 'pending' AND status = 'pending' AND expires_at <= $3 ORDER BY ledger_id, id`
)

// Check runs the ledger_check job once, while this process holds the controller lease: for each tenant, in a
// transaction of its own that tenant.Begin opens and that is fenced with the lease, it verifies that each ledger's
// entries sum to zero, that each snapshot equals the recomputed sum of the entries it covers, and that every
// pending transfer past its expiry, on the ledger's clock, is voided (ST-SUP-RP08-LC08). It returns an error that
// names each finding, and the jobs then report unhealthy (Health) until a check finds nothing. A process that does
// not hold the lease does no work and clears the findings: the holder reports them.
func (j *Jobs) Check(ctx context.Context) error {
	h := j.held()
	if h == nil {
		j.report(nil)
		return nil
	}
	tenants, err := j.cfg.Tenants(ctx)
	if err != nil {
		j.report([]string{"the tenant list cannot be read: " + err.Error()})
		return err
	}
	var findings []string
	var errs []error
	for _, t := range tenants {
		found, err := j.checkOne(ctx, h, t)
		findings = append(findings, found...)
		if err != nil {
			findings = append(findings, fmt.Sprintf("the tenant %s cannot be checked: %v", t.ID(), err))
			errs = append(errs, err)
			if errors.Is(err, lease.ErrFenced) || errors.Is(err, lease.ErrStopped) {
				break
			}
		}
	}
	j.report(findings)
	if len(findings) > 0 {
		errs = append(errs, fmt.Errorf("%s failed: %s", CheckJob, strings.Join(findings, "; ")))
	}
	return errors.Join(errs...)
}

func (j *Jobs) checkOne(ctx context.Context, h *lease.Held, t tenant.Tenant) ([]string, error) {
	tx, err := tenant.Begin(ctx, j.cfg.DB, t, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := h.Fence(ctx, tx); err != nil {
		return nil, err
	}
	ids := make([]string, len(j.cfg.Ledgers))
	for i, l := range j.cfg.Ledgers {
		ids[i] = l.id
	}
	list, err := jsonList(ids...)
	if err != nil {
		return nil, err
	}
	var findings []string
	// Every ledger shares one schema in a service; the first ledger names it.
	first := j.cfg.Ledgers[0]
	err = rowsOf(ctx, tx, first.q(unbalancedLedgers), []any{t.ID(), list}, func(rows *sql.Rows) error {
		var ledger, sum string
		if err := rows.Scan(&ledger, &sum); err != nil {
			return err
		}
		findings = append(findings, fmt.Sprintf("the ledger %s of the tenant %s does not sum to zero: debits less credits are %s", ledger, t.ID(), sum))
		return nil
	})
	if err != nil {
		return findings, err
	}
	err = rowsOf(ctx, tx, first.q(wrongSnapshots), []any{t.ID(), list}, func(rows *sql.Rows) error {
		var ledger, account, through, amount, recomputed string
		if err := rows.Scan(&ledger, &account, &through, &amount, &recomputed); err != nil {
			return err
		}
		findings = append(findings, fmt.Sprintf("the snapshot through_xid %s of the account %s in the ledger %s of the tenant %s holds %s, and its entries sum to %s",
			through, account, ledger, t.ID(), amount, recomputed))
		return nil
	})
	if err != nil {
		return findings, err
	}
	for _, l := range j.cfg.Ledgers {
		one, err := jsonList(l.id)
		if err != nil {
			return findings, err
		}
		err = rowsOf(ctx, tx, l.q(expiredPending), []any{t.ID(), one, l.now()}, func(rows *sql.Rows) error {
			var ledger, id string
			var expires time.Time
			if err := rows.Scan(&ledger, &id, &expires); err != nil {
				return err
			}
			findings = append(findings, fmt.Sprintf("the pending transfer %s in the ledger %s of the tenant %s expired at %s and is not voided",
				id, ledger, t.ID(), expires.UTC().Format(time.RFC3339)))
			return nil
		})
		if err != nil {
			return findings, err
		}
	}
	return findings, tenant.Commit(ctx, tx, t)
}

func jsonList(items ...string) (string, error) {
	b, err := json.Marshal(items)
	return string(b), err
}

func rowsOf(ctx context.Context, tx *sql.Tx, statement string, args []any, each func(*sql.Rows) error) error {
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := each(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (j *Jobs) report(findings []string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.failures = slices.Clone(findings)
}

// Health returns whether the ledger is healthy and, when it is not, the reason, which names ledger_check and each
// finding of the last check. A failed check is a health signal of the service (ST-SUP-RP08-LC08).
func (j *Jobs) Health() (bool, string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.failures) == 0 {
		return true, ""
	}
	return false, CheckJob + " failed: " + strings.Join(j.failures, "; ")
}

// Components returns the two jobs as components of the controller mode: each runs its job once at start and then
// every period of after, until its context ends. The generated code registers them for modes.Controller only, so
// the api and worker modes start neither (ST-SUP-RC31-OC12). A run's error is kept in Health for the check, and
// does not stop the component. after is the controller's clock; nil is time.After.
func (j *Jobs) Components(period time.Duration, after func(time.Duration) <-chan time.Time) []modes.Component {
	if after == nil {
		after = time.After
	}
	loop := func(run func(ctx context.Context) error) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			for {
				_ = run(ctx)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-after(period):
				}
			}
		}
	}
	return []modes.Component{
		{Name: SnapshotJob, Run: loop(func(ctx context.Context) error { _, err := j.Snapshot(ctx); return err })},
		{Name: CheckJob, Run: loop(j.Check)},
	}
}
