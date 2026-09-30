package grpcapplication_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/grpcapplication"
)

const overlayReference = `syntax="proto3";package sample;message Record{string name=1;int32 size=2;optional string note=3;}`

const overlayFork = `syntax="proto3";package sample;message Record{string name=1;int32 size=2;reserved 3;reserved "note";string extra=4;}`

func overlayContext(fork string) gen.Context {
	return gen.Context{ModuleName: "example.com/overlay", OutDirName: "out", OutputNamespace: "grpcapi", StorageContract: "example.com/overlay/out/contracts/storage", AuthPackage: "example.com/overlay/out/auth", PeerNamespaces: map[string]string{"jwt-auth": "auth"}, Inputs: map[string][]byte{"api.proto": []byte(fork), "reference.proto": []byte(overlayReference)}, ComponentConfig: map[string]any{"factory_package": "domain", "proto_files": []any{map[string]any{"path": "api.proto", "import_path": "api.proto", "reference": "reference.proto"}}}}
}

func TestOverlaysRejectInvalidReferences(t *testing.T) {
	cases := map[string]struct {
		fork      string
		reference string
	}{
		"field removed without retirement":      {`syntax="proto3";package sample;message Record{string name=1;}`, overlayReference},
		"field number reused by a new field":    {`syntax="proto3";package sample;message Record{string name=1;int32 size=2;string tag=3;}`, overlayReference},
		"reference reserved number reused":      {`syntax="proto3";package sample;message Record{string name=1;int32 size=2;string spare=3;}`, `syntax="proto3";package sample;message Record{string name=1;int32 size=2;reserved 3;}`},
		"reference field kind changed":          {`syntax="proto3";package sample;message Record{string name=1;string size=2;}`, overlayReference},
		"reference message removed":             {`syntax="proto3";package sample;message Other{}`, overlayReference},
		"reference field cardinality changed":   {`syntax="proto3";package sample;message Record{string name=1;repeated int32 size=2;}`, overlayReference},
		"reference enum value changed on wire":  {`syntax="proto3";package sample;message Record{string name=1;}enum Level{LEVEL_UNSPECIFIED=0;}`, `syntax="proto3";package sample;message Record{string name=1;}enum Level{LEVEL_UNSPECIFIED=1;}`},
		"reference service method removed":      {`syntax="proto3";package sample;message Record{string name=1;}`, `syntax="proto3";package sample;message Record{string name=1;}message GetRequest{}message GetResponse{}service Records{rpc Get(GetRequest) returns (GetResponse);}`},
		"reference method input type changed":   {`syntax="proto3";package sample;message Record{string name=1;}message A{}message GetRequest{}message GetResponse{}service Records{rpc Get(A) returns (GetResponse);}`, `syntax="proto3";package sample;message Record{string name=1;}message GetRequest{}message GetResponse{}service Records{rpc Get(GetRequest) returns (GetResponse);}`},
		"reference missing from inputs":         {overlayFork, "absent.proto"},
		"reference equals declared file":        {overlayFork, "api.proto"},
		"reference equals declared import path": {overlayFork, "api.proto"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := overlayContext(test.fork)
			entry := ctx.ComponentConfig["proto_files"].([]any)[0].(map[string]any)
			if test.reference == "absent.proto" {
				entry["reference"] = test.reference
			} else if test.reference == "api.proto" {
				entry["reference"] = test.reference
			} else {
				ctx.Inputs["reference.proto"] = []byte(test.reference)
			}
			if _, _, err := new(grpcapplication.Generator).Generate(ctx); err == nil {
				t.Fatalf("invalid overlay accepted: %s", name)
			}
		})
	}
}

func TestOverlaysAcceptValidExtensions(t *testing.T) {
	ctx := overlayContext(overlayFork)
	generator := new(grpcapplication.Generator)
	if err := generator.ValidateContext(ctx); err != nil {
		t.Fatal(err)
	}
	files, _, err := generator.Generate(ctx)
	if err != nil {
		t.Fatalf("valid overlay rejected: %v", err)
	}
	var testPath string
	project := t.TempDir()
	for _, file := range files {
		if !strings.HasPrefix(file.Path, "grpcapi/pb/") {
			continue
		}
		name := filepath.Join(project, "out", file.Path)
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, file.Content, 0644); err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(file.Path, "_overlay_test.go") {
			testPath = file.Path
		}
	}
	if testPath == "" {
		t.Fatal("overlay test not emitted")
	}
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte("module example.com/overlay\ngo 1.26.8\nrequire google.golang.org/protobuf v1.36.11\n"), 0644); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "test", "-mod=mod", "./out/grpcapi/pb/")
	command.Dir = project
	command.Env = append(os.Environ(), "GOWORK=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("overlay test failed: %v\n%s", err, output)
	}
}
