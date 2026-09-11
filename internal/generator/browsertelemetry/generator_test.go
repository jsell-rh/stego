package browsertelemetry

import (
	"context"
	"github.com/jsell-rh/stego/internal/gen"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func fixture() gen.Context {
	return gen.Context{OutputNamespace: "telemetry", ComponentConfig: map[string]any{"service_name": "example-console"}}
}
func TestGeneration(t *testing.T) {
	g := new(Generator)
	ctx := fixture()
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	again, _, err := g.Generate(ctx)
	if err != nil || !reflect.DeepEqual(files, again) || len(files) != 3 {
		t.Fatal("generation differs", err)
	}
	for _, cfg := range []map[string]any{nil, {}, {"service_name": ""}, {"service_name": "name\n"}, {"service_name": "secret name"}, {"service_name": 42}, {"service_name": "valid", "url": "https://other.example"}} {
		ctx.ComponentConfig = cfg
		if g.ValidateContext(ctx) == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
}
func TestRuntime(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("STEGO_REQUIRE_NODE") == "1" {
			t.Fatal(err)
		}
		t.Skip("Node fixture is not installed")
	}
	modules, err := filepath.Abs("testdata/node_modules")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(modules); err != nil {
		if os.Getenv("STEGO_REQUIRE_NODE") == "1" {
			t.Fatal(err)
		}
		t.Skip("Node dependencies are not installed")
	}
	dir := t.TempDir()
	files, _, err := new(Generator).Generate(fixture())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		target := filepath.Join(dir, f.Path)
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, f.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(modules, filepath.Join(dir, "node_modules")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"runtime.test.mjs", "usage.mts"} {
		data, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"--test", "runtime.test.mjs"}, {filepath.Join(modules, "typescript/lib/tsc.js"), "--strict", "--noEmit", "--target", "ES2022", "--module", "NodeNext", "--moduleResolution", "NodeNext", "usage.mts"}} {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		cmd := exec.CommandContext(ctx, node, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "NODE_OPTIONS=--max-old-space-size=256")
		output, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("browser telemetry: %v\n%s", err, output)
		}
		t.Log(string(output))
	}
}
