package idempotency

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/types"
)

//go:embed testdata/emitted_test.go
var emittedTests []byte

// pinnedGo is the Go command that builds the emitted package: STEGO_TEST_PINNED_GO, else bin/go of the SDK that
// runs the test.
func pinnedGo() string {
	if name := os.Getenv("STEGO_TEST_PINNED_GO"); name != "" {
		return name
	}
	return filepath.Join(runtime.GOROOT(), "bin", "go")
}

// emittedProject writes the emitted package of the test declaration, with testdata/emitted_test.go beside it, into
// a module that requires the runtime library from this repository's registry/runtime folder, and vets it.
func emittedProject(t *testing.T) string {
	t.Helper()
	files := generated(t, todoContext([]types.Operation{types.OpCreate, types.OpPatch}, "48h", todoBase))
	project := t.TempDir()
	write := func(name string, data []byte) {
		full := filepath.Join(project, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range files {
		write(name, data)
	}
	write("internal/idempotency/emitted_test.go", emittedTests)
	runtimeFolder, err := filepath.Abs(filepath.Join("..", "..", "..", "registry", "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	sums, err := os.ReadFile(filepath.Join(runtimeFolder, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	write("go.sum", sums)
	write("go.mod", []byte("module example.com/idempotency-test\n\ngo "+new(Generator).MinimumGoVersion()+
		"\n\nrequire stego.dev/runtime v0.1.0\n\nreplace stego.dev/runtime => "+runtimeFolder+"\n"))
	for _, args := range [][]string{{"mod", "tidy"}, {"vet", "./..."}} {
		goCommand(t, project, args...)
	}
	return project
}

func goCommand(t *testing.T, project string, args ...string) string {
	t.Helper()
	cmd := exec.Command(pinnedGo(), args...)
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go %v: %v\n%s", args, err, output)
	}
	t.Log(string(output))
	return string(output)
}

// runEmitted runs one subtest of testdata/emitted_test.go in the emitted module. A test that needs PostgreSQL
// skips without STEGO_TEST_POSTGRES_DSN, and fails without it when STEGO_REQUIRE_POSTGRES=1.
func runEmitted(project, id string, postgres bool) func(t *testing.T) {
	return func(t *testing.T) {
		if postgres && os.Getenv("STEGO_TEST_POSTGRES_DSN") == "" {
			if os.Getenv("STEGO_REQUIRE_POSTGRES") == "1" {
				t.Fatal("PostgreSQL integration tests require STEGO_TEST_POSTGRES_DSN")
			}
			t.Skip("STEGO_TEST_POSTGRES_DSN is not set")
		}
		output := goCommand(t, project, "test", "-count=1", "-timeout=60s", "-v", "-run", "^TestEmitted$/^"+id+"_", "./internal/idempotency")
		if !strings.Contains(output, "--- PASS: TestEmitted/"+id+"_") {
			t.Fatalf("the emitted test %s did not run and pass:\n%s", id, output)
		}
	}
}

func TestEmittedPackage(t *testing.T) {
	project := emittedProject(t)
	t.Run("ST-SUP-RC30-OC03-T01 The fingerprint covers the method, the path and the canonical body",
		runEmitted(project, "ST-SUP-RC30-OC03-T01", false))
	t.Run("ST-SUP-RC30-OC03-T02 The query string is not in the fingerprint",
		runEmitted(project, "ST-SUP-RC30-OC03-T02", false))
	t.Run("ST-SUP-RC30-OC03-T03 Only declared fields are canonicalised",
		runEmitted(project, "ST-SUP-RC30-OC03-T03", false))
	t.Run("ST-SUP-RC30-OC04-T04 The emitted migration makes the key table scoped by tenant, principal and key",
		runEmitted(project, "ST-SUP-RC30-OC04-T04", true))
	t.Run("ST-SUP-RC30-OC04-T06 The purge job deletes the expired key rows and keeps the live ones",
		runEmitted(project, "ST-SUP-RC30-OC04-T06", true))
}
