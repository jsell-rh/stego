package command

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jsell-rh/stego/registry/runtime/idempotency"
	"github.com/jsell-rh/stego/registry/runtime/sealed"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// The secret fields of an entity (ST-SUP-RP10-LC44, decision ST-DECL-Q44): the generated Values gives each readable
// secret as SealAt and each compare-only secret as HashOf, and the generated Scan reads each readable secret into
// OpenAt and each compare-only secret into ComparedAt. The runtime seals and hashes the values of an insert and an
// update just before the statement, and opens the values of each row that a read of the handle scans, with the place
// of the value: the tenant of the transaction, the table, the column and the row ID.

// sealArg is a readable secret that the runtime seals before the statement.
type sealArg struct {
	at    sealed.Place
	value sealed.Secret
}

// hashArg is a compare-only secret that the runtime stores as its hash.
type hashArg struct {
	value sealed.Compared
}

// SealAt gives the statement value of a readable secret of the column of the table, in the row of ID row: the
// runtime seals it with the first key of the key ring before the statement, and a secret that is not set is NULL.
func SealAt(table, column, row string, value sealed.Secret) any {
	// The scan gives the ID in lower case, so the seal takes it in lower case too (security review of PR #441, L7).
	return sealArg{at: sealed.Place{Table: table, Column: column, Row: strings.ToLower(row)}, value: value}
}

// HashOf gives the statement value of a compare-only secret: the runtime stores the Argon2id hash of a new text, the
// stored hash of a value that a read gave, and NULL for a value that is not set.
func HashOf(value sealed.Compared) any { return hashArg{value: value} }

// openTarget is the scan target of a readable secret: it keeps the sealed bytes, which the runtime opens after the
// scan of the row.
type openTarget struct {
	table, column string
	row           *string
	into          *sealed.Secret
	raw           []byte
	null          bool
}

// Scan keeps the sealed bytes of the column, or NULL.
func (o *openTarget) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		o.raw, o.null = nil, true
	case []byte:
		o.raw, o.null = append([]byte(nil), v...), false
	default:
		return fmt.Errorf("command: the sealed column %s holds a %T, not bytes", o.column, src)
	}
	return nil
}

// OpenAt gives the scan target of a readable secret of the column of the table, which the runtime opens into into
// after the scan of the row, at the row ID that row holds then.
func OpenAt(table, column string, row *string, into *sealed.Secret) any {
	return &openTarget{table: table, column: column, row: row, into: into}
}

// comparedTarget is the scan target of a compare-only secret: its stored hash.
type comparedTarget struct{ into *sealed.Compared }

// Scan keeps the stored hash, or a value that is not set for NULL.
func (c comparedTarget) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*c.into = sealed.Compared{}
	case string:
		*c.into = sealed.ComparedHash(v)
	case []byte:
		*c.into = sealed.ComparedHash(string(v))
	default:
		return fmt.Errorf("command: a compare-only column holds a %T, not text", src)
	}
	return nil
}

// ComparedAt gives the scan target of a compare-only secret.
func ComparedAt(into *sealed.Compared) any { return comparedTarget{into: into} }

// sealedError is a sealed value that does not seal or open, or no key ring: an internal error of the runtime.
type sealedError struct{ internalError }

// Unwrap gives the internal error, which the outcome of a call and its log read.
func (e *sealedError) Unwrap() error { return &e.internalError }

// sealedFault tells whether err is a sealedError.
func sealedFault(err error) bool {
	var e *sealedError
	return errors.As(err, &e)
}

// sealedf is a sealedError with the text of the format.
func sealedf(format string, args ...any) error {
	return &sealedError{internalError{text: fmt.Sprintf(format, args...)}}
}

// errNoRing is the error of a sealed value with no key ring in the runtime.
var errNoRing = errors.New("command: the runtime has no key ring for the secret fields (STEGO_SECRET_KEY_RING_FILE)")

// sealArgs gives the statement values with each readable secret sealed and each compare-only secret hashed, at the
// tenant of the transaction. A failure names the column and the row, never a value.
func (s *state) sealArgs(ctx context.Context, args []any) ([]any, error) {
	out := args
	copied := false
	for i, arg := range args {
		var value any
		switch a := arg.(type) {
		case sealArg:
			if !a.value.IsSet() {
				value = nil
				break
			}
			ring := s.rt.cfg.Ring
			if ring == nil {
				return nil, sealedf("%v: the column %s of %s", errNoRing, a.at.Column, a.at.Table)
			}
			at := a.at
			at.Tenant = s.tenant.ID()
			b, err := ring.Seal([]byte(a.value.Reveal()), at)
			if err != nil {
				return nil, sealedf("command: the column %s of the row %s of %s does not seal", at.Column, at.Row, at.Table)
			}
			value = b
		case countedHash:
			// A stored counted value goes back only into the field and the row whose count it is bound to, in the
			// attempt that read it: a copy to another row would get a new count of 0 (security review of PR #479,
			// L1).
			if a.value.IsCounted() && !s.boundTo(a.value, attemptKey{counter: a.counter, row: a.row}) {
				return nil, sealedf("command: a counted compare-only value of %s is written to another field or row than the one it was read from",
					a.counter.spec.Entity)
			}
			// A new value of a counted field resets the count of its row when the call commits (security review of
			// PR #451, L2).
			if a.value.IsNew() {
				// sealArgs runs inside a method of the handle, which holds the lock of the state.
				s.resetAttemptsHeld(attemptKey{counter: a.counter, row: a.row})
			}
			stored, err := a.value.StoredBy(ctx, storeKey)
			if err != nil {
				return nil, sealedf("command: a compare-only value does not hash")
			}
			if stored == "" {
				value = nil
			} else {
				value = stored
			}
		case hashArg:
			// A field with no counter never takes a counted value, so its read cannot give the hash of the value to
			// the fill (security review of PR #479, L1).
			if a.value.IsCounted() {
				return nil, sealedf("command: a counted compare-only value is written to a field with no limit of wrong attempts")
			}
			stored, err := a.value.StoredContext(ctx)
			if err != nil {
				return nil, sealedf("command: a compare-only value does not hash")
			}
			if stored == "" {
				value = nil
			} else {
				value = stored
			}
		default:
			continue
		}
		if !copied {
			out = append([]any(nil), args...)
			copied = true
		}
		out[i] = value
	}
	return out, nil
}

// openingRow is a Scanner that opens each readable secret of a row after its scan, at the tenant of the transaction.
type openingRow struct {
	Scanner
	s *state
}

// opening wraps a row of a statement of the handle so that its scan opens each readable secret and binds each counted
// compare-only value to the counter of its row (ST-SUP-RP10-LC54).
func (s *state) opening(row Scanner) Scanner { return openingRow{Scanner: row, s: s} }

func (o openingRow) Scan(dest ...any) error {
	if err := o.Scanner.Scan(dest...); err != nil {
		return err
	}
	for _, d := range dest {
		if counted, ok := d.(*countedTarget); ok {
			o.s.bindCounted(counted)
			continue
		}
		if sealing, ok := d.(sealingTarget); ok {
			// A field whose seal step is open reads the clear value when the two columns differ (ST-SUP-RP10-LC49).
			at := sealed.Place{Tenant: o.s.tenant.ID(), Table: sealing.s.table, Column: sealing.s.column}
			if sealing.s.row != nil {
				at.Row = strings.ToLower(*sealing.s.row)
			}
			sealing.s.resolve(context.Background(), o.s.rt.cfg.Ring, at)
			continue
		}
		target, ok := d.(*openTarget)
		if !ok {
			continue
		}
		if target.null {
			*target.into = sealed.Secret{}
			continue
		}
		at := sealed.Place{Tenant: o.s.tenant.ID(), Table: target.table, Column: target.column}
		if target.row != nil {
			at.Row = strings.ToLower(*target.row)
		}
		ring := o.s.rt.cfg.Ring
		if ring == nil {
			return sealedf("%v: the column %s of %s", errNoRing, at.Column, at.Table)
		}
		plain, err := ring.Open(target.raw, at)
		if err != nil {
			// The error names the column and the row of the place, never a value (ST-SUP-RP10-LC44).
			return sealedf("command: %v", err)
		}
		*target.into = sealed.SecretOf(string(plain))
	}
	return nil
}

// keyed gives req with a keyed fingerprint and a sealer of its answer when the runtime has a key ring: a body of a
// service with a secret or a pii field can hold a secret or personal data, so the key table stores the HMAC of the
// fingerprint with a key that HKDF derives from the first key of the ring, and a fingerprint of an earlier key of the
// ring still matches while the ring holds that key (ST-SUP-RP16-LC05, security review of PR #441, M2). A runtime with
// no key ring is of a service with no secret and no pii field, and keeps the plain fingerprint and the clear answer.
func (rt *Runtime) keyed(req idempotency.Request) idempotency.Request {
	ring := rt.cfg.Ring
	if ring == nil || req.Fingerprint == "" {
		return req
	}
	raw := req.Fingerprint
	req.Fingerprint = ring.Fingerprint(raw)
	req.Matches = func(stored string) bool { return ring.FingerprintMatches(stored, raw) }
	// An answer can hold a secret or a pii field of a row, so the key table keeps it sealed with the ring for the
	// retention of the key (ST-SUP-RP07-LC13, decision on ST-SUP-Q182 (b)).
	req.Sealer = answerSealer{ring: ring}
	return req
}

// answerSealer seals and opens the stored answers of the idempotency keys with the key ring of the runtime.
type answerSealer struct{ ring *sealed.Ring }

func (a answerSealer) SealAnswer(tenant, column, row string, plain []byte) ([]byte, error) {
	return a.ring.Seal(plain, sealed.Place{Tenant: tenant, Table: idempotency.AnswerTable, Column: column, Row: row})
}

func (a answerSealer) OpenAnswer(tenant, column, row string, data []byte) ([]byte, error) {
	return a.ring.Open(data, sealed.Place{Tenant: tenant, Table: idempotency.AnswerTable, Column: column, Row: row})
}

// ResealColumn is one sealed column that the re-seal job moves to the first key of the ring (ST-SUP-RP10-LC46): its
// entity, table and column, and the generated statements of the job, which take the tenant as $1:
//   - Select reads the ID text and the sealed value of at most $3 rows after the ID $4, in ID order, whose value does
//     not start with the prefix $2 of the first key, FOR UPDATE SKIP LOCKED: the job pages by the key and reads each
//     row once in a run;
//   - Update sets the column of the row of ID $2 to $3 only while it holds $4;
//   - Left counts, with no lock, the rows whose value does not start with $2, after the pages: a row that SKIP
//     LOCKED passed or that a command wrote in between counts until a later run moves it (security re-review of PR
//     #441, N1).
type ResealColumn struct {
	Entity, Table, Column string
	Select, Update, Left  string
}

// ResealBatch is the most rows that one statement of the re-seal job reads.
const ResealBatch = 100

// firstID is the ID before every UUID, where the pages of the re-seal job start.
const firstID = "00000000-0000-0000-0000-000000000000"

// ResealResult is what one run of the re-seal job did for one column over all tenants: the values that it moved to
// the first key; Left, the values that are still on an earlier key after its pages, by a count with no lock; and
// Failed, those of them that do not open, such as of a key that the ring no longer holds. An earlier key may go only
// after a whole run that gives Left 0.
type ResealResult struct {
	Entity, Column      string
	Moved, Left, Failed int64
}

// Reseal is the re-seal job of the secret fields of a service (ST-SUP-RP10-LC46).
type Reseal struct {
	db      *sql.DB
	schema  string
	ring    *sealed.Ring
	columns []ResealColumn
	tenants func(ctx context.Context) ([]tenant.Tenant, error)
}

// NewReseal makes the re-seal job of the columns over the tenants of the tenant list of schema, on db, a pool that
// tenant.Open returned, with the key ring.
func NewReseal(db *sql.DB, schema string, ring *sealed.Ring, columns []ResealColumn) (*Reseal, error) {
	switch {
	case db == nil || ring == nil:
		return nil, errors.New("command: the re-seal job needs a pool and the key ring")
	case schema == "":
		return nil, errors.New("command: the re-seal job needs the schema of the tenant list")
	}
	for _, c := range columns {
		if c.Entity == "" || c.Table == "" || c.Column == "" || c.Select == "" || c.Update == "" || c.Left == "" {
			return nil, fmt.Errorf("command: the re-seal column %s.%s lacks a name or a statement", c.Entity, c.Column)
		}
	}
	r := &Reseal{db: db, schema: schema, ring: ring, columns: columns}
	r.tenants = func(ctx context.Context) ([]tenant.Tenant, error) { return tenant.List(ctx, db, schema) }
	return r, nil
}

// Run moves each sealed value of each column of each provisioned tenant that is not sealed with the first key of the
// ring to it, in the transaction of its tenant, a page at a time in ID order. It changes no other column, and writes
// no audit row and no event, because the value does not change. A value that does not open is logged with its
// column and row, never a value, and left as it is, so one bad row does not stop the column (security review of PR
// #441, L4). It logs, for each column, the values that it moved and those that are left on an earlier key.
func (r *Reseal) Run(ctx context.Context) ([]ResealResult, error) {
	tenants, err := r.tenants(ctx)
	if err != nil {
		return nil, err
	}
	prefix := sealed.Prefix(r.ring.First())
	results := make([]ResealResult, len(r.columns))
	var failed []error
	for i, c := range r.columns {
		results[i] = ResealResult{Entity: c.Entity, Column: c.Column}
		for _, t := range tenants {
			moved, left, bad, err := r.tenantColumn(ctx, t, c, prefix)
			results[i].Moved += moved
			results[i].Left += left
			results[i].Failed += bad
			if err != nil {
				failed = append(failed, err)
				if ctx.Err() != nil {
					return results, errors.Join(failed...)
				}
			}
		}
		slog.InfoContext(ctx, "command: re-seal of a secret column", "entity", c.Entity, "column", c.Column,
			"moved", results[i].Moved, "left", results[i].Left, "failed", results[i].Failed, "first_key", r.ring.First())
	}
	return results, errors.Join(failed...)
}

// tenantColumn moves the values of one column of one tenant, one page per transaction, until the last page; then it
// counts, with no lock, the values that are still on an earlier key. It gives the values that it moved, those that
// are left, and those of them that it read and that do not open. It logs the first maxFailedLogs rows that do not
// open, and then one count for the column and the tenant (security re-review of PR #441, N4).
func (r *Reseal) tenantColumn(ctx context.Context, t tenant.Tenant, c ResealColumn, prefix []byte) (moved, left, failed int64, err error) {
	last := firstID
	for {
		var p page
		err := tenant.Each(ctx, r.db, []tenant.Tenant{t}, func(ctx context.Context, tx tenant.Querier, t tenant.Tenant) error {
			var err error
			p, err = r.batch(ctx, tx, t, c, prefix, last, failed)
			return err
		})
		if err != nil {
			return moved, left, failed, fmt.Errorf("command: the re-seal of the column %s of %s for the tenant %s: %w", c.Column, c.Entity, t.ID(), unwrapEach(err))
		}
		moved += p.written
		failed += p.skipped
		if p.read < ResealBatch {
			break
		}
		last = p.last
	}
	if failed > maxFailedLogs {
		slog.WarnContext(ctx, "command: re-seal leaves values that do not open", "entity", c.Entity, "column", c.Column,
			"tenant", t.ID(), "failed", failed)
	}
	err = tenant.Each(ctx, r.db, []tenant.Tenant{t}, func(ctx context.Context, tx tenant.Querier, t tenant.Tenant) error {
		return tx.QueryRowContext(ctx, c.Left, t.ID(), prefix).Scan(&left)
	})
	if err != nil {
		return moved, left, failed, fmt.Errorf("command: the count of the column %s of %s for the tenant %s: %w", c.Column, c.Entity, t.ID(), unwrapEach(err))
	}
	return moved, left, failed, nil
}

// maxFailedLogs is the most rows that do not open that the re-seal job logs one by one for a column and a tenant in
// a run; it logs the rest as one count.
const maxFailedLogs = 3

// page is what one batch of the re-seal job did: the rows that it read, wrote and skipped, and the last ID it read.
type page struct {
	read, written, skipped int64
	last                   string
}

// batch moves at most ResealBatch values of one column of one tenant after the ID after, in its transaction.
func (r *Reseal) batch(ctx context.Context, tx tenant.Querier, t tenant.Tenant, c ResealColumn, prefix []byte, after string, failedBefore int64) (page, error) {
	var p page
	rows, err := tx.QueryContext(ctx, c.Select, t.ID(), prefix, ResealBatch, after)
	if err != nil {
		return p, err
	}
	type found struct {
		id    string
		value []byte
	}
	var all []found
	for rows.Next() {
		var f found
		if err := rows.Scan(&f.id, &f.value); err != nil {
			rows.Close()
			return p, err
		}
		all = append(all, f)
	}
	if err := rows.Close(); err != nil {
		return p, err
	}
	if err := rows.Err(); err != nil {
		return p, err
	}
	p.read = int64(len(all))
	for _, f := range all {
		p.last = f.id
		at := sealed.Place{Tenant: t.ID(), Table: c.Table, Column: c.Column, Row: f.id}
		plain, err := r.ring.Open(f.value, at)
		if err != nil {
			// The error names the column and the row, never a value.
			if failedBefore+p.skipped < maxFailedLogs {
				slog.WarnContext(ctx, "command: re-seal skips a value that does not open", "entity", c.Entity, "error", err.Error())
			}
			p.skipped++
			continue
		}
		again, err := r.ring.Seal(plain, at)
		clear(plain)
		if err != nil {
			return p, err
		}
		res, err := tx.ExecContext(ctx, c.Update, t.ID(), f.id, again, f.value)
		if err != nil {
			return p, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return p, err
		}
		p.written += n
	}
	return p, nil
}

// unwrapEach gives the error of the one tenant of a tenant.EachError.
func unwrapEach(err error) error {
	var each *tenant.EachError
	if errors.As(err, &each) && len(each.Failures) == 1 {
		return each.Failures[0].Err
	}
	return err
}
