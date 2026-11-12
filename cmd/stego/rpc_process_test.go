package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

const rpcProto = `syntax="proto3";package sample.v1;message Request{string text=1;}message Response{string text=1;}service Records{rpc Echo(Request)returns(Response);rpc Watch(Request)returns(stream Response);}`

const rpcDeclaration = `kind: service
name: records
archetype: rpc-service
language: go
overrides:
  jwt-auth:
    mode: verifier
  grpc-processes:
    processes: [{name: records, factory_package: factory}]
    proto_files: [{path: api.proto, import_path: sample/v1/records.proto}]
`

// rpcFactory returns the process factory that rpcDeclaration names.
func rpcFactory(t *testing.T) []byte {
	t.Helper()
	_, source, _, _ := runtime.Caller(0)
	factory, err := os.ReadFile(filepath.Join(filepath.Dir(source), "../../internal/generator/grpcapplication/testdata/process_factory.go"))
	if err != nil {
		t.Fatal(err)
	}
	// The archetype uses the registry namespace. The generator fixture renames it.
	return bytes.ReplaceAll(factory, []byte("example.com/grpc-test/out/telemetry"), []byte("example.com/grpc-test/out/tracing"))
}

func TestRPCProcessCommandsWithoutStorage(t *testing.T) {
	// An RPC process service has no data access: its archetype in the registry copy holds otel-tracing and
	// grpc-processes, and no data component (ST-SUP-Q101).
	registry := shippedRegistryCopy(t)
	archetype := filepath.Join(registry, "archetypes", "rpc-service", "archetype.yaml")
	data, err := os.ReadFile(archetype)
	if err != nil {
		t.Fatal(err)
	}
	text := regexp.MustCompile(`(?s)components:\n(  - [a-z-]+\n)+`).ReplaceAllString(string(data), "components:\n  - otel-tracing\n  - grpc-processes\n")
	text = strings.Replace(text, "  storage-adapter: postgres-adapter\n", "  application-grpc-processes: grpc-processes\n", 1)
	if err := os.WriteFile(archetype, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	factory := rpcFactory(t)
	project := t.TempDir()
	t.Chdir(project)
	t.Setenv("STEGO_REGISTRY", registry)
	t.Setenv("STEGO_MODULE", "example.com/grpc-test")
	t.Setenv("STEGO_GO_VERSION", "1.26.11")
	t.Setenv("GOWORK", "off")
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("factory/rpc.go", factory)
	write("api.proto", []byte(rpcProto))
	write("service.yaml", []byte(rpcDeclaration))
	for _, command := range []func([]string) error{runValidate, runApply, runDependencies, runApply, runDrift} {
		if err := command(nil); err != nil {
			t.Fatal(err)
		}
	}
	build := exec.Command("go", "build", "-mod=readonly", "./...")
	if data, err := build.CombinedOutput(); err != nil {
		t.Fatalf("standalone project build: %v\n%s", err, data)
	}
	if _, err := os.Stat("out/grpcapi/processes/records/main.go"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("out/contracts/storage"); !os.IsNotExist(err) {
		t.Fatal("standalone RPC created storage")
	}
	snapshot := map[string][]byte{}
	for _, root := range []string{"out", ".stego"} {
		if err := filepath.WalkDir(root, func(name string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() {
				data, err := os.ReadFile(name)
				if err != nil {
					return err
				}
				snapshot[name] = data
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	write("factory/rpc.go", []byte("package factory\nfunc Open(){}\n"))
	for _, command := range []func([]string) error{runValidate, runPlan, runApply} {
		if err := command(nil); err == nil {
			t.Fatal("invalid process factory accepted")
		}
	}
	for name, want := range snapshot {
		got, err := os.ReadFile(name)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("invalid factory changed output", name, err)
		}
	}
}
