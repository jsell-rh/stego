package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

// setupValidateProject creates a temporary project with a valid service.yaml
// and registry. It returns the project dir, registry dir, and a ready-to-use
// ReconcilerInput. Callers can modify the project or registry before calling
// Validate.
func setupValidateProject(t *testing.T) (string, string, ReconcilerInput) {
	t.Helper()
	projectDir := t.TempDir()
	registryDir := t.TempDir()

	serviceYAML := `kind: service
name: test-service
archetype: test-arch
language: go

entities:
  - name: Widget
    fields:
      - { name: label, type: string }
      - { name: org_id, type: ref, to: Org }
  - name: Org
    fields:
      - { name: name, type: string }

expose:
  - entity: Widget
    operations: [create, read]
  - entity: Org
    operations: [create, read]
`
	writeFile(t, filepath.Join(projectDir, "service.yaml"), serviceYAML)

	// Archetype.
	archDir := filepath.Join(registryDir, "archetypes", "test-arch")
	mkdirAll(t, archDir)
	writeFile(t, filepath.Join(archDir, "archetype.yaml"), `kind: archetype
name: test-arch
language: go
version: 1.0.0
components:
  - stub-api
  - stub-store
conventions:
  layout: flat
  error_handling: problem-details-rfc
  logging: structured-json
  test_pattern: table-driven
bindings:
  storage-adapter: stub-store
`)

	// Components.
	for _, comp := range []struct {
		name, yaml string
		slotProtos []string // proto file names to create under slots/
	}{
		{
			name: "stub-api",
			yaml: `kind: component
name: stub-api
version: 1.0.0
output_namespace: internal/api
requires:
  - storage-adapter
provides:
  - http-server
slots:
  - name: before_create
    proto: stego.components.rest_api.slots.BeforeCreate
    default: passthrough
`,
			slotProtos: []string{"before_create.proto"},
		},
		{
			name: "stub-store",
			yaml: `kind: component
name: stub-store
version: 1.0.0
output_namespace: internal/storage
requires: []
provides:
  - storage-adapter
slots: []
`,
		},
	} {
		compDir := filepath.Join(registryDir, "components", comp.name)
		mkdirAll(t, compDir)
		writeFile(t, filepath.Join(compDir, "component.yaml"), comp.yaml)
		if len(comp.slotProtos) > 0 {
			slotsDir := filepath.Join(compDir, "slots")
			mkdirAll(t, slotsDir)
			for _, proto := range comp.slotProtos {
				writeFile(t, filepath.Join(slotsDir, proto), `syntax = "proto3";
package stego.components.rest_api.slots;

service BeforeCreate {
  rpc Evaluate(BeforeCreateRequest) returns (SlotResult);
}

message BeforeCreateRequest {
  string input = 1;
}

message SlotResult {
  bool ok = 1;
}
`)
			}
		}
	}

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	return projectDir, registryDir, input
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestValidate_AllValid(t *testing.T) {
	_, _, input := setupValidateProject(t)

	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	if result.HasErrors() {
		t.Fatalf("expected no errors, got %d:\n%s", len(result.Errors), FormatValidation(result))
	}
}

func TestValidate_MissingArchetype(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	// Rewrite service.yaml to reference a non-existent archetype.
	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: nonexistent-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: label, type: string }
expose:
  - entity: Widget
    operations: [create]
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	assertHasError(t, result, "archetype", "nonexistent-arch")
}

func TestValidate_MissingComponent(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	// Modify archetype to reference a non-existent component.
	archPath := filepath.Join(registryDir, "archetypes", "test-arch", "archetype.yaml")
	writeFile(t, archPath, `kind: archetype
name: test-arch
language: go
version: 1.0.0
components:
  - stub-api
  - stub-store
  - nonexistent-component
conventions:
  layout: flat
  error_handling: problem-details-rfc
  logging: structured-json
  test_pattern: table-driven
bindings:
  storage-adapter: stub-store
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	assertHasError(t, result, "component", "nonexistent-component")
}

func TestValidate_InvalidFieldType(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: label, type: foobar }
expose:
  - entity: Widget
    operations: [create]
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	assertHasError(t, result, "field-type", "foobar")
}

func TestValidate_RefFieldMissingTo(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: owner, type: ref }
expose:
  - entity: Widget
    operations: [create]
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	assertHasError(t, result, "field-type", "no 'to' attribute")
}

func TestValidate_RefFieldBadTarget(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: owner, type: ref, to: NonexistentEntity }
expose:
  - entity: Widget
    operations: [create]
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	assertHasError(t, result, "field-type", "NonexistentEntity")
}

func TestValidate_EnumFieldNoValues(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: status, type: enum }
expose:
  - entity: Widget
    operations: [create]
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	assertHasError(t, result, "field-type", "no values")
}

func TestValidate_UnresolvedPort(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	// Modify stub-api to require a port that nothing provides.
	compPath := filepath.Join(registryDir, "components", "stub-api", "component.yaml")
	writeFile(t, compPath, `kind: component
name: stub-api
version: 1.0.0
output_namespace: internal/api
requires:
  - storage-adapter
  - magical-port
provides:
  - http-server
slots: []
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	assertHasError(t, result, "port", "magical-port")
}

func TestValidate_MissingFill(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: label, type: string }
expose:
  - entity: Widget
    operations: [create, read]
slots:
  - slot: before_create
    entity: Widget
    gate:
      - nonexistent-fill
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	assertHasError(t, result, "fill", "nonexistent-fill")
}

func TestValidate_FillExists(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: label, type: string }
expose:
  - entity: Widget
    operations: [create, read]
slots:
  - slot: before_create
    entity: Widget
    gate:
      - my-policy
`)

	// Create the fill directory and fill.yaml.
	fillDir := filepath.Join(projectDir, "fills", "my-policy")
	mkdirAll(t, fillDir)
	writeFile(t, filepath.Join(fillDir, "fill.yaml"), `kind: fill
name: my-policy
implements: rest-api.before_create
entity: Widget
qualified_by: tester
qualified_at: 2026-04-01
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	// No fill-related errors.
	for _, e := range result.Errors {
		if e.Category == "fill" {
			t.Errorf("unexpected fill error: %s", e.Message)
		}
	}
}

func TestValidate_ExposeBadEntity(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: label, type: string }
expose:
  - entity: NonexistentEntity
    operations: [create]
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	assertHasError(t, result, "entity-ref", "NonexistentEntity")
}

func TestValidate_ExposeBadParent(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: label, type: string }
expose:
  - entity: Widget
    operations: [create]
    parent: NonexistentParent
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	assertHasError(t, result, "entity-ref", "NonexistentParent")
}

func TestValidate_InvalidOperation(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: label, type: string }
expose:
  - entity: Widget
    operations: [create, explode]
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	assertHasError(t, result, "operation", "explode")
}

func TestValidate_UnknownSlot(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: label, type: string }
expose:
  - entity: Widget
    operations: [create, read]
slots:
  - slot: nonexistent_slot
    entity: Widget
    gate:
      - my-policy
`)

	// Create fill so that fill validation passes.
	fillDir := filepath.Join(projectDir, "fills", "my-policy")
	mkdirAll(t, fillDir)
	writeFile(t, filepath.Join(fillDir, "fill.yaml"), `kind: fill
name: my-policy
implements: stub-api.nonexistent_slot
entity: Widget
qualified_by: tester
qualified_at: 2026-04-01
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	assertHasError(t, result, "slot", "nonexistent_slot")
}

func TestValidate_SlotEntityNotExposed(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: label, type: string }
  - name: Gadget
    fields:
      - { name: name, type: string }
expose:
  - entity: Widget
    operations: [create, read]
slots:
  - slot: before_create
    entity: Gadget
    gate:
      - my-policy
`)

	fillDir := filepath.Join(projectDir, "fills", "my-policy")
	mkdirAll(t, fillDir)
	writeFile(t, filepath.Join(fillDir, "fill.yaml"), `kind: fill
name: my-policy
implements: stub-api.before_create
entity: Gadget
qualified_by: tester
qualified_at: 2026-04-01
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	assertHasError(t, result, "slot", "Gadget")
}

func TestValidate_MultipleErrors(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: label, type: foobar }
      - { name: status, type: enum }
expose:
  - entity: NonexistentEntity
    operations: [create, explode]
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	if !result.HasErrors() {
		t.Fatal("expected errors")
	}
	// Should have at least: invalid type, enum no values, bad entity ref, bad operation.
	if len(result.Errors) < 4 {
		t.Errorf("expected at least 4 errors, got %d:\n%s", len(result.Errors), FormatValidation(result))
	}
	assertHasError(t, result, "field-type", "foobar")
	assertHasError(t, result, "field-type", "no values")
	assertHasError(t, result, "entity-ref", "NonexistentEntity")
	assertHasError(t, result, "operation", "explode")
}

func TestFormatValidation_NoErrors(t *testing.T) {
	r := &ValidationResult{}
	got := FormatValidation(r)
	if !strings.Contains(got, "Validation passed") {
		t.Errorf("expected 'Validation passed' message, got: %s", got)
	}
}

func TestFormatValidation_WithErrors(t *testing.T) {
	r := &ValidationResult{
		Errors: []ValidationError{
			{Category: "field-type", Message: "bad type"},
			{Category: "port", Message: "unresolved"},
		},
	}
	got := FormatValidation(r)
	if !strings.Contains(got, "2 error(s)") {
		t.Errorf("expected '2 error(s)' in output, got: %s", got)
	}
	if !strings.Contains(got, "[field-type]") {
		t.Errorf("expected '[field-type]' category in output, got: %s", got)
	}
	if !strings.Contains(got, "[port]") {
		t.Errorf("expected '[port]' category in output, got: %s", got)
	}
}

func TestValidate_ConstraintTypeApplicability(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	// min_length on int32, max on string, pattern on bool, values on string, to on string.
	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: count, type: int32, min_length: 5 }
      - { name: label, type: string, max: 100 }
      - { name: flag, type: bool, pattern: "^true$" }
      - { name: tag, type: string, values: [a, b] }
      - { name: ref_field, type: string, to: Widget }
      - { name: amount, type: float, min_length: 1 }
      - { name: score, type: double, max_length: 10 }
expose:
  - entity: Widget
    operations: [create]
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}

	// min_length on int32
	assertHasError(t, result, "field-type", "constraint 'min_length' is only valid for string fields, not \"int32\"")
	// max on string
	assertHasError(t, result, "field-type", "constraint 'max' is only valid for numeric fields")
	// pattern on bool
	assertHasError(t, result, "field-type", "constraint 'pattern' is only valid for string fields, not \"bool\"")
	// values on string (not enum)
	assertHasError(t, result, "field-type", "constraint 'values' is only valid for enum fields, not \"string\"")
	// to on string (not ref)
	assertHasError(t, result, "field-type", "attribute 'to' is only valid for ref fields, not \"string\"")
	// min_length on float
	assertHasError(t, result, "field-type", "constraint 'min_length' is only valid for string fields, not \"float\"")
	// max_length on double
	assertHasError(t, result, "field-type", "constraint 'max_length' is only valid for string fields, not \"double\"")
}

func TestValidate_ConstraintTypeApplicability_ValidCombinations(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	// All constraints applied to their correct types — should produce no errors.
	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: label, type: string, min_length: 1, max_length: 255, pattern: "^[a-z]" }
      - { name: count, type: int32, min: 0, max: 100 }
      - { name: score, type: double, min: 0.0, max: 1.0 }
      - { name: role, type: enum, values: [admin, member] }
      - { name: org_id, type: ref, to: Org }
  - name: Org
    fields:
      - { name: name, type: string }
expose:
  - entity: Widget
    operations: [create]
  - entity: Org
    operations: [create]
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	if result.HasErrors() {
		t.Fatalf("expected no errors for valid constraint combinations, got:\n%s", FormatValidation(result))
	}
}

func TestValidate_ComputedWithoutFilledBy(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: status, type: jsonb, computed: true }
expose:
  - entity: Widget
    operations: [create]
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	assertHasError(t, result, "field-type", "has 'computed: true' but no 'filled_by' attribute")
}

func TestValidate_FilledByWithoutComputed(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: status, type: jsonb, filled_by: aggregator }
expose:
  - entity: Widget
    operations: [create]
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	assertHasError(t, result, "field-type", "has 'filled_by: aggregator' but 'computed' is not set to true")
}

func TestValidate_ComputedAndFilledByTogether(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: label, type: string }
      - { name: status, type: jsonb, computed: true, filled_by: status-aggregator }
expose:
  - entity: Widget
    operations: [create]
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	// No computed/filled_by errors.
	for _, e := range result.Errors {
		if strings.Contains(e.Message, "computed") || strings.Contains(e.Message, "filled_by") {
			t.Errorf("unexpected computed/filled_by error: %s", e.Message)
		}
	}
}

func TestValidate_FillStatError(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: label, type: string }
expose:
  - entity: Widget
    operations: [create, read]
slots:
  - slot: before_create
    entity: Widget
    gate:
      - my-policy
`)

	// Create fills/my-policy as a file (not a directory), so that
	// os.Stat("fills/my-policy/fill.yaml") returns a non-IsNotExist error
	// (the parent path component is a file, not a directory).
	fillsDir := filepath.Join(projectDir, "fills")
	mkdirAll(t, fillsDir)
	writeFile(t, filepath.Join(fillsDir, "my-policy"), "not a directory")

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	// Should get an infrastructure error, not a validation result.
	if err == nil {
		t.Fatalf("expected infrastructure error for non-IsNotExist stat failure, got validation result:\n%s", FormatValidation(result))
	}
	if !strings.Contains(err.Error(), "my-policy") {
		t.Errorf("expected error to mention fill name, got: %v", err)
	}
}

func TestValidate_MixinAddedSlotIsValid(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	// Create a mixin with adds_slots.
	mixinDir := filepath.Join(registryDir, "mixins", "event-publisher")
	mkdirAll(t, filepath.Join(mixinDir, "slots"))
	writeFile(t, filepath.Join(mixinDir, "mixin.yaml"), `kind: mixin
name: event-publisher
version: 1.0.0
adds_components: []
adds_slots:
  - name: on_entity_changed
    proto: stego.mixins.event_publisher.slots.OnEntityChanged
    default: noop
overrides: none
`)
	writeFile(t, filepath.Join(mixinDir, "slots", "on_entity_changed.proto"), `syntax = "proto3";
package stego.mixins.event_publisher.slots;
service OnEntityChanged { rpc Evaluate(OnEntityChangedRequest) returns (SlotResult); }
message OnEntityChangedRequest { string input = 1; }
message SlotResult { bool ok = 1; }
`)

	// Create fill for the mixin slot.
	fillDir := filepath.Join(projectDir, "fills", "my-notifier")
	mkdirAll(t, fillDir)
	writeFile(t, filepath.Join(fillDir, "fill.yaml"), `kind: fill
name: my-notifier
implements: event-publisher.on_entity_changed
entity: Widget
qualified_by: tester
qualified_at: 2026-04-01
`)

	// Service uses the mixin and binds to its slot.
	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: label, type: string }
  - name: Org
    fields:
      - { name: name, type: string }
expose:
  - entity: Widget
    operations: [create, read]
  - entity: Org
    operations: [create, read]
mixins:
  - event-publisher
slots:
  - slot: on_entity_changed
    entity: Widget
    fan-out:
      - my-notifier
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	// The mixin-added slot should be recognized — no slot errors.
	for _, e := range result.Errors {
		if e.Category == "slot" {
			t.Errorf("unexpected slot error: %s", e.Message)
		}
	}
}

func TestValidate_MixinAddedSlotNotAvailableWithoutMixin(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	// Create the mixin in the registry but do NOT declare it in the service.
	mixinDir := filepath.Join(registryDir, "mixins", "event-publisher")
	mkdirAll(t, filepath.Join(mixinDir, "slots"))
	writeFile(t, filepath.Join(mixinDir, "mixin.yaml"), `kind: mixin
name: event-publisher
version: 1.0.0
adds_components: []
adds_slots:
  - name: on_entity_changed
    proto: stego.mixins.event_publisher.slots.OnEntityChanged
    default: noop
overrides: none
`)
	writeFile(t, filepath.Join(mixinDir, "slots", "on_entity_changed.proto"), `syntax = "proto3";
package stego.mixins.event_publisher.slots;
service OnEntityChanged { rpc Evaluate(OnEntityChangedRequest) returns (SlotResult); }
message OnEntityChangedRequest { string input = 1; }
message SlotResult { bool ok = 1; }
`)

	fillDir := filepath.Join(projectDir, "fills", "my-notifier")
	mkdirAll(t, fillDir)
	writeFile(t, filepath.Join(fillDir, "fill.yaml"), `kind: fill
name: my-notifier
implements: event-publisher.on_entity_changed
entity: Widget
qualified_by: tester
qualified_at: 2026-04-01
`)

	// Service does NOT use the mixin but tries to bind to its slot.
	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: label, type: string }
  - name: Org
    fields:
      - { name: name, type: string }
expose:
  - entity: Widget
    operations: [create, read]
  - entity: Org
    operations: [create, read]
slots:
  - slot: on_entity_changed
    entity: Widget
    fan-out:
      - my-notifier
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	// The mixin slot should NOT be available because the mixin is not declared.
	assertHasError(t, result, "slot", "on_entity_changed")
}

func TestValidate_UniqueCompositeReferencesNonexistentField(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: email, type: string, unique_composite: [nonexistent_field] }
      - { name: label, type: string }
expose:
  - entity: Widget
    operations: [create]
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	assertHasError(t, result, "field-type", "unique_composite references field \"nonexistent_field\"")
}

func TestValidate_UniqueCompositeValidReferences(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: email, type: string, unique_composite: [label] }
      - { name: label, type: string }
expose:
  - entity: Widget
    operations: [create]
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	// No unique_composite errors.
	for _, e := range result.Errors {
		if strings.Contains(e.Message, "unique_composite") {
			t.Errorf("unexpected unique_composite error: %s", e.Message)
		}
	}
}

func TestValidate_ExposeScopeNonexistentField(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: label, type: string }
      - { name: org_id, type: ref, to: Org }
  - name: Org
    fields:
      - { name: name, type: string }
expose:
  - entity: Widget
    operations: [list]
    scope: nonexistent_field
  - entity: Org
    operations: [create, read]
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	assertHasError(t, result, "entity-ref", "scope \"nonexistent_field\"")
}

func TestValidate_ExposeScopeValidField(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: label, type: string }
      - { name: org_id, type: ref, to: Org }
  - name: Org
    fields:
      - { name: name, type: string }
expose:
  - entity: Widget
    operations: [list]
    scope: org_id
  - entity: Org
    operations: [create, read]
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	// No scope-related errors.
	for _, e := range result.Errors {
		if strings.Contains(e.Message, "scope") {
			t.Errorf("unexpected scope error: %s", e.Message)
		}
	}
}

func TestValidate_ExposeUpsertKeyNonexistentField(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: label, type: string }
      - { name: resource_type, type: string }
expose:
  - entity: Widget
    operations: [upsert]
    upsert_key: [resource_type, nonexistent_field]
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	assertHasError(t, result, "entity-ref", "upsert_key field \"nonexistent_field\"")
	// resource_type should NOT cause an error since it exists.
	for _, e := range result.Errors {
		if strings.Contains(e.Message, "resource_type") {
			t.Errorf("unexpected error for valid upsert_key field resource_type: %s", e.Message)
		}
	}
}

func TestValidate_ExposeUpsertKeyValidFields(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: resource_type, type: string }
      - { name: resource_id, type: string }
      - { name: adapter, type: string }
expose:
  - entity: Widget
    operations: [upsert]
    upsert_key: [resource_type, resource_id, adapter]
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	// No upsert_key errors.
	for _, e := range result.Errors {
		if strings.Contains(e.Message, "upsert_key") {
			t.Errorf("unexpected upsert_key error: %s", e.Message)
		}
	}
}

func TestValidate_DuplicateEntityName(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: label, type: string }
  - name: Widget
    fields:
      - { name: name, type: string }
expose:
  - entity: Widget
    operations: [create]
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	assertHasError(t, result, "entity", "entity \"Widget\" is defined more than once")
}

func TestValidate_DuplicateFieldName(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: label, type: string }
      - { name: label, type: int32 }
expose:
  - entity: Widget
    operations: [create]
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	assertHasError(t, result, "field-type", "entity \"Widget\" has duplicate field name \"label\"")
}

func TestValidate_UpsertKeyWithoutUpsertOperation(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: resource_type, type: string }
      - { name: resource_id, type: string }
expose:
  - entity: Widget
    operations: [create, read]
    upsert_key: [resource_type, resource_id]
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	assertHasError(t, result, "entity-ref", "specifies upsert_key but does not include 'upsert'")
}

func TestValidate_UpsertKeyWithUpsertOperation(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: resource_type, type: string }
      - { name: resource_id, type: string }
expose:
  - entity: Widget
    operations: [upsert, list]
    upsert_key: [resource_type, resource_id]
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	// No upsert_key/operation errors.
	for _, e := range result.Errors {
		if strings.Contains(e.Message, "upsert_key") {
			t.Errorf("unexpected upsert_key error: %s", e.Message)
		}
	}
}

func TestValidate_UpsertOperationWithoutUpsertKey(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: resource_type, type: string }
      - { name: resource_id, type: string }
expose:
  - entity: Widget
    operations: [upsert]
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	assertHasError(t, result, "entity-ref", "includes 'upsert' operation but does not specify upsert_key")
}

func TestValidate_ConcurrencyWithoutUpsertOperation(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: label, type: string }
expose:
  - entity: Widget
    operations: [create, read]
    concurrency: optimistic
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	assertHasError(t, result, "entity-ref", "specifies concurrency \"optimistic\" but does not include 'upsert'")
}

func TestValidate_ConcurrencyWithUpsertOperation(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: resource_type, type: string }
      - { name: resource_id, type: string }
      - { name: generation, type: int64 }
expose:
  - entity: Widget
    operations: [upsert, list]
    upsert_key: [resource_type, resource_id]
    concurrency: optimistic
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	// No concurrency errors.
	for _, e := range result.Errors {
		if strings.Contains(e.Message, "concurrency") {
			t.Errorf("unexpected concurrency error: %s", e.Message)
		}
	}
}

func TestValidate_InvalidConcurrencyMode(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: resource_type, type: string }
      - { name: resource_id, type: string }
expose:
  - entity: Widget
    operations: [upsert, list]
    upsert_key: [resource_type, resource_id]
    concurrency: pessimistic
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	assertHasError(t, result, "entity-ref", "invalid concurrency mode \"pessimistic\"")
}

func TestValidate_ParentNotInExposeList(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	// Org is defined in entities but NOT in the expose list.
	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: label, type: string }
      - { name: org_id, type: ref, to: Org }
  - name: Org
    fields:
      - { name: name, type: string }
expose:
  - entity: Widget
    operations: [create, read]
    parent: Org
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	assertHasError(t, result, "entity-ref", "declares parent \"Org\", but \"Org\" is not in the expose list")
}

func TestValidate_ParentInExposeList(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	// Both Widget and Org are in the expose list — no error expected.
	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: label, type: string }
      - { name: org_id, type: ref, to: Org }
  - name: Org
    fields:
      - { name: name, type: string }
expose:
  - entity: Org
    operations: [create, read]
  - entity: Widget
    operations: [create, read]
    parent: Org
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	// No parent-related errors.
	for _, e := range result.Errors {
		if strings.Contains(e.Message, "parent") {
			t.Errorf("unexpected parent error: %s", e.Message)
		}
	}
}

func TestValidate_DuplicateExposeBlock(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: label, type: string }
  - name: Org
    fields:
      - { name: name, type: string }
expose:
  - entity: Widget
    operations: [create]
  - entity: Widget
    operations: [read]
  - entity: Org
    operations: [create]
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	assertHasError(t, result, "entity-ref", "duplicate expose blocks for entity \"Widget\"")
	// Org should NOT have a duplicate error.
	for _, e := range result.Errors {
		if strings.Contains(e.Message, "duplicate expose blocks") && strings.Contains(e.Message, "Org") {
			t.Errorf("unexpected duplicate expose block error for Org: %s", e.Message)
		}
	}
}

func TestValidate_DuplicateOperationInExposeBlock(t *testing.T) {
	projectDir, registryDir, _ := setupValidateProject(t)

	writeFile(t, filepath.Join(projectDir, "service.yaml"), `kind: service
name: test-service
archetype: test-arch
language: go
entities:
  - name: Widget
    fields:
      - { name: label, type: string }
expose:
  - entity: Widget
    operations: [create, create, read]
`)

	input := ReconcilerInput{
		ProjectDir:  projectDir,
		RegistryDir: registryDir,
		Generators:  map[string]gen.Generator{},
		GoVersion:   "1.22",
		ModuleName:  "github.com/test/svc",
	}
	result, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	assertHasError(t, result, "operation", "duplicate operation \"create\"")
	// "read" should NOT have a duplicate error.
	for _, e := range result.Errors {
		if strings.Contains(e.Message, "duplicate operation") && strings.Contains(e.Message, "read") {
			t.Errorf("unexpected duplicate operation error for read: %s", e.Message)
		}
	}
}

// assertHasError checks that the result contains at least one error with the
// given category whose message contains the given substring.
func assertHasError(t *testing.T, result *ValidationResult, category, messageSubstring string) {
	t.Helper()
	for _, e := range result.Errors {
		if e.Category == category && strings.Contains(e.Message, messageSubstring) {
			return
		}
	}
	t.Errorf("expected error with category %q containing %q, got:\n%s",
		category, messageSubstring, FormatValidation(result))
}
