package gen

import (
	"fmt"
	"go/token"
	"regexp"

	"github.com/jsell-rh/stego/internal/types"
)

// GoModelProvider declares generated source types for checked field mappings.
// It must not render files or change its context. The declaration must remain
// the same through generation. It does not select observations or grant access.
type GoModelProvider interface {
	GoModels(Context) ([]GoModel, error)
}

// GoModel names a generated struct and its available scalar fields. Internal
// storage state and relationships need not be exposed in this contract.
type GoModel struct {
	Name   string
	GoType string
	Fields []GoModelField
}

// GoModelField identifies one field without an arbitrary Go expression.
// Pointer is the actual Go representation, not the service's optional flag.
// Bytes and JSON values use slices; nil is their absent representation.
type GoModelField struct {
	Name      string
	Selection string
	Type      types.FieldType
	Pointer   bool
}

// GoModelSource binds declarations to the namespace owned by their provider.
// The compiler supplies ImportPath; a provider cannot select another package.
type GoModelSource struct {
	ImportPath string
	Models     []GoModel
}

var modelLogicalName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidateGoModels checks names, scalar representations, and ambiguous fields.
// A consumer must also require exported types and selectors before importing
// them. This check permits a provider to describe its private Go types.
func ValidateGoModels(models []GoModel) error {
	names, goTypes := map[string]bool{}, map[string]bool{}
	for _, model := range models {
		if !modelLogicalName.MatchString(model.Name) || names[model.Name] || !token.IsIdentifier(model.GoType) || model.GoType == "_" || goTypes[model.GoType] {
			return fmt.Errorf("invalid or duplicate Go model declaration")
		}
		names[model.Name], goTypes[model.GoType] = true, true
		fields, selections := map[string]bool{}, map[string]bool{}
		for _, field := range model.Fields {
			if !modelLogicalName.MatchString(field.Name) || fields[field.Name] || selections[field.Selection] || !types.ValidFieldTypes[field.Type] {
				return fmt.Errorf("Go model %q has an invalid or duplicate field", model.Name)
			}
			if !token.IsIdentifier(field.Selection) || field.Selection == "_" {
				return fmt.Errorf("Go model %q has an invalid field selection", model.Name)
			}
			if field.Pointer && (field.Type == types.FieldTypeBytes || field.Type == types.FieldTypeJsonb) {
				return fmt.Errorf("Go model %q byte fields must use slice presence", model.Name)
			}
			fields[field.Name], selections[field.Selection] = true, true
		}
	}
	return nil
}

// CloneGoModels gives each compiler phase and consumer its own field slices.
func CloneGoModels(models []GoModel) []GoModel {
	if models == nil {
		return nil
	}
	result := make([]GoModel, len(models))
	for i, model := range models {
		result[i] = model
		if model.Fields != nil {
			result[i].Fields = append(make([]GoModelField, 0, len(model.Fields)), model.Fields...)
		}
	}
	return result
}

// CloneGoModelSources prevents a consumer from changing another consumer's
// mapping inputs or the compiler's saved declaration.
func CloneGoModelSources(sources map[string]GoModelSource) map[string]GoModelSource {
	result := make(map[string]GoModelSource, len(sources))
	for name, source := range sources {
		result[name] = GoModelSource{ImportPath: source.ImportPath, Models: CloneGoModels(source.Models)}
	}
	return result
}
