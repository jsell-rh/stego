package compiler

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

//go:embed testdata/http_lifecycle_test.go
var httpLifecycleTests []byte

func TestGeneratedHTTPLifecycle(t *testing.T) {
	files, err := Assemble(AssemblerInput{
		ModuleName: "example.com/http-lifecycle", GoVersion: "1.26.8",
		Wirings: []ComponentWiring{{Name: "api", Wiring: &gen.Wiring{
			Routes: []string{`mux.Handle("/", http.NotFoundHandler())`},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	for _, file := range files {
		if err := os.WriteFile(filepath.Join(project, file.Path), file.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(project, "main_test.go"), httpLifecycleTests, 0644); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "test", "-race", "-mod=readonly", "-timeout=20s", "./...")
	command.Dir = project
	command.Env = append(os.Environ(), "GOWORK=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated HTTP lifecycle: %v\n%s", err, output)
	}
	if os.Getenv("STEGO_CROSS_COMPILE") == "1" {
		for _, target := range []struct{ os, arch string }{{"windows", "amd64"}, {"darwin", "arm64"}} {
			command := exec.Command("go", "test", "-c", "-mod=readonly", "-o", filepath.Join(t.TempDir(), "http-test"))
			command.Dir = project
			command.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0", "GOOS="+target.os, "GOARCH="+target.arch)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("generated HTTP cross compilation for %s/%s: %v\n%s", target.os, target.arch, err, output)
			}
		}
	}
}

func TestConstructorFailureReturnsAfterCleanup(t *testing.T) {
	input := AssemblerInput{
		ModuleName: "example.com/lifecycle", GoVersion: "1.26.8",
		Wirings: []ComponentWiring{{Name: "api", Wiring: &gen.Wiring{
			Imports:                 []string{"internal/api"},
			Constructors:            []string{"api.NewHandle()", "api.NewBroken(handle)"},
			ConstructorDeps:         map[int][]string{1: {"handle"}},
			ConstructorReturnsError: map[int]bool{0: true, 1: true},
			ConstructorDeferCalls:   map[int]string{0: "Close()"},
			Routes:                  []string{`mux.Handle("/", broken)`},
		}}},
	}
	files, err := Assemble(input)
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	for _, file := range files {
		if err := os.WriteFile(filepath.Join(project, file.Path), file.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(project, "internal/api"), 0755); err != nil {
		t.Fatal(err)
	}
	api := `package api
import ("errors"; "net/http")
var ErrStartup = errors.New("startup failed")
var Closed bool
var FailStartup = true
type Handle struct{}
func NewHandle() (*Handle, error) { return &Handle{}, nil }
func (h *Handle) Close() { Closed = true }
func NewBroken(h *Handle) (http.Handler, error) {
  if FailStartup { return nil, ErrStartup }
  return http.NotFoundHandler(), nil
}
`
	check := `package main
import ("errors"; "net"; "strconv"; "testing"; "example.com/lifecycle/internal/api")
func TestStartupFailure(t *testing.T) {
  api.FailStartup = true
  api.Closed = false
  if err := run(); !errors.Is(err, api.ErrStartup) { t.Fatalf("lost error: %v", err) }
  if !api.Closed { t.Fatal("earlier constructor resource was not closed") }
}
func TestListenFailure(t *testing.T) {
  listener, err := net.Listen("tcp", "127.0.0.1:0")
  if err != nil { t.Fatal(err) }
  defer listener.Close()
  t.Setenv("PORT", strconv.Itoa(listener.Addr().(*net.TCPAddr).Port))
  api.FailStartup = false
  api.Closed = false
  if err := run(); err == nil { t.Fatal("occupied port returned success") }
  if !api.Closed { t.Fatal("listener failure skipped constructor cleanup") }
}
`
	for name, source := range map[string]string{"internal/api/api.go": api, "main_test.go": check} {
		if err := os.WriteFile(filepath.Join(project, name), []byte(source), 0644); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command("go", "test", "-race", "-mod=readonly", "./...")
	command.Dir = project
	command.Env = append(os.Environ(), "GOWORK=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated startup test: %v\n%s", err, output)
	}
}

func TestConstructorErrorIndexMustExist(t *testing.T) {
	for _, index := range []int{-1, 1} {
		_, err := Assemble(AssemblerInput{
			ModuleName: "example.com/service", GoVersion: "1.26.8",
			Wirings: []ComponentWiring{{Name: "invalid", Wiring: &gen.Wiring{
				Constructors: []string{"api.New()"}, ConstructorReturnsError: map[int]bool{index: true},
			}}},
		})
		if err == nil {
			t.Errorf("accepted invalid constructor index %d", index)
		}
	}
}

func TestWiringReferencesUseGoSyntax(t *testing.T) {
	for _, test := range []struct {
		source string
		want   string
		found  bool
	}{
		{`mux.Handle("/", handler)`, `mux.Handle("/", handler2)`, true},
		{`mux.HandleFunc("/handler.Get", handler.Get)`, `mux.HandleFunc("/handler.Get", handler2.Get)`, true},
		{`mux.Handle("/handler.Get", other)`, `mux.Handle("/handler.Get", other)`, false},
		{`pkg.handler(other)`, `pkg.handler(other)`, false},
		{`wrap(handler), wrap(handler.Get)`, `wrap(handler2), wrap(handler2.Get)`, true},
	} {
		if got := containsIdentRef(test.source, "handler"); got != test.found {
			t.Errorf("reference detection for %s = %v", test.source, got)
		}
		if got := replaceIdentRef(test.source, "handler", "handler2"); got != test.want {
			t.Errorf("rename = %s, want %s", got, test.want)
		}
	}
}
