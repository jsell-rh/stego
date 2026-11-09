package query

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The fixture of the compile checks: a stand-in for the generated package of a service, with the entity type and
// its loaded type as the generator emits them, and a fill package that uses the loaded value as a fill may.
const (
	genSource = `package gen

import "stego.dev/runtime/query"

type Reservation struct{ ID, SKU string }

// LoadedReservation is the generated loaded type of Reservation.
type LoadedReservation = *query.Loaded[Reservation]
`
	fillSource = `package fill

import "example.com/service/gen"

// SKU reads a field of a loaded value, as a probe method does.
func SKU(r gen.LoadedReservation) string { return r.Value().SKU }
`
)

// buildFill builds the fixture module with the fill file extra beside fillSource, through the work file of
// scripts/runtime-workspace.sh, and gives the build's output and whether it succeeded.
func buildFill(t *testing.T, extra string) (string, bool) {
	t.Helper()
	dir := t.TempDir()
	module := filepath.Join(dir, "service")
	files := map[string]string{
		"go.mod":       "module example.com/service\n\ngo 1.26.10\n\nrequire stego.dev/runtime v0.0.0\n",
		"gen/gen.go":   genSource,
		"fill/fill.go": fillSource,
	}
	if extra != "" {
		files["fill/extra.go"] = extra
	}
	for name, src := range files {
		path := filepath.Join(module, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script, err := filepath.Abs("../../../scripts/runtime-workspace.sh")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("bash", script, module, filepath.Join(dir, "service.work")).Output()
	if err != nil {
		t.Fatalf("write the work file: %v", err)
	}
	build := exec.Command("go", "build", "./...")
	build.Dir = module
	build.Env = append(os.Environ(), "GOWORK="+strings.TrimSpace(string(out)), "GOFLAGS=")
	output, err := build.CombinedOutput()
	return string(output), err == nil
}

func TestLoaded(t *testing.T) {
	t.Run("ST-SUP-RP11-LC12-T01 A fill gets a loaded value only from the query handle", func(t *testing.T) {
		e := newEnv(t)
		e.insert(t, "INSERT INTO app.reservations (tenant_id, id, sku) VALUES ('t1', 'r-1', 'sku-77')")
		rt := e.runtimeOf(t, &fakeDecision{decide: allowAll})
		reservations := reservationEntity(t)
		q := mustQuery(t, "open-reservations", "", reservations)
		got, err := Run(context.Background(), rt, q, bound(t, "t1", "user:u-7", "open-reservations"), "r-1",
			func(ctx context.Context, id string, h *Handle[noProbe]) (*Loaded[Reservation], error) {
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

		if output, ok := buildFill(t, ""); !ok {
			t.Fatalf("the fixture without a bad file does not build, so the checks below prove nothing:\n%s", output)
		}
		for name, check := range map[string]struct{ src, want string }{
			"a composite literal": {
				src:  "package fill\n\nimport \"example.com/service/gen\"\n\nvar Made = gen.LoadedReservation{}\n",
				want: "invalid composite literal type",
			},
			"a conversion from Reservation": {
				src:  "package fill\n\nimport \"example.com/service/gen\"\n\nfunc Made(r gen.Reservation) gen.LoadedReservation { return gen.LoadedReservation(r) }\n",
				want: "cannot convert",
			},
			"a call of the unexported constructor": {
				src:  "package fill\n\nimport (\n\t\"example.com/service/gen\"\n\t\"stego.dev/runtime/query\"\n)\n\nvar Made = query.newLoaded[gen.Reservation]\n",
				want: "newLoaded",
			},
		} {
			output, ok := buildFill(t, check.src)
			if ok {
				t.Errorf("a fill file with %s builds", name)
				continue
			}
			if !strings.Contains(output, check.want) || !strings.Contains(output, "extra.go") {
				t.Errorf("a fill file with %s fails for another reason than %q:\n%s", name, check.want, output)
			}
		}
	})
}
