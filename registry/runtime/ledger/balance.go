package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"stego.dev/runtime/money"
	"stego.dev/runtime/tenant"
)

// A snapshot covers the posted entries of its account whose transaction ID is below its through_xid. The snapshot
// job takes through_xid as the oldest transaction ID that was still running when it read (pg_snapshot_xmin), so
// every transaction below it has ended and no entry below it can commit later: the entries after a snapshot are
// exactly those with a transaction ID at or above its through_xid. The entries of a pending transfer count in no
// balance.

// selectBalance reads the balance through ledger_balance, the one derivation that the owner functions also use for
// the balance limit. The sum is numeric, so it cannot overflow in the database; Go reads it as text, and NULL is
// an account that the ledger does not hold.
const selectBalance = `SELECT {s}.ledger_balance($2, $3)::text WHERE $1::text IS NOT NULL`

// selectBalanceAt reads the sum of the posted entries of the account that take effect at or before the time,
// whatever their posted_at.
const selectBalanceAt = `SELECT coalesce((
 SELECT sum(` + signedSQL + `) FROM {s}.entries e
 WHERE e.tenant_id = a.tenant_id AND e.ledger_id = a.ledger_id AND e.account_id = a.id AND NOT e.pending
 AND e.effective_at <= $4), 0)::text
FROM {s}.accounts a WHERE a.tenant_id = $1 AND a.ledger_id = $2 AND a.id = $3`

// Balance returns the balance of the account: its latest snapshot plus the sum of the entries posted after it,
// each signed by the account's normal side, so a debit-normal account grows with debits and a credit-normal
// account with credits (ST-SUP-RP08-LC07). A balance that does not fit in an int64 is an overflow error.
func (l *Ledger) Balance(ctx context.Context, tx Tx, t tenant.Tenant, account string) (money.Money, error) {
	if t.IsZero() {
		return money.Money{}, tenant.ErrZero
	}
	n, err := l.sum(ctx, tx, l.q(selectBalance), t.ID(), l.id, account)
	if err != nil {
		return money.Money{}, err
	}
	return money.Of(n, l.currency)
}

// BalanceAt returns the balance of the account at the time: the sum of the posted entries whose effective_at is at
// or before it, each signed by the account's normal side, whatever their posted_at (ST-SUP-RP08-LC07).
func (l *Ledger) BalanceAt(ctx context.Context, tx Tx, t tenant.Tenant, account string, at time.Time) (money.Money, error) {
	if t.IsZero() {
		return money.Money{}, tenant.ErrZero
	}
	n, err := l.sum(ctx, tx, l.q(selectBalanceAt), t.ID(), l.id, account, at)
	if err != nil {
		return money.Money{}, err
	}
	return money.Of(n, l.currency)
}

func (l *Ledger) sum(ctx context.Context, tx Tx, statement string, args ...any) (int64, error) {
	var text sql.NullString
	err := tx.QueryRowContext(ctx, statement, args...).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !text.Valid) {
		return 0, fmt.Errorf("%w: the account %v of the ledger %s", ErrNotFound, args[2], l.id)
	}
	if err != nil {
		return 0, fmt.Errorf("ledger %s: read the balance of %v: %w", l.id, args[2], err)
	}
	return parseSum(text.String)
}
