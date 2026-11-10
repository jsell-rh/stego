package idempotency

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// The header names and the problem types of the Idempotency-Key header draft. The problem types are slugs: the
// service joins each to its declared problem base and writes the problem body in its own form.
const (
	// KeyHeader is the request header that carries the inbound key as a structured string (RFC 9651).
	KeyHeader = "Idempotency-Key"
	// ReplayedHeader is the response header that marks a replay of a stored response.
	ReplayedHeader = "Idempotent-Replayed"
	// ProblemMissing is the problem type of a 400 for an absent or malformed key.
	ProblemMissing = "idempotency-key-missing"
	// ProblemInProgress is the problem type of a 409 for a duplicate of a request that is not complete.
	ProblemInProgress = "idempotency-key-in-progress"
	// ProblemReused is the problem type of a 422 for a used key with another fingerprint.
	ProblemReused = "idempotency-key-reused"
	// MaxKeyLength is the longest key in characters that the key table keeps (ST-SUP-RP07-LC01).
	MaxKeyLength = 255
	// ConflictCode is the fixed code of the 409 that the command runtime answers after its last conflict
	// (ST-SUP-RC32-OC10). Wrap stores no response with this code.
	ConflictCode = "conflict"
)

// The errors of KeyFrom. Each one is a 400 with the problem type ProblemMissing.
var (
	// ErrKeyMissing is the error for a request with no Idempotency-Key to an operation that requires the key.
	ErrKeyMissing = errors.New("idempotency: the request has no Idempotency-Key header")
	// ErrKeyMalformed is the error for an Idempotency-Key header that is not one structured string of 1 to 255
	// characters.
	ErrKeyMalformed = errors.New("idempotency: the Idempotency-Key header is not a structured string of 1 to 255 characters")
)

// Tx is the part of the store's open write transaction that Wrap uses. SQLTx adapts a *sql.Tx to it; a caller that
// must run something before each statement, such as SET LOCAL lock_timeout (ST-SUP-RP10-LC28), wraps it. Rollback
// is in the interface so that only a transaction satisfies it, never a pool or a connection. Wrap does not call
// Rollback: the caller ends the transaction as Result.Commit says.
//
// The caller opens the transaction with the tenant set, at the isolation level READ COMMITTED, and sets lock_timeout
// in it before it calls Wrap (ST-SUP-RP10-LC28). Wrap expects READ COMMITTED: its SELECT ... FOR UPDATE NOWAIT must
// see a key row that another transaction committed after this one began.
type Tx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) Row
	Rollback() error
}

// Row is the result of Tx.QueryRowContext. *sql.Row satisfies it.
type Row interface {
	Scan(dest ...any) error
}

// SQLTx adapts a database/sql transaction to Tx.
func SQLTx(tx *sql.Tx) Tx { return sqlTx{tx} }

type sqlTx struct{ tx *sql.Tx }

func (t sqlTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, query, args...)
}

func (t sqlTx) QueryRowContext(ctx context.Context, query string, args ...any) Row {
	return t.tx.QueryRowContext(ctx, query, args...)
}

func (t sqlTx) Rollback() error { return t.tx.Rollback() }

// Request is what Wrap knows of one inbound request: the scope of the key, the key, and the request's
// Fingerprint.
type Request struct {
	Tenant      string
	Principal   string
	Key         string
	Fingerprint string
	// Retention is how long a completed key is kept; zero is DefaultRetention.
	Retention time.Duration
	// Clock gives the current time; nil is time.Now.
	Clock func() time.Time
}

// Response is a response that a handler gives and that Wrap stores and replays.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
	// Code is the fixed error code of a problem response, such as ConflictCode, or empty.
	Code string
}

// Result is the outcome of Wrap.
type Result struct {
	// Response is the response to send. When Problem is not empty, only its Status is set, and the caller writes
	// the problem body of that type in the service's own form.
	Response Response
	// Problem is ProblemInProgress or ProblemReused when Wrap answered without the handler, or empty.
	Problem string
	// Replayed is true when Response is a stored response; its header then holds Idempotent-Replayed: true.
	Replayed bool
	// Commit is true only when the handler ran and Wrap stored its response: the caller then commits the
	// transaction. Otherwise the caller rolls it back, so the key row and every write of the handler go.
	Commit bool
}

// Handler runs the guarded write in the transaction that the caller gave Wrap, and gives its response. An error
// from it comes back from Wrap as it is, and the caller rolls back.
type Handler func(ctx context.Context) (Response, error)

// StatementError is the error of a statement of Wrap itself that is not a lock failure or a cancel. It is an
// internal error, and never a conflict: it keeps the database error in Err but does not unwrap to it, so the
// command runtime does not see its SQLSTATE and does not run the request again (ST-SUP-RP10-LC25).
type StatementError struct {
	Step string
	Err  error
}

func (e *StatementError) Error() string {
	return fmt.Sprintf("idempotency: the %s statement failed: %v", e.Step, e.Err)
}

const (
	lockSQL   = `SELECT pg_try_advisory_xact_lock($1::bigint # 'idempotency_keys'::regclass::oid::bigint)`
	insertSQL = `INSERT INTO idempotency_keys (tenant_id, principal_id, key, fingerprint, status, locked_at, expires_at)
VALUES ($1, $2, $3, $4, 'in_progress', $5, $6)
ON CONFLICT DO NOTHING
RETURNING key`
	selectSQL = `SELECT fingerprint, status, response_code, response_headers, response_body
FROM idempotency_keys
WHERE tenant_id = $1 AND principal_id = $2 AND key = $3
FOR UPDATE NOWAIT`
	completeSQL = `UPDATE idempotency_keys
SET status = 'completed', response_code = $4, response_headers = $5, response_body = $6, expires_at = $7
WHERE tenant_id = $1 AND principal_id = $2 AND key = $3 AND status = 'in_progress'`
)

// Wrap runs handler once for the key of req, inside the caller's write transaction tx (ST-SUP-RP07-LC02,
// ST-SUP-RP07-LC03, ST-SUP-RP07-LC04).
//
// Before the handler runs, Wrap takes a transaction-scoped advisory lock on the key with no wait, and inserts the
// key row with status in_progress with INSERT ... ON CONFLICT DO NOTHING RETURNING. When the handler's response is
// not transient, Wrap writes status completed, the response and expires_at in the same transaction. A transient
// response (5xx, 429, or 409 with the code conflict) is not stored, and Commit is false, so the key row goes with
// the rollback.
//
// When the insert returns nothing, Wrap reads the row with SELECT ... FOR UPDATE NOWAIT and does not run the
// handler: a duplicate that holds the lock, a row that cannot be locked or a row in progress gets 409
// ProblemInProgress, another fingerprint gets 422 ProblemReused, and otherwise the stored response is replayed byte
// for byte with Idempotent-Replayed: true. A lock timeout (SQLSTATE 55P03) or a cancel during a statement of Wrap
// is also 409 ProblemInProgress, and any other database error of Wrap is a *StatementError.
func Wrap(ctx context.Context, tx Tx, req Request, handler Handler) (Result, error) {
	if err := req.check(); err != nil {
		return Result{}, err
	}
	clock := req.Clock
	if clock == nil {
		clock = time.Now
	}
	retention := req.Retention
	if retention <= 0 {
		retention = DefaultRetention
	}

	var locked bool
	if err := tx.QueryRowContext(ctx, lockSQL, req.lockID()).Scan(&locked); err != nil {
		return failure(ctx, "lock", err)
	}
	if !locked {
		return inProgress(), nil
	}

	start := clock()
	var inserted string
	err := tx.QueryRowContext(ctx, insertSQL, req.Tenant, req.Principal, req.Key, req.Fingerprint, start,
		start.Add(retention)).Scan(&inserted)
	if errors.Is(err, sql.ErrNoRows) {
		return existing(ctx, tx, req)
	}
	if err != nil {
		return failure(ctx, "insert", err)
	}

	resp, err := handler(ctx)
	if err != nil {
		return Result{}, err
	}
	if transient(resp) {
		return Result{Response: resp}, nil
	}
	body := resp.Body
	if body == nil {
		body = []byte{}
	}
	done := clock()
	res, err := tx.ExecContext(ctx, completeSQL, req.Tenant, req.Principal, req.Key, resp.Status,
		encodeHeader(resp.Header), body, done.Add(retention))
	if err != nil {
		return failure(ctx, "complete", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return Result{}, &StatementError{Step: "complete", Err: fmt.Errorf("%d rows changed, not 1 (%v)", n, err)}
	}
	return Result{Response: resp, Commit: true}, nil
}

// existing answers a request whose key row exists already.
func existing(ctx context.Context, tx Tx, req Request) (Result, error) {
	var (
		fingerprint, status string
		code                sql.NullInt64
		header              []byte
		body                []byte
	)
	err := tx.QueryRowContext(ctx, selectSQL, req.Tenant, req.Principal, req.Key).
		Scan(&fingerprint, &status, &code, &header, &body)
	if errors.Is(err, sql.ErrNoRows) {
		// The row went after the insert saw it, as when Purge removed it: the client can send the request again.
		return inProgress(), nil
	}
	if err != nil {
		return failure(ctx, "select", err)
	}
	if status != "completed" {
		return inProgress(), nil
	}
	if fingerprint != req.Fingerprint {
		return Result{Response: Response{Status: http.StatusUnprocessableEntity}, Problem: ProblemReused}, nil
	}
	h, err := decodeHeader(header)
	if err != nil {
		return Result{}, &StatementError{Step: "select", Err: err}
	}
	if body == nil {
		body = []byte{}
	}
	h.Set(ReplayedHeader, "true")
	return Result{Response: Response{Status: int(code.Int64), Header: h, Body: body}, Replayed: true}, nil
}

// failure maps an error of a statement of Wrap: a lock failure or a cancel is 409 ProblemInProgress, and anything
// else is a *StatementError.
func failure(ctx context.Context, step string, err error) (Result, error) {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		sqlState(err) == "55P03" {
		return inProgress(), nil
	}
	return Result{}, &StatementError{Step: step, Err: err}
}

func inProgress() Result {
	return Result{Response: Response{Status: http.StatusConflict}, Problem: ProblemInProgress}
}

// transient reports a response that Wrap must not store (ST-SUP-RP07-LC04).
func transient(resp Response) bool {
	return resp.Status >= 500 || resp.Status == http.StatusTooManyRequests ||
		(resp.Status == http.StatusConflict && resp.Code == ConflictCode)
}

// sqlState returns the SQLSTATE of a database error, as the PostgreSQL drivers give it, or empty.
func sqlState(err error) string {
	var coded interface{ SQLState() string }
	if errors.As(err, &coded) {
		return coded.SQLState()
	}
	return ""
}

func (req Request) check() error {
	switch {
	case req.Tenant == "":
		return errors.New("idempotency: the request has no tenant")
	case req.Principal == "":
		return errors.New("idempotency: the request has no principal")
	case req.Key == "" || len(req.Key) > MaxKeyLength:
		return ErrKeyMalformed
	case req.Fingerprint == "":
		return errors.New("idempotency: the request has no fingerprint")
	}
	return nil
}

// lockID is the advisory lock key of the request's key scope: the first 8 bytes of a SHA-256 over a fixed name and
// the tenant, the principal and the key, each prefixed by its length. lockSQL mixes in the OID of the key table that
// the search path resolves, so two schemas or services in one database never share a lock.
func (req Request) lockID() int64 {
	sum, err := sumParts([]byte("github.com/jsell-rh/stego/registry/runtime/idempotency"), []byte(req.Tenant), []byte(req.Principal),
		[]byte(req.Key))
	if err != nil {
		panic(err) // check keeps every part short.
	}
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

// KeyFrom reads the inbound key from the request header (ST-SUP-RP07-LC04). The header must be one RFC 9651
// structured string, such as "abc" with its quotes, of 1 to 255 characters, with no parameters. With no header,
// KeyFrom returns ErrKeyMissing when required is true and an empty key otherwise; a header that is not such a string
// is ErrKeyMalformed. The service answers either error with 400 and the problem type ProblemMissing.
func KeyFrom(header http.Header, required bool) (string, error) {
	values := header.Values(KeyHeader)
	if len(values) == 0 {
		if required {
			return "", ErrKeyMissing
		}
		return "", nil
	}
	if len(values) > 1 {
		return "", ErrKeyMalformed
	}
	key, ok := parseString(strings.Trim(values[0], " \t"))
	if !ok || key == "" || len(key) > MaxKeyLength {
		return "", ErrKeyMalformed
	}
	return key, nil
}

// parseString parses an RFC 9651 sf-string that fills all of s.
func parseString(s string) (string, bool) {
	if len(s) < 2 || s[0] != '"' {
		return "", false
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\':
			i++
			if i >= len(s) || (s[i] != '"' && s[i] != '\\') {
				return "", false
			}
			b.WriteByte(s[i])
		case c == '"':
			if i != len(s)-1 {
				return "", false
			}
			return b.String(), true
		case c < 0x20 || c > 0x7e:
			return "", false
		default:
			b.WriteByte(c)
		}
	}
	return "", false
}

// encodeHeader writes h losslessly as bytes: the count of names, and for each name in sorted order the name and the
// count of its values, then each value. Each count and each string has a 4-byte big-endian length prefix, so any
// byte, obs-text and NUL included, comes back as it was.
func encodeHeader(h http.Header) []byte {
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	sort.Strings(names)
	var out bytes.Buffer
	putUint32(&out, len(names))
	for _, name := range names {
		putString(&out, name)
		putUint32(&out, len(h[name]))
		for _, v := range h[name] {
			putString(&out, v)
		}
	}
	return out.Bytes()
}

func putUint32(out *bytes.Buffer, n int) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(n))
	out.Write(b[:])
}

func putString(out *bytes.Buffer, s string) {
	putUint32(out, len(s))
	out.WriteString(s)
}

// decodeHeader reads the bytes that encodeHeader writes. No bytes is no header.
func decodeHeader(data []byte) (http.Header, error) {
	h := http.Header{}
	if len(data) == 0 {
		return h, nil
	}
	r := headerReader{data: data}
	names := r.uint32()
	for i := uint32(0); i < names && r.err == nil; i++ {
		name := r.string()
		count := r.uint32()
		if r.err == nil && uint64(count)*4 > uint64(len(r.data)) {
			r.err = errors.New("a value count is longer than the data")
		}
		values := make([]string, 0, count)
		for j := uint32(0); j < count && r.err == nil; j++ {
			values = append(values, r.string())
		}
		h[name] = values
	}
	if r.err == nil && len(r.data) != 0 {
		r.err = errors.New("bytes are left after the last header")
	}
	if r.err != nil {
		return nil, fmt.Errorf("the stored headers are not valid: %w", r.err)
	}
	return h, nil
}

type headerReader struct {
	data []byte
	err  error
}

func (r *headerReader) uint32() uint32 {
	if r.err != nil {
		return 0
	}
	if len(r.data) < 4 {
		r.err = errors.New("the data ends inside a length")
		return 0
	}
	n := binary.BigEndian.Uint32(r.data)
	r.data = r.data[4:]
	return n
}

func (r *headerReader) string() string {
	n := r.uint32()
	if r.err != nil {
		return ""
	}
	if uint64(n) > uint64(len(r.data)) {
		r.err = errors.New("the data ends inside a string")
		return ""
	}
	s := string(r.data[:n])
	r.data = r.data[n:]
	return s
}
