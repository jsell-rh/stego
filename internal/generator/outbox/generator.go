// Package outbox generates a PostgreSQL queue for durable notifications.
package outbox

import (
	_ "embed"
	"fmt"
	"go/format"
	"path"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
)

//go:embed queue.go.tmpl
var queueSource string

//go:embed worker.go.tmpl
var workerSource string

//go:embed migrations/000001_outbox.sql
var migration []byte

type Generator struct{}

// Generate emits queue code and an explicit migration. It does not start a
// worker or apply the migration. The application must supply a transaction.
func (*Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	ns := ctx.OutputNamespace
	if err := gen.ValidatePath(ns); err != nil {
		return nil, nil, err
	}
	files := []gen.File{
		{Path: path.Join(ns, "migrations/000001_outbox.sql"), Content: migration},
	}
	for _, template := range []struct{ name, source string }{{"queue.go", queueSource}, {"worker.go", workerSource}} {
		source, err := format.Source([]byte(strings.Replace(template.source, "package outbox", "package "+path.Base(ns), 1)))
		if err != nil {
			return nil, nil, fmt.Errorf("formatting outbox %s: %w", template.name, err)
		}
		files = append(files, gen.File{Path: path.Join(ns, template.name), Content: source})
	}
	if err := gen.ValidateNamespace(ns, files); err != nil {
		return nil, nil, err
	}
	return files, &gen.Wiring{GoModRequires: map[string]string{"github.com/google/uuid": "v1.6.0"}}, nil
}
