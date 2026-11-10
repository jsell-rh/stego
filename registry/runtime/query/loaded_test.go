package query

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// genSource is the generated code of the fixture module: an entity type, its loaded type and an unexported
// constructor that a fill must not reach.
const genSource = `package gen

import "github.com/jsell-rh/stego/registry/runtime/query"

type Reservation struct{ ID, SKU string }

type LoadedReservation = query.Loaded[Reservation]

func newLoadedReservation() LoadedReservation { return nil }

var _ = newLoadedReservation
`

// buildFill builds a fill package with the body src in a module outside the runtime module, beside the generated
// package gen, and gives the error of go build with its output. The module resolves github.com/jsell-rh/stego/registry/runtime from this
// checkout through the work file of scripts/runtime-workspace.sh.
func buildFill(t *testing.T, src string) error {
	t.Helper()
	dir := t.TempDir()
	module := filepath.Join(dir, "outside")
	files := map[string]string{
		"go.mod":       "module example.com/outside\n\ngo 1.26.10\n\nrequire github.com/jsell-rh/stego/registry/runtime v0.0.0\n",
		"gen/gen.go":   genSource,
		"fill/fill.go": "package fill\n\nimport \"example.com/outside/gen\"\n\nvar _ gen.LoadedReservation\n" + src,
	}
	for name, text := range files {
		path := filepath.Join(module, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script, err := filepath.Abs("../../../scripts/runtime-workspace.sh")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("bash", script, module, filepath.Join(dir, "outside.work")).Output()
	if err != nil {
		t.Fatalf("write the work file: %v", err)
	}
	build := exec.Command("go", "build", "./...")
	build.Dir = module
	build.Env = append(os.Environ(), "GOWORK="+strings.TrimSpace(string(out)))
	if output, err := build.CombinedOutput(); err != nil {
		return errors.New(string(output))
	}
	return nil
}

// zeroOf makes a new zero value of the type that l points to, as a fill can without naming that type.
func zeroOf[E any](*E) *E { return new(E) }

func TestLoaded(t *testing.T) {
	t.Run("ST-SUP-RP11-LC12-T01 A fill gets a loaded value only from the query handle", func(t *testing.T) {
		e := newEnv(t)
		e.insert(t, "INSERT INTO app.reservations (tenant_id, id, sku) VALUES ('t1', 'r-1', 'sku-77')")
		e.insert(t, "INSERT INTO app.orders (tenant_id, id, total) VALUES ('t1', 'o-1', 5)")
		port := &fakeDecision{decide: allowAll}
		rt := e.runtimeOf(t, port)
		reservations, orders := reservationEntity(t), orderEntity(t, false)
		q := mustQuery(t, "open-reservations", reservations, orders)
		got, err := Run(context.Background(), rt, q, bound(t, "t1", "user:u-7", "open-reservations"), "r-1",
			func(ctx context.Context, id string, h *Handle[noProbe]) (Loaded[Reservation], error) {
				repo, err := Repo(h, reservations)
				if err != nil {
					return nil, err
				}
				return repo.Get(ctx, id)
			})
		if err != nil {
			t.Fatal(err)
		}
		if got.Value().SKU != "sku-77" || got.ID() != "r-1" {
			t.Errorf("the loaded value holds %+v, want the stored sku sku-77 of r-1", got.Value())
		}

		// A value that a fill makes itself, with new or as a copy of a loaded value, is refused by
		// every operation that takes a loaded value, before the decision port.
		var refusals map[string]error
		var sent int
		_, err = RunStream(context.Background(), rt, q, bound(t, "t1", "user:u-7", "open-reservations"), struct{}{}, orders,
			func(Order) error { sent++; return nil },
			func(ctx context.Context, _ struct{}, s *Stream[Order, noProbe]) error {
				repo, err := Repo(s.Handle(), orders)
				if err != nil {
					return err
				}
				loaded, err := repo.Get(ctx, "o-1")
				if err != nil {
					return err
				}
				made := zeroOf(loaded)
				copied := *loaded
				before := len(port.recorded())
				_, evalMade := s.Handle().Authz().Evaluate(ctx, "view", made)
				_, evalCopy := s.Handle().Authz().Evaluate(ctx, "view", &copied)
				refusals = map[string]error{
					"Evaluate of a value that new made": evalMade,
					"Evaluate of a copy":                evalCopy,
					"Send of a value that new made":     s.Send(ctx, made),
					"Send of a copy":                    s.Send(ctx, &copied),
				}
				if len(port.recorded()) != before {
					return errors.New("a value that the fill made reached the decision port")
				}
				// The control: the value that the handle loaded is sent.
				return s.Send(ctx, loaded)
			})
		if err != nil {
			t.Fatal(err)
		}
		for name, err := range refusals {
			if !errors.Is(err, errForeignValue) {
				t.Errorf("%s gives %v, want errForeignValue", name, err)
			}
		}
		if sent != 1 {
			t.Errorf("%d messages sent, want only the 1 of the loaded value", sent)
		}

		// A fill cannot build a loaded value: a composite literal, a conversion and the unexported constructor of
		// the generated package do not compile.
		for name, c := range map[string]struct{ src, want string }{
			"a composite literal":       {"\nvar _ = gen.LoadedReservation{}\n", "invalid composite literal type"},
			"a conversion":              {"\nvar _ = gen.LoadedReservation(gen.Reservation{})\n", "cannot convert"},
			"an unexported constructor": {"\nvar _ = gen.newLoadedReservation()\n", "newLoadedReservation"},
		} {
			err := buildFill(t, c.src)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s in a fill gave %v, want a compile error with %q", name, err, c.want)
			}
		}
		if err := buildFill(t, "\nfunc sku(r gen.LoadedReservation) string { return r.Value().SKU }\n"); err != nil {
			t.Fatalf("the control fill does not compile: %v", err)
		}
	})

	t.Run("ST-SUP-RP11-LC12-T02 A change to the value a fill got does not change what the handle sends", func(t *testing.T) {
		e := newEnv(t)
		e.insert(t, "INSERT INTO app.orders (tenant_id, id, total, note) VALUES ('t1', 'o-1', 5, 'abc')")
		rt := e.runtimeOf(t, &fakeDecision{decide: allowAll})
		type Noted struct {
			ID   string
			Note []byte
		}
		noted, err := NewEntity(Spec{Name: "order", Schema: "app", Table: "orders", Key: "id", Columns: []string{"id", "note"},
			ResourceType: "order", ReadPermission: "view", ListPermission: "view"},
			func(r Row) (Noted, error) {
				id, err := r.Text("id")
				note, _ := r.Value("note")
				b, _ := note.([]byte)
				return Noted{ID: id, Note: b}, err
			})
		if err != nil {
			t.Fatal(err)
		}
		var sent []string
		_, err = RunStream(context.Background(), rt, mustQuery(t, "watch-orders", noted), bound(t, "t1", "user:u-7", "watch-orders"), struct{}{}, noted,
			func(n Noted) error { sent = append(sent, string(n.Note)); return nil },
			func(ctx context.Context, _ struct{}, s *Stream[Noted, noProbe]) error {
				repo, err := Repo(s.Handle(), noted)
				if err != nil {
					return err
				}
				order, err := repo.Get(ctx, "o-1")
				if err != nil {
					return err
				}
				order.Value().Note[0] = 'X'
				return s.Send(ctx, order)
			})
		if err != nil {
			t.Fatal(err)
		}
		if len(sent) != 1 || sent[0] != "abc" {
			t.Errorf("the stream sent %q, want the stored note abc", sent)
		}
	})
}
