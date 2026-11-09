package query

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"stego.dev/runtime/authz"
)

func TestStream(t *testing.T) {
	t.Run("ST-SUP-RP11-LC13-T01 A streamed query sends allowed values only, each decided when it is sent", func(t *testing.T) {
		e := newEnv(t)
		e.insert(t, "INSERT INTO app.orders (tenant_id, id, total, authz_token) VALUES ('t1', 'o-1', 1, 'r1'), ('t1', 'o-2', 2, 'r1'), ('t1', 'o-3', 3, 'r1')")
		// The fake provider allows the list on every order. It allows view on o-1 and o-3 and denies it on o-2; a
		// check of o-3 at least as fresh as the revocation token f9 sees the revocation, and an older one does not.
		port := &fakeDecision{decide: func(r authz.EvaluationRequest) bool {
			if r.Action.Name == "list_orders" {
				return true
			}
			switch r.Resource.ID {
			case "o-2":
				return false
			case "o-3":
				return consistencyOf(r.Consistency) != "at_least:f9"
			}
			return true
		}}
		rt := e.runtimeOf(t, port)
		orders := orderEntity(t, false)
		q := mustQuery(t, "watch-orders", "list_orders", orders)
		var kept *Stream[Order, noProbe]
		var keptOrder *Loaded[Order]
		run := func(beforeThird func()) ([]string, uint64) {
			var mu sync.Mutex
			var sent []string
			dropped, err := RunStream(context.Background(), rt, q, bound(t, "t1", "user:u-7", "watch-orders"), struct{}{}, orders,
				func(o Order) error {
					mu.Lock()
					sent = append(sent, o.ID)
					mu.Unlock()
					return nil
				},
				func(ctx context.Context, _ struct{}, s *Stream[Order, noProbe]) error {
					kept = s
					repo, err := Repo(s.Handle(), orders)
					if err != nil {
						return err
					}
					page, err := repo.List(ctx, ListRequest{})
					if err != nil {
						return err
					}
					if len(page.Items) != 3 {
						return errors.New("the list does not hold the three orders")
					}
					keptOrder = page.Items[0]
					for i, order := range page.Items {
						if i == 2 && beforeThird != nil {
							beforeThird()
						}
						if err := s.Send(ctx, order); err != nil {
							return err
						}
					}
					return nil
				})
			if err != nil {
				t.Fatal(err)
			}
			return sent, dropped
		}
		sent, dropped := run(nil)
		if strings.Join(sent, ",") != "o-1,o-3" || dropped != 1 {
			t.Errorf("the first run sends %v and drops %d, want o-1 and o-3 sent and o-2 dropped", sent, dropped)
		}
		sent, dropped = run(func() { e.insert(t, "UPDATE authz.authz_floor SET token = 'f9'") })
		if strings.Join(sent, ",") != "o-1" || dropped != 2 {
			t.Errorf("the second run sends %v and drops %d, want o-1 sent and o-2 and o-3 dropped", sent, dropped)
		}
		if rt.DroppedSends() != 3 {
			t.Errorf("the runtime counts %d dropped messages, want 3", rt.DroppedSends())
		}
		decided := len(port.recorded())
		if err := kept.Send(context.Background(), keptOrder); err == nil {
			t.Error("a send after the stream ended succeeds")
		}
		if len(port.recorded()) != decided {
			t.Error("a send after the stream ended reached the decision port")
		}
		// A send after the stream's context ended, while the fill still runs, sends nothing.
		ctx, cancel := context.WithCancel(context.Background())
		var afterEnd error
		var sentAfterEnd int
		_, err := RunStream(ctx, rt, q, bound(t, "t1", "user:u-7", "watch-orders"), struct{}{}, orders,
			func(Order) error { sentAfterEnd++; return nil },
			func(ctx context.Context, _ struct{}, s *Stream[Order, noProbe]) error {
				repo, err := Repo(s.Handle(), orders)
				if err != nil {
					return err
				}
				order, err := repo.Get(ctx, "o-1")
				if err != nil {
					return err
				}
				cancel()
				afterEnd = s.Send(context.Background(), order)
				return nil
			})
		_ = err
		if !errors.Is(afterEnd, ErrStreamEnded) || sentAfterEnd != 0 {
			t.Errorf("a send after the stream ended gives %v and sends %d messages, want ErrStreamEnded and none", afterEnd, sentAfterEnd)
		}
	})
}
