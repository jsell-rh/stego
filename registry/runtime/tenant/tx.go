package tenant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// SettingName is the transaction-local setting that holds the tenant of a transaction. The row-level security
// policy of every tenant-owned table reads it with current_setting('app.tenant', true).
const SettingName = "app.tenant"

// setTenantStatement is the first statement of every transaction that Begin opens (ST-SUP-RP04-LC03). Its third
// argument true makes the setting transaction-local, so it ends with the transaction.
const setTenantStatement = "SELECT set_config('app.tenant', $1, true)"

// Begin opens a transaction on db and sets its tenant with SELECT set_config('app.tenant', $1, true) as the first
// statement, before the caller's first statement (ST-SUP-RP04-LC03). It refuses the zero tenant before it sends a
// statement. The caller commits or rolls back the transaction it returns.
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

// Config is what a connector needs to open a database: the connection string, the extra startup parameters and the
// statements that run on each new connection. Guard checks it before a connector opens anything.
type Config struct {
	// DSN is a PostgreSQL connection string, in URL or key=value form. The PG* environment values apply as pgx
	// reads them, and Guard checks the result.
	DSN string
	// Params are startup parameters to add to those of the DSN.
	Params map[string]string
	// AfterConnect are statements that run on each new connection, before it serves a transaction.
	AfterConnect []string
}

// allowedStartupParams are the startup parameters that set no value which differs between clients. The pooler
// keeps application_name per client itself.
var allowedStartupParams = map[string]bool{"application_name": true}

// Guard refuses a configuration that sets a session-scoped value: a connection option (options=-c name=value), a
// startup parameter, or an after-connect statement that runs SET, RESET or set_config with is_local false
// (ST-SUP-RP04-LC04). The error names each setting. A connector calls it before it opens a connection.
func Guard(cfg Config) error {
	parsed, err := pgx.ParseConfig(cfg.DSN)
	if err != nil {
		return fmt.Errorf("tenant: read the connection string: %w", err)
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
		if setting, ok := sessionSetting(statement); ok {
			refused = append(refused, fmt.Sprintf("the after-connect statement sets the session value %s", setting))
		}
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

// sessionSetting reports whether statement sets a session-scoped value, and returns the name of the setting.
// SET LOCAL and set_config with is_local true are transaction-local and pass.
func sessionSetting(statement string) (string, bool) {
	words := strings.Fields(strings.ToLower(strings.TrimSpace(statement)))
	if len(words) == 0 {
		return "", false
	}
	switch words[0] {
	case "set":
		rest := words[1:]
		if len(rest) > 0 && rest[0] == "local" {
			return "", false
		}
		if len(rest) > 0 && rest[0] == "session" {
			rest = rest[1:]
		}
		if len(rest) == 0 {
			return "(unnamed)", true
		}
		name, _, _ := strings.Cut(rest[0], "=")
		return name, true
	case "reset":
		if len(words) > 1 {
			return strings.TrimSuffix(words[1], ";"), true
		}
		return "(unnamed)", true
	}
	compact := strings.Join(words, "")
	for {
		at := strings.Index(compact, "set_config(")
		if at < 0 {
			return "", false
		}
		compact = compact[at+len("set_config("):]
		end := strings.Index(compact, ")")
		if end < 0 {
			end = len(compact)
		}
		args := strings.Split(compact[:end], ",")
		if len(args) != 3 || args[2] != "true" {
			return strings.Trim(args[0], "'\""), true
		}
	}
}

// CheckConnection verifies on the first connection of db that current_setting('app.tenant', true) is empty outside
// a transaction (ST-SUP-RP04-LC04), so a role default or a pooled connection never carries a tenant to the next
// client. A connector calls it before it returns db.
func CheckConnection(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("tenant: no database")
	}
	var value sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT current_setting('app.tenant', true)").Scan(&value); err != nil {
		return fmt.Errorf("tenant: read the tenant setting of the first connection: %w", err)
	}
	if value.Valid && value.String != "" {
		return fmt.Errorf("tenant: the pooler guard refuses the connection: the session value %s is %q outside a transaction",
			SettingName, value.String)
	}
	return nil
}
