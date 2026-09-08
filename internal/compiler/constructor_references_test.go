package compiler

import (
	"github.com/jsell-rh/stego/internal/gen"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestConstructorPackageAndValueReferences(t *testing.T) {
	for _, test := range []struct{ source, want string }{
		{`api.New(store)`, `api2.New(store3)`},
		{`api.New("store", store)`, `api2.New("store", store3)`},
		{`api.New(store.Default(), other.store)`, `api2.New(store2.Default(), other.store)`},
		{`api.New(store, handle.Method())`, `api2.New(store3, handle2.Method())`},
		{`api.New(struct{ store any }{store: store})`, `api2.New(struct{ store any }{store: store3})`},
		{`api.New(map[string]any{store: store})`, `api2.New(map[string]any{store3: store3})`},
	} {
		got, err := renameConstructorReferences(test.source, map[string]string{"store": "store3", "handle": "handle2"}, map[string]string{"api": "api2", "store": "store2"}, map[string]bool{"store2": true}, nil)
		if err != nil || got != test.want {
			t.Errorf("%s: got %q, %v; want %q", test.source, got, err, test.want)
		}
	}
	if _, err := renameConstructorReferences(`api.New(store.Get())`, map[string]string{"store": "store3"}, nil, map[string]bool{"store": true}, []string{"store"}); err == nil {
		t.Fatal("ambiguous selector accepted")
	}
	if _, err := renameConstructorReferences(`api.New(Options{store: store})`, map[string]string{"store": "store2"}, nil, nil, nil); err == nil {
		t.Fatal("literal key renamed without type information")
	}
	got, err := renameConstructorReferences(`api.New(context.Background(), context)`, map[string]string{"context": "context2"}, nil, map[string]bool{"context": true}, nil)
	if err != nil || got != `api.New(context.Background(), context2)` {
		t.Fatalf("standard package reference changed: %s, %v", got, err)
	}
	if _, err := renameConstructorReferences(`api.New(func(store any) any { return store })`, map[string]string{"store": "store2"}, nil, nil, nil); err == nil {
		t.Fatal("closure binding was renamed without scope information")
	}
}

func verifyDiamondConstructors(t *testing.T, files []gen.File) {
	t.Helper()
	project := t.TempDir()
	sources := map[string][]byte{
		"out/internal/d/d.go": []byte(`package d
   type D struct { Value int }
   func NewD() *D { return &D{Value: 7} }
  `),
		"out/internal/b/b.go": []byte(`package b
   import "github.com/myorg/svc/out/internal/d"
   type B struct { D *d.D }
   func NewB(value *d.D) *B { return &B{D: value} }
  `),
		"out/internal/c/c.go": []byte(`package c
   import "github.com/myorg/svc/out/internal/d"
   type C struct { D *d.D }
   func NewC(value *d.D) *C { return &C{D: value} }
  `),
		"out/internal/a/a.go": []byte(`package a
   import ("net/http"; "github.com/myorg/svc/out/internal/b"; "github.com/myorg/svc/out/internal/c")
   var Constructed bool
   type A struct{}
   func NewA(first *b.B, second *c.C) *A {
    if first.D != second.D || first.D.Value != 7 { panic("constructor dependency identity changed") }
    Constructed = true
    return &A{}
   }
   func (*A) Index(http.ResponseWriter, *http.Request) {}
  `),
		"out/main_test.go": []byte(`package main
   import ("testing"; "github.com/myorg/svc/out/internal/a")
   func TestConstructors(t *testing.T) {
    t.Setenv("PORT", "invalid-port")
    if err := run(); err == nil { t.Fatal("invalid port accepted") }
    if !a.Constructed { t.Fatal("constructors were skipped") }
   }
  `),
	}
	for _, file := range files {
		name := filepath.Join("out", file.Path)
		if file.Path == "go.mod" {
			name = "go.mod"
		}
		sources[name] = file.Bytes()
	}
	for name, content := range sources {
		name = filepath.Join(project, name)
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, content, 0644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("go", "test", "-race", "-mod=readonly", "./...")
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated constructor references: %v\n%s", err, output)
	}
}
