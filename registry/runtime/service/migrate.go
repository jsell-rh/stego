package service

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"google.golang.org/grpc"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/idempotency"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// MigrationTimeout is the longest time that the migration job runs when Migration gives none.
const MigrationTimeout = 10 * time.Minute

// The steps of the migration job, in the order that Migrate runs them (ST-SUP-RP14-LC03). A fault names one.
const (
	StepSettings            = "settings"
	StepOpen                = "open"
	StepOwnerRole           = "owner_role"
	StepLedgerCheck         = "ledger_check"
	StepAuthorizationSchema = "authorization_schema"
	StepTables              = "tables"
	StepOutboxQueue         = "outbox_queue"
	StepAuditTables         = "audit_tables"
	StepIdempotencyKeys     = "idempotency_keys"
	StepAuthorizationTables = "authorization_tables"
	StepRuntimeGrants       = "runtime_grants"
	StepAuditPartitions     = "audit_partitions"
	StepSchemaAgain         = "authorization_schema_again"
)

// Migration is what varies with the declaration of the migration job of one service, which the generated code
// gives Migrate: the roles, the adapter's connection configuration and ledger, the ordered migration texts of the
// component packages, the schema writer and the grants of the runtime role.
type Migration struct {
	// OwnerRole is the role that owns the database objects; RuntimeRole the role that the service connects as.
	OwnerRole, RuntimeRole string
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
	// Timeout is the longest time that the job runs; zero is MigrationTimeout.
	Timeout time.Duration
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

// Migrate runs the migration job of a service (tenant.MigrationJob) on one connection, with a time limit
// (ST-SUP-RP14-LC03). It opens the database with the migration settings through tenant.OpenOwner and the adapter's
// connection configuration, and runs, as the owner role and in this order: the read-only ledger check, which
// refuses a database whose tables this release did not make and writes nothing; WriteSchema, which SpiceDB refuses
// for a schema that removes what has relationships, so a refused schema leaves the database as it was; the ledger of
// the adapter; the outbox, audit, idempotency and authorization migrations, each in one transaction; the grants of
// the runtime role; the audit partitions of this month and the next; and WriteSchema again, so the release ends on
// its own schema. It stops at the first step that fails, with a StepError that names the step. Each step runs again
// on a database that it made, so the job runs on every release.
func Migrate(parent context.Context, s MigrationSettings, m Migration) error {
	if m.OwnerRole == "" || m.RuntimeRole == "" || m.TenantConfig == nil || m.LedgerCheck == nil || m.Ledger == nil ||
		m.WriteSchema == nil || m.Outbox == "" || m.Audit == "" || m.Authorization == "" {
		return failed(StepSettings, errors.New("the migration job needs its roles, the adapter, the ledger, the migration texts and the schema writer"))
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
	asOwner := "SET ROLE " + quote(m.OwnerRole)
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
	if err := step(StepTables, func() error {
		if _, err := conn.ExecContext(ctx, asOwner); err != nil {
			return err
		}
		return m.Ledger(ctx, conn)
	}); err != nil {
		return err
	}
	texts := []struct{ name, text string }{
		{StepOutboxQueue, m.Outbox},
		{StepAuditTables, m.Audit},
		{StepIdempotencyKeys, idempotency.Migration},
		{StepAuthorizationTables, m.Authorization},
		{StepRuntimeGrants, granted(m.Grants, m.RuntimeRole)},
	}
	for _, t := range texts {
		if err := step(t.name, func() error {
			return ownerStep(ctx, conn, m.OwnerRole, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, t.text)
				return err
			})
		}); err != nil {
			return err
		}
	}
	if err := step(StepAuditPartitions, func() error { return maintainAudit(ctx, conn, m.OwnerRole) }); err != nil {
		return err
	}
	return step(StepSchemaAgain, func() error { return m.WriteSchema(ctx, spice) })
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

// maintainAudit runs the audit partition maintenance until it has no step left, so the partitions of this month and
// the next exist before the service writes an audit row.
func maintainAudit(ctx context.Context, conn *sql.Conn, owner string) error {
	for range 64 {
		var more bool
		if err := ownerStep(ctx, conn, owner, func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, "SELECT step_left FROM "+audit.MaintainFunction+"()").Scan(&more)
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
