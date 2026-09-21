package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/httpapplication"
	"github.com/jsell-rh/stego/internal/types"
)

func TestHTTPResponseMappingsGateOutput(t *testing.T) {
	input := applicationPreflightInput(t, new(httpapplication.Generator), "domain")
	metadata, err := os.ReadFile("../../registry/components/http-application/component.yaml")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(input.RegistryDir, "components/stub-api/component.yaml"), strings.Replace(string(metadata), "name: http-application", "name: stub-api", 1))
	probe := &modelProbe{models: []gen.GoModel{{Name: "Parcel", GoType: "Parcel", Fields: []gen.GoModelField{{Name: "id", Selection: "ID", Type: types.FieldTypeString}}}}}
	input.Generators["stub-store"] = probe
	servicePath := filepath.Join(input.ProjectDir, "service.yaml")
	service, err := os.ReadFile(servicePath)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, servicePath, string(service)+`
overrides:
  stub-api:
    factory_package: domain
    document: responses.yaml
    response_mappings:
      - name: Parcel
        provider: stub-store
        model: Parcel
        schema: Parcel
        fields:
          - {target: id, source: id}
`)
	schema := `openapi: 3.0.3
info: {title: Shipping, version: '1'}
paths:
  /parcels:
    get:
      operationId: getParcel
      responses:
        '200':
          description: Parcel
          content:
            application/json:
              schema: {$ref: '#/components/schemas/Parcel'}
components:
  schemas:
    Parcel:
      type: object
      required: [id]
      properties:
        id: {type: string}
`
	writeFile(t, filepath.Join(input.ProjectDir, "responses.yaml"), schema)
	if result, err := Validate(input); err != nil || result.HasErrors() {
		t.Fatal("valid HTTP mapping rejected", result, err)
	}
	writeFile(t, filepath.Join(input.ProjectDir, "responses.yaml"), schema+"        new_field: {type: string}\n")
	result, err := Validate(input)
	if err != nil || !result.HasErrors() || !strings.Contains(FormatValidation(result), `property "new_field" has no mapping`) {
		t.Fatal("new response property did not require a mapping", result, err)
	}
	if plan, err := Reconcile(input); plan != nil || err == nil || !strings.Contains(err.Error(), `property "new_field" has no mapping`) {
		t.Fatal("incomplete response mapping returned an output plan", plan, err)
	}
	if probe.renders != 0 {
		t.Fatal("a provider rendered before response validation completed")
	}
	retained, err := os.ReadFile(filepath.Join(input.ProjectDir, "out/retained.txt"))
	if err != nil || string(retained) != "retained output" {
		t.Fatal("response validation changed existing output", err)
	}
}
