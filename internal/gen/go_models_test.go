package gen

import (
	"reflect"
	"testing"

	"github.com/jsell-rh/stego/internal/types"
)

func TestGoModelDeclarations(t *testing.T) {
	valid := []GoModel{{Name: "Parcel", GoType: "Parcel", Fields: []GoModelField{{Name: "id", Selection: "ID", Type: types.FieldTypeString}}}}
	if err := ValidateGoModels(valid); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func([]GoModel) []GoModel{
		"duplicate model":   func(m []GoModel) []GoModel { return append(m, m[0]) },
		"duplicate Go type": func(m []GoModel) []GoModel { return append(m, GoModel{Name: "Other", GoType: "Parcel"}) },
		"invalid Go type":   func(m []GoModel) []GoModel { m[0].GoType = "func"; return m },
		"invalid name":      func(m []GoModel) []GoModel { m[0].Name = "a.b"; return m },
		"duplicate field":   func(m []GoModel) []GoModel { m[0].Fields = append(m[0].Fields, m[0].Fields[0]); return m },
		"duplicate selection": func(m []GoModel) []GoModel {
			m[0].Fields = append(m[0].Fields, GoModelField{Name: "other", Selection: "ID", Type: types.FieldTypeString})
			return m
		},
		"expression":        func(m []GoModel) []GoModel { m[0].Fields[0].Selection = "ID()"; return m },
		"pointer traversal": func(m []GoModel) []GoModel { m[0].Fields[0].Selection = "Meta.ID"; return m },
		"unknown type":      func(m []GoModel) []GoModel { m[0].Fields[0].Type = "complex"; return m },
		"byte pointer": func(m []GoModel) []GoModel {
			m[0].Fields[0].Type = types.FieldTypeBytes
			m[0].Fields[0].Pointer = true
			return m
		},
		"JSON pointer": func(m []GoModel) []GoModel {
			m[0].Fields[0].Type = types.FieldTypeJsonb
			m[0].Fields[0].Pointer = true
			return m
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateGoModels(change(CloneGoModels(valid))); err == nil {
				t.Fatal("invalid model accepted")
			}
		})
	}
}

func TestGoModelCopiesOwnAllFields(t *testing.T) {
	source := map[string]GoModelSource{"provider": {ImportPath: "example.com/store", Models: []GoModel{{Name: "Parcel", Fields: []GoModelField{{Name: "id"}}}, {Name: "Empty", Fields: []GoModelField{}}, {Name: "Nil"}}}}
	copy := CloneGoModelSources(source)
	if !reflect.DeepEqual(source, copy) {
		t.Fatal("copy changes the declaration")
	}
	copy["provider"].Models[0].Fields[0].Name = "changed"
	copy["provider"].Models[1].Name = "Changed"
	delete(copy, "provider")
	if source["provider"].Models[0].Fields[0].Name != "id" || source["provider"].Models[1].Name != "Empty" {
		t.Fatal("copy shares mutable model state")
	}
}
