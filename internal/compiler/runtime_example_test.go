package compiler

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// exampleFixture is the folder of the check that runs the example user-management on the runtime data path.
const exampleFixture = "testdata/runtime_example"

// TestRuntimeExample copies the example user-management as it is committed, with its generated output and its
// record, builds it with the pinned Go against the runtime module of this repository, and runs the check of
// testdata/runtime_example/check in its module once: the check runs the migration job and the service binary
// against the local PostgreSQL server of STEGO_TEST_POSTGRES_DSN and a local SpiceDB container, and calls the
// example's collections over HTTP; then it changes the migrated database once for each kind of object of the schema
// manifest and sees each start refused. A missing server fails the test: a missing result is not a pass.
func TestRuntimeExample(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the example service")
	}
	t.Parallel()
	if os.Getenv("STEGO_TEST_POSTGRES_DSN") == "" {
		t.Fatal("STEGO_TEST_POSTGRES_DSN is not set: this test needs a local PostgreSQL server")
	}
	p := &wiringProject{t: t, dir: t.TempDir()}
	if err := os.CopyFS(p.dir, os.DirFS(filepath.Join("..", "..", "examples", "user-management"))); err != nil {
		t.Fatal(err)
	}
	if record := p.read(".stego/state.yaml"); !strings.Contains(record, "format_version: 5") || strings.Contains(record, "out/internal/storage/store.go") {
		t.Fatal("the record of the example user-management is not of format version 5 on the runtime data path")
	}
	check, err := os.ReadFile(filepath.Join(exampleFixture, "check", "example_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	p.write("out/examplecheck/example_test.go", string(check))
	p.useLocalRuntime()
	p.goCommand("mod", "tidy")
	p.goCommand("vet", "./out/...")
	binary := filepath.Join(p.dir, "bin", "service")
	p.goCommand("build", "-o", binary, "./out")
	endpoint, key := startWiringSpiceDB(t)
	cmd := exec.Command(wiringPinnedGo(), "test", "-count=1", "-timeout=480s", "-v", "-run", "^(TestExample|TestSchemaManifest)$", "./out/examplecheck")
	cmd.Dir = p.dir
	cmd.Env = wiringGoEnv("STEGO_TEST_SPICEDB="+endpoint, "STEGO_TEST_SPICEDB_KEY="+key, "STEGO_EXAMPLE_BINARY="+binary)
	output, runErr := cmd.CombinedOutput()
	t.Run("ST-SUP-RC20-OC39-T02 The example user-management runs on the runtime data path with its scoped collections, upsert, implicit fields and fills", func(t *testing.T) {
		name := "ST-SUP-RC20-OC39-T02_The_example_user-management_runs_on_the_runtime_data_path_with_its_scoped_collections,_upsert,_implicit_fields_and_fills"
		if !strings.Contains(string(output), "--- PASS: TestExample/"+name+" ") {
			t.Fatalf("the check of the example did not run and pass (go test: %v):\n%s", runErr, output)
		}
	})
	t.Run("ST-SUP-RC01-OC12-T08 Startup fails closed when the database does not satisfy the schema manifest", func(t *testing.T) {
		name := "ST-SUP-RC01-OC12-T08_Startup_fails_closed_when_the_database_does_not_satisfy_the_schema_manifest"
		if !strings.Contains(string(output), "--- PASS: TestSchemaManifest/"+name+" ") {
			t.Fatalf("the schema manifest check of the example did not run and pass (go test: %v):\n%s", runErr, output)
		}
	})
}
