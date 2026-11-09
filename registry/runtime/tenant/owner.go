package tenant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// MigrationJob is the name of the process run that migrates the schema. Only that run may open the owner
// connector (ST-SUP-RP04-LC06); the modes api, worker, controller and all may not.
const MigrationJob = "migrate"

// OpenOwner opens the database as the owner role, for the migration job only (ST-SUP-RP04-LC06). run names what the
// process runs: MigrationJob, or the mode it runs, such as api, worker, controller or all. For any run other than
// MigrationJob it refuses with an error that names the run and opens no connection. It checks cfg with Guard and
// the first connection with CheckConnection before it returns.
func OpenOwner(ctx context.Context, run string, cfg Config) (*sql.DB, error) {
	if run != MigrationJob {
		return nil, fmt.Errorf("tenant: the owner connector opens only in the migration job, and this process runs %q", run)
	}
	return open(ctx, cfg)
}

// Open opens the database as the runtime role, for the api, worker and controller modes. It checks cfg with Guard,
// the first connection with CheckConnection, and that the role owns no table, is not a superuser and holds no
// BYPASSRLS (ST-SUP-RP04-LC06); when a check fails it closes the database and returns an error that names the role
// and each table it owns, so startup fails.
func Open(ctx context.Context, cfg Config) (*sql.DB, error) {
	db, err := open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := checkRuntimeRole(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func open(ctx context.Context, cfg Config) (*sql.DB, error) {
	if err := Guard(cfg); err != nil {
		return nil, err
	}
	connConfig, err := pgx.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("tenant: read the connection string: %w", err)
	}
	for name, value := range cfg.Params {
		connConfig.RuntimeParams[name] = value
	}
	var options []stdlib.OptionOpenDB
	if len(cfg.AfterConnect) > 0 {
		statements := append([]string(nil), cfg.AfterConnect...)
		options = append(options, stdlib.OptionAfterConnect(func(ctx context.Context, conn *pgx.Conn) error {
			for _, statement := range statements {
				if _, err := conn.Exec(ctx, statement); err != nil {
					return fmt.Errorf("tenant: run the after-connect statement: %w", err)
				}
			}
			return nil
		}))
	}
	db := stdlib.OpenDB(*connConfig, options...)
	if err := CheckConnection(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// checkRuntimeRole fails when the role of db owns a table, directly or through a role it is a member of, is a
// superuser, or holds BYPASSRLS.
func checkRuntimeRole(ctx context.Context, db *sql.DB) error {
	var role string
	var super, bypass bool
	err := db.QueryRowContext(ctx,
		"SELECT rolname, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user").Scan(&role, &super, &bypass)
	if err != nil {
		return fmt.Errorf("tenant: read the runtime role: %w", err)
	}
	var faults []string
	if super {
		faults = append(faults, "is a superuser")
	}
	if bypass {
		faults = append(faults, "holds BYPASSRLS")
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
	if len(faults) > 0 {
		return errors.New("tenant: the runtime role " + role + " " + strings.Join(faults, " and ") +
			", so the service does not start (ST-STD-TXN-R03)")
	}
	return nil
}
