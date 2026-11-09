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
	// ErrExpired is the error of a post of a pending transfer after its expiry by the database clock.
	ErrExpired = errors.New("ledger: expired: the pending transfer is expired")
	// ErrExceedsPending is the error of a post of an amount larger than the pending amount.
	ErrExceedsPending = errors.New("ledger: exceeds_pending: the amount is larger than the pending amount")
)

// The other errors of the package.
var (
	// ErrInvalid is the error of a transfer that cannot be written: fewer than two entries, debits that differ
	// from the credits, an entry in another currency than the ledger's or on no account of the ledger, an amount
	// that is not positive, an effective time in the future or before the close date, a post that is not a split
	// of its pending transfer, a reversal that is not the mirror of its original, or text that is not valid UTF-8
	// or holds a control character.
	ErrInvalid = errors.New("ledger: the transfer is not valid")
	// ErrBelowMinimum is the error of a transfer that would take an account with a balance limit below it.
	ErrBelowMinimum = errors.New("ledger: the account would go below its minimum balance")
	// ErrNotFound is the error for a transfer or an account that the tenant's ledger does not hold.
	ErrNotFound = errors.New("ledger: not found")
	// ErrNotPosted is the error of a reversal of a transfer that is not posted, such as a pending or voided one.
	ErrNotPosted = errors.New("ledger: the transfer is not posted")
	// ErrAlreadyReversed is the error of a second reversal of one transfer.
	ErrAlreadyReversed = errors.New("ledger: the transfer is reversed already")
	// ErrKeyReused is the error of a call whose idempotency key names a transfer of another body
	// (ST-SUP-RP08-LC05). It writes nothing.
	ErrKeyReused = errors.New("ledger: the idempotency key names a transfer of another body")
	// ErrIsolation is the error of a write in a transaction whose isolation is not READ COMMITTED or
	// SERIALIZABLE (ST-SUP-RP08-LC10). It comes before any write.
	ErrIsolation = errors.New("ledger: a ledger write needs READ COMMITTED or SERIALIZABLE isolation")
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

// Settings are the declared settings of one ledger. DDL writes them into the owner functions, so the database and
// the Go side read the same declaration.
type Settings struct {
	// ID is the ledger_id of the ledger's rows.
	ID string
	// Currency is the ISO 4217 code of the ledger's one currency.
	Currency string
	// Schema holds the ledger tables; the empty schema is public.
	Schema string
	// PendingTimeout is the time a pending transfer stays open: ledger_pend sets its expiry this long after the
	// database clock. Zero declares no two-phase transfers, and Pend refuses then.
	PendingTimeout time.Duration
	// SnapshotEvery is the number of entries of an account after which the snapshot job writes a snapshot.
	SnapshotEvery int
	// ClosedBefore is the declared close date: an effective time before it is refused. Zero declares none.
	ClosedBefore time.Time
}

// Ledger is one declared ledger: its ID, its one currency, and the calls of the owner functions.
type Ledger struct {
	id       string
	currency money.Currency
	schema   string
	timeout  time.Duration
	every    int64
	closed   time.Time
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
	return &Ledger{id: s.ID, currency: c, schema: s.Schema, timeout: s.PendingTimeout, every: int64(s.SnapshotEvery), closed: s.ClosedBefore}, nil
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

// The calls of the owner functions. Each is one statement; the function writes the transfer and its entries with
// one INSERT (ST-SUP-RP08-LC03).
const (
	callOpen        = `SELECT {s}.ledger_open($1, $2, $3, $4, $5, $6, $7, $8)`
	callPost        = `SELECT out_id::text, out_replayed FROM {s}.ledger_post($1, $2, $3, $4, $5::jsonb, $6::timestamptz)`
	callPend        = `SELECT out_id::text, out_replayed FROM {s}.ledger_pend($1, $2, $3, $4, $5::jsonb, $6::timestamptz)`
	callPostPending = `SELECT out_id::text, out_replayed FROM {s}.ledger_post_pending($1, $2, $3, $4::uuid, $5::jsonb)`
	callVoid        = `SELECT out_id::text, out_replayed FROM {s}.ledger_void($1, $2, $3, $4::uuid)`
	callReverse     = `SELECT out_id::text, out_replayed FROM {s}.ledger_reverse($1, $2, $3, $4::uuid, $5::jsonb)`
	readIsolation   = `SELECT current_setting('transaction_isolation'), now()`
)

// Open inserts the account into the tenant's ledger, in the ledger's currency, through ledger_open.
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
	if _, err := l.writable(ctx, tx); err != nil {
		return err
	}
	var limit sql.NullInt64
	if a.MinBalance != nil {
		limit = sql.NullInt64{Int64: *a.MinBalance, Valid: true}
	}
	if _, err := tx.ExecContext(ctx, l.q(callOpen), t.ID(), l.id, a.ID, a.Code, string(a.Normal), l.currency.Code(), limit, a.Hot); err != nil {
		return stateError(fmt.Errorf("ledger %s: open the account %s: %w", l.id, a.ID, err))
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
	// Key is the caller's idempotency key: a retry with the same key and the same body returns the transfer of the
	// first call, and one with another body gets ErrKeyReused. The database stores the key derived with the tenant,
	// so keys of two tenants never meet.
	Key string
	// Code is the declared transfer code, such as topup.
	Code string
	// Entries are the debits and credits; at least two, and the debits equal the credits.
	Entries []Entry
	// EffectiveAt is when the transfer takes effect for a balance at a time; zero is the time of posting. It may
	// not be in the future or before the ledger's close date.
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
	// Replayed is set when the call found the transfer of an earlier call with the same key and body and wrote
	// nothing.
	Replayed bool
}

const selectTransfer = `SELECT id::text, kind, code, amount, currency, status, coalesce(pending_id::text, ''),
 coalesce(reverses::text, ''), coalesce(reversed_by::text, ''), expires_at, effective_at, posted_at
 FROM {s}.transfers WHERE tenant_id = $1 AND ledger_id = $2 AND id = $3::uuid`

const selectEntries = `SELECT account_id, side, amount FROM {s}.entries
 WHERE tenant_id = $1 AND ledger_id = $2 AND transfer_id = $3::uuid ORDER BY seq`

// selectKey reports whether the tenant's ledger holds a transfer of the caller's key, as ledger_key derives it.
const selectKey = `SELECT EXISTS (SELECT 1 FROM {s}.transfers WHERE tenant_id = $1 AND ledger_id = $2
 AND idempotency_key = encode(sha256(convert_to($1, 'UTF8') || '\x00'::bytea || convert_to($3, 'UTF8')), 'hex'))`

// writable refuses a transaction whose isolation can overdraw (ST-SUP-RP08-LC10), before any write, and returns
// the database clock of the transaction.
func (l *Ledger) writable(ctx context.Context, tx Tx) (time.Time, error) {
	var isolation string
	var now time.Time
	if err := tx.QueryRowContext(ctx, readIsolation).Scan(&isolation, &now); err != nil {
		return time.Time{}, fmt.Errorf("ledger %s: read the isolation: %w", l.id, err)
	}
	if isolation != "read committed" && isolation != "serializable" {
		return time.Time{}, fmt.Errorf("%w: the transaction is %s", ErrIsolation, isolation)
	}
	return now, nil
}

// Post writes a transfer and its entries through one call of ledger_post, which inserts them in one statement
// (ST-SUP-RP08-LC03). Go refuses fewer than two entries, debits that differ from the credits and another currency
// before the call; the function refuses them again, refuses an entry on no account of the ledger, takes the row
// lock of each account with a balance limit, in account ID order, and of no other, and refuses a transfer that
// takes such an account below its limit, with open pending debits counted. A retry with the same key and body
// returns the original and inserts nothing (ST-SUP-RP08-LC05). Post does not commit.
func (l *Ledger) Post(ctx context.Context, tx Tx, t tenant.Tenant, p Posting) (Transfer, error) {
	return l.post(ctx, tx, t, p, callPost)
}

// Pend writes a pending transfer and its entries through ledger_pend, as Post does, with an expiry PendingTimeout
// after the database clock. No later call changes its amount or its entries: PostPending and Void write new rows
// that name it (ST-SUP-RP08-LC04). The entries of a pending transfer count in no balance; a pending debit on an
// account with a balance limit holds that amount against the limit until it is posted, voided or expired.
func (l *Ledger) Pend(ctx context.Context, tx Tx, t tenant.Tenant, p Posting) (Transfer, error) {
	if l.timeout <= 0 {
		return Transfer{}, fmt.Errorf("%w: the ledger %s declares no two-phase transfers", ErrInvalid, l.id)
	}
	return l.post(ctx, tx, t, p, callPend)
}

func (l *Ledger) post(ctx context.Context, tx Tx, t tenant.Tenant, p Posting, call string) (Transfer, error) {
	if t.IsZero() {
		return Transfer{}, tenant.ErrZero
	}
	if err := clean("transfer code", p.Code); err != nil {
		return Transfer{}, err
	}
	if err := checkKey(p.Key); err != nil {
		return Transfer{}, err
	}
	if _, err := l.balanced(p.Entries); err != nil {
		return Transfer{}, err
	}
	if _, err := l.writable(ctx, tx); err != nil {
		return Transfer{}, err
	}
	list, err := entriesJSON(p.Entries)
	if err != nil {
		return Transfer{}, err
	}
	return l.call(ctx, tx, t, call, t.ID(), l.id, p.Key, p.Code, list, nullTime(p.EffectiveAt))
}

// call runs one owner function and reads the transfer it wrote or replayed.
func (l *Ledger) call(ctx context.Context, tx Tx, t tenant.Tenant, call string, args ...any) (Transfer, error) {
	var id string
	var replayed bool
	if err := tx.QueryRowContext(ctx, l.q(call), args...).Scan(&id, &replayed); err != nil {
		return Transfer{}, stateError(fmt.Errorf("ledger %s: %w", l.id, err))
	}
	out, err := l.Get(ctx, tx, t, id)
	if err != nil {
		return Transfer{}, err
	}
	out.Replayed = replayed
	return out, nil
}

// entriesJSON is the entries as the owner functions take them: a JSON array of account, side and amount.
func entriesJSON(entries []Entry) (string, error) {
	type item struct {
		Account string `json:"account"`
		Side    string `json:"side"`
		Amount  int64  `json:"amount"`
	}
	items := make([]item, len(entries))
	for i, e := range entries {
		items[i] = item{Account: e.Account, Side: string(e.Side), Amount: e.Amount.Amount()}
	}
	b, err := json.Marshal(items)
	return string(b), err
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

// Get reads the transfer with the ID and its entries from the tenant's ledger.
func (l *Ledger) Get(ctx context.Context, tx Tx, t tenant.Tenant, id string) (Transfer, error) {
	if t.IsZero() {
		return Transfer{}, tenant.ErrZero
	}
	if !isUUID(id) {
		return Transfer{}, fmt.Errorf("%w: the transfer %q", ErrNotFound, id)
	}
	id = strings.ToLower(id)
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
// transfer in the status, with the expiry, may happen at the database time now, for the amount. Its only errors
// are ErrAlreadyPosted, ErrAlreadyVoided, ErrExpired and ErrExceedsPending. A void of an expired pending transfer
// is allowed, so an expired one can be voided. ledger_post_pending and ledger_void apply the same rules in the
// database, under the row lock of the pending transfer.
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

// PostPending posts the pending transfer through ledger_post_pending: a new transfer row whose pending_id names the
// pending one, with posted entries for the amount, and the change of the pending status from pending to posted;
// the pending row's amount and entries do not change (ST-SUP-RP08-LC04). An amount below the pending amount posts
// that part: each side of the pending entries is split by Allocate in the ratios of its amounts. The function
// refuses entries that are not such a split and a pending transfer whose expiry has passed by the database clock.
// The errors of the state machine are ErrAlreadyPosted, ErrAlreadyVoided, ErrExpired and ErrExceedsPending. A
// retry with the same key returns that post.
func (l *Ledger) PostPending(ctx context.Context, tx Tx, t tenant.Tenant, pendingID, key string, amount money.Money) (Transfer, error) {
	return l.settle(ctx, tx, t, pendingID, key, amount, opPost)
}

// Void voids the pending transfer through ledger_void: a new transfer row of no entries whose pending_id names the
// pending one, and the change of the pending status from pending to voided (ST-SUP-RP08-LC04). The errors of the
// state machine are ErrAlreadyPosted and ErrAlreadyVoided. A retry with the same key returns that void.
func (l *Ledger) Void(ctx context.Context, tx Tx, t tenant.Tenant, pendingID, key string) (Transfer, error) {
	return l.settle(ctx, tx, t, pendingID, key, money.Money{}, opVoid)
}

func (l *Ledger) settle(ctx context.Context, tx Tx, t tenant.Tenant, pendingID, key string, amount money.Money, o op) (Transfer, error) {
	if t.IsZero() {
		return Transfer{}, tenant.ErrZero
	}
	// The database gives a uuid in lower case; the comparisons with the rows it gives use that form.
	pendingID = strings.ToLower(pendingID)
	if err := checkKey(key); err != nil {
		return Transfer{}, err
	}
	now, err := l.writable(ctx, tx)
	if err != nil {
		return Transfer{}, err
	}
	p, err := l.Get(ctx, tx, t, pendingID)
	if err != nil {
		return Transfer{}, err
	}
	if p.Kind != Hold {
		return Transfer{}, fmt.Errorf("%w: the pending transfer %s of the ledger %s", ErrNotFound, pendingID, l.id)
	}
	if o == opVoid {
		if err := transition(o, p.Status, now, p.ExpiresAt, 0, p.Amount.Amount()); err != nil && !l.replayable(ctx, tx, t, key) {
			return Transfer{}, fmt.Errorf("ledger %s: the pending transfer %s: %w", l.id, pendingID, err)
		}
		return l.call(ctx, tx, t, callVoid, t.ID(), l.id, key, pendingID)
	}
	if amount.Currency() != l.currency {
		return Transfer{}, fmt.Errorf("%w: the amount is in %q, and the ledger %s holds only %s", ErrInvalid, amount.Currency().Code(), l.id, l.currency)
	}
	if amount.Amount() <= 0 {
		return Transfer{}, fmt.Errorf("%w: the amount %d is not positive", ErrInvalid, amount.Amount())
	}
	if err := transition(o, p.Status, now, p.ExpiresAt, amount.Amount(), p.Amount.Amount()); err != nil && !l.replayable(ctx, tx, t, key) {
		return Transfer{}, fmt.Errorf("ledger %s: the pending transfer %s: %w", l.id, pendingID, err)
	}
	entries, err := part(p.Entries, amount)
	if err != nil {
		return Transfer{}, err
	}
	list, err := entriesJSON(entries)
	if err != nil {
		return Transfer{}, err
	}
	return l.call(ctx, tx, t, callPostPending, t.ID(), l.id, key, pendingID, list)
}

// replayable reports whether the tenant's ledger holds a transfer of the key, so the call is a retry that the owner
// function answers with the original, and not a new call that the state machine refuses.
func (l *Ledger) replayable(ctx context.Context, tx Tx, t tenant.Tenant, key string) bool {
	var found bool
	err := tx.QueryRowContext(ctx, l.q(selectKey), t.ID(), l.id, key).Scan(&found)
	return err == nil && found
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

// Reverse reverses the posted transfer through ledger_reverse: a new transfer whose entries are exactly the
// original's with each side swapped, whose reverses names the original, and reversed_by set on the original
// (ST-SUP-RP08-LC06). It refuses a transfer that is not posted (ErrNotPosted), such as a pending or voided one, and
// one that is reversed already (ErrAlreadyReversed); the function refuses entries that are not the mirror of the
// original. It takes the row lock of each account with a balance limit, as Post does. A retry with the same key
// returns that reversal. A correction is a reversal and a new transfer: the package has no other change of a
// written transfer.
func (l *Ledger) Reverse(ctx context.Context, tx Tx, t tenant.Tenant, transferID, key string) (Transfer, error) {
	if t.IsZero() {
		return Transfer{}, tenant.ErrZero
	}
	transferID = strings.ToLower(transferID)
	if err := checkKey(key); err != nil {
		return Transfer{}, err
	}
	if _, err := l.writable(ctx, tx); err != nil {
		return Transfer{}, err
	}
	o, err := l.Get(ctx, tx, t, transferID)
	if err != nil {
		return Transfer{}, err
	}
	if !l.replayable(ctx, tx, t, key) {
		if o.Status != Posted || o.Kind == Hold {
			return Transfer{}, fmt.Errorf("%w: the transfer %s is %s %s", ErrNotPosted, transferID, o.Kind, o.Status)
		}
		if o.ReversedBy != "" {
			return Transfer{}, fmt.Errorf("%w: the transfer %s by %s", ErrAlreadyReversed, transferID, o.ReversedBy)
		}
	}
	entries := make([]Entry, len(o.Entries))
	for i, e := range o.Entries {
		entries[i] = Entry{Account: e.Account, Side: opposite(e.Side), Amount: e.Amount}
	}
	list, err := entriesJSON(entries)
	if err != nil {
		return Transfer{}, err
	}
	return l.call(ctx, tx, t, callReverse, t.ID(), l.id, key, transferID, list)
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
	var to error
	switch coded.SQLState() {
	case codeAlreadyPosted:
		to = ErrAlreadyPosted
	case codeAlreadyVoided:
		to = ErrAlreadyVoided
	case codeAlreadyReversed:
		to = ErrAlreadyReversed
	case codeNotPosted:
		to = ErrNotPosted
	case codeNoPending:
		to = ErrNotFound
	case codeKeyReused:
		to = ErrKeyReused
	case codeBelowMinimum:
		to = ErrBelowMinimum
	case codeExpired:
		to = ErrExpired
	case codeExceedsPending:
		to = ErrExceedsPending
	case codeIsolation:
		to = ErrIsolation
	case codeEffective, codeNotSplit, codeNotMirror, "22023", "23514":
		to = ErrInvalid
	case "22003":
		to = money.ErrOverflow
	default:
		return err
	}
	return fmt.Errorf("%w: %w", to, err)
}

// checkKey refuses an idempotency key that a text column must not get or that is longer than the idempotency
// runtime allows (ST-SUP-RP07).
func checkKey(key string) error {
	if len(key) > idempotency.MaxKeyLength {
		return fmt.Errorf("%w: the idempotency key is longer than %d bytes", ErrInvalid, idempotency.MaxKeyLength)
	}
	return clean("idempotency key", key)
}

// clean refuses text that a text column must not get: empty, longer than 255 bytes, invalid UTF-8, or with NUL or
// another control character.
func clean(what, s string) error {
	if s == "" || len(s) > 255 || !utf8.ValidString(s) {
		return fmt.Errorf("%w: the %s %q is empty, longer than 255 bytes or not valid UTF-8", ErrInvalid, what, s)
	}
	for _, r := range s {
		if r < 0x20 || (r >= 0x7f && r < 0xa0) {
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
