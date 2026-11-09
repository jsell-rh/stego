package ledger

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"stego.dev/runtime/internal/pgtest"
	"stego.dev/runtime/lease"
	"stego.dev/runtime/money"
	"stego.dev/runtime/tenant"
)

// The PostgreSQL tests run against the local PostgreSQL server of pgtest: the server that STEGO_TEST_POSTGRES_DSN
// names, a superuser URL, else a container without privilege that the test binary starts and removes
// (ST-STD-LOCAL-R04). They reach the server through pgx's database/sql driver (ST-STD-RUNTIME-R06). Each test makes
// a database of its own, owned by the owner role, applies the DDL text and the lease runtime's leases table there
// as the owner, and drops the database at the end. Every test uses one ledger in USD, and the tenant acme with the
// accounts: limited (credit normal, minimum balance 0), hot (debit normal, hot), plain (credit normal, no limit),
// dr (debit normal) and cr (credit normal).

const (
	ownerRole    = "stego_ledger_owner"
	runtimeRole  = "stego_ledger_runtime"
	rolePassword = "stego-ledger-test"
	leaseName    = "controller"
	acme         = "acme"
	other        = "other"
	guardMessage = "the ledger table"
)

// TestMain gives the PostgreSQL tests of the package their local server (ST-STD-LOCAL-R04).
func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

var rolesOnce sync.Once

type fixture struct {
	t       *testing.T
	ctx     context.Context
	cfg     *pgx.ConnConfig
	db      string
	admin   *sql.DB
	owner   *sql.DB
	runtime *sql.DB
	traced  *sql.DB // runtime connections whose statements the trace records
	trace   *statementTrace
	clock   *fakeClock
	usd     *Ledger
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := pgtest.DSN(t)
	ctx := context.Background()
	base, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*base)
	t.Cleanup(func() { admin.Close() })
	rolesOnce.Do(func() {
		for _, role := range []string{ownerRole, runtimeRole} {
			_, err := admin.ExecContext(ctx, fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s'", role, rolePassword))
			var pgErr *pgconn.PgError
			if err != nil && !(errors.As(err, &pgErr) && pgErr.Code == "42710") {
				t.Fatalf("create the role %s: %v", role, err)
			}
		}
	})
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, ctx: ctx, cfg: base, db: "ledger_" + hex.EncodeToString(suffix[:]), admin: admin, trace: &statementTrace{},
		clock: &fakeClock{now: time.Now().UTC()}}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+f.db+" OWNER "+ownerRole); err != nil {
		t.Fatalf("create the database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(ctx, "DROP DATABASE "+f.db+" WITH (FORCE)"); err != nil {
			t.Errorf("drop the database %s: %v", f.db, err)
		}
	})
	f.owner = f.open(ownerRole, nil)
	ddl, err := DDL(Config{RuntimeRole: runtimeRole})
	if err != nil {
		t.Fatal(err)
	}
	setup := ddl + lease.LeasesTable + `;
INSERT INTO leases (name) VALUES ('` + leaseName + `');
GRANT SELECT, UPDATE ON leases TO ` + runtimeRole + `;`
	if _, err := f.owner.ExecContext(ctx, setup); err != nil {
		t.Fatalf("apply the DDL text: %v", err)
	}
	f.runtime = f.open(runtimeRole, nil)
	f.traced = f.open(runtimeRole, f.trace)
	f.usd = f.ledger("usd", "USD")
	for _, id := range []string{acme, other} {
		f.openAccounts(f.usd, id)
	}
	return f
}

func (f *fixture) open(role string, trace *statementTrace) *sql.DB {
	cfg := f.cfg.Copy()
	cfg.Database, cfg.User, cfg.Password = f.db, role, rolePassword
	cfg.RuntimeParams = map[string]string{"TimeZone": "UTC"}
	if trace != nil {
		cfg.Tracer = trace
	}
	db := stdlib.OpenDB(*cfg)
	f.t.Cleanup(func() { db.Close() })
	return db
}

func (f *fixture) ledger(id, currency string) *Ledger {
	f.t.Helper()
	l, err := New(Settings{ID: id, Currency: currency, PendingTimeout: 15 * time.Minute, SnapshotEvery: 3, Now: f.clock.Now})
	if err != nil {
		f.t.Fatal(err)
	}
	return l
}

func (f *fixture) openAccounts(l *Ledger, tenantID string) {
	f.t.Helper()
	zero := int64(0)
	accounts := []Account{
		{ID: "limited", Code: "customer_wallet", Normal: Credit, MinBalance: &zero},
		{ID: "hot", Code: "settlement", Normal: Debit, Hot: true},
		{ID: "plain", Code: "fees", Normal: Credit},
		{ID: "dr", Code: "receivable", Normal: Debit},
		{ID: "cr", Code: "payable", Normal: Credit},
	}
	f.inTx(f.runtime, tenantID, func(tx *sql.Tx) error {
		for _, a := range accounts {
			if err := l.Open(f.ctx, tx, f.tn(tenantID), a); err != nil {
				return err
			}
		}
		return nil
	})
}

// tn returns the tenant of the ID, built as the tenant runtime builds a tenant from a column that a statement read
// (tenant.ScanRows).
func (f *fixture) tn(id string) tenant.Tenant {
	f.t.Helper()
	rows, err := f.admin.QueryContext(f.ctx, "SELECT $1::text", id)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		f.t.Fatal("no row")
	}
	t, err := tenant.ScanRows(rows)
	if err != nil {
		f.t.Fatal(err)
	}
	return t
}

// try runs fn in a transaction of the tenant on db, opened with tenant.Begin, and commits it with tenant.Commit
// when fn returns no error; else it rolls back and returns the error.
func (f *fixture) try(db *sql.DB, tenantID string, fn func(tx *sql.Tx) error) error {
	t := f.tn(tenantID)
	tx, err := tenant.Begin(f.ctx, db, t, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tenant.Commit(f.ctx, tx, t)
}

// inTx is try, and fails the test on an error.
func (f *fixture) inTx(db *sql.DB, tenantID string, fn func(tx *sql.Tx) error) {
	f.t.Helper()
	if err := f.try(db, tenantID, fn); err != nil {
		f.t.Fatal(err)
	}
}

func usd(n int64) money.Money {
	m, err := money.New(n, "USD")
	if err != nil {
		panic(err)
	}
	return m
}

func dr(account string, n int64) Entry { return Entry{Account: account, Side: Debit, Amount: usd(n)} }
func cr(account string, n int64) Entry { return Entry{Account: account, Side: Credit, Amount: usd(n)} }

// post posts a transfer for acme in a transaction of its own on db, and commits it.
func (f *fixture) post(db *sql.DB, key string, entries ...Entry) Transfer {
	f.t.Helper()
	var out Transfer
	f.inTx(db, acme, func(tx *sql.Tx) error {
		var err error
		out, err = f.usd.Post(f.ctx, tx, f.tn(acme), Posting{Key: key, Code: "topup", Entries: entries})
		return err
	})
	return out
}

func (f *fixture) pend(key string, entries ...Entry) Transfer {
	f.t.Helper()
	var out Transfer
	f.inTx(f.runtime, acme, func(tx *sql.Tx) error {
		var err error
		out, err = f.usd.Pend(f.ctx, tx, f.tn(acme), Posting{Key: key, Code: "purchase", Entries: entries})
		return err
	})
	return out
}

// call runs one ledger call for acme in a transaction of its own on the runtime pool, and returns its error; the
// transaction commits only when the call returned no error.
func (f *fixture) call(fn func(tx *sql.Tx, t tenant.Tenant) (Transfer, error)) (Transfer, error) {
	var out Transfer
	err := f.try(f.runtime, acme, func(tx *sql.Tx) error {
		var err error
		out, err = fn(tx, f.tn(acme))
		return err
	})
	return out, err
}

// count returns the number of rows of the table that match the condition, for acme, read as the owner.
func (f *fixture) count(table, where string, args ...any) int {
	f.t.Helper()
	var n int
	f.inTx(f.owner, acme, func(tx *sql.Tx) error {
		return tx.QueryRowContext(f.ctx, "SELECT count(*) FROM "+table+" WHERE "+where, args...).Scan(&n)
	})
	return n
}

// row returns the row of the transfer as JSON, read as the owner.
func (f *fixture) row(table, where string, args ...any) string {
	f.t.Helper()
	var out string
	f.inTx(f.owner, acme, func(tx *sql.Tx) error {
		return tx.QueryRowContext(f.ctx, "SELECT coalesce(json_agg(r ORDER BY r::text)::text, '') FROM "+table+" r WHERE "+where, args...).Scan(&out)
	})
	return out
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// statementTrace records each statement that the traced connections send.
type statementTrace struct {
	mu   sync.Mutex
	sqls []string
}

func (s *statementTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	s.mu.Lock()
	s.sqls = append(s.sqls, data.SQL)
	s.mu.Unlock()
	return ctx
}

func (s *statementTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (s *statementTrace) since(mark int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.sqls[mark:])
}

func (s *statementTrace) mark() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sqls)
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func TestPost(t *testing.T) {
	t.Run("ST-SUP-RP08-LC03-T01 A transfer and its entries are inserted in one statement", func(t *testing.T) {
		f := newFixture(t)
		mark := f.trace.mark()
		var tr Transfer
		var transferCmin, transferXmin string
		var entryCmins, entryXmins []string
		f.inTx(f.traced, acme, func(tx *sql.Tx) error {
			var err error
			tr, err = f.usd.Post(f.ctx, tx, f.tn(acme), Posting{Key: "k1", Code: "topup", Entries: []Entry{dr("hot", 500), cr("limited", 500)}})
			if err != nil {
				return err
			}
			// Rows that one statement inserted share the transaction and the command ID.
			if err := tx.QueryRowContext(f.ctx, "SELECT cmin::text, xmin::text FROM transfers WHERE id = $1", tr.ID).Scan(&transferCmin, &transferXmin); err != nil {
				return err
			}
			rows, err := tx.QueryContext(f.ctx, "SELECT cmin::text, xmin::text FROM entries WHERE transfer_id = $1", tr.ID)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var c, x string
				if err := rows.Scan(&c, &x); err != nil {
					return err
				}
				entryCmins, entryXmins = append(entryCmins, c), append(entryXmins, x)
			}
			return rows.Err()
		})
		if len(entryCmins) != 2 {
			t.Fatalf("the transfer has %d entries, not 2", len(entryCmins))
		}
		for i := range entryCmins {
			if entryCmins[i] != transferCmin || entryXmins[i] != transferXmin {
				t.Fatalf("the transfer row is command %s of %s and an entry row command %s of %s: not one statement",
					transferCmin, transferXmin, entryCmins[i], entryXmins[i])
			}
		}
		var writes []string
		for _, q := range f.trace.since(mark) {
			if strings.Contains(q, "INSERT INTO") && (strings.Contains(q, ".transfers") || strings.Contains(q, ".entries")) {
				writes = append(writes, q)
			}
		}
		if len(writes) != 1 || !strings.Contains(writes[0], ".transfers") || !strings.Contains(writes[0], ".entries") {
			t.Fatalf("the statements that write transfers or entries are %q; want one that writes both", writes)
		}
	})

	t.Run("ST-SUP-RP08-LC03-T02 A transfer with one entry is refused", func(t *testing.T) {
		f := newFixture(t)
		_, err := f.call(func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
			return f.usd.Post(f.ctx, tx, tn, Posting{Key: "one", Code: "topup", Entries: []Entry{dr("hot", 0+1)}})
		})
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("Post with one entry: %v; want ErrInvalid", err)
		}
		// The statement trigger refuses it too, for a writer that does not go through Post.
		err = f.try(f.runtime, acme, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(f.ctx, `WITH t AS (INSERT INTO transfers (tenant_id, ledger_id, idempotency_key, kind, code, amount, currency, status, effective_at)
 VALUES ('acme', 'usd', 'raw-one', 'transfer', 'topup', 5, 'USD', 'posted', now()) RETURNING id)
 INSERT INTO entries (tenant_id, ledger_id, transfer_id, seq, account_id, side, amount, currency, pending, effective_at, posted_at)
 SELECT 'acme', 'usd', t.id, 1, 'hot', 'debit', 5, 'USD', false, now(), now() FROM t`)
			return err
		})
		if sqlState(err) != "23514" {
			t.Fatalf("a raw insert of one entry: %v; want the check violation of the trigger", err)
		}
		if n := f.count("transfers", "true") + f.count("entries", "true"); n != 0 {
			t.Fatalf("%d transfer and entry rows; want none", n)
		}
	})

	t.Run("ST-SUP-RP08-LC03-T03 An unbalanced transfer is refused", func(t *testing.T) {
		f := newFixture(t)
		_, err := f.call(func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
			return f.usd.Post(f.ctx, tx, tn, Posting{Key: "unbalanced", Code: "topup", Entries: []Entry{dr("hot", 100), cr("limited", 90)}})
		})
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("Post of a debit of 100 and a credit of 90: %v; want ErrInvalid", err)
		}
		// A raw insert that does not balance, and a raw entry pair added to a balanced transfer in a second
		// statement, are refused by the statement trigger.
		raw := `WITH t AS (INSERT INTO transfers (tenant_id, ledger_id, idempotency_key, kind, code, amount, currency, status, effective_at)
 VALUES ('acme', 'usd', $1, 'transfer', 'topup', 100, 'USD', 'posted', now()) RETURNING id)
 INSERT INTO entries (tenant_id, ledger_id, transfer_id, seq, account_id, side, amount, currency, pending, effective_at, posted_at)
 SELECT 'acme', 'usd', t.id, x.seq, x.account, x.side, x.amount, 'USD', false, now(), now()
 FROM t, (VALUES (1, 'hot', 'debit', 100), (2, 'limited', 'credit', $2::bigint)) AS x(seq, account, side, amount)`
		err = f.try(f.runtime, acme, func(tx *sql.Tx) error { _, err := tx.ExecContext(f.ctx, raw, "raw-unbalanced", 90); return err })
		if sqlState(err) != "23514" || !strings.Contains(err.Error(), "does not balance") {
			t.Fatalf("a raw unbalanced insert: %v; want the check violation of the trigger", err)
		}
		err = f.try(f.runtime, acme, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(f.ctx, raw, "raw-balanced", 100); err != nil {
				return err
			}
			_, err := tx.ExecContext(f.ctx, `INSERT INTO entries (tenant_id, ledger_id, transfer_id, seq, account_id, side, amount, currency, pending, effective_at, posted_at)
 SELECT 'acme', 'usd', id, s, 'hot', side, 7, 'USD', false, now(), now() FROM transfers, (VALUES (3, 'debit'), (4, 'credit')) AS x(s, side)
 WHERE idempotency_key = 'raw-balanced'`)
			return err
		})
		if sqlState(err) != "23514" || !strings.Contains(err.Error(), "one statement") {
			t.Fatalf("entries added in a second statement: %v; want the check violation of the trigger", err)
		}
		if n := f.count("transfers", "true") + f.count("entries", "true"); n != 0 {
			t.Fatalf("%d transfer and entry rows; want none", n)
		}
	})

	t.Run("ST-SUP-RP08-LC03-T04 An entry in another currency than the ledger's is refused", func(t *testing.T) {
		f := newFixture(t)
		eur := func(n int64) money.Money { m, _ := money.New(n, "EUR"); return m }
		_, err := f.call(func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
			return f.usd.Post(f.ctx, tx, tn, Posting{Key: "eur", Code: "topup", Entries: []Entry{
				{Account: "hot", Side: Debit, Amount: eur(100)}, {Account: "limited", Side: Credit, Amount: eur(100)}}})
		})
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "EUR") {
			t.Fatalf("Post in EUR on the USD ledger: %v; want ErrInvalid that names EUR", err)
		}
		// The trigger refuses an entry whose currency is not its account's.
		err = f.try(f.runtime, acme, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(f.ctx, `WITH t AS (INSERT INTO transfers (tenant_id, ledger_id, idempotency_key, kind, code, amount, currency, status, effective_at)
 VALUES ('acme', 'usd', 'raw-eur', 'transfer', 'topup', 100, 'EUR', 'posted', now()) RETURNING id)
 INSERT INTO entries (tenant_id, ledger_id, transfer_id, seq, account_id, side, amount, currency, pending, effective_at, posted_at)
 SELECT 'acme', 'usd', t.id, x.seq, x.account, x.side, 100, 'EUR', false, now(), now()
 FROM t, (VALUES (1, 'hot', 'debit'), (2, 'limited', 'credit')) AS x(seq, account, side)`)
			return err
		})
		if sqlState(err) != "23514" || !strings.Contains(err.Error(), "EUR") {
			t.Fatalf("a raw insert in EUR: %v; want the check violation of the trigger that names EUR", err)
		}
		if n := f.count("transfers", "true") + f.count("entries", "true"); n != 0 {
			t.Fatalf("%d transfer and entry rows; want none", n)
		}
	})

	t.Run("ST-SUP-RP08-LC03-T05 Only an account with a balance limit takes the row lock", func(t *testing.T) {
		f := newFixture(t)
		for _, c := range []struct {
			locked  string
			entries []Entry
			waits   bool
		}{
			{"limited", []Entry{dr("hot", 50), cr("limited", 50)}, true},
			{"hot", []Entry{dr("hot", 50), cr("plain", 50)}, false},
			{"plain", []Entry{dr("hot", 50), cr("plain", 50)}, false},
		} {
			holder, err := tenant.Begin(f.ctx, f.owner, f.tn(acme), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := holder.ExecContext(f.ctx, "SELECT 1 FROM accounts WHERE ledger_id = 'usd' AND id = $1 FOR UPDATE", c.locked); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := f.call(func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
					return f.usd.Post(f.ctx, tx, tn, Posting{Key: "lock-" + c.locked, Code: "topup", Entries: c.entries})
				})
				done <- err
			}()
			if c.waits {
				f.waitForLockWait(t)
				select {
				case err := <-done:
					t.Fatalf("the post on %s finished (%v) while its row was locked", c.locked, err)
				default:
				}
				if err := holder.Rollback(); err != nil {
					t.Fatal(err)
				}
				if err := <-done; err != nil {
					t.Fatalf("the post on %s after the lock: %v", c.locked, err)
				}
				continue
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("the post on %s: %v", c.locked, err)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("the post on %s waits for the row lock of the account", c.locked)
			}
			if err := holder.Rollback(); err != nil {
				t.Fatal(err)
			}
		}
		// The lock serves the limit: limited holds 50, a pending debit of 40 holds against it, and a post that
		// would take it below 0 is refused.
		f.pend("hold", dr("limited", 40), cr("plain", 40))
		if _, err := f.call(func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
			return f.usd.Post(f.ctx, tx, tn, Posting{Key: "over", Code: "purchase", Entries: []Entry{dr("limited", 11), cr("plain", 11)}})
		}); !errors.Is(err, ErrBelowMinimum) {
			t.Fatalf("a post below the minimum balance: %v; want ErrBelowMinimum", err)
		}
		f.post(f.runtime, "exact", dr("limited", 10), cr("plain", 10))
		// The hot and the plain account have no limit.
		f.post(f.runtime, "free", dr("plain", 1000), cr("hot", 1000))

		// 40 goroutines at once: 20 posts and 20 pends of 50 against the limited account, which holds 1000 more,
		// and 40 posts on the hot account. The lock serializes the limited account, so exactly 20 fit and the rest
		// are refused; the hot account takes every post.
		f.post(f.runtime, "fund-more", dr("hot", 1000), cr("limited", 1000))
		start := make(chan struct{})
		type result struct {
			pend bool
			err  error
		}
		limitedDone, hotDone := make(chan result, 40), make(chan error, 40)
		for i := range 40 {
			go func() {
				<-start
				_, err := f.call(func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
					p := Posting{Key: fmt.Sprintf("race-limited-%d", i), Code: "purchase", Entries: []Entry{dr("limited", 50), cr("plain", 50)}}
					if i%2 == 1 {
						return f.usd.Pend(f.ctx, tx, tn, p)
					}
					return f.usd.Post(f.ctx, tx, tn, p)
				})
				limitedDone <- result{pend: i%2 == 1, err: err}
			}()
			go func() {
				<-start
				_, err := f.call(func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
					return f.usd.Post(f.ctx, tx, tn, Posting{Key: fmt.Sprintf("race-hot-%d", i), Code: "topup", Entries: []Entry{dr("hot", 1), cr("plain", 1)}})
				})
				hotDone <- err
			}()
		}
		close(start)
		var posts, pends int64
		for range 40 {
			r := <-limitedDone
			switch {
			case r.err == nil && r.pend:
				pends++
			case r.err == nil:
				posts++
			case !errors.Is(r.err, ErrBelowMinimum):
				t.Fatalf("a racing transfer on the limited account: %v", r.err)
			}
			if err := <-hotDone; err != nil {
				t.Fatalf("a racing post on the hot account: %v", err)
			}
		}
		if posts+pends != 20 {
			t.Fatalf("%d posts and %d pends fit on the limited account; want 20 in all", posts, pends)
		}
		// limited had 40 posted and 40 held after the first part, so 0 available; now 1040 less 50 for each post,
		// with 50 held for each pend, and 0 available.
		if got := f.balance("limited"); got != usd(1040-50*posts) {
			t.Fatalf("the limited balance is %v; want %d USD", got, 1040-50*posts)
		}
		var available int64
		f.inTx(f.runtime, acme, func(tx *sql.Tx) error {
			var err error
			available, err = f.usd.available(f.ctx, tx, f.tn(acme), "limited")
			return err
		})
		if available != 0 {
			t.Fatalf("the limited account has %d available after the race; want 0", available)
		}
		// hot: 3 x 50 from the lock cases, -1000, +1000, and 40 x 1 from the race.
		if got := f.balance("hot"); got != usd(150-1000+1000+40) {
			t.Fatalf("the hot balance is %v; want 190 USD", got)
		}
	})
}

// waitForLockWait waits until a session of the test database waits for a lock.
func (f *fixture) waitForLockWait(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := f.admin.QueryRowContext(f.ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND wait_event_type = 'Lock'", f.db).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no session waits for the row lock")
}

func TestPending(t *testing.T) {
	t.Run("ST-SUP-RP08-LC04-T01 A pending transfer is not changed by a later call", func(t *testing.T) {
		f := newFixture(t)
		f.post(f.runtime, "fund", dr("hot", 1000), cr("limited", 1000))
		p := f.pend("hold", dr("limited", 300), cr("plain", 300))
		var status string
		withoutStatus := "SELECT (to_jsonb(r) - 'status')::text FROM transfers r WHERE id = $1"
		read := func() (string, string, string) {
			var row, entries string
			f.inTx(f.owner, acme, func(tx *sql.Tx) error {
				if err := tx.QueryRowContext(f.ctx, withoutStatus, p.ID).Scan(&row); err != nil {
					return err
				}
				return tx.QueryRowContext(f.ctx, "SELECT status FROM transfers WHERE id = $1", p.ID).Scan(&status)
			})
			entries = f.row("entries", "transfer_id = $1", p.ID)
			return row, entries, status
		}
		row1, entries1, status1 := read()
		if status1 != "pending" {
			t.Fatalf("the pending row has the status %s", status1)
		}
		if _, err := f.call(func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
			return f.usd.PostPending(f.ctx, tx, tn, p.ID, "post-1", usd(300))
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.call(func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
			return f.usd.PostPending(f.ctx, tx, tn, p.ID, "post-2", usd(300))
		}); !errors.Is(err, ErrAlreadyPosted) {
			t.Fatalf("a second post: %v; want ErrAlreadyPosted", err)
		}
		if _, err := f.call(func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
			return f.usd.Void(f.ctx, tx, tn, p.ID, "void-1")
		}); !errors.Is(err, ErrAlreadyPosted) {
			t.Fatalf("a void after the post: %v; want ErrAlreadyPosted", err)
		}
		row2, entries2, status2 := read()
		if row1 != row2 {
			t.Fatalf("the pending row changed:\n%s\n%s", row1, row2)
		}
		if entries1 != entries2 || entries1 == "" {
			t.Fatalf("the pending entries changed:\n%s\n%s", entries1, entries2)
		}
		if status2 != "posted" {
			t.Fatalf("the pending row has the status %s after the post; want posted", status2)
		}
	})

	t.Run("ST-SUP-RP08-LC04-T02 Posting a pending transfer writes a new row that names it", func(t *testing.T) {
		f := newFixture(t)
		f.post(f.runtime, "fund", dr("hot", 1000), cr("limited", 1000))
		p := f.pend("hold", dr("limited", 300), cr("plain", 300))
		before := f.count("transfers", "true")
		// The ID in upper case names the same transfer, and a retry with it returns the post.
		posted, err := f.call(func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
			return f.usd.PostPending(f.ctx, tx, tn, strings.ToUpper(p.ID), "post", usd(300))
		})
		if err != nil {
			t.Fatal(err)
		}
		again, err := f.call(func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
			return f.usd.PostPending(f.ctx, tx, tn, strings.ToUpper(p.ID), "post", usd(300))
		})
		if err != nil || again.ID != posted.ID || !again.Replayed || again.PendingID != p.ID {
			t.Fatalf("the retry with the upper-case ID: %+v, %v; want the post %s", again, err, posted.ID)
		}
		if posted.ID == p.ID || f.count("transfers", "true") != before+1 {
			t.Fatalf("the post %s is not a new row", posted.ID)
		}
		if n := f.count("transfers", "id = $1 AND pending_id = $2 AND kind = 'post' AND status = 'posted'", posted.ID, p.ID); n != 1 {
			t.Fatalf("no posted row names the pending transfer %s", p.ID)
		}
		if n := f.count("entries", "transfer_id = $1 AND NOT pending", posted.ID); n != 2 {
			t.Fatalf("the post has %d posted entries, not 2", n)
		}
		// A post of a part posts that part on each side, split by Allocate.
		p2 := f.pend("hold-2", dr("limited", 100), dr("cr", 200), cr("plain", 300))
		part, err := f.call(func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
			return f.usd.PostPending(f.ctx, tx, tn, p2.ID, "post-2", usd(100))
		})
		if err != nil {
			t.Fatal(err)
		}
		got, err := f.getTransfer(part.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := []Entry{dr("limited", 33), dr("cr", 67), cr("plain", 100)}
		if fmt.Sprint(got.Entries) != fmt.Sprint(want) || got.Amount != usd(100) {
			t.Fatalf("the part post is %v of %v; want %v of 100 USD", got.Entries, got.Amount, want)
		}
	})

	t.Run("ST-SUP-RP08-LC04-T03 Voiding a pending transfer writes a new row that names it", func(t *testing.T) {
		f := newFixture(t)
		f.post(f.runtime, "fund", dr("hot", 1000), cr("limited", 1000))
		p := f.pend("hold", dr("limited", 300), cr("plain", 300))
		voided, err := f.call(func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
			return f.usd.Void(f.ctx, tx, tn, p.ID, "void")
		})
		if err != nil {
			t.Fatal(err)
		}
		if n := f.count("transfers", "id = $1 AND pending_id = $2 AND kind = 'void' AND status = 'voided'", voided.ID, p.ID); n != 1 || voided.ID == p.ID {
			t.Fatalf("no new voided row names the pending transfer %s", p.ID)
		}
		if n := f.count("transfers", "id = $1 AND status = 'voided'", p.ID); n != 1 {
			t.Fatal("the pending transfer is not voided")
		}
		if n := f.count("entries", "transfer_id = $1", voided.ID); n != 0 {
			t.Fatalf("the void has %d entries", n)
		}
	})

	t.Run("ST-SUP-RP08-LC04-T04 A second post or void returns the state machine's error", func(t *testing.T) {
		f := newFixture(t)
		f.post(f.runtime, "fund", dr("hot", 10000), cr("limited", 10000))
		posted := f.pend("p1", dr("limited", 100), cr("plain", 100))
		if _, err := f.call(func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
			return f.usd.PostPending(f.ctx, tx, tn, posted.ID, "post-p1", usd(100))
		}); err != nil {
			t.Fatal(err)
		}
		voided := f.pend("p2", dr("limited", 100), cr("plain", 100))
		if _, err := f.call(func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
			return f.usd.Void(f.ctx, tx, tn, voided.ID, "void-p2")
		}); err != nil {
			t.Fatal(err)
		}
		expired := f.pend("p3", dr("limited", 100), cr("plain", 100))
		small := f.pend("p4", dr("limited", 100), cr("plain", 100))
		before := f.count("transfers", "true")
		check := func(name string, want error, fn func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error)) {
			t.Helper()
			if _, err := f.call(fn); !errors.Is(err, want) {
				t.Fatalf("%s: %v; want %v", name, err, want)
			}
		}
		check("post a posted one", ErrAlreadyPosted, func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
			return f.usd.PostPending(f.ctx, tx, tn, posted.ID, "again-1", usd(100))
		})
		check("void a posted one", ErrAlreadyPosted, func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
			return f.usd.Void(f.ctx, tx, tn, posted.ID, "again-2")
		})
		check("post a voided one", ErrAlreadyVoided, func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
			return f.usd.PostPending(f.ctx, tx, tn, voided.ID, "again-3", usd(100))
		})
		check("post more than the pending amount", ErrExceedsPending, func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
			return f.usd.PostPending(f.ctx, tx, tn, small.ID, "again-4", usd(101))
		})
		f.clock.Advance(15 * time.Minute)
		check("post an expired one", ErrExpired, func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
			return f.usd.PostPending(f.ctx, tx, tn, expired.ID, "again-5", usd(100))
		})
		if after := f.count("transfers", "true"); after != before {
			t.Fatalf("%d new rows", after-before)
		}
		// Two posts of one pending transfer at once: the second waits for the first, and the post function then
		// refuses it in the database with already_posted.
		f.clock.Advance(-15 * time.Minute)
		first, err := tenant.Begin(f.ctx, f.runtime, f.tn(acme), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.usd.PostPending(f.ctx, first, f.tn(acme), small.ID, "race-1", usd(100)); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := f.call(func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
				return f.usd.Void(f.ctx, tx, tn, small.ID, "race-2")
			})
			done <- err
		}()
		f.waitForLockWait(t)
		if err := tenant.Commit(f.ctx, first, f.tn(acme)); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, ErrAlreadyPosted) || sqlState(err) != codeAlreadyPosted {
			t.Fatalf("the void that raced the post: %v; want ErrAlreadyPosted from the post and void function", err)
		}
		if n := f.count("transfers", "pending_id = $1", small.ID); n != 1 {
			t.Fatalf("%d rows settle the pending transfer; want 1", n)
		}
	})

	t.Run("ST-SUP-RP08-LC04-T05 The state machine returns only its four errors and has no edit of a pending transfer", func(t *testing.T) {
		four := []error{ErrAlreadyPosted, ErrAlreadyVoided, ErrExpired, ErrExceedsPending}
		now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
		open, past := now.Add(time.Minute), now.Add(-time.Minute)
		type state struct {
			name    string
			status  Status
			expires time.Time
			amount  int64
		}
		cases := []struct {
			state
			post, void error
		}{
			{state{"pending", Pending, open, 100}, nil, nil},
			{state{"posted", Posted, open, 100}, ErrAlreadyPosted, ErrAlreadyPosted},
			{state{"voided", Voided, open, 100}, ErrAlreadyVoided, ErrAlreadyVoided},
			{state{"expired", Pending, past, 100}, ErrExpired, nil},
			{state{"expires now", Pending, now, 100}, ErrExpired, nil},
			{state{"above the pending amount", Pending, open, 101}, ErrExceedsPending, nil},
			{state{"posted and expired", Posted, past, 101}, ErrAlreadyPosted, ErrAlreadyPosted},
		}
		for _, c := range cases {
			for _, o := range []struct {
				op   op
				want error
			}{{opPost, c.post}, {opVoid, c.void}} {
				err := transition(o.op, c.status, now, c.expires, c.amount, 100)
				if !errors.Is(err, o.want) || (err == nil) != (o.want == nil) {
					t.Fatalf("%s from %s: %v; want %v", map[op]string{opPost: "post", opVoid: "void"}[o.op], c.name, err, o.want)
				}
				if err != nil && !slices.ContainsFunc(four, func(e error) bool { return errors.Is(err, e) }) {
					t.Fatalf("%s: %v is not one of the four errors", c.name, err)
				}
			}
		}
		for _, name := range exportedNames(t) {
			for _, word := range camelWords(name) {
				if slices.Contains([]string{"Edit", "Update", "Amend", "Change", "Modify", "Set", "Adjust", "Correct", "Patch", "Replace"}, word) {
					t.Fatalf("the package exports %s, which edits a written transfer", name)
				}
			}
		}
	})
}

func (f *fixture) getTransfer(id string) (Transfer, error) {
	var out Transfer
	err := f.try(f.runtime, acme, func(tx *sql.Tx) error {
		var err error
		out, err = f.usd.Get(f.ctx, tx, f.tn(acme), id)
		return err
	})
	return out, err
}

func TestIdempotentTransfer(t *testing.T) {
	t.Run("ST-SUP-RP08-LC05-T01 The transfer insert uses ON CONFLICT on ledger and key with DO NOTHING RETURNING", func(t *testing.T) {
		f := newFixture(t)
		mark := f.trace.mark()
		var p, posted Transfer
		f.inTx(f.traced, acme, func(tx *sql.Tx) error {
			var err error
			if posted, err = f.usd.Post(f.ctx, tx, f.tn(acme), Posting{Key: "k1", Code: "topup", Entries: []Entry{dr("hot", 500), cr("limited", 500)}}); err != nil {
				return err
			}
			if p, err = f.usd.Pend(f.ctx, tx, f.tn(acme), Posting{Key: "k2", Code: "purchase", Entries: []Entry{dr("limited", 100), cr("plain", 100)}}); err != nil {
				return err
			}
			if _, err = f.usd.PostPending(f.ctx, tx, f.tn(acme), p.ID, "k3", usd(100)); err != nil {
				return err
			}
			if posted, err = f.usd.Post(f.ctx, tx, f.tn(acme), Posting{Key: "k4", Code: "topup", Entries: []Entry{dr("hot", 50), cr("plain", 50)}}); err != nil {
				return err
			}
			_, err = f.usd.Reverse(f.ctx, tx, f.tn(acme), posted.ID, "k5")
			return err
		})
		var inserts int
		for _, q := range f.trace.since(mark) {
			if !strings.Contains(q, "INSERT INTO {s}.transfers") && !strings.Contains(q, "INSERT INTO public.transfers") {
				continue
			}
			inserts++
			if !strings.Contains(q, "ON CONFLICT (ledger_id, idempotency_key) DO NOTHING RETURNING") {
				t.Fatalf("the transfer insert does not hold ON CONFLICT (ledger_id, idempotency_key) DO NOTHING RETURNING:\n%s", q)
			}
		}
		if inserts != 5 {
			t.Fatalf("%d transfer inserts recorded; want 5 (two posts, pend, post of the pending, reverse)", inserts)
		}
		var def string
		f.inTx(f.owner, acme, func(tx *sql.Tx) error {
			return tx.QueryRowContext(f.ctx, `SELECT pg_get_constraintdef(c.oid) FROM pg_constraint c
 WHERE c.conrelid = 'transfers'::regclass AND c.contype = 'u'`).Scan(&def)
		})
		if def != "UNIQUE (ledger_id, idempotency_key)" {
			t.Fatalf("the unique constraint of transfers is %q", def)
		}
	})

	t.Run("ST-SUP-RP08-LC05-T02 A retried transfer returns the original and inserts no entry", func(t *testing.T) {
		f := newFixture(t)
		first := f.post(f.runtime, "k1", dr("hot", 500), cr("limited", 500))
		entries := f.count("entries", "true")
		again := f.post(f.runtime, "k1", dr("hot", 700), cr("plain", 700))
		if again.ID != first.ID || again.Amount != usd(500) || !again.Replayed || fmt.Sprint(again.Entries) != fmt.Sprint(first.Entries) {
			t.Fatalf("the retry returned %+v; want the original %+v", again, first)
		}
		if n := f.count("entries", "true"); n != entries {
			t.Fatalf("the retry inserted %d entries", n-entries)
		}
		p := f.pend("k2", dr("limited", 100), cr("plain", 100))
		entries = f.count("entries", "true")
		p2 := f.pend("k2", dr("limited", 50), cr("plain", 50))
		if p2.ID != p.ID || p2.Amount != usd(100) || !p2.Replayed {
			t.Fatalf("the retried pend returned %+v; want the original %+v", p2, p)
		}
		if n := f.count("entries", "true"); n != entries {
			t.Fatalf("the retried pend inserted %d entries", n-entries)
		}
		// A retry that runs while the first call is not committed waits for it and then returns its transfer.
		tx1, err := tenant.Begin(f.ctx, f.runtime, f.tn(acme), nil)
		if err != nil {
			t.Fatal(err)
		}
		racing, err := f.usd.Post(f.ctx, tx1, f.tn(acme), Posting{Key: "k5", Code: "topup", Entries: []Entry{dr("hot", 5), cr("plain", 5)}})
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan Transfer, 1)
		go func() { done <- f.post(f.runtime, "k5", dr("hot", 9), cr("plain", 9)) }()
		f.waitForLockWait(t)
		if err := tenant.Commit(f.ctx, tx1, f.tn(acme)); err != nil {
			t.Fatal(err)
		}
		if got := <-done; got.ID != racing.ID || got.Amount != usd(5) || !got.Replayed {
			t.Fatalf("the racing retry returned %+v; want the original %+v", got, racing)
		}
		// A key of a post that is used for a pend names another operation.
		if _, err := f.call(func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
			return f.usd.Pend(f.ctx, tx, tn, Posting{Key: "k1", Code: "topup", Entries: []Entry{dr("limited", 1), cr("plain", 1)}})
		}); !errors.Is(err, ErrKeyReused) {
			t.Fatalf("a pend with the key of a post: %v; want ErrKeyReused", err)
		}
		// The key of one tenant does not meet the transfer of another tenant.
		var others Transfer
		f.inTx(f.runtime, other, func(tx *sql.Tx) error {
			var err error
			others, err = f.usd.Post(f.ctx, tx, f.tn(other), Posting{Key: "k1", Code: "topup", Entries: []Entry{dr("hot", 3), cr("limited", 3)}})
			return err
		})
		if others.ID == first.ID || others.Replayed || others.Amount != usd(3) {
			t.Fatalf("the other tenant's post with k1 returned %+v", others)
		}
	})
}

func TestReverse(t *testing.T) {
	t.Run("ST-SUP-RP08-LC06-T01 A reversal is a new transfer with opposite entries that names the original", func(t *testing.T) {
		f := newFixture(t)
		o := f.post(f.runtime, "orig", dr("hot", 500), cr("limited", 300), cr("plain", 200))
		r, err := f.call(func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
			return f.usd.Reverse(f.ctx, tx, tn, o.ID, "rev")
		})
		if err != nil {
			t.Fatal(err)
		}
		got, err := f.getTransfer(r.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := []Entry{cr("hot", 500), dr("limited", 300), dr("plain", 200)}
		if r.ID == o.ID || got.Kind != Reversal || got.Reverses != o.ID || fmt.Sprint(got.Entries) != fmt.Sprint(want) || got.Amount != usd(500) {
			t.Fatalf("the reversal is %+v; want entries %v that reverse %s", got, want, o.ID)
		}
		orig, err := f.getTransfer(o.ID)
		if err != nil {
			t.Fatal(err)
		}
		if orig.ReversedBy != r.ID {
			t.Fatalf("reversed_by of the original is %q; want %s", orig.ReversedBy, r.ID)
		}
		// The balances are back where they were.
		f.inTx(f.runtime, acme, func(tx *sql.Tx) error {
			for _, a := range []string{"hot", "limited", "plain"} {
				b, err := f.usd.Balance(f.ctx, tx, f.tn(acme), a)
				if err != nil {
					return err
				}
				if b != usd(0) {
					t.Errorf("the balance of %s after the reversal is %v", a, b)
				}
			}
			return nil
		})
	})

	t.Run("ST-SUP-RP08-LC06-T02 A second reversal of one transfer is refused", func(t *testing.T) {
		f := newFixture(t)
		o := f.post(f.runtime, "orig", dr("hot", 500), cr("plain", 500))
		if _, err := f.call(func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
			return f.usd.Reverse(f.ctx, tx, tn, o.ID, "rev-1")
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.call(func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
			return f.usd.Reverse(f.ctx, tx, tn, o.ID, "rev-2")
		}); !errors.Is(err, ErrAlreadyReversed) {
			t.Fatalf("a second reversal: %v; want ErrAlreadyReversed", err)
		}
		// Two reversals at once: the second waits, and the reverse function refuses it in the database.
		o2 := f.post(f.runtime, "orig-2", dr("hot", 5), cr("plain", 5))
		first, err := tenant.Begin(f.ctx, f.runtime, f.tn(acme), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.usd.Reverse(f.ctx, first, f.tn(acme), o2.ID, "race-1"); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := f.call(func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
				return f.usd.Reverse(f.ctx, tx, tn, o2.ID, "race-2")
			})
			done <- err
		}()
		f.waitForLockWait(t)
		if err := tenant.Commit(f.ctx, first, f.tn(acme)); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, ErrAlreadyReversed) || sqlState(err) != codeAlreadyReversed {
			t.Fatalf("the racing reversal: %v; want ErrAlreadyReversed from the reverse function", err)
		}
		if n := f.count("transfers", "kind = 'reversal'"); n != 2 {
			t.Fatalf("%d reversal rows; want one for each original", n)
		}
	})

	t.Run("ST-SUP-RP08-LC06-T03 A reversal of a transfer that is not posted is refused", func(t *testing.T) {
		f := newFixture(t)
		f.post(f.runtime, "fund", dr("hot", 1000), cr("limited", 1000))
		pending := f.pend("p1", dr("limited", 100), cr("plain", 100))
		toVoid := f.pend("p2", dr("limited", 100), cr("plain", 100))
		void, err := f.call(func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
			return f.usd.Void(f.ctx, tx, tn, toVoid.ID, "void")
		})
		if err != nil {
			t.Fatal(err)
		}
		before := f.count("transfers", "true")
		for _, id := range []string{pending.ID, toVoid.ID, void.ID} {
			if _, err := f.call(func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
				return f.usd.Reverse(f.ctx, tx, tn, id, "rev-"+id)
			}); !errors.Is(err, ErrNotPosted) {
				t.Fatalf("the reversal of %s: %v; want ErrNotPosted", id, err)
			}
		}
		// A pending transfer that was posted is reversed through its post row, not itself.
		posted := f.pend("p3", dr("limited", 100), cr("plain", 100))
		if _, err := f.call(func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
			return f.usd.PostPending(f.ctx, tx, tn, posted.ID, "post-p3", usd(100))
		}); err != nil {
			t.Fatal(err)
		}
		before = f.count("transfers", "true")
		if _, err := f.call(func(tx *sql.Tx, tn tenant.Tenant) (Transfer, error) {
			return f.usd.Reverse(f.ctx, tx, tn, posted.ID, "rev-p3")
		}); !errors.Is(err, ErrNotPosted) {
			t.Fatalf("the reversal of a posted pending row: %v; want ErrNotPosted", err)
		}
		// The reverse function refuses a pending transfer also when it is called directly.
		for _, id := range []string{pending.ID, posted.ID} {
			err = f.try(f.runtime, acme, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(f.ctx, "SELECT ledger_reverse($1::uuid, gen_random_uuid())", id)
				return err
			})
			if sqlState(err) != codeNotPosted {
				t.Fatalf("ledger_reverse of the pending row %s: %v; want %s", id, err, codeNotPosted)
			}
		}
		// The reverse function refuses a pending transfer also when it is called directly.
		err = f.try(f.runtime, acme, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(f.ctx, "SELECT ledger_reverse($1::uuid, gen_random_uuid())", pending.ID)
			return err
		})
		if sqlState(err) != codeNotPosted {
			t.Fatalf("ledger_reverse of a pending transfer: %v; want %s", err, codeNotPosted)
		}
		if after := f.count("transfers", "true"); after != before {
			t.Fatalf("%d new rows", after-before)
		}
	})

	t.Run("ST-SUP-RP08-LC06-T04 The ledger package has no difference adjustment", func(t *testing.T) {
		for _, name := range exportedNames(t) {
			if strings.Contains(name, "Adjust") || strings.Contains(name, "Correct") {
				t.Fatalf("the package exports %s", name)
			}
		}
		writers := insertingExports(t)
		allowed := []string{"Ledger.Post", "Ledger.Pend", "Ledger.PostPending", "Ledger.Void", "Ledger.Reverse"}
		for _, w := range writers {
			if !slices.Contains(allowed, w) {
				t.Fatalf("the exported %s inserts transfers or entries; only %v may", w, allowed)
			}
		}
		if len(writers) != len(allowed) {
			t.Fatalf("the exported functions that insert transfers or entries are %v; want %v", writers, allowed)
		}
	})
}

// camelWords splits an identifier such as Ledger.PostPending into its words: Ledger, Post, Pending.
func camelWords(name string) []string {
	var out []string
	for _, part := range strings.Split(name, ".") {
		start := 0
		for i := 1; i < len(part); i++ {
			if part[i] >= 'A' && part[i] <= 'Z' && part[i-1] >= 'a' && part[i-1] <= 'z' {
				out = append(out, part[start:i])
				start = i
			}
		}
		out = append(out, part[start:])
	}
	return out
}

// typeCheck type-checks the package source files that are not tests, with go/types, and returns the package, its
// files and the uses of each identifier.
func typeCheck(t *testing.T) (*types.Package, []*ast.File, *types.Info) {
	t.Helper()
	fset := token.NewFileSet()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{}}
	config := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	pkg, err := config.Check("stego.dev/runtime/ledger", fset, files, info)
	if err != nil {
		t.Fatalf("type-check the package: %v", err)
	}
	return pkg, files, info
}

// exportedNames lists every exported name of the package: package-level names, and Type.Method and Type.Field.
func exportedNames(t *testing.T) []string {
	t.Helper()
	pkg, _, _ := typeCheck(t)
	var out []string
	for _, name := range pkg.Scope().Names() {
		obj := pkg.Scope().Lookup(name)
		if !obj.Exported() {
			continue
		}
		out = append(out, name)
		tn, ok := obj.(*types.TypeName)
		if !ok {
			continue
		}
		named, ok := tn.Type().(*types.Named)
		if !ok {
			continue
		}
		for i := range named.NumMethods() {
			if m := named.Method(i); m.Exported() {
				out = append(out, name+"."+m.Name())
			}
		}
		if st, ok := named.Underlying().(*types.Struct); ok {
			for i := range st.NumFields() {
				if fl := st.Field(i); fl.Exported() {
					out = append(out, name+"."+fl.Name())
				}
			}
		}
	}
	return out
}

// insertingExports returns the exported functions and methods that reach, through the calls of the package, a
// statement constant that inserts into transfers or entries.
func insertingExports(t *testing.T) []string {
	t.Helper()
	pkg, files, info := typeCheck(t)
	inserts := func(obj types.Object) bool {
		c, ok := obj.(*types.Const)
		if !ok || c.Pkg() != pkg || c.Val().Kind() != constant.String {
			return false
		}
		s := constant.StringVal(c.Val())
		return strings.Contains(s, "INSERT INTO {s}.transfers") || strings.Contains(s, "INSERT INTO {s}.entries")
	}
	name := func(fn *types.Func) string {
		sig := fn.Type().(*types.Signature)
		if sig.Recv() == nil {
			return fn.Name()
		}
		recv := sig.Recv().Type()
		if p, ok := recv.(*types.Pointer); ok {
			recv = p.Elem()
		}
		return recv.(*types.Named).Obj().Name() + "." + fn.Name()
	}
	calls := map[*types.Func][]*types.Func{}
	direct := map[*types.Func]bool{}
	for _, file := range files {
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			fn := info.Defs[fd.Name].(*types.Func)
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				switch obj := info.Uses[id].(type) {
				case *types.Func:
					if obj.Pkg() == pkg {
						calls[fn] = append(calls[fn], obj.Origin())
					}
				case *types.Const:
					if inserts(obj) {
						direct[fn] = true
					}
				}
				return true
			})
		}
	}
	reaches := map[*types.Func]bool{}
	var visit func(fn *types.Func, seen map[*types.Func]bool) bool
	visit = func(fn *types.Func, seen map[*types.Func]bool) bool {
		if direct[fn] {
			return true
		}
		if seen[fn] {
			return false
		}
		seen[fn] = true
		for _, c := range calls[fn] {
			if visit(c, seen) {
				return true
			}
		}
		return false
	}
	var out []string
	for fn := range calls {
		reaches[fn] = visit(fn, map[*types.Func]bool{})
	}
	for fn := range direct {
		reaches[fn] = true
	}
	for fn, ok := range reaches {
		if ok && fn.Exported() {
			out = append(out, name(fn))
		}
	}
	slices.Sort(out)
	return out
}
