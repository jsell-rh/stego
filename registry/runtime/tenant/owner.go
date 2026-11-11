package tenant

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"weak"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// MigrationJob is the name of the process run that migrates the schema. Only that run may open the owner
// connector (ST-SUP-RP04-LC06); the modes api, worker, controller and all may not.
const MigrationJob = "migrate"

// OpenOwner opens the database as the owner role, for the migration job only (ST-SUP-RP04-LC06). run names what the
// process runs: MigrationJob, or the mode it runs, such as api, worker, controller or all. For any run other than
// MigrationJob it refuses with an error that names the run and opens no connection. It checks cfg with Guard and
// each connection with the pooler guard's check of app.tenant before it returns.
func OpenOwner(ctx context.Context, run string, cfg Config) (*sql.DB, error) {
	if run != MigrationJob {
		return nil, fmt.Errorf("tenant: the owner connector opens only in the migration job, and this process runs %q", run)
	}
	db, err := open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	remember(db)
	return db, nil
}

// Open opens the database as the runtime role, for the api, worker and controller modes. It checks cfg with Guard,
// each connection with the pooler guard's check of app.tenant, and that the role owns no table, is not a superuser
// and holds no BYPASSRLS, also through a role it is a member of (ST-SUP-RP04-LC06). When a check fails it closes the
// database and returns an error that names the role and each fault, so startup fails.
func Open(ctx context.Context, cfg Config) (*sql.DB, error) {
	db, err := open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := checkRuntimeRole(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	remember(db)
	return db, nil
}

// opened holds a weak pointer to each pool that Open returned, so Opened can tell such a pool from one that
// sql.Open or another connector made. A pool that is no longer reachable leaves the set.
var opened = struct {
	mu    sync.Mutex
	pools map[weak.Pointer[sql.DB]]struct{}
}{pools: map[weak.Pointer[sql.DB]]struct{}{}}

func remember(db *sql.DB) {
	key := weak.Make(db)
	opened.mu.Lock()
	opened.pools[key] = struct{}{}
	opened.mu.Unlock()
	runtime.AddCleanup(db, func(key weak.Pointer[sql.DB]) {
		opened.mu.Lock()
		delete(opened.pools, key)
		opened.mu.Unlock()
	}, key)
}

// Opened reports whether db is a pool that Open or OpenOwner returned and that is not closed (ST-SUP-RP04-LC08): a
// pool whose connections the pooler guard checks each time they come back, so a connection with a tenant set is
// closed. It reports false for any other pool, for a copy of a pool value, whose address differs, and for a closed
// pool. The set holds weak pointers only, so it keeps no pool alive, and a pool made later at a reused address does
// not match. The command and query runtimes take only a pool for which it reports true (ST-SUP-RP10-LC30,
// ST-SUP-RP11-LC14).
func Opened(db *sql.DB) bool {
	if db == nil {
		return false
	}
	opened.mu.Lock()
	_, ok := opened.pools[weak.Make(db)]
	opened.mu.Unlock()
	if !ok {
		return false
	}
	// A ping with a context that has ended reads only the closed flag of the pool: database/sql refuses a closed
	// pool before it looks at the context, and it opens no connection.
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	return errors.Is(db.PingContext(ended), context.Canceled)
}

// Connection gives the connection configuration that Open and OpenOwner open with, after the checks that they make
// before any connection: Guard, which refuses a session setting (ST-SUP-RP04-LC04), and the transport check, which
// refuses a configuration or a connection string whose TLS does not verify the server, unless cfg.InsecureLoopback
// holds and every target is a loopback IP address (ST-SUP-RP04-LC09). A connector that opens a session of its own,
// such as the listener of pgconn (ST-SUP-RP01-LC20), opens it with this configuration and checks each session with
// CheckSession. The result is a copy, so a change of the caller's value after the check changes nothing.
func Connection(cfg Config) (*pgx.ConnConfig, error) {
	if err := Guard(cfg); err != nil {
		return nil, err
	}
	conn, err := connConfig(cfg)
	if err != nil {
		return nil, err
	}
	// conn is a copy of the opener's configuration or the parsed connection string, made before the check, so a
	// change of the caller's value after the check changes nothing: the pool opens with the TLS policy that the
	// check saw. A connection string gets the same check as a configuration (ST-SUP-RP04-LC09).
	if err := transport(conn, cfg.InsecureLoopback); err != nil {
		return nil, err
	}
	if len(cfg.Params) > 0 && conn.RuntimeParams == nil {
		conn.RuntimeParams = map[string]string{}
	}
	for name, value := range cfg.Params {
		conn.RuntimeParams[name] = value
	}
	return conn, nil
}

// CheckSession is the pooler guard's check of one session (ST-SUP-RP04-LC04): current_setting('app.tenant', true)
// must be empty outside a transaction. Open and OpenOwner run it on each new connection and each time a connection
// comes back to the pool.
func CheckSession(ctx context.Context, conn *pgx.Conn) error { return checkConn(ctx, conn) }

func open(ctx context.Context, cfg Config) (*sql.DB, error) {
	conn, err := Connection(cfg)
	if err != nil {
		return nil, err
	}
	db := stdlib.OpenDB(*conn,
		stdlib.OptionAfterConnect(checkConn),
		stdlib.OptionResetSession(func(ctx context.Context, conn *pgx.Conn) error {
			if err := checkConn(ctx, conn); err != nil {
				// The pool discards the connection, and the next request gets a new one.
				return driver.ErrBadConn
			}
			return nil
		}),
	)
	if b := cfg.Pool; b.MaxOpen > 0 || b.MaxIdle > 0 || b.MaxLifetime > 0 || b.MaxIdleTime > 0 {
		if b.MaxOpen > 0 {
			db.SetMaxOpenConns(b.MaxOpen)
		}
		if b.MaxIdle > 0 {
			db.SetMaxIdleConns(b.MaxIdle)
		}
		db.SetConnMaxLifetime(b.MaxLifetime)
		db.SetConnMaxIdleTime(b.MaxIdleTime)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("tenant: open the database: %w", err)
	}
	return db, nil
}

// checkRuntimeRole fails when the role of db, or a role it is a member of, is a superuser, holds BYPASSRLS or owns
// a table, and when the role of db is a member of the outbox relay role stego_outbox_relay or of a hook role, a role
// that may run the outbox relay functions, or of the idempotency purge role stego_idempotency_purge, which reads and
// deletes the key rows of every tenant (ST-SUP-RP07-LC11). A member of such a role can take it with SET ROLE, so membership counts
// as holding it.
func checkRuntimeRole(ctx context.Context, db *sql.DB) error {
	var role string
	if err := db.QueryRowContext(ctx, "SELECT current_user").Scan(&role); err != nil {
		return fmt.Errorf("tenant: read the runtime role: %w", err)
	}
	var faults []string
	privileged, err := db.QueryContext(ctx, `SELECT r.rolname, r.rolsuper, r.rolbypassrls FROM pg_roles r
WHERE (r.rolsuper OR r.rolbypassrls) AND pg_has_role(current_user, r.oid, 'MEMBER')
ORDER BY r.rolname`)
	if err != nil {
		return fmt.Errorf("tenant: read the roles of the runtime role %s: %w", role, err)
	}
	defer privileged.Close()
	for privileged.Next() {
		var name string
		var super, bypass bool
		if err := privileged.Scan(&name, &super, &bypass); err != nil {
			return fmt.Errorf("tenant: read the roles of the runtime role %s: %w", role, err)
		}
		via := ""
		if name != role {
			via = " through its membership of " + name
		}
		if super {
			faults = append(faults, "is a superuser"+via)
		}
		if bypass {
			faults = append(faults, "holds BYPASSRLS"+via)
		}
	}
	if err := privileged.Err(); err != nil {
		return fmt.Errorf("tenant: read the roles of the runtime role %s: %w", role, err)
	}
	rows, err := db.QueryContext(ctx, `SELECT format('%I.%I', n.nspname, c.relname)
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p')
	AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg_toast%'
	AND pg_has_role(current_user, c.relowner, 'MEMBER')
ORDER BY 1`)
	if err != nil {
		return fmt.Errorf("tenant: read the tables of the runtime role %s: %w", role, err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return fmt.Errorf("tenant: read the tables of the runtime role %s: %w", role, err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("tenant: read the tables of the runtime role %s: %w", role, err)
	}
	if len(tables) > 0 {
		faults = append(faults, "owns the tables "+strings.Join(tables, ", "))
	}
	// The runtime role must not take the outbox relay role or a hook role, the worker role that runs the relay
	// functions and the authz state function (ST-SUP-RP06-LC21), or the idempotency purge role (ST-SUP-RP07-LC11),
	// with SET ROLE.
	members, err := db.QueryContext(ctx, `SELECT r.rolname FROM pg_roles r
WHERE r.rolname <> current_user AND pg_has_role(current_user, r.oid, 'MEMBER')
	AND (r.rolname IN ('stego_outbox_relay', 'stego_idempotency_purge')
		OR (pg_catalog.to_regprocedure('stego_outbox.claim(timestamptz, integer, uuid, bigint)') IS NOT NULL
			AND pg_catalog.has_function_privilege(r.oid, 'stego_outbox.claim(timestamptz, integer, uuid, bigint)', 'EXECUTE')))
ORDER BY r.rolname`)
	if err != nil {
		return fmt.Errorf("tenant: read the roles of the runtime role %s: %w", role, err)
	}
	defer members.Close()
	var taken []string
	for members.Next() {
		var name string
		if err := members.Scan(&name); err != nil {
			return fmt.Errorf("tenant: read the roles of the runtime role %s: %w", role, err)
		}
		taken = append(taken, name)
	}
	if err := members.Err(); err != nil {
		return fmt.Errorf("tenant: read the roles of the runtime role %s: %w", role, err)
	}
	if len(taken) > 0 {
		faults = append(faults, "is a member of the outbox relay, idempotency purge or hook roles "+strings.Join(taken, ", "))
	}
	if len(faults) > 0 {
		return errors.New("tenant: the runtime role " + role + " " + strings.Join(faults, " and ") +
			", so the service does not start (ST-STD-TXN-R03)")
	}
	return nil
}
