package postgresadapter

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"fmt"
	"go/format"
	"path"
	"text/template"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

//go:embed versions.go.tmpl
var versionSource string

func hasVersioned(entities []types.Entity) bool {
	for _, entity := range entities {
		if entity.Versioned {
			return true
		}
	}
	return false
}

const revisionBody = `
BEGIN
 IF TG_OP = 'DELETE' THEN
  RAISE EXCEPTION 'versioned resource history cannot be removed' USING ERRCODE = '23514';
 END IF;
 IF TG_OP = 'INSERT' THEN
  NEW.stego_revision := 1;
 ELSE
  IF NEW.id COLLATE "C" IS DISTINCT FROM OLD.id COLLATE "C" THEN
   RAISE EXCEPTION 'resource identity is immutable' USING ERRCODE = '23514';
  END IF;
  IF OLD.deleted_at IS NOT NULL AND NEW.deleted_at IS DISTINCT FROM OLD.deleted_at THEN
   RAISE EXCEPTION 'resource deletion cannot be reversed' USING ERRCODE = '23514';
  END IF;
  NEW.stego_revision := OLD.stego_revision + 1;
 END IF;
 RETURN NEW;
END;
`

func generateVersions(ctx gen.Context) ([]gen.File, error) {
	type entity struct{ Name, Table, Function, Columns string }
	data := struct {
		Package, StorageImport, Migration, Body string
		Entities                                []entity
		Statements                              []string
	}{Package: path.Base(ctx.OutputNamespace), StorageImport: ctx.StorageContract, Body: revisionBody}
	var ddl bytes.Buffer
	add := func(statement string) {
		data.Statements = append(data.Statements, statement)
		ddl.WriteString(statement)
	}
	for _, e := range ctx.Entities {
		if !e.Versioned {
			continue
		}
		table := tableName(e.Name)
		hash := sha256.Sum256([]byte(table))
		function := fmt.Sprintf("stego_revision_%x", hash[:12])
		data.Entities = append(data.Entities, entity{e.Name, table, function, quoteStringSlice(writeColumns(e))})
		add(fmt.Sprintf("ALTER TABLE %q ADD COLUMN IF NOT EXISTS stego_revision bigint NOT NULL DEFAULT 1;\n", table))
		add(fmt.Sprintf("CREATE OR REPLACE FUNCTION %q() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog AS $stego$%s$stego$;\n", function, revisionBody))
		add(fmt.Sprintf("DROP TRIGGER IF EXISTS stego_resource_revision ON %q;\n", table))
		add(fmt.Sprintf("CREATE TRIGGER stego_resource_revision BEFORE INSERT OR UPDATE OR DELETE ON %q FOR EACH ROW EXECUTE FUNCTION %q();\n", table, function))
	}
	data.Migration = ddl.String()
	tmpl, err := template.New("versions").Parse(versionSource)
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	if err := tmpl.Execute(&output, data); err != nil {
		return nil, err
	}
	code, err := format.Source(output.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format resource versions: %w", err)
	}
	return []gen.File{
		{Path: path.Join(ctx.OutputNamespace, "versions.go"), Content: code},
		{Path: path.Join(ctx.OutputNamespace, "migrations/000002_resource_versions.sql"), Content: []byte("BEGIN;\n" + data.Migration + "COMMIT;\n")},
	}, nil
}
