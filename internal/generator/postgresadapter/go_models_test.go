package postgresadapter

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

func TestGoModelDeclarationMatchesGeneratedStorage(t *testing.T) {
	// Read actual Go syntax, including promoted metadata. Do not compare the
	// declaration with the helper that produced it.
	goTypes := map[types.FieldType]string{
		types.FieldTypeString: "string", types.FieldTypeRef: "string", types.FieldTypeEnum: "string",
		types.FieldTypeInt32: "int32", types.FieldTypeInt64: "int64", types.FieldTypeFloat: "float32",
		types.FieldTypeDouble: "float64", types.FieldTypeBool: "bool", types.FieldTypeBytes: "[]byte",
		types.FieldTypeTimestamp: "time.Time", types.FieldTypeJsonb: "datatypes.JSON",
	}
	entity := types.Entity{Name: "Parcel"}
	for _, kind := range []types.FieldType{types.FieldTypeString, types.FieldTypeRef, types.FieldTypeEnum, types.FieldTypeInt32, types.FieldTypeInt64, types.FieldTypeFloat, types.FieldTypeDouble, types.FieldTypeBool, types.FieldTypeBytes, types.FieldTypeTimestamp, types.FieldTypeJsonb} {
		for _, form := range []string{"required", "optional", "computed"} {
			field := types.Field{Name: string(kind) + "_" + form, Type: kind, Optional: form == "optional", Computed: form == "computed"}
			if kind == types.FieldTypeEnum {
				field.Values = []string{"ready", "waiting"}
			}
			if kind == types.FieldTypeRef {
				field.To = "Parcel"
			}
			entity.Fields = append(entity.Fields, field)
		}
	}
	ctx := gen.Context{OutputNamespace: "store", Entities: []types.Entity{entity}}
	models, err := new(Generator).GoModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	file, err := generateModels("store", ctx.Entities, nil)
	if err != nil {
		t.Fatal(err)
	}
	set := token.NewFileSet()
	parsed, err := parser.ParseFile(set, "models.go", file.Bytes(), 0)
	if err != nil {
		t.Fatal(err)
	}
	structs := map[string]*ast.StructType{}
	for _, decl := range parsed.Decls {
		group, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range group.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			if shape, ok := typeSpec.Type.(*ast.StructType); ok {
				structs[typeSpec.Name.Name] = shape
			}
		}
	}
	actual := map[string]string{}
	for _, name := range []string{"Meta", "Parcel"} {
		if structs[name] == nil {
			t.Fatal("generated type is missing", name)
		}
		for _, field := range structs[name].Fields.List {
			var text bytes.Buffer
			if err := format.Node(&text, set, field.Type); err != nil {
				t.Fatal(err)
			}
			for _, name := range field.Names {
				actual[name.Name] = text.String()
			}
		}
	}
	if len(models) != 1 || len(models[0].Fields) != len(entity.Fields)+3 {
		t.Fatal("model field coverage differs")
	}
	for _, field := range models[0].Fields {
		want := goTypes[field.Type]
		if field.Pointer {
			want = "*" + want
		}
		if got := actual[field.Selection]; got != want {
			t.Errorf("%s: declaration %s, emitted %s", field.Name, want, got)
		}
		if field.Name == "deleted_at" || field.Name == "string_required_ref" {
			t.Fatal("internal state entered the model contract")
		}
	}
}
