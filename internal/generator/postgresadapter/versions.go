package postgresadapter

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"go/format"
	"path"
	"slices"
	"sort"
	"strings"
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
	type projection struct {
		Name          string
		Unobserved    string
		HasUnobserved bool
	}
	type observation struct {
		Name, SQL, Fields, Args string
		Projections             []projection
	}
	type entity struct {
		Name, Table, Function, Columns, Body string
		Generation                           bool
		Observations                         []observation
		CleanupOwners                        []string
	}
	data := struct {
		Package, StorageImport, Migration string
		NotFoundImport                    string
		HasGeneration                     bool
		HasObservations                   bool
		HasCleanup                        bool
		Entities                          []entity
		Statements                        []string
	}{Package: path.Base(ctx.OutputNamespace), StorageImport: ctx.StorageContract}
	if ctx.StorageContract == "" && ctx.ModuleName != "" && ctx.PeerNamespaces["rest-api"] != "" {
		data.NotFoundImport = path.Join(ctx.ModuleName, ctx.OutDirName, ctx.PeerNamespaces["rest-api"])
	}
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
		definition := entity{Name: e.Name, Table: table, Function: function, Columns: quoteStringSlice(writeColumns(e)), Body: revisionBody, Generation: len(e.GenerationFields) > 0}
		add(fmt.Sprintf("ALTER TABLE %q ADD COLUMN IF NOT EXISTS stego_revision bigint NOT NULL DEFAULT 1;\n", table))
		// The table lock and transaction keep this temporary disable private.
		add(fmt.Sprintf(`DO $disable$ BEGIN
 IF EXISTS (SELECT 1 FROM pg_catalog.pg_trigger WHERE tgrelid=%s::regclass AND tgname='stego_resource_revision') THEN
  ALTER TABLE %q DISABLE TRIGGER stego_resource_revision;
 END IF;
 END; $disable$;
`, sqlLiteral(table), table))
		if definition.Generation {
			data.HasGeneration = true
			if len(e.Observations) > 0 {
				data.HasObservations = true
			}
			add(fmt.Sprintf("ALTER TABLE %q ADD COLUMN IF NOT EXISTS stego_generation bigint NOT NULL DEFAULT 1, ADD COLUMN IF NOT EXISTS stego_observations jsonb NOT NULL DEFAULT '{}';\n", table))
			var changes []string
			for _, name := range e.GenerationFields {
				changes = append(changes, fmt.Sprintf("to_jsonb(NEW.%q) IS DISTINCT FROM to_jsonb(OLD.%q)", name, name))
			}
			changes = append(changes, "NEW.deleted_at IS DISTINCT FROM OLD.deleted_at")
			generation := " IF TG_OP = 'INSERT' THEN\n  NEW.stego_generation := 1; NEW.stego_observations := '{}'::jsonb;\n ELSE\n  NEW.stego_generation := OLD.stego_generation;\n  IF " + strings.Join(changes, " OR ") + " THEN\n   NEW.stego_generation := OLD.stego_generation + 1;\n   NEW.stego_observations := OLD.stego_observations;\n  END IF;\n END IF;\n"
			definitions := make([]types.Field, 0, len(e.Fields))
			for _, field := range e.Fields {
				if e.IsObservationField(field.Name) || slices.Contains(e.GenerationFields, field.Name) {
					definitions = append(definitions, field)
				}
			}
			contract, err := json.Marshal(struct {
				Fields       []string
				Observations map[string][]string
				Definitions  []types.Field
			}{e.GenerationFields, e.Observations, definitions})
			if err != nil {
				return nil, fmt.Errorf("encode generation contract: %w", err)
			}
			signature := sha256.Sum256(contract)
			definition.Body = strings.Replace(revisionBody, " RETURN NEW;", generation+fmt.Sprintf(" -- generation contract %x\n RETURN NEW;", signature), 1)
			var owners []string
			for owner := range e.Observations {
				owners = append(owners, owner)
			}
			sort.Strings(owners)
			for _, owner := range owners {
				var setters, args []string
				for _, name := range e.Observations[owner] {
					setters = append(setters, fmt.Sprintf("%q = ?", name))
					args = append(args, "row."+toPascalCase(name))
				}
				statement := fmt.Sprintf(`UPDATE %q SET %s, stego_observations=jsonb_set(stego_observations, ARRAY[?]::text[], to_jsonb(stego_generation)), updated_time=now() WHERE id=? AND id COLLATE "C"=? AND stego_revision=? AND deleted_at IS NULL`, table, strings.Join(setters, ", "))
				group := observation{Name: owner, SQL: statement, Fields: quoteStringSlice(e.Observations[owner]), Args: strings.Join(args, ", ")}
				for _, name := range e.Observations[owner] {
					for _, field := range e.Fields {
						if field.Name == name {
							projected := projection{Name: toPascalCase(name)}
							if field.Unobserved != nil {
								projected.Unobserved = *field.Unobserved
								projected.HasUnobserved = true
							}
							group.Projections = append(group.Projections, projected)
						}
					}
				}
				definition.Observations = append(definition.Observations, group)
			}
		}
		cleanupOwners, initial, keys, cleanupBody := cleanupContract(e)
		definition.CleanupOwners = cleanupOwners
		if len(cleanupOwners) > 0 {
			data.HasCleanup = true
			add(fmt.Sprintf("ALTER TABLE %q ADD COLUMN IF NOT EXISTS stego_cleanup jsonb NOT NULL DEFAULT '{}';\n", table))
			// Existing owners cannot disappear, including on live resources.
			add(fmt.Sprintf(`DO $owners$ BEGIN
 IF EXISTS (SELECT 1 FROM %q WHERE jsonb_typeof(stego_cleanup) IS DISTINCT FROM 'object') THEN
  RAISE EXCEPTION 'invalid stored cleanup state';
 END IF;
 IF EXISTS (SELECT 1 FROM %q WHERE stego_cleanup - %s <> '{}'::jsonb) THEN
  RAISE EXCEPTION 'cleanup owners cannot be removed from retained resources';
 END IF;
 END; $owners$;
`, table, table, keys))
			contract, err := json.Marshal(e.Fields)
			if err != nil {
				return nil, fmt.Errorf("encode cleanup field contract: %w", err)
			}
			signature := sha256.Sum256(contract)
			definition.Body = strings.Replace(definition.Body, " RETURN NEW;", cleanupBody+fmt.Sprintf(" -- cleanup fields %x\n RETURN NEW;", signature), 1)
		} else {
			add(fmt.Sprintf(`DO $owners$ BEGIN
 IF EXISTS (SELECT 1 FROM pg_catalog.pg_attribute WHERE attrelid=%s::regclass AND attname='stego_cleanup' AND NOT attisdropped) THEN
  IF EXISTS (SELECT 1 FROM %q WHERE stego_cleanup <> '{}'::jsonb) THEN
   RAISE EXCEPTION 'cleanup owners cannot be removed from retained resources';
  END IF;
 END IF;
 END; $owners$;
`, sqlLiteral(table), table))
		}
		if definition.Generation || len(cleanupOwners) > 0 {
			assignments := []string{"stego_revision=stego_revision+1"}
			if definition.Generation {
				assignments = append(assignments, "stego_generation=stego_generation+1", "stego_observations='{}'::jsonb")
			}
			if len(cleanupOwners) > 0 {
				assignments = append(assignments, "stego_cleanup="+initial)
			}
			add(fmt.Sprintf(`DO $upgrade$ BEGIN
 IF EXISTS (SELECT 1 FROM pg_catalog.pg_trigger t JOIN pg_catalog.pg_proc p ON p.oid=t.tgfoid WHERE t.tgrelid=%s::regclass AND t.tgname='stego_resource_revision' AND p.prosrc IS DISTINCT FROM %s) THEN
  UPDATE %q SET %s;
 END IF;
 END; $upgrade$;
`, sqlLiteral(table), sqlLiteral(definition.Body), table, strings.Join(assignments, ", ")))
		}
		if len(cleanupOwners) > 0 {
			add(fmt.Sprintf("UPDATE %q SET stego_cleanup=%s || stego_cleanup WHERE NOT (stego_cleanup ?& %s);\n", table, initial, keys))
		}
		data.Entities = append(data.Entities, definition)
		add(fmt.Sprintf("CREATE OR REPLACE FUNCTION %q() RETURNS trigger LANGUAGE plpgsql SET search_path = pg_catalog AS $stego$%s$stego$;\n", function, definition.Body))
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
	files := []gen.File{
		{Path: path.Join(ctx.OutputNamespace, "versions.go"), Content: code},
		{Path: path.Join(ctx.OutputNamespace, "migrations/000002_resource_versions.sql"), Content: []byte("BEGIN;\n" + data.Migration + "COMMIT;\n")},
	}
	if data.HasGeneration {
		files = append(files, gen.File{Path: path.Join(ctx.OutputNamespace, "migrations/000003_resource_generations.sql"), Content: []byte("BEGIN;\n" + data.Migration + "COMMIT;\n")})
	}
	if data.HasCleanup {
		files = append(files, gen.File{Path: path.Join(ctx.OutputNamespace, "migrations/000004_resource_cleanup.sql"), Content: []byte("BEGIN;\n" + data.Migration + "COMMIT;\n")})
	}
	return files, nil
}

func sqlLiteral(value string) string {
	return "E'" + strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), "'", "''") + "'"
}

// currentObservationTable supplies the same values for display, predicates,
// ordering, relation filters, and counts.
func currentObservationTable(entity types.Entity) string {
	if len(entity.Observations) == 0 {
		return ""
	}
	columns := []string{`"id"`, `"created_time"`, `"updated_time"`, `"deleted_at"`, `"stego_revision"`, `"stego_generation"`, `"stego_observations"`}
	if len(entity.CleanupOwners) > 0 {
		columns = append(columns, `"stego_cleanup"`)
	}
	for _, field := range entity.Fields {
		expression := fmt.Sprintf("%q", field.Name)
		for group, fields := range entity.Observations {
			if !slices.Contains(fields, field.Name) {
				continue
			}
			unknown := "NULL"
			if field.Unobserved != nil {
				unknown = sqlLiteral(*field.Unobserved)
			}
			key := sqlLiteral(group)
			expression = fmt.Sprintf("CASE WHEN stego_generation>0 AND jsonb_typeof(stego_observations -> %s)='number' AND stego_observations ->> %s = stego_generation::text THEN %q ELSE %s END AS %q", key, key, field.Name, unknown, field.Name)
		}
		columns = append(columns, expression)
	}
	table := tableName(entity.Name)
	return fmt.Sprintf("(SELECT %s FROM %q) AS %q", strings.Join(columns, ", "), table, table)
}
