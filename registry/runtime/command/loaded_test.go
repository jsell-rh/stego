package command

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// genSource is the generated code of the fixture module: the entity types, their loaded types, an unexported
// constructor that a fill must not reach, and a generated effect method that takes a loaded value.
const genSource = `package gen

import (
	"context"

	"stego.dev/runtime/command"
)

type Reservation struct{ ID, SKU string }

type LoadedReservation = command.Loaded[Reservation]

func newLoadedReservation() LoadedReservation { return nil }

var _ = newLoadedReservation

type Charge struct{ ID string }

type LoadedCharge = command.Loaded[Charge]

func ChargeRequested(ctx context.Context, h *command.Handle[struct{}], c LoadedCharge) error {
	return h.Effects().Stage(ctx, "billing", "charge-requested", c, []byte("{}"))
}
`

// buildFill builds a fill package with the body src in a module outside the runtime module, beside the generated
// package gen, and gives the error of go build with its output. The module resolves stego.dev/runtime from this
// checkout through the work file of scripts/runtime-workspace.sh.
func buildFill(t *testing.T, src string) error {
	t.Helper()
	dir := t.TempDir()
	module := filepath.Join(dir, "outside")
	files := map[string]string{
		"go.mod":     "module example.com/outside\n\ngo 1.26.10\n\nrequire stego.dev/runtime v0.0.0\n",
		"gen/gen.go": genSource,
		"fill/fill.go": "package fill\n\nimport (\n\t\"context\"\n\n\t\"example.com/outside/gen\"\n\t\"stego.dev/runtime/command\"\n)\n\n" +
			"var (\n\t_ context.Context\n\t_ = gen.ChargeRequested\n\t_ *command.Handle[struct{}]\n)\n" + src,
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

func TestLoadedValues(t *testing.T) {
	t.Run("ST-SUP-RP10-LC15-T01 A fill gets a loaded value only from the command handle", func(t *testing.T) {
		e := newEnv(t)
		e.exec(`INSERT INTO reservations (tenant_id, id, order_id, sku) VALUES ('t1', 'r1', 'o1', 'sku-7')`)
		h, _, end := e.bind("t1", u7(t), e.command("reserve", nil, e.reservationE))
		defer end()
		got, err := repo(t, h, e.reservationE).Get(e.ctx, "r1")
		if err != nil || got.Value().SKU != "sku-7" {
			t.Fatalf("the read gave %v, %v; want the stored sku", got, err)
		}
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
}

// profile is an entity with a map field, which its generated Clone copies deeply.
type profile struct {
	ID    string
	Attrs map[string]string
}

func TestLoadedCopies(t *testing.T) {
	t.Run("ST-SUP-RP10-LC15-T02 A change to the map of a loaded value is not written", func(t *testing.T) {
		e := newEnv(t)
		e.exec(`CREATE TABLE profiles (tenant_id text NOT NULL, id text NOT NULL, attrs jsonb NOT NULL,
  version bigint NOT NULL DEFAULT 1, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id))`,
			`ALTER TABLE profiles ENABLE ROW LEVEL SECURITY`, `ALTER TABLE profiles FORCE ROW LEVEL SECURITY`,
			`CREATE POLICY profiles_tenant ON profiles USING (tenant_id = current_setting('app.tenant', true))`,
			"GRANT SELECT, INSERT, UPDATE ON profiles TO "+e.roles[0],
			`INSERT INTO profiles (tenant_id, id, attrs) VALUES ('t1', 'p1', '{"plan": "basic"}')`)
		ps := spec("Profile", "", "profiles", []string{"attrs"},
			func(s Scanner) (profile, Meta, error) {
				var v profile
				var m Meta
				var raw []byte
				if err := s.Scan(&v.ID, &raw, &m.Version, &m.Token); err != nil {
					return v, m, err
				}
				m.ID = v.ID
				return v, m, json.Unmarshal(raw, &v.Attrs)
			},
			func(v profile) []any { raw, _ := json.Marshal(v.Attrs); return []any{v.ID, string(raw)} },
			func(v profile) map[string]any {
				attrs := map[string]any{}
				for k, a := range v.Attrs {
					attrs[k] = a
				}
				return map[string]any{"id": v.ID, "attrs": attrs}
			})
		ps.Clone = func(v profile) profile {
			attrs := make(map[string]string, len(v.Attrs))
			for k, a := range v.Attrs {
				attrs[k] = a
			}
			return profile{ID: v.ID, Attrs: attrs}
		}
		profiles, err := NewEntity(ps)
		if err != nil {
			t.Fatal(err)
		}
		cmd := e.command("touch-profile", nil, profiles)
		if _, err := e.run(cmd, e.request("t1", u7(t), "k-profile", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			r := repo(t, h, profiles)
			p, err := r.Get(ctx, "p1")
			if err != nil {
				return err
			}
			p.Value().Attrs["plan"] = "changed"
			_, err = r.Update(ctx, p, p.Value())
			return err
		})); err != nil {
			t.Fatal(err)
		}
		if n := e.count(`SELECT count(*) FROM profiles WHERE id = 'p1' AND attrs->>'plan' = 'basic' AND version = 2`); n != 1 {
			t.Fatal("the change to the map that Value returned was written")
		}
	})
}
