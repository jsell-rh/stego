package command

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"stego.dev/runtime/audit"
	"stego.dev/runtime/idempotency"
)

// runTriggers are the deferred constraint triggers of the COMMIT cases on reservations: a SKU fail-commit raises
// 40001 at the first COMMIT only, sleep-commit sleeps at COMMIT, and wait-commit waits for the row lock of
// commit_locks 1 with a lock timeout of its own.
const runTriggers = `
CREATE SEQUENCE commit_once;
CREATE TABLE commit_locks (id int PRIMARY KEY);
INSERT INTO commit_locks VALUES (1);
CREATE FUNCTION at_commit() RETURNS trigger LANGUAGE plpgsql SET lock_timeout = '30s' AS $$
BEGIN
  IF NEW.sku = 'fail-commit' AND nextval('commit_once') = 1 THEN
    RAISE EXCEPTION 'first commit' USING ERRCODE = '40001';
  ELSIF NEW.sku = 'sleep-commit' THEN
    PERFORM pg_sleep(3);
  ELSIF NEW.sku = 'wait-commit' THEN
    PERFORM 1 FROM commit_locks WHERE id = 1 FOR UPDATE;
  END IF;
  RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER reservations_at_commit AFTER INSERT ON reservations DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION at_commit();
`

func (e *env) keyRows(key string) int {
	e.t.Helper()
	return e.count(`SELECT count(*) FROM idempotency_keys WHERE key = $1`, key)
}

// bumpVersion moves the version of a user row from another transaction, so the handle's next write of it conflicts.
func (e *env) bumpVersion(id string) {
	e.t.Helper()
	e.exec(`UPDATE users SET version = version + 1 WHERE id = '` + id + `'`)
}

// conflictOn makes the handle meet a conflict on the user id: it reads the row, moves its version from another
// transaction, and writes it.
func (e *env) conflictOn(ctx context.Context, h *Handle[testProbe], id string) error {
	r, err := Repo(h, e.users)
	if err != nil {
		return err
	}
	u, err := r.Get(ctx, id)
	if err != nil {
		return err
	}
	e.bumpVersion(id)
	_, err = r.Update(ctx, u, u.Value())
	return err
}

func TestRunLifetime(t *testing.T) {
	e := newEnv(t)
	e.seedOrg("t1", "o1", 0)
	cmd := e.command("reserve", nil, e.reservationE, e.chargeE)
	create := func(ctx context.Context, h *Handle[testProbe], id string) error {
		_, err := repo(t, h, e.reservationE).Create(ctx, reservation{ID: id, OrderID: "o1", SKU: "s"}, nil)
		return err
	}

	t.Run("ST-SUP-RP10-LC09-T02 Run commits after a fill that returns no error", func(t *testing.T) {
		if _, err := e.run(cmd, e.request("t1", u7(t), "k-lc09", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			return create(ctx, h, "r-lc09")
		})); err != nil {
			t.Fatal(err)
		}
		var commits int
		if err := e.admin.QueryRowContext(e.ctx, `SELECT count(DISTINCT x) FROM (
 SELECT xmin::text AS x FROM reservations WHERE id = 'r-lc09'
 UNION ALL SELECT xmin::text FROM audit_event WHERE resource_id = 'r-lc09'
 UNION ALL SELECT xmin::text FROM idempotency_keys WHERE key = 'k-lc09') s`).Scan(&commits); err != nil {
			t.Fatal(err)
		}
		if commits != 1 || e.keyRows("k-lc09") != 1 || e.count(`SELECT count(*) FROM audit_event WHERE resource_id = 'r-lc09'`) != 1 {
			t.Fatalf("the Reservation, its audit row and the key row are in %d commits, want one", commits)
		}
	})

	t.Run("ST-SUP-RP10-LC10-T01 A kept handle does nothing after the commit", func(t *testing.T) {
		e.exec(`INSERT INTO charges (tenant_id, id, amount, card_last4) VALUES ('t1', 'c-kept', 1, '0000')`)
		var kept *Handle[testProbe]
		var keptRepo *Repository[reservation]
		var keptCharge Loaded[charge]
		if _, err := e.run(cmd, e.request("t1", u7(t), "k-lc10", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			kept, keptRepo = h, repo(t, h, e.reservationE)
			var err error
			keptCharge, err = repo(t, h, e.chargeE).Get(ctx, "c-kept")
			return err
		})); err != nil {
			t.Fatal(err)
		}
		e.decision.taken()
		before := e.count(`SELECT count(*) FROM stego_outbox.messages`)
		if _, err := Repo(kept, e.reservationE); !errors.Is(err, ErrClosed) {
			t.Errorf("Repo on the kept handle gave %v", err)
		}
		if _, err := keptRepo.Create(e.ctx, reservation{ID: "r-kept", OrderID: "o1", SKU: "s"}, nil); !errors.Is(err, ErrClosed) {
			t.Errorf("a write on the kept handle gave %v", err)
		}
		if _, err := kept.Authz().Evaluate(e.ctx, "reserve", keptCharge, nil); !errors.Is(err, ErrClosed) {
			t.Errorf("an evaluation on the kept handle gave %v", err)
		}
		if err := kept.Effects().Stage(e.ctx, "billing", "charge-requested", keptCharge, []byte(`{}`)); !errors.Is(err, ErrClosed) {
			t.Errorf("an effect on the kept handle gave %v", err)
		}
		if e.count(`SELECT count(*) FROM reservations WHERE id = 'r-kept'`) != 0 || len(e.decision.taken()) != 0 ||
			e.count(`SELECT count(*) FROM stego_outbox.messages`) != before {
			t.Fatal("a call on the kept handle did something")
		}
	})

	t.Run("ST-SUP-RP10-LC10-T02 A kept handle does nothing after the rollback", func(t *testing.T) {
		var kept *Repository[reservation]
		if _, err := e.run(cmd, e.request("t1", u7(t), "k-lc10-rb", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			kept = repo(t, h, e.reservationE)
			return errors.New("refused")
		})); err == nil {
			t.Fatal("the run committed")
		}
		if _, err := kept.Create(e.ctx, reservation{ID: "r-kept-rb", OrderID: "o1", SKU: "s"}, nil); !errors.Is(err, ErrClosed) {
			t.Fatalf("a write on the kept handle gave %v", err)
		}
		if e.count(`SELECT count(*) FROM reservations WHERE id = 'r-kept-rb'`) != 0 {
			t.Fatal("the write made a row")
		}
	})

	t.Run("ST-SUP-RP10-LC10-T03 A goroutine of the fill cannot write after the fill returns", func(t *testing.T) {
		returned, wrote := make(chan struct{}), make(chan struct{})
		rt := e.runtime(nil)
		rt.beforeCommit = func(context.Context, *sql.Tx) error {
			close(returned)
			<-wrote
			return nil
		}
		var goroutineErr error
		_, err := e.runOn(rt, cmd, e.request("t1", u7(t), "k-lc10-go", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			go func() {
				<-returned
				r, err := Repo(h, e.reservationE)
				if err == nil {
					_, err = r.Create(ctx, reservation{ID: "r-goroutine", OrderID: "o1", SKU: "s"}, nil)
				}
				goroutineErr = err
				close(wrote)
			}()
			return create(ctx, h, "r-fill")
		}))
		if err != nil {
			t.Fatal(err)
		}
		if !errors.Is(goroutineErr, ErrClosed) {
			t.Fatalf("the goroutine's write gave %v, want ErrClosed", goroutineErr)
		}
		if e.count(`SELECT count(*) FROM reservations WHERE id = 'r-goroutine'`) != 0 || e.count(`SELECT count(*) FROM reservations WHERE id = 'r-fill'`) != 1 {
			t.Fatal("the commit holds the goroutine's row, or not the fill's")
		}
	})

	t.Run("ST-SUP-RP10-LC11-T01 A replay of a completed command does not call the fill", func(t *testing.T) {
		calls := 0
		fill := func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
			calls++
			if err := create(ctx, h, "r-replay"); err != nil {
				return idempotency.Response{}, err
			}
			return idempotency.Response{Status: 201, Body: []byte(`{"id":"r-replay","n":1}`)}, nil
		}
		first, err := e.run(cmd, e.request("t1", u7(t), "k1", `{"sku":"s"}`), fill)
		if err != nil {
			t.Fatal(err)
		}
		second, err := e.run(cmd, e.request("t1", u7(t), "k1", `{"sku":"s"}`), fill)
		if err != nil {
			t.Fatal(err)
		}
		if calls != 1 || !second.Replayed || second.Response.Status != first.Response.Status ||
			!bytes.Equal(second.Response.Body, first.Response.Body) {
			t.Fatalf("%d fill calls; the second answer %+v, the first %+v", calls, second, first)
		}
		if n := e.count(`SELECT count(*) FROM reservations WHERE id = 'r-replay'`); n != 1 {
			t.Fatalf("%d Reservations", n)
		}
	})

	t.Run("ST-SUP-RP10-LC11-T02 A failed command leaves no key row", func(t *testing.T) {
		calls := 0
		_, err := e.run(cmd, e.request("t1", u7(t), "k-failed", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			calls++
			if err := create(ctx, h, "r-failed"); err != nil {
				return err
			}
			return errors.New("refused")
		}))
		if err == nil || e.keyRows("k-failed") != 0 || e.count(`SELECT count(*) FROM reservations WHERE id = 'r-failed'`) != 0 {
			t.Fatalf("the failed call gave %v and left a row", err)
		}
		if _, err := e.run(cmd, e.request("t1", u7(t), "k-failed", `{}`), fillOf(func(context.Context, *Handle[testProbe]) error {
			calls++
			return nil
		})); err != nil {
			t.Fatal(err)
		}
		if calls != 2 {
			t.Fatalf("the fill ran %d times, want again on the second call", calls)
		}
	})

	t.Run("ST-SUP-RP10-LC12-T01 A fill panic rolls back and hides the panic value", func(t *testing.T) {
		outbox := e.count(`SELECT count(*) FROM stego_outbox.messages`)
		audits := e.auditRows("t1", "")
		_, err := e.run(cmd, e.request("t1", u7(t), "k-panic", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			r, err := repo(t, h, e.reservationE).Create(ctx, reservation{ID: "r-panic", OrderID: "o1", SKU: "s"}, nil)
			if err != nil {
				return err
			}
			if err := h.Effects().Stage(ctx, "billing", "reservation-made", r, []byte(`{}`)); err != nil {
				return err
			}
			panic("secret-42")
		}))
		if err == nil || strings.Contains(err.Error(), "secret-42") || !errors.Is(err, ErrInternal) {
			t.Fatalf("the panic gave %v", err)
		}
		if e.count(`SELECT count(*) FROM reservations WHERE id = 'r-panic'`) != 0 || e.keyRows("k-panic") != 0 ||
			e.count(`SELECT count(*) FROM stego_outbox.messages`) != outbox || e.auditRows("t1", "") != audits {
			t.Fatal("the panic left a row")
		}
	})

	t.Run("ST-SUP-RP10-LC12-T02 A fill that ends through runtime.Goexit is treated as a panic", func(t *testing.T) {
		// runtime.Goexit, as t.FailNow calls it, skips recover: the attempt still closes the handle and rolls back.
		var kept *Repository[reservation]
		var runErr error
		var res Result
		done := make(chan struct{})
		go func() {
			defer close(done)
			res, runErr = e.run(cmd, e.request("t1", u7(t), "k-goexit", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
				kept = repo(t, h, e.reservationE)
				if _, err := kept.Create(ctx, reservation{ID: "r-goexit", OrderID: "o1", SKU: "s"}, nil); err != nil {
					return err
				}
				runtime.Goexit()
				return nil
			}))
		}()
		<-done
		if !errors.Is(runErr, ErrInternal) || res.Response.Status != 0 {
			t.Fatalf("Run of a fill that ended through Goexit gave %+v, %v; want no result and an internal error", res, runErr)
		}
		if _, err := kept.Create(e.ctx, reservation{ID: "r-goexit-2", OrderID: "o1", SKU: "s"}, nil); !errors.Is(err, ErrClosed) {
			t.Fatalf("the handle of a fill that ended through Goexit gave %v", err)
		}
		if e.count(`SELECT count(*) FROM reservations WHERE id LIKE 'r-goexit%'`) != 0 || e.keyRows("k-goexit") != 0 ||
			e.pool.Stats().InUse != 0 {
			t.Fatal("the fill that ended through Goexit left a row or a connection in use")
		}
	})
}

func TestRunConflicts(t *testing.T) {
	e := newEnv(t)
	e.seedOrg("t1", "o1", 0)
	for _, id := range []string{"u1", "u2", "u3", "u4"} {
		e.seedUser("t1", id, "o1", false, 0, 1)
	}
	if _, err := e.admin.ExecContext(e.ctx, runTriggers); err != nil {
		t.Fatal(err)
	}
	e.exec("GRANT USAGE ON SEQUENCE commit_once TO "+e.roles[0], "GRANT SELECT, UPDATE ON commit_locks TO "+e.roles[0])
	retry := func(n int) func(*CommandSpec[testProbe]) {
		return func(s *CommandSpec[testProbe]) { s.RetryOnConflict = n }
	}

	t.Run("ST-SUP-RP10-LC24-T03 A loaded value from an earlier attempt or a stale value of the same transaction is an internal error", func(t *testing.T) {
		cmd := e.command("keep-value", retry(1), e.users)
		var kept Loaded[user]
		calls := 0
		res, err := e.run(cmd, e.request("t1", u7(t), "k-kept", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			calls++
			r := repo(t, h, e.users)
			if calls == 1 {
				var err error
				if kept, err = r.Get(ctx, "u1"); err != nil {
					return err
				}
				return e.conflictOn(ctx, h, "u2")
			}
			v := kept.Value()
			v.Rank = 99
			_, err := r.Update(ctx, kept, v)
			return err
		}))
		if !errors.Is(err, ErrInternal) || errors.Is(err, ErrConflict) || calls != 2 || res.Attempts != 2 {
			t.Fatalf("the kept value gave %v after %d calls, want an internal error after the rerun and no other", err, calls)
		}
		res, err = e.run(e.command("stale-value", retry(1), e.users), e.request("t1", u7(t), "k-stale", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			r := repo(t, h, e.users)
			first, err := r.Get(ctx, "u1")
			if err != nil {
				return err
			}
			v := first.Value()
			v.Rank = 5
			if _, err := r.Update(ctx, first, v); err != nil {
				return err
			}
			v.Rank = 6
			_, err = r.Update(ctx, first, v)
			return err
		}))
		if !errors.Is(err, ErrInternal) || res.Attempts != 1 {
			t.Fatalf("the stale value gave %v after %d attempts, want an internal error and no rerun", err, res.Attempts)
		}
		if e.count(`SELECT count(*) FROM users WHERE id = 'u1' AND rank = 0 AND version = 1`) != 1 ||
			e.auditRows("t1", "keep-value")+e.auditRows("t1", "stale-value") != 0 {
			t.Fatal("a refused write changed u1 or left an audit row")
		}
	})

	t.Run("ST-SUP-RP10-LC25-T01 A conflict reruns the fill up to the declared count in new transactions", func(t *testing.T) {
		rt := e.runtime(nil)
		var waits []time.Duration
		rt.rerun = func(_ int, wait, _ time.Duration) { waits = append(waits, wait) }
		type call struct {
			h  *Handle[testProbe]
			tx *sql.Tx
		}
		var calls []call
		fill := func(conflictUntil int) func(context.Context, *Handle[testProbe]) (idempotency.Response, error) {
			return fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
				calls = append(calls, call{h, h.st.tx})
				if conflictUntil < 0 || len(calls) <= conflictUntil {
					return e.conflictOn(ctx, h, "u3")
				}
				_, err := repo(t, h, e.users).Create(ctx, user{ID: "u-lc25", OrgID: "o1", Email: "lc25@example.com"}, nil)
				return err
			})
		}
		res, err := e.runOn(rt, e.command("retry-two", retry(2), e.users), e.request("t1", u7(t), "k-lc25-a", `{}`), fill(1))
		if err != nil || len(calls) != 2 || res.Attempts != 2 || e.keyRows("k-lc25-a") != 1 ||
			e.count(`SELECT count(*) FROM users WHERE id = 'u-lc25'`) != 1 {
			t.Fatalf("one conflict: %v after %d calls", err, len(calls))
		}
		if calls[0].h == calls[1].h || calls[0].tx == calls[1].tx {
			t.Fatal("the rerun used the handle or the transaction of the first call")
		}
		if len(waits) != 1 || waits[0] > RerunStep {
			t.Fatalf("the waits were %v, want one of at most 50 ms", waits)
		}
		calls, waits = nil, nil
		outbox, audits := e.count(`SELECT count(*) FROM stego_outbox.messages`), e.auditRows("t1", "")
		_, err = e.runOn(rt, e.command("retry-two", retry(2), e.users), e.request("t1", u7(t), "k-lc25-b", `{}`), fill(-1))
		if !errors.Is(err, ErrConflict) || len(calls) != 3 || e.keyRows("k-lc25-b") != 0 ||
			e.count(`SELECT count(*) FROM stego_outbox.messages`) != outbox || e.auditRows("t1", "") != audits {
			t.Fatalf("a conflict on every call: %v after %d calls", err, len(calls))
		}
		if len(waits) != 2 || waits[0] > RerunStep || waits[1] > 2*RerunStep {
			t.Fatalf("the waits were %v, want at most 50 ms and 100 ms", waits)
		}
		calls = nil
		_, err = e.runOn(rt, e.command("retry-none", retry(0), e.users), e.request("t1", u7(t), "k-lc25-c", `{}`), fill(-1))
		if !errors.Is(err, ErrConflict) || len(calls) != 1 {
			t.Fatalf("Retry on conflict 0: %v after %d calls", err, len(calls))
		}
	})

	t.Run("ST-SUP-RP10-LC25-T02 A fill that ignores a conflict cannot commit", func(t *testing.T) {
		cmd := e.command("ignore-conflict", retry(0), e.users, e.chargeE)
		outbox := e.count(`SELECT count(*) FROM stego_outbox.messages`)
		_, err := e.run(cmd, e.request("t1", u7(t), "k-ignore", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			r := repo(t, h, e.users)
			u4, err := r.Get(ctx, "u4")
			if err != nil {
				return err
			}
			c, err := r.Get(ctx, "u3")
			if err != nil {
				return err
			}
			_ = e.conflictOn(ctx, h, "u3")
			v := u4.Value()
			v.Rank = 44
			if _, err := r.Update(ctx, u4, v); !errors.Is(err, ErrConflict) {
				t.Errorf("the update after the conflict gave %v", err)
			}
			if err := h.Effects().Stage(ctx, "billing", "charge-requested", c, []byte(`{}`)); !errors.Is(err, ErrConflict) {
				t.Errorf("the effect after the conflict gave %v", err)
			}
			return nil
		}))
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("Run gave %v, want the conflict", err)
		}
		if e.count(`SELECT count(*) FROM users WHERE id = 'u4' AND rank = 44`) != 0 || e.keyRows("k-ignore") != 0 ||
			e.count(`SELECT count(*) FROM stego_outbox.messages`) != outbox {
			t.Fatal("the ignored conflict committed something")
		}
	})

	t.Run("ST-SUP-RP10-LC25-T03 A real deadlock between two fills is a conflict, and the victim runs again", func(t *testing.T) {
		before := e.deadlocks()
		var barrier sync.WaitGroup
		barrier.Add(2)
		type outcome struct {
			res   Result
			err   error
			calls int
		}
		done := make(chan outcome, 2)
		for i, order := range [][2]string{{"u1", "u2"}, {"u2", "u1"}} {
			go func() {
				calls := 0
				res, err := e.run(e.command(fmt.Sprintf("deadlock-%d", i), retry(1), e.users),
					e.request("t1", u7(t), fmt.Sprintf("k-deadlock-%d", i), `{}`),
					fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
						calls++
						r := repo(t, h, e.users)
						if _, err := r.GetForUpdate(ctx, order[0]); err != nil {
							return err
						}
						if calls == 1 {
							barrier.Done()
							barrier.Wait()
						}
						_, err := r.GetForUpdate(ctx, order[1])
						return err
					}))
				done <- outcome{res, err, calls}
			}()
		}
		calls := 0
		for range 2 {
			o := <-done
			if o.err != nil || o.res.Response.Status != 200 {
				t.Fatalf("a command gave %v with %d", o.err, o.res.Response.Status)
			}
			calls += o.calls
		}
		if calls != 3 || e.deadlocks()-before != 1 {
			t.Fatalf("%d fill calls and %d deadlocks, want 3 and one 40P01", calls, e.deadlocks()-before)
		}
	})

	t.Run("ST-SUP-RP10-LC25-T04 A conflict at COMMIT runs the fill again, and a lock wait of the wrapper is no conflict", func(t *testing.T) {
		cmd := e.command("reserve-at-commit", retry(1), e.reservationE, e.chargeE)
		e.exec(`INSERT INTO charges (tenant_id, id, amount, card_last4) VALUES ('t1', 'c-rerun', 1, '0000')`)
		calls := 0
		res, err := e.run(cmd, e.request("t1", u7(t), "k-commit-40001", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			calls++
			// Each attempt reads a pii entity: the rerun's read row must not wait on, or be refused for, the audit
			// record of the attempt that rolled back.
			if _, err := repo(t, h, e.chargeE).Get(ctx, "c-rerun"); err != nil {
				return err
			}
			_, err := repo(t, h, e.reservationE).Create(ctx, reservation{ID: "r-fail-commit", OrderID: "o1", SKU: "fail-commit"}, nil)
			return err
		}))
		if err != nil || calls != 2 || res.Attempts != 2 || e.keyRows("k-commit-40001") != 1 ||
			e.count(`SELECT count(*) FROM audit_event WHERE resource_id = 'r-fail-commit'`) != 1 {
			t.Fatalf("the 40001 at COMMIT: %v after %d calls", err, calls)
		}
		// Session A holds the in-progress key row of k1, so the key insert waits until its lock timeout.
		a := e.session()
		fingerprint, _ := idempotency.Fingerprint("POST", "/commands", []byte(`{}`))
		a.exec(`INSERT INTO idempotency_keys (tenant_id, principal_id, key, fingerprint, status, locked_at, expires_at)
 VALUES ('t1', 'user:u-7', 'k1', $1, 'in_progress', now(), now() + interval '1 hour')`, fingerprint)
		calls = 0
		res, err = e.run(cmd, e.request("t1", u7(t), "k1", `{}`), fillOf(func(context.Context, *Handle[testProbe]) error {
			calls++
			return nil
		}))
		a.commit()
		if err != nil || res.Problem != idempotency.ProblemInProgress || res.Response.Status != 409 || calls != 0 || res.Attempts != 1 {
			t.Fatalf("the waiting key insert gave %+v, %v after %d calls; want 409 idempotency-key-in-progress", res, err, calls)
		}
	})

	t.Run("ST-SUP-RP10-LC25-T06 Reruns stop at the deadline, decode the request again and emit per attempt", func(t *testing.T) {
		recorder := newObservations(t, "deactivate-user")
		rt := e.runtime(func(c *Config) { c.Probes = recorder.probes })
		cmd := e.command("deactivate-user", retry(3), e.users)
		type request struct{ Items []string }
		decode := func(body []byte) (request, error) {
			var r request
			return r, json.Unmarshal(body, &r)
		}
		var seen [][]string
		fill := func(conflictAlways bool) Fill[request, testProbe] {
			return func(ctx context.Context, req request, h *Handle[testProbe]) (idempotency.Response, error) {
				seen = append(seen, append([]string(nil), req.Items...))
				// The fill changes its request in place: a rerun must not see the change.
				req.Items[0] = "changed"
				req.Items = append(req.Items, "added")
				h.Probe().emit(recorder.refused)
				h.Probe().emit(recorder.deactivated)
				if conflictAlways || len(seen) == 1 {
					time.Sleep(50 * time.Millisecond)
					return idempotency.Response{}, e.conflictOn(ctx, h, "u4")
				}
				return ok, nil
			}
		}
		ctx, cancel := context.WithTimeout(e.ctx, 120*time.Millisecond)
		_, err := Run(ctx, rt, cmd, e.request("t1", u7(t), "k-deadline", `{"items":["a"]}`), decode, fill(true))
		cancel()
		if !errors.Is(err, ErrConflict) || len(seen) >= 4 {
			t.Fatalf("with a deadline of 120 ms: %v after %d calls", err, len(seen))
		}
		seen = nil
		before := len(recorder.probes.All())
		ctx, cancel = context.WithTimeout(e.ctx, 5*time.Second)
		_, err = Run(ctx, rt, cmd, e.request("t1", u7(t), "k-deadline-2", `{"items":["a"]}`), decode, fill(false))
		cancel()
		if err != nil || len(seen) != 2 || strings.Join(seen[1], ",") != "a" {
			t.Fatalf("one conflict: %v; the calls saw %v", err, seen)
		}
		emissions := recorder.probes.All()[before:]
		var outcomes []string
		deactivated := 0
		for _, em := range emissions {
			switch em.Name {
			case "deactivation_refused":
				outcomes = append(outcomes, em.Outcome)
			case "user_deactivated":
				deactivated++
			}
		}
		if strings.Join(outcomes, ",") != "rolled_back,committed" || deactivated != 1 {
			t.Fatalf("the recorder holds deactivation_refused %v and user_deactivated %d times", outcomes, deactivated)
		}
	})

	t.Run("ST-SUP-RP10-LC28-T01 A blocked lock ends as a conflict at the lock timeout or before the deadline", func(t *testing.T) {
		a := e.session()
		a.exec(`SELECT 1 FROM users WHERE id = 'u1' FOR UPDATE`)
		defer a.commit()
		rt := e.runtime(nil)
		var lefts []time.Duration
		rt.rerun = func(_ int, _, left time.Duration) { lefts = append(lefts, left) }
		lock := fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			_, err := repo(t, h, e.users).GetForUpdate(ctx, "u1")
			return err
		})
		check := func(name string, err error, took, from, to time.Duration) {
			t.Helper()
			if !errors.Is(err, ErrConflict) || took < from || took > to {
				t.Errorf("%s: %v after %s, want a conflict after %s to %s", name, err, took, from, to)
			}
			if waits := e.lockWaits(); waits != 0 || e.pool.Stats().InUse != 0 {
				t.Errorf("%s: %d sessions wait for a lock and %d connections are in use after the call", name, waits, e.pool.Stats().InUse)
			}
		}
		timed := func(deadline time.Duration, cmd *Command[testProbe], key string, fill func(context.Context, *Handle[testProbe]) (idempotency.Response, error)) (error, time.Duration) {
			ctx, cancel := context.WithTimeout(e.ctx, deadline)
			defer cancel()
			start := time.Now()
			_, err := Run(ctx, rt, cmd, e.request("t1", u7(t), key, `{}`), noBody,
				func(ctx context.Context, _ struct{}, h *Handle[testProbe]) (idempotency.Response, error) {
					return fill(ctx, h)
				})
			return err, time.Since(start)
		}
		err, took := timed(time.Minute, e.command("lock-200", func(s *CommandSpec[testProbe]) { s.LockTimeout = 200 * time.Millisecond }, e.users), "k-28-1", lock)
		check("a Lock timeout of 200 ms", err, took, 190*time.Millisecond, 600*time.Millisecond)
		err, took = timed(500*time.Millisecond, e.command("lock-default", nil, e.users), "k-28-2", lock)
		check("a deadline of 500 ms", err, took, 400*time.Millisecond, 500*time.Millisecond)
		var lockStart time.Time
		err, took = timed(time.Second, e.command("lock-late", nil, e.users), "k-28-3", fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			time.Sleep(700 * time.Millisecond)
			lockStart = time.Now()
			_, err := repo(t, h, e.users).GetForUpdate(ctx, "u1")
			return err
		}))
		check("a lock wait after 700 ms of a deadline of 1 s", err, took, 900*time.Millisecond, time.Second)
		if waited := time.Since(lockStart); waited < 200*time.Millisecond || waited > 320*time.Millisecond {
			t.Errorf("the late lock waited %s, want about 250 ms", waited)
		}
		calls := 0
		err, took = timed(250*time.Millisecond, e.command("slow-conflict", retry(2), e.users), "k-28-4", fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			calls++
			time.Sleep(160 * time.Millisecond)
			return e.conflictOn(ctx, h, "u2")
		}))
		if !errors.Is(err, ErrConflict) || calls != 1 {
			t.Errorf("a conflict with 90 ms left: %v after %d calls, want the conflict and no rerun", err, calls)
		}
		lefts, calls = nil, 0
		var starts []time.Duration
		deadline := time.Now().Add(400 * time.Millisecond)
		err, _ = timed(400*time.Millisecond, e.command("fast-conflict", retry(3), e.users), "k-28-5", fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			calls++
			starts = append(starts, time.Until(deadline))
			return e.conflictOn(ctx, h, "u2")
		}))
		if !errors.Is(err, ErrConflict) {
			t.Errorf("conflicts on every attempt gave %v", err)
		}
		for i, left := range lefts {
			if left < RerunRoom {
				t.Errorf("the rerun %d started with %s left after its wait", i+1, left)
			}
		}
		for i, left := range starts {
			if left <= 0 {
				t.Errorf("the attempt %d started after the deadline", i+1)
			}
		}
		if len(starts) != len(lefts)+1 {
			t.Errorf("%d attempts and %d reruns", len(starts), len(lefts))
		}
		w, err := NewWorker(WorkerSpec[testProbe]{Name: "lock-worker", Permission: "sync", Interval: time.Minute, Writes: []Declared{e.users}, Probe: newTestProbe})
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		err = RunWorker(e.ctx, rt, w, time.Unix(1793966400, 0), mustTenant(t, "t1"), nil, func(ctx context.Context, _ time.Time, h *Handle[testProbe]) error {
			_, err := repo(t, h, e.users).GetForUpdate(ctx, "u1")
			return err
		})
		check("a worker tick", err, time.Since(start), 1900*time.Millisecond, 2600*time.Millisecond)

		// The time left is checked again after the random wait: a wait that ends with less than 100 ms left starts no
		// rerun. The hook of the test runs after the wait and uses up the room.
		late := e.runtime(nil)
		late.rerun = func(_ int, _, left time.Duration) { time.Sleep(left - 60*time.Millisecond) }
		calls = 0
		ctx, cancel := context.WithTimeout(e.ctx, 600*time.Millisecond)
		_, err = Run(ctx, late, e.command("late-rerun", retry(3), e.users), e.request("t1", u7(t), "k-28-late", `{}`), noBody,
			func(ctx context.Context, _ struct{}, h *Handle[testProbe]) (idempotency.Response, error) {
				calls++
				return idempotency.Response{}, e.conflictOn(ctx, h, "u2")
			})
		cancel()
		if !errors.Is(err, ErrConflict) || calls != 1 {
			t.Errorf("a wait that left less than 100 ms: %v after %d calls, want the conflict and no rerun", err, calls)
		}

		// lock_timeout is set again before each statement of the wrapper, the enqueue, the audit record and COMMIT, to
		// the time left before the deadline less 50 ms. The hook reads it and lets 100 ms pass, so a value that was
		// not set again is too large at the next step.
		refreshed := e.runtime(nil)
		deadline = time.Now().Add(1500 * time.Millisecond)
		seen := map[string]int{}
		refreshed.step = func(name string, tx *sql.Tx) {
			var setting string
			if err := tx.QueryRowContext(e.ctx, `SELECT current_setting('lock_timeout')`).Scan(&setting); err != nil {
				t.Errorf("%s: read lock_timeout: %v", name, err)
				return
			}
			got, err := time.ParseDuration(setting)
			want := time.Until(deadline) - LockMargin
			if err != nil || got > want+40*time.Millisecond || got < want-40*time.Millisecond {
				t.Errorf("before %s, lock_timeout is %s, want about %s", name, setting, want)
			}
			seen[name]++
			time.Sleep(100 * time.Millisecond)
		}
		ctx, cancel = context.WithDeadline(e.ctx, deadline)
		_, err = Run(ctx, refreshed, e.command("refresh", nil, e.users), e.request("t1", u7(t), "k-28-refresh", `{}`), noBody,
			func(ctx context.Context, _ struct{}, h *Handle[testProbe]) (idempotency.Response, error) {
				r := repo(t, h, e.users)
				u, err := r.Get(ctx, "u4")
				if err != nil {
					return idempotency.Response{}, err
				}
				v := u.Value()
				v.Rank++
				if u, err = r.Update(ctx, u, v); err != nil {
					return idempotency.Response{}, err
				}
				err = h.Effects().Stage(ctx, "billing", "user-ranked", u, []byte(`{}`))
				// Time passes after the fill's last statement, so a value that the enqueue did not set again is stale.
				time.Sleep(150 * time.Millisecond)
				return ok, err
			})
		cancel()
		if err != nil || seen["wrapper"] < 2 || seen["enqueue"] != 1 || seen["record"] != 1 || seen["commit"] != 1 {
			t.Errorf("the run gave %v and passed the steps %v", err, seen)
		}

		// A cancel just before COMMIT, after its lock_timeout is set, ends tenant.Commit before COMMIT is sent: a
		// conflict, not an outcome that is not known.
		canceled := e.runtime(nil)
		ctx, cancel = context.WithCancel(e.ctx)
		canceled.step = func(name string, _ *sql.Tx) {
			if name == "commit" {
				cancel()
			}
		}
		_, err = Run(ctx, canceled, e.command("cancel-before", nil, e.users), e.request("t1", u7(t), "k-28-cancel", `{}`), noBody,
			func(ctx context.Context, _ struct{}, h *Handle[testProbe]) (idempotency.Response, error) {
				return ok, nil
			})
		if !errors.Is(err, ErrConflict) || errors.Is(err, ErrCommitUnknown) {
			t.Errorf("a cancel before COMMIT gave %v, want a conflict", err)
		}
	})
}

func TestRunCommit(t *testing.T) {
	e := newEnv(t)
	e.seedOrg("t1", "o1", 0)
	if _, err := e.admin.ExecContext(e.ctx, runTriggers); err != nil {
		t.Fatal(err)
	}
	e.exec("GRANT USAGE ON SEQUENCE commit_once TO "+e.roles[0], "GRANT SELECT, UPDATE ON commit_locks TO "+e.roles[0])

	t.Run("ST-SUP-RP10-LC25-T05 A COMMIT with an unknown outcome is not run again and is audited as failed", func(t *testing.T) {
		proxy := newPGProxy(t, e.dsn(e.roles[0]))
		proxied := e.openPool(proxy.dsn)
		rt := e.runtime(func(c *Config) { c.DB = proxied })
		cmd := e.command("reserve-unknown", func(s *CommandSpec[testProbe]) { s.RetryOnConflict = 2 }, e.reservationE)
		reserve := func(id, sku string, calls *int) Fill[struct{}, testProbe] {
			return func(ctx context.Context, _ struct{}, h *Handle[testProbe]) (idempotency.Response, error) {
				*calls++
				_, err := repo(t, h, e.reservationE).Create(ctx, reservation{ID: id, OrderID: "o1", SKU: sku}, nil)
				return idempotency.Response{Status: 201, Body: []byte(`{"id":"` + id + `"}`)}, err
			}
		}
		cases := []struct {
			name, id, sku string
			deadline      time.Duration
			during        func()
			took          bool
		}{
			{name: "the connection ends before the commit takes effect", id: "r-before", sku: "sleep-commit", deadline: time.Minute,
				during: func() { e.terminateAtCommit(t) }},
			{name: "the connection ends after the commit took effect", id: "r-after", sku: "s", deadline: time.Minute,
				during: func() {}, took: true},
			{name: "the deadline passes while COMMIT waits for a lock", id: "r-deadline", sku: "wait-commit", deadline: 1500 * time.Millisecond,
				during: func() {}},
		}
		for _, c := range cases {
			key := "k-unknown-" + c.id
			req := e.request("t1", u7(t), key, `{}`)
			var a *session
			if c.sku == "wait-commit" {
				a = e.session()
				a.exec(`SELECT 1 FROM commit_locks WHERE id = 1 FOR UPDATE`)
			}
			if c.took {
				proxy.dropCommitReply.Store(true)
			}
			go c.during()
			calls := 0
			ctx, cancel := context.WithTimeout(e.ctx, c.deadline)
			proxy.commitPID.Store(0)
			_, err := Run(ctx, rt, cmd, req, noBody, reserve(c.id, c.sku, &calls))
			cancel()
			proxy.dropCommitReply.Store(false)
			// The cancel of pgx and the end of a connection reach the server after Run returns. The outcome of the
			// COMMIT is fixed only once the backend that ran it is idle or has ended, so the test waits for that first,
			// and only then lets session A go and reads the rows.
			e.waitBackendDone(t, int(proxy.commitPID.Load()))
			if a != nil {
				a.commit()
			}
			if !errors.Is(err, ErrCommitUnknown) || !errors.Is(err, ErrInternal) || errors.Is(err, ErrConflict) || calls != 1 {
				t.Fatalf("%s: Run gave %v after %d calls, want an internal error after one call", c.name, err, calls)
			}
			// The generated handler writes the outcome row in its own short transaction.
			if err := rt.RecordOutcome(e.ctx, req, "reserve-unknown", "reservation", c.id, audit.OutcomeFailed, ErrorCode(err)); err != nil {
				t.Fatal(err)
			}
			if n := e.count(`SELECT count(*) FROM audit_event WHERE request_id = $1 AND outcome = 'failed' AND error_code = 'commit_outcome_unknown'`, req.RequestID); n != 1 {
				t.Fatalf("%s: %d outcome rows commit_outcome_unknown, want 1", c.name, n)
			}
			// Whether the commit took effect is the server's: each case accepts either outcome, and checks the retry
			// against the one that the rows show.
			took := e.count(`SELECT count(*) FROM reservations WHERE id = $1`, c.id) == 1
			ownRow := e.count(`SELECT count(*) FROM audit_event WHERE resource_id = $1 AND outcome = 'ok' AND action = 'reserve-unknown'`, c.id)
			if (ownRow == 1) != took {
				t.Fatalf("%s: %d rows with the outcome ok, and the commit took effect: %t", c.name, ownRow, took)
			}
			calls = 0
			res, err := Run(e.ctx, rt, cmd, req, noBody, reserve(c.id, "s", &calls))
			if err != nil {
				t.Fatalf("%s: the request sent again gave %v", c.name, err)
			}
			if took && (!res.Replayed || calls != 0) {
				t.Fatalf("%s: the commit took effect, and the request sent again ran the fill %d times, want a replay", c.name, calls)
			}
			if !took && (res.Replayed || calls != 1) {
				t.Fatalf("%s: the commit did not take effect, and the request sent again ran the fill %d times, want once", c.name, calls)
			}
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE outcome NOT IN ('ok', 'denied', 'failed')`); n != 0 {
			t.Fatalf("%d audit rows with another outcome", n)
		}
	})

	t.Run("ST-SUP-RP10-LC29-T01 Read audits finish while fills hold every main connection, and a blocked audit write fails the read", func(t *testing.T) {
		pools := newEnv(t, func(c *envConfig) { c.mainConns, c.auditConns = 2, 2 })
		pools.seedOrg("t1", "o1", 0)
		for _, id := range []string{"a1", "a2"} {
			pools.seedUser("t1", id, "o1", false, 0, 1)
		}
		pools.exec(`INSERT INTO charges (tenant_id, id, amount, card_last4) VALUES ('t1', 'c1', 1, '4242')`)
		cmd := pools.command("lock-and-read", nil, pools.users, pools.chargeE)
		var holding sync.WaitGroup
		holding.Add(2)
		release := make(chan struct{})
		var ended [2]time.Time
		var thirdStart time.Time
		errs := make(chan error, 3)
		start := time.Now()
		for i, id := range []string{"a1", "a2"} {
			go func() {
				_, err := pools.run(cmd, pools.request("t1", u7(t), "k-29-"+id, `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
					if _, err := repo(t, h, pools.users).GetForUpdate(ctx, id); err != nil {
						return err
					}
					holding.Done()
					<-release
					_, err := repo(t, h, pools.chargeE).Get(ctx, "c1")
					ended[i] = time.Now()
					return err
				}))
				errs <- err
			}()
		}
		holding.Wait()
		go func() {
			_, err := pools.run(cmd, pools.request("t1", u7(t), "k-29-third", `{}`), fillOf(func(context.Context, *Handle[testProbe]) error {
				thirdStart = time.Now()
				return nil
			}))
			errs <- err
		}()
		time.Sleep(50 * time.Millisecond)
		close(release)
		for range 3 {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}
		// The third request waits for a main connection until one of the two fills ends and its command commits.
		if time.Since(start) > 5*time.Second || (thirdStart.Before(ended[0]) && thirdStart.Before(ended[1])) {
			t.Fatalf("the run took %s; the third request started at %s, the two fills ended at %v", time.Since(start), thirdStart, ended)
		}
		if n := pools.count(`SELECT count(*) FROM audit_event WHERE action = 'read' AND resource_id = 'c1'`); n != 2 {
			t.Fatalf("%d read rows, want 2", n)
		}
		a := pools.session()
		a.exec(`LOCK TABLE audit_event IN SHARE MODE`)
		var readErr error
		var readTook time.Duration
		_, err := pools.run(cmd, pools.request("t1", u7(t), "k-29-blocked", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			if _, err := repo(t, h, pools.users).GetForUpdate(ctx, "a1"); err != nil {
				return err
			}
			begin := time.Now()
			var got Loaded[charge]
			got, readErr = repo(t, h, pools.chargeE).Get(ctx, "c1")
			readTook = time.Since(begin)
			if got != nil {
				t.Error("the read whose audit row failed gave a result")
			}
			return readErr
		}))
		a.commit()
		if readErr == nil || err == nil || readTook > 2500*time.Millisecond || pools.keyRows("k-29-blocked") != 0 {
			t.Fatalf("the blocked read audit gave %v after %s, and Run %v", readErr, readTook, err)
		}
		// The audit connections are a pool of their own, of at least two connections.
		single := pools.openPool(pools.dsn(pools.roles[0]))
		single.SetMaxOpenConns(1)
		for name, auditDB := range map[string]*sql.DB{"the main pool": pools.pool, "a pool of one connection": single} {
			cfg := Config{DB: pools.pool, AuditDB: auditDB, Authz: pools.authz, Audit: pools.writer, Outbox: pools.queue,
				Probes: pools.probes, Service: "orders"}
			if _, err := New(pools.ctx, cfg); err == nil {
				t.Errorf("New accepted %s as the audit connections", name)
			}
		}
		if pools.auditDB.Stats().InUse != 0 {
			t.Fatalf("%d audit connections are in use", pools.auditDB.Stats().InUse)
		}
	})

	t.Run("ST-SUP-RP10-LC30-T01 The command runtime commits only through tenant.Commit", func(t *testing.T) {
		for _, call := range commitCalls(t) {
			t.Errorf("%s calls Commit on a transaction", call)
		}
		// A statement of the generated code that sets app.tenant to another tenant with is_local false.
		moveSpec := e.reservationE.spec
		moveSpec.Name = "Moved"
		moveSpec.Select = `SELECT id, order_id, sku, version, authz_token FROM reservations WHERE tenant_id = $1 AND id = $2
 AND pg_catalog.set_config('app.tenant', 't2', false) IS NOT NULL`
		moved, err := NewEntity(moveSpec)
		if err != nil {
			t.Fatal(err)
		}
		e.exec(`INSERT INTO reservations (tenant_id, id, order_id, sku) VALUES ('t1', 'r-anchor', 'o1', 's')`)
		cmd := e.command("move-tenant", nil, e.reservationE, moved)
		for _, write := range []bool{true, false} {
			key := fmt.Sprintf("k-move-%t", write)
			_, err := e.run(cmd, e.request("t1", u7(t), key, `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
				if write {
					if err := create(t, ctx, e, h, "r-moved"); err != nil {
						return err
					}
				}
				_, err := repo(t, h, moved).Get(ctx, "r-anchor")
				return err
			}))
			if !errors.Is(err, ErrInternal) || e.keyRows(key) != 0 || e.count(`SELECT count(*) FROM reservations WHERE id = 'r-moved'`) != 0 {
				t.Fatalf("the moved tenant (write %t) gave %v and left a row", write, err)
			}
			if !write && !strings.Contains(err.Error(), "tenant setting") {
				t.Fatalf("with no write, tenant.Commit's check did not refuse the run: %v", err)
			}
		}
	})

	t.Run("ST-SUP-RP10-LC30-T02 The command runtime refuses a pool that tenant.Open did not make", func(t *testing.T) {
		plain, err := sql.Open("pgx", e.dsn(e.roles[0]))
		if err != nil {
			t.Fatal(err)
		}
		defer plain.Close()
		cfg := Config{DB: plain, AuditDB: e.auditDB, Authz: e.authz, Audit: e.writer, Outbox: e.queue, Probes: e.probes, Service: "orders"}
		if _, err := New(e.ctx, cfg); err == nil || !strings.Contains(err.Error(), "tenant.Open") {
			t.Fatalf("a pool from sql.Open gave %v, want it refused", err)
		}
		cfg.DB, cfg.AuditDB = e.auditDB, plain
		if _, err := New(e.ctx, cfg); err == nil {
			t.Fatal("an audit pool from sql.Open was accepted")
		}
		cfg.DB, cfg.AuditDB = e.pool, e.auditDB
		if _, err := New(e.ctx, cfg); err != nil {
			t.Fatalf("pools from tenant.Open were refused: %v", err)
		}
	})
}

func create(t *testing.T, ctx context.Context, e *env, h *Handle[testProbe], id string) error {
	t.Helper()
	_, err := repo(t, h, e.reservationE).Create(ctx, reservation{ID: id, OrderID: "o1", SKU: "s"}, nil)
	return err
}

// waitBackendDone waits until the backend pid has ended or is idle, out of any transaction, so the outcome of its
// last COMMIT is fixed, and fails the test after 10 s.
func (e *env) waitBackendDone(t *testing.T, pid int) {
	t.Helper()
	if pid == 0 {
		t.Fatal("the proxy saw no COMMIT")
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if e.count(`SELECT count(*) FROM pg_stat_activity WHERE pid = $1 AND state IS DISTINCT FROM 'idle'`, pid) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	var info string
	_ = e.admin.QueryRowContext(e.ctx, `SELECT coalesce(state, '') || ' ' || coalesce(wait_event_type, '') || ' ' || left(query, 60)
 FROM pg_stat_activity WHERE pid = $1`, pid).Scan(&info)
	t.Fatalf("the backend %d that ran COMMIT is still at work: %s", pid, info)
}

// terminateAtCommit ends, from the server side, the session of the runtime role that runs COMMIT.
func (e *env) terminateAtCommit(t *testing.T) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var pid int
		err := e.admin.QueryRowContext(e.ctx, `SELECT pid FROM pg_stat_activity WHERE datname = current_database()
 AND usename = $1 AND state = 'active' AND query ILIKE 'commit%'`, e.roles[0]).Scan(&pid)
		if err == nil {
			e.exec(fmt.Sprintf(`SELECT pg_terminate_backend(%d)`, pid))
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("no session ran COMMIT")
}

// commitCalls lists each call of a method named Commit, in the package's files that are not tests, on a *sql.Tx or
// on a type whose struct holds one. A call of the function tenant.Commit is not a method call.
func commitCalls(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-export", "-deps", "-f", "{{.ImportPath}}={{.Export}}", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	exports := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		path, file, _ := strings.Cut(line, "=")
		exports[path] = file
	}
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file, ok := exports[path]
		if !ok || file == "" {
			return nil, os.ErrNotExist
		}
		return os.Open(file)
	})
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
	info := &types.Info{Selections: map[*ast.SelectorExpr]*types.Selection{}, Uses: map[*ast.Ident]types.Object{}}
	if _, err := (&types.Config{Importer: imp}).Check(modulePath+"/command", fset, files, info); err != nil {
		t.Fatal(err)
	}
	var calls []string
	tenantCommits := 0
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Commit" {
				return true
			}
			selection, isMethod := info.Selections[sel]
			if !isMethod {
				if fn, ok := info.Uses[sel.Sel].(*types.Func); ok && fn.Pkg().Path() == modulePath+"/tenant" {
					tenantCommits++
				}
				return true
			}
			if holdsTx(selection.Recv()) {
				calls = append(calls, fset.Position(call.Pos()).String())
			}
			return true
		})
	}
	if tenantCommits == 0 {
		t.Fatal("the scan found no call of tenant.Commit")
	}
	return calls
}

// holdsTx reports whether t is *sql.Tx, or a struct, or a pointer to one, with a field that holds a *sql.Tx.
func holdsTx(t types.Type) bool {
	if t.String() == "*database/sql.Tx" || t.String() == "database/sql.Tx" {
		return true
	}
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	s, ok := t.Underlying().(*types.Struct)
	if !ok {
		return false
	}
	for i := range s.NumFields() {
		if strings.Contains(s.Field(i).Type().String(), "database/sql.Tx") {
			return true
		}
	}
	return false
}

// pgProxy is a TCP proxy to the PostgreSQL server. It reads the backend's process ID from the server's BackendKeyData
// message of each connection, and keeps the one of the last connection that sent COMMIT, so a test can wait until
// that backend has ended and its outcome is fixed. When dropCommitReply is set, it stops passing on the server's
// messages of that connection, then passes on the client's COMMIT and closes the client's connection: the server
// reads the COMMIT and commits, and the client never gets the answer.
type pgProxy struct {
	dsn             string
	dropCommitReply atomic.Bool
	commitPID       atomic.Int32
}

func newPGProxy(t *testing.T, target string) *pgProxy {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	server := u.Host
	u.Host = listener.Addr().String()
	p := &pgProxy{dsn: u.String()}
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			go p.serve(client, server)
		}
	}()
	return p
}

func (p *pgProxy) serve(client net.Conn, address string) {
	defer client.Close()
	server, err := net.Dial("tcp", address)
	if err != nil {
		return
	}
	defer server.Close()
	var pid atomic.Int32
	var drop atomic.Bool
	go func() {
		// The server's messages, typed after the one-byte answer to an SSL or GSS request.
		typed := make([]byte, 5)
		for {
			if _, err := io.ReadFull(server, typed[:1]); err != nil {
				return
			}
			if typed[0] == 'N' || typed[0] == 'G' {
				if _, err := client.Write(typed[:1]); err != nil {
					return
				}
				continue
			}
			if _, err := io.ReadFull(server, typed[1:]); err != nil {
				return
			}
			body := make([]byte, binary.BigEndian.Uint32(typed[1:])-4)
			if _, err := io.ReadFull(server, body); err != nil {
				return
			}
			if typed[0] == 'K' && len(body) >= 4 {
				pid.Store(int32(binary.BigEndian.Uint32(body[:4])))
			}
			if drop.Load() {
				continue
			}
			if _, err := client.Write(append(append([]byte(nil), typed...), body...)); err != nil {
				return
			}
		}
	}()
	header := make([]byte, 4)
	// The messages before the startup message have no type byte: an SSL or GSS request, then the startup message.
	for {
		if _, err := io.ReadFull(client, header); err != nil {
			return
		}
		body := make([]byte, binary.BigEndian.Uint32(header)-4)
		if _, err := io.ReadFull(client, body); err != nil {
			return
		}
		if _, err := server.Write(append(append([]byte(nil), header...), body...)); err != nil {
			return
		}
		if code := binary.BigEndian.Uint32(body[:4]); code != 80877103 && code != 80877104 {
			break
		}
	}
	typed := make([]byte, 5)
	for {
		if _, err := io.ReadFull(client, typed); err != nil {
			return
		}
		body := make([]byte, binary.BigEndian.Uint32(typed[1:])-4)
		if _, err := io.ReadFull(client, body); err != nil {
			return
		}
		commit := typed[0] == 'Q' && strings.HasPrefix(strings.ToLower(string(body)), "commit")
		if commit {
			p.commitPID.Store(pid.Load())
		}
		armed := commit && p.dropCommitReply.Load()
		if armed {
			// The answer to the COMMIT must never reach the client, so the server's messages stop before it is sent.
			drop.Store(true)
		}
		if _, err := server.Write(append(append([]byte(nil), typed...), body...)); err != nil {
			return
		}
		if armed {
			_ = client.Close()
			time.Sleep(500 * time.Millisecond)
			return
		}
	}
}
