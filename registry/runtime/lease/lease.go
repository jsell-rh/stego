package lease

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// The defaults of ST-SUP-RP09-LC02: a lease of 15 s, renewed every 10 s, and a retry every 2 s while another
// holder has the lease.
const (
	DefaultTTL   = 15 * time.Second
	DefaultRenew = 10 * time.Second
	DefaultRetry = 2 * time.Second
)

// LeasesTable is the table that keeps one row for each lease. The migration that the generated code runs creates
// it, and inserts one row for each lease name with Declare's statement; the client only changes the rows.
const LeasesTable = `CREATE TABLE IF NOT EXISTS leases (
	name text PRIMARY KEY,
	holder text NOT NULL DEFAULT '',
	expires_at timestamptz NOT NULL DEFAULT '-infinity',
	token bigint NOT NULL DEFAULT 0
)`

// acquireStatement is the one statement of ST-SUP-RP09-LC02 that acquires and renews a lease: it gives the lease to
// the holder when the holder already has it or the lease has expired, and it increments the token each time.
const acquireStatement = `UPDATE leases SET holder=$1, expires_at=now()+make_interval(secs => $3), token=token+1 WHERE name=$2 AND (holder=$1 OR expires_at<now()) RETURNING token`

// releaseStatement makes the lease expire now, but only while the row still names the holder with its token. It
// keeps the holder and the token, so the next acquire increments the token past every write of this holder.
const releaseStatement = `UPDATE leases SET expires_at='-infinity' WHERE name=$1 AND holder=$2 AND token=$3`

// declareStatement inserts the row of a lease name with no holder, or does nothing when the row is there.
const declareStatement = `INSERT INTO leases (name) VALUES ($1) ON CONFLICT (name) DO NOTHING`

// fenceStatement locks the lease row against a change of holder or token until the caller's transaction ends, and
// reads its token (ST-SUP-RP09-LC03, ST-SUP-RP10-LC20).
const fenceStatement = `SELECT token FROM leases WHERE name=$1 FOR SHARE`

var (
	// ErrTaken is the error of Acquire when another holder has an unexpired lease.
	ErrTaken = errors.New("lease: another holder has the lease")
	// ErrFenced is the error of a write under the lease when the lease row holds a higher token than the holder's
	// (ST-SUP-RP09-LC03), and of a checkpoint when its row holds a higher token (ST-SUP-RP09-LC07).
	ErrFenced = errors.New("lease: fenced by a higher token")
	// ErrLost is the error of Renew when the holder no longer has the lease, or the renew gave a token other than
	// the next one (ST-SUP-RP09-LC04).
	ErrLost = errors.New("lease: lost")
	// ErrStopped is the error of a write through a held lease after the holder stopped (ST-SUP-RP09-LC04).
	ErrStopped = errors.New("lease: the holder stopped")
)

// DB is the part of *sql.DB that the client uses. *sql.DB and *sql.Conn satisfy it.
type DB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// Clock is the process clock of the client. The renew interval, the retry period and the holder's own bound of the
// lease run on it; expires_at in the row runs on the database clock.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time                         { return time.Now() }
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Settings are the settings of a lease client. A zero duration takes its default, and a nil clock the system clock.
type Settings struct {
	TTL   time.Duration
	Renew time.Duration
	Retry time.Duration
	Clock Clock
}

// Client acquires, renews and releases one named lease for one holder.
type Client struct {
	db     DB
	name   string
	holder string
	ttl    time.Duration
	renew  time.Duration
	retry  time.Duration
	clock  Clock
}

// New returns the client of the lease name for the holder. It refuses an empty name or holder, and a renew interval
// that is not shorter than the lease.
func New(db DB, name, holder string, settings Settings) (*Client, error) {
	if db == nil {
		return nil, errors.New("lease: no database")
	}
	if name == "" || holder == "" {
		return nil, errors.New("lease: a lease needs a name and a holder")
	}
	c := &Client{db: db, name: name, holder: holder, ttl: settings.TTL, renew: settings.Renew, retry: settings.Retry, clock: settings.Clock}
	if c.ttl == 0 {
		c.ttl = DefaultTTL
	}
	if c.renew == 0 {
		c.renew = DefaultRenew
	}
	if c.retry == 0 {
		c.retry = DefaultRetry
	}
	if c.clock == nil {
		c.clock = systemClock{}
	}
	if c.ttl < 0 || c.renew < 0 || c.retry < 0 || c.renew >= c.ttl {
		return nil, fmt.Errorf("lease %s: the renew interval %s must be shorter than the lease %s", name, c.renew, c.ttl)
	}
	return c, nil
}

// Name is the lease name.
func (c *Client) Name() string { return c.name }

// Holder is the holder this client acquires the lease for.
func (c *Client) Holder() string { return c.holder }

// TTL is the lease duration.
func (c *Client) TTL() time.Duration { return c.ttl }

// RenewEvery is the renew interval.
func (c *Client) RenewEvery() time.Duration { return c.renew }

// RetryEvery is the period of a new acquire while another holder has the lease.
func (c *Client) RetryEvery() time.Duration { return c.retry }

// Declare inserts the row of the lease name when it is not there. A migration runs it once for each lease name.
func Declare(ctx context.Context, db DB, name string) error {
	if _, err := db.ExecContext(ctx, declareStatement, name); err != nil {
		return fmt.Errorf("lease %s: declare: %w", name, err)
	}
	return nil
}

// Acquire runs the acquire statement once. It returns the held lease, or ErrTaken when another holder has an
// unexpired lease.
func (c *Client) Acquire(ctx context.Context) (*Held, error) {
	sent := c.clock.Now()
	token, err := c.update(ctx)
	if err != nil {
		return nil, err
	}
	h := &Held{client: c, token: token, expires: sent.Add(c.ttl), stopped: make(chan struct{})}
	return h, nil
}

func (c *Client) update(ctx context.Context) (int64, error) {
	var token int64
	err := c.db.QueryRowContext(ctx, acquireStatement, c.holder, c.name, c.ttl.Seconds()).Scan(&token)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrTaken
	}
	if err != nil {
		return 0, fmt.Errorf("lease %s: %w", c.name, err)
	}
	return token, nil
}

// Held is a lease that its holder has. Its token changes at each renew. Once it stops, through a failed renew, a
// fenced write or a release, no write goes through it again.
type Held struct {
	client *Client

	// mu orders a renew against a fence: a fence reads the token and locks the row while no renew runs.
	mu      sync.RWMutex
	token   int64
	expires time.Time

	// act orders the writes under the lease against the release: a release waits for every write that runs.
	act sync.RWMutex

	stop    sync.Once
	stopped chan struct{}
	cause   atomic.Pointer[error]
}

// Name is the lease name.
func (h *Held) Name() string { return h.client.name }

// Token is the holder's fencing token as of the last acquire or renew.
func (h *Held) Token() int64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.token
}

// Expires is the time on the process clock at which the lease can expire at the latest: the lease duration after
// the last good acquire or renew was sent, so it is never after expires_at in the row.
func (h *Held) Expires() time.Time {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.expires
}

// TTL is the lease duration.
func (h *Held) TTL() time.Duration { return h.client.ttl }

// RenewEvery is the renew interval.
func (h *Held) RenewEvery() time.Duration { return h.client.renew }

// Done is closed when the holder stops.
func (h *Held) Done() <-chan struct{} { return h.stopped }

// Err is the cause of the stop, or nil while the holder holds the lease.
func (h *Held) Err() error {
	if cause := h.cause.Load(); cause != nil {
		return *cause
	}
	return nil
}

func (h *Held) halt(cause error) {
	h.stop.Do(func() {
		h.cause.Store(&cause)
		close(h.stopped)
	})
}

func (h *Held) active() error {
	select {
	case <-h.stopped:
		return fmt.Errorf("lease %s: %w: %w", h.client.name, ErrStopped, h.Err())
	default:
		return nil
	}
}

// Renew runs the acquire statement again. When the holder no longer has the lease, the statement fails, or the
// token is not the next one, the holder stops and Renew returns the error (ST-SUP-RP09-LC04).
func (h *Held) Renew(ctx context.Context) error {
	if err := h.active(); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	sent := h.client.clock.Now()
	token, err := h.client.update(ctx)
	switch {
	case errors.Is(err, ErrTaken):
		err = fmt.Errorf("lease %s: %w: another holder has the lease", h.client.name, ErrLost)
	case err != nil:
		err = fmt.Errorf("%w: %w", ErrLost, err)
	case token != h.token+1:
		err = fmt.Errorf("lease %s: %w: the renew gave token %d, not %d", h.client.name, ErrLost, token, h.token+1)
	}
	if err != nil {
		h.halt(err)
		return err
	}
	h.token = token
	h.expires = sent.Add(h.client.ttl)
	return nil
}

// Release stops the holder, waits for the writes under the lease that run, and makes the lease expire, so another
// holder can take it at once.
func (h *Held) Release(ctx context.Context) error {
	h.halt(fmt.Errorf("lease %s: released", h.client.name))
	h.act.Lock()
	defer h.act.Unlock()
	if _, err := h.client.db.ExecContext(ctx, releaseStatement, h.client.name, h.client.holder, h.Token()); err != nil {
		return fmt.Errorf("lease %s: release: %w", h.client.name, err)
	}
	return nil
}

// Fence locks the lease row in the caller's transaction until the transaction ends, so that no other holder can
// take the lease while it runs, and returns the holder's token for the caller's writes to carry. When the row
// holds another token, the holder stops and Fence returns ErrFenced (ST-SUP-RP09-LC03).
func (h *Held) Fence(ctx context.Context, tx *sql.Tx) (int64, error) {
	if err := h.active(); err != nil {
		return 0, err
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	var token int64
	if err := tx.QueryRowContext(ctx, fenceStatement, h.client.name).Scan(&token); err != nil {
		return 0, fmt.Errorf("lease %s: fence: %w", h.client.name, err)
	}
	if token != h.token {
		err := fmt.Errorf("lease %s: %w: the row holds token %d, the holder %d", h.client.name, ErrFenced, token, h.token)
		h.halt(err)
		return 0, err
	}
	return token, nil
}

// deadline is the context of a write under the lease: it ends when the lease can expire on the process clock.
func (h *Held) deadline(ctx context.Context) (context.Context, context.CancelFunc) {
	left := h.Expires().Sub(h.client.clock.Now())
	return context.WithTimeout(ctx, left)
}

// WithToken runs write in one transaction that holds the lease row (Fence), with the holder's token for the write
// to carry as a bound parameter, and commits it. The transaction ends before the lease can expire. A write after
// the holder stopped fails with ErrStopped and touches nothing; a write that meets a higher token fails with
// ErrFenced, is rolled back and stops the holder.
func (h *Held) WithToken(ctx context.Context, write func(ctx context.Context, tx *sql.Tx, token int64) error) error {
	h.act.RLock()
	defer h.act.RUnlock()
	if err := h.active(); err != nil {
		return err
	}
	ctx, cancel := h.deadline(ctx)
	defer cancel()
	tx, err := h.client.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("lease %s: begin: %w", h.client.name, err)
	}
	defer func() { _ = tx.Rollback() }()
	token, err := h.Fence(ctx, tx)
	if err != nil {
		return err
	}
	if err := write(ctx, tx, token); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lease %s: commit: %w", h.client.name, err)
	}
	return nil
}

// Hold acquires the lease, retrying every retry period while another holder has it, runs work with the held lease,
// and renews the lease every renew interval while work runs. When a renew fails or gives another token, or a write
// meets a higher token, it cancels work's context at once, which is within one renew interval and before the lease
// can expire, waits for work to return, and returns the cause (ST-SUP-RP09-LC03, ST-SUP-RP09-LC04). When work
// returns or ctx ends, it releases the lease.
func (c *Client) Hold(ctx context.Context, work func(ctx context.Context, held *Held) error) error {
	for {
		h, err := c.Acquire(ctx)
		if err == nil {
			return c.hold(ctx, h, work)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.clock.After(c.retry):
		}
	}
}

func (c *Client) hold(ctx context.Context, h *Held, work func(ctx context.Context, held *Held) error) error {
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- work(workCtx, h) }()
	release := func() {
		releaseCtx, cancelRelease := context.WithTimeout(context.WithoutCancel(ctx), c.renew)
		defer cancelRelease()
		_ = h.Release(releaseCtx)
	}
	for {
		select {
		case err := <-done:
			if h.Err() == nil {
				release()
			}
			if errors.Is(h.Err(), ErrFenced) {
				return h.Err()
			}
			return err
		case <-h.Done():
			cancel()
			err := <-done
			if cause := h.Err(); errors.Is(cause, ErrFenced) || errors.Is(cause, ErrLost) {
				return cause
			}
			return err
		case <-ctx.Done():
			cancel()
			err := <-done
			if h.Err() == nil {
				release()
			}
			return errors.Join(ctx.Err(), err)
		case <-c.clock.After(c.renew):
			// The renew must answer before the lease can expire on the process clock. A renew can wait for a write
			// under the lease that holds the row, and that write ends before the same time.
			renewCtx, cancelRenew := h.deadline(ctx)
			err := h.Renew(renewCtx)
			cancelRenew()
			if err != nil {
				cancel()
				<-done
				return err
			}
		}
	}
}
