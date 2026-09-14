package postgresadapter

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/format"
	"path"
	"slices"
	"text/template"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

//go:embed cleanup_summary.go.tmpl
var cleanupSummarySource string

func generateCleanupSummary(ctx gen.Context) (gen.File, error) {
	type entity struct {
		Name, Table                         string
		Owners, Targets, Scopes, References []string
		TargetFields                        map[string]string
	}
	data := struct {
		Package, StorageImport string
		Entities               []entity
	}{Package: path.Base(ctx.OutputNamespace), StorageImport: ctx.StorageContract}
	for _, e := range ctx.Entities {
		if len(e.CleanupOwners) == 0 {
			continue
		}
		item := entity{Name: e.Name, Table: tableName(e.Name), Owners: slices.Clone(e.CleanupOwners), Scopes: []string{"id"}, TargetFields: e.CleanupTargets}
		for owner := range e.CleanupTargets {
			item.Targets = append(item.Targets, owner)
		}
		slices.Sort(item.Owners)
		slices.Sort(item.Targets)
		for _, field := range e.Fields {
			if field.Type == types.FieldTypeRef {
				item.References = append(item.References, field.Name)
			}
			if !e.IsObservationField(field.Name) && (field.Type == types.FieldTypeString || field.Type == types.FieldTypeEnum || field.Type == types.FieldTypeRef) {
				item.Scopes = append(item.Scopes, field.Name)
			}
		}
		slices.Sort(item.Scopes)
		slices.Sort(item.References)
		data.Entities = append(data.Entities, item)
	}
	tmpl, err := template.New("cleanup-summary").Parse(cleanupSummarySource)
	if err != nil {
		return gen.File{}, err
	}
	var output bytes.Buffer
	if err := tmpl.Execute(&output, data); err != nil {
		return gen.File{}, err
	}
	code, err := format.Source(output.Bytes())
	if err != nil {
		return gen.File{}, fmt.Errorf("format cleanup summary: %w", err)
	}
	return gen.File{Path: path.Join(ctx.OutputNamespace, "cleanup_summary.go"), Content: code}, nil
}
