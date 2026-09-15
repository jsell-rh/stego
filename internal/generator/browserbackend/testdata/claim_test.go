package browser

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
)

// Keep the real PostgreSQL operation. Lose its first returned row at the
// driver boundary, before the caller can use a refresh token.
type claimReadFault struct {
	driver.Connector
	failure    error
	used       atomic.Bool
	loseCommit bool
}

func (c *claimReadFault) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &claimFaultConn{Conn: conn, fault: c}, nil
}

type claimFaultConn struct {
	driver.Conn
	fault *claimReadFault
}

func (c *claimFaultConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, options)
	if err != nil || !c.fault.loseCommit {
		return tx, err
	}
	return &claimFaultTx{Tx: tx, fault: c.fault}, nil
}

func (c *claimFaultConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	if c.fault.failure != nil && strings.HasPrefix(query, "UPDATE stego_browser_sessions SET state='refreshing'") && c.fault.used.CompareAndSwap(false, true) {
		return &claimFaultRows{Rows: rows, failure: c.fault.failure}, nil
	}
	return rows, nil
}

func (c *claimFaultConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

type claimFaultTx struct {
	driver.Tx
	fault *claimReadFault
}

func (tx *claimFaultTx) Commit() error {
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	tx.fault.used.Store(true)
	return io.ErrUnexpectedEOF
}

func faultClaimStore(t *testing.T, f *fixture, fault *claimReadFault) *sessionStore {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := f.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = conn.Raw(func(raw any) error {
		config := raw.(*stdlib.Conn).Conn().Config().Copy()
		fault.Connector = stdlib.GetConnector(*config)
		return nil
	})
	conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	probe := sql.OpenDB(fault)
	probe.SetMaxOpenConns(1)
	probe.SetMaxIdleConns(1)
	t.Cleanup(func() { probe.Close() })
	store := *f.backend.store
	store.db = probe
	return &store
}

type claimFaultRows struct {
	driver.Rows
	failure error
}

func (r *claimFaultRows) Next(values []driver.Value) error {
	if err := r.Rows.Next(values); err != nil {
		return err
	}
	return r.failure
}

func TestRefreshClaimReadFailureKeepsSessionUsable(t *testing.T) {
	for name, failure := range map[string]error{"lost row": io.ErrUnexpectedEOF, "cancelled": context.Canceled, "deadline": context.DeadlineExceeded} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			active, _ := login(t, f)
			expireAccess(t, f, active)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			before, state, _, err := f.backend.store.read(ctx, active.Value)
			require(t, err == nil && state == "active", "session setup failed")
			fault := &claimReadFault{failure: failure}
			store := faultClaimStore(t, f, fault)
			_, err = store.claimRefresh(ctx, active.Value)
			require(t, errors.Is(err, errStore) && fault.used.Load(), "claim did not report the injected read failure")
			after, state, _, err := f.backend.store.read(ctx, active.Value)
			require(t, err == nil && state == "active" && after.Access == before.Access && after.Refresh == before.Refresh, "failed claim left a pending refresh or changed credentials")
			_, refreshes, _ := f.oidc.counts()
			require(t, refreshes == 0, "failed claim used a refresh token")
			_, err = f.backend.active(ctx, active.Value)
			require(t, err == nil, "session could not renew after a failed claim")
			_, refreshes, _ = f.oidc.counts()
			require(t, refreshes == 1, "recovery did not use one refresh request")
		})
	}
}

func TestRefreshClaimLostCommitRequiresLogin(t *testing.T) {
	f := setup(t)
	active, _ := login(t, f)
	expireAccess(t, f, active)
	fault := &claimReadFault{loseCommit: true}
	store := faultClaimStore(t, f, fault)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := store.claimRefresh(ctx, active.Value)
	require(t, errors.Is(err, errSession) && fault.used.Load(), "uncertain commit did not require a new login")
	_, _, _, err = f.backend.store.read(ctx, active.Value)
	require(t, errors.Is(err, errSession), "uncertain commit retained its session")
	_, refreshes, _ := f.oidc.counts()
	require(t, refreshes == 0, "uncertain claim used a refresh token")
}
