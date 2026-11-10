package command

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"stego.dev/runtime/authz"
	"stego.dev/runtime/idempotency"
)

// orderResource is the generated resource declaration of a command on an order: its token statement share-locks
// the row.
func orderResource() *Resource {
	return &Resource{Type: "order", Token: `SELECT authz_token FROM orders WHERE tenant_id = $1 AND id = $2 FOR SHARE`,
		ValidID: func(id string) bool { return id != "" && !strings.HasPrefix(id, "bad-") }}
}

// consistencyOf is the consistency of an evaluation as text: fresh, or at_least and the token.
func consistencyOf(e evaluation) string {
	if token, ok := e.Consistency.Token(); ok {
		return "at_least " + token.Text()
	}
	if e.Consistency.IsFresh() {
		return "fresh"
	}
	return "none"
}

func TestCommandPermission(t *testing.T) {
	t.Run("ST-SUP-RP10-LC31-T01 A denied command does not reach the fill", func(t *testing.T) {
		e := newEnv(t)
		e.exec(`INSERT INTO orders (tenant_id, id, status, authz_token) VALUES ('t1', 'o5', 'open', 't5'),
  ('t2', 'o5', 'open', 't5'), ('t1', 'o7', 'open', 't7')`)
		cmd := e.command("reserve", func(s *CommandSpec[testProbe]) {
			s.Permission, s.Resource = "reserve", orderResource()
		}, e.reservationE)
		// The provider holds one relationship: user:u-7 may reserve on order o5 of t1.
		e.decision.setRelations(relation(t, "user:u-7", "reserve", "t1", "order", "o5"))
		calls := 0
		fill := func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
			calls++
			_, err := repo(t, h, e.reservationE).Create(ctx, reservation{ID: "r-" + h.Principal().Subject().ID,
				OrderID: "o5", SKU: "sku-1"}, nil)
			return ok, err
		}
		run := func(tenantID, subject, key, resource string) (Result, error) {
			req := e.request(tenantID, person(t, subject), key, `{}`)
			req.Resource = resource
			return e.run(cmd, req, fill)
		}
		denyCase := func(name, tenantID, subject, key, resource string, decisions int) {
			t.Helper()
			before := calls
			e.decision.taken()
			if _, err := run(tenantID, subject, key, resource); !errors.Is(err, ErrDenied) {
				t.Fatalf("%s gave %v, want ErrDenied", name, err)
			}
			if calls != before || e.keyRows(key) != 0 {
				t.Fatalf("%s called the fill or wrote a key row", name)
			}
			if taken := e.decision.taken(); len(taken) != decisions {
				t.Fatalf("%s asked the port %d times, want %d: %+v", name, len(taken), decisions, taken)
			}
		}

		denyCase("another principal", "t1", "user:u-9", "k-lc31-denied", "o5", 1)
		if n := e.count(`SELECT count(*) FROM reservations`); n != 0 {
			t.Fatalf("the denied command wrote %d reservations", n)
		}
		denyCase("the same ID in another tenant", "t2", "user:u-7", "k-lc31-tenant", "o5", 1)
		denyCase("an order the provider holds no relation of", "t1", "user:u-7", "k-lc31-o7", "o7", 1)
		denyCase("a missing order", "t1", "user:u-7", "k-lc31-missing", "o404", 1)
		denyCase("an ID that ValidID refuses", "t1", "user:u-7", "k-lc31-bad", "bad-o5", 0)
		denyCase("an ID too long for a resource ID", "t1", "user:u-7", "k-lc31-long", strings.Repeat("o", 2000), 0)

		e.decision.taken()
		if _, err := run("t1", "user:u-7", "k-lc31-allowed", "o5"); err != nil {
			t.Fatalf("the allowed command gave %v", err)
		}
		if calls != 1 || e.keyRows("k-lc31-allowed") != 1 {
			t.Fatalf("the allowed command called the fill %d times and wrote %d key rows, want one each", calls, e.keyRows("k-lc31-allowed"))
		}
		taken := e.decision.taken()
		if len(taken) != 1 || taken[0].Subject != "user:u-7" || taken[0].Resource != "order:o5" || consistencyOf(taken[0]) != "fresh" {
			t.Fatalf("the allowed command was decided as %+v, want user:u-7 on order:o5, fresh while the floor is zero", taken)
		}

		// With a raised floor, an existing and a missing order are decided at the same consistency, the floor.
		e.exec(`UPDATE authz.authz_floor SET token = 't9'`)
		e.decision.taken()
		if _, err := run("t1", "user:u-7", "k-lc31-floor", "o7"); !errors.Is(err, ErrDenied) {
			t.Fatalf("the existing order o7 gave %v", err)
		}
		if _, err := run("t1", "user:u-7", "k-lc31-floor-missing", "o404"); !errors.Is(err, ErrDenied) {
			t.Fatalf("the missing order gave %v", err)
		}
		taken = e.decision.taken()
		if len(taken) != 2 || consistencyOf(taken[0]) != "at_least t9" || consistencyOf(taken[1]) != "at_least t9" {
			t.Fatalf("the existing and the missing order were decided as %+v, want both at least as fresh as t9", taken)
		}

		// The permission is revoked: a replay of the completed key is decided too, before the wrapper reads the
		// key, so the stored response is not given.
		e.decision.setRelations()
		if result, err := run("t1", "user:u-7", "k-lc31-allowed", "o5"); !errors.Is(err, ErrDenied) || result.Replayed || calls != 1 {
			t.Fatalf("a revoked replay gave %+v, %v with %d fill calls, want ErrDenied and no stored response", result, err, calls)
		}

		for name, token := range map[string]string{
			"no lock":           `SELECT authz_token FROM orders WHERE tenant_id = $1 AND id = $2`,
			"$12 for $1":        `SELECT authz_token FROM orders WHERE tenant_id = $12 AND id = $2 FOR SHARE`,
			"a third parameter": `SELECT authz_token FROM orders WHERE tenant_id = $1 AND id = $2 AND status = $3 FOR SHARE`,
			"an update lock":    `SELECT authz_token FROM orders WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
			"no tenant at all":  `SELECT authz_token FROM orders WHERE id = $2 FOR SHARE`,
			"FOR KEY SHARE, which does not block the token update": `SELECT authz_token FROM orders WHERE tenant_id = $1 AND id = $2 FOR KEY SHARE`,
			"a lock clause in a comment":                           `SELECT authz_token FROM orders WHERE tenant_id = $1 AND id = $2 -- FOR SHARE`,
			"an OR that widens the read":                           `SELECT authz_token FROM orders WHERE tenant_id = $1 AND id = $2 OR true FOR SHARE`,
			"a UNION":                                              `SELECT authz_token FROM orders WHERE tenant_id = $1 AND id = $2 UNION SELECT authz_token FROM orders WHERE tenant_id = $1 FOR SHARE`,
			"a quoted literal":                                     `SELECT authz_token FROM orders WHERE tenant_id = $1 AND id = $2 AND status <> 'x' FOR SHARE`,
			"a dollar quote":                                       `SELECT authz_token FROM orders WHERE tenant_id = $1 AND id = $2 AND status <> $$x$$ FOR SHARE`,
			"a WITH":                                               `WITH o AS (SELECT * FROM orders) SELECT authz_token FROM o WHERE tenant_id = $1 AND id = $2 FOR SHARE`,
			"a TABLE":                                              `TABLE orders`,
			"a VALUES":                                             `SELECT authz_token FROM orders WHERE tenant_id = $1 AND id = $2 FOR SHARE; VALUES ($1, $2)`,
			"a second statement":                                   `SELECT authz_token FROM orders WHERE tenant_id = $1 AND id = $2 FOR SHARE; SELECT 1`,
			"a placeholder in a comment":                           `SELECT authz_token FROM orders WHERE tenant_id = $1 /* $2 */ AND id = 'o5' FOR SHARE`,
			"a subselect":                                          `SELECT authz_token FROM orders WHERE tenant_id = $1 AND id = (SELECT $2::text) FOR SHARE`,
		} {
			if _, err := NewCommand(CommandSpec[testProbe]{Name: "bad", Probe: newTestProbe, Permission: "reserve",
				Resource: &Resource{Type: "order", Token: token, ValidID: func(string) bool { return true }}}); err == nil {
				t.Errorf("a token statement with %s was accepted", name)
			}
		}

		for _, token := range []string{`SELECT authz_token FROM app.orders WHERE tenant_id = $1 AND id = $2::uuid FOR SHARE`,
			"select authz_token\n  from orders\n where tenant_id = $1 and id = $2 for share;"} {
			if _, err := NewCommand(CommandSpec[testProbe]{Name: "good", Probe: newTestProbe, Permission: "reserve",
				Resource: &Resource{Type: "order", Token: token, ValidID: func(string) bool { return true }}}); err != nil {
				t.Errorf("the token statement %q was refused: %v", token, err)
			}
		}
		if _, err := NewCommand(CommandSpec[testProbe]{Name: "half", Probe: newTestProbe, Permission: "reserve"}); err == nil {
			t.Fatal("a command with a permission and no resource was accepted")
		}
	})

	t.Run("ST-SUP-RP10-LC31-T02 A revocation waits for the decided command to end", func(t *testing.T) {
		e := newEnv(t)
		e.exec(`INSERT INTO orders (tenant_id, id, status, authz_token) VALUES ('t1', 'o5', 'open', 't5')`)
		cmd := e.command("reserve", func(s *CommandSpec[testProbe]) {
			s.Permission, s.Resource = "reserve", orderResource()
		}, e.reservationE)
		e.decision.setRelations(relation(t, "user:u-7", "reserve", "t1", "order", "o5"), relation(t, "user:u-7", "view", "t1", "order", "o5"))
		inFill, release := make(chan struct{}), make(chan struct{})
		done := make(chan error, 1)
		go func() {
			req := e.request("t1", u7(t), "k-lc31-race", `{}`)
			req.Resource = "o5"
			_, err := e.run(cmd, req, func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
				close(inFill)
				<-release
				return ok, nil
			})
			done <- err
		}()
		<-inFill
		// The relay stores the token of a revocation on the row. It must wait until the decided command ends.
		wrote := make(chan time.Duration, 1)
		go func() {
			start := time.Now()
			_, _ = e.admin.ExecContext(context.Background(), `UPDATE orders SET authz_token = 't6' WHERE tenant_id = 't1' AND id = 'o5'`)
			wrote <- time.Since(start)
		}()
		select {
		case <-wrote:
			t.Fatal("the revocation's token was written while the command that decided on the row was running")
		case <-time.After(300 * time.Millisecond):
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatalf("the command gave %v", err)
		}
		if d := <-wrote; d < 300*time.Millisecond {
			t.Fatalf("the token write took %s, want it to wait for the command", d)
		}

		// The parent of a create: the command's resource is the order the new reservation goes under, read FOR
		// SHARE too, so the revocation of the parent waits as well.
		create := e.command("add-reservation", func(s *CommandSpec[testProbe]) {
			s.Permission, s.Resource = "reserve", orderResource()
		}, e.underOrder, e.orders)
		e.exec(`UPDATE orders SET authz_token = 't5' WHERE tenant_id = 't1' AND id = 'o5'`)
		inFill, release = make(chan struct{}), make(chan struct{})
		go func() {
			req := e.request("t1", u7(t), "k-lc31-parent", `{}`)
			req.Resource = "o5"
			_, err := e.run(create, req, func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
				r, err := Repo(h, e.underOrder)
				if err != nil {
					return ok, err
				}
				orders, err := Repo(h, e.orders)
				if err != nil {
					return ok, err
				}
				o5, err := orders.Get(ctx, "o5")
				if err != nil {
					return ok, err
				}
				if _, err := r.Create(ctx, reservation{ID: "r-parent", OrderID: "o5", SKU: "s"}, o5); err != nil {
					return ok, err
				}
				close(inFill)
				<-release
				return ok, nil
			})
			done <- err
		}()
		select {
		case <-inFill:
		case err := <-done:
			t.Fatalf("the create command ended before its fill waited: %v", err)
		}
		go func() {
			start := time.Now()
			_, _ = e.admin.ExecContext(context.Background(), `UPDATE orders SET authz_token = 't6' WHERE tenant_id = 't1' AND id = 'o5'`)
			wrote <- time.Since(start)
		}()
		select {
		case <-wrote:
			t.Fatal("the parent's token was written while the create that decided on it was running")
		case <-time.After(300 * time.Millisecond):
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatalf("the create command gave %v", err)
		}
		<-wrote
	})
}

func TestCommandPermissionOnRows(t *testing.T) {
	t.Run("ST-SUP-RP10-LC05-T03 A fill cannot act on another row without the command's permission on it", func(t *testing.T) {
		e := newEnv(t)
		e.exec(`INSERT INTO orders (tenant_id, id, status, authz_token) VALUES ('t1', 'o5', 'open', 't5'), ('t1', 'o6', 'open', 't6')`)
		cmd := e.command("cancel", func(s *CommandSpec[testProbe]) {
			s.Permission, s.Resource = "cancel", orderResource()
		}, e.orders)
		// user:u-7 may view both orders but cancel only o5.
		e.decision.setRelations(relation(t, "user:u-7", "cancel", "t1", "order", "o5"),
			relation(t, "user:u-7", "view", "t1", "order", "o5"), relation(t, "user:u-7", "view", "t1", "order", "o6"))
		act := func(id string, change func(ctx context.Context, r *Repository[order], id string) error) error {
			req := e.request("t1", u7(t), "k-lc05-"+id+"-"+time.Now().Format("150405.000000"), `{}`)
			req.Resource = "o5"
			_, err := e.run(cmd, req, func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
				return ok, change(ctx, repo(t, h, e.orders), id)
			})
			return err
		}
		update := func(ctx context.Context, r *Repository[order], id string) error {
			o, err := r.GetForUpdate(ctx, id)
			if err != nil {
				return err
			}
			_, err = r.Update(ctx, o, order{ID: id, Status: "cancelled"})
			return err
		}
		plainUpdate := func(ctx context.Context, r *Repository[order], id string) error {
			o, err := r.Get(ctx, id)
			if err != nil {
				return err
			}
			_, err = r.Update(ctx, o, order{ID: id, Status: "cancelled"})
			return err
		}
		del := func(ctx context.Context, r *Repository[order], id string) error {
			o, err := r.Get(ctx, id)
			if err != nil {
				return err
			}
			return r.Delete(ctx, o)
		}
		lockedRead := func(ctx context.Context, r *Repository[order], id string) error {
			_, err := r.GetForUpdate(ctx, id)
			return err
		}
		for name, change := range map[string]func(context.Context, *Repository[order], string) error{
			"a locked read": lockedRead, "a locked read and update": update, "a read and update": plainUpdate, "a delete": del} {
			if err := act("o6", change); !errors.Is(err, ErrNotFound) {
				t.Fatalf("%s of o6 gave %v, want not found", name, err)
			}
		}
		if n := e.count(`SELECT count(*) FROM orders WHERE id = 'o6' AND status = 'open'`); n != 1 {
			t.Fatal("the fill changed o6")
		}
		// A plain read of o6 needs only view.
		if err := act("o6", func(ctx context.Context, r *Repository[order], id string) error {
			_, err := r.Get(ctx, id)
			return err
		}); err != nil {
			t.Fatalf("a read of o6 gave %v", err)
		}
		if err := act("o5", update); err != nil {
			t.Fatalf("the update of the decided order o5 gave %v", err)
		}
		// With cancel on o6 too, the fill may change it.
		e.decision.setRelations(relation(t, "user:u-7", "cancel", "t1", "order", "o5"),
			relation(t, "user:u-7", "cancel", "t1", "order", "o6"), relation(t, "user:u-7", "view", "t1", "order", "o6"))
		if err := act("o6", update); err != nil {
			t.Fatalf("the update of o6 with cancel on it gave %v", err)
		}

		// A locked list keeps only the rows on which the command's permission is allowed too.
		e.seedOrg("t1", "org1", 2)
		e.seedUser("t1", "u1", "org1", true, 2, 1)
		e.seedUser("t1", "u2", "org1", true, 1, 1)
		e.decision.setRelations(relation(t, "user:u-7", "cancel", "t1", "order", "o5"),
			relation(t, "user:u-7", "view", "t1", "user", "u1"), relation(t, "user:u-7", "view", "t1", "user", "u2"),
			relation(t, "user:u-7", "cancel", "t1", "user", "u2"))
		withUsers := e.command("cancel", func(s *CommandSpec[testProbe]) {
			s.Permission, s.Resource = "cancel", orderResource()
		}, e.orders, e.users)
		req := e.request("t1", u7(t), "k-lc05-list", `{}`)
		req.Resource = "o5"
		var listed []string
		if _, err := e.run(withUsers, req, func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
			rows, err := repo(t, h, e.users).ListForUpdate(ctx, e.admins, nil, "org1")
			for _, u := range rows {
				listed = append(listed, u.Value().ID)
			}
			return ok, err
		}); err != nil {
			t.Fatal(err)
		}
		if strings.Join(listed, ",") != "u2" {
			t.Fatalf("the locked list holds %v, want only u2, on which cancel is allowed", listed)
		}

	})

	// rowsEnv is the fixture of the tests of relationships, parents and effects: orders o5 and o6 of t1, which
	// user:u-7 may view, and o5, the decided resource, which it may cancel.
	rowsEnv := func(t *testing.T) (*env, func(name string, fill func(ctx context.Context, h *Handle[testProbe]) error) error,
		func(ctx context.Context, h *Handle[testProbe], id string) Loaded[order]) {
		e := newEnv(t)
		e.exec(`INSERT INTO orders (tenant_id, id, status, authz_token) VALUES ('t1', 'o5', 'open', 't5'), ('t1', 'o6', 'open', 't6')`)
		e.decision.setRelations(relation(t, "user:u-7", "cancel", "t1", "order", "o5"),
			relation(t, "user:u-7", "view", "t1", "order", "o5"), relation(t, "user:u-7", "view", "t1", "order", "o6"))
		owner := authz.Relation{ResourceType: "order", Name: "owner"}
		full := e.command("cancel", func(s *CommandSpec[testProbe]) {
			s.Permission, s.Resource, s.Relations = "cancel", orderResource(), []authz.Relation{owner}
		}, e.orders, e.underOrder, e.reservationE)
		step := func(name string, fill func(ctx context.Context, h *Handle[testProbe]) error) error {
			t.Helper()
			req := e.request("t1", u7(t), "k-lc05-"+name, `{}`)
			req.Resource = "o5"
			_, err := e.run(full, req, func(ctx context.Context, h *Handle[testProbe]) (idempotency.Response, error) {
				return ok, fill(ctx, h)
			})
			return err
		}
		get := func(ctx context.Context, h *Handle[testProbe], id string) Loaded[order] {
			o, err := repo(t, h, e.orders).Get(ctx, id)
			if err != nil {
				t.Fatalf("read %s: %v", id, err)
			}
			return o
		}
		return e, step, get
	}

	t.Run("ST-SUP-RP10-LC05-T04 A fill cannot change the relationships of a row it may not act on", func(t *testing.T) {
		e, step, get := rowsEnv(t)
		ownerTuple := func(id string) authz.Tuple {
			r, err := authz.NewResource(mustTenant(t, "t1"), "order", id)
			if err != nil {
				t.Fatal(err)
			}
			return authz.Tuple{Resource: r, Relation: "owner", Subject: authz.Subject{Type: "user", ID: "u-9"}}
		}
		before := e.outboxRows("t1", "authz")
		if err := step("relate-o6", func(ctx context.Context, h *Handle[testProbe]) error {
			_, err := h.Authz().Relate(ctx, get(ctx, h, "o6"), ownerTuple("o6"))
			return err
		}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("making u-9 owner of o6 gave %v, want not found", err)
		}
		if err := step("relate-other", func(ctx context.Context, h *Handle[testProbe]) error {
			_, err := h.Authz().Relate(ctx, get(ctx, h, "o5"), ownerTuple("o6"))
			return err
		}); !errors.Is(err, ErrInternal) {
			t.Fatalf("a tuple of o6 on the loaded o5 gave %v, want an internal error", err)
		}
		if err := step("unrelate-o6", func(ctx context.Context, h *Handle[testProbe]) error {
			_, err := h.Authz().Unrelate(ctx, get(ctx, h, "o6"), ownerTuple("o6"))
			return err
		}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("an unrelate on o6 gave %v, want not found", err)
		}
		if n := e.outboxRows("t1", "authz"); n != before {
			t.Fatalf("the refused relationship writes left %d authz messages", n-before)
		}
		if err := step("relate-o5", func(ctx context.Context, h *Handle[testProbe]) error {
			_, err := h.Authz().Relate(ctx, get(ctx, h, "o5"), ownerTuple("o5"))
			return err
		}); err != nil || e.outboxRows("t1", "authz") != before+1 {
			t.Fatalf("making u-9 owner of the decided o5 gave %v", err)
		}

	})

	t.Run("ST-SUP-RP10-LC05-T05 A create takes only the parent that the new row names", func(t *testing.T) {
		e, step, get := rowsEnv(t)
		if err := step("create-wrong-parent", func(ctx context.Context, h *Handle[testProbe]) error {
			_, err := repo(t, h, e.underOrder).Create(ctx, reservation{ID: "r-o6", OrderID: "o6", SKU: "s"}, get(ctx, h, "o5"))
			return err
		}); !errors.Is(err, ErrInternal) {
			t.Fatalf("a reservation of o6 under the parent o5 gave %v, want an internal error", err)
		}
		if err := step("create-no-parent", func(ctx context.Context, h *Handle[testProbe]) error {
			_, err := repo(t, h, e.underOrder).Create(ctx, reservation{ID: "r-none", OrderID: "o6", SKU: "s"}, nil)
			return err
		}); !errors.Is(err, ErrInternal) {
			t.Fatalf("a command's reservation with no parent gave %v, want an internal error", err)
		}
		if err := step("create-under-o6", func(ctx context.Context, h *Handle[testProbe]) error {
			_, err := repo(t, h, e.underOrder).Create(ctx, reservation{ID: "r-o6", OrderID: "o6", SKU: "s"}, get(ctx, h, "o6"))
			return err
		}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a reservation under o6, on which cancel is not allowed, gave %v, want not found", err)
		}
		if n := e.count(`SELECT count(*) FROM reservations WHERE id IN ('r-o6', 'r-none')`); n != 0 {
			t.Fatalf("%d refused reservations were written", n)
		}
		if err := step("create-under-o5", func(ctx context.Context, h *Handle[testProbe]) error {
			_, err := repo(t, h, e.underOrder).Create(ctx, reservation{ID: "r-o5", OrderID: "o5", SKU: "s"}, get(ctx, h, "o5"))
			return err
		}); err != nil {
			t.Fatalf("a reservation under the decided o5 gave %v", err)
		}

	})

	t.Run("ST-SUP-RP10-LC05-T06 An effect is only for a row the command may act on", func(t *testing.T) {
		e, step, get := rowsEnv(t)
		e.exec(`INSERT INTO reservations (tenant_id, id, order_id, sku) VALUES ('t1', 'r-o5', 'o5', 's')`)
		if err := step("stage-o6", func(ctx context.Context, h *Handle[testProbe]) error {
			return h.Effects().Stage(ctx, "billing", "cancelled", get(ctx, h, "o6"), []byte(`{}`))
		}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("an effect for o6 gave %v, want not found", err)
		}
		if err := step("stage-read", func(ctx context.Context, h *Handle[testProbe]) error {
			r, err := repo(t, h, e.reservationE).Get(ctx, "r-o5")
			if err != nil {
				return err
			}
			return h.Effects().Stage(ctx, "billing", "noted", r, []byte(`{}`))
		}); err == nil || !strings.Contains(err.Error(), "locked or wrote") {
			t.Fatalf("an effect for a reservation that was only read gave %v, want a refusal", err)
		}
		if err := step("stage-written", func(ctx context.Context, h *Handle[testProbe]) error {
			r, err := repo(t, h, e.reservationE).Create(ctx, reservation{ID: "r-written", OrderID: "o5", SKU: "s"}, nil)
			if err != nil {
				return err
			}
			if err := h.Effects().Stage(ctx, "billing", "noted", r, []byte(`{}`)); err != nil {
				return err
			}
			return h.Effects().Stage(ctx, "billing", "cancelled", get(ctx, h, "o5"), []byte(`{}`))
		}); err != nil {
			t.Fatalf("effects for a written reservation and the decided o5 gave %v", err)
		}
	})

	// The child rows of the probes: r5 under o5, which the command may act on, and r6 under o6, which it may not.
	children := func(t *testing.T) (*env, func(string, func(context.Context, *Handle[testProbe]) error) error,
		func(context.Context, *Handle[testProbe], string) Loaded[order]) {
		e, step, get := rowsEnv(t)
		e.exec(`INSERT INTO reservations (tenant_id, id, order_id, sku) VALUES ('t1', 'r5', 'o5', 's'), ('t1', 'r6', 'o6', 's')`)
		return e, step, get
	}

	t.Run("ST-SUP-RP10-LC05-T07 An update of a child row decides on its parent", func(t *testing.T) {
		e, step, get := children(t)
		for name, c := range map[string]struct {
			fill func(ctx context.Context, h *Handle[testProbe]) error
			want error
		}{
			"a locked read with no parent": {func(ctx context.Context, h *Handle[testProbe]) error {
				_, err := repo(t, h, e.underOrder).GetForUpdate(ctx, "r6")
				return err
			}, ErrInternal},
			"a locked read under o6": {func(ctx context.Context, h *Handle[testProbe]) error {
				_, err := repo(t, h, e.underOrder).GetForUpdate(ctx, "r6", get(ctx, h, "o6"))
				return err
			}, ErrNotFound},
			"a locked list under o6": {func(ctx context.Context, h *Handle[testProbe]) error {
				_, err := repo(t, h, e.underOrder).ListForUpdate(ctx, e.ofOrder, get(ctx, h, "o6"), "o6")
				return err
			}, ErrNotFound},
			"a locked list whose rows name another parent": {func(ctx context.Context, h *Handle[testProbe]) error {
				_, err := repo(t, h, e.underOrder).ListForUpdate(ctx, e.ofOrder, get(ctx, h, "o5"), "o6")
				return err
			}, ErrInternal},
			"an update under o6": {func(ctx context.Context, h *Handle[testProbe]) error {
				r := repo(t, h, e.underOrder)
				v, err := r.Get(ctx, "r6")
				if err != nil {
					return err
				}
				_, err = r.Update(ctx, v, reservation{ID: "r6", OrderID: "o6", SKU: "changed"}, get(ctx, h, "o6"))
				return err
			}, ErrNotFound},
		} {
			if err := step("update-"+strings.ReplaceAll(name, " ", "-"), c.fill); !errors.Is(err, c.want) {
				t.Fatalf("%s gave %v, want %v", name, err, c.want)
			}
		}
		if n := e.count(`SELECT count(*) FROM reservations WHERE id = 'r6' AND sku = 's'`); n != 1 {
			t.Fatal("the fill changed r6")
		}
		if err := step("update-r5", func(ctx context.Context, h *Handle[testProbe]) error {
			r := repo(t, h, e.underOrder)
			o5 := get(ctx, h, "o5")
			v, err := r.GetForUpdate(ctx, "r5", o5)
			if err != nil {
				return err
			}
			if _, err := r.ListForUpdate(ctx, e.ofOrder, o5, "o5"); err != nil {
				return err
			}
			_, err = r.Update(ctx, v, reservation{ID: "r5", OrderID: "o5", SKU: "changed"}, o5)
			return err
		}); err != nil {
			t.Fatalf("an update of r5 under the decided o5 gave %v", err)
		}
	})

	t.Run("ST-SUP-RP10-LC05-T08 A child row moves only to a parent the command may act on", func(t *testing.T) {
		e, step, get := children(t)
		move := func(parents ...string) func(ctx context.Context, h *Handle[testProbe]) error {
			return func(ctx context.Context, h *Handle[testProbe]) error {
				r := repo(t, h, e.underOrder)
				var refs []Ref
				for _, p := range parents {
					refs = append(refs, get(ctx, h, p))
				}
				v, err := r.GetForUpdate(ctx, "r5", refs[0])
				if err != nil {
					return err
				}
				_, err = r.Update(ctx, v, reservation{ID: "r5", OrderID: "o6", SKU: "s"}, refs...)
				return err
			}
		}
		if err := step("move-to-o6", move("o5", "o6")); !errors.Is(err, ErrNotFound) {
			t.Fatalf("moving r5 under o6 gave %v, want not found", err)
		}
		if err := step("move-without-new-parent", move("o5")); !errors.Is(err, ErrInternal) {
			t.Fatalf("moving r5 with no value of the new parent gave %v, want an internal error", err)
		}
		if n := e.count(`SELECT count(*) FROM reservations WHERE id = 'r5' AND order_id = 'o5'`); n != 1 {
			t.Fatal("r5 moved")
		}
		e.decision.setRelations(relation(t, "user:u-7", "cancel", "t1", "order", "o5"), relation(t, "user:u-7", "cancel", "t1", "order", "o6"),
			relation(t, "user:u-7", "view", "t1", "order", "o5"), relation(t, "user:u-7", "view", "t1", "order", "o6"))
		if err := step("move-allowed", move("o5", "o6")); err != nil {
			t.Fatalf("moving r5 under o6 with cancel on both gave %v", err)
		}
	})

	t.Run("ST-SUP-RP10-LC05-T09 A delete of a child row decides on its parent", func(t *testing.T) {
		e, step, get := children(t)
		for name, c := range map[string]struct {
			fill func(ctx context.Context, h *Handle[testProbe]) error
			want error
		}{
			"a locked read for delete under o6": {func(ctx context.Context, h *Handle[testProbe]) error {
				_, err := repo(t, h, e.underOrder).GetForDelete(ctx, "r6", get(ctx, h, "o6"))
				return err
			}, ErrNotFound},
			"a delete under o6": {func(ctx context.Context, h *Handle[testProbe]) error {
				r := repo(t, h, e.underOrder)
				v, err := r.Get(ctx, "r6")
				if err != nil {
					return err
				}
				return r.Delete(ctx, v, get(ctx, h, "o6"))
			}, ErrNotFound},
			"a delete with no parent": {func(ctx context.Context, h *Handle[testProbe]) error {
				r := repo(t, h, e.underOrder)
				v, err := r.Get(ctx, "r6")
				if err != nil {
					return err
				}
				return r.Delete(ctx, v)
			}, ErrInternal},
		} {
			if err := step("delete-"+strings.ReplaceAll(name, " ", "-"), c.fill); !errors.Is(err, c.want) {
				t.Fatalf("%s gave %v, want %v", name, err, c.want)
			}
		}
		if n := e.count(`SELECT count(*) FROM reservations WHERE id = 'r6'`); n != 1 {
			t.Fatal("the fill deleted r6")
		}
		if err := step("delete-r5", func(ctx context.Context, h *Handle[testProbe]) error {
			r := repo(t, h, e.underOrder)
			o5 := get(ctx, h, "o5")
			v, err := r.GetForDelete(ctx, "r5", o5)
			if err != nil {
				return err
			}
			return r.Delete(ctx, v, o5)
		}); err != nil {
			t.Fatalf("a delete of r5 under the decided o5 gave %v", err)
		}
	})

	t.Run("ST-SUP-RP10-LC05-T10 An effect for a child row decides on its parent", func(t *testing.T) {
		e, step, get := children(t)
		stage := func(id string, parents ...string) func(ctx context.Context, h *Handle[testProbe]) error {
			return func(ctx context.Context, h *Handle[testProbe]) error {
				v, err := repo(t, h, e.underOrder).Get(ctx, id)
				if err != nil {
					return err
				}
				var refs []Ref
				for _, p := range parents {
					refs = append(refs, get(ctx, h, p))
				}
				return h.Effects().Stage(ctx, "billing", "noted", v, []byte(`{}`), refs...)
			}
		}
		if err := step("stage-r6", stage("r6", "o6")); !errors.Is(err, ErrNotFound) {
			t.Fatalf("an effect for r6 under o6 gave %v, want not found", err)
		}
		if err := step("stage-r6-none", stage("r6")); !errors.Is(err, ErrInternal) {
			t.Fatalf("an effect for r6 with no parent gave %v, want an internal error", err)
		}
		if err := step("stage-r6-o5", stage("r6", "o5")); !errors.Is(err, ErrInternal) {
			t.Fatalf("an effect for r6 with the parent o5 gave %v, want an internal error", err)
		}
		if n := e.count(`SELECT count(*) FROM stego_outbox.messages WHERE destination = 'billing'`); n != 0 {
			t.Fatalf("%d refused effects were enqueued", n)
		}
		if err := step("stage-r5", stage("r5", "o5")); err != nil {
			t.Fatalf("an effect for r5 under the decided o5 gave %v", err)
		}
	})
}
