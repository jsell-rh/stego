package idempotency

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// wrapper is a transaction wrapper with the shape of Wrap; the broken ones below each leave out one duty.
type wrapper func(ctx context.Context, db *sql.DB, tx *sql.Tx, req Request, h Handler) (Result, error)

func reference(ctx context.Context, _ *sql.DB, tx *sql.Tx, req Request, h Handler) (Result, error) {
	return Wrap(ctx, tx, req, h)
}

// noReplay runs the handler again on a matching request.
func noReplay(ctx context.Context, _ *sql.DB, tx *sql.Tx, req Request, h Handler) (Result, error) {
	res, err := Wrap(ctx, tx, req, h)
	if err == nil && res.Replayed {
		resp, err := h(ctx)
		return Result{Response: resp, Commit: true}, err
	}
	return res, err
}

// noFingerprint does not compare fingerprints.
func noFingerprint(ctx context.Context, _ *sql.DB, tx *sql.Tx, req Request, h Handler) (Result, error) {
	req.Fingerprint = "one fingerprint for every body"
	return Wrap(ctx, tx, req, h)
}

// noLock does not claim the key row before the handler: it looks for a stored row, runs the handler, and inserts
// the completed row at the end.
func noLock(ctx context.Context, _ *sql.DB, tx *sql.Tx, req Request, h Handler) (Result, error) {
	var (
		code int
		body []byte
	)
	err := tx.QueryRowContext(ctx, `SELECT response_code, response_body FROM idempotency_keys
		WHERE tenant_id = $1 AND principal_id = $2 AND key = $3 AND status = 'completed'`,
		req.Tenant, req.Principal, req.Key).Scan(&code, &body)
	if err == nil {
		return Result{Response: Response{Status: code, Body: body, Header: http.Header{ReplayedHeader: {"true"}}},
			Replayed: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Result{}, err
	}
	resp, err := h(ctx)
	if err != nil {
		return Result{}, err
	}
	now := time.Now()
	_, err = tx.ExecContext(ctx, `INSERT INTO idempotency_keys
		(tenant_id, principal_id, key, fingerprint, status, response_code, response_body, locked_at, expires_at)
		VALUES ($1, $2, $3, $4, 'completed', $5, $6, $7, $8) ON CONFLICT DO NOTHING`,
		req.Tenant, req.Principal, req.Key, req.Fingerprint, resp.Status, resp.Body, now, now.Add(time.Hour))
	return Result{Response: resp, Commit: true}, err
}

// ownTransaction writes the key row in transactions of its own, outside the write transaction.
func ownTransaction(ctx context.Context, db *sql.DB, _ *sql.Tx, req Request, h Handler) (Result, error) {
	now := time.Now()
	if _, err := db.ExecContext(ctx, `INSERT INTO idempotency_keys
		(tenant_id, principal_id, key, fingerprint, status, locked_at, expires_at)
		VALUES ($1, $2, $3, $4, 'in_progress', $5, $6) ON CONFLICT DO NOTHING`,
		req.Tenant, req.Principal, req.Key, req.Fingerprint, now, now.Add(time.Hour)); err != nil {
		return Result{}, err
	}
	resp, err := h(ctx)
	if err != nil {
		return Result{}, err
	}
	_, err = db.ExecContext(ctx, `UPDATE idempotency_keys SET status = 'completed', response_code = $4,
		response_body = $5 WHERE tenant_id = $1 AND principal_id = $2 AND key = $3`,
		req.Tenant, req.Principal, req.Key, resp.Status, resp.Body)
	return Result{Response: resp, Commit: true}, err
}

// orderStore is a store that binds a wrapper, as a generated store does: its handler writes one order row in the
// write transaction and answers 201.
func orderStore(db, admin *sql.DB, wrap wrapper) Subject {
	do := func(ctx context.Context, call Call) (Response, error) {
		fp, err := Fingerprint(http.MethodPost, "/v1/orders", call.Body)
		if err != nil {
			return Response{}, err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return Response{}, err
		}
		req := Request{Tenant: "t1", Principal: "p1", Key: call.Key, Fingerprint: fp}
		res, err := wrap(ctx, db, tx, req, func(ctx context.Context) (Response, error) {
			if _, err := tx.ExecContext(ctx, `INSERT INTO orders (id, note) VALUES (gen_random_uuid()::text, $1)`,
				call.Key); err != nil {
				return Response{}, err
			}
			if call.Inside != nil {
				if err := call.Inside(ctx); err != nil {
					return Response{}, err
				}
			}
			if call.Crash {
				var pid int
				if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
					return Response{}, err
				}
				if _, err := admin.ExecContext(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
					return Response{}, err
				}
			}
			return Response{Status: http.StatusCreated, Header: http.Header{"Content-Type": {"application/json"}},
				Body: []byte(`{"key":"` + call.Key + `"}`)}, nil
		})
		if err != nil || !res.Commit {
			_ = tx.Rollback()
			return res.Response, err
		}
		if err := tx.Commit(); err != nil {
			return Response{}, err
		}
		return res.Response, nil
	}
	return Subject{
		Do: do,
		KeyRows: func(ctx context.Context, key string) (int, error) {
			var n int
			err := db.QueryRowContext(ctx, `SELECT count(*) FROM idempotency_keys
				WHERE tenant_id = 't1' AND principal_id = 'p1' AND key = $1`, key).Scan(&n)
			return n, err
		},
		Body:      []byte(`{"item":"book","qty":1}`),
		OtherBody: []byte(`{"item":"book","qty":2}`),
		Wait:      2 * time.Second,
	}
}

// failed returns the reason of the case in failures, and whether it failed.
func failed(failures []Failure, name string) (string, bool) {
	for _, f := range failures {
		if f.Case == name {
			return f.Reason, true
		}
	}
	return "", false
}

func TestConform(t *testing.T) {
	ctx := context.Background()

	t.Run("ST-SUP-RP07-LC08-T01 The suite fails a wrapper that does not replay", func(t *testing.T) {
		db, admin := testDB(t)
		if failures := Conform(ctx, orderStore(db, admin, reference)); len(failures) != 0 {
			t.Fatalf("the reference Wrap failed: %v", failures)
		}
		if _, ok := failed(Conform(ctx, orderStore(db, admin, noReplay)), CaseReplay); !ok {
			t.Fatal("a wrapper that runs the handler again passed the replay case")
		}
	})

	t.Run("ST-SUP-RP07-LC08-T02 The suite fails a wrapper that accepts a changed body", func(t *testing.T) {
		db, admin := testDB(t)
		reason, ok := failed(Conform(ctx, orderStore(db, admin, noFingerprint)), CaseReuse)
		if !ok || !strings.Contains(reason, "422 expected") {
			t.Fatalf("reuse case = %q, %v; want a failure with 422 expected", reason, ok)
		}
	})

	t.Run("ST-SUP-RP07-LC08-T03 The suite fails a wrapper that lets two concurrent duplicates run", func(t *testing.T) {
		db, admin := testDB(t)
		reason, ok := failed(Conform(ctx, orderStore(db, admin, noLock)), CaseRace)
		if !ok || !strings.Contains(reason, "409 expected") {
			t.Fatalf("race case = %q, %v; want a failure with 409 expected", reason, ok)
		}
	})

	t.Run("ST-SUP-RP07-LC08-T04 The suite fails a wrapper that keeps a key after a crash", func(t *testing.T) {
		db, admin := testDB(t)
		reason, ok := failed(Conform(ctx, orderStore(db, admin, ownTransaction)), CaseCrash)
		if !ok || !strings.Contains(reason, "key row") {
			t.Fatalf("crash case = %q, %v; want a failure for a key row left", reason, ok)
		}
	})
}
