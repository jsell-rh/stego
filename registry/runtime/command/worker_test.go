package command

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"stego.dev/runtime/idempotency"
	"stego.dev/runtime/lease"
	"stego.dev/runtime/outbox"
)

// tick is the tick of the worker tests: due at 1793966400 seconds since the Unix epoch, a multiple of 1m.
var tick = time.Unix(1793966400, 0)

func (e *env) worker(name, permission string, change func(*WorkerSpec[testProbe]), writes ...Declared) *Worker[testProbe] {
	e.t.Helper()
	spec := WorkerSpec[testProbe]{Name: name, Permission: permission, Interval: time.Minute, Writes: writes, Probe: newTestProbe,
		Destinations: []Destination{{Name: "partner-orders", Idempotent: true}, {Name: "billing", Idempotent: true}}}
	if change != nil {
		change(&spec)
	}
	w, err := NewWorker(spec)
	if err != nil {
		e.t.Fatal(err)
	}
	return w
}

// tickFill adapts a tick fill to RunWorker.
func tickFill(fill func(ctx context.Context, h *Handle[testProbe]) error) TickFill[testProbe] {
	return func(ctx context.Context, _ time.Time, h *Handle[testProbe]) error { return fill(ctx, h) }
}

// updateOrder sets the status of the order id through the handle.
func updateOrder(ctx context.Context, h *Handle[testProbe], e *Entity[order], id, status string) (Loaded[order], error) {
	r, err := Repo(h, e)
	if err != nil {
		return nil, err
	}
	o, err := r.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	v := o.Value()
	v.Status = status
	return r.Update(ctx, o, v)
}

// testClock is the injected clock of the outbox in the consumer tests: the system clock, which the test moves on
// with add.
type testClock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

// consumer runs the outbox worker of the worker role with the destination partner-replies, whose handler calls
// RunConsumer for w with fill, until the test ends. The other destinations complete at once.
func (e *env) consumer(w *Worker[testProbe], change func(*outbox.WorkerConfig),
	fill MessageFill[map[string]string, testProbe]) {
	e.t.Helper()
	config := outbox.DefaultWorkerConfig()
	config.Concurrency, config.PollInterval = 1, 10*time.Millisecond
	config.RetryMin, config.RetryMax, config.AttemptTimeout = 10*time.Millisecond, 50*time.Millisecond, 5*time.Second
	if change != nil {
		change(&config)
	}
	worker, err := outbox.NewWorker(e.wqueue, config)
	if err != nil {
		e.t.Fatal(err)
	}
	decode := func(payload []byte) (map[string]string, error) {
		var m map[string]string
		return m, json.Unmarshal(payload, &m)
	}
	// The generated code registers a consumed destination as one that must land (ST-SUP-RC35-OC06); such a message is
	// never parked (ST-SUP-RP01-LC23), so the tests register it as idempotent only, to see the attempts limit.
	destinations := []outbox.Destination{{Name: "partner-replies", Idempotent: true,
		Handler: func(ctx context.Context, tx *sql.Tx, d outbox.Delivery) error {
			return RunConsumer(ctx, e.rt, w, tx, d, decode, fill)
		}}}
	for _, name := range []string{"audit-sink", "billing", "partner-orders"} {
		destinations = append(destinations, outbox.Destination{Name: name, Idempotent: true,
			Handler: func(context.Context, *sql.Tx, outbox.Delivery) error { return nil }})
	}
	for _, d := range destinations {
		if err := worker.Register(d); err != nil {
			e.t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = worker.Run(ctx) }()
	e.t.Cleanup(func() { cancel(); <-done })
}

// stage runs the command reserve-and-charge as user:u-7 in tenant tenantID, whose fill stages a message for
// partner-replies for the order orderID with payload, and returns the message's ID.
func (e *env) stage(tenantID, orderID string, payload map[string]string) uuid.UUID {
	e.t.Helper()
	cmd := e.command("reserve-and-charge", nil, e.orders)
	body, _ := json.Marshal(payload)
	if _, err := e.run(cmd, e.request(tenantID, u7(e.t), "k-stage-"+uuid.NewString(), `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
		o, err := repo(e.t, h, e.orders).Get(ctx, orderID)
		if err != nil {
			return err
		}
		return h.Effects().Stage(ctx, "partner-replies", "reply-received", o, body)
	})); err != nil {
		e.t.Fatal(err)
	}
	var id uuid.UUID
	if err := e.admin.QueryRowContext(e.ctx, `SELECT id FROM stego_outbox.messages WHERE destination = 'partner-replies'
 AND tenant_id = $1 ORDER BY sequence DESC LIMIT 1`, tenantID).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// waitState waits until the message id is in state, and fails the test after 10 s.
func (e *env) waitState(t *testing.T, id uuid.UUID, state string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if e.count(`SELECT count(*) FROM stego_outbox.messages WHERE id = $1 AND state = $2`, id, state) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	var got string
	_ = e.admin.QueryRowContext(e.ctx, `SELECT state || ' ' || failure_code || ' attempts ' || attempts || ' conflicts ' || conflicts
 || ' available ' || available_at FROM stego_outbox.messages WHERE id = $1`, id).Scan(&got)
	t.Fatalf("the message %s is %s, not %s", id, got, state)
}

// redeliver makes a done message pending again, as a dispatch again after a crash before its state was recorded.
func (e *env) redeliver(id uuid.UUID) {
	e.t.Helper()
	e.exec(fmt.Sprintf(`UPDATE stego_outbox.messages SET state = 'pending', available_at = now(), done_at = NULL
 WHERE id = '%s'`, id))
}

func TestWorkers(t *testing.T) {
	e := newEnv(t)
	e.exec(`INSERT INTO orders (tenant_id, id, status) VALUES ('t1', 'o-1', 'new'), ('t1', 'o-2', 'new'), ('t2', 'o-1', 'new'),
 ('t2', 'o-2', 'new')`,
		`INSERT INTO charges (tenant_id, id, amount, card_last4) VALUES ('t1', 'c1', 1, '0000')`)
	allOrders, err := NewList(e.orders, ListSpec{Name: "all",
		Read: `SELECT id, status, version, authz_token FROM orders WHERE tenant_id = $1 ORDER BY id LIMIT $2`})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("ST-SUP-RP10-LC17-T01 A worker handle reads only allowed rows and writes only allowed resources", func(t *testing.T) {
		w := e.worker("sync-partner-orders", "sync", nil, e.orders)
		var o2 Loaded[order]
		var listed []string
		var writeCalls []evaluation
		err := RunWorker(e.ctx, e.rt, w, tick, mustTenant(t, "t1"), nil, tickFill(func(ctx context.Context, h *Handle[testProbe]) error {
			r := repo(t, h, e.orders)
			var err error
			// The value of o-2 is loaded while the port still allows it, so the fill holds one to write.
			if o2, err = r.Get(ctx, "o-2"); err != nil {
				return err
			}
			e.decision.setAllow(func(subject, action, resource string) bool {
				return !(resource == "order:o-2" && action == "sync")
			})
			list, err := r.List(ctx, allOrders, 10)
			if err != nil {
				return err
			}
			for _, o := range list {
				listed = append(listed, o.ID())
			}
			e.decision.taken()
			if _, err := updateOrderFrom(ctx, r, list[0], "synced"); err != nil {
				return err
			}
			if _, err := updateOrderFrom(ctx, r, o2, "synced"); !errors.Is(err, ErrRefused) {
				t.Errorf("the update of o-2 gave %v, want the refused error", err)
			}
			if _, err := r.Create(ctx, order{ID: "o-new", Status: "new"}, o2); !errors.Is(err, ErrRefused) {
				t.Errorf("the create under o-2 gave %v, want the refused error", err)
			}
			writeCalls = e.decision.taken()
			return nil
		}))
		e.decision.setAllow(nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(listed, ",") != "o-1" {
			t.Fatalf("the list holds %v, want o-1 only", listed)
		}
		if e.count(`SELECT count(*) FROM orders WHERE tenant_id = 't1' AND status = 'synced'`) != 1 ||
			e.count(`SELECT count(*) FROM orders WHERE id = 'o-new'`) != 0 {
			t.Fatal("the writes are not o-1 alone")
		}
		if len(writeCalls) != 3 {
			t.Fatalf("the three writes made the decisions %+v, want one each", writeCalls)
		}
		for _, c := range writeCalls {
			if c.Subject != "service_account:sync-partner-orders" || c.Action != "sync" {
				t.Fatalf("a write was decided as %+v", c)
			}
		}
	})

	t.Run("ST-SUP-RP10-LC18-T01 A worker tick commits its writes and effects together or not at all", func(t *testing.T) {
		w := e.worker("tick-orders", "sync", nil, e.orders)
		for i, end := range []string{"nil", "error", "panic"} {
			e.exec(`UPDATE orders SET status = 'new' WHERE tenant_id = 't2' AND id = 'o-1'`)
			outbox, audits := e.outboxRows("t2", "partner-orders"), e.auditRows("t2", "tick-orders")
			calls := 0
			err := RunWorker(e.ctx, e.rt, w, tick.Add(time.Duration(i)*time.Minute), mustTenant(t, "t2"), nil,
				tickFill(func(ctx context.Context, h *Handle[testProbe]) error {
					calls++
					o, err := updateOrder(ctx, h, e.orders, "o-1", "ticked")
					if err != nil {
						return err
					}
					if err := h.Effects().Stage(ctx, "partner-orders", "order-changed", o, []byte(`{}`)); err != nil {
						return err
					}
					switch end {
					case "error":
						return errors.New("refused")
					case "panic":
						panic("secret-42")
					}
					return nil
				}))
			wrote := e.count(`SELECT count(*) FROM orders WHERE tenant_id = 't2' AND id = 'o-1' AND status = 'ticked'`) +
				e.outboxRows("t2", "partner-orders") - outbox + e.auditRows("t2", "tick-orders") - audits
			if calls != 1 {
				t.Fatalf("%s: the fill ran %d times", end, calls)
			}
			if end == "nil" && (err != nil || wrote != 3) {
				t.Fatalf("the tick that returned nil gave %v and left %d of the 3 rows", err, wrote)
			}
			if end != "nil" && (err == nil || wrote != 0 || strings.Contains(err.Error(), "secret-42")) {
				t.Fatalf("the tick that ended with %s gave %v and left %d rows", end, err, wrote)
			}
		}
	})

	t.Run("ST-SUP-RP10-LC21-T01 Two replicas that run one tick stage one key for each entity", func(t *testing.T) {
		w := e.worker("sync-partner-orders", "sync", nil, e.orders)
		stageBoth := tickFill(func(ctx context.Context, h *Handle[testProbe]) error {
			r := repo(t, h, e.orders)
			for _, id := range []string{"o-1", "o-2"} {
				o, err := r.Get(ctx, id)
				if err != nil {
					return err
				}
				if err := h.Effects().Stage(ctx, "partner-orders", "order-changed", o, []byte(`{}`)); err != nil {
					return err
				}
			}
			return nil
		})
		e.exec(`DELETE FROM stego_outbox.messages WHERE destination = 'partner-orders'`)
		var replicas sync.WaitGroup
		errs := make(chan error, 2)
		for range 2 {
			replicas.Add(1)
			go func() {
				defer replicas.Done()
				errs <- RunWorker(e.ctx, e.rt, w, tick, mustTenant(t, "t1"), nil, stageBoth)
			}()
		}
		replicas.Wait()
		for range 2 {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}
		o1 := e.outgoingKey("t1", "partner-orders", "order-changed", "order:o-1")
		o2 := e.outgoingKey("t1", "partner-orders", "order-changed", "order:o-2")
		want := idempotency.Derive("t1", "service_account:sync-partner-orders", lp("sync-partner-orders", "1793966400", "o-1"),
			"partner-orders", "order-changed")
		if len(o1) != 2 || o1[0] != want || o1[1] != want || len(o2) != 2 || o2[0] != o2[1] || o2[0] == want {
			t.Fatalf("the replicas' keys are %v for o-1 and %v for o-2, want %s twice and another key twice", o1, o2, want)
		}
		err := RunWorker(e.ctx, e.rt, w, tick, mustTenant(t, "t1"), nil, tickFill(func(ctx context.Context, h *Handle[testProbe]) error {
			o, err := repo(t, h, e.orders).Get(ctx, "o-1")
			if err != nil {
				return err
			}
			if err := h.Effects().Stage(ctx, "partner-orders", "order-changed", o, []byte(`{}`)); err != nil {
				return err
			}
			if err := h.Effects().Stage(ctx, "partner-orders", "order-changed", o, []byte(`{}`)); err == nil {
				t.Error("a second order-changed for o-1 in one run was staged")
			}
			return errors.New("roll back")
		}))
		if err == nil {
			t.Fatal("the run committed")
		}
		if err := RunWorker(e.ctx, e.rt, w, tick.Add(time.Minute), mustTenant(t, "t1"), nil, stageBoth); err != nil {
			t.Fatal(err)
		}
		next := e.outgoingKey("t1", "partner-orders", "order-changed", "order:o-1")
		if len(next) != 3 || next[2] == want {
			t.Fatalf("the next tick gave the keys %v for o-1, want a new key", next)
		}
		if err := RunWorker(e.ctx, e.rt, w, tick.Add(time.Second), mustTenant(t, "t1"), nil, stageBoth); err == nil {
			t.Fatal("a tick that is not due at a multiple of the interval ran")
		}
	})

	t.Run("ST-SUP-RP10-LC20-T01 A run with a stale lease token is fenced, and a live run holds back a new holder", func(t *testing.T) {
		const name = "stego.worker.sync-partner-orders"
		if err := lease.Declare(e.ctx, e.admin, name); err != nil {
			t.Fatal(err)
		}
		e.exec(`UPDATE leases SET token = 3 WHERE name = '` + name + `'`)
		h1, err := lease.New(e.pool, name, "h1", lease.Settings{})
		if err != nil {
			t.Fatal(err)
		}
		held, err := h1.Acquire(e.ctx)
		if err != nil || held.Token() != 4 {
			t.Fatalf("h1 acquired token %v, %v; want 4", held, err)
		}
		// The lease expires in the row, while h1's run holds it.
		e.exec(`UPDATE leases SET expires_at = now() - interval '1 second' WHERE name = '` + name + `'`)
		w := e.worker("sync-partner-orders", "sync", nil, e.orders)
		running, release := make(chan struct{}), make(chan struct{})
		first := make(chan error, 1)
		go func() {
			first <- RunWorker(e.ctx, e.rt, w, tick, mustTenant(t, "t1"), held, tickFill(func(ctx context.Context, h *Handle[testProbe]) error {
				close(running)
				<-release
				_, err := updateOrder(ctx, h, e.orders, "o-1", "fenced-run")
				return err
			}))
		}()
		<-running
		h2, err := lease.New(e.pool, name, "h2", lease.Settings{})
		if err != nil {
			t.Fatal(err)
		}
		var committed atomic.Bool
		acquired := make(chan *lease.Held, 1)
		go func() {
			got, err := h2.Acquire(e.ctx)
			if err != nil || !committed.Load() {
				t.Errorf("h2's acquire gave %v before the run committed (%t)", err, committed.Load())
			}
			acquired <- got
		}()
		e.waitLockWaits(1)
		committed.Store(true)
		close(release)
		if err := <-first; err != nil {
			t.Fatal(err)
		}
		if got := <-acquired; got == nil || got.Token() != 5 {
			t.Fatalf("h2 holds %v, want token 5", got)
		}
		calls := 0
		err = RunWorker(e.ctx, e.rt, w, tick, mustTenant(t, "t1"), held, tickFill(func(ctx context.Context, h *Handle[testProbe]) error {
			calls++
			_, err := updateOrder(ctx, h, e.orders, "o-1", "stale-run")
			return err
		}))
		if !errors.Is(err, lease.ErrFenced) || calls != 0 || e.count(`SELECT count(*) FROM orders WHERE status = 'stale-run'`) != 0 {
			t.Fatalf("the run with token 4 gave %v after %d calls", err, calls)
		}
	})

	t.Run("ST-SUP-RP10-LC22-T01 A tenant's run that would outlive the lease is rolled back before expiry and retried", func(t *testing.T) {
		const name = "stego.worker.long-run"
		if err := lease.Declare(e.ctx, e.admin, name); err != nil {
			t.Fatal(err)
		}
		client, err := lease.New(e.pool, name, "h1", lease.Settings{TTL: 15 * time.Second, Renew: 10 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		held, err := client.Acquire(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		var expires time.Time
		if err := e.admin.QueryRowContext(e.ctx, `SELECT expires_at FROM leases WHERE name = $1`, name).Scan(&expires); err != nil {
			t.Fatal(err)
		}
		rt := e.runtime(nil)
		commitAttempted := false
		rt.beforeCommit = func(context.Context, *sql.Tx) error { commitAttempted = true; return nil }
		w := e.worker("long-run", "sync", nil, e.reservationE)
		start := time.Now()
		ended := make(chan time.Duration, 1)
		go func() {
			// The transaction's session leaves 'idle in transaction' when the transaction ends.
			for time.Since(start) < 8*time.Second {
				if time.Since(start) > 500*time.Millisecond && e.count(`SELECT count(*) FROM pg_stat_activity
 WHERE datname = current_database() AND usename = $1 AND state LIKE 'idle in transaction%'`, e.roles[0]) == 0 {
					ended <- time.Since(start)
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
			ended <- -1
		}()
		err = RunWorker(e.ctx, rt, w, tick, mustTenant(t, "t1"), held, tickFill(func(ctx context.Context, h *Handle[testProbe]) error {
			if _, err := repo(t, h, e.reservationE).Create(ctx, reservation{ID: "r-long", OrderID: "o-1", SKU: "s"}, nil); err != nil {
				return err
			}
			time.Sleep(6 * time.Second)
			return nil
		}))
		took := <-ended
		if !errors.Is(err, ErrDeadline) || commitAttempted || took < 4800*time.Millisecond || took > 5600*time.Millisecond ||
			!start.Add(took).Before(expires) || e.count(`SELECT count(*) FROM reservations WHERE id = 'r-long'`) != 0 {
			t.Fatalf("the long run gave %v, ended its transaction after %s (expires %s), commit attempted %t",
				err, took, expires.Sub(start), commitAttempted)
		}
		err = RunWorker(e.ctx, rt, w, tick.Add(time.Minute), mustTenant(t, "t1"), held, tickFill(func(ctx context.Context, h *Handle[testProbe]) error {
			_, err := repo(t, h, e.reservationE).Create(ctx, reservation{ID: "r-long", OrderID: "o-1", SKU: "s"}, nil)
			return err
		}))
		if err != nil || e.count(`SELECT count(*) FROM reservations WHERE id = 'r-long'`) != 1 {
			t.Fatalf("the next tick of t1 gave %v", err)
		}
	})

	t.Run("ST-SUP-RP10-LC26-T01 A worker conflict runs the tenant in the next tick, and a consumer conflict is dispatched again and is not an attempt", func(t *testing.T) {
		w := e.worker("sync-partner-orders", "sync", nil, e.orders)
		calls := map[string]int{}
		fill := func(conflictTenant string) TickFill[testProbe] {
			return tickFill(func(ctx context.Context, h *Handle[testProbe]) error {
				id := h.Tenant().ID()
				calls[id]++
				r := repo(t, h, e.orders)
				o, err := r.Get(ctx, "o-2")
				if err != nil {
					return err
				}
				if id == conflictTenant {
					e.exec(`UPDATE orders SET version = version + 1 WHERE tenant_id = '` + id + `' AND id = 'o-2'`)
				}
				v := o.Value()
				v.Status = "tick-" + id
				_, err = r.Update(ctx, o, v)
				return err
			})
		}
		results := map[string]error{}
		for _, id := range []string{"t1", "t2"} {
			results[id] = RunWorker(e.ctx, e.rt, w, tick, mustTenant(t, id), nil, fill("t1"))
		}
		if !errors.Is(results["t1"], ErrConflict) || results["t2"] != nil || calls["t1"] != 1 ||
			e.count(`SELECT count(*) FROM orders WHERE id = 'o-2' AND status LIKE 'tick-%'`) != 1 {
			t.Fatalf("the first tick gave %v after %d calls for t1", results, calls["t1"])
		}
		if err := RunWorker(e.ctx, e.rt, w, tick.Add(time.Minute), mustTenant(t, "t1"), nil, fill("")); err != nil {
			t.Fatalf("the next tick of t1 gave %v", err)
		}
		if e.count(`SELECT count(*) FROM orders WHERE tenant_id = 't1' AND id = 'o-2' AND status = 'tick-t1'`) != 1 {
			t.Fatal("the next tick did not commit t1")
		}
	})
}

// updateOrderFrom writes status over a loaded order.
func updateOrderFrom(ctx context.Context, r *Repository[order], o Loaded[order], status string) (Loaded[order], error) {
	v := o.Value()
	v.Status = status
	return r.Update(ctx, o, v)
}

func TestConsumers(t *testing.T) {
	clock := &testClock{}
	e := newEnv(t, func(c *envConfig) { c.queueClock = clock })
	e.exec(`INSERT INTO orders (tenant_id, id, status) VALUES ('t1', 'o-1', 'new'), ('t1', 'o-2', 'new')`,
		`INSERT INTO charges (tenant_id, id, amount, card_last4) VALUES ('t1', 'c1', 1, '0000')`)
	var mode atomic.Value
	mode.Store("ok")
	var calls atomic.Int64
	w := e.worker("apply-partner-replies", "apply", func(s *WorkerSpec[testProbe]) {
		s.Interval = 0
		s.Stagers = map[string]string{"reserve-and-charge": "reserve"}
	}, e.orders, e.chargeE)
	e.consumer(w, func(c *outbox.WorkerConfig) { c.MaxAttempts = 3 }, func(ctx context.Context, m map[string]string, _ string, h *Handle[testProbe]) error {
		calls.Add(1)
		r := repo(t, h, e.orders)
		o, err := r.Get(ctx, m["order"])
		if err != nil {
			return err
		}
		c, err := repo(t, h, e.chargeE).Get(ctx, "c1")
		if err != nil {
			return err
		}
		if err := h.Effects().Stage(ctx, "billing", "charge-requested", c, []byte(`{}`)); err != nil {
			return err
		}
		switch mode.Load() {
		case "conflict-once":
			mode.Store("ok")
			e.exec(`UPDATE orders SET version = version + 1 WHERE id = '` + m["order"] + `'`)
		case "conflict-always":
			e.exec(`UPDATE orders SET version = version + 1 WHERE id = '` + m["order"] + `'`)
		}
		v := o.Value()
		v.Status = "applied-" + m["n"]
		_, err = r.Update(ctx, o, v)
		return err
	})
	chargeKeys := func() []string { return e.outgoingKey("t1", "billing", "charge-requested", "charge:c1") }
	derive := func(message uuid.UUID) string {
		return idempotency.Derive("t1", "service_account:apply-partner-replies", lp(message.String(), "c1"), "billing", "charge-requested")
	}

	t.Run("ST-SUP-RP10-LC19-T01 A message delivered twice runs the consumer fill once", func(t *testing.T) {
		calls.Store(0)
		m1 := e.stage("t1", "o-1", map[string]string{"order": "o-1", "n": "1"})
		e.waitState(t, m1, "done")
		e.redeliver(m1)
		e.waitState(t, m1, "done")
		m2 := e.stage("t1", "o-1", map[string]string{"order": "o-1", "n": "2"})
		e.waitState(t, m2, "done")
		if calls.Load() != 2 {
			t.Fatalf("the fill ran %d times, want for the first m-1 and for m-2 only", calls.Load())
		}
		if e.keyRows(m1.String()) != 1 || e.keyRows(m2.String()) != 1 {
			t.Fatal("each message does not have one key row")
		}
		keys := chargeKeys()
		if len(keys) != 2 || keys[0] != derive(m1) || keys[1] != derive(m2) {
			t.Fatalf("the effects carry %v, want the keys of m-1 and m-2", keys)
		}
	})

	t.Run("ST-SUP-RP10-LC08-T02 An effect that a consumer stages is keyed by the message identity and its entity", func(t *testing.T) {
		e.exec(`DELETE FROM stego_outbox.messages WHERE destination = 'billing'`)
		m1 := e.stage("t1", "o-2", map[string]string{"order": "o-2", "n": "3"})
		e.waitState(t, m1, "done")
		if keys := chargeKeys(); len(keys) != 1 || keys[0] != derive(m1) {
			t.Fatalf("the staged row carries %v, want %s", keys, derive(m1))
		}
		e.redeliver(m1)
		e.waitState(t, m1, "done")
		if keys := chargeKeys(); len(keys) != 1 {
			t.Fatalf("the second run staged %d rows, want none", len(keys)-1)
		}
	})

	t.Run("ST-SUP-RP10-LC26-T01 A worker conflict runs the tenant in the next tick, and a consumer conflict is dispatched again and is not an attempt", func(t *testing.T) {
		e.exec(`DELETE FROM stego_outbox.messages WHERE destination = 'billing'`)
		calls.Store(0)
		mode.Store("conflict-once")
		m := e.stage("t1", "o-1", map[string]string{"order": "o-1", "n": "4"})
		deadline := time.Now().Add(5 * time.Second)
		for calls.Load() < 1 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		var attempts, conflicts int
		var wait time.Duration
		e.waitState(t, m, "pending")
		var waitMS float64
		if err := e.admin.QueryRowContext(e.ctx, `SELECT attempts, conflicts,
 extract(epoch FROM available_at - dispatched_at) * 1000 FROM stego_outbox.messages WHERE id = $1`, m).
			Scan(&attempts, &conflicts, &waitMS); err != nil {
			t.Fatal(err)
		}
		wait = time.Duration(waitMS * float64(time.Millisecond))
		// The wait after the conflict starts at 100 ms on the injected clock, from the dispatch.
		if attempts != 0 || conflicts != 1 || wait < 100*time.Millisecond || e.keyRows(m.String()) != 0 ||
			len(chargeKeys()) != 0 {
			t.Fatalf("after the conflict: %d attempts, %d conflicts, due %s after the dispatch, %d key rows, %d effects",
				attempts, conflicts, wait, e.keyRows(m.String()), len(chargeKeys()))
		}
		clock.add(time.Second)
		e.waitState(t, m, "done")
		if keys := chargeKeys(); calls.Load() != 2 || len(keys) != 1 || keys[0] != derive(m) {
			t.Fatalf("the dispatch again: %d calls, keys %v", calls.Load(), keys)
		}
		calls.Store(0)
		mode.Store("conflict-always")
		parked := e.stage("t1", "o-2", map[string]string{"order": "o-2", "n": "5"})
		// The clock moves on only while the message waits in pending, to the time it is due, so no lease of a
		// dispatch expires on the clock.
		for i := 0; i < 2000; i++ {
			var state string
			var waitMS float64
			if err := e.admin.QueryRowContext(e.ctx, `SELECT state, extract(epoch FROM available_at) * 1000 FROM stego_outbox.messages WHERE id = $1`, parked).
				Scan(&state, &waitMS); err != nil {
				t.Fatal(err)
			}
			if state == "parked" {
				break
			}
			if state == "pending" {
				if due := time.UnixMilli(int64(waitMS)).Sub(clock.Now()); due > 0 {
					clock.add(due + time.Millisecond)
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
		e.waitState(t, parked, "parked")
		mode.Store("ok")
		if calls.Load() != 23 {
			t.Fatalf("the message was parked after %d calls, want 20 conflicts with no attempt and then 3 attempts", calls.Load())
		}
	})
}
