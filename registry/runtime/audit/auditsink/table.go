package auditsink

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/jsell-rh/stego/registry/runtime/audit"
)

// Schema and Table name the default sink: the table entries in the schema stego_audit_sink of the service database.
const (
	Schema = "stego_audit_sink"
	Table  = "entries"
)

// roleName is a role name that the migration text takes: a lower-case SQL name of at most 63 characters.
var roleName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// Migration gives the statements of the default sink for workerRole, the role that the worker mode connects as. The
// owner role of the migration job runs them, so it owns the schema and the table. The worker role gets USAGE on the
// schema and SELECT and INSERT on the table, and no UPDATE, DELETE or TRUNCATE; the public role gets nothing. Each
// statement runs again with no error on a database that it made.
func Migration(workerRole string) (string, error) {
	if !roleName.MatchString(workerRole) {
		return "", errors.New("auditsink: the worker role is not a lower-case SQL name of at most 63 characters")
	}
	role := `"` + workerRole + `"`
	table := Schema + "." + Table
	return strings.Join([]string{
		"CREATE SCHEMA IF NOT EXISTS " + Schema,
		"REVOKE ALL ON SCHEMA " + Schema + " FROM PUBLIC",
		"CREATE TABLE IF NOT EXISTS " + table + ` (
	entry_id    text        PRIMARY KEY CHECK (entry_id <> ''),
	kind        text        NOT NULL CHECK (kind IN ('event', 'anchor')),
	tenant_id   text        NOT NULL CHECK (tenant_id <> ''),
	hash        bytea       NOT NULL,
	prev_hash   bytea,
	body        bytea       NOT NULL,
	received_at timestamptz NOT NULL DEFAULT pg_catalog.now()
)`,
		"REVOKE ALL ON TABLE " + table + " FROM PUBLIC",
		"REVOKE ALL ON TABLE " + table + " FROM " + role,
		"GRANT USAGE ON SCHEMA " + Schema + " TO " + role,
		"GRANT SELECT, INSERT ON TABLE " + table + " TO " + role,
	}, ";\n") + ";\n", nil
}

// ErrConflict is the error of an entry whose ID the default table holds with another entry. It does not wrap
// audit.ErrDuplicate, so the outbox keeps the message and sends it again.
var ErrConflict = errors.New("auditsink: the default sink holds another entry under the ID")

// TableSink is the default sink: it writes each entry as one row of stego_audit_sink.entries, keyed by its ID. A
// resend of an entry with an ID that the table holds stores nothing new. It has no update, no delete and no read.
type TableSink struct {
	db *sql.DB
}

var _ audit.Sink = (*TableSink)(nil)

// NewTableSink gives the default sink on db, the worker role's pool.
func NewTableSink(db *sql.DB) (*TableSink, error) {
	if db == nil {
		return nil, errors.New("auditsink: the default sink needs the worker role's database")
	}
	return &TableSink{db: db}, nil
}

// Append stores the events in one transaction. When the table already holds an event's ID with the same entry, that
// row is left as it is, and Append returns an error that wraps audit.ErrDuplicate after it stores the others; when
// it holds another entry under the ID, Append stores nothing and returns ErrConflict.
func (s *TableSink) Append(ctx context.Context, events []audit.Event) error {
	entries, err := eventEntries(events)
	if err != nil {
		return err
	}
	return s.store(ctx, entries)
}

// Anchor stores the anchor. A resend of the same anchor stores nothing new.
func (s *TableSink) Anchor(ctx context.Context, a audit.Anchor) error {
	entry, err := anchorEntry(a)
	if err != nil {
		return err
	}
	if err := s.store(ctx, []Entry{entry}); err != nil && !errors.Is(err, audit.ErrDuplicate) {
		return err
	}
	return nil
}

// store inserts the entries in one transaction, each with ON CONFLICT DO NOTHING.
func (s *TableSink) store(ctx context.Context, entries []Entry) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("auditsink: the default sink: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	held := 0
	for _, e := range entries {
		result, err := tx.ExecContext(ctx, "INSERT INTO "+Schema+"."+Table+
			" (entry_id, kind, tenant_id, hash, prev_hash, body) VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (entry_id) DO NOTHING",
			e.ID, e.Kind, e.Tenant, e.Hash, e.PrevHash, e.Body)
		if err != nil {
			return fmt.Errorf("auditsink: the default sink: %w", err)
		}
		if n, err := result.RowsAffected(); err == nil && n == 0 {
			// The table holds a row under the ID: it is a resend only when the row holds the same entry. Another row
			// under the ID is a conflict, which keeps the message in the outbox, and the log names the ID only.
			var kind, tenantID string
			var hash, body []byte
			if err := tx.QueryRowContext(ctx, "SELECT kind, tenant_id, hash, body FROM "+Schema+"."+Table+" WHERE entry_id = $1",
				e.ID).Scan(&kind, &tenantID, &hash, &body); err != nil {
				return fmt.Errorf("auditsink: the default sink: %w", err)
			}
			if kind != e.Kind || tenantID != e.Tenant || !bytes.Equal(hash, e.Hash) || !bytes.Equal(body, e.Body) {
				slog.WarnContext(ctx, "auditsink: the default sink holds another entry under the ID of an entry", "entry_id", e.ID)
				return ErrConflict
			}
			held++
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("auditsink: the default sink: %w", err)
	}
	if held > 0 {
		return fmt.Errorf("auditsink: the default sink holds %d of the entries: %w", held, audit.ErrDuplicate)
	}
	return nil
}
