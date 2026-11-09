package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"stego.dev/runtime/idempotency"
	"stego.dev/runtime/money"
	"stego.dev/runtime/tenant"
)

// The errors of the two-phase state machine (ST-SUP-RP08-LC04). PostPending and Void return no other error of the
// state machine; a change to a pending transfer is a void and a new pend.
var (
	// ErrAlreadyPosted is the error of a post or a void of a pending transfer that is posted.
	ErrAlreadyPosted = errors.New("ledger: already_posted: the pending transfer is posted already")
	// ErrAlreadyVoided is the error of a post or a void of a pending transfer that is voided.
	ErrAlreadyVoided = errors.New("ledger: already_voided: the pending transfer is voided already")
	// ErrExpired is the error of a post of a pending transfer after its expiry.
	ErrExpired = errors.New("ledger: expired: the pending transfer is expired")
	// ErrExceedsPending is the error of a post of an amount larger than the pending amount.
	ErrExceedsPending = errors.New("ledger: exceeds_pending: the amount is larger than the pending amount")
)

// The other errors of the package.
var (
	// ErrInvalid is the error of a transfer that cannot be written: fewer than two entries, debits that differ
	// from the credits, an entry in another currency than the ledger's, an amount that is not positive, or text
	// that is not valid UTF-8 or holds NUL.
	ErrInvalid = errors.New("ledger: the transfer is not valid")
	// ErrBelowMinimum is the error of a transfer that would take an account with a balance limit below it.
	ErrBelowMinimum = errors.New("ledger: the account would go below its minimum balance")
	// ErrNotFound is the error for a transfer or an account that the tenant's ledger does not hold.
	ErrNotFound = errors.New("ledger: not found")
	// ErrNotPosted is the error of a reversal of a transfer that is not posted, such as a pending or voided one.
	ErrNotPosted = errors.New("ledger: the transfer is not posted")
	// ErrAlreadyReversed is the error of a second reversal of one transfer.
	ErrAlreadyReversed = errors.New("ledger: the transfer is reversed already")
	// ErrKeyReused is the error of a call whose idempotency key names a transfer of another kind or of another
	// pending or reversed transfer.
	ErrKeyReused = errors.New("ledger: the idempotency key names another operation")
)

// Side is the side of an entry, and the normal side of an account.
type Side string

// The two sides.
const (
	Debit  Side = "debit"
	Credit Side = "credit"
)

// Status is the status of a transfer. Only a pending transfer changes its status, once, from pending.
type Status string

// The statuses.
const (
	Pending Status = "pending"
	Posted  Status = "posted"
	Voided  Status = "voided"
)

// Kind is what a transfer row is.
type Kind string

// The kinds of transfer row.
const (
	// Single is a transfer that Post wrote.
	Single Kind = "transfer"
	// Hold is a pending transfer that Pend wrote.
	Hold Kind = "pending"
	// Settle is the row that posts a pending transfer, which PostPending wrote.
	Settle Kind = "post"
	// Release is the row that voids a pending transfer, which Void wrote. It has no entries.
	Release Kind = "void"
	// Reversal is the transfer that reverses a posted one, which Reverse wrote.
	Reversal Kind = "reversal"
)

// Tx is the part of *sql.Tx that the ledger uses (ST-STD-RUNTIME-R06). The caller opens the transaction with
// tenant.Begin and commits it with tenant.Commit; the ledger never commits.
type Tx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Settings are the declared settings of one ledger.
type Settings struct {
	// ID is the ledger_id of the ledger's rows.
	ID string
	// Currency is the ISO 4217 code of the ledger's one currency.
	Currency string
	// Schema holds the ledger tables; the empty schema is public.
	Schema string
	// PendingTimeout is the time a pending transfer stays open: Pend sets its expiry this long after now. Zero
	// declares no two-phase transfers, and Pend refuses then.
	PendingTimeout time.Duration
	// SnapshotEvery is the number of entries of an account after which the snapshot job writes a snapshot.
	SnapshotEvery int
	// Now is the clock of the pending expiry; nil is time.Now.
	Now func() time.Time
}

// Ledger is one declared ledger: its ID, its one currency, and the statements over the shared tables.
type Ledger struct {
	id       string
	currency money.Currency
	schema   string
	timeout  time.Duration
	every    int64
	now      func() time.Time
}

// New returns the ledger of the settings.
func New(s Settings) (*Ledger, error) {
	if s.Schema == "" {
		s.Schema = "public"
	}
	if !plainName.MatchString(s.Schema) {
		return nil, errors.New("ledger: the schema must be a plain lower-case name")
	}
	if err := clean("ledger ID", s.ID); err != nil {
		return nil, err
	}
	c, err := money.Parse(s.Currency)
	if err != nil {
		return nil, fmt.Errorf("ledger %s: %w", s.ID, err)
	}
	if s.PendingTimeout < 0 || s.SnapshotEvery < 0 {
		return nil, fmt.Errorf("ledger %s: the pending timeout and the snapshot interval must not be negative", s.ID)
	}
	if s.Now == nil {
		s.Now = time.Now
	}
	return &Ledger{id: s.ID, currency: c, schema: s.Schema, timeout: s.PendingTimeout, every: int64(s.SnapshotEvery), now: s.Now}, nil
}

// ID is the ledger_id.
func (l *Ledger) ID() string { return l.id }

// Currency is the ledger's one currency.
func (l *Ledger) Currency() money.Currency { return l.currency }

// q puts the schema into a statement.
func (l *Ledger) q(statement string) string { return strings.ReplaceAll(statement, "{s}", l.schema) }

// Account is one account of a ledger.
type Account struct {
	// ID is the account's ID in the ledger.
	ID string
	// Code is the declared account it is one of, such as customer_wallet.
	Code string
	// Normal is the side on which the balance grows.
	Normal Side
	// MinBalance is the declared balance limit, or nil for an account with no limit. A post on an account with a
	// limit takes the account's row lock.
	MinBalance *int64
	// Hot marks an account with many concurrent posts; it has no limit and no post locks it.
	Hot bool
}

const insertAccount = `INSERT INTO {s}.accounts (tenant_id, ledger_id, id, code, normal, currency, min_balance, hot)
 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

// Open inserts the account into the tenant's ledger, in the ledger's currency.
func (l *Ledger) Open(ctx context.Context, tx Tx, t tenant.Tenant, a Account) error {
	if t.IsZero() {
		return tenant.ErrZero
	}
	if err := clean("account ID", a.ID); err != nil {
		return err
	}
	if err := clean("account code", a.Code); err != nil {
		return err
	}
	if a.Normal != Debit && a.Normal != Credit {
		return fmt.Errorf("%w: the normal side %q is not debit or credit", ErrInvalid, a.Normal)
	}
	if a.Hot && a.MinBalance != nil {
		return fmt.Errorf("%w: the hot account %s has a balance limit", ErrInvalid, a.ID)
	}
	var limit sql.NullInt64
	if a.MinBalance != nil {
		limit = sql.NullInt64{Int64: *a.MinBalance, Valid: true}
	}
	if _, err := tx.ExecContext(ctx, l.q(insertAccount), t.ID(), l.id, a.ID, a.Code, string(a.Normal), l.currency.Code(), limit, a.Hot); err != nil {
		return fmt.Errorf("ledger %s: open the account %s: %w", l.id, a.ID, err)
	}
	return nil
}

// Entry is one debit or credit of a transfer.
type Entry struct {
	Account string
	Side    Side
	Amount  money.Money
}

// Posting is what Post and Pend write.
type Posting struct {
	// Key is the caller's idempotency key: a retry with the same key returns the transfer of the first call. The
	// stored key is derived from it with the tenant, so keys of two tenants never meet (ST-SUP-RP07-LC06).
	Key string
	// Code is the declared transfer code, such as topup.
	Code string
	// Entries are the debits and credits; at least two, and the debits equal the credits.
	Entries []Entry
	// EffectiveAt is when the transfer takes effect for a balance at a time; zero is the time of posting.
	EffectiveAt time.Time
}

// Transfer is one transfer row and its entries.
type Transfer struct {
	ID          string
	Kind        Kind
	Code        string
	Amount      money.Money
	Status      Status
	PendingID   string // the pending transfer that a post or void row settles
	Reverses    string // the transfer that a reversal reverses
	ReversedBy  string // the reversal of a reversed transfer
	ExpiresAt   time.Time
	EffectiveAt time.Time
	PostedAt    time.Time
	Entries     []Entry
	// Replayed is set when the call found the transfer of an earlier call with the same key and wrote nothing.
	Replayed bool
}

// insertTransfer inserts one transfer and its entries in one statement (ST-SUP-RP08-LC03). A retry with a key
// already used inserts nothing (ST-SUP-RP08-LC05); the entry insert then has no transfer row to join. The
// statement triggers of the DDL text refuse the whole statement when the entries do not balance. {after} is the
// call of an owner function that settles a pending transfer or marks a reversed one in the same statement, or
// nothing; it runs only when the transfer row is inserted.
const insertTransfer = `WITH t AS (
 INSERT INTO {s}.transfers (tenant_id, ledger_id, idempotency_key, kind, code, amount, currency, status, pending_id, reverses, expires_at, effective_at)
 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::uuid, $10::uuid, $11::timestamptz, coalesce($12::timestamptz, now()))
 ON CONFLICT (ledger_id, idempotency_key) DO NOTHING RETURNING id, effective_at, posted_at
), e AS (
 INSERT INTO {s}.entries (tenant_id, ledger_id, transfer_id, seq, account_id, side, amount, currency, pending, effective_at, posted_at)
 SELECT $1, $2, t.id, x.seq, x.account, x.side, x.amount, $7, $4 = 'pending', t.effective_at, t.posted_at
 FROM t, ROWS FROM (jsonb_to_recordset($13::jsonb) AS (account text, side text, amount bigint)) WITH ORDINALITY AS x(account, side, amount, seq)
 RETURNING 1
), f AS ({after})
SELECT t.id::text, t.effective_at, t.posted_at, (SELECT count(*) FROM e), (SELECT count(*) FROM f) FROM t`

const (
	afterNothing = `SELECT $14::text WHERE false`
	afterSettle  = `SELECT {s}.ledger_settle($9::uuid, $14::text) FROM t`
	afterReverse = `SELECT {s}.ledger_reverse($10::uuid, t.id), $14::text FROM t`
)

const selectByKey = `SELECT id::text FROM {s}.transfers WHERE tenant_id = $1 AND ledger_id = $2 AND idempotency_key = $3`

const selectTransfer = `SELECT id::text, kind, code, amount, currency, status, coalesce(pending_id::text, ''),
 coalesce(reverses::text, ''), coalesce(reversed_by::text, ''), expires_at, effective_at, posted_at
 FROM {s}.transfers WHERE tenant_id = $1 AND ledger_id = $2 AND id = $3::uuid`

const selectEntries = `SELECT account_id, side, amount FROM {s}.entries
 WHERE tenant_id = $1 AND ledger_id = $2 AND transfer_id = $3::uuid ORDER BY seq`

// lockAccounts reads the accounts of a transfer and takes the row lock of each account with a balance limit, and of
// no other account (ST-SUP-RP08-LC03).
const lockAccounts = `SELECT id, normal, currency, min_balance, hot FROM {s}.ledger_lock($1, $2::jsonb)`

// write is one transfer row to insert.
type write struct {
	key       string
	kind      Kind
	code      string
	amount    int64
	status    Status
	pendingID string
	reverses  string
	expires   time.Time
	effective time.Time
	entries   []Entry
	after     string
	settleTo  Status
}

// Post inserts a transfer and its entries in one statement (ST-SUP-RP08-LC03). It refuses fewer than two entries,
// debits that differ from the credits and an entry in another currency than the ledger's, before any statement.
// It takes the row lock of each account with a balance limit, and of no other account, and refuses a transfer
// that would take such an account below its limit. A retry with a key already used returns the original transfer
// and inserts nothing (ST-SUP-RP08-LC05). Post does not commit.
func (l *Ledger) Post(ctx context.Context, tx Tx, t tenant.Tenant, p Posting) (Transfer, error) {
	return l.post(ctx, tx, t, p, Single)
}

// Pend inserts a pending transfer and its entries in one statement, as Post does, with an expiry PendingTimeout
// after now. No later call changes its amount or its entries: PostPending and Void write new rows that name it
// (ST-SUP-RP08-LC04). The entries of a pending transfer count in no balance; a pending debit on an account with a
// balance limit holds that amount against the limit until it is posted or voided.
func (l *Ledger) Pend(ctx context.Context, tx Tx, t tenant.Tenant, p Posting) (Transfer, error) {
	if l.timeout <= 0 {
		return Transfer{}, fmt.Errorf("%w: the ledger %s declares no two-phase transfers", ErrInvalid, l.id)
	}
	return l.post(ctx, tx, t, p, Hold)
}

func (l *Ledger) post(ctx context.Context, tx Tx, t tenant.Tenant, p Posting, kind Kind) (Transfer, error) {
	if t.IsZero() {
		return Transfer{}, tenant.ErrZero
	}
	if err := clean("transfer code", p.Code); err != nil {
		return Transfer{}, err
	}
	amount, err := l.balanced(p.Entries)
	if err != nil {
		return Transfer{}, err
	}
	key, err := deriveKey(t, l.id, p.Key)
	if err != nil {
		return Transfer{}, err
	}
	if found, ok, err := l.byKey(ctx, tx, t, key); err != nil || ok {
		return replay(found, err, func(f Transfer) bool { return f.Kind == kind })
	}
	if err := l.limit(ctx, tx, t, p.Entries, kind == Hold); err != nil {
		return Transfer{}, err
	}
	w := write{key: key, kind: kind, code: p.Code, amount: amount, status: Posted, effective: p.EffectiveAt, entries: p.Entries, after: afterNothing}
	if kind == Hold {
		w.status = Pending
		w.expires = l.now().Add(l.timeout)
	}
	return l.insert(ctx, tx, t, w)
}

// balanced checks the entries of a transfer and returns its amount, the sum of the debits.
func (l *Ledger) balanced(entries []Entry) (int64, error) {
	if len(entries) < 2 {
		return 0, fmt.Errorf("%w: a transfer has at least two entries, not %d", ErrInvalid, len(entries))
	}
	zero, _ := money.Of(0, l.currency)
	debits, credits := zero, zero
	for i, e := range entries {
		if e.Amount.Currency() != l.currency {
			return 0, fmt.Errorf("%w: the entry %d is in %q, and the ledger %s holds only %s", ErrInvalid, i, e.Amount.Currency().Code(), l.id, l.currency)
		}
		if e.Amount.Amount() <= 0 {
			return 0, fmt.Errorf("%w: the entry %d has the amount %d; an entry amount is positive", ErrInvalid, i, e.Amount.Amount())
		}
		if err := clean("account ID", e.Account); err != nil {
			return 0, err
		}
		var err error
		switch e.Side {
		case Debit:
			debits, err = debits.Add(e.Amount)
		case Credit:
			credits, err = credits.Add(e.Amount)
		default:
			return 0, fmt.Errorf("%w: the entry %d has the side %q", ErrInvalid, i, e.Side)
		}
		if err != nil {
			return 0, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
	}
	if debits != credits {
		return 0, fmt.Errorf("%w: the debits %d differ from the credits %d", ErrInvalid, debits.Amount(), credits.Amount())
	}
	return debits.Amount(), nil
}

// limit takes the row lock of each account of the entries that has a balance limit, and refuses the entries when
// such an account would go below it. Each balance counts the posted entries, the pending debits that hold against
// the limit, and the entries of this transfer; the entries of a pending transfer count only where they lower the
// balance. It refuses an account that the ledger does not hold.
func (l *Ledger) limit(ctx context.Context, tx Tx, t tenant.Tenant, entries []Entry, pending bool) error {
	ids := make([]string, 0, len(entries))
	seen := map[string]bool{}
	for _, e := range entries {
		if !seen[e.Account] {
			seen[e.Account] = true
			ids = append(ids, e.Account)
		}
	}
	list, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, l.q(lockAccounts), l.id, string(list))
	if err != nil {
		return fmt.Errorf("ledger %s: lock the accounts: %w", l.id, err)
	}
	type account struct {
		normal Side
		min    sql.NullInt64
	}
	accounts := map[string]account{}
	for rows.Next() {
		var id, normal, currency string
		var a account
		var hot bool
		if err := rows.Scan(&id, &normal, &currency, &a.min, &hot); err != nil {
			rows.Close()
			return err
		}
		if currency != l.currency.Code() {
			rows.Close()
			return fmt.Errorf("%w: the account %s is in %s, not %s", ErrInvalid, id, currency, l.currency)
		}
		a.normal = Side(normal)
		accounts[id] = a
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		a, ok := accounts[id]
		if !ok {
			return fmt.Errorf("%w: the account %s of the ledger %s", ErrNotFound, id, l.id)
		}
		if !a.min.Valid {
			continue
		}
		available, err := l.available(ctx, tx, t, id)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.Account != id {
				continue
			}
			delta := normal(e.Side, a.normal, e.Amount.Amount())
			if pending && delta > 0 {
				continue
			}
			if available, err = add(available, delta); err != nil {
				return err
			}
		}
		if available < a.min.Int64 {
			return fmt.Errorf("%w: the account %s would have %d, below its minimum %d", ErrBelowMinimum, id, available, a.min.Int64)
		}
	}
	return nil
}

// insert writes the transfer row and its entries in one statement, or finds the transfer of the same key.
func (l *Ledger) insert(ctx context.Context, tx Tx, t tenant.Tenant, w write) (Transfer, error) {
	type item struct {
		Account string `json:"account"`
		Side    string `json:"side"`
		Amount  int64  `json:"amount"`
	}
	items := make([]item, len(w.entries))
	for i, e := range w.entries {
		items[i] = item{Account: e.Account, Side: string(e.Side), Amount: e.Amount.Amount()}
	}
	list, err := json.Marshal(items)
	if err != nil {
		return Transfer{}, err
	}
	statement := l.q(strings.Replace(insertTransfer, "{after}", w.after, 1))
	var id string
	var effective, posted time.Time
	var nEntries, nAfter int64
	err = tx.QueryRowContext(ctx, statement, t.ID(), l.id, w.key, string(w.kind), w.code, w.amount, l.currency.Code(), string(w.status),
		null(w.pendingID), null(w.reverses), nullTime(w.expires), nullTime(w.effective), string(list), string(w.settleTo)).
		Scan(&id, &effective, &posted, &nEntries, &nAfter)
	if errors.Is(err, sql.ErrNoRows) {
		// Another call with the same key inserted the transfer first.
		found, ok, err := l.byKey(ctx, tx, t, w.key)
		if err == nil && !ok {
			err = fmt.Errorf("ledger %s: the key conflicts, and the tenant holds no transfer with it", l.id)
		}
		return replay(found, err, func(f Transfer) bool {
			return f.Kind == w.kind && f.PendingID == w.pendingID && f.Reverses == w.reverses
		})
	}
	if err != nil {
		return Transfer{}, stateError(fmt.Errorf("ledger %s: insert the transfer: %w", l.id, err))
	}
	if nEntries != int64(len(w.entries)) {
		return Transfer{}, fmt.Errorf("ledger %s: the statement inserted %d entries, not %d", l.id, nEntries, len(w.entries))
	}
	amount, _ := money.Of(w.amount, l.currency)
	out := Transfer{ID: id, Kind: w.kind, Code: w.code, Amount: amount, Status: w.status, PendingID: w.pendingID, Reverses: w.reverses,
		ExpiresAt: w.expires, EffectiveAt: effective, PostedAt: posted, Entries: append([]Entry(nil), w.entries...)}
	return out, nil
}

// replay returns the transfer of an earlier call with the same key, or ErrKeyReused when that transfer is of
// another operation.
func replay(found Transfer, err error, same func(Transfer) bool) (Transfer, error) {
	if err != nil {
		return Transfer{}, err
	}
	if !same(found) {
		return Transfer{}, fmt.Errorf("%w: the key names the transfer %s of kind %s", ErrKeyReused, found.ID, found.Kind)
	}
	found.Replayed = true
	return found, nil
}

// byKey reads the transfer with the stored key, and reports whether there is one.
func (l *Ledger) byKey(ctx context.Context, tx Tx, t tenant.Tenant, key string) (Transfer, bool, error) {
	var id string
	switch err := tx.QueryRowContext(ctx, l.q(selectByKey), t.ID(), l.id, key).Scan(&id); {
	case errors.Is(err, sql.ErrNoRows):
		return Transfer{}, false, nil
	case err != nil:
		return Transfer{}, false, fmt.Errorf("ledger %s: read the transfer of the key: %w", l.id, err)
	}
	found, err := l.Get(ctx, tx, t, id)
	return found, err == nil, err
}

// Get reads the transfer with the ID and its entries from the tenant's ledger.
func (l *Ledger) Get(ctx context.Context, tx Tx, t tenant.Tenant, id string) (Transfer, error) {
	if t.IsZero() {
		return Transfer{}, tenant.ErrZero
	}
	if !isUUID(id) {
		return Transfer{}, fmt.Errorf("%w: the transfer %q", ErrNotFound, id)
	}
	var out Transfer
	var kind, status, currency string
	var amount int64
	var expires sql.NullTime
	err := tx.QueryRowContext(ctx, l.q(selectTransfer), t.ID(), l.id, id).Scan(&out.ID, &kind, &out.Code, &amount, &currency, &status,
		&out.PendingID, &out.Reverses, &out.ReversedBy, &expires, &out.EffectiveAt, &out.PostedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Transfer{}, fmt.Errorf("%w: the transfer %s of the ledger %s", ErrNotFound, id, l.id)
	}
	if err != nil {
		return Transfer{}, fmt.Errorf("ledger %s: read the transfer %s: %w", l.id, id, err)
	}
	out.Kind, out.Status, out.ExpiresAt = Kind(kind), Status(status), expires.Time
	if out.Amount, err = money.New(amount, currency); err != nil {
		return Transfer{}, err
	}
	rows, err := tx.QueryContext(ctx, l.q(selectEntries), t.ID(), l.id, id)
	if err != nil {
		return Transfer{}, fmt.Errorf("ledger %s: read the entries of %s: %w", l.id, id, err)
	}
	defer rows.Close()
	for rows.Next() {
		var e Entry
		var side string
		var n int64
		if err := rows.Scan(&e.Account, &side, &n); err != nil {
			return Transfer{}, err
		}
		e.Side = Side(side)
		if e.Amount, err = money.New(n, currency); err != nil {
			return Transfer{}, err
		}
		out.Entries = append(out.Entries, e)
	}
	return out, rows.Err()
}

// op is a call of the state machine on a pending transfer.
type op int

const (
	opPost op = iota
	opVoid
)

// transition is the two-phase state machine (ST-SUP-RP08-LC04): it answers whether the post or void of a pending
// transfer in the status, with the expiry, may happen now, for the amount. Its only errors are ErrAlreadyPosted,
// ErrAlreadyVoided, ErrExpired and ErrExceedsPending. A void of an expired pending transfer is allowed, so an
// expired one can be voided.
func transition(o op, status Status, now, expires time.Time, amount, pending int64) error {
	switch status {
	case Posted:
		return ErrAlreadyPosted
	case Voided:
		return ErrAlreadyVoided
	}
	if o == opVoid {
		return nil
	}
	if !now.Before(expires) {
		return ErrExpired
	}
	if amount > pending {
		return ErrExceedsPending
	}
	return nil
}

// PostPending posts the pending transfer: it writes a new transfer row whose pending_id names the pending one,
// with posted entries for the amount, and the post and void function changes the pending transfer's status from
// pending to posted, in one statement; the pending row's amount and entries do not change (ST-SUP-RP08-LC04). An
// amount below the pending amount posts that part: each side of the pending entries is split by Allocate in the
// ratios of its amounts, and the rest is released. The errors of the state machine are ErrAlreadyPosted,
// ErrAlreadyVoided, ErrExpired and ErrExceedsPending. A retry with the key of an earlier post returns that post.
func (l *Ledger) PostPending(ctx context.Context, tx Tx, t tenant.Tenant, pendingID, key string, amount money.Money) (Transfer, error) {
	return l.settle(ctx, tx, t, pendingID, key, amount, opPost)
}

// Void voids the pending transfer: it writes a new transfer row of no entries whose pending_id names the pending
// one, and the post and void function changes the pending transfer's status from pending to voided, in one
// statement (ST-SUP-RP08-LC04). The errors of the state machine are ErrAlreadyPosted and ErrAlreadyVoided. A retry
// with the key of an earlier void returns that void.
func (l *Ledger) Void(ctx context.Context, tx Tx, t tenant.Tenant, pendingID, key string) (Transfer, error) {
	return l.settle(ctx, tx, t, pendingID, key, money.Money{}, opVoid)
}

func (l *Ledger) settle(ctx context.Context, tx Tx, t tenant.Tenant, pendingID, key string, amount money.Money, o op) (Transfer, error) {
	if t.IsZero() {
		return Transfer{}, tenant.ErrZero
	}
	stored, err := deriveKey(t, l.id, key)
	if err != nil {
		return Transfer{}, err
	}
	kind := Settle
	if o == opVoid {
		kind = Release
	}
	if found, ok, err := l.byKey(ctx, tx, t, stored); err != nil || ok {
		return replay(found, err, func(f Transfer) bool { return f.Kind == kind && f.PendingID == pendingID })
	}
	p, err := l.Get(ctx, tx, t, pendingID)
	if err != nil {
		return Transfer{}, err
	}
	if p.Kind != Hold {
		return Transfer{}, fmt.Errorf("%w: the pending transfer %s of the ledger %s", ErrNotFound, pendingID, l.id)
	}
	if o == opPost {
		if amount.Currency() != l.currency {
			return Transfer{}, fmt.Errorf("%w: the amount is in %q, and the ledger %s holds only %s", ErrInvalid, amount.Currency().Code(), l.id, l.currency)
		}
		if amount.Amount() <= 0 {
			return Transfer{}, fmt.Errorf("%w: the amount %d is not positive", ErrInvalid, amount.Amount())
		}
	}
	if err := transition(o, p.Status, l.now(), p.ExpiresAt, amount.Amount(), p.Amount.Amount()); err != nil {
		return Transfer{}, fmt.Errorf("ledger %s: the pending transfer %s: %w", l.id, pendingID, err)
	}
	w := write{key: stored, kind: kind, code: p.Code, pendingID: pendingID, after: afterSettle}
	if o == opVoid {
		w.amount, w.status, w.settleTo = p.Amount.Amount(), Voided, Voided
	} else {
		w.amount, w.status, w.settleTo = amount.Amount(), Posted, Posted
		if w.entries, err = part(p.Entries, amount); err != nil {
			return Transfer{}, err
		}
	}
	return l.insert(ctx, tx, t, w)
}

// part splits the amount over the pending entries: the debits by the ratios of the debit amounts and the credits
// by the ratios of the credit amounts, so each side sums to the amount. A part of zero is left out.
func part(entries []Entry, amount money.Money) ([]Entry, error) {
	var out []Entry
	for _, side := range []Side{Debit, Credit} {
		var ratios []int64
		var of []Entry
		for _, e := range entries {
			if e.Side == side {
				ratios = append(ratios, e.Amount.Amount())
				of = append(of, e)
			}
		}
		parts, err := amount.Allocate(ratios...)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		for i, p := range parts {
			if p.Amount() > 0 {
				out = append(out, Entry{Account: of[i].Account, Side: side, Amount: p})
			}
		}
	}
	return out, nil
}

// Reverse reverses the posted transfer: it inserts a new transfer whose entries are the original's on the opposite
// side, with the same amounts, whose reverses names the original, and the reverse function sets reversed_by on the
// original, in one statement (ST-SUP-RP08-LC06). It refuses a transfer that is not posted (ErrNotPosted), such as a
// pending or voided one, and one that is reversed already (ErrAlreadyReversed). It takes the row lock of each
// account with a balance limit, as Post does. A retry with the key of an earlier reversal returns that reversal.
// A correction is a reversal and a new transfer: the package has no other change of a written transfer.
func (l *Ledger) Reverse(ctx context.Context, tx Tx, t tenant.Tenant, transferID, key string) (Transfer, error) {
	if t.IsZero() {
		return Transfer{}, tenant.ErrZero
	}
	stored, err := deriveKey(t, l.id, key)
	if err != nil {
		return Transfer{}, err
	}
	if found, ok, err := l.byKey(ctx, tx, t, stored); err != nil || ok {
		return replay(found, err, func(f Transfer) bool { return f.Kind == Reversal && f.Reverses == transferID })
	}
	o, err := l.Get(ctx, tx, t, transferID)
	if err != nil {
		return Transfer{}, err
	}
	if o.Status != Posted || o.Kind == Hold {
		return Transfer{}, fmt.Errorf("%w: the transfer %s is %s %s", ErrNotPosted, transferID, o.Kind, o.Status)
	}
	if o.ReversedBy != "" {
		return Transfer{}, fmt.Errorf("%w: the transfer %s by %s", ErrAlreadyReversed, transferID, o.ReversedBy)
	}
	entries := make([]Entry, len(o.Entries))
	for i, e := range o.Entries {
		entries[i] = Entry{Account: e.Account, Side: opposite(e.Side), Amount: e.Amount}
	}
	if err := l.limit(ctx, tx, t, entries, false); err != nil {
		return Transfer{}, err
	}
	w := write{key: stored, kind: Reversal, code: o.Code, amount: o.Amount.Amount(), status: Posted, reverses: transferID, entries: entries, after: afterReverse}
	return l.insert(ctx, tx, t, w)
}

func opposite(s Side) Side {
	if s == Debit {
		return Credit
	}
	return Debit
}

// stateError maps the SQLSTATE that an owner function raised to the error of the package. The statement failed
// as a whole, so it wrote nothing, and the caller's transaction is aborted.
func stateError(err error) error {
	var coded interface{ SQLState() string }
	if !errors.As(err, &coded) {
		return err
	}
	switch coded.SQLState() {
	case codeAlreadyPosted:
		return fmt.Errorf("%w: %w", ErrAlreadyPosted, err)
	case codeAlreadyVoided:
		return fmt.Errorf("%w: %w", ErrAlreadyVoided, err)
	case codeAlreadyReversed:
		return fmt.Errorf("%w: %w", ErrAlreadyReversed, err)
	case codeNotPosted:
		return fmt.Errorf("%w: %w", ErrNotPosted, err)
	case codeNoPending:
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	return err
}

// deriveKey returns the stored idempotency key of the caller's key: the key derived with the tenant and the
// ledger (ST-SUP-RP07-LC06), so a key of one tenant never meets a row of another under UNIQUE (ledger_id,
// idempotency_key).
func deriveKey(t tenant.Tenant, ledger, key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("%w: a transfer needs an idempotency key", ErrInvalid)
	}
	if len(key) > idempotency.MaxKeyLength {
		return "", fmt.Errorf("%w: the idempotency key is longer than %d bytes", ErrInvalid, idempotency.MaxKeyLength)
	}
	return idempotency.Derive(t.ID(), "", key, "ledger:"+ledger, "transfer"), nil
}

// clean refuses text that a text column must not get: empty, longer than 255 bytes, invalid UTF-8, or with NUL or
// another control character.
func clean(what, s string) error {
	if s == "" || len(s) > 255 || !utf8.ValidString(s) {
		return fmt.Errorf("%w: the %s %q is empty, longer than 255 bytes or not valid UTF-8", ErrInvalid, what, s)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: the %s %q holds a control character", ErrInvalid, what, s)
		}
	}
	return nil
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if r != '-' {
				return false
			}
		case !strings.ContainsRune("0123456789abcdefABCDEF", r):
			return false
		}
	}
	return true
}

func null(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// parseSum reads a numeric sum that the database gives as text into an int64, or an overflow error.
func parseSum(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: the sum %s does not fit in int64 minor units", money.ErrOverflow, s)
	}
	return n, nil
}
