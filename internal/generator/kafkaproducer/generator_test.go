package kafkaproducer

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	queuegen "github.com/jsell-rh/stego/internal/generator/outbox"
)

//go:embed testdata/publisher_test.go
var publisherTests []byte

//go:embed testdata/integration_test.go
var integrationTests []byte

func TestKafkaRuntimeFenceWiring(t *testing.T) {
	base := gen.Context{OutputNamespace: "publisher", ModuleName: "example.com/kafka-test"}
	// Without a fenced postgres-adapter peer the runtime keeps the plain
	// constructor and no store dependency.
	plain, plainWiring, err := new(Generator).Generate(gen.Context{OutputNamespace: "publisher", ModuleName: "example.com/kafka-test", PeerNamespaces: map[string]string{"outbox": "queue"}})
	if err != nil {
		t.Fatal(err)
	}
	plainSource := findGeneratedSource(t, plain, "publisher/runtime.go")
	if strings.Contains(plainSource, "NewWithFence") || strings.Contains(plainSource, "stegostorage") {
		t.Fatal("unfenced runtime references the fence")
	}
	if len(plainWiring.ConstructorDeps) != 0 {
		t.Fatal("unfenced runtime declares constructor dependencies")
	}
	// With a fenced postgres-adapter peer the runtime claims work only while
	// its process holds the database writer lease.
	fenced, fencedWiring, err := new(Generator).Generate(gen.Context{
		OutputNamespace: "publisher", ModuleName: "example.com/kafka-test",
		PeerNamespaces: map[string]string{"outbox": "queue", "postgres-adapter": "storage"},
		PeerConfigs:    map[string]map[string]any{"postgres-adapter": {"schema_generation": "fresh-v1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	fencedSource := findGeneratedSource(t, fenced, "publisher/runtime.go")
	for _, required := range []string{"NewWithFence(db, store.WriterLeaseCheck)", "store *stegostorage.Store", `"example.com/kafka-test/storage"`} {
		if !strings.Contains(fencedSource, required) {
			t.Fatalf("fenced runtime missing %q", required)
		}
	}
	if len(fencedWiring.Constructors) != 1 || fencedWiring.Constructors[0] != "publisher.NewRuntime(store)" {
		t.Fatal("fenced runtime constructor is wrong", fencedWiring.Constructors)
	}
	if !slicesEqual(fencedWiring.ConstructorDeps[0], []string{"store"}) {
		t.Fatal("fenced runtime constructor dependencies are wrong", fencedWiring.ConstructorDeps)
	}
	// With an otel-tracing peer the runtime reports worker counters as
	// stego.outbox gauges through the common telemetry runtime.
	traced, tracedWiring, err := new(Generator).Generate(gen.Context{
		OutputNamespace: "publisher", ModuleName: "example.com/kafka-test",
		PeerNamespaces: map[string]string{"outbox": "queue", "otel-tracing": "tracing"},
	})
	if err != nil {
		t.Fatal(err)
	}
	tracedSource := findGeneratedSource(t, traced, "publisher/runtime.go")
	for _, required := range []string{
		`tracingRuntime *stegoevents.Runtime`,
		`"example.com/kafka-test/tracing"`,
		`tracingRuntime.OutboxTelemetry()`,
		`telemetry.ObserveOutbox`,
		`worker.OnClose(detach)`,
	} {
		if !strings.Contains(tracedSource, required) {
			t.Fatalf("traced runtime missing %q", required)
		}
	}
	if strings.Contains(tracedSource, "stegostorage") {
		t.Fatal("traced runtime references a fence it does not have")
	}
	if len(tracedWiring.Constructors) != 1 || tracedWiring.Constructors[0] != "publisher.NewRuntime(tracingRuntime)" {
		t.Fatal("traced runtime constructor is wrong", tracedWiring.Constructors)
	}
	if !slicesEqual(tracedWiring.ConstructorDeps[0], []string{"tracingRuntime"}) {
		t.Fatal("traced runtime constructor dependencies are wrong", tracedWiring.ConstructorDeps)
	}
	// Fence and telemetry combine: the store keeps the writer-lease check and
	// the tracing runtime keeps the counter gauges.
	fencedTraced, fencedTracedWiring, err := new(Generator).Generate(gen.Context{
		OutputNamespace: "publisher", ModuleName: "example.com/kafka-test",
		PeerNamespaces: map[string]string{"outbox": "queue", "otel-tracing": "tracing", "postgres-adapter": "storage"},
		PeerConfigs:    map[string]map[string]any{"postgres-adapter": {"schema_generation": "fresh-v1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fencedTracedWiring.Constructors) != 1 || fencedTracedWiring.Constructors[0] != "publisher.NewRuntime(store, tracingRuntime)" {
		t.Fatal("fenced traced runtime constructor is wrong", fencedTracedWiring.Constructors)
	}
	if !slicesEqual(fencedTracedWiring.ConstructorDeps[0], []string{"store", "tracingRuntime"}) {
		t.Fatal("fenced traced runtime constructor dependencies are wrong", fencedTracedWiring.ConstructorDeps)
	}
	fencedTracedSource := findGeneratedSource(t, fencedTraced, "publisher/runtime.go")
	for _, required := range []string{
		`stegostorage "example.com/kafka-test/storage"`,
		`tracingRuntime.OutboxTelemetry()`,
		`worker.OnClose(detach)`,
	} {
		if !strings.Contains(fencedTracedSource, required) {
			t.Fatalf("fenced traced runtime missing %q", required)
		}
	}
	_ = base
}

func findGeneratedSource(t *testing.T, files []gen.File, name string) string {
	t.Helper()
	for _, file := range files {
		if file.Path == name {
			return string(file.Content)
		}
	}
	t.Fatalf("generated file %q is missing", name)
	return ""
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestGeneratedKafkaPublisher(t *testing.T) {
	postgresDSN := os.Getenv("STEGO_TEST_POSTGRES_DSN")
	requirePostgres := os.Getenv("STEGO_REQUIRE_POSTGRES")
	if requirePostgres == "1" && postgresDSN == "" {
		t.Fatal("PostgreSQL integration tests require STEGO_TEST_POSTGRES_DSN")
	}
	files, _, err := new(Generator).Generate(gen.Context{OutputNamespace: "publisher", ModuleName: "example.com/kafka-test", PeerNamespaces: map[string]string{"outbox": "queue"}})
	if err != nil {
		t.Fatal(err)
	}
	queueFiles, _, err := new(queuegen.Generator).Generate(gen.Context{OutputNamespace: "queue"})
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, queueFiles...)
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
	module := `module example.com/kafka-test

go ` + new(Generator).MinimumGoVersion() + `

require (
    github.com/google/uuid v1.6.0
    github.com/jackc/pgx/v5 v5.11.0
    github.com/twmb/franz-go v1.21.6
    github.com/twmb/franz-go/pkg/kfake v0.0.0-20260908033342-6b0b6509f117
)
`
	for name, data := range map[string][]byte{"go.mod": []byte(module), "publisher/publisher_test.go": publisherTests, "publisher/integration_test.go": integrationTests} {
		if err := os.WriteFile(filepath.Join(project, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"mod", "tidy", "-go=" + new(Generator).MinimumGoVersion()}, {"vet", "-mod=readonly", "./..."}, {"test", "-race", "-mod=readonly", "-timeout=60s", "-v", "./..."}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = project
		cmd.Env = append(os.Environ(), "GOWORK=off", "STEGO_TEST_POSTGRES_DSN="+postgresDSN, "STEGO_REQUIRE_POSTGRES="+requirePostgres)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("generated Kafka test, go %v: %v\n%s", args, err, output)
		}
		t.Log(string(output))
	}
}
