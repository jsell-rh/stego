package postgresadapter

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

// TestGoModelDeclarationMatchesGeneratedStorage checks that the adapter declares no Go model, because its generated
// storage holds no Go model: the storage is the SQL tables of the runtime data path, and the query component
// declares the value types that a query loads (ST-SUP-RC01-OC41). A model declaration of the adapter would name a
// Go type that no generated file declares. It reads the generated Go syntax, not the helper that produced it.
func TestGoModelDeclarationMatchesGeneratedStorage(t *testing.T) {
	if _, provides := any(new(Generator)).(gen.GoModelProvider); provides {
		t.Fatal("the adapter declares Go models, but its generated storage holds no Go model")
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
	ctx := basicContext()
	ctx.OutputNamespace, ctx.Entities = "store", []types.Entity{entity}
	files, _, err := new(Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if filepath.Ext(file.Path) != ".go" {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file.Path, file.Bytes(), 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range parsed.Decls {
			group, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range group.Specs {
				if typeSpec, ok := spec.(*ast.TypeSpec); ok && (typeSpec.Name.Name == entity.Name || typeSpec.Name.Name == "Meta") {
					t.Errorf("%s declares the model type %s", file.Path, typeSpec.Name.Name)
				}
			}
		}
	}
}
