package slot

import (
	"strings"
	"testing"
)

func TestParseProto_CommonTypes(t *testing.T) {
	input := `syntax = "proto3";
package stego.common;

// Identity represents the caller's identity.
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
		t.Fatalf("ParseProto: %v", err)
	}

	if pf.Syntax != "proto3" {
		t.Errorf("Syntax = %q, want %q", pf.Syntax, "proto3")
	}
	if pf.Package != "stego.common" {
		t.Errorf("Package = %q, want %q", pf.Package, "stego.common")
	}
	if len(pf.Services) != 0 {
		t.Errorf("expected 0 services, got %d", len(pf.Services))
	}
	if len(pf.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(pf.Messages))
	}

	// Verify Identity message.
	identity := pf.Messages[0]
	if identity.Name != "Identity" {
		t.Errorf("Messages[0].Name = %q, want %q", identity.Name, "Identity")
	}
	if len(identity.Fields) != 3 {
		t.Fatalf("Identity fields: got %d, want 3", len(identity.Fields))
	}
	if identity.Fields[0].Name != "user_id" || identity.Fields[0].Type != "string" {
		t.Errorf("Identity.Fields[0] = %+v, want user_id:string", identity.Fields[0])
	}
	if identity.Fields[2].Type != "map<string, string>" {
		t.Errorf("Identity.Fields[2].Type = %q, want %q", identity.Fields[2].Type, "map<string, string>")
	}

	// Verify SlotResult has halt and status_code.
	slotResult := pf.Messages[2]
	if slotResult.Name != "SlotResult" {
		t.Errorf("Messages[2].Name = %q, want %q", slotResult.Name, "SlotResult")
	}
	if len(slotResult.Fields) != 4 {
		t.Fatalf("SlotResult fields: got %d, want 4", len(slotResult.Fields))
	}
	// Check halt field.
	if slotResult.Fields[2].Name != "halt" || slotResult.Fields[2].Type != "bool" {
		t.Errorf("SlotResult halt field = %+v", slotResult.Fields[2])
	}
	// Check status_code field.
	if slotResult.Fields[3].Name != "status_code" || slotResult.Fields[3].Type != "int32" {
		t.Errorf("SlotResult status_code field = %+v", slotResult.Fields[3])
	}
}

func TestParseProto_SlotWithImport(t *testing.T) {
	input := `syntax = "proto3";
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
	pf, err := ParseProto(strings.NewReader(input))
	if err != nil {
		t.Fatalf("ParseProto: %v", err)
	}

	if pf.Package != "stego.components.rest_api.slots" {
		t.Errorf("Package = %q", pf.Package)
	}
	if len(pf.Imports) != 1 || pf.Imports[0] != "stego/common/types.proto" {
		t.Errorf("Imports = %v, want [stego/common/types.proto]", pf.Imports)
	}
	if len(pf.Services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(pf.Services))
	}

	svc := pf.Services[0]
	if svc.Name != "BeforeCreate" {
		t.Errorf("Service.Name = %q", svc.Name)
	}
	if len(svc.Methods) != 1 {
		t.Fatalf("expected 1 method, got %d", len(svc.Methods))
	}

	m := svc.Methods[0]
	if m.Name != "Evaluate" {
		t.Errorf("Method.Name = %q", m.Name)
	}
	if m.InputType != "BeforeCreateRequest" {
		t.Errorf("Method.InputType = %q", m.InputType)
	}
	if m.OutputType != "stego.common.SlotResult" {
		t.Errorf("Method.OutputType = %q", m.OutputType)
	}

	if len(pf.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(pf.Messages))
	}
	msg := pf.Messages[0]
	if msg.Name != "BeforeCreateRequest" {
		t.Errorf("Message.Name = %q", msg.Name)
	}
	if len(msg.Fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(msg.Fields))
	}
	if msg.Fields[0].Type != "stego.common.CreateRequest" {
		t.Errorf("Field[0].Type = %q", msg.Fields[0].Type)
	}
}

func TestParseProto_MissingSyntax(t *testing.T) {
	input := `package stego.common;
message Foo {
  string bar = 1;
}
`
	_, err := ParseProto(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for missing syntax")
	}
	if !strings.Contains(err.Error(), "syntax") {
		t.Errorf("error should mention syntax: %v", err)
	}
}

func TestParseProto_MissingPackage(t *testing.T) {
	input := `syntax = "proto3";
message Foo {
  string bar = 1;
}
`
	_, err := ParseProto(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for missing package")
	}
	if !strings.Contains(err.Error(), "package") {
		t.Errorf("error should mention package: %v", err)
	}
}

func TestParseProto_ValidateProto(t *testing.T) {
	// Parse the validate.proto structure from the spec.
	input := `syntax = "proto3";
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
	pf, err := ParseProto(strings.NewReader(input))
	if err != nil {
		t.Fatalf("ParseProto: %v", err)
	}

	if pf.Services[0].Name != "Validate" {
		t.Errorf("Service.Name = %q, want Validate", pf.Services[0].Name)
	}
	if pf.Messages[0].Fields[1].Type != "string" {
		t.Errorf("ValidateRequest.entity type = %q, want string", pf.Messages[0].Fields[1].Type)
	}
}
