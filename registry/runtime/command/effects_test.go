package command

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"stego.dev/runtime/idempotency"
)

// lp is a part of an inbound key: its length in bytes as a 4-byte big-endian unsigned integer, then the part.
func lp(parts ...string) string {
	var b []byte
	for _, p := range parts {
		b = binary.BigEndian.AppendUint32(b, uint32(len(p)))
		b = append(b, p...)
	}
	return string(b)
}

// outgoingKeys reads the outgoing keys of the tenant's messages for destination, kind and resource key.
func (e *env) outgoingKey(tenantID, destination, kind, resourceKey string) []string {
	e.t.Helper()
	rows, err := e.admin.QueryContext(e.ctx, `SELECT outgoing_key FROM stego_outbox.messages
 WHERE tenant_id = $1 AND destination = $2 AND kind = $3 AND resource_key = $4 ORDER BY created_at`, tenantID, destination, kind, resourceKey)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			e.t.Fatal(err)
		}
		keys = append(keys, key)
	}
	return keys
}

// fillOf adapts a fill that returns only an error.
func fillOf(fill func(ctx context.Context, h *Handle[testProbe]) error) func(context.Context, *Handle[testProbe]) (idempotency.Response, error) {
	return func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
		if err := fill(ctx, h); err != nil {
			return idempotency.Response{}, err
		}
		return ok, nil
	}
}

func TestEffects(t *testing.T) {
	e := newEnv(t)
	e.exec(`INSERT INTO orders (tenant_id, id, status) VALUES ('t1', 'o1', 'open'), ('t1', 'o2', 'open')`,
		`INSERT INTO charges (tenant_id, id, amount, card_last4) VALUES ('t1', 'c1', 100, '4242'), ('t1', 'c2', 200, '1881')`)
	cmd := e.command("reserve-and-charge", nil, e.orders, e.reservationE, e.chargeE)

	t.Run("ST-SUP-RP10-LC05-T01 A write of a command that declares no permission makes no decision call", func(t *testing.T) {
		_, err := e.run(cmd, e.request("t1", u7(t), "k-lc05", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			orders := repo(t, h, e.orders)
			o, err := orders.Get(ctx, "o1")
			if err != nil {
				return err
			}
			// The read is decided as a query read (ST-SUP-RP10-LC13); the writes after it make no decision.
			e.decision.taken()
			v := o.Value()
			v.Status = "reserved"
			if _, err := orders.Update(ctx, o, v); err != nil {
				return err
			}
			if _, err := repo(t, h, e.reservationE).Create(ctx, reservation{ID: "r-lc05", OrderID: "o1", SKU: "s"}, nil); err != nil {
				return err
			}
			if calls := e.decision.taken(); len(calls) != 0 {
				t.Errorf("the writes made the decisions %+v", calls)
			}
			return nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		if e.count(`SELECT count(*) FROM orders WHERE id = 'o1' AND status = 'reserved'`) != 1 ||
			e.count(`SELECT count(*) FROM reservations WHERE id = 'r-lc05'`) != 1 {
			t.Fatal("the writes were not made")
		}
	})

	t.Run("ST-SUP-RP10-LC05-T02 A write to an entity the command does not declare is refused without a decision", func(t *testing.T) {
		reserveOnly := e.command("reserve", nil, e.reservationE)
		h, _, end := e.bind("t1", u7(t), reserveOnly)
		defer end()
		e.decision.taken()
		_, err := Repo(h, e.chargeE)
		if err == nil || !strings.Contains(err.Error(), "Charge") {
			t.Fatalf("a write of Charge gave %v, want an error that names Charge", err)
		}
		if calls := e.decision.taken(); len(calls) != 0 {
			t.Fatalf("the refusal made the decisions %+v", calls)
		}
		if n := e.count(`SELECT count(*) FROM charges WHERE tenant_id = 't1'`); n != 2 {
			t.Fatalf("%d charges, want the 2 seeded", n)
		}
	})

	t.Run("ST-SUP-RP10-LC07-T01 A staged effect is an outbox row of the transaction", func(t *testing.T) {
		var dials atomic.Int64
		transport := http.DefaultTransport.(*http.Transport)
		dial := transport.DialContext
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			dials.Add(1)
			return dial(ctx, network, address)
		}
		defer func() { transport.DialContext = dial }()
		rt := e.runtime(nil)
		inside := -1
		rt.beforeCommit = func(ctx context.Context, tx *sql.Tx) error {
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM stego_outbox.messages WHERE destination = 'billing'`).Scan(&inside); err != nil {
				return err
			}
			return errors.New("roll back")
		}
		_, err := Run(e.ctx, rt, cmd, e.request("t1", u7(t), "k-lc07", `{}`), noBody,
			func(ctx context.Context, _ struct{}, h *Handle[testProbe]) (idempotency.Response, error) {
				c, err := repo(t, h, e.chargeE).Get(ctx, "c1")
				if err != nil {
					return idempotency.Response{}, err
				}
				return ok, h.Effects().Stage(ctx, "billing", "charge-requested", c, []byte(`{"amount":100}`))
			})
		if err == nil {
			t.Fatal("the run committed")
		}
		if inside != 1 {
			t.Fatalf("the transaction held %d billing rows, want 1", inside)
		}
		if n := e.outboxRows("t1", "billing"); n != 0 {
			t.Fatalf("%d billing rows after the rollback", n)
		}
		if dials.Load() != 0 {
			t.Fatalf("the effect dialed the network %d times", dials.Load())
		}
	})

	t.Run("ST-SUP-RP10-LC07-T02 An undeclared destination is refused", func(t *testing.T) {
		_, err := e.run(cmd, e.request("t1", u7(t), "k-lc07-mail", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			c, err := repo(t, h, e.chargeE).Get(ctx, "c1")
			if err != nil {
				return err
			}
			if err := h.Effects().Stage(ctx, "mail", "receipt", c, []byte(`{}`)); err == nil || !strings.Contains(err.Error(), "mail") {
				t.Errorf("the stage for mail gave %v, want an error that names mail", err)
			}
			return nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		if n := e.outboxRows("t1", "mail"); n != 0 {
			t.Fatalf("%d mail rows", n)
		}
	})

	t.Run("ST-SUP-RP10-LC07-T04 An effect for the authz or audit-sink destination is refused", func(t *testing.T) {
		before := e.count(`SELECT count(*) FROM stego_outbox.messages WHERE kind = 'forged'`)
		_, err := e.run(cmd, e.request("t1", u7(t), "k-lc07-reserved", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			c, err := repo(t, h, e.chargeE).Get(ctx, "c1")
			if err != nil {
				return err
			}
			for _, destination := range []string{"authz", "audit-sink"} {
				if err := h.Effects().Stage(ctx, destination, "forged", c, []byte(`{}`)); err == nil || !strings.Contains(err.Error(), destination) {
					t.Errorf("the stage for %s gave %v, want an error that names it", destination, err)
				}
			}
			return nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		if n := e.count(`SELECT count(*) FROM stego_outbox.messages WHERE kind = 'forged'`); n != before {
			t.Fatalf("%d forged rows", n-before)
		}
		if _, err := NewCommand(CommandSpec[testProbe]{Name: "forge", Probe: newTestProbe,
			Destinations: []Destination{{Name: "authz"}}}); err == nil {
			t.Fatal("a command that declares the destination authz was accepted")
		}
	})

	t.Run("ST-SUP-RP10-LC08-T01 A staged effect carries the scoped outgoing key with its entity", func(t *testing.T) {
		_, err := e.run(cmd, e.request("t1", u7(t), "k1", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			c, err := repo(t, h, e.chargeE).Get(ctx, "c1")
			if err != nil {
				return err
			}
			payload := []byte(`{"amount":1}`)
			if err := h.Effects().Stage(ctx, "billing", "charge-requested", c, payload); err != nil {
				return err
			}
			// A change of the payload after the call changes nothing that is staged.
			payload[10] = '9'
			return h.Effects().Stage(ctx, "billing", "charge-voided", c, []byte(`{}`))
		}))
		if err != nil {
			t.Fatal(err)
		}
		requested := e.outgoingKey("t1", "billing", "charge-requested", "charge:c1")
		voided := e.outgoingKey("t1", "billing", "charge-voided", "charge:c1")
		want := idempotency.Derive("t1", "user:u-7", lp("k1", "c1"), "billing", "charge-requested")
		if len(requested) != 1 || requested[0] != want {
			t.Fatalf("the key of charge-requested is %v, want %s", requested, want)
		}
		if len(voided) != 1 || voided[0] == requested[0] {
			t.Fatalf("the key of charge-voided is %v, the same as the first", voided)
		}
		if n := e.count(`SELECT count(*) FROM stego_outbox.messages WHERE kind = 'charge-requested' AND convert_from(payload, 'UTF8')::jsonb->>'amount' = '1'`); n != 1 {
			t.Fatal("the staged payload changed after the call")
		}
	})

	t.Run("ST-SUP-RP10-LC08-T03 A second effect of one name for one entity in one call is refused", func(t *testing.T) {
		_, err := e.run(cmd, e.request("t1", u7(t), "k-twice", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			charges := repo(t, h, e.chargeE)
			c1, err := charges.Get(ctx, "c1")
			if err != nil {
				return err
			}
			c2, err := charges.Get(ctx, "c2")
			if err != nil {
				return err
			}
			if err := h.Effects().Stage(ctx, "billing", "charge-refunded", c1, []byte(`{}`)); err != nil {
				return err
			}
			if err := h.Effects().Stage(ctx, "billing", "charge-refunded", c1, []byte(`{}`)); err == nil {
				t.Error("a second charge-refunded for c1 was staged")
			}
			return h.Effects().Stage(ctx, "billing", "charge-refunded", c2, []byte(`{}`))
		}))
		if err != nil {
			t.Fatal(err)
		}
		first := e.outgoingKey("t1", "billing", "charge-refunded", "charge:c1")
		second := e.outgoingKey("t1", "billing", "charge-refunded", "charge:c2")
		if len(first) != 1 || len(second) != 1 || first[0] == second[0] {
			t.Fatalf("the rows of c1 and c2 have the keys %v and %v, want one row each with two keys", first, second)
		}
	})

	t.Run("ST-SUP-RP10-LC08-T04 An effect with no loaded value cannot be staged", func(t *testing.T) {
		err := buildFill(t, `
func charge(ctx context.Context, h *command.Handle[struct{}]) error {
	return gen.ChargeRequested(ctx, h)
}`)
		if err == nil || !strings.Contains(err.Error(), "not enough arguments") {
			t.Fatalf("a call of the effect method with no loaded value gave %v, want a compile error", err)
		}
		if err := buildFill(t, `
func charge(ctx context.Context, h *command.Handle[struct{}], c gen.LoadedCharge) error {
	return gen.ChargeRequested(ctx, h, c)
}`); err != nil {
			t.Fatalf("the control fill does not compile: %v", err)
		}
		var other Loaded[charge]
		h, _, end := e.bind("t1", u7(t), cmd)
		other, err = repo(t, h, e.chargeE).Get(e.ctx, "c1")
		end()
		if err != nil {
			t.Fatal(err)
		}
		before := e.outboxRows("t1", "billing")
		_, err = e.run(cmd, e.request("t1", u7(t), "k-other", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			if err := h.Effects().Stage(ctx, "billing", "charge-requested", other, []byte(`{}`)); err == nil {
				t.Error("an effect for a loaded value of another handle was staged")
			}
			return nil
		}))
		if !errors.Is(err, ErrInternal) {
			t.Fatalf("the run gave %v, want an internal error", err)
		}
		if n := e.outboxRows("t1", "billing"); n != before {
			t.Fatalf("%d rows were staged", n-before)
		}
	})

	t.Run("ST-SUP-RP10-LC13-T01 A denied read in a command returns no data", func(t *testing.T) {
		e.decision.setAllow(func(_, action, resource string) bool { return !(action == "view" && resource == "order:o2") })
		defer e.decision.setAllow(nil)
		h, _, end := e.bind("t1", u7(t), cmd)
		defer end()
		e.decision.taken()
		got, err := repo(t, h, e.orders).Get(e.ctx, "o2")
		if !errors.Is(err, ErrNotFound) || got != nil {
			t.Fatalf("the read of the denied o2 gave %v, %v; want not found and no data", got, err)
		}
		// A denied row and a missing row give the same answer, each after one decision.
		missing, err2 := repo(t, h, e.orders).Get(e.ctx, "o-none")
		if missing != nil || !errors.Is(err2, ErrNotFound) || strings.Replace(err.Error(), "o2", "", 1) != strings.Replace(err2.Error(), "o-none", "", 1) {
			t.Fatalf("a missing row gave %v, a denied one %v", err2, err)
		}
		if calls := e.decision.taken(); len(calls) != 2 || calls[1].Resource != "order:o-none" {
			t.Fatalf("the two reads made the decisions %+v, want one each", calls)
		}
		// A key that does not have the form of the key type, here a uuid, is not found after the same decision, and
		// the handle goes on.
		e.exec(`CREATE TABLE tokens (tenant_id text NOT NULL, id uuid NOT NULL, label text NOT NULL,
  version bigint NOT NULL DEFAULT 1, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id))`,
			"GRANT SELECT ON tokens TO "+e.roles[0],
			`INSERT INTO tokens (tenant_id, id, label) VALUES ('t1', '0b6f3f5e-0b7c-4b4e-9a53-3f2c1f0e2d11', 'first')`)
		ts := spec("Token", "token", "tokens", []string{"label"},
			func(s Scanner) (order, Meta, error) {
				var v order
				var m Meta
				err := s.Scan(&v.ID, &v.Status, &m.Version, &m.Token)
				m.ID = v.ID
				return v, m, err
			},
			func(v order) []any { return []any{v.ID, v.Status} },
			func(v order) map[string]any { return map[string]any{"id": v.ID, "label": v.Status} })
		ts.Select = `SELECT id::text, label, version, authz_token FROM tokens WHERE tenant_id = $1 AND id = $2::uuid`
		ts.ValidID = func(id string) bool { _, err := uuid.Parse(id); return err == nil }
		tokens, err := NewEntity(ts)
		if err != nil {
			t.Fatal(err)
		}
		th, _, tend := e.bind("t1", u7(t), e.command("read-tokens", nil, tokens))
		defer tend()
		e.decision.taken()
		bad, err3 := repo(t, th, tokens).Get(e.ctx, "not-a-uuid")
		if bad != nil || !errors.Is(err3, ErrNotFound) || strings.Replace(err3.Error(), "Token", "", 1) != strings.Replace(err2.Error(), "Order", "", 1) {
			t.Fatalf("a malformed key gave %v, %v; want not found", bad, err3)
		}
		if calls := e.decision.taken(); len(calls) != 1 || calls[0].Resource != "token:not-a-uuid" {
			t.Fatalf("the malformed key made the decisions %+v, want one", calls)
		}
		if got, err := repo(t, th, tokens).Get(e.ctx, "0b6f3f5e-0b7c-4b4e-9a53-3f2c1f0e2d11"); err != nil || got.Value().Status != "first" {
			t.Fatalf("a read after the malformed key gave %v, %v; the handle broke", got, err)
		}
	})

	t.Run("ST-SUP-RP10-LC13-T02 A pii read in a command is audited and stays after a rollback", func(t *testing.T) {
		before := e.auditRows("t1", "")
		_, err := e.run(cmd, e.request("t1", u7(t), "k-pii", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			if _, err := repo(t, h, e.chargeE).Get(ctx, "c2"); err != nil {
				return err
			}
			if _, err := repo(t, h, e.reservationE).Create(ctx, reservation{ID: "r-pii", OrderID: "o1", SKU: "s"}, nil); err != nil {
				return err
			}
			return errors.New("refused")
		}))
		if err == nil {
			t.Fatal("the run committed")
		}
		if n := e.auditRows("t1", "") - before; n != 1 {
			t.Fatalf("%d new audit rows, want only the read row", n)
		}
		var action, actor, resource, row string
		if err := e.admin.QueryRowContext(e.ctx, `SELECT action, actor_id, resource_type || ':' || resource_id, to_jsonb(a)::text
 FROM audit_event a WHERE tenant_id = 't1' ORDER BY seq DESC LIMIT 1`).Scan(&action, &actor, &resource, &row); err != nil {
			t.Fatal(err)
		}
		if action != "read" || actor != "user:u-7" || resource != "charge:c2" || strings.Contains(row, "1881") {
			t.Fatalf("the read row is %s by %s on %s: %s", action, actor, resource, row)
		}
		// A list of a pii entity is audited for each resource it returns.
		charges, err := NewList(e.chargeE, ListSpec{Name: "all",
			Read: `SELECT id, amount, card_last4, version, authz_token FROM charges WHERE tenant_id = $1 ORDER BY id LIMIT $2`})
		if err != nil {
			t.Fatal(err)
		}
		before = e.auditRows("t1", "read")
		listed := 0
		_, err = e.run(cmd, e.request("t1", u7(t), "k-pii-list", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			got, err := repo(t, h, e.chargeE).List(ctx, charges, 10)
			listed = len(got)
			return err
		}))
		if err != nil || listed < 2 || e.auditRows("t1", "read")-before != listed {
			t.Fatalf("a list of %d charges gave %v and %d read rows", listed, err, e.auditRows("t1", "read")-before)
		}
	})
}
