package kafkaproducer

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	queuegen "github.com/jsell-rh/stego/internal/generator/outbox"
)

//go:embed testdata/publisher_test.go
var publisherTests []byte

//go:embed testdata/integration_test.go
var integrationTests []byte

func TestGeneratedKafkaPublisher(t *testing.T) {
	postgresDSN := os.Getenv("STEGO_TEST_POSTGRES_DSN")
	requirePostgres := os.Getenv("STEGO_REQUIRE_POSTGRES")
	if requirePostgres == "1" && postgresDSN == "" {
		t.Fatal("PostgreSQL integration tests require STEGO_TEST_POSTGRES_DSN")
	}
	files, _, err := new(Generator).Generate(gen.Context{OutputNamespace: "publisher"})
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

go 1.26.8

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
	for _, args := range [][]string{{"mod", "tidy"}, {"test", "-race", "-mod=readonly", "-timeout=60s", "-v", "./..."}} {
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
