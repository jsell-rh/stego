package query

import (
	"context"
	"errors"
	"testing"

	"github.com/jsell-rh/stego/registry/runtime/audit"
)

func TestReadAuditCode(t *testing.T) {
	t.Run("ST-SUP-RP11-LC16-T01 A failed read is audited with its code", func(t *testing.T) {
		e := newEnv(t)
		decision := &fakeDecision{decide: allowAll}
		rt := e.runtimeOf(t, decision)
		charges := chargeEntity(t)
		e.insert(t, "INSERT INTO app.charges (tenant_id, id, card_last4, amount) VALUES ('t1', 'ch-1', '4242', 100), ('t1', 'ch-2', '1881', 200)")
		q := mustQuery(t, "read-charge", charges)
		read := func(id string) error {
			_, err := Run(context.Background(), rt, q, bound(t, "t1", "user:u-7", q.Name()), id,
				func(ctx context.Context, id string, h *Handle[noProbe]) (int64, error) {
					repo, err := Repo(h, charges)
					if err != nil {
						return 0, err
					}
					c, err := repo.Get(ctx, id)
					if err != nil {
						return 0, err
					}
					return c.Value().Amount, nil
				})
			return err
		}
		if err := read("ch-1"); err != nil {
			t.Fatalf("the read that succeeds gave %v", err)
		}
		decision.mu.Lock()
		decision.down = true
		decision.mu.Unlock()
		if err := read("ch-2"); !errors.Is(err, ErrUndecided) {
			t.Fatalf("the read whose decision fails gave %v, want ErrUndecided", err)
		}
		rows, err := e.admin.QueryContext(context.Background(), "SELECT resource_id, outcome, error_code, coalesce(after::text, '') FROM "+
			audit.EventsTable+" WHERE action = 'read' ORDER BY seq")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		type row struct{ id, outcome, code, state string }
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.outcome, &r.code, &r.state); err != nil {
				t.Fatal(err)
			}
			got = append(got, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("the reads wrote %d read rows, want two: %+v", len(got), got)
		}
		if got[0].id != "ch-1" || got[0].outcome != "ok" || got[0].code != "" {
			t.Fatalf("the read that succeeded has the row %+v, want outcome ok and no error code", got[0])
		}
		if got[1].id != "ch-2" || got[1].outcome != "failed" || got[1].code != audit.ErrorCodeUndecided {
			t.Fatalf("the read whose decision failed has the row %+v, want outcome failed and the code %s", got[1], audit.ErrorCodeUndecided)
		}
		if got[1].state != "" && got[1].state != "null" {
			t.Fatalf("the failed read row holds the state %s, want none", got[1].state)
		}
		if n := e.count(t, "SELECT count(*) FROM "+audit.EventsTable+" WHERE row_to_json("+audit.EventsTable+")::text LIKE '%down%' OR row_to_json("+audit.EventsTable+")::text LIKE '%1881%'"); n != 0 {
			t.Fatalf("%d audit rows hold the error text or the row data", n)
		}
		// A list page whose batch decision fails writes a failed read row for each row of personal data it read.
		lq := mustListQuery(t, "list-charges", charges, "view")
		if _, err := Run(context.Background(), rt, lq, bound(t, "t1", "user:u-7", lq.Name()), struct{}{},
			func(ctx context.Context, _ struct{}, h *Handle[noProbe]) (int, error) {
				repo, err := Repo(h, charges)
				if err != nil {
					return 0, err
				}
				page, err := repo.List(ctx, ListRequest{})
				return len(page.Items), err
			}); err == nil {
			t.Fatal("the list whose decision fails gave a result")
		}
		if n := e.count(t, "SELECT count(*) FROM "+audit.EventsTable+" WHERE action = 'read' AND operation = 'list-charges' AND outcome = 'failed' AND error_code = $1", audit.ErrorCodeUndecided); n != 2 {
			t.Fatalf("the failed list wrote %d failed read rows, want one for each of the 2 rows", n)
		}
		if _, err := e.writer.RecordRead(context.Background(), e.auditDB, mustTenant(t, "t1"), audit.Event{ActorID: "user:u-7",
			ActorKind: audit.ActorUser, ResourceType: "charge", ResourceID: "ch-3", Service: "shop", ErrorCode: "the provider is down"}); err == nil {
			t.Fatal("a read row with an error text as its code was written")
		}
	})
}
