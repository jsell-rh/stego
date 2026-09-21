package grpcapplication_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/grpcapplication"
)

func TestGeneratedTimestampConversions(t *testing.T) {
	for _, namespace := range []string{"grpcapi", "rpc/provider"} {
		t.Run(namespace, func(t *testing.T) {
			var sources [][]byte
			for _, standalone := range []bool{false, true} {
				ctx := processContext(t)
				ctx.OutputNamespace = namespace
				ctx.Inputs["factory/rpc.go"] = []byte(strings.ReplaceAll(string(ctx.Inputs["factory/rpc.go"]), "out/grpcapi/", "out/"+namespace+"/"))
				var generator gen.Generator = new(grpcapplication.Generator)
				if standalone {
					delete(ctx.ComponentConfig, "factory_package")
					generator = new(grpcapplication.ProcessGenerator)
				}
				files, _, err := generator.Generate(ctx)
				if err != nil {
					t.Fatal(err)
				}
				var source []byte
				for _, f := range files {
					if f.Path == path.Join(namespace, "transport/conversion.go") {
						if source != nil {
							t.Fatal("conversion source is repeated")
						}
						source = f.Bytes()
					}
				}
				if len(source) == 0 {
					t.Fatal("conversion source is missing")
				}
				sources = append(sources, source)
			}
			if !bytes.Equal(sources[0], sources[1]) {
				t.Fatal("application and standalone conversion contracts differ")
			}
			project := t.TempDir()
			directory := filepath.Join(project, "out", filepath.FromSlash(namespace), "transport")
			if err := os.MkdirAll(directory, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "conversion.go"), sources[0], 0644); err != nil {
				t.Fatal(err)
			}
			tests, err := os.ReadFile("testdata/conversion_test.go")
			if err != nil {
				t.Fatal(err)
			}
			tests = []byte(strings.ReplaceAll(string(tests), "CONVERSION_NAMESPACE", namespace))
			if err := os.WriteFile(filepath.Join(directory, "conversion_test.go"), tests, 0644); err != nil {
				t.Fatal(err)
			}
			module := fmt.Sprintf("module example.com/timestamp-test\ngo %s\nrequire google.golang.org/protobuf v1.36.11\n", new(grpcapplication.Generator).MinimumGoVersion())
			if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte(module), 0644); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"mod", "tidy"}, {"test", "-json", "-race", "-count=1", "-mod=readonly", "-timeout=30s", "./..."}} {
				command := exec.Command("go", args...)
				command.Dir = project
				command.Env = append(os.Environ(), "GOWORK=off")
				output, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("generated timestamp conversion: %v\n%s", err, output)
				}
				if args[0] == "test" {
					t.Logf("%s", output)
				}
			}
		})
	}
}
