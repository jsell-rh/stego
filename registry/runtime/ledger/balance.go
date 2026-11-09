package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"stego.dev/runtime/money"
	"stego.dev/runtime/tenant"
)

// A snapshot covers the posted entries of its account whose transaction ID is below its through_xid. The snapshot
// job takes through_xid as the oldest transaction ID that was still running when it read (pg_snapshot_xmin), so
// every transaction below it has ended and no entry below it can commit later: the entries after a snapshot are
// exactly those with a transaction ID at or above its through_xid. The entries of a pending transfer count in no
// balance.

// signed is the SQL of an entry's amount on the normal side of its account: positive on that side, negative on
// the other (ST-SUP-RP08-LC07).
const signed = `CASE WHEN e.side = a.normal THEN e.amount ELSE -e.amount END`

// selectBalance reads the account's normal side, its latest snapshot and the sum of the posted entries after it.
// The sums are numeric, so they cannot overflow in the database; Go reads them as text.
const selectBalance = `WITH s AS (
 SELECT amount, through_xid FROM {s}.balance_snapshots
 WHERE tenant_id = $1 AND ledger_id = $2 AND account_id = $3 ORDER BY through_xid DESC LIMIT 1
)
SELECT (coalesce((SELECT amount FROM s), 0)::numeric + coalesce((
 SELECT sum(` + signed + `) FROM {s}.entries e
 WHERE e.tenant_id = a.tenant_id AND e.ledger_id = a.ledger_id AND e.account_id = a.id AND NOT e.pending
 AND (NOT EXISTS (SELECT 1 FROM s) OR e.txid >= (SELECT through_xid FROM s))), 0))::text
FROM {s}.accounts a WHERE a.tenant_id = $1 AND a.ledger_id = $2 AND a.id = $3`

// selectBalanceAt reads the sum of the posted entries of the account that take effect at or before the time,
// whatever their posted_at.
const selectBalanceAt = `SELECT coalesce((
 SELECT sum(` + signed + `) FROM {s}.entries e
 WHERE e.tenant_id = a.tenant_id AND e.ledger_id = a.ledger_id AND e.account_id = a.id AND NOT e.pending
 AND e.effective_at <= $4), 0)::text
FROM {s}.accounts a WHERE a.tenant_id = $1 AND a.ledger_id = $2 AND a.id = $3`

// selectHolds reads the sum of the entries of the account's open pending transfers that lower its balance.
const selectHolds = `SELECT coalesce(sum(LEAST(` + signed + `, 0)), 0)::text
FROM {s}.entries e JOIN {s}.accounts a ON a.tenant_id = e.tenant_id AND a.ledger_id = e.ledger_id AND a.id = e.account_id
JOIN {s}.transfers p ON p.tenant_id = e.tenant_id AND p.ledger_id = e.ledger_id AND p.id = e.transfer_id
WHERE e.tenant_id = $1 AND e.ledger_id = $2 AND e.account_id = $3 AND e.pending AND p.status = 'pending'`

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

// available returns the balance of the account less its open pending holds, which a balance limit counts.
func (l *Ledger) available(ctx context.Context, tx Tx, t tenant.Tenant, account string) (int64, error) {
	balance, err := l.sum(ctx, tx, l.q(selectBalance), t.ID(), l.id, account)
	if err != nil {
		return 0, err
	}
	holds, err := l.sum(ctx, tx, l.q(selectHolds), t.ID(), l.id, account)
	if err != nil {
		return 0, err
	}
	return add(balance, holds)
}

func (l *Ledger) sum(ctx context.Context, tx Tx, statement string, args ...any) (int64, error) {
	var text string
	err := tx.QueryRowContext(ctx, statement, args...).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: the account %v of the ledger %s", ErrNotFound, args[2], l.id)
	}
	if err != nil {
		return 0, fmt.Errorf("ledger %s: read the balance of %v: %w", l.id, args[2], err)
	}
	return parseSum(text)
}

// normal returns the amount of an entry on the side, signed by the normal side of its account: positive when the
// side is the normal side, negative when not.
func normal(side, normalSide Side, amount int64) int64 {
	if side == normalSide {
		return amount
	}
	return -amount
}

// add returns a + b, or an overflow error.
func add(a, b int64) (int64, error) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, fmt.Errorf("%w: %d + %d", money.ErrOverflow, a, b)
	}
	return a + b, nil
}
