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
		} {
			if _, err := NewCommand(CommandSpec[testProbe]{Name: "bad", Probe: newTestProbe, Permission: "reserve",
				Resource: &Resource{Type: "order", Token: token, ValidID: func(string) bool { return true }}}); err == nil {
				t.Errorf("a token statement with %s was accepted", name)
			}
		}
		if _, err := NewCommand(CommandSpec[testProbe]{Name: "parent", Probe: newTestProbe, Permission: "reserve",
			Resource: &Resource{Type: "order", Token: `SELECT authz_token FROM orders WHERE tenant_id = $1 AND id = $2 FOR KEY SHARE`,
				ValidID: func(string) bool { return true }}}); err != nil {
			t.Errorf("a parent's token statement FOR KEY SHARE was refused: %v", err)
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
		e.decision.setRelations(relation(t, "user:u-7", "reserve", "t1", "order", "o5"))
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
			rows, err := repo(t, h, e.users).ListForUpdate(ctx, e.admins, "org1")
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
		_ = authz.Relation{}
	})
}
