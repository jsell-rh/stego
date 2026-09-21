package compiler

import (
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/grpcapplication"
	"github.com/jsell-rh/stego/internal/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResponseMappingPreparedInputsGateOutput(t *testing.T) {
	input := applicationPreflightInput(t, new(grpcapplication.Generator), "domain")
	archetypePath := filepath.Join(input.RegistryDir, "archetypes/test-arch/archetype.yaml")
	archetype, err := os.ReadFile(archetypePath)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, archetypePath, strings.Replace(string(archetype), "  storage-adapter: stub-store", "  storage-adapter: stub-store\n  auth-provider: jwt-auth", 1))
	metadata, err := os.ReadFile("../../registry/components/grpc-application/component.yaml")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(input.RegistryDir, "components/stub-api/component.yaml"), strings.Replace(string(metadata), "name: grpc-application", "name: stub-api", 1))
	probe := &modelProbe{models: []gen.GoModel{{Name: "Parcel", GoType: "Parcel", Fields: []gen.GoModelField{{Name: "id", Selection: "ID", Type: types.FieldTypeString}}}}}
	input.Generators["stub-store"] = probe
	servicePath := filepath.Join(input.ProjectDir, "service.yaml")
	service, err := os.ReadFile(servicePath)
	if err != nil {
		t.Fatal(err)
	}
	declaration := string(service) + `
overrides:
  stub-api:
    factory_package: domain
    proto_files:
      - {path: api.proto, import_path: api.proto}
    response_mappings:
      - name: Parcel
        provider: stub-store
        model: Parcel
        message: shipping.Parcel
        inputs:
          - {name: Carrier, type: string}
        fields:
          - {target: id, source: id}
          - {target: carrier, input: Carrier}
`
	writeFile(t, servicePath, declaration)
	writeFile(t, filepath.Join(input.ProjectDir, "api.proto"), `syntax="proto3";package shipping;message Parcel{string id=1;string carrier=2;}`)
	if result, err := Validate(input); err != nil || result.HasErrors() {
		t.Fatal("valid prepared protobuf mapping rejected", result, err)
	}
	for name, bad := range map[string]string{
		"missing":    strings.Replace(declaration, "input: Carrier", "input: Missing", 1),
		"wrong type": strings.Replace(declaration, "type: string", "type: bool", 1),
		"ambiguous":  strings.Replace(declaration, "target: carrier, input: Carrier", "target: carrier, source: id, input: Carrier", 1),
	} {
		t.Run(name, func(t *testing.T) {
			writeFile(t, servicePath, bad)
			if result, err := Validate(input); err != nil || !result.HasErrors() {
				t.Fatal("invalid input passed compiler validation", result, err)
			}
			if plan, err := Reconcile(input); err == nil || plan != nil {
				t.Fatal("invalid input produced a plan", plan, err)
			}
			if probe.renders != 0 {
				t.Fatal("a provider rendered before input checks")
			}
			retained, err := os.ReadFile(filepath.Join(input.ProjectDir, "out/retained.txt"))
			if err != nil || string(retained) != "retained output" {
				t.Fatal("input validation changed output", err)
			}
		})
	}
}
