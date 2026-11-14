package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"google.golang.org/grpc"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/audit/auditsink"
	"github.com/jsell-rh/stego/registry/runtime/idempotency"
	"github.com/jsell-rh/stego/registry/runtime/lease"
	"github.com/jsell-rh/stego/registry/runtime/otel"
	"github.com/jsell-rh/stego/registry/runtime/probe"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// MigrationTimeout is the longest time that the migration job runs when Migration gives none.
const MigrationTimeout = 10 * time.Minute

// The steps of the migration job, in the order that Migrate runs them (ST-SUP-RP14-LC03). A fault names one.
const (
	StepSettings            = "settings"
	StepOpen                = "open"
	StepOwnerRole           = "owner_role"
	StepJobLock             = "job_lock"
	StepLedgerCheck         = "ledger_check"
	StepAuthorizationSchema = "authorization_schema"
	StepTables              = "tables"
	StepOutboxQueue         = "outbox_queue"
	StepOutboxSequence      = "outbox_sequence"
	StepAuditTables         = "audit_tables"
	StepAuditSink           = "audit_sink"
	StepIdempotencyKeys     = "idempotency_keys"
	StepAuthorizationTables = "authorization_tables"
	StepRuntimeGrants       = "runtime_grants"
	StepAuditPartitions     = "audit_partitions"
	StepSchemaAgain         = "authorization_schema_again"
	StepBackfill            = "backfill"
)

// BackfillTimeout is the longest time that the backfill step of the migration job runs when Migration gives none.
// A job that it stops resumes from the checkpoint on its next run.
const BackfillTimeout = time.Hour

// Migration is what varies with the declaration of the migration job of one service, which the generated code
// gives Migrate: the roles, the adapter's connection configuration and ledger, the ordered migration texts of the
// component packages, the schema writer and the grants of the runtime role.
type Migration struct {
	// OwnerRole is the role that owns the database objects; RuntimeRole the role that the service connects as;
	// WorkerRole the role that the worker mode connects as, which writes the default audit sink (ST-SUP-RP05-LC15).
	OwnerRole, RuntimeRole, WorkerRole string
	// PurgeRole is the idempotency purge role of the service, <service role prefix>_idempotency_purge, which the
	// tenancy role migration makes and which owns the purge function of the key table (ST-SUP-RP07-LC11).
	PurgeRole string
	// OutboxRelayRole is the outbox relay role of the service, which may read every message of the queue; the job
	// reads the highest sequence as that role to check the identity sequence of the queue (ST-SUP-RP14-LC03).
	OutboxRelayRole string
	// TenantConfig gives the connection configuration of the PostgreSQL adapter for a connection string.
	TenantConfig func(dsn string) (tenant.Config, error)
	// LedgerCheck reads the database and refuses one whose tables this release did not make, writing nothing; Ledger
	// runs the ledger of the adapter, which makes the tables of the declaration. Each gets the job's one connection,
	// as the owner role.
	LedgerCheck, Ledger func(ctx context.Context, conn *sql.Conn) error
	// Outbox, Audit and Authorization are the migration texts of the outbox queue, the audit tables and the
	// authorization tables, for the roles of the service; the idempotency keys take the runtime's own text.
	Outbox, Audit, Authorization string
	// WriteSchema writes the authorization schema of the release to SpiceDB, which refuses a schema that removes a
	// definition or a relation that has relationships (ST-SUP-RC29-OC23).
	WriteSchema func(ctx context.Context, conn grpc.ClientConnInterface) error
	// Grants are the GRANT statements of the runtime role, each without its grantee.
	Grants []string
	// TenantSchema is the schema of the tenant list stego_tenants, which the tables step makes with the migration
	// text of the tenant runtime (ST-SUP-RP04-LC07); empty is public.
	TenantSchema string
	// Timeout is the longest time that the job runs; zero is MigrationTimeout.
	Timeout time.Duration
	// Backfills are the backfills of the schema changes of this release whose last step is expand, which the job
	// runs last, before the rollout of the release (decision on finding F5 of the schema-change research, owner,
	// 2026-11-13).
	Backfills []Backfill
	// BackfillLease are the lease settings of the backfills, and BackfillTimeout the longest time that the backfill
	// step runs, apart from Timeout; zero is BackfillTimeout. Holder names the job in the lease; empty is the host
	// name and the process ID.
	BackfillLease   lease.Settings
	BackfillTimeout time.Duration
	Holder          string
	// Service names the service on the probe runtime of the backfills, which reports their progress as counts
	// (ST-GEN-UC13-R06).
	Service string
}

// Backfill is the backfill of one schema change at expand: its change ID, the generated step of one batch, and its
// settings (ST-GEN-UC13).
type Backfill struct {
	Change   string
	Step     lease.Step
	Settings lease.BackfillSettings
}

// StepError is the failure of one step of the migration job: it names the step, and its text holds no value of a
// setting.
type StepError struct {
	Step string
	Err  error
}

func (e *StepError) Error() string { return "migrate: " + e.Step + ": " + e.Err.Error() }
func (e *StepError) Unwrap() error { return e.Err }

// failed gives the failure of a step of the migration job.
func failed(step string, err error) error { return &StepError{Step: step, Err: err} }

// Stage gives the stage of a failure of Migrate, such as migrate.ledger_check, which names the step and no error
// text, for the failure record of the process.
func Stage(err error) string {
	var step *StepError
	if errors.As(err, &step) {
		return "migrate." + step.Step
	}
	return "migrate"
}

// stepped, when a test of the package sets it, runs at the start of each step with the step's name and the job's
// connection.
var stepped func(step string, conn *sql.Conn)

// retried, when a test of the package sets it, runs each time a migration text waited too long for a lock and is
// run again, with the step's name.
var retried func(step string)

// jobLock is the key of the job lock: the session advisory lock that the migration job holds from its start to its
// backfill step, so a second job of the same database waits for the first, within its time limit, and does not race it on the
// objects that both make (followups P2: two jobs at once failed on pg_type with 23505).
const jobLock = "1937007983,1785684555"

// The lock limits of a migration text (followups P1, research F1): each text runs in one transaction with a lock
// timeout, so a statement that needs a strong lock on a table that a long transaction of the running release holds
// waits at most textLockTimeout, and the reads and writes of the release that queue behind it wait no longer. A text
// whose lock wait ends at the timeout runs again after a pause, at most textAttempts times.
const (
	textLockTimeout = "1s"
	textAttempts    = 10
)

// Migrate runs the migration job of a service (tenant.MigrationJob) on one connection, with a time limit
// (ST-SUP-RP14-LC03). It opens the database with the migration settings through tenant.OpenOwner and the adapter's
// connection configuration, and runs, as the owner role and in this order: the read-only ledger check, which
// refuses a database whose tables this release did not make and writes nothing; WriteSchema, which SpiceDB refuses
// for a schema that removes what has relationships, so a refused schema leaves the database as it was; the ledger of
// the adapter and then the tenant list of the tenant runtime (tenant.Migration) in TenantSchema; the outbox, audit, idempotency and authorization migrations, each in one transaction, the audit step
// with the lease table and the row of the controller lease (ControllerLease); the grants of
// the runtime role; the audit partitions of this month and the next; and WriteSchema again, so the release ends on
// its own schema. It stops at the first step that fails, with a StepError that names the step. After the owner role
// it takes the job lock, which it holds until the backfill, so a second job of the database waits for it (StepJobLock); and
// it runs the tenant list and each migration text under a lock timeout of 1 s, and again after a pause when a lock
// wait ends at that timeout, at most 10 times, so no read or write of the running release queues long behind it. Each step runs again
// on a database that it made, so the job runs on every release.
func Migrate(parent context.Context, s MigrationSettings, m Migration) error {
	if m.OwnerRole == "" || m.RuntimeRole == "" || m.WorkerRole == "" || m.PurgeRole == "" || m.OutboxRelayRole == "" || m.TenantConfig == nil || m.LedgerCheck == nil || m.Ledger == nil ||
		m.WriteSchema == nil || m.Outbox == "" || m.Audit == "" || m.Authorization == "" {
		return failed(StepSettings, errors.New("the migration job needs its roles, the adapter, the ledger, the migration texts and the schema writer"))
	}
	keys, err := idempotency.Migration(m.PurgeRole, m.RuntimeRole)
	if err != nil {
		return failed(StepSettings, err)
	}
	timeout := m.Timeout
	if timeout <= 0 {
		timeout = MigrationTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	config, err := m.TenantConfig(s.DatabaseURL)
	if err != nil {
		return failed(StepSettings, err)
	}
	db, err := tenant.OpenOwner(ctx, tenant.MigrationJob, config)
	if err != nil {
		return failed(StepOpen, err)
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return failed(StepOpen, err)
	}
	defer conn.Close()
	// The owner role runs each step with the search path public, pg_temp, so a name with no schema in a migration
	// text, such as the key table of the idempotency migration, resolves in public, never in a schema named after
	// the owner role ("$user") or the login of the job (ST-STD-TXN-R19).
	asOwner := "SET ROLE " + quote(m.OwnerRole) + "; SET search_path = public, pg_temp"
	step := func(name string, run func() error) error {
		if stepped != nil {
			stepped(name, conn)
		}
		if err := run(); err != nil {
			return failed(name, err)
		}
		return nil
	}
	if err := step(StepOwnerRole, func() error { _, err := conn.ExecContext(ctx, asOwner); return err }); err != nil {
		return err
	}
	// The job lock is a session lock of the job's connection: a second job waits here until the first ends its
	// schema steps, and a wait past the time limit of the job fails this step.
	if err := step(StepJobLock, func() error { return takeJobLock(ctx, conn) }); err != nil {
		return err
	}
	locked := true
	unlock := func() {
		if locked {
			locked = false
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), "SELECT pg_catalog.pg_advisory_unlock("+jobLock+")")
		}
	}
	defer unlock()
	if err := step(StepLedgerCheck, func() error { return m.LedgerCheck(ctx, conn) }); err != nil {
		return err
	}
	spice, err := DialSpiceDB(s.SpiceDB)
	if err != nil {
		return failed(StepAuthorizationSchema, err)
	}
	defer spice.Close()
	if err := step(StepAuthorizationSchema, func() error { return m.WriteSchema(ctx, spice) }); err != nil {
		return err
	}
	tenants := m.TenantSchema
	if tenants == "" {
		tenants = "public"
	}
	tenantList, err := tenant.Migration(tenants, m.RuntimeRole)
	if err != nil {
		return failed(StepSettings, err)
	}
	sinkTable, err := auditsink.Migration(m.WorkerRole)
	if err != nil {
		return failed(StepSettings, err)
	}
	if err := step(StepTables, func() error {
		if _, err := conn.ExecContext(ctx, asOwner); err != nil {
			return err
		}
		if err := m.Ledger(ctx, conn); err != nil {
			return err
		}
		// The tenant list is the same for every service, and its text runs again on a database that it made
		// (ST-SUP-Q97).
		return textStep(ctx, conn, m.OwnerRole, StepTables, tenantList)
	}); err != nil {
		return err
	}
	texts := []struct{ name, text string }{
		{StepOutboxQueue, m.Outbox},
		{StepAuditTables, m.Audit + ";\n" + controllerLease(m.RuntimeRole)},
		{StepAuditSink, sinkTable},
		{StepIdempotencyKeys, keys},
		{StepAuthorizationTables, m.Authorization},
		{StepRuntimeGrants, granted(m.Grants, m.RuntimeRole)},
	}
	for _, t := range texts {
		if err := step(t.name, func() error { return textStep(ctx, conn, m.OwnerRole, t.name, t.text) }); err != nil {
			return err
		}
		if t.name == StepOutboxQueue {
			if err := step(StepOutboxSequence, func() error { return checkOutboxSequence(ctx, conn, m.OwnerRole, m.OutboxRelayRole) }); err != nil {
				return err
			}
		}
	}
	if err := step(StepAuditPartitions, func() error { return maintainAudit(ctx, conn, m.OwnerRole) }); err != nil {
		return err
	}
	if err := step(StepSchemaAgain, func() error { return m.WriteSchema(ctx, spice) }); err != nil {
		return err
	}
	// The backfill runs after the job lock is released: two jobs of the database share it under the lease of each
	// change, so one moves rows and the other waits for the lease (ST-GEN-UC13-R01).
	unlock()
	return step(StepBackfill, func() error { return runBackfills(parent, db, m) })
}

// BackfillLeaseName gives the lease of the backfill of a schema change: stego.backfill.<change ID>
// (ST-GEN-UC13-R01).
func BackfillLeaseName(change string) string { return "stego.backfill." + change }

// runBackfills runs the backfill of each schema change of m in turn, as the owner role, the backfill role of the
// sync trigger (ST-SUP-RC01-OC32; decision on finding F4, orchestrator under the owner's delegation, 2026-11-13),
// each under its own lease, so that of two jobs that run at the same time one moves rows and the other waits
// (ST-GEN-UC13-R01). A job whose batch was fenced or whose lease was lost waits for the lease again, and finds the
// change complete or resumes it from the checkpoint (ST-GEN-UC13-R04). The step ends when every change is complete,
// or at BackfillTimeout with an error, and the next run of the job resumes.
func runBackfills(parent context.Context, db *sql.DB, m Migration) error {
	if len(m.Backfills) == 0 {
		return nil
	}
	timeout := m.BackfillTimeout
	if timeout <= 0 {
		timeout = BackfillTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	holder := m.Holder
	if holder == "" {
		holder = defaultHolder()
	}
	var changes []probe.SchemaChange
	for _, b := range m.Backfills {
		changes = append(changes, probe.SchemaChange{ID: b.Change, Open: true})
	}
	builtins, err := probe.NewBuiltins(nil, changes)
	if err != nil {
		return err
	}
	if m.Service == "" {
		return errors.New("the backfills need the service name")
	}
	telemetry, err := otel.New(m.Service, probe.SpanNames()...)
	if err != nil {
		return err
	}
	defer telemetry.Close()
	probes, err := probe.Start(telemetry, builtins)
	if err != nil {
		return err
	}
	defer func() { _ = probes.Close(context.WithoutCancel(ctx)) }()
	// Two sessions as the owner role: the lease renews on one while a batch holds a transaction on the other.
	session := func() (*sql.Conn, error) {
		conn, err := db.Conn(ctx)
		if err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, "SET ROLE "+quote(m.OwnerRole)+"; SET search_path = public, pg_temp"); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return conn, nil
	}
	// A session goes back to the pool as the login role, with its own search path.
	release := func(conn *sql.Conn) {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), "RESET ROLE; RESET search_path")
		_ = conn.Close()
	}
	for _, b := range m.Backfills {
		settings := b.Settings
		if settings.Observer == nil {
			settings.Observer = probes.Backfill()
		}
		backfill, err := lease.NewBackfill(b.Change, b.Step, settings)
		if err != nil {
			return err
		}
		name := BackfillLeaseName(b.Change)
		// Each hold of the lease gets new sessions, since a statement that a lost lease cancels can break its
		// connection.
		hold := func() error {
			leaseConn, err := session()
			if err != nil {
				return err
			}
			defer release(leaseConn)
			batchConn, err := session()
			if err != nil {
				return err
			}
			defer release(batchConn)
			if err := lease.Declare(ctx, leaseConn, name); err != nil {
				return err
			}
			client, err := lease.New(leaseConn, name, holder, m.BackfillLease)
			if err != nil {
				return err
			}
			return client.Hold(ctx, func(ctx context.Context, held *lease.Held) error {
				return backfill.Run(ctx, batchConn, held)
			})
		}
		for {
			err := hold()
			if err == nil {
				break
			}
			if !errors.Is(err, lease.ErrFenced) && !errors.Is(err, lease.ErrLost) && !errors.Is(err, lease.ErrStopped) {
				return fmt.Errorf("backfill %s: %w", b.Change, err)
			}
			if ctx.Err() != nil {
				return fmt.Errorf("backfill %s: %w", b.Change, ctx.Err())
			}
		}
	}
	return nil
}

// errSequenceBehind is the refusal of a queue whose identity sequence would give a message a sequence that a stored
// message already has or that is below one, as after a logical replication failover that does not carry the state
// of the sequences: the delivery order of a key follows the sequence (ST-SUP-RP01-LC03).
var errSequenceBehind = errors.New("the identity sequence of stego_outbox.messages is behind its highest stored sequence; set it past that sequence as the owner (SELECT setval) before the job runs again")

// checkOutboxSequence refuses a queue whose identity sequence is behind its highest stored sequence. It reads the
// highest sequence first, as the outbox relay role, the one role whose policy sees every message, and the state of
// the sequence second, as the owner, which owns it, so an enqueue that commits between the two reads only raises the
// state of the sequence and never makes a healthy queue look behind. It refuses, and does not move the sequence
// itself, so a person sees the fault and its cause.
func checkOutboxSequence(ctx context.Context, conn *sql.Conn, owner, relay string) error {
	return ownerStep(ctx, conn, owner, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "SET LOCAL ROLE "+quote(relay)); err != nil {
			return err
		}
		var highest sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT max(m.sequence) FROM stego_outbox.messages m`).Scan(&highest); err != nil {
			return err
		}
		if betweenSequenceReads != nil {
			betweenSequenceReads()
		}
		if _, err := tx.ExecContext(ctx, "SET LOCAL ROLE "+quote(owner)); err != nil {
			return err
		}
		var last int64
		var called bool
		if err := tx.QueryRowContext(ctx, `SELECT s.last_value, s.is_called FROM stego_outbox.messages_sequence_seq s`).Scan(&last, &called); err != nil {
			return err
		}
		if !highest.Valid {
			return nil
		}
		// The next value is last_value when is_called is false, and last_value + 1 when it is true.
		if (called && last < highest.Int64) || (!called && last <= highest.Int64) {
			return errSequenceBehind
		}
		return nil
	})
}

// betweenSequenceReads, when a test of the package sets it, runs between the two reads of checkOutboxSequence.
var betweenSequenceReads func()

// controllerLease gives the statements of the lease table and the row of the controller lease, under which the audit
// jobs run (ST-SUP-RP14-LC04), with SELECT and UPDATE on the table for the runtime role, the role of the lease
// client; the table belongs to no tenant. Each statement runs again with no error on a database that it made.
func controllerLease(runtimeRole string) string {
	return lease.LeasesTable + ";\nINSERT INTO public.leases (name) VALUES ('" + ControllerLease + "') ON CONFLICT (name) DO NOTHING;\n" +
		"GRANT SELECT, UPDATE ON TABLE public.leases TO " + quote(runtimeRole) + ";\n"
}

// granted gives the statements of the grants to the runtime role.
func granted(grants []string, role string) string {
	var text strings.Builder
	for _, grant := range grants {
		text.WriteString(grant + " TO " + quote(role) + ";\n")
	}
	return text.String()
}

// ownerStep runs one step of the migration job in one transaction on conn, as the owner role from its start: a step
// can reset the role, as the outbox migration does after it made the relay functions.
func ownerStep(ctx context.Context, conn *sql.Conn, owner string, run func(*sql.Tx) error) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "SET LOCAL ROLE "+quote(owner)); err != nil {
		return err
	}
	if err := run(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// textStep runs a migration text as the owner role in one transaction under the lock timeout of a text, and runs it
// again after a pause when a lock wait ends at the timeout (SQLSTATE 55P03), at most textAttempts times. Each text
// runs again on a database that it made, so a run that the timeout ended changes nothing that the next run needs.
func textStep(ctx context.Context, conn *sql.Conn, owner, name, text string) error {
	for attempt := 1; ; attempt++ {
		err := ownerStep(ctx, conn, owner, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, "SELECT pg_catalog.set_config('lock_timeout', $1, true)", textLockTimeout); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, text)
			return err
		})
		if err == nil || !lockTimedOut(err) || attempt >= textAttempts {
			return err
		}
		if retried != nil {
			retried(name)
		}
		if err := pause(ctx, attempt); err != nil {
			return err
		}
	}
}

// takeJobLock takes the job lock on conn. It tries the lock, and while another job holds it, it pauses and tries
// again, until ctx ends at the time limit of the job. A blocking pg_advisory_lock would hold a snapshot while it
// waits, and CREATE INDEX CONCURRENTLY of the job that holds the lock waits for every snapshot, so the two jobs
// would deadlock (security review of PR #295, L1). Between two tries the connection holds no open transaction.
func takeJobLock(ctx context.Context, conn *sql.Conn) error {
	for attempt := 1; ; attempt++ {
		var taken bool
		if err := conn.QueryRowContext(ctx, "SELECT pg_catalog.pg_try_advisory_lock("+jobLock+")").Scan(&taken); err != nil {
			return err
		}
		if taken {
			return nil
		}
		if waited != nil {
			waited()
		}
		if err := pause(ctx, attempt); err != nil {
			return fmt.Errorf("another migration job holds the job lock: %w", err)
		}
	}
}

// waited is called after each try of the job lock that another job holds; the tests set it.
var waited func()

// lockTimedOut tells whether err ends a wait for a lock at the lock timeout: SQLSTATE 55P03, lock_not_available.
func lockTimedOut(err error) bool {
	var state interface{ SQLState() string }
	return errors.As(err, &state) && state.SQLState() == "55P03"
}

// pause waits before the next run of a text that waited too long for a lock: a time that doubles with each attempt
// from 100 ms to at most 5 s, of which a random part, so two jobs do not try again at once.
func pause(ctx context.Context, attempt int) error {
	wait := min(100*time.Millisecond<<min(attempt-1, 6), 5*time.Second)
	wait = wait/2 + rand.N(wait/2+1)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// maintainAudit runs the audit partition maintenance until it has no step left, so the partitions of this month and
// the next exist before the service writes an audit row.
func maintainAudit(ctx context.Context, conn *sql.Conn, owner string) error {
	for range 64 {
		var more bool
		if err := ownerStep(ctx, conn, owner, func(tx *sql.Tx) error {
			// By its schema, so no function of the same name earlier on the search path runs in its place.
			return tx.QueryRowContext(ctx, "SELECT step_left FROM public."+audit.MaintainFunction+"()").Scan(&more)
		}); err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
	return errors.New("the audit partition maintenance did not finish")
}

// quote quotes one SQL name.
func quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// CopyBackfills gives the backfill of each copy, the batch of a rename at expand, over the tenant list stego_tenants
// in tenantSchema (empty is public), with the default batch settings of the lease runtime and a pause of
// BackfillPause between batches.
func CopyBackfills(copies []lease.Copy, tenantSchema string) ([]Backfill, error) {
	if tenantSchema == "" {
		tenantSchema = "public"
	}
	var out []Backfill
	for _, c := range copies {
		c.Tenants = quote(tenantSchema) + ".stego_tenants"
		step, err := c.Step()
		if err != nil {
			return nil, err
		}
		key, parts := c.Settings()
		out = append(out, Backfill{Change: c.Change, Step: step, Settings: lease.BackfillSettings{Key: key, KeyParts: parts, Pause: BackfillPause}})
	}
	return out, nil
}

// BackfillPause is the wait of a backfill of the migration job after each batch, which leaves room for the writes of
// the running release and for the replicas (decision on finding F6, orchestrator under the owner's delegation,
// 2026-11-13).
const BackfillPause = 50 * time.Millisecond
