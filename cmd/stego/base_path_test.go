package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCommandsRejectAmbiguousBasePathsBeforeWriting(t *testing.T) {
	for _, prefix := range []string{"/api/{id}", "/api/../v1", "/api/%2f", "/api?mode=x", "/api\"v1", "/api\nv1", "/"} {
		t.Run(strconv.Quote(prefix), func(t *testing.T) {
			assertCommandsRejectPathsBeforeWriting(t, "base_path: "+strconv.Quote(prefix)+"\n", "  widgets: {entity: Widget, operations: [read]}\n")
		})
	}
}

func TestCommandsRejectCollectionRoutesBeforeWriting(t *testing.T) {
	for _, prefix := range []string{"/items/{id}", "/items/%2f", "/items/../widgets", "/items\"next", "/items\nnext", "/openapi", "/{name}"} {
		t.Run(strconv.Quote(prefix), func(t *testing.T) {
			assertCommandsRejectPathsBeforeWriting(t, "base_path: /api/v1\n", "  widgets: {entity: Widget, operations: [list], path_prefix: "+strconv.Quote(prefix)+"}\n")
		})
	}
	t.Run("wildcard conflict", func(t *testing.T) {
		assertCommandsRejectPathsBeforeWriting(t, "base_path: /api/v1\n", "  first: {entity: Widget, operations: [list], path_prefix: '/owners/{left}/widgets'}\n  second: {entity: Widget, operations: [list], path_prefix: '/owners/{right}/widgets'}\n")
	})
}

func assertCommandsRejectPathsBeforeWriting(t *testing.T, base, collections string) {
	t.Helper()
	registry, err := filepath.Abs("../../registry")
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	t.Chdir(project)
	t.Setenv("STEGO_REGISTRY", registry)
	t.Setenv("STEGO_MODULE", "example.com/base-path")
	t.Setenv("STEGO_GO_VERSION", "1.26.8")
	files := map[string]string{
		"service.yaml": "kind: service\nname: base-path\narchetype: rest-crud\nlanguage: go\n" + base + "entities:\n  - name: Widget\n    fields:\n      - {name: label, type: string}\ncollections:\n" + collections,
		"out/main.go":  "existing output\n", ".stego/state.yaml": "last_applied: null\n",
		"go.mod": "module example.com/base-path\ngo 1.26.8\n", "go.sum": "",
	}
	for name, data := range files {
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, command := range []struct {
		name string
		run  func([]string) error
	}{{"validate", runValidate}, {"plan", runPlan}, {"apply", runApply}} {
		if err := command.run(nil); err == nil || !strings.Contains(err.Error(), "validation failed") {
			t.Fatal(command.name, "accepted an invalid prefix", err)
		}
		for name, expected := range files {
			actual, err := os.ReadFile(name)
			if err != nil || string(actual) != expected {
				t.Fatal(command.name, "changed project state", name, err)
			}
		}
	}
}
