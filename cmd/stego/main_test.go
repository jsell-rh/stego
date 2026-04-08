package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/types"
	"gopkg.in/yaml.v3"
)

func TestRunInit(t *testing.T) {
	tmp := t.TempDir()

	// Set up a minimal registry with a rest-crud archetype.
	setupMinimalRegistry(t, tmp)

	// Change to project directory.
	projDir := filepath.Join(tmp, "myproject")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}
	origDir, _ := os.Getwd()
	if err := os.Chdir(projDir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origDir)

	t.Setenv("STEGO_REGISTRY", filepath.Join(tmp, "registry"))

	err := runInit([]string{"--archetype", "rest-crud"})
	if err != nil {
		t.Fatalf("runInit failed: %v", err)
	}

	// Verify service.yaml was created.
	svcPath := filepath.Join(projDir, "service.yaml")
	data, err := os.ReadFile(svcPath)
	if err != nil {
		t.Fatalf("service.yaml not created: %v", err)
	}

	var svc types.ServiceDeclaration
	if err := yaml.Unmarshal(data, &svc); err != nil {
		t.Fatalf("service.yaml is not valid YAML: %v", err)
	}
	if svc.Kind != "service" {
		t.Errorf("expected kind=service, got %q", svc.Kind)
	}
	if svc.Archetype != "rest-crud" {
		t.Errorf("expected archetype=rest-crud, got %q", svc.Archetype)
	}
	if svc.Name != "myproject" {
		t.Errorf("expected name=myproject, got %q", svc.Name)
	}

	// Verify .stego/config.yaml was created.
	cfgPath := filepath.Join(projDir, ".stego", "config.yaml")
	if _, err := os.Stat(cfgPath); err != nil {
		t.Errorf(".stego/config.yaml not created: %v", err)
	}

	// Verify fills/ was created.
	fillsDir := filepath.Join(projDir, "fills")
	if info, err := os.Stat(fillsDir); err != nil || !info.IsDir() {
		t.Errorf("fills/ directory not created")
	}
}

func TestRunInitAlreadyExists(t *testing.T) {
	tmp := t.TempDir()
	setupMinimalRegistry(t, tmp)

	projDir := filepath.Join(tmp, "existing")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Create existing service.yaml.
	if err := os.WriteFile(filepath.Join(projDir, "service.yaml"), []byte("kind: service"), 0644); err != nil {
		t.Fatal(err)
	}

	origDir, _ := os.Getwd()
	if err := os.Chdir(projDir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origDir)
	t.Setenv("STEGO_REGISTRY", filepath.Join(tmp, "registry"))

	err := runInit([]string{"--archetype", "rest-crud"})
	if err == nil {
		t.Fatal("expected error for existing service.yaml")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected 'already exists' error, got: %v", err)
	}
}

func TestRunInitUnknownArchetype(t *testing.T) {
	tmp := t.TempDir()
	setupMinimalRegistry(t, tmp)

	projDir := filepath.Join(tmp, "proj")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}

	origDir, _ := os.Getwd()
	if err := os.Chdir(projDir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origDir)
	t.Setenv("STEGO_REGISTRY", filepath.Join(tmp, "registry"))

	err := runInit([]string{"--archetype", "nonexistent"})
	if err == nil {
		t.Fatal("expected error for unknown archetype")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' error, got: %v", err)
	}
}

func TestRunInitMissingArchetypeFlag(t *testing.T) {
	err := runInit([]string{})
	if err == nil {
		t.Fatal("expected error for missing --archetype")
	}
	if !strings.Contains(err.Error(), "--archetype is required") {
		t.Errorf("expected '--archetype is required' error, got: %v", err)
	}
}

func TestRunFillCreate(t *testing.T) {
	tmp := t.TempDir()
	setupMinimalRegistry(t, tmp)

	projDir := filepath.Join(tmp, "fillproject")
	if err := os.MkdirAll(filepath.Join(projDir, "fills"), 0755); err != nil {
		t.Fatal(err)
	}

	origDir, _ := os.Getwd()
	if err := os.Chdir(projDir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origDir)
	t.Setenv("STEGO_REGISTRY", filepath.Join(tmp, "registry"))

	// Test with name before flag: fill create my-policy --slot before_create
	err := runFillCreate([]string{"my-policy", "--slot", "before_create"})
	if err != nil {
		t.Fatalf("runFillCreate failed: %v", err)
	}

	// Verify fill.yaml was created.
	fillYAML := filepath.Join(projDir, "fills", "my-policy", "fill.yaml")
	data, err := os.ReadFile(fillYAML)
	if err != nil {
		t.Fatalf("fill.yaml not created: %v", err)
	}

	var fill types.Fill
	if err := yaml.Unmarshal(data, &fill); err != nil {
		t.Fatalf("fill.yaml is not valid YAML: %v", err)
	}
	if fill.Kind != "fill" {
		t.Errorf("expected kind=fill, got %q", fill.Kind)
	}
	if fill.Name != "my-policy" {
		t.Errorf("expected name=my-policy, got %q", fill.Name)
	}
	if fill.Implements != "rest-api.before_create" {
		t.Errorf("expected implements=rest-api.before_create, got %q", fill.Implements)
	}

	// Verify interface.go was created.
	ifacePath := filepath.Join(projDir, "fills", "my-policy", "interface.go")
	ifaceData, err := os.ReadFile(ifacePath)
	if err != nil {
		t.Fatalf("interface.go not created: %v", err)
	}
	if !strings.Contains(string(ifaceData), "BeforeCreateSlot") {
		t.Error("interface.go should contain BeforeCreateSlot interface")
	}
	if !strings.Contains(string(ifaceData), "package my_policy") {
		t.Error("interface.go should have sanitized package name my_policy")
	}
}

func TestRunFillCreateAlreadyExists(t *testing.T) {
	tmp := t.TempDir()
	setupMinimalRegistry(t, tmp)

	projDir := filepath.Join(tmp, "fillproject2")
	fillDir := filepath.Join(projDir, "fills", "existing")
	if err := os.MkdirAll(fillDir, 0755); err != nil {
		t.Fatal(err)
	}

	origDir, _ := os.Getwd()
	if err := os.Chdir(projDir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origDir)
	t.Setenv("STEGO_REGISTRY", filepath.Join(tmp, "registry"))

	err := runFillCreate([]string{"existing", "--slot", "before_create"})
	if err == nil {
		t.Fatal("expected error for existing fill directory")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected 'already exists' error, got: %v", err)
	}
}

func TestRunFillCreateUnknownSlot(t *testing.T) {
	tmp := t.TempDir()
	setupMinimalRegistry(t, tmp)

	projDir := filepath.Join(tmp, "fillproject3")
	if err := os.MkdirAll(filepath.Join(projDir, "fills"), 0755); err != nil {
		t.Fatal(err)
	}

	origDir, _ := os.Getwd()
	if err := os.Chdir(projDir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origDir)
	t.Setenv("STEGO_REGISTRY", filepath.Join(tmp, "registry"))

	err := runFillCreate([]string{"my-fill", "--slot", "nonexistent"})
	if err == nil {
		t.Fatal("expected error for unknown slot")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' error, got: %v", err)
	}
}

func TestRunRegistrySearch(t *testing.T) {
	tmp := t.TempDir()
	setupMinimalRegistry(t, tmp)

	origDir, _ := os.Getwd()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origDir)
	t.Setenv("STEGO_REGISTRY", filepath.Join(tmp, "registry"))

	// Should not error with no filters.
	if err := runRegistrySearch([]string{}); err != nil {
		t.Fatalf("runRegistrySearch failed: %v", err)
	}

	// Filter by provides.
	if err := runRegistrySearch([]string{"--provides", "auth-provider"}); err != nil {
		t.Fatalf("runRegistrySearch with --provides failed: %v", err)
	}

	// Filter by requires.
	if err := runRegistrySearch([]string{"--requires", "storage-adapter"}); err != nil {
		t.Fatalf("runRegistrySearch with --requires failed: %v", err)
	}

	// Filter by slot.
	if err := runRegistrySearch([]string{"--slot", "before_create"}); err != nil {
		t.Fatalf("runRegistrySearch with --slot failed: %v", err)
	}
}

func TestRunRegistryInspect(t *testing.T) {
	tmp := t.TempDir()
	setupMinimalRegistry(t, tmp)

	origDir, _ := os.Getwd()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origDir)
	t.Setenv("STEGO_REGISTRY", filepath.Join(tmp, "registry"))

	if err := runRegistryInspect([]string{"rest-api"}); err != nil {
		t.Fatalf("runRegistryInspect failed: %v", err)
	}
}

func TestRunRegistryInspectNotFound(t *testing.T) {
	tmp := t.TempDir()
	setupMinimalRegistry(t, tmp)

	origDir, _ := os.Getwd()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origDir)
	t.Setenv("STEGO_REGISTRY", filepath.Join(tmp, "registry"))

	err := runRegistryInspect([]string{"nonexistent"})
	if err == nil {
		t.Fatal("expected error for nonexistent component")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' error, got: %v", err)
	}
}

func TestRunRegistryInspectMissingArg(t *testing.T) {
	err := runRegistryInspect([]string{})
	if err == nil {
		t.Fatal("expected error for missing component name")
	}
	if !strings.Contains(err.Error(), "component name is required") {
		t.Errorf("expected 'component name is required' error, got: %v", err)
	}
}

func TestRunRegistryFills(t *testing.T) {
	tmp := t.TempDir()
	setupMinimalRegistry(t, tmp)

	projDir := filepath.Join(tmp, "fillsproject")
	fillDir := filepath.Join(projDir, "fills", "my-fill")
	if err := os.MkdirAll(fillDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a fill that implements before_create.
	fill := types.Fill{
		Kind:       "fill",
		Name:       "my-fill",
		Implements: "rest-api.before_create",
	}
	fillData, _ := yaml.Marshal(fill)
	if err := os.WriteFile(filepath.Join(fillDir, "fill.yaml"), fillData, 0644); err != nil {
		t.Fatal(err)
	}

	origDir, _ := os.Getwd()
	if err := os.Chdir(projDir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origDir)
	t.Setenv("STEGO_REGISTRY", filepath.Join(tmp, "registry"))

	if err := runRegistryFills([]string{"--slot", "before_create"}); err != nil {
		t.Fatalf("runRegistryFills failed: %v", err)
	}
}

func TestRunRegistryFillsMissingSlot(t *testing.T) {
	err := runRegistryFills([]string{})
	if err == nil {
		t.Fatal("expected error for missing --slot")
	}
	if !strings.Contains(err.Error(), "--slot is required") {
		t.Errorf("expected '--slot is required' error, got: %v", err)
	}
}

func TestRunRegistryFillsNoFillsDir(t *testing.T) {
	tmp := t.TempDir()
	origDir, _ := os.Getwd()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origDir)

	// Should not error, just print "No fills directory found."
	if err := runRegistryFills([]string{"--slot", "before_create"}); err != nil {
		t.Fatalf("runRegistryFills should not error when fills dir missing: %v", err)
	}
}

func TestPortListContains(t *testing.T) {
	ports := []types.Port{{Name: "http-server"}, {Name: "auth-provider"}}
	if !portListContains(ports, "http-server") {
		t.Error("expected true for http-server")
	}
	if portListContains(ports, "nonexistent") {
		t.Error("expected false for nonexistent")
	}
}

func TestSlotListContains(t *testing.T) {
	slots := []types.SlotDefinition{{Name: "before_create"}, {Name: "validate"}}
	if !slotListContains(slots, "before_create") {
		t.Error("expected true for before_create")
	}
	if slotListContains(slots, "nonexistent") {
		t.Error("expected false for nonexistent")
	}
}

func TestPortNames(t *testing.T) {
	ports := []types.Port{{Name: "a"}, {Name: "b"}}
	names := portNames(ports)
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Errorf("expected [a b], got %v", names)
	}
}

func TestSlotDefNames(t *testing.T) {
	slots := []types.SlotDefinition{{Name: "x"}, {Name: "y"}}
	names := slotDefNames(slots)
	if len(names) != 2 || names[0] != "x" || names[1] != "y" {
		t.Errorf("expected [x y], got %v", names)
	}
}

func TestRunFillDispatch(t *testing.T) {
	err := runFill([]string{})
	if err == nil {
		t.Fatal("expected error for missing subcommand")
	}

	err = runFill([]string{"unknown"})
	if err == nil {
		t.Fatal("expected error for unknown subcommand")
	}
}

func TestRunRegistryDispatch(t *testing.T) {
	err := runRegistry([]string{})
	if err == nil {
		t.Fatal("expected error for missing subcommand")
	}

	err = runRegistry([]string{"unknown"})
	if err == nil {
		t.Fatal("expected error for unknown subcommand")
	}
}

// setupMinimalRegistry creates a minimal registry with a rest-crud archetype and rest-api component.
func setupMinimalRegistry(t *testing.T, baseDir string) {
	t.Helper()

	// Create archetype.
	archDir := filepath.Join(baseDir, "registry", "archetypes", "rest-crud")
	if err := os.MkdirAll(archDir, 0755); err != nil {
		t.Fatal(err)
	}
	archData := `kind: archetype
name: rest-crud
language: go
version: 1.0.0
components:
  - rest-api
`
	if err := os.WriteFile(filepath.Join(archDir, "archetype.yaml"), []byte(archData), 0644); err != nil {
		t.Fatal(err)
	}

	// Create rest-api component.
	compDir := filepath.Join(baseDir, "registry", "components", "rest-api")
	slotsDir := filepath.Join(compDir, "slots")
	if err := os.MkdirAll(slotsDir, 0755); err != nil {
		t.Fatal(err)
	}
	compData := `kind: component
name: rest-api
version: 2.1.0
output_namespace: internal/api
config:
  port:
    type: int
    default: 8080
requires:
  - auth-provider
  - storage-adapter
provides:
  - http-server
  - openapi-spec
slots:
  - name: before_create
    proto: stego.components.rest_api.slots.BeforeCreate
    default: passthrough
`
	if err := os.WriteFile(filepath.Join(compDir, "component.yaml"), []byte(compData), 0644); err != nil {
		t.Fatal(err)
	}

	// Create before_create.proto stub.
	protoData := `syntax = "proto3";
package stego.components.rest_api.slots;

service BeforeCreate {
  rpc Evaluate(BeforeCreateRequest) returns (SlotResult);
}

message BeforeCreateRequest {
  CreateRequest input = 1;
  Identity caller = 2;
}

message CreateRequest {
}

message Identity {
}

message SlotResult {
  bool ok = 1;
  string error_message = 2;
}
`
	if err := os.WriteFile(filepath.Join(slotsDir, "before_create.proto"), []byte(protoData), 0644); err != nil {
		t.Fatal(err)
	}

	// Create jwt-auth component to satisfy the provides for auth-provider.
	jwtDir := filepath.Join(baseDir, "registry", "components", "jwt-auth")
	if err := os.MkdirAll(jwtDir, 0755); err != nil {
		t.Fatal(err)
	}
	jwtData := `kind: component
name: jwt-auth
version: 1.0.0
provides:
  - auth-provider
`
	if err := os.WriteFile(filepath.Join(jwtDir, "component.yaml"), []byte(jwtData), 0644); err != nil {
		t.Fatal(err)
	}
}
