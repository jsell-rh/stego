package outbox

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

//go:embed testdata/queue_test.go
var queueTests []byte

//go:embed testdata/worker_test.go
var workerTests []byte

//go:embed testdata/worker_failure_test.go
var workerFailureTests []byte

//go:embed testdata/source_test.go
var sourceTests []byte

func TestGeneratedOutbox(t *testing.T) {
	postgresDSN := os.Getenv("STEGO_TEST_POSTGRES_DSN")
	requirePostgres := os.Getenv("STEGO_REQUIRE_POSTGRES")
	if requirePostgres == "1" && postgresDSN == "" {
		t.Fatal("PostgreSQL integration tests require STEGO_TEST_POSTGRES_DSN")
	}
	files, wiring, err := new(Generator).Generate(gen.Context{OutputNamespace: "queue"})
	if err != nil {
		t.Fatal(err)
	}
	if wiring.GoModRequires["github.com/google/uuid"] != "v1.6.0" {
		t.Fatal("outbox UUID dependency is missing")
	}
	project := t.TempDir()
	for _, file := range files {
		name := filepath.Join(project, file.Path)
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, file.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	module := "module example.com/outbox-test\n\ngo 1.26.8\n\nrequire (\n github.com/google/uuid v1.6.0\n github.com/jackc/pgx/v5 v5.11.0\n)\n"
	for name, data := range map[string][]byte{"go.mod": []byte(module), "queue/queue_test.go": queueTests, "queue/worker_test.go": workerTests, "queue/worker_failure_test.go": workerFailureTests, "queue/source_test.go": sourceTests} {
		if err := os.WriteFile(filepath.Join(project, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	commands := [][]string{{"mod", "tidy"}, {"test", "-race", "-mod=readonly", "-timeout=60s", "-v", "./..."}}
	if os.Getenv("STEGO_BENCH_OUTBOX") == "1" {
		commands = append(commands, []string{"test", "-mod=readonly", "-run=^$", "-bench=BenchmarkClaimAndAcknowledge", "-benchtime=100x", "-benchmem", "./..."})
	}
	for _, args := range commands {
		cmd := exec.Command("go", args...)
		cmd.Dir = project
		cmd.Env = append(os.Environ(), "GOWORK=off", "STEGO_TEST_POSTGRES_DSN="+postgresDSN, "STEGO_REQUIRE_POSTGRES="+requirePostgres)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("generated outbox, go %v: %v\n%s", args, err, output)
		}
		t.Log(string(output))
	}
}
