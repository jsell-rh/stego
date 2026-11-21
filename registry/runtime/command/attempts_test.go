package command

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jsell-rh/stego/registry/runtime/idempotency"
	"github.com/jsell-rh/stego/registry/runtime/sealed"
)

// pinWallet is a test entity with a compare-only PIN with a limit of wrong attempts, as the generated code declares it
// (ST-SUP-RC32-OC24).
type pinWallet struct {
	ID      string
	Balance int64
	PIN     sealed.Compared
}

const pinWalletDDL = `CREATE TABLE pin_wallets (tenant_id text NOT NULL, id text NOT NULL, balance bigint NOT NULL, pin text,
  version bigint NOT NULL DEFAULT 1, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id))`

// pinCounter is the counter of PinWallet.pin with the limit 3.
var pinCounter = Must(NewCounter(CounterSpec{Entity: "PinWallet", Field: "pin", Limit: 3}))

func pinWalletEntity(t *testing.T) *Entity[pinWallet] {
	t.Helper()
	s := spec("PinWallet", "", "pin_wallets", []string{"balance", "pin"},
		func(row Scanner) (pinWallet, Meta, error) {
			var v pinWallet
			var m Meta
			err := row.Scan(&v.ID, &v.Balance, CountedAt(pinCounter, &v.ID, &v.PIN), &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		func(v pinWallet) []any { return []any{v.ID, v.Balance, HashCountedOf(pinCounter, v.ID, v.PIN)} },
		func(v pinWallet) map[string]any { return map[string]any{"id": v.ID, "balance": v.Balance} })
	e, err := NewEntity(s)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// pinCopy is a test entity with a compare-only PIN with no limit of wrong attempts (ST-SUP-RC32-OC20).
type pinCopy struct {
	ID  string
	PIN sealed.Compared
}

func pinCopyEntity(t *testing.T) *Entity[pinCopy] {
	t.Helper()
	s := spec("PinCopy", "", "pin_copies", []string{"pin"},
		func(row Scanner) (pinCopy, Meta, error) {
			var v pinCopy
			var m Meta
			err := row.Scan(&v.ID, ComparedAt(&v.PIN), &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		func(v pinCopy) []any { return []any{v.ID, HashOf(v.PIN)} },
		func(v pinCopy) map[string]any { return map[string]any{"id": v.ID} })
	e, err := NewEntity(s)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// pinWallets makes the table of the PIN wallets in the database of e with the wallet walletID, whose PIN is right.
func pinWallets(t *testing.T, e *env, walletID, right string) {
	t.Helper()
	e.exec(pinWalletDDL, "GRANT SELECT, INSERT, UPDATE, DELETE ON pin_wallets TO "+e.roles[0],
		"ALTER TABLE pin_wallets ENABLE ROW LEVEL SECURITY", "ALTER TABLE pin_wallets FORCE ROW LEVEL SECURITY",
		"CREATE POLICY pin_wallets_tenant ON pin_wallets USING (tenant_id = current_setting('app.tenant', true)) WITH CHECK (tenant_id = current_setting('app.tenant', true))")
	hash, err := sealed.Hash(right)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.admin.ExecContext(e.ctx, `INSERT INTO pin_wallets (tenant_id, id, balance, pin) VALUES ('t1', $1, 100, $2)`, walletID, hash); err != nil {
		t.Fatal(err)
	}
}

func TestAttempts(t *testing.T) {
	const walletID, right = "0198a1b2-0000-7000-8000-000000000001", "4711"
	e := newEnv(t)
	pinWallets(t, e, walletID, right)
	wallets := pinWalletEntity(t)
	copies := pinCopyEntity(t)
	e.exec(`CREATE TABLE pin_copies (tenant_id text NOT NULL, id text NOT NULL, pin text, version bigint NOT NULL DEFAULT 1,
  authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id))`, "GRANT SELECT, INSERT, UPDATE, DELETE ON pin_copies TO "+e.roles[0])
	rt := e.runtime(func(c *Config) { c.Entities = []Declared{wallets, copies} })
	var charges, refusedCharges atomic.Int32
	rt.charged = func(ok bool) {
		if ok {
			charges.Add(1)
		} else {
			refusedCharges.Add(1)
		}
	}
	cmd := e.command("withdraw", nil, wallets)
	errWrongPIN := errors.New("pin.wrong")
	var runs atomic.Int32
	type read func(ctx context.Context, r *Repository[pinWallet]) (Loaded[pinWallet], error)
	locked := func(ctx context.Context, r *Repository[pinWallet]) (Loaded[pinWallet], error) {
		return r.GetForUpdate(ctx, walletID)
	}
	unlocked := func(ctx context.Context, r *Repository[pinWallet]) (Loaded[pinWallet], error) {
		return r.Get(ctx, walletID)
	}
	// withdraw takes 10 from the wallet when the last PIN matches, and else returns the refusal; matched tells what
	// the last check gave.
	withdraw := func(get read, pins ...string) (matched bool, err error) {
		n := runs.Add(1)
		_, err = e.runOn(rt, cmd, e.request("t1", u7(t), fmt.Sprintf("w-%d", n), `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
			r, err := Repo(h, wallets)
			if err != nil {
				return idempotency.Response{}, err
			}
			w, err := get(ctx, r)
			if err != nil {
				return idempotency.Response{}, err
			}
			v := w.Value()
			for _, pin := range pins {
				matched = v.PIN.Matches(pin)
			}
			if !matched {
				return idempotency.Response{}, errWrongPIN
			}
			v.Balance -= 10
			if _, err := r.Update(ctx, w, v); err != nil {
				return idempotency.Response{}, err
			}
			return ok, nil
		})
		return matched, err
	}
	// setPIN writes a new PIN with a command, which resets the count.
	setPIN := func(pin string) {
		t.Helper()
		n := runs.Add(1)
		if _, err := e.runOn(rt, cmd, e.request("t1", u7(t), fmt.Sprintf("set-%d", n), `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
			r, err := Repo(h, wallets)
			if err != nil {
				return idempotency.Response{}, err
			}
			w, err := r.GetForUpdate(ctx, walletID)
			if err != nil {
				return idempotency.Response{}, err
			}
			v := w.Value()
			v.PIN = sealed.ComparedOf(pin)
			_, err = r.Update(ctx, w, v)
			return ok, err
		}); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		t.Helper()
		return e.count(`SELECT coalesce(sum(checks), 0) FROM attempt_counts WHERE counter = 'PinWallet.pin' AND row_id = $1`, walletID)
	}
	state := func() (balance, version int64) {
		t.Helper()
		if err := e.admin.QueryRowContext(e.ctx, `SELECT balance, version FROM pin_wallets WHERE id = $1`, walletID).Scan(&balance, &version); err != nil {
			t.Fatal(err)
		}
		return balance, version
	}
	t.Run("ST-SUP-RP10-LC54-T01 Each wrong PIN is charged before its check, and the charge stays after the refusal", func(t *testing.T) {
		_, before := state()
		for i := 1; i <= 3; i++ {
			if matched, err := withdraw(locked, "0000"); matched || !errors.Is(err, errWrongPIN) {
				t.Fatalf("attempt %d: matched %v, error %v; want the refusal", i, matched, err)
			}
			balance, version := state()
			if balance != 100 || count() != i || version != before {
				t.Fatalf("after wrong attempt %d: balance %d, count %d, version %d; want 100, %d, %d", i, balance, count(), version, i, before)
			}
		}
		if n := e.auditRows("t1", "withdraw"); n != 0 {
			t.Fatalf("the count wrote %d audit rows", n)
		}
		if n := e.count(`SELECT count(*) FROM stego_outbox.messages`); n != 0 {
			t.Fatalf("the count staged %d messages", n)
		}
	})
	t.Run("ST-SUP-RP10-LC54-T01 At the limit the right PIN gives false and runs no charge", func(t *testing.T) {
		refusedBefore := refusedCharges.Load()
		if matched, err := withdraw(locked, right); matched || !errors.Is(err, errWrongPIN) {
			t.Fatalf("matched %v, error %v; want false and the refusal", matched, err)
		}
		if balance, _ := state(); balance != 100 || count() != 3 || refusedCharges.Load() != refusedBefore+1 {
			t.Fatalf("balance %d, count %d, refused charges %d; want 100, 3, one more", balance, count(), refusedCharges.Load()-refusedBefore)
		}
	})
	t.Run("ST-SUP-RP10-LC54-T01 A new value of the field resets the count, and a match that commits resets it", func(t *testing.T) {
		_, before := state()
		setPIN(right)
		if count() != 0 {
			t.Fatalf("after a new PIN the count is %d, want 0", count())
		}
		if _, err := withdraw(locked, "0000"); !errors.Is(err, errWrongPIN) || count() != 1 {
			t.Fatalf("a wrong PIN gave %v and the count %d, want the refusal and 1", err, count())
		}
		if matched, err := withdraw(locked, right); !matched || err != nil || count() != 0 {
			t.Fatalf("the right PIN gave %v, %v and the count %d; want a commit and 0", matched, err, count())
		}
		if balance, version := state(); balance != 90 || version != before+2 {
			t.Fatalf("balance %d, version %d; want 90 and two writes", balance, version)
		}
	})
	t.Run("ST-SUP-RP10-LC54-T01 A command that fails after a match leaves the charge", func(t *testing.T) {
		n := runs.Add(1)
		failed := errors.New("short balance")
		_, err := e.runOn(rt, cmd, e.request("t1", u7(t), fmt.Sprintf("fail-%d", n), `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
			r, err := Repo(h, wallets)
			if err != nil {
				return idempotency.Response{}, err
			}
			w, err := r.Get(ctx, walletID)
			if err != nil {
				return idempotency.Response{}, err
			}
			if !w.Value().PIN.Matches(right) {
				return idempotency.Response{}, errWrongPIN
			}
			return idempotency.Response{}, failed
		})
		if !errors.Is(err, failed) || count() != 1 {
			t.Fatalf("error %v, count %d; want the failure and 1", err, count())
		}
		setPIN(right)
	})
	t.Run("ST-SUP-RP10-LC54-T01 Parallel wrong PINs against the limit 3 run at most 3 checks", func(t *testing.T) {
		for _, get := range []struct {
			name string
			read read
		}{{"with no lock", unlocked}, {"locked", locked}} {
			setPIN(right)
			before := charges.Load()
			var wg sync.WaitGroup
			errs := make(chan error, 12)
			for range 12 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if matched, err := withdraw(get.read, "0000"); matched || !errors.Is(err, errWrongPIN) {
						errs <- fmt.Errorf("matched %v, error %v", matched, err)
					}
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Errorf("%s: %v", get.name, err)
			}
			if checks := charges.Load() - before; checks != 3 || count() != 3 {
				t.Fatalf("%s: 12 parallel wrong PINs ran %d checks and left the count %d; want 3 and 3", get.name, checks, count())
			}
		}
		setPIN(right)
	})
	t.Run("ST-SUP-RP10-LC54-T01 A charge that cannot commit fails the check and answers 503", func(t *testing.T) {
		e.exec("REVOKE INSERT, UPDATE ON attempt_counts FROM " + e.roles[0])
		defer e.exec("GRANT INSERT, UPDATE ON attempt_counts TO " + e.roles[0])
		matched, err := withdraw(locked, right)
		if matched || !errors.Is(err, ErrUnavailable) {
			t.Fatalf("matched %v, error %v; want false and ErrUnavailable", matched, err)
		}
		if balance, _ := state(); balance != 90 {
			t.Fatalf("the balance is %d after a failed charge, want 90", balance)
		}
	})
	t.Run("ST-SUP-RP10-LC54-T03 A rerun of the call after a conflict does not charge the checks of an earlier attempt again", func(t *testing.T) {
		setPIN(right)
		rerun := e.command("withdraw-rerun", func(s *CommandSpec[testProbe]) { s.RetryOnConflict = 2 }, wallets)
		charges.Store(0)
		attempts := 0
		n := runs.Add(1)
		result, err := e.runOn(rt, rerun, e.request("t1", u7(t), fmt.Sprintf("rerun-%d", n), `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
			attempts++
			r, err := Repo(h, wallets)
			if err != nil {
				return idempotency.Response{}, err
			}
			w, err := r.Get(ctx, walletID)
			if err != nil {
				return idempotency.Response{}, err
			}
			v := w.Value()
			// Two wrong PINs in each attempt.
			if v.PIN.Matches("0000") || v.PIN.Matches("1111") {
				return idempotency.Response{}, errors.New("a wrong PIN matched")
			}
			if attempts < 3 {
				// Another transaction moves the version of the row: the update of the attempt is a conflict, and the
				// call runs again.
				e.exec(`UPDATE pin_wallets SET version = version + 1 WHERE id = '` + walletID + `'`)
				if _, err := r.Update(ctx, w, v); err != nil {
					return idempotency.Response{}, err
				}
			}
			return idempotency.Response{}, errWrongPIN
		})
		if !errors.Is(err, errWrongPIN) || attempts != 3 || result.Attempts != 3 {
			t.Fatalf("the call gave %v after %d calls of the fill (%d attempts), want the refusal at the third", err, attempts, result.Attempts)
		}
		if got := count(); got != 2 || charges.Load() != 2 {
			t.Fatalf("three attempts of two wrong PINs left the count %d after %d charges, want 2 and 2", got, charges.Load())
		}
		// A new call is a new request: its checks are charged.
		if matched, err := withdraw(unlocked, "2222"); matched || !errors.Is(err, errWrongPIN) || count() != 3 {
			t.Fatalf("the next call gave %v, %v and the count %d, want a refusal and 3", matched, err, count())
		}
		setPIN(right)
	})
	t.Run("ST-SUP-RP10-LC54-T05 A counted PIN that a read gives is not written to a field with no limit or to another row", func(t *testing.T) {
		setPIN(right)
		copier := e.command("copy-pin", nil, wallets, copies)
		otherID, copyID := "0198a1b2-0000-7000-8000-0000000000a1", "0198a1b2-0000-7000-8000-0000000000a2"
		for _, write := range []struct {
			name string
			do   func(ctx context.Context, h *Handle[testProbe], pin sealed.Compared) error
		}{
			{"a field with no limit", func(ctx context.Context, h *Handle[testProbe], pin sealed.Compared) error {
				r, err := Repo(h, copies)
				if err != nil {
					return err
				}
				_, err = r.Create(ctx, pinCopy{ID: copyID, PIN: pin})
				return err
			}},
			{"a new row with a new count", func(ctx context.Context, h *Handle[testProbe], pin sealed.Compared) error {
				r, err := Repo(h, wallets)
				if err != nil {
					return err
				}
				_, err = r.Create(ctx, pinWallet{ID: otherID, Balance: 1, PIN: pin})
				return err
			}},
		} {
			n := runs.Add(1)
			_, err := e.runOn(rt, copier, e.request("t1", u7(t), fmt.Sprintf("copy-%d", n), `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
				r, err := Repo(h, wallets)
				if err != nil {
					return idempotency.Response{}, err
				}
				w, err := r.Get(ctx, walletID)
				if err != nil {
					return idempotency.Response{}, err
				}
				return ok, write.do(ctx, h, w.Value().PIN)
			})
			if err == nil || !sealedFault(err) {
				t.Fatalf("%s: the copy of the counted PIN gave %v, want the internal error of a sealed value", write.name, err)
			}
		}
		if n := e.count(`SELECT count(*) FROM pin_copies`) + e.count(`SELECT count(*) FROM pin_wallets WHERE id = $1`, otherID); n != 0 {
			t.Fatalf("the copies of the counted PIN left %d rows, want 0", n)
		}
		// The same field of the same row takes the value that the read gave, as each update of the row does.
		if matched, err := withdraw(locked, right); !matched || err != nil {
			t.Fatalf("an update of the wallet with its own PIN gave %v and %v, want a commit", matched, err)
		}
	})
	t.Run("ST-SUP-RP10-LC54-T06 A rerun after a new PIN between two attempts charges the checks of the new PIN", func(t *testing.T) {
		setPIN(right)
		newHash, err := sealed.Hash("9999")
		if err != nil {
			t.Fatal(err)
		}
		rerun := e.command("withdraw-reset", func(s *CommandSpec[testProbe]) { s.RetryOnConflict = 1 }, wallets)
		charges.Store(0)
		attempts := 0
		n := runs.Add(1)
		_, err = e.runOn(rt, rerun, e.request("t1", u7(t), fmt.Sprintf("reset-%d", n), `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
			attempts++
			r, err := Repo(h, wallets)
			if err != nil {
				return idempotency.Response{}, err
			}
			w, err := r.Get(ctx, walletID)
			if err != nil {
				return idempotency.Response{}, err
			}
			v := w.Value()
			if v.PIN.Matches("0000") || v.PIN.Matches("1111") {
				return idempotency.Response{}, errors.New("a wrong PIN matched")
			}
			if attempts == 1 {
				// A reset commits between the two attempts: a new PIN and a count of 0, and the version moves, so the
				// update of the attempt is a conflict and the call runs again.
				if _, err := e.admin.ExecContext(e.ctx, `UPDATE pin_wallets SET pin = $2, version = version + 1 WHERE id = $1`, walletID, newHash); err != nil {
					return idempotency.Response{}, err
				}
				e.exec(`DELETE FROM attempt_counts WHERE row_id = '` + walletID + `'`)
				if _, err := r.Update(ctx, w, v); err != nil {
					return idempotency.Response{}, err
				}
			}
			return idempotency.Response{}, errWrongPIN
		})
		if !errors.Is(err, errWrongPIN) || attempts != 2 {
			t.Fatalf("the call gave %v after %d attempts, want the refusal at the second", err, attempts)
		}
		if got := count(); got != 2 || charges.Load() != 4 {
			t.Fatalf("two wrong PINs against the new PIN left the count %d after %d charges, want 2 and 4", got, charges.Load())
		}
		setPIN(right)
	})
}

// TestAttemptPools proves that the charge of a check runs on its own pool, not on the main pool (security review of
// PR #451, L2).
func TestAttemptPools(t *testing.T) {
	t.Run("ST-SUP-RP10-LC54-T02 The charge of a check runs on the pool of the charges while the fill holds the only main connection", func(t *testing.T) {
		const walletID, right = "0198a1b2-0000-7000-8000-000000000002", "4711"
		e := newEnv(t, func(c *envConfig) { c.mainConns = 1 })
		pinWallets(t, e, walletID, right)
		wallets := pinWalletEntity(t)
		if _, err := New(e.ctx, Config{DB: e.pool, AuditDB: e.auditDB, ChargeDB: e.pool, Authz: e.authz, Audit: e.writer, Outbox: e.queue,
			Probes: e.probes, Service: "orders", ServiceVersion: "1.0.0", DeclarationVersion: "d1"}); err == nil {
			t.Fatal("New takes the main pool as the pool of the charges")
		}
		chargeDB := e.openPool(e.dsn(e.roles[0]))
		chargeDB.SetMaxOpenConns(1)
		rt := e.runtime(func(c *Config) { c.Entities, c.ChargeDB = []Declared{wallets}, chargeDB })
		cmd := e.command("withdraw", nil, wallets)
		for i, pin := range []string{"0000", right} {
			var matched bool
			_, err := e.runOn(rt, cmd, e.request("t1", u7(t), fmt.Sprintf("pool-%d", i), `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
				r, err := Repo(h, wallets)
				if err != nil {
					return idempotency.Response{}, err
				}
				w, err := r.GetForUpdate(ctx, walletID)
				if err != nil {
					return idempotency.Response{}, err
				}
				if matched = w.Value().PIN.Matches(pin); !matched {
					return idempotency.Response{}, errors.New("pin.wrong")
				}
				return ok, nil
			})
			if matched != (pin == right) || (matched && err != nil) || errors.Is(err, ErrUnavailable) {
				t.Fatalf("the PIN %s gave %v and %v with one main connection, want a match only of the right PIN and no 503", pin, matched, err)
			}
		}
		if got := chargeDB.Stats().OpenConnections; got != 1 {
			t.Fatalf("the pool of the charges has %d open connections, want the one of the charges", got)
		}
	})
	t.Run("ST-SUP-RP10-LC54-T07 A charge ends at its lock timeout behind a reset, and a full pool of the charges is logged with no value", func(t *testing.T) {
		const walletID, right = "0198a1b2-0000-7000-8000-000000000003", "4711"
		e := newEnv(t)
		pinWallets(t, e, walletID, right)
		wallets := pinWalletEntity(t)
		chargeDB := e.openPool(e.dsn(e.roles[0]))
		chargeDB.SetMaxOpenConns(1)
		rt := e.runtime(func(c *Config) { c.Entities, c.ChargeDB = []Declared{wallets}, chargeDB })
		var chargeErrs []error
		var mu sync.Mutex
		rt.chargeErr = func(err error) { mu.Lock(); chargeErrs = append(chargeErrs, err); mu.Unlock() }
		cmd := e.command("withdraw", nil, wallets)
		check := func(key, pin string) error {
			_, err := e.runOn(rt, cmd, e.request("t1", u7(t), key, `{}`), func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
				r, err := Repo(h, wallets)
				if err != nil {
					return idempotency.Response{}, err
				}
				w, err := r.Get(ctx, walletID)
				if err != nil {
					return idempotency.Response{}, err
				}
				if !w.Value().PIN.Matches(pin) {
					return idempotency.Response{}, errors.New("pin.wrong")
				}
				return ok, nil
			})
			return err
		}
		if err := check("lock-0", "0000"); err == nil || errors.Is(err, ErrUnavailable) {
			t.Fatalf("the first wrong PIN gave %v, want the refusal", err)
		}
		// A reset of the count that has not committed holds the row of the count.
		hold, err := e.admin.BeginTx(e.ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := hold.ExecContext(e.ctx, `DELETE FROM attempt_counts WHERE row_id = $1`, walletID); err != nil {
			t.Fatal(err)
		}
		err = check("lock-1", "0000")
		_ = hold.Rollback()
		var state interface{ SQLState() string }
		mu.Lock()
		got := append([]error(nil), chargeErrs...)
		mu.Unlock()
		if !errors.Is(err, ErrUnavailable) || len(got) != 1 || !errors.As(got[0], &state) || state.SQLState() != "55P03" {
			t.Fatalf("the charge behind the reset gave %v with the charge errors %v, want 503 after the lock timeout (55P03)", err, got)
		}
		// Each connection of the pool of the charges is in use: the charge waits for one, and the wait is logged.
		var logs lockedBuffer
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		t.Cleanup(func() { slog.SetDefault(previous) })
		held, err := chargeDB.Conn(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- check("full-1", "0000") }()
		var result error
		for waiting := true; waiting; {
			select {
			case result = <-done:
				waiting = false
			default:
				if strings.Contains(logs.String(), "command.attempts.pool_saturated") {
					_ = held.Close()
					held = nil
					result = <-done
					waiting = false
				}
				runtime.Gosched()
			}
		}
		if held != nil {
			_ = held.Close()
			t.Fatalf("the charge ended with %v and logged no full pool of the charges", result)
		}
		if result == nil || errors.Is(result, ErrUnavailable) {
			t.Fatalf("the charge after the wait for the pool gave %v, want the refusal of the wrong PIN", result)
		}
		text := logs.String()
		if !strings.Contains(text, "entity=PinWallet") || !strings.Contains(text, "max_open=1") || strings.Contains(text, "0000") || strings.Contains(text, "argon2") {
			t.Fatalf("the log of the full pool is %q, want the entity and the counts of the pool and no value", text)
		}
	})
}

// lockedBuffer is a bytes.Buffer that a log handler and the test can use at one time.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
