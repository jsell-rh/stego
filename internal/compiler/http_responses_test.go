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

func TestHTTPResponseMappingsGateOutput(t *testing.T) { testHTTPResponseMappingsGateOutput(t, false) }
func TestHTTPResponseJSONMappingsGateOutput(t *testing.T) {
	testHTTPResponseMappingsGateOutput(t, true)
}
func testHTTPResponseMappingsGateOutput(t *testing.T, lists bool) {
	t.Helper()
	input := applicationPreflightInput(t, new(httpapplication.Generator), "domain")
	archetypePath := filepath.Join(input.RegistryDir, "archetypes/test-arch/archetype.yaml")
	archetype, err := os.ReadFile(archetypePath)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, archetypePath, strings.Replace(string(archetype), "  storage-adapter: stub-store", "  storage-adapter: stub-store\n  auth-provider: jwt-auth", 1))
	metadata, err := os.ReadFile("../../registry/components/http-application/component.yaml")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(input.RegistryDir, "components/stub-api/component.yaml"), strings.Replace(string(metadata), "name: http-application", "name: stub-api", 1))
	probe := &modelProbe{models: []gen.GoModel{{Name: "Parcel", GoType: "Parcel", Fields: []gen.GoModelField{{Name: "id", Selection: "ID", Type: types.FieldTypeString}}}}}
	if lists {
		probe.models[0].Fields = append(probe.models[0].Fields, gen.GoModelField{Name: "tags", Selection: "Tags", Type: types.FieldTypeJsonb})
	}
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
	if lists {
		schema += "        tags: {type: array, items: {type: string}}\n"
		current, err := os.ReadFile(servicePath)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, servicePath, string(current)+"          - {target: tags, source: tags, conversion: json_strings, max_bytes: 64, max_items: 3, max_item_bytes: 16}\n")
	}
	writeFile(t, filepath.Join(input.ProjectDir, "responses.yaml"), schema)
	if result, err := Validate(input); err != nil || result.HasErrors() {
		t.Fatal("valid HTTP mapping rejected", result, err)
	}
	if lists {
		current, err := os.ReadFile(servicePath)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, servicePath, strings.Replace(string(current), "max_bytes: 64", "max_bytes: 0", 1))
		if result, err := Validate(input); err != nil || !result.HasErrors() || !strings.Contains(FormatValidation(result), "json_strings limits are invalid") {
			t.Fatal("invalid JSON limit passed validation", result, err)
		}
		if plan, err := Reconcile(input); err == nil || plan != nil || !strings.Contains(err.Error(), "json_strings limits are invalid") {
			t.Fatal("invalid JSON limit produced output", plan, err)
		}
		if probe.renders != 0 {
			t.Fatal("JSON limit validation ran after provider generation")
		}
		writeFile(t, servicePath, string(current))
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
