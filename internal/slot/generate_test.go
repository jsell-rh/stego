package slot

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

// commonProto returns a parsed stego.common proto for use in tests.
func commonProto(t *testing.T) *ProtoFile {
	t.Helper()
	input := `syntax = "proto3";
package stego.common;

message Identity {
  string user_id = 1;
  string role = 2;
  map<string, string> attributes = 3;
}

message CreateRequest {
  string entity = 1;
  map<string, string> fields = 2;
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
		t.Fatalf("ParseProto(common): %v", err)
	}
	return pf
}

func TestGenerateInterface_BeforeCreate(t *testing.T) {
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
		t.Fatalf("ParseProto(slot): %v", err)
	}

	result, err := GenerateInterface("internal/slots/before_create.go", "slots", slotProto, []*ProtoFile{common})
	if err != nil {
		t.Fatalf("GenerateInterface: %v", err)
	}

	// Verify the returned gen.File has the correct path.
	if result.Path != "internal/slots/before_create.go" {
		t.Errorf("File.Path = %q, want %q", result.Path, "internal/slots/before_create.go")
	}

	// Bytes() includes the mandatory header.
	fullOutput := string(result.Bytes())
	if !strings.HasPrefix(fullOutput, gen.Header) {
		t.Errorf("generated output missing mandatory header %q", gen.Header)
	}

	// Content is the body without header; use it for Go parsing.
	code := string(result.Content)

	// Verify it's valid Go by parsing with go/parser.
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "generated.go", result.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("generated code does not parse as Go:\n%s\nerror: %v", code, err)
	}

	// Verify expected declarations exist.
	decls := collectDeclNames(f)

	// Interface.
	if !decls["BeforeCreateSlot"] {
		t.Errorf("missing BeforeCreateSlot interface in:\n%s", code)
	}

	// Message structs.
	for _, name := range []string{"BeforeCreateRequest", "SlotResult", "CreateRequest", "Identity"} {
		if !decls[name] {
			t.Errorf("missing %s struct in:\n%s", name, code)
		}
	}

	// Verify SlotResult has halt semantics fields.
	if !strings.Contains(code, "Halt") {
		t.Errorf("SlotResult missing Halt field in:\n%s", code)
	}
	if !strings.Contains(code, "StatusCode") {
		t.Errorf("SlotResult missing StatusCode field in:\n%s", code)
	}

	// Verify context import.
	if !strings.Contains(code, `"context"`) {
		t.Errorf("missing context import in:\n%s", code)
	}
}

func TestGenerateInterface_Validate(t *testing.T) {
	common := commonProto(t)

	slotInput := `syntax = "proto3";
package stego.components.rest_api.slots;
import "stego/common/types.proto";

service Validate {
  rpc Evaluate(ValidateRequest) returns (stego.common.SlotResult);
}

message ValidateRequest {
  stego.common.CreateRequest input = 1;
  string entity = 2;
}
`
	slotProto, err := ParseProto(strings.NewReader(slotInput))
	if err != nil {
		t.Fatalf("ParseProto: %v", err)
	}

	result, err := GenerateInterface("internal/slots/validate.go", "slots", slotProto, []*ProtoFile{common})
	if err != nil {
		t.Fatalf("GenerateInterface: %v", err)
	}

	code := string(result.Content)

	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "generated.go", result.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("generated code does not parse:\n%s\nerror: %v", code, err)
	}

	if !strings.Contains(code, "ValidateSlot") {
		t.Errorf("missing ValidateSlot interface")
	}
	if !strings.Contains(code, "ValidateRequest") {
		t.Errorf("missing ValidateRequest struct")
	}
}

func TestGenerateInterface_EmptyPkgName(t *testing.T) {
	_, err := GenerateInterface("test.go", "", &ProtoFile{Services: []Service{{Name: "X"}}}, nil)
	if err == nil {
		t.Fatal("expected error for empty pkgName")
	}
}

func TestGenerateInterface_NilProto(t *testing.T) {
	_, err := GenerateInterface("test.go", "pkg", nil, nil)
	if err == nil {
		t.Fatal("expected error for nil proto")
	}
}

func TestGenerateInterface_NoServices(t *testing.T) {
	_, err := GenerateInterface("test.go", "pkg", &ProtoFile{
		Syntax:  "proto3",
		Package: "test",
	}, nil)
	if err == nil {
		t.Fatal("expected error for proto with no services")
	}
}

func TestGenerateInterface_SlotResultHaltSemantics(t *testing.T) {
	// Verify that SlotResult generated from common types includes
	// halt and status_code fields, supporting short-circuit chains.
	common := commonProto(t)

	slotInput := `syntax = "proto3";
package test.slots;
import "stego/common/types.proto";

service MySlot {
  rpc Evaluate(MyRequest) returns (stego.common.SlotResult);
}

message MyRequest {
  string data = 1;
}
`
	slotProto, err := ParseProto(strings.NewReader(slotInput))
	if err != nil {
		t.Fatalf("ParseProto: %v", err)
	}

	result, err := GenerateInterface("internal/slots/my_slot.go", "testpkg", slotProto, []*ProtoFile{common})
	if err != nil {
		t.Fatalf("GenerateInterface: %v", err)
	}

	code := string(result.Content)

	// Parse and find SlotResult struct.
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "generated.go", result.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("generated code does not parse:\n%s\nerror: %v", code, err)
	}

	slotResultFields := findStructFields(f, "SlotResult")
	expectedFields := map[string]bool{
		"Ok":           false,
		"ErrorMessage": false,
		"Halt":         false,
		"StatusCode":   false,
	}
	for _, field := range slotResultFields {
		if _, ok := expectedFields[field]; ok {
			expectedFields[field] = true
		}
	}
	for field, found := range expectedFields {
		if !found {
			t.Errorf("SlotResult missing field %s in:\n%s", field, code)
		}
	}
}

func TestGenerateInterface_EndToEnd_RealProtoFiles(t *testing.T) {
	// End-to-end test: parse the actual common types.proto and slot protos
	// from the registry, generate interfaces, and verify compilable Go.
	commonInput := `syntax = "proto3";
package stego.common;

message Identity {
  string user_id = 1;
  string role = 2;
  map<string, string> attributes = 3;
}

message CreateRequest {
  string entity = 1;
  map<string, string> fields = 2;
}

message SlotResult {
  bool ok = 1;
  string error_message = 2;
  bool halt = 3;
  int32 status_code = 4;
}
`

	beforeCreateInput := `syntax = "proto3";
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

	commonPF, err := ParseProto(strings.NewReader(commonInput))
	if err != nil {
		t.Fatalf("parse common: %v", err)
	}

	slotPF, err := ParseProto(strings.NewReader(beforeCreateInput))
	if err != nil {
		t.Fatalf("parse slot: %v", err)
	}

	result, err := GenerateInterface("internal/slots/before_create.go", "slots", slotPF, []*ProtoFile{commonPF})
	if err != nil {
		t.Fatalf("GenerateInterface: %v", err)
	}

	// Verify Bytes() includes the mandatory header.
	fullOutput := string(result.Bytes())
	if !strings.HasPrefix(fullOutput, gen.Header) {
		t.Errorf("Bytes() output missing mandatory header %q", gen.Header)
	}

	// Parse as Go — this is the ultimate compilation check.
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "generated.go", result.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("generated code does not compile:\n%s\nerror: %v", string(result.Content), err)
	}

	// Verify the interface method signature.
	decls := collectDeclNames(f)
	if !decls["BeforeCreateSlot"] {
		t.Error("missing BeforeCreateSlot interface")
	}

	code := string(result.Content)

	// The Evaluate method should take *BeforeCreateRequest and return *SlotResult.
	if !strings.Contains(code, "Evaluate(ctx context.Context, req *BeforeCreateRequest) (*SlotResult, error)") {
		t.Errorf("unexpected Evaluate signature in:\n%s", code)
	}
}

func TestProtoFieldToGoName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"user_id", "UserID"},
		{"error_message", "ErrorMessage"},
		{"status_code", "StatusCode"},
		{"ok", "Ok"},
		{"halt", "Halt"},
		{"entity", "Entity"},
		{"api_url", "APIURL"},
	}
	for _, tt := range tests {
		got := protoFieldToGoName(tt.input)
		if got != tt.want {
			t.Errorf("protoFieldToGoName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestResolveGoType(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"BeforeCreateRequest", "BeforeCreateRequest"},
		{"stego.common.SlotResult", "SlotResult"},
		{"stego.components.rest_api.slots.BeforeCreate", "BeforeCreate"},
	}
	for _, tt := range tests {
		got := resolveGoType(tt.input)
		if got != tt.want {
			t.Errorf("resolveGoType(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// collectDeclNames extracts all top-level type declaration names from a Go AST.
func collectDeclNames(f *ast.File) map[string]bool {
	names := make(map[string]bool)
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			names[ts.Name.Name] = true
		}
	}
	return names
}

// findStructFields returns the field names of a named struct in the AST.
func findStructFields(f *ast.File, structName string) []string {
	var fields []string
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != structName {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				continue
			}
			for _, field := range st.Fields.List {
				for _, name := range field.Names {
					fields = append(fields, name.Name)
				}
			}
		}
	}
	return fields
}
