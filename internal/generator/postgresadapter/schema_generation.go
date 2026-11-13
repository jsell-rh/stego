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

// validateWriterLease checks the writer_lease setting. The single-writer lease fenced the writes of the GORM store,
// which is removed (ST-SUP-RC20-OC39), so the adapter emits no lease: writer_lease may be left out or set to off, as
// a declaration of the runtime data path sets it, and the value single is refused with its cause. The setting has
// no meaning without schema_generation.
func validateWriterLease(ctx gen.Context) error {
	raw, present := ctx.ComponentConfig["writer_lease"]
	if !present || raw == nil {
		return nil
	}
	value, ok := raw.(string)
	if ok && value == "single" {
		return fmt.Errorf("postgres-adapter: writer_lease single is refused: the single-writer lease fenced the writes of the GORM store, and %s; remove writer_lease or set it to off", removedStore)
	}
	if !ok || value != "off" {
		return fmt.Errorf("writer_lease must be off or left out")
	}
	if _, present := ctx.ComponentConfig["schema_generation"]; !present {
		return fmt.Errorf("writer_lease requires schema_generation")
	}
	return nil
}

// LeaseEnabled reports whether the given postgres-adapter component configuration declares a single-writer lease:
// schema_generation with writer_lease single. The adapter refuses that configuration (validateWriterLease), so it
// is false for every service that compiles. The kafka-producer generator still reads it to gate its fence wiring.
func LeaseEnabled(config map[string]any) bool {
	if config["schema_generation"] == nil {
		return false
	}
	value, _ := config["writer_lease"].(string)
	return value == "single"
}

func generateSchemaGeneration(ctx gen.Context) (gen.File, error) {
	generation, err := schemaGeneration(ctx)
	if err != nil {
		return gen.File{}, err
	}
	// The digest is of the baseline, the declaration with every schema step undone, so that each release after the
	// first one holds the marker that the first release wrote, and a step changes the tables only by its migration.
	baseline, err := ctx.AtSchemaStep(0)
	if err != nil {
		return gen.File{}, err
	}
	declaration, err := json.Marshal(struct {
		Entities    any
		Collections any
	}{baseline.Entities, baseline.Collections})
	if err != nil {
		return gen.File{}, err
	}
	hash := sha256.Sum256(declaration)
	// The marker holds the generation name and the digest of the baseline: the bootstrap, the marker check and
	// the ledger read (ST-SUP-RC01-OC11, ST-SUP-RC01-OC41).
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
