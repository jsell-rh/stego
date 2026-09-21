package outbox

import (
	"bytes"
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

//go:embed testdata/queue_test.go
var queueTests []byte

//go:embed testdata/worker_test.go
var workerTests []byte

//go:embed testdata/worker_failure_test.go
var workerFailureTests []byte

//go:embed testdata/claim_failure_test.go
var claimFailureTests []byte

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
	module := "module example.com/outbox-test\n\ngo " + new(Generator).MinimumGoVersion() + "\n\nrequire (\n github.com/google/uuid v1.6.0\n github.com/jackc/pgx/v5 v5.11.0\n)\n"
	for name, data := range map[string][]byte{"go.mod": []byte(module), "queue/queue_test.go": queueTests, "queue/worker_test.go": workerTests, "queue/worker_failure_test.go": workerFailureTests, "queue/source_test.go": sourceTests, "queue/claim_failure_test.go": claimFailureTests} {
		if err := os.WriteFile(filepath.Join(project, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	commands := [][]string{{"mod", "tidy", "-go=" + new(Generator).MinimumGoVersion()}, {"vet", "-mod=readonly", "./..."}, {"test", "-race", "-mod=readonly", "-timeout=60s", "-v", "./..."}}
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
	if postgresDSN != "" {
		checkPartialClaimMutation(t, project)
	}
}

// Check the regression case against the former partial-result behavior.
func checkPartialClaimMutation(t *testing.T, project string) {
	t.Helper()
	file := filepath.Join(project, "queue/queue.go")
	original, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	checked := []byte("\tif err := rows.Err(); err != nil {\n\t\treturn nil, err\n\t}\n\treturn deliveries, nil")
	if bytes.Count(original, checked) != 1 {
		t.Fatal("claim mutation target differs")
	}
	changed := bytes.Replace(original, checked, []byte("\treturn deliveries, rows.Err()"), 1)
	if err := os.WriteFile(file, changed, 0644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.WriteFile(file, original, 0644); err != nil {
			t.Error(err)
		}
	})
	cmd := exec.Command("go", "test", "-mod=readonly", "-count=1", "-timeout=15s", "-run=^TestClaimDiscardsPartialResults$", "-v", "./queue")
	cmd.Dir = project
	cmd.Env = append(os.Environ(), "GOWORK=off")
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "claim returned a partial batch after a row error") || !strings.Contains(string(output), "--- FAIL: TestClaimDiscardsPartialResults") {
		t.Fatalf("partial-result regression was not detected: %v\n%s", err, output)
	}
	t.Log("The partial-result mutation failed the required regression check")
}
