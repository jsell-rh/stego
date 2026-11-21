package compiler

import (
	"slices"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/registry"
)

func TestReadWithVariables(t *testing.T) {
	t.Run("ST-DLV-UC01-R06-T06 The platform requirements leave out a variable whose runtime package the generated service does not import", func(t *testing.T) {
		pin := registry.RuntimePin{Module: "github.com/jsell-rh/stego/registry/runtime", Version: "v0.125.0", Sum: "h1:a"}
		variables := []registry.EnvironmentVariable{{Name: "DATABASE_URL"}, {Name: "STEGO_WORKER_DATABASE_URL", ReadWith: "service"}, {Name: "STEGO_KAFKA_TOPIC", ReadWith: "outbox/kafkaoutbox"}}
		names := func(files []gen.File) []string {
			var out []string
			for _, variable := range readVariables(variables, runtimePackages(files, pin)) {
				out = append(out, variable.Name)
			}
			return out
		}
		// A browser-service imports no package of the runtime: only the variable with no Read with stays.
		browser := []gen.File{{Path: "main.go", Content: []byte("package main\n\nimport \"example.com/svc/out/browser\"\n\nvar _ = browser.New\n")}, {Path: "browser/schema.sql", Content: []byte("import \"x\"\n")}}
		if got := names(browser); !slices.Equal(got, []string{"DATABASE_URL"}) {
			t.Errorf("a service that imports no runtime package lists %v, want only DATABASE_URL", got)
		}
		// A service that imports the runtime package service lists its variables, and not those of kafkaoutbox.
		service := append(slices.Clone(browser), gen.File{Path: "internal/stegoruntime/runtime.go", Content: []byte("package stegoruntime\n\nimport (\n\t\"github.com/jsell-rh/stego/registry/runtime/service\"\n\tother \"github.com/jsell-rh/stego/registry/runtime/servicex\"\n)\n")})
		if got := names(service); !slices.Equal(got, []string{"DATABASE_URL", "STEGO_WORKER_DATABASE_URL"}) {
			t.Errorf("a service that imports runtime/service lists %v, want DATABASE_URL and STEGO_WORKER_DATABASE_URL", got)
		}
		kafka := append(slices.Clone(service), gen.File{Path: "internal/outbox/runtime.go", Content: []byte("package outbox\n\nimport \"github.com/jsell-rh/stego/registry/runtime/outbox/kafkaoutbox\"\n")})
		if got := names(kafka); len(got) != 3 {
			t.Errorf("a service that imports both packages lists %v, want all three variables", got)
		}
	})
}

func TestKeyRingNeed(t *testing.T) {
	t.Run("ST-SUP-RC32-OC21-T02 A service with no secret field gets no key ring variable and no key ring need", func(t *testing.T) {
		pin := registry.RuntimePin{Module: "github.com/jsell-rh/stego/registry/runtime", Version: "v0.180.0", Sum: "h1:a"}
		source := &registry.SecretSource{Name: "{service}-secret-key-ring", Key: "key-ring"}
		variables := []registry.EnvironmentVariable{{Name: "STEGO_MODE"}, {Name: "STEGO_SECRET_KEY_RING_FILE", Type: "path", SecretSource: source, ReadWith: "sealed"}}
		needs := []registry.Need{{Kind: "secret", Name: "{service}-secret-key-ring", Keys: []string{"key-ring"}}, {Kind: "secret", Name: "{service}-other", Keys: []string{"k"}}}
		plain := []gen.File{{Path: "internal/api/data/entities.go", Content: []byte("package data\n\nimport \"github.com/jsell-rh/stego/registry/runtime/command\"\n")}}
		sealed := append(slices.Clone(plain), gen.File{Path: "internal/api/data/vault.go", Content: []byte("package data\n\nimport \"github.com/jsell-rh/stego/registry/runtime/sealed\"\n")})
		for _, c := range []struct {
			name  string
			files []gen.File
			want  int
		}{{"no secret field", plain, 1}, {"a secret field", sealed, 2}} {
			read := readVariables(variables, runtimePackages(c.files, pin))
			got := readNeeds(needs, variables, read)
			if len(read) != c.want || len(got) != c.want {
				t.Errorf("a service with %s lists %d variables and %d needs, want %d of each", c.name, len(read), len(got), c.want)
			}
			if c.want == 1 && (got[0].Name != "{service}-other" || read[0].Name != "STEGO_MODE") {
				t.Errorf("a service with no secret field keeps %v and %v", read, got)
			}
		}
	})
}

func TestClientVariables(t *testing.T) {
	t.Run("ST-DLV-UC01-R06-T07 The platform requirements leave out a variable that only a client program of the component reads", func(t *testing.T) {
		pin := registry.RuntimePin{Module: "github.com/jsell-rh/stego/registry/runtime", Version: "v0.180.0", Sum: "h1:a"}
		variables := []registry.EnvironmentVariable{{Name: "APPLY_CONFIG", ReadByClient: true}, {Name: "DATABASE_URL"}, {Name: "STEGO_WORKER_DATABASE_URL", ReadWith: "service"}}
		service := []gen.File{{Path: "internal/stegoruntime/runtime.go", Content: []byte("package stegoruntime\n\nimport \"github.com/jsell-rh/stego/registry/runtime/service\"\n")}}
		var names []string
		for _, variable := range readVariables(variables, runtimePackages(service, pin)) {
			names = append(names, variable.Name)
		}
		if !slices.Equal(names, []string{"DATABASE_URL", "STEGO_WORKER_DATABASE_URL"}) {
			t.Errorf("the service lists %v, want DATABASE_URL and STEGO_WORKER_DATABASE_URL and not the variable of the CLI", names)
		}
	})
}
