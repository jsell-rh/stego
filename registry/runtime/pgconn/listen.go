// Package pgconn is the connector package of the runtime library: the one package of the module that imports a
// database driver (ST-STD-RUNTIME-R06). It holds the Listener adapter of the outbox's live event source
// (ST-SUP-RP01-LC20).
//
// It is a library, not a framework (ST-STD-RUNTIME-R01): generated code calls its functions and types, and the
// package calls back into no generated code and owns no main loop.
package pgconn

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"stego.dev/runtime/outbox"
)

// ProbeTimeout bounds the wait for the session's own notice when the listener checks the session.
const ProbeTimeout = 5 * time.Second

// ErrPooled is the error of a session that is not one direct backend, such as a session behind a transaction-mode
// pooler: LISTEN does not work behind one.
var ErrPooled = errors.New("pgconn: the listener session is not a direct session; LISTEN needs one backend")

// Listener is the outbox Listener over one direct pgx session, opened for the source alone and in no pool.
type Listener struct {
	conn    *pgx.Conn
	pid     uint32
	pending []outbox.Notification
}

var _ outbox.Listener = (*Listener)(nil)

// Listen opens the listener session from dsn and checks that it is one direct backend (ST-SUP-RP01-LC20): the
// process ID of the startup must be the process ID that the server reports, which a pooler that gives the client
// a key of its own does not keep. Listener.Listen then proves the session with a notice of its own.
func Listen(ctx context.Context, dsn string) (*Listener, error) {
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("pgconn: read the listener DSN: %w", err)
	}
	// The source owns notification handling; the session inherits no hook.
	config.OnNotification = nil
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("pgconn: open the listener session: %w", err)
	}
	var pid uint32
	if err := conn.QueryRow(ctx, "SELECT pg_catalog.pg_backend_pid()").Scan(&pid); err != nil {
		_ = conn.Close(ctx)
		return nil, fmt.Errorf("pgconn: read the backend of the listener session: %w", err)
	}
	if pid != conn.PgConn().PID() {
		_ = conn.Close(ctx)
		return nil, ErrPooled
	}
	return &Listener{conn: conn, pid: pid}, nil
}

// Listen subscribes the session to channel, then sends a notice with a random nonce on the channel in a statement of
// its own and waits for it, at most ProbeTimeout. A session that does not get its own notice is refused with
// ErrPooled. A notice of another sender that comes during the wait is kept for WaitForNotification.
func (l *Listener) Listen(ctx context.Context, channel string) error {
	if l == nil || l.conn == nil {
		return errors.New("pgconn: the listener is not open")
	}
	if _, err := l.conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
		return err
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	nonce := "pgconn-probe-" + hex.EncodeToString(raw)
	if _, err := l.conn.Exec(ctx, "SELECT pg_catalog.pg_notify($1, $2)", channel, nonce); err != nil {
		return err
	}
	wait, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	for {
		n, err := l.conn.WaitForNotification(wait)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return ErrPooled
		}
		if n.Channel == channel && n.Payload == nonce && n.PID == l.pid {
			return nil
		}
		l.pending = append(l.pending, outbox.Notification{Channel: n.Channel, Payload: n.Payload})
	}
}

// WaitForNotification returns the next notification of the session.
func (l *Listener) WaitForNotification(ctx context.Context) (outbox.Notification, error) {
	if l == nil || l.conn == nil {
		return outbox.Notification{}, errors.New("pgconn: the listener is not open")
	}
	if len(l.pending) > 0 {
		n := l.pending[0]
		l.pending = l.pending[1:]
		return n, nil
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

// PID is the process ID of the session's backend, as the server reports it, for an operator or a test that ends the
// session.
func (l *Listener) PID() uint32 {
	if l == nil {
		return 0
	}
	return l.pid
}
