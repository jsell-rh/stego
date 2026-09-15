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

//go:embed resource_state.go.tmpl
var resourceStateSource string

const resourceStateGuard = `
BEGIN
 IF TG_OP = 'DELETE' THEN
  RAISE EXCEPTION 'resource state history cannot be removed' USING ERRCODE = '23514';
 END IF;
 IF NEW.entity IS DISTINCT FROM OLD.entity OR NEW.resource_id IS DISTINCT FROM OLD.resource_id
    OR NEW.scope IS DISTINCT FROM OLD.scope OR OLD.version = 9223372036854775807
    OR NEW.version IS DISTINCT FROM OLD.version + 1 THEN
  RAISE EXCEPTION 'resource state identity or version differs' USING ERRCODE = '23514';
 END IF;
 RETURN NEW;
END;
`

const resourceStateTable = `CREATE TABLE IF NOT EXISTS stego_resource_state (
 entity text COLLATE "C" NOT NULL,
 resource_id text COLLATE "C" NOT NULL,
 scope text COLLATE "C" NOT NULL,
 data bytea NOT NULL,
 version bigint NOT NULL,
 PRIMARY KEY(entity,resource_id,scope),
 CHECK(octet_length(entity) BETWEEN 1 AND 256),
 CHECK(octet_length(resource_id) BETWEEN 1 AND 256),
 CHECK(octet_length(scope) BETWEEN 1 AND 128),
 CHECK(octet_length(data) <= 65536),
 CHECK(version > 0)
);`

const resourceStateFunction = `CREATE OR REPLACE FUNCTION stego_guard_resource_state() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $guard$` + resourceStateGuard + `$guard$;`
const resourceStateDropTrigger = `DROP TRIGGER IF EXISTS stego_resource_state_guard ON stego_resource_state;`
const resourceStateTrigger = `CREATE TRIGGER stego_resource_state_guard BEFORE UPDATE OR DELETE ON stego_resource_state FOR EACH ROW EXECUTE FUNCTION stego_guard_resource_state();`

const resourceStateMigration = resourceStateTable + "\n" + resourceStateFunction + "\n" + resourceStateDropTrigger + "\n" + resourceStateTrigger

func generateResourceStates(ctx gen.Context) ([]gen.File, error) {
	data := struct {
		Package, StorageImport, Migration, Guard string
		Entities, Statements                     []string
	}{Package: path.Base(ctx.OutputNamespace), StorageImport: ctx.StorageContract, Migration: resourceStateMigration, Guard: resourceStateGuard, Statements: []string{resourceStateTable, resourceStateFunction, resourceStateDropTrigger, resourceStateTrigger}}
	for _, entity := range ctx.Entities {
		data.Entities = append(data.Entities, entity.Name)
	}
	tmpl, err := template.New("resource-state").Parse(resourceStateSource)
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	if err = tmpl.Execute(&output, data); err != nil {
		return nil, err
	}
	code, err := format.Source(output.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format resource state: %w", err)
	}
	return []gen.File{
		{Path: path.Join(ctx.OutputNamespace, "resource_state.go"), Content: code},
		{Path: path.Join(ctx.OutputNamespace, "migrations/000009_resource_state.sql"), Content: []byte("BEGIN;\n" + resourceStateMigration + "\nCOMMIT;\n")},
	}, nil
}
