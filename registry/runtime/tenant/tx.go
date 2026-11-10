package tenant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// SettingName is the transaction-local setting that holds the tenant of a transaction. The row-level security
// policy of every tenant-owned table reads it with current_setting('app.tenant', true).
const SettingName = "app.tenant"

// setTenantStatement is the first statement of every transaction that Begin opens (ST-SUP-RP04-LC03). Its third
// argument true makes the setting transaction-local, so it ends with the transaction. The function is named with
// its schema, so a function set_config of the caller's search_path cannot stand in for it.
const setTenantStatement = "SELECT pg_catalog.set_config('app.tenant', $1, true)"

// readTenantStatement reads the tenant setting of the connection, with the function named with its schema.
const readTenantStatement = "SELECT pg_catalog.current_setting('app.tenant', true)"

// Begin opens a transaction on db and sets its tenant with SELECT set_config('app.tenant', $1, true) as the first
// statement, before the caller's first statement (ST-SUP-RP04-LC03). It refuses the zero tenant before it sends a
// statement. The caller commits the transaction it returns with Commit, which checks the tenant setting first, or
// rolls it back.
func Begin(ctx context.Context, db *sql.DB, t Tenant, opts *sql.TxOptions) (*sql.Tx, error) {
	if t.IsZero() {
		return nil, ErrZero
	}
	if db == nil {
		return nil, errors.New("tenant: no database")
	}
	tx, err := db.BeginTx(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("tenant: begin the transaction of tenant %s: %w", t.id, err)
	}
	var set string
	if err := tx.QueryRowContext(ctx, setTenantStatement, t.id).Scan(&set); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("tenant: set the tenant %s of the transaction: %w", t.id, err)
	}
	if set != t.id {
		_ = tx.Rollback()
		return nil, fmt.Errorf("tenant: the transaction holds the tenant %q, not %s", set, t.id)
	}
	return tx, nil
}

// Querier is the part of a transaction that a worker function or a provisioning hook gets: it can run statements
// in the transaction, and it cannot commit or roll it back. The tenant runtime commits.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// querier hides the *sql.Tx behind Querier, so a type assertion cannot reach Commit.
type querier struct {
	tx *sql.Tx
}

func (q querier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return q.tx.ExecContext(ctx, query, args...)
}

func (q querier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return q.tx.QueryContext(ctx, query, args...)
}

func (q querier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return q.tx.QueryRowContext(ctx, query, args...)
}

// ErrNotCommitted matches each error of Commit that ends before COMMIT is sent: the transaction is rolled back and
// surely did not commit, so a caller can tell it from a COMMIT whose outcome is not known (ST-SUP-RP10-LC25).
var ErrNotCommitted = errors.New("tenant: the transaction did not commit")

// notCommitted is an error of Commit before COMMIT: it matches ErrNotCommitted and unwraps to its cause.
type notCommitted struct{ err error }

func (e *notCommitted) Error() string        { return e.err.Error() }
func (e *notCommitted) Unwrap() error        { return e.err }
func (e *notCommitted) Is(target error) bool { return target == ErrNotCommitted }

// ErrTenantChanged is the error of Commit when the tenant setting of the transaction no longer holds its tenant.
var ErrTenantChanged = errors.New("tenant: the tenant setting of the transaction changed, so the transaction is rolled back")

// Commit commits tx, which Begin opened for t, only when its tenant setting still holds t (ST-SUP-RP04-LC03). When a
// statement of the caller changed app.tenant, with is_local true or false, it rolls the transaction back and
// returns ErrTenantChanged, which names no tenant value, so no row of another tenant commits and the connection
// keeps no tenant. Each error that ends before COMMIT is sent also matches ErrNotCommitted. It refuses the zero tenant before it sends a statement.
func Commit(ctx context.Context, tx *sql.Tx, t Tenant) error {
	if t.IsZero() {
		return ErrZero
	}
	if tx == nil {
		return errors.New("tenant: no transaction")
	}
	var current sql.NullString
	if err := tx.QueryRowContext(ctx, readTenantStatement).Scan(&current); err != nil {
		_ = tx.Rollback()
		return &notCommitted{err: fmt.Errorf("tenant: read the tenant setting before the commit: %w", err)}
	}
	if current.String != t.id {
		_ = tx.Rollback()
		return &notCommitted{err: ErrTenantChanged}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("tenant: commit the transaction of tenant %s: %w", t.id, err)
	}
	return nil
}

// Config is what a connector needs to open a database: the connection string, the extra startup parameters and the
// statements that run on each new connection. Guard checks it before a connector opens anything.
type Config struct {
	// Conn is the connection configuration that the PostgreSQL adapter's opener builds, with its verified TLS
	// policy (ST-SUP-RP04-LC09, ST-SUP-RC01-OC01 to OC04). Open and OpenOwner open the pool with it as it is and
	// parse no DSN; they refuse a Conn whose TLS is not verified. Give Conn or DSN, not both.
	Conn *pgx.ConnConfig
	// Pool are the bounds of the pool (ST-SUP-RC01-OC12), which Open sets on the pool it returns. A zero bound
	// keeps the default of database/sql.
	Pool PoolBounds
	// DSN is a PostgreSQL connection string, in URL or key=value form, for a connector with no opener, such as a
	// test. The PG* environment values apply as pgx reads them, and Guard checks the result.
	DSN string
	// Params are startup parameters to add to those of the DSN.
	Params map[string]string
	// AfterConnect are statements to run on each new connection. Guard refuses every one: a statement can set a
	// session value in more ways than a check of its text finds.
	AfterConnect []string
	// InsecureLoopback lets Conn or DSN connect without verified TLS when the host and every fallback is a loopback
	// address or a local socket, for a local test only, as the PostgreSQL adapter's loopback test mode does. Without
	// it, Open and OpenOwner refuse a configuration or a connection string whose TLS does not verify the server,
	// such as sslmode disable, prefer or require (ST-SUP-RP04-LC09).
	InsecureLoopback bool
}

// PoolBounds are the bounds of a pool.
type PoolBounds struct {
	MaxOpen, MaxIdle         int
	MaxLifetime, MaxIdleTime time.Duration
}

// connConfig is the connection configuration of cfg: Conn as it is, or the parsed DSN.
func connConfig(cfg Config) (*pgx.ConnConfig, error) {
	switch {
	case cfg.Conn != nil && cfg.DSN != "":
		return nil, errors.New("tenant: give a connection configuration or a connection string, not both")
	case cfg.Conn != nil:
		// A hook of the configuration runs on each new connection and can set a session value, as an after-connect
		// statement can (ST-SUP-RP04-LC04).
		if cfg.Conn.AfterConnect != nil || cfg.Conn.AfterNetConnect != nil {
			return nil, errors.New("tenant: the pooler guard refuses a connection configuration with an after-connect hook")
		}
		return cfg.Conn.Copy(), nil
	}
	parsed, err := pgx.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("tenant: read the connection string: %w", err)
	}
	return parsed, nil
}

// errNoVerifiedTLS is the error of a connection configuration of the opener whose TLS is not verified.
var errNoVerifiedTLS = errors.New("tenant: the connection configuration does not verify TLS")

// transport refuses a connection configuration that connects without verified TLS, unless insecureLoopback is set
// and every target of it is a loopback address or a local socket (ST-SUP-RP04-LC09).
func transport(c *pgx.ConnConfig, insecureLoopback bool) error {
	err := verifiedTLS(c)
	if err == nil || !insecureLoopback {
		return err
	}
	if !local(c.Host) {
		return fmt.Errorf("%w, and the host %q is not a loopback address", err, c.Host)
	}
	for _, f := range c.Fallbacks {
		if !local(f.Host) {
			return fmt.Errorf("%w, and the fallback host %q is not a loopback address", err, f.Host)
		}
	}
	return nil
}

// local tells whether host is a loopback IP address, localhost or the path of a local socket.
func local(host string) bool {
	if strings.HasPrefix(host, "/") || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// verifiedTLS refuses a connection configuration, or one of its fallbacks, that connects without TLS or does not
// verify the server's certificate (ST-SUP-RP04-LC09).
func verifiedTLS(c *pgx.ConnConfig) error {
	if c.TLSConfig == nil || c.TLSConfig.InsecureSkipVerify {
		return errNoVerifiedTLS
	}
	for _, f := range c.Fallbacks {
		if f.TLSConfig == nil || f.TLSConfig.InsecureSkipVerify {
			return fmt.Errorf("%w: a fallback connects without verified TLS", errNoVerifiedTLS)
		}
	}
	return nil
}

// allowedStartupParams are the startup parameters that set no value which differs between clients. The pooler
// keeps application_name per client itself.
var allowedStartupParams = map[string]bool{"application_name": true}

// Guard refuses a configuration that can set a session-scoped value: a connection option (options=-c name=value),
// a startup parameter other than application_name, or any after-connect statement (ST-SUP-RP04-LC04). The error
// names each setting, and quotes each statement. A connector calls it before it opens a connection.
func Guard(cfg Config) error {
	parsed, err := connConfig(cfg)
	if err != nil {
		return err
	}
	var refused []string
	params := map[string]string{}
	for name, value := range parsed.RuntimeParams {
		params[name] = value
	}
	for name, value := range cfg.Params {
		params[name] = value
	}
	for name, value := range params {
		if name == "options" {
			for _, setting := range optionSettings(value) {
				refused = append(refused, fmt.Sprintf("the connection option sets the session value %s", setting))
			}
			continue
		}
		if !allowedStartupParams[name] {
			refused = append(refused, fmt.Sprintf("the startup parameter sets the session value %s", name))
		}
	}
	for _, statement := range cfg.AfterConnect {
		refused = append(refused, fmt.Sprintf("the after-connect statement %q can set a session value", statement))
	}
	if len(refused) == 0 {
		return nil
	}
	sort.Strings(refused)
	return fmt.Errorf("tenant: the pooler guard refuses a session setting: %s", strings.Join(refused, "; "))
}

// optionSettings returns the names of the settings in the value of the options startup parameter, which holds
// command-line switches such as -c name=value and --name=value. Every switch sets a session value; an option that
// is not one of those forms is returned whole.
func optionSettings(options string) []string {
	var names []string
	fields := strings.Fields(options)
	for i := 0; i < len(fields); i++ {
		field := fields[i]
		switch {
		case field == "-c" && i+1 < len(fields):
			i++
			names = append(names, settingName(fields[i]))
		case strings.HasPrefix(field, "-c"):
			names = append(names, settingName(strings.TrimPrefix(field, "-c")))
		case strings.HasPrefix(field, "--"):
			names = append(names, settingName(strings.TrimPrefix(field, "--")))
		default:
			names = append(names, field)
		}
	}
	return names
}

func settingName(assignment string) string {
	name, _, _ := strings.Cut(assignment, "=")
	return strings.ReplaceAll(name, "-", "_")
}

// errTenantOnConnection is the error of a connection that holds app.tenant outside a transaction.
var errTenantOnConnection = errors.New("tenant: the pooler guard refuses the connection")

// checkConn verifies that current_setting('app.tenant', true) is empty on conn outside a transaction
// (ST-SUP-RP04-LC04). The connectors run it on each new connection and each time a connection comes back to the
// pool, so a role default or a session setting never carries a tenant to the next client.
func checkConn(ctx context.Context, conn *pgx.Conn) error {
	var value *string
	if err := conn.QueryRow(ctx, readTenantStatement).Scan(&value); err != nil {
		return fmt.Errorf("tenant: read the tenant setting of the connection: %w", err)
	}
	if value != nil && *value != "" {
		return fmt.Errorf("%w: the session value %s is %q outside a transaction", errTenantOnConnection, SettingName, *value)
	}
	return nil
}
