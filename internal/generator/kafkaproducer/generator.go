// Package kafkaproducer generates a Kafka publisher with verified TLS.
package kafkaproducer

import (
	_ "embed"
	"fmt"
	"go/format"
	"path"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
)

//go:embed publisher.go.tmpl
var publisherSource string

type Generator struct{}

// Generate emits the publisher library. Worker and storage composition is a
// separate compiler stage. This generator does not start a background worker.
func (*Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	ns := ctx.OutputNamespace
	if err := gen.ValidatePath(ns); err != nil {
		return nil, nil, err
	}
	source, err := format.Source([]byte(strings.Replace(publisherSource, "package publisher", "package "+path.Base(ns), 1)))
	if err != nil {
		return nil, nil, fmt.Errorf("formatting Kafka publisher: %w", err)
	}
	files := []gen.File{{Path: path.Join(ns, "publisher.go"), Content: source}}
	if err := gen.ValidateNamespace(ns, files); err != nil {
		return nil, nil, err
	}
	return files, &gen.Wiring{GoModRequires: map[string]string{
		"github.com/twmb/franz-go": "v1.21.6", "github.com/google/uuid": "v1.6.0",
	}}, nil
}
