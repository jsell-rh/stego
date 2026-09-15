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

//go:embed effects.go.tmpl
var effectBindingSource string

const effectBindingGuard = `
BEGIN
 IF TG_OP = 'DELETE' THEN
  RAISE EXCEPTION 'effect binding history cannot be removed' USING ERRCODE = '23514';
 END IF;
 IF NEW.entity IS DISTINCT FROM OLD.entity OR NEW.resource_id IS DISTINCT FROM OLD.resource_id
    OR NEW.scope IS DISTINCT FROM OLD.scope OR NEW.digest IS DISTINCT FROM OLD.digest
    OR (OLD.closed AND NOT NEW.closed) THEN
  RAISE EXCEPTION 'effect binding identity and closure are immutable' USING ERRCODE = '23514';
 END IF;
 RETURN NEW;
END;
`

const effectBindingTable = `CREATE TABLE IF NOT EXISTS stego_effect_bindings (
 entity text COLLATE "C" NOT NULL,
 resource_id text COLLATE "C" NOT NULL,
 scope text COLLATE "C" NOT NULL,
 digest text COLLATE "C" NOT NULL,
 closed boolean NOT NULL,
 PRIMARY KEY(entity,resource_id,scope),
 CHECK(octet_length(entity) BETWEEN 1 AND 256),
 CHECK(octet_length(resource_id) BETWEEN 1 AND 256),
 CHECK(octet_length(scope) BETWEEN 1 AND 128),
 CHECK(digest ~ '^[0-9a-f]{64}$' OR (closed AND digest=''))
);`

const effectBindingFunction = `CREATE OR REPLACE FUNCTION stego_guard_effect_binding() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $guard$` + effectBindingGuard + `$guard$;`
const effectBindingDropTrigger = `DROP TRIGGER IF EXISTS stego_effect_binding_guard ON stego_effect_bindings;`
const effectBindingTrigger = `CREATE TRIGGER stego_effect_binding_guard BEFORE UPDATE OR DELETE ON stego_effect_bindings FOR EACH ROW EXECUTE FUNCTION stego_guard_effect_binding();`

const effectBindingMigration = effectBindingTable + "\n" + effectBindingFunction + "\n" + effectBindingDropTrigger + "\n" + effectBindingTrigger

func generateEffectBindings(ctx gen.Context) ([]gen.File, error) {
	data := struct {
		Package, StorageImport, Migration, Guard string
		Entities, Statements                     []string
	}{Package: path.Base(ctx.OutputNamespace), StorageImport: ctx.StorageContract, Migration: effectBindingMigration, Guard: effectBindingGuard, Statements: []string{effectBindingTable, effectBindingFunction, effectBindingDropTrigger, effectBindingTrigger}}
	for _, entity := range ctx.Entities {
		data.Entities = append(data.Entities, entity.Name)
	}
	tmpl, err := template.New("effects").Parse(effectBindingSource)
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	if err = tmpl.Execute(&output, data); err != nil {
		return nil, err
	}
	code, err := format.Source(output.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format effect bindings: %w", err)
	}
	return []gen.File{
		{Path: path.Join(ctx.OutputNamespace, "effects.go"), Content: code},
		{Path: path.Join(ctx.OutputNamespace, "migrations/000008_effect_bindings.sql"), Content: []byte("BEGIN;\n" + effectBindingMigration + "\nCOMMIT;\n")},
	}, nil
}
