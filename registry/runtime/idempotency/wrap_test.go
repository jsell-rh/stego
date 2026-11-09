package idempotency

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder is a Tx that keeps the text of every statement in order.
type recorder struct {
	tx    *sql.Tx
	mu    sync.Mutex
	stmts []string
}

func (r *recorder) add(q string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stmts = append(r.stmts, q)
}

func (r *recorder) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	r.add(q)
	return r.tx.ExecContext(ctx, q, args...)
}

func (r *recorder) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	r.add(q)
	return r.tx.QueryRowContext(ctx, q, args...)
}

// handlerFunc is a test handler that gets the write transaction.
type handlerFunc func(ctx context.Context, tx Tx) (Response, error)

// runWrap opens a write transaction, calls Wrap in it, and commits when the result says so; otherwise it rolls back.
func runWrap(ctx context.Context, db *sql.DB, req Request, h handlerFunc) (Result, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, err
	}
	res, err := Wrap(ctx, tx, req, func(ctx context.Context) (Response, error) { return h(ctx, tx) })
	if err != nil || !res.Commit {
		_ = tx.Rollback()
		return res, err
	}
	return res, tx.Commit()
}

func request(key, fingerprint string) Request {
	return Request{Tenant: "t1", Principal: "p1", Key: key, Fingerprint: fingerprint}
}

func writeOrder(ctx context.Context, tx Tx, id string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO orders (id, note) VALUES ($1, 'x')`, id)
	return err
}

// created is a handler that writes the order id and answers 201 with a header and a body.
func created(id string, runs *int) handlerFunc {
	return func(ctx context.Context, tx Tx) (Response, error) {
		*runs++
		if err := writeOrder(ctx, tx, id); err != nil {
			return Response{}, err
		}
		h := http.Header{}
		h.Set("Location", "/v1/orders/"+id)
		h.Add("X-Multi", "a")
		h.Add("X-Multi", "b")
		return Response{Status: 201, Header: h, Body: []byte(`{"id": "` + id + `" }` + "\n")}, nil
	}
}

func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestWrap(t *testing.T) {
	ctx := context.Background()

	t.Run("ST-SUP-RP07-LC02-T01 Wrap inserts the key row before the handler runs, in the write transaction", func(t *testing.T) {
		db, _ := testDB(t)
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		rec := &recorder{tx: tx}
		const handlerSQL = `SELECT status FROM idempotency_keys WHERE tenant_id = $1 AND principal_id = $2 AND key = $3`
		var seen string
		res, err := Wrap(ctx, rec, request("k1", "f1"), func(ctx context.Context) (Response, error) {
			if err := rec.QueryRowContext(ctx, handlerSQL, "t1", "p1", "k1").Scan(&seen); err != nil {
				return Response{}, err
			}
			return Response{Status: 201}, nil
		})
		if err != nil || !res.Commit {
			t.Fatalf("Wrap = %+v, %v", res, err)
		}
		if seen != "in_progress" {
			t.Fatalf("the handler saw status %q, want in_progress", seen)
		}
		insert, handler := -1, -1
		for i, q := range rec.stmts {
			if insert < 0 && strings.HasPrefix(q, "INSERT INTO idempotency_keys") &&
				strings.Contains(q, "ON CONFLICT DO NOTHING") && strings.Contains(q, "RETURNING") {
				insert = i
			}
			if handler < 0 && q == handlerSQL {
				handler = i
			}
		}
		if insert < 0 || handler < 0 || insert > handler {
			t.Fatalf("statements %q: the key insert (%d) must come before the handler's first statement (%d)",
				rec.stmts, insert, handler)
		}
	})

	t.Run("ST-SUP-RP07-LC02-T02 Wrap completes the key row in the same transaction", func(t *testing.T) {
		db, _ := testDB(t)
		t0 := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
		clock := []time.Time{t0, t0.Add(3 * time.Second)}
		req := request("k1", "f1")
		req.Clock = func() time.Time { c := clock[0]; clock = clock[1:]; return c }
		runs := 0
		res, err := runWrap(ctx, db, req, created("o1", &runs))
		if err != nil || !res.Commit {
			t.Fatalf("Wrap = %+v, %v", res, err)
		}
		var (
			status, header string
			code           int
			body           []byte
			expires        time.Time
			keyXmin        string
		)
		if err := db.QueryRowContext(ctx, `SELECT status, response_code, response_headers::text, response_body,
			expires_at, xmin::text FROM idempotency_keys WHERE key = 'k1'`).
			Scan(&status, &code, &header, &body, &expires, &keyXmin); err != nil {
			t.Fatal(err)
		}
		stored, err := decodeHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if status != "completed" || code != 201 || !bytes.Equal(body, res.Response.Body) ||
			!sameHeader(stored, res.Response.Header) {
			t.Fatalf("key row = %s %d %s %q, want completed 201 and the response", status, code, header, body)
		}
		if want := t0.Add(3 * time.Second).Add(24 * time.Hour); !expires.Equal(want) {
			t.Fatalf("expires_at = %v, want %v", expires, want)
		}
		var orderXmin string
		if err := db.QueryRowContext(ctx, `SELECT xmin::text FROM orders WHERE id = 'o1'`).Scan(&orderXmin); err != nil {
			t.Fatal(err)
		}
		if orderXmin != keyXmin {
			t.Fatalf("the key row (xmin %s) and the order row (xmin %s) are in two commits", keyXmin, orderXmin)
		}
	})

	t.Run("ST-SUP-RP07-LC02-T03 A crash before the commit leaves no key row and no write", func(t *testing.T) {
		db, admin := testDB(t)
		_, err := runWrap(ctx, db, request("k1", "f1"), func(ctx context.Context, tx Tx) (Response, error) {
			if err := writeOrder(ctx, tx, "o1"); err != nil {
				return Response{}, err
			}
			var pid int
			if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				return Response{}, err
			}
			if _, err := admin.ExecContext(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
				return Response{}, err
			}
			return Response{Status: 201}, nil
		})
		if err == nil {
			t.Fatal("the transaction committed after its session was terminated")
		}
		if n := countRows(t, db, `SELECT count(*) FROM idempotency_keys`); n != 0 {
			t.Fatalf("%d key rows after the crash, want 0", n)
		}
		if n := countRows(t, db, `SELECT count(*) FROM orders`); n != 0 {
			t.Fatalf("%d order rows after the crash, want 0", n)
		}
	})

	t.Run("ST-SUP-RP07-LC03-T01 A duplicate of a request in progress gets 409", func(t *testing.T) {
		db, _ := testDB(t)
		entered, release := make(chan struct{}), make(chan struct{})
		firstDone := make(chan error, 1)
		go func() {
			_, err := runWrap(ctx, db, request("k1", "f1"), func(ctx context.Context, tx Tx) (Response, error) {
				close(entered)
				<-release
				return Response{Status: 201}, writeOrder(ctx, tx, "o1")
			})
			firstDone <- err
		}()
		<-entered
		// No lock_timeout is set: a key insert that waited for the first request would wait until the timeout.
		waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		ran := false
		start := time.Now()
		res, err := runWrap(waitCtx, db, request("k1", "f1"), func(context.Context, Tx) (Response, error) {
			ran = true
			return Response{Status: 201}, nil
		})
		took := time.Since(start)
		close(release)
		if err := <-firstDone; err != nil {
			t.Fatalf("the first request: %v", err)
		}
		if err != nil || res.Response.Status != 409 || res.Problem != ProblemInProgress || ran {
			t.Fatalf("duplicate = %+v, %v, handler ran %v; want 409 %s", res, err, ran, ProblemInProgress)
		}
		if took > time.Second {
			t.Fatalf("the duplicate took %v: it waited for a lock", took)
		}
	})

	t.Run("ST-SUP-RP07-LC03-T02 A key row with status in_progress and no lock gets 409", func(t *testing.T) {
		db, _ := testDB(t)
		if err := insertKey(ctx, db, "t1", "p1", "k1", "in_progress"); err != nil {
			t.Fatal(err)
		}
		ran := false
		res, err := runWrap(ctx, db, request("k1", "f"), func(context.Context, Tx) (Response, error) {
			ran = true
			return Response{Status: 201}, nil
		})
		if err != nil || res.Response.Status != 409 || res.Problem != ProblemInProgress || ran {
			t.Fatalf("Wrap = %+v, %v, handler ran %v; want 409 %s", res, err, ran, ProblemInProgress)
		}
	})

	t.Run("ST-SUP-RP07-LC03-T03 A changed body with a used key gets 422", func(t *testing.T) {
		db, _ := testDB(t)
		runs := 0
		if _, err := runWrap(ctx, db, request("k1", "f1"), created("o1", &runs)); err != nil {
			t.Fatal(err)
		}
		res, err := runWrap(ctx, db, request("k1", "f2"), created("o2", &runs))
		if err != nil || res.Response.Status != 422 || res.Problem != ProblemReused || runs != 1 {
			t.Fatalf("Wrap = %+v, %v, %d runs; want 422 %s and one run", res, err, runs, ProblemReused)
		}
	})

	t.Run("ST-SUP-RP07-LC03-T04 A matching request replays the stored response byte for byte", func(t *testing.T) {
		db, _ := testDB(t)
		runs := 0
		first, err := runWrap(ctx, db, request("k1", "f1"), created("o1", &runs))
		if err != nil {
			t.Fatal(err)
		}
		again, err := runWrap(ctx, db, request("k1", "f1"), created("o1", &runs))
		if err != nil {
			t.Fatal(err)
		}
		if runs != 1 || !again.Replayed || again.Response.Status != first.Response.Status ||
			!bytes.Equal(again.Response.Body, first.Response.Body) || !sameHeader(again.Response.Header, first.Response.Header) {
			t.Fatalf("replay = %+v after %d runs, want %+v once", again, runs, first)
		}
		if got := again.Response.Header.Values(ReplayedHeader); len(got) != 1 || got[0] != "true" {
			t.Fatalf("Idempotent-Replayed = %q, want true", got)
		}
	})

	t.Run("ST-SUP-RP07-LC04-T01 A 5xx, 429 or conflict response stores no key row", func(t *testing.T) {
		db, _ := testDB(t)
		for i, failed := range []Response{{Status: 500}, {Status: 503}, {Status: 429}, {Status: 409, Code: ConflictCode}} {
			key := fmt.Sprintf("k%d", i)
			res, err := runWrap(ctx, db, request(key, "f1"), func(ctx context.Context, tx Tx) (Response, error) {
				return failed, writeOrder(ctx, tx, key)
			})
			if err != nil || res.Commit || res.Response.Status != failed.Status {
				t.Fatalf("%d: Wrap = %+v, %v; want the response and no commit", failed.Status, res, err)
			}
			if n := countRows(t, db, `SELECT count(*) FROM idempotency_keys WHERE key = $1`, key); n != 0 {
				t.Fatalf("%d: %d key rows, want 0", failed.Status, n)
			}
			if n := countRows(t, db, `SELECT count(*) FROM orders WHERE id = $1`, key); n != 0 {
				t.Fatalf("%d: %d order rows, want 0", failed.Status, n)
			}
			runs := 0
			res, err = runWrap(ctx, db, request(key, "f1"), created(key, &runs))
			if err != nil || runs != 1 || res.Replayed || res.Response.Status != 201 {
				t.Fatalf("%d: retry = %+v, %v, %d runs; want 201 from one run", failed.Status, res, err, runs)
			}
			if n := countRows(t, db, `SELECT count(*) FROM idempotency_keys WHERE key = $1 AND response_code = 201`, key); n != 1 {
				t.Fatalf("%d: the retry stored %d rows with 201, want 1", failed.Status, n)
			}
		}
	})

	t.Run("ST-SUP-RP07-LC07-T03 The default retention is 24 hours", func(t *testing.T) {
		db, _ := testDB(t)
		t0 := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
		done := t0.Add(1500 * time.Millisecond)
		calls := 0
		req := request("k1", "f1")
		req.Clock = func() time.Time {
			calls++
			if calls == 1 {
				return t0
			}
			return done
		}
		runs := 0
		if _, err := runWrap(ctx, db, req, created("o1", &runs)); err != nil {
			t.Fatal(err)
		}
		var expires time.Time
		if err := db.QueryRowContext(ctx, `SELECT expires_at FROM idempotency_keys WHERE key = 'k1'`).Scan(&expires); err != nil {
			t.Fatal(err)
		}
		if want := done.Add(24 * time.Hour); !expires.Equal(want) {
			t.Fatalf("expires_at = %v, want %v", expires, want)
		}
	})
}

// noStatement is a Tx that fails the test on any statement: a service that refuses a request before Wrap writes no
// key row.
type noStatement struct{ t *testing.T }

func (n noStatement) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	n.t.Error("a statement ran")
	return nil, errors.New("no statement expected")
}

func (n noStatement) QueryRowContext(context.Context, string, ...any) *sql.Row {
	n.t.Error("a statement ran")
	return &sql.Row{}
}

// keyService is a service with one operation that requires the key, as generated code binds it: it reads the key
// with KeyFrom, answers an error with a 400 problem, and calls Wrap otherwise.
func keyService(t *testing.T, tx Tx, ran *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, err := KeyFrom(r.Header, true)
		if err != nil {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"type":"https://errors.example.com/orders/%s","status":400,"detail":%q}`, ProblemMissing, err.Error())
			return
		}
		body, _ := io.ReadAll(r.Body)
		fp, err := Fingerprint(r.Method, r.URL.Path, body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		res, err := Wrap(r.Context(), tx, Request{Tenant: "t1", Principal: "p1", Key: key, Fingerprint: fp},
			func(context.Context) (Response, error) {
				*ran = true
				return Response{Status: 201}, nil
			})
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(res.Response.Status)
	})
}

func TestKeyFrom(t *testing.T) {
	t.Run("ST-SUP-RP07-LC04-T02 An absent key on a key-requiring operation gets 400", func(t *testing.T) {
		ran := false
		srv := httptest.NewServer(keyService(t, noStatement{t}, &ran))
		defer srv.Close()
		resp, err := http.Post(srv.URL+"/v1/orders", "application/json", strings.NewReader(`{"item":"book"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 400 || resp.Header.Get("Content-Type") != "application/problem+json" ||
			!strings.Contains(string(body), ProblemMissing) || ran {
			t.Fatalf("got %d %s %s, handler ran %v; want a 400 problem and no run", resp.StatusCode,
				resp.Header.Get("Content-Type"), body, ran)
		}
	})

	t.Run("ST-SUP-RP07-LC04-T03 A key that is not a structured string gets 400", func(t *testing.T) {
		ran := false
		srv := httptest.NewServer(keyService(t, noStatement{t}, &ran))
		defer srv.Close()
		for _, value := range []string{`abc`, `1`, `"abc`, `"abc"x`, `""`, `"` + strings.Repeat("k", 256) + `"`} {
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/orders", strings.NewReader(`{}`))
			req.Header.Set(KeyHeader, value)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != 400 || ran {
				t.Fatalf("%s: got %d, handler ran %v; want 400 and no run", value, resp.StatusCode, ran)
			}
		}
		for value, want := range map[string]string{`"abc"`: "abc", ` "a\"b\\c" `: `a"b\c`} {
			h := http.Header{}
			h.Set(KeyHeader, value)
			if got, err := KeyFrom(h, true); err != nil || got != want {
				t.Fatalf("KeyFrom(%s) = %q, %v; want %q", value, got, err, want)
			}
		}
		h := http.Header{}
		h.Add(KeyHeader, `"a"`)
		h.Add(KeyHeader, `"b"`)
		if _, err := KeyFrom(h, true); !errors.Is(err, ErrKeyMalformed) {
			t.Fatalf("two Idempotency-Key fields: err = %v, want ErrKeyMalformed", err)
		}
		if got, err := KeyFrom(http.Header{}, false); err != nil || got != "" {
			t.Fatalf("no key on an operation that does not require it = %q, %v; want no key and no error", got, err)
		}
	})
}
