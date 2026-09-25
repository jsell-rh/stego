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

// writerLeaseMode resolves the writer_lease component setting. The default
// keeps the single-writer lease. Applications that write from several live
// processes, such as a runtime plus independent tooling, set writer_lease to
// off; epoch and identity rollback detection still apply. The setting has no
// effect without schema_generation.
func writerLeaseMode(ctx gen.Context) (string, error) {
	raw, present := ctx.ComponentConfig["writer_lease"]
	if !present || raw == nil {
		return "single", nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("writer_lease must be single or off")
	}
	if value != "single" && value != "off" {
		return "", fmt.Errorf("writer_lease must be single or off")
	}
	if _, present := ctx.ComponentConfig["schema_generation"]; !present {
		return "", fmt.Errorf("writer_lease requires schema_generation")
	}
	return value, nil
}

// LeaseEnabled reports whether the given postgres-adapter component
// configuration declares a single-writer lease. The empty configuration means
// no postgres-adapter peer. Other packages use it to gate fence wiring on the
// peer's lease mode.
func LeaseEnabled(config map[string]any) bool {
	if config["schema_generation"] == nil {
		return false
	}
	value, _ := config["writer_lease"].(string)
	return value != "off"
}

func generateSchemaGeneration(ctx gen.Context) (gen.File, error) {
	generation, err := schemaGeneration(ctx)
	if err != nil {
		return gen.File{}, err
	}
	lease, err := writerLeaseMode(ctx)
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
	data := struct{ Package, Generation, Definition, Lease string }{path.Base(ctx.OutputNamespace), generation, hex.EncodeToString(hash[:]), lease}
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
