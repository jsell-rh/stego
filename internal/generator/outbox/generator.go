// Package outbox generates a PostgreSQL queue for durable notifications.
package outbox

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/format"
	"path"
	"strings"
	"text/template"

	"github.com/jsell-rh/stego/internal/gen"
)

//go:embed queue.go.tmpl
var queueSource string

//go:embed worker.go.tmpl
var workerSource string

//go:embed source.go.tmpl
var eventSource string

//go:embed migrations/000001_outbox.sql
var migration []byte

type Generator struct{}

// MinimumGoVersion covers the pinned pgx dependency.
func (*Generator) MinimumGoVersion() string { return "1.25.0" }

// ValidateContext checks the output namespace before queue code is rendered.
func (*Generator) ValidateContext(ctx gen.Context) error {
	ns := ctx.OutputNamespace
	if err := gen.ValidateGoPackageNamespace(ns); err != nil {
		return err
	}

	return nil
}

func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := g.ValidateContext(ctx); err != nil {
		return nil, nil, err
	}
	ns := ctx.OutputNamespace
	files := []gen.File{
		{Path: path.Join(ns, "migrations/000001_outbox.sql"), Content: migration},
	}
	tmpl, err := template.New("source").Parse(eventSource + gen.UnicodeEscapeValidation)
	if err != nil {
		return nil, nil, err
	}
	var sourceBuf bytes.Buffer
	if err := tmpl.Execute(&sourceBuf, struct{ Package, EventsImport string }{path.Base(ns), ctx.EventsContract}); err != nil {
		return nil, nil, err
	}
	sourceCode, err := format.Source(sourceBuf.Bytes())
	if err != nil {
		return nil, nil, fmt.Errorf("formatting event source: %w", err)
	}
	files = append(files, gen.File{Path: path.Join(ns, "source.go"), Content: sourceCode})
	for _, template := range []struct{ name, source string }{{"queue.go", queueSource}, {"worker.go", workerSource}} {
		sourceText := strings.Replace(template.source, "package outbox", "package "+path.Base(ns), 1)
		if ctx.StorageContract != "" && template.name == "queue.go" {
			start := strings.Index(sourceText, "type Message struct {")
			if start < 0 {
				return nil, nil, fmt.Errorf("outbox template has no message declaration")
			}
			relativeEnd := strings.Index(sourceText[start:], "\n}")
			if relativeEnd < 0 {
				return nil, nil, fmt.Errorf("outbox template has an incomplete message declaration")
			}
			end := relativeEnd + start + 2
			sourceText = sourceText[:start] + "type Message = stegostorage.Notification" + sourceText[end:]
			sourceText = strings.Replace(sourceText, "import (", fmt.Sprintf("import (\n stegostorage %q", ctx.StorageContract), 1)
		}
		source, err := format.Source([]byte(sourceText))
		if err != nil {
			return nil, nil, fmt.Errorf("formatting outbox %s: %w", template.name, err)
		}
		files = append(files, gen.File{Path: path.Join(ns, template.name), Content: source})
	}
	if err := gen.ValidateNamespace(ns, files); err != nil {
		return nil, nil, err
	}
	wiring := &gen.Wiring{GoModRequires: map[string]string{"github.com/google/uuid": "v1.6.0", "github.com/jackc/pgx/v5": "v5.11.0"}}
	if ctx.StorageContract != "" {
		wiring.Contracts = []gen.Contract{gen.StorageV1}
	}
	if ctx.EventsContract != "" {
		wiring.Contracts = append(wiring.Contracts, gen.EventsV1)
	}
	return files, wiring, nil
}
