package postgresadapter

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/format"
	"path"
	"regexp"
	"text/template"

	"github.com/jsell-rh/stego/internal/gen"
)

var schemaGenerationPattern = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,63}$`)

//go:embed schema_generation.go.tmpl
var schemaGenerationTemplate string

func schemaGeneration(ctx gen.Context) (string, error) {
	raw, present := ctx.ComponentConfig["schema_generation"]
	if !present {
		return "", nil
	}
	value, ok := raw.(string)
	if !ok || !schemaGenerationPattern.MatchString(value) {
		return "", fmt.Errorf("schema_generation must be a lowercase name of 1 through 64 characters")
	}
	if len(ctx.Entities) == 0 {
		return "", fmt.Errorf("schema_generation requires storage entities")
	}
	return value, nil
}

func generateSchemaGeneration(ctx gen.Context) (gen.File, error) {
	generation, err := schemaGeneration(ctx)
	if err != nil {
		return gen.File{}, err
	}
	declaration, err := json.Marshal(struct {
		Entities    any
		Collections any
	}{ctx.Entities, ctx.Collections})
	if err != nil {
		return gen.File{}, err
	}
	hash := sha256.Sum256(declaration)
	data := struct{ Package, Generation, Definition string }{path.Base(ctx.OutputNamespace), generation, hex.EncodeToString(hash[:])}
	var output bytes.Buffer
	t, err := template.New("schema-generation").Parse(schemaGenerationTemplate)
	if err != nil {
		return gen.File{}, err
	}
	if err = t.Execute(&output, data); err != nil {
		return gen.File{}, err
	}
	code, err := format.Source(output.Bytes())
	if err != nil {
		return gen.File{}, err
	}
	return gen.File{Path: path.Join(ctx.OutputNamespace, "schema_generation.go"), Content: code}, nil
}
