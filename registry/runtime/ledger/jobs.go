package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"stego.dev/runtime/lease"
	"stego.dev/runtime/modes"
	"stego.dev/runtime/tenant"
)

// The names of the three jobs, as the controller mode registers them and as the health reason names them.
const (
	SnapshotJob = "ledger_snapshot"
	CheckJob    = "ledger_check"
	SweepJob    = "ledger_expiry_sweep"
)

// DefaultLockTimeout is the longest a statement of a job waits for a lock, unless the job's budget is shorter.
const DefaultLockTimeout = time.Second

// DefaultPeriod is the period of the three jobs when JobsConfig.Period is zero.
const DefaultPeriod = time.Minute

// SweepKey is the idempotency key of the void that the expiry sweep writes for the pending transfer
// (ST-SUP-RP08-LC11): expire:<transfer ID>.
func SweepKey(transferID string) string { return "expire:" + strings.ToLower(transferID) }

// LeaseOf returns the controller lease that this process holds now, or nil when it holds none. The controller
// gives it from its lease.Client loop (ST-SUP-RP09).
type LeaseOf func() *lease.Held

// TenantsOf returns the tenants to visit, such as tenant.List of the service's schema (ST-SUP-RP04-LC07).
type TenantsOf func(ctx context.Context) ([]tenant.Tenant, error)

// JobsConfig configures the snapshot, ledger_check and expiry sweep jobs.
type JobsConfig struct {
	// Ledgers are the declared ledgers, all in one schema.
	Ledgers []*Ledger
	// Lease is the controller lease: the jobs work only while it is held, and fence each transaction.
	Lease LeaseOf
	// Tenants lists the tenants of the service.
	Tenants TenantsOf
	// DB is the runtime role's pool: the jobs open one transaction per tenant with tenant.Begin.
	DB *sql.DB
	// LockTimeout is the longest a statement of a job waits for a lock; zero is DefaultLockTimeout. The job's budget
	// can make it shorter.
	LockTimeout time.Duration
	// Period is the period at which Components runs each job; zero is DefaultPeriod. Check gives an expired hold a
	// grace of one period before it reports that the sweep did not void it.
	Period time.Duration
}

// Jobs are the snapshot, ledger_check and expiry sweep jobs (ST-SUP-RP08-LC08, ST-SUP-RP08-LC11). The controller
// mode registers Components; the jobs own no loop beyond them.
type Jobs struct {
	cfg JobsConfig

	mu       sync.Mutex
	failures []string             // the findings of the last check, or its error
	checked  map[string]*position // the last checked position of each tenant's ledger
}

// position is how far Check has verified one tenant's ledger: the ledger sums to zero over the entries below
// ledgerXid, and the snapshot of each account at xid holds amount and is verified.
type position struct {
	ledgerXid uint64
	accounts  map[string]snapshotAt
}

type snapshotAt struct {
	xid    uint64
	amount int64
}

func (p *position) clone() *position {
	c := &position{ledgerXid: p.ledgerXid, accounts: make(map[string]snapshotAt, len(p.accounts))}
	for k, v := range p.accounts {
		c.accounts[k] = v
	}
	return c
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
	case cfg.LockTimeout < 0 || cfg.Period < 0:
		return nil, errors.New("ledger: the lock timeout and the period must not be negative")
	}
	for _, l := range cfg.Ledgers {
		if l == nil {
			return nil, errors.New("ledger: a ledger of the jobs is nil")
		}
		if l.schema != cfg.Ledgers[0].schema {
			return nil, errors.New("ledger: the ledgers of the jobs are in more than one schema")
		}
	}
	if cfg.LockTimeout == 0 {
		cfg.LockTimeout = DefaultLockTimeout
	}
	if cfg.Period == 0 {
		cfg.Period = DefaultPeriod
	}
	return &Jobs{cfg: cfg, checked: map[string]*position{}}, nil
}

// held returns the held lease, or nil when there is none or it stopped.
func (j *Jobs) held() *lease.Held {
	h := j.cfg.Lease()
	if h == nil || h.Err() != nil {
		return nil
	}
	return h
}

// ErrDeadline is the error of a job transaction that has no time left in its budget.
var ErrDeadline = errors.New("ledger: the job's transaction has no time left before the lease deadline")

// budget is the longest one transaction of a job may last: half of the renew slack, the time from a renew being
// due to the lease expiring, and never past the lease's own deadline. The transaction holds the lease row
// (lease.Held.Fence), so a renew that comes due waits for it; the budget lets that renew run before the lease can
// expire.
func budget(h *lease.Held) time.Duration {
	slack := (h.TTL() - h.RenewEvery()) / 2
	return min(slack, time.Until(h.Expires()))
}

// readOnly asks inTenant for a REPEATABLE READ transaction that turns READ ONLY after the fence.
type readOnly bool

// inTenant runs fn in one transaction of the tenant, opened with tenant.Begin and committed with tenant.Commit.
// The whole transaction runs within the job's budget: its context ends at the budget, and it sets a statement
// timeout of the budget and a lock timeout of at most the configured one and a quarter of the budget. It is fenced
// with the lease before the first statement of the job (ST-SUP-RP08-LC08). A read-only job runs as REPEATABLE READ,
// so its reads see one state, and turns READ ONLY after the fence, which needs a row lock; the rule that a ledger
// write runs only under READ COMMITTED (ST-SUP-RP08-LC10) is for writes, and this transaction writes nothing.
func (j *Jobs) inTenant(ctx context.Context, h *lease.Held, t tenant.Tenant, ro readOnly, fn func(ctx context.Context, tx *sql.Tx, token int64) error) error {
	left := budget(h)
	if left <= 10*time.Millisecond {
		return fmt.Errorf("%w: %v left", ErrDeadline, left)
	}
	ctx, cancel := context.WithTimeout(ctx, left)
	defer cancel()
	lock := min(j.cfg.LockTimeout, left/4)
	var opts *sql.TxOptions
	if ro {
		opts = &sql.TxOptions{Isolation: sql.LevelRepeatableRead}
	}
	tx, err := tenant.Begin(ctx, j.cfg.DB, t, opts)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "SELECT set_config('lock_timeout', $1, true), set_config('statement_timeout', $2, true)",
		fmt.Sprintf("%dms", max(lock.Milliseconds(), 1)), fmt.Sprintf("%dms", max(left.Milliseconds(), 1))); err != nil {
		return fmt.Errorf("ledger: set the job's timeouts: %w", err)
	}
	token, err := h.Fence(ctx, tx)
	if err != nil {
		return err
	}
	if ro {
		if _, err := tx.ExecContext(ctx, "SET TRANSACTION READ ONLY"); err != nil {
			return fmt.Errorf("ledger: make the check read only: %w", err)
		}
	}
	if err := fn(ctx, tx, token); err != nil {
		return err
	}
	return tenant.Commit(ctx, tx, t)
}

func stopsRun(err error) bool {
	return errors.Is(err, lease.ErrFenced) || errors.Is(err, lease.ErrStopped) || errors.Is(err, ErrDeadline)
}

const callSnapshot = `SELECT {s}.ledger_snapshot($1, $2, $3)`

// Snapshot runs the snapshot job once, while this process holds the controller lease: for each tenant, in a
// transaction of its own within the job's budget and fenced with the lease, ledger_snapshot computes and writes one
// balance_snapshots row for each account of each ledger that has at least the declared number of posted entries
// after its latest snapshot, with the holder's fencing token (ST-SUP-RP08-LC08). It returns the number of rows
// written. A process that does not hold the lease does no work. A fenced or stopped lease, or no time left, stops
// the run with its error, and the tenant's transaction writes nothing.
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
		n := 0
		err := j.inTenant(ctx, h, t, false, func(ctx context.Context, tx *sql.Tx, token int64) error {
			for _, l := range j.cfg.Ledgers {
				var k int
				if err := tx.QueryRowContext(ctx, l.q(callSnapshot), t.ID(), l.id, token).Scan(&k); err != nil {
					return fmt.Errorf("ledger %s: write the snapshots: %w", l.id, err)
				}
				n += k
			}
			return nil
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("ledger snapshot of the tenant %s: %w", t.ID(), err))
			if stopsRun(err) {
				break
			}
			continue
		}
		written += n
	}
	return written, errors.Join(errs...)
}

// readXmin reads the oldest running transaction ID: entries below it are final.
const readXmin = `SELECT pg_snapshot_xmin(pg_current_snapshot())::text`

// newSnapshots reads, in order, the snapshots of each account that are newer than the last one checked, which $3
// maps from account to through_xid.
const newSnapshots = `SELECT b.account_id, b.through_xid::text, b.amount FROM {s}.balance_snapshots b
 WHERE b.tenant_id = $1 AND b.ledger_id = $2 AND b.through_xid > coalesce(($3::jsonb ->> b.account_id)::xid8, '0'::xid8)
 ORDER BY b.account_id, b.through_xid`

// checkSums calls ledger_check_sums, which reads, for each account of the ledger, its entries from its start to the
// oldest running transaction ID $5 once, through the index on account and transaction ID, and sums them per snapshot
// interval two ways: debits less credits of the entries from $4, where the last check of the ledger stopped, for
// the ledger's sum; and on the account's normal side for the posted entries. The start of an account is the
// earlier of $4 and its last checked snapshot in $3. The interval of an entry is the first snapshot of its account
// whose through_xid is above the entry's transaction ID.
const checkSums = `SELECT account_id, coalesce(covered_by::text, ''), ledger_raw::text, normal_sum::text
 FROM {s}.ledger_check_sums($1, $2, $3::jsonb, $4::xid8, $5::xid8)`

// expiredPending reads the pending transfers whose expiry passed more than one sweep period ($3) ago, by the
// database clock, and that are still pending.
const expiredPending = `SELECT id::text, expires_at FROM {s}.transfers
 WHERE tenant_id = $1 AND ledger_id = $2 AND status = 'pending' AND kind = 'pending'
 AND expires_at <= now() - make_interval(secs => $3) ORDER BY id LIMIT 1000`

// Check runs the ledger_check job once, while this process holds the controller lease: for each tenant, in a
// REPEATABLE READ READ ONLY transaction of its own within the job's budget and fenced with the lease, it verifies
// what is new since the last check (ST-SUP-RP08-LC08). Each ledger must sum to zero over its final entries, and
// each snapshot newer than the last one checked must equal its stored predecessor plus the posted entries between
// them; the check reads only the entries from the last checked position, each once, and never sums a snapshot from
// the first entry. A snapshot that fails is reported alone: the next one is checked against what the failed one
// should hold, and the position stays before the failure, so the failure is reported again until it is mended.
// Each pending transfer whose expiry passed more than one Period ago is reported as a failure of the expiry sweep
// (ST-SUP-RP08-LC11). It returns an error that names each finding, and the jobs then report unhealthy (Health)
// until a check finds nothing. A process that does not hold the lease does no work and clears the findings: the
// holder reports them. The positions are kept in memory; after a restart the first check reads every entry once.
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
		var found []string
		next := map[string]*position{}
		err := j.inTenant(ctx, h, t, true, func(ctx context.Context, tx *sql.Tx, _ int64) error {
			for _, l := range j.cfg.Ledgers {
				key := t.ID() + "\x00" + l.id
				j.mu.Lock()
				pos := &position{accounts: map[string]snapshotAt{}}
				if p, ok := j.checked[key]; ok {
					pos = p.clone()
				}
				j.mu.Unlock()
				f, err := j.checkLedger(ctx, tx, t, l, pos)
				found = append(found, f...)
				if err != nil {
					return err
				}
				next[key] = pos
			}
			return nil
		})
		findings = append(findings, found...)
		if err != nil {
			findings = append(findings, fmt.Sprintf("the tenant %s cannot be checked: %v", t.ID(), err))
			errs = append(errs, err)
			if stopsRun(err) {
				break
			}
			continue
		}
		j.mu.Lock()
		for k, p := range next {
			j.checked[k] = p
		}
		j.mu.Unlock()
	}
	j.report(findings)
	if len(findings) > 0 {
		errs = append(errs, fmt.Errorf("%s failed: %s", CheckJob, strings.Join(findings, "; ")))
	}
	return errors.Join(errs...)
}

// checkLedger checks one tenant's ledger from pos, and moves pos to what it verified.
func (j *Jobs) checkLedger(ctx context.Context, tx *sql.Tx, t tenant.Tenant, l *Ledger, pos *position) ([]string, error) {
	var xminText string
	if err := tx.QueryRowContext(ctx, readXmin).Scan(&xminText); err != nil {
		return nil, err
	}
	xmin, err := strconv.ParseUint(xminText, 10, 64)
	if err != nil {
		return nil, err
	}
	starts := map[string]string{}
	for account, at := range pos.accounts {
		starts[account] = strconv.FormatUint(at.xid, 10)
	}
	startsJSON, err := json.Marshal(starts)
	if err != nil {
		return nil, err
	}
	type snap struct {
		account string
		xid     uint64
		amount  int64
	}
	var snaps []snap
	err = rowsOf(ctx, tx, l.q(newSnapshots), []any{t.ID(), l.id, string(startsJSON)}, func(rows *sql.Rows) error {
		var s snap
		var xid string
		if err := rows.Scan(&s.account, &xid, &s.amount); err != nil {
			return err
		}
		if s.xid, err = strconv.ParseUint(xid, 10, 64); err != nil {
			return err
		}
		snaps = append(snaps, s)
		return nil
	})
	if err != nil {
		return nil, err
	}
	type interval struct {
		account string
		xid     uint64
	}
	sums := map[interval]int64{}
	var ledgerSum int64
	err = rowsOf(ctx, tx, l.q(checkSums), []any{t.ID(), l.id, string(startsJSON), strconv.FormatUint(pos.ledgerXid, 10), xminText},
		func(rows *sql.Rows) error {
			var account, covered, raw, normalSum string
			if err := rows.Scan(&account, &covered, &raw, &normalSum); err != nil {
				return err
			}
			n, err := parseSum(raw)
			if err != nil {
				return err
			}
			if ledgerSum, err = addInt(ledgerSum, n); err != nil {
				return err
			}
			if covered == "" {
				return nil
			}
			xid, err := strconv.ParseUint(covered, 10, 64)
			if err != nil {
				return err
			}
			n, err = parseSum(normalSum)
			sums[interval{account, xid}] = n
			return err
		})
	if err != nil {
		return nil, err
	}
	var findings []string
	if ledgerSum != 0 {
		findings = append(findings, fmt.Sprintf("the ledger %s of the tenant %s does not sum to zero: debits less credits of its entries from the last check are %d", l.id, t.ID(), ledgerSum))
	} else {
		pos.ledgerXid = xmin
	}
	// Each new snapshot against its predecessor: the stored one while the stored ones hold, else what the failed one
	// should hold.
	base := map[string]int64{}
	failed := map[string]bool{}
	for account, at := range pos.accounts {
		base[account] = at.amount
	}
	for _, s := range snaps {
		want, err := addInt(base[s.account], sums[interval{s.account, s.xid}])
		if err != nil {
			return findings, err
		}
		if s.amount != want {
			findings = append(findings, fmt.Sprintf("the snapshot through_xid %d of the account %s in the ledger %s of the tenant %s holds %d, and its predecessor and the entries between them give %d",
				s.xid, s.account, l.id, t.ID(), s.amount, want))
			failed[s.account] = true
			base[s.account] = want
			continue
		}
		base[s.account] = s.amount
		if !failed[s.account] {
			pos.accounts[s.account] = snapshotAt{xid: s.xid, amount: s.amount}
		}
	}
	err = rowsOf(ctx, tx, l.q(expiredPending), []any{t.ID(), l.id, j.cfg.Period.Seconds()}, func(rows *sql.Rows) error {
		var id string
		var expires time.Time
		if err := rows.Scan(&id, &expires); err != nil {
			return err
		}
		findings = append(findings, fmt.Sprintf("the expiry sweep did not void the pending transfer %s in the ledger %s of the tenant %s, which expired at %s",
			id, l.id, t.ID(), expires.UTC().Format(time.RFC3339)))
		return nil
	})
	return findings, err
}

// addInt returns a + b, or an overflow error.
func addInt(a, b int64) (int64, error) {
	s := a + b
	if (b > 0 && s < a) || (b < 0 && s > a) {
		return 0, fmt.Errorf("ledger: the expected balance %d + %d overflows int64", a, b)
	}
	return s, nil
}

// expiredOf reads the expired pending transfers of one ledger, at most a batch.
const expiredOf = `SELECT id::text FROM {s}.transfers
 WHERE tenant_id = $1 AND ledger_id = $2 AND status = 'pending' AND kind = 'pending' AND expires_at <= clock_timestamp()
 ORDER BY expires_at, id LIMIT 100`

// Sweep runs the expiry sweep once, while this process holds the controller lease (ST-SUP-RP08-LC11): for each
// tenant and ledger it reads the pending transfers whose expiry has passed by the database clock, and voids each
// through Void with the idempotency key SweepKey(ID), in a transaction of its own within the job's budget and
// fenced with the lease, so a retry or a second replica writes no second void. It returns the number of voids it
// wrote. A void or a post that a client made first is not an error.
func (j *Jobs) Sweep(ctx context.Context) (int, error) {
	h := j.held()
	if h == nil {
		return 0, nil
	}
	tenants, err := j.cfg.Tenants(ctx)
	if err != nil {
		return 0, err
	}
	voided := 0
	var errs []error
	for _, t := range tenants {
		for _, l := range j.cfg.Ledgers {
			var ids []string
			err := j.inTenant(ctx, h, t, false, func(ctx context.Context, tx *sql.Tx, _ int64) error {
				return rowsOf(ctx, tx, l.q(expiredOf), []any{t.ID(), l.id}, func(rows *sql.Rows) error {
					var id string
					if err := rows.Scan(&id); err != nil {
						return err
					}
					ids = append(ids, id)
					return nil
				})
			})
			if err != nil {
				errs = append(errs, fmt.Errorf("ledger %s sweep of the tenant %s: %w", l.id, t.ID(), err))
				if stopsRun(err) {
					return voided, errors.Join(errs...)
				}
				continue
			}
			for _, id := range ids {
				var replayed bool
				err := j.inTenant(ctx, h, t, false, func(ctx context.Context, tx *sql.Tx, _ int64) error {
					v, err := l.Void(ctx, tx, t, id, SweepKey(id))
					replayed = v.Replayed
					return err
				})
				switch {
				case err == nil && !replayed:
					voided++
				case err == nil, errors.Is(err, ErrAlreadyVoided), errors.Is(err, ErrAlreadyPosted):
				default:
					errs = append(errs, fmt.Errorf("ledger %s sweep of %s: %w", l.id, id, err))
					if stopsRun(err) {
						return voided, errors.Join(errs...)
					}
				}
			}
		}
	}
	return voided, errors.Join(errs...)
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

// Components returns the three jobs as components of the controller mode: each runs its job once at start and then
// every Period of after, until its context ends. The generated code registers them for modes.Controller only, so
// the api and worker modes start none (ST-SUP-RC31-OC12). A run's error is kept in Health for the check, and does
// not stop the component. after is the controller's clock; nil is time.After.
func (j *Jobs) Components(after func(time.Duration) <-chan time.Time) []modes.Component {
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
				case <-after(j.cfg.Period):
				}
			}
		}
	}
	return []modes.Component{
		{Name: SnapshotJob, Run: loop(func(ctx context.Context) error { _, err := j.Snapshot(ctx); return err })},
		{Name: CheckJob, Run: loop(j.Check)},
		{Name: SweepJob, Run: loop(func(ctx context.Context) error { _, err := j.Sweep(ctx); return err })},
	}
}
