package command

import (
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
