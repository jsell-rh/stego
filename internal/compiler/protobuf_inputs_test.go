package compiler

import (
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResponseMappingPreparedInputsGateOutput(t *testing.T) {
	// Response mappings are served by grpc-processes since grpc-application serves only the declared commands and
	// queries (ST-SUP-RC07-OC19); the schema is the one that grpc-processes ships.
	metadata, err := os.ReadFile("../../registry/components/grpc-processes/component.yaml")
	if err != nil {
		t.Fatal(err)
	}
	input := processPreflightInput(t, strings.Replace(string(metadata), "name: grpc-processes", "name: stub-api", 1), "grpcapi")
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
    processes:
      - {name: records, factory_package: factory}
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
		"wrong type": strings.Replace(declaration, "name: Carrier, type: string", "name: Carrier, type: bool", 1),
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
