package openapicontract

import (
	"fmt"
	"go/ast"
	"reflect"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestGoResponseEnumBindings(t *testing.T) {
	fixture := strings.Replace(goFixture, "serial: {type: string}", "serial: {type: string, enum: [sent, ready]}", 1)
	doc, err := goDocument(fixture)
	if err != nil {
		t.Fatal(err)
	}
	code, objects, err := GenerateGoModels(doc, "responses", []string{"Shipment"})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, field := range objects["Shipment"].Fields {
		if field.JSONName == "serial" {
			found = true
			if !reflect.DeepEqual(field.StringEnum, []string{"ready", "sent"}) || field.EnumGoType == "" || field.EnumGoType != field.GoType {
				t.Fatalf("wrong enum binding: %#v", field)
			}
			field.StringEnum[0] = "changed"
		}
	}
	if !found {
		t.Fatal("enum field is absent")
	}
	plain, err := GenerateGo(doc, "responses", GoModels)
	if err != nil || code != plain {
		t.Fatal("enum binding changed backend output", err)
	}
	_, again, err := GenerateGoModels(doc, "responses", []string{"Shipment"})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range again["Shipment"].Fields {
		if field.JSONName == "serial" && !reflect.DeepEqual(field.StringEnum, []string{"ready", "sent"}) {
			t.Fatal("enum binding shares mutable storage")
		}
	}
}

func TestGoResponseEnumLimits(t *testing.T) {
	schema := func() *openapi3.Schema {
		return &openapi3.Schema{Type: &openapi3.Types{"string"}, Enum: []any{"ready", "sent"}}
	}
	for name, edit := range map[string]func(*openapi3.Schema){
		"count": func(s *openapi3.Schema) {
			s.Enum = make([]any, 257)
			for i := range s.Enum {
				s.Enum[i] = fmt.Sprint(i)
			}
		},
		"value bytes": func(s *openapi3.Schema) { s.Enum = []any{strings.Repeat("a", 4097)} },
		"total bytes": func(s *openapi3.Schema) {
			s.Enum = nil
			for i := range 17 {
				s.Enum = append(s.Enum, fmt.Sprintf("%04d", i)+strings.Repeat("a", 4092))
			}
		},
		"duplicate":   func(s *openapi3.Schema) { s.Enum = []any{"ready", "ready"} },
		"non string":  func(s *openapi3.Schema) { s.Enum = []any{true} },
		"encoding":    func(s *openapi3.Schema) { s.Enum = []any{string([]byte{255})} },
		"composition": func(s *openapi3.Schema) { s.Not = &openapi3.SchemaRef{Value: openapi3.NewStringSchema()} },
	} {
		t.Run(name, func(t *testing.T) {
			s := schema()
			edit(s)
			values, name, err := stringEnumProperty(s, ast.NewIdent("string"), nil)
			if err == nil || values != nil || name != "" {
				t.Fatal("invalid enum returned a binding")
			}
		})
	}
	for name, expression := range map[string]ast.Expr{"unresolved": ast.NewIdent("Missing"), "private": ast.NewIdent("private"), "non string": ast.NewIdent("int"), "slice": &ast.ArrayType{Elt: ast.NewIdent("string")}, "cycle": ast.NewIdent("Cycle"), "double pointer": &ast.StarExpr{X: &ast.StarExpr{X: ast.NewIdent("string")}}} {
		t.Run(name, func(t *testing.T) {
			_, _, err := stringEnumProperty(schema(), expression, map[string]ast.Expr{"Cycle": ast.NewIdent("Cycle")})
			if err == nil {
				t.Fatal("unsupported backend enum type passed")
			}
		})
	}
	for _, expression := range []ast.Expr{ast.NewIdent("string"), ast.NewIdent("State"), &ast.StarExpr{X: ast.NewIdent("State")}} {
		s := schema()
		s.Enum = []any{"sent", "", "ready"}
		values, _, err := stringEnumProperty(s, expression, map[string]ast.Expr{"State": ast.NewIdent("Alias"), "Alias": ast.NewIdent("string")})
		if err != nil || !reflect.DeepEqual(values, []string{"", "ready", "sent"}) {
			t.Fatal("valid enum was rejected", err)
		}
	}
}
