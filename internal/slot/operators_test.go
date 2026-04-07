package slot

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

func TestGenerateOperators_BeforeCreate(t *testing.T) {
	slotInput := `syntax = "proto3";
package stego.components.rest_api.slots;
import "stego/common/types.proto";

service BeforeCreate {
  rpc Evaluate(BeforeCreateRequest) returns (stego.common.SlotResult);
}

message BeforeCreateRequest {
  stego.common.CreateRequest input = 1;
  stego.common.Identity caller = 2;
}
`
	slotProto, err := ParseProto(strings.NewReader(slotInput))
	if err != nil {
		t.Fatalf("ParseProto: %v", err)
	}

	result, err := GenerateOperators("internal/slots/operators.go", "slots", slotProto)
	if err != nil {
		t.Fatalf("GenerateOperators: %v", err)
	}

	if result.Path != "internal/slots/operators.go" {
		t.Errorf("Path = %q, want %q", result.Path, "internal/slots/operators.go")
	}

	// Bytes() includes the mandatory header for .go files.
	fullOutput := string(result.Bytes())
	if !strings.HasPrefix(fullOutput, gen.Header) {
		t.Errorf("generated output missing mandatory header %q", gen.Header)
	}

	code := string(result.Content)

	// Verify it parses as valid Go.
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "operators.go", result.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("generated operators do not parse as Go:\n%s\nerror: %v", code, err)
	}

	decls := collectDeclNames(f)

	// Gate operator.
	if !decls["BeforeCreateGate"] {
		t.Errorf("missing BeforeCreateGate in:\n%s", code)
	}
	// Chain operator.
	if !decls["BeforeCreateChain"] {
		t.Errorf("missing BeforeCreateChain in:\n%s", code)
	}
	// FanOut operator.
	if !decls["BeforeCreateFanOut"] {
		t.Errorf("missing BeforeCreateFanOut in:\n%s", code)
	}

	// Verify gate implements the interface contract (has fills field).
	gateFields := findStructFields(f, "BeforeCreateGate")
	if !containsField(gateFields, "fills") {
		t.Errorf("BeforeCreateGate missing fills field")
	}

	// Verify chain has ShortCircuit field.
	chainFields := findStructFields(f, "BeforeCreateChain")
	if !containsField(chainFields, "ShortCircuit") {
		t.Errorf("BeforeCreateChain missing ShortCircuit field")
	}

	// Verify constructors exist.
	funcs := collectFuncNames(f)
	for _, name := range []string{"NewBeforeCreateGate", "NewBeforeCreateChain", "NewBeforeCreateFanOut"} {
		if !funcs[name] {
			t.Errorf("missing constructor %s in:\n%s", name, code)
		}
	}

	// Verify Evaluate method signatures exist for all operators.
	for _, typeName := range []string{"BeforeCreateGate", "BeforeCreateChain", "BeforeCreateFanOut"} {
		methods := collectMethodNames(f, typeName)
		if !methods["Evaluate"] {
			t.Errorf("%s missing Evaluate method in:\n%s", typeName, code)
		}
	}
}

func TestGenerateOperators_GateLogic(t *testing.T) {
	// Verify the gate code contains the Ok check pattern.
	slotProto := simpleSlotProto(t)

	result, err := GenerateOperators("test.go", "testpkg", slotProto)
	if err != nil {
		t.Fatalf("GenerateOperators: %v", err)
	}

	code := string(result.Content)

	// Gate must check !result.Ok and return early.
	if !strings.Contains(code, "!result.Ok") {
		t.Errorf("gate missing Ok check in:\n%s", code)
	}
	// Gate must return Ok: true at the end.
	if !strings.Contains(code, "Ok: true") {
		t.Errorf("gate missing final Ok: true return in:\n%s", code)
	}
}

func TestGenerateOperators_ChainHaltLogic(t *testing.T) {
	slotProto := simpleSlotProto(t)

	result, err := GenerateOperators("test.go", "testpkg", slotProto)
	if err != nil {
		t.Fatalf("GenerateOperators: %v", err)
	}

	code := string(result.Content)

	// Chain must check ShortCircuit && Halt.
	if !strings.Contains(code, "c.ShortCircuit && result.Halt") {
		t.Errorf("chain missing ShortCircuit halt check in:\n%s", code)
	}
	// Chain must track lastResult.
	if !strings.Contains(code, "lastResult = result") {
		t.Errorf("chain missing lastResult tracking in:\n%s", code)
	}
}

func TestGenerateOperators_FanOutConcurrency(t *testing.T) {
	slotProto := simpleSlotProto(t)

	result, err := GenerateOperators("test.go", "testpkg", slotProto)
	if err != nil {
		t.Fatalf("GenerateOperators: %v", err)
	}

	code := string(result.Content)

	// FanOut must use goroutines.
	if !strings.Contains(code, "go func(") {
		t.Errorf("fan-out missing goroutine launch in:\n%s", code)
	}
	// FanOut must use a channel.
	if !strings.Contains(code, "make(chan fanOutResult") {
		t.Errorf("fan-out missing channel creation in:\n%s", code)
	}
	// FanOut must collect all results before returning.
	if !strings.Contains(code, "for range fo.fills") {
		t.Errorf("fan-out missing result collection loop in:\n%s", code)
	}
}

func TestGenerateOperators_EmptyPkgName(t *testing.T) {
	_, err := GenerateOperators("test.go", "", &ProtoFile{Services: []Service{{Name: "X"}}})
	if err == nil {
		t.Fatal("expected error for empty pkgName")
	}
}

func TestGenerateOperators_NilProto(t *testing.T) {
	_, err := GenerateOperators("test.go", "pkg", nil)
	if err == nil {
		t.Fatal("expected error for nil proto")
	}
}

func TestGenerateOperators_NoServices(t *testing.T) {
	_, err := GenerateOperators("test.go", "pkg", &ProtoFile{
		Syntax:  "proto3",
		Package: "test",
	})
	if err == nil {
		t.Fatal("expected error for proto with no services")
	}
}

func TestGenerateOperators_CompileWithInterface(t *testing.T) {
	// End-to-end test: generate interface AND operators for the same slot,
	// combine them, and verify they compile together as a single package.
	common := commonProto(t)

	slotInput := `syntax = "proto3";
package stego.components.rest_api.slots;
import "stego/common/types.proto";

service BeforeCreate {
  rpc Evaluate(BeforeCreateRequest) returns (stego.common.SlotResult);
}

message BeforeCreateRequest {
  stego.common.CreateRequest input = 1;
  stego.common.Identity caller = 2;
}
`
	slotProto, err := ParseProto(strings.NewReader(slotInput))
	if err != nil {
		t.Fatalf("ParseProto: %v", err)
	}

	ifaceFile, err := GenerateInterface("slots/iface.go", "slots", slotProto, []*ProtoFile{common})
	if err != nil {
		t.Fatalf("GenerateInterface: %v", err)
	}

	opsFile, err := GenerateOperators("slots/operators.go", "slots", slotProto)
	if err != nil {
		t.Fatalf("GenerateOperators: %v", err)
	}

	// Write both files to a temp directory and compile together.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "iface.go"), ifaceFile.Content, 0644); err != nil {
		t.Fatalf("writing iface.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "operators.go"), opsFile.Content, 0644); err != nil {
		t.Fatalf("writing operators.go: %v", err)
	}

	// Parse both files as a package to verify they compile together.
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("package does not compile:\n%v\n\niface.go:\n%s\n\noperators.go:\n%s",
			err, string(ifaceFile.Content), string(opsFile.Content))
	}

	pkg, ok := pkgs["slots"]
	if !ok {
		t.Fatalf("package 'slots' not found in parsed packages: %v", pkgs)
	}

	// Verify all expected types are present across both files.
	allDecls := make(map[string]bool)
	for _, file := range pkg.Files {
		for name := range collectDeclNames(file) {
			allDecls[name] = true
		}
	}

	expected := []string{
		"BeforeCreateSlot",   // interface
		"BeforeCreateGate",   // gate operator
		"BeforeCreateChain",  // chain operator
		"BeforeCreateFanOut", // fan-out operator
		"SlotResult",         // result type
		"BeforeCreateRequest",
	}
	for _, name := range expected {
		if !allDecls[name] {
			t.Errorf("missing declaration %s in combined package", name)
		}
	}
}

func TestGenerateOperators_MultipleServices(t *testing.T) {
	// Proto with two services — operators should be generated for both.
	slotInput := `syntax = "proto3";
package test.slots;

service BeforeCreate {
  rpc Evaluate(CreateReq) returns (Result);
}

service Validate {
  rpc Check(ValidateReq) returns (Result);
}

message CreateReq {
  string data = 1;
}

message ValidateReq {
  string entity = 1;
}

message Result {
  bool ok = 1;
  string error_message = 2;
  bool halt = 3;
  int32 status_code = 4;
}
`
	slotProto, err := ParseProto(strings.NewReader(slotInput))
	if err != nil {
		t.Fatalf("ParseProto: %v", err)
	}

	result, err := GenerateOperators("test.go", "testpkg", slotProto)
	if err != nil {
		t.Fatalf("GenerateOperators: %v", err)
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "operators.go", result.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("generated code does not parse:\n%s\nerror: %v", string(result.Content), err)
	}

	decls := collectDeclNames(f)

	for _, name := range []string{
		"BeforeCreateGate", "BeforeCreateChain", "BeforeCreateFanOut",
		"ValidateGate", "ValidateChain", "ValidateFanOut",
	} {
		if !decls[name] {
			t.Errorf("missing %s", name)
		}
	}

	// Verify each operator's method matches its service.
	methods := collectMethodNames(f, "ValidateGate")
	if !methods["Check"] {
		t.Errorf("ValidateGate missing Check method")
	}
}

// simpleSlotProto returns a minimal parsed slot proto for testing.
func simpleSlotProto(t *testing.T) *ProtoFile {
	t.Helper()
	input := `syntax = "proto3";
package test.slots;

service TestSlot {
  rpc Evaluate(TestRequest) returns (SlotResult);
}

message TestRequest {
  string data = 1;
}

message SlotResult {
  bool ok = 1;
  string error_message = 2;
  bool halt = 3;
  int32 status_code = 4;
}
`
	pf, err := ParseProto(strings.NewReader(input))
	if err != nil {
		t.Fatalf("ParseProto: %v", err)
	}
	return pf
}

// collectFuncNames extracts all top-level function names from a Go AST.
func collectFuncNames(f *ast.File) map[string]bool {
	names := make(map[string]bool)
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		names[fd.Name.Name] = true
	}
	return names
}

// collectMethodNames extracts method names for a given receiver type name.
func collectMethodNames(f *ast.File, typeName string) map[string]bool {
	names := make(map[string]bool)
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Recv == nil {
			continue
		}
		for _, recv := range fd.Recv.List {
			var recvName string
			switch t := recv.Type.(type) {
			case *ast.StarExpr:
				if ident, ok := t.X.(*ast.Ident); ok {
					recvName = ident.Name
				}
			case *ast.Ident:
				recvName = t.Name
			}
			if recvName == typeName {
				names[fd.Name.Name] = true
			}
		}
	}
	return names
}

func containsField(fields []string, name string) bool {
	for _, f := range fields {
		if f == name {
			return true
		}
	}
	return false
}
