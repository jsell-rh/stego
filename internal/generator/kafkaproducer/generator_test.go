package kafkaproducer

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

//go:embed testdata/publisher_test.go
var publisherTests []byte


func TestKafkaOutboxRefusal(t *testing.T) {
	t.Run("ST-SUP-RC18-OC30-T06 kafka-producer with the outbox is refused", func(t *testing.T) {
		ctx := gen.Context{OutputNamespace: "publisher", ModuleName: "example.com/kafka-test", PeerNamespaces: map[string]string{"outbox": "queue"}}
		for name, check := range map[string]func() error{
			"ValidateContext": func() error { return new(Generator).ValidateContext(ctx) },
			"Generate": func() error {
				_, _, err := new(Generator).Generate(ctx)
				return err
			},
		} {
			err := check()
			if err == nil || !strings.Contains(err.Error(), "ST-SUP-RC20-OC39") || !strings.Contains(err.Error(), "ST-SUP-Q103") {
				t.Fatalf("%s with the outbox gave %v, want a refusal that names ST-SUP-RC20-OC39 and ST-SUP-Q103", name, err)
			}
		}
		files, wiring, err := new(Generator).Generate(gen.Context{OutputNamespace: "publisher"})
		if err != nil || len(files) != 1 || files[0].Path != "publisher/publisher.go" || len(wiring.Constructors) != 0 {
			t.Fatalf("the publisher alone gave %d files, %v and %v, want publisher.go only and no constructor", len(files), wiring, err)
		}
	})
}

func TestGeneratedKafkaPublisher(t *testing.T) {
	files, _, err := new(Generator).Generate(gen.Context{OutputNamespace: "publisher", ModuleName: "example.com/kafka-test"})
	if err != nil {
		t.Fatal(err)
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
	module := `module example.com/kafka-test

go ` + new(Generator).MinimumGoVersion() + `

require (
    github.com/google/uuid v1.6.0
    github.com/twmb/franz-go v1.21.6
    github.com/twmb/franz-go/pkg/kfake v0.0.0-20260908033342-6b0b6509f117
)
`
	for name, data := range map[string][]byte{"go.mod": []byte(module), "publisher/publisher_test.go": publisherTests} {
		if err := os.WriteFile(filepath.Join(project, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"mod", "tidy", "-go=" + new(Generator).MinimumGoVersion()}, {"vet", "-mod=readonly", "./..."}, {"test", "-race", "-mod=readonly", "-timeout=60s", "-v", "./..."}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = project
		cmd.Env = append(os.Environ(), "GOWORK=off")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("generated Kafka test, go %v: %v\n%s", args, err, output)
		}
		t.Log(string(output))
	}
}
