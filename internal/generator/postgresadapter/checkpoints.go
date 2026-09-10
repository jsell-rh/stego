package postgresadapter

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/format"
	"path"
	"text/template"

	"github.com/jsell-rh/stego/internal/gen"
)

//go:embed checkpoints.go.tmpl
var checkpointSource string

const checkpointMigration = `CREATE TABLE IF NOT EXISTS stego_scan_checkpoints (
 entity text COLLATE "C" NOT NULL,
 resource_id text COLLATE "C" NOT NULL,
 scope text COLLATE "C" NOT NULL,
 after_cursor text COLLATE "C" NOT NULL,
 version bigint NOT NULL,
 PRIMARY KEY(entity, resource_id, scope),
 CHECK(octet_length(entity) BETWEEN 1 AND 256),
 CHECK(octet_length(resource_id) BETWEEN 1 AND 256),
 CHECK(octet_length(scope) BETWEEN 1 AND 128),
 CHECK(octet_length(after_cursor) <= 1024),
 CHECK(version > 0)
);`

func generateCheckpoints(ctx gen.Context) ([]gen.File, error) {
	data := struct {
		Package, StorageImport, Migration string
		Entities                          []string
	}{Package: path.Base(ctx.OutputNamespace), StorageImport: ctx.StorageContract, Migration: checkpointMigration}
	for _, e := range ctx.Entities {
		data.Entities = append(data.Entities, e.Name)
	}
	tmpl, err := template.New("checkpoints").Parse(checkpointSource)
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	if err := tmpl.Execute(&output, data); err != nil {
		return nil, err
	}
	code, err := format.Source(output.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format checkpoints: %w", err)
	}
	return []gen.File{{Path: path.Join(ctx.OutputNamespace, "checkpoints.go"), Content: code}, {Path: path.Join(ctx.OutputNamespace, "migrations/000006_scan_checkpoints.sql"), Content: []byte("BEGIN;\n" + checkpointMigration + "\nCOMMIT;\n")}}, nil
}
