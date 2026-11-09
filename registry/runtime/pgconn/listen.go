// Package pgconn is the connector package of the runtime library: the one package of the module that imports a
// database driver (ST-STD-RUNTIME-R06). It holds the Listener adapter of the outbox's live event source
// (ST-SUP-RP01-LC20).
//
// It is a library, not a framework (ST-STD-RUNTIME-R01): generated code calls its functions and types, and the
// package calls back into no generated code and owns no main loop.
package pgconn

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"stego.dev/runtime/outbox"
)

// PoolModeParam is the connection parameter that names the pool mode of a pooler, such as pool_mode=transaction in
// the query of a DSN. A transaction-mode pooler does not keep a session, so LISTEN does not work behind it.
const PoolModeParam = "pool_mode"

// Listener is the outbox Listener over one direct pgx session, opened for the source alone and in no pool.
type Listener struct {
	conn *pgx.Conn
}

var _ outbox.Listener = (*Listener)(nil)

// Listen opens the listener session from dsn. It refuses at start a DSN that names a pooler mode other than session,
// in the parameter pool_mode or in the options parameter, because the session must be direct (ST-SUP-RP01-LC20).
func Listen(ctx context.Context, dsn string) (*Listener, error) {
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("pgconn: read the listener DSN: %w", err)
	}
	if mode, pooled := poolMode(config); pooled {
		return nil, fmt.Errorf("pgconn: the listener DSN names the pool mode %q; LISTEN needs a direct session", mode)
	}
	// The source owns notification handling; the session inherits no hook.
	config.OnNotification = nil
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("pgconn: open the listener session: %w", err)
	}
	return &Listener{conn: conn}, nil
}

// poolMode returns the pool mode that config names, and whether it is a mode other than session.
func poolMode(config *pgx.ConnConfig) (string, bool) {
	if mode, ok := config.RuntimeParams[PoolModeParam]; ok && !strings.EqualFold(mode, "session") {
		return mode, true
	}
	for _, field := range strings.Fields(config.RuntimeParams["options"]) {
		field = strings.TrimPrefix(strings.TrimPrefix(field, "-c"), "--")
		name, value, found := strings.Cut(field, "=")
		if found && strings.EqualFold(strings.ReplaceAll(name, "-", "_"), PoolModeParam) && !strings.EqualFold(value, "session") {
			return value, true
		}
	}
	return "", false
}

// Listen subscribes the session to channel.
func (l *Listener) Listen(ctx context.Context, channel string) error {
	if l == nil || l.conn == nil {
		return errors.New("pgconn: the listener is not open")
	}
	_, err := l.conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize())
	return err
}

// WaitForNotification returns the next notification of the session.
func (l *Listener) WaitForNotification(ctx context.Context) (outbox.Notification, error) {
	if l == nil || l.conn == nil {
		return outbox.Notification{}, errors.New("pgconn: the listener is not open")
	}
	n, err := l.conn.WaitForNotification(ctx)
	if err != nil {
		return outbox.Notification{}, err
	}
	return outbox.Notification{Channel: n.Channel, Payload: n.Payload}, nil
}

// Close ends the session.
func (l *Listener) Close(ctx context.Context) error {
	if l == nil || l.conn == nil {
		return nil
	}
	return l.conn.Close(ctx)
}

// PID is the backend process ID of the session, for an operator or a test that ends the session.
func (l *Listener) PID() uint32 {
	if l == nil || l.conn == nil {
		return 0
	}
	return l.conn.PgConn().PID()
}
