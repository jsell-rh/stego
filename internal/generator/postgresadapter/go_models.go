package postgresadapter

import (
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

// GoModels describes the same field names and pointer forms used by models.go.
// Observation selection and authorization remain the caller's responsibility.
func (g *Generator) GoModels(ctx gen.Context) ([]gen.GoModel, error) {
	if err := g.ValidateContext(ctx); err != nil {
		return nil, err
	}
	result := make([]gen.GoModel, 0, len(ctx.Entities))
	for _, entity := range ctx.Entities {
		model := gen.GoModel{Name: entity.Name, GoType: entity.Name, Fields: []gen.GoModelField{
			{Name: "id", Selection: "ID", Type: types.FieldTypeString},
			{Name: "created_time", Selection: "CreatedTime", Type: types.FieldTypeTimestamp},
			{Name: "updated_time", Selection: "UpdatedTime", Type: types.FieldTypeTimestamp},
		}}
		for _, field := range entity.Fields {
			model.Fields = append(model.Fields, gen.GoModelField{
				Name: field.Name, Selection: toPascalCase(field.Name), Type: field.Type,
				Pointer: strings.HasPrefix(fieldTypeToGo(field), "*"),
			})
		}
		result = append(result, model)
	}
	return result, gen.ValidateGoModels(result)
}
