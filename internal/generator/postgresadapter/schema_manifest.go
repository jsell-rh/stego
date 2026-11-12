package postgresadapter

import (
	"fmt"
	"go/format"
	"path"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
)

// The schema manifest of the runtime data path (ST-SUP-RC01-OC12): what the generated commands and queries need of
// the tables of the declaration, which the service runtime checks against the catalog before the service serves
// (ST-SUP-RP14-LC06). It comes from tableModels, the model of the migration 001_initial, and from the row-level
// security of the tenancy component, so a database that the migrations made holds it.

// schemaManifestFile is the Go file that holds the manifest, and schemaManifestVariable its variable.
const (
	schemaManifestFile     = "schema_manifest.go"
	schemaManifestVariable = "SchemaManifest"
	manifestImport         = "github.com/jsell-rh/stego/registry/runtime/manifest"
)

// TenantPolicy is the name of the one tenant policy that the tenancy component makes on each owned table
// (ST-SUP-RC27-OC03); the manifest names it.
const TenantPolicy = "stego_tenant"

// keywords are the PostgreSQL 17 keywords that are not unreserved, from pg_get_keywords(): pg_get_expr quotes a
// column name that is one of them, and no other lower-case name of the tenant key's grammar.
var keywords = map[string]bool{}

func init() {
	for _, word := range strings.Fields(`all analyse analyze and any array as asc asymmetric authorization between bigint
binary bit boolean both case cast char character check coalesce collate collation column concurrently constraint
create cross current_catalog current_date current_role current_schema current_time current_timestamp current_user
dec decimal default deferrable desc distinct do else end except exists extract false fetch float for foreign freeze
from full grant greatest group grouping having ilike in initially inner inout int integer intersect interval into
is isnull join json json_array json_arrayagg json_exists json_object json_objectagg json_query json_scalar
json_serialize json_table json_value lateral leading least left like limit localtime localtimestamp merge_action
national natural nchar none normalize not notnull null nullif numeric offset on only or order out outer overlaps
overlay placing position precision primary real references returning right row select session_user setof similar
smallint some substring symmetric system_user table tablesample then time timestamp to trailing treat trim true
union unique user using values varchar variadic verbose when where window with xmlattributes xmlconcat xmlelement
xmlexists xmlforest xmlnamespaces xmlparse xmlpi xmlroot xmlserialize xmltable`) {
		keywords[word] = true
	}
}

// deparsed gives a column name of the tenant key's grammar as pg_get_expr writes it: quoted only when it is a
// keyword that is not unreserved.
func deparsed(column string) string {
	if keywords[column] {
		return sqlIdentifier(column)
	}
	return column
}

// tenantPolicyExpression is the USING and WITH CHECK expression of the tenant policy on the tenant column, as
// pg_get_expr gives it with the search path pg_catalog.
func tenantPolicyExpression(key string) string {
	return "(" + deparsed(key) + " = current_setting('app.tenant'::text, true))"
}

// manifestSource gives the Go source of the manifest of the tables: for each table its columns with their types
// and NOT NULL, its primary key, check and foreign key constraints, its primary key and unique indexes, and on an
// owned table of a pooled service the row-level security and the tenant policy. The search indexes are left out:
// a list that has no search index is slower, not wrong.
func manifestSource(models []tableModel, key string) string {
	var b strings.Builder
	b.WriteString("manifest.Manifest{Tables: []manifest.Table{\n")
	for _, table := range models {
		fmt.Fprintf(&b, "{Schema: %q, Name: %q,\n", "public", table.name)
		b.WriteString("Columns: []manifest.Column{\n")
		for _, column := range table.columns {
			fmt.Fprintf(&b, "{Name: %q, Type: %q, NotNull: %t},\n", column.name, column.typ, column.notNull)
		}
		b.WriteString("},\nConstraints: []manifest.Constraint{\n")
		fmt.Fprintf(&b, "{Name: %q, Kind: manifest.KindPrimaryKey},\n", primaryName(table.name))
		for _, check := range table.checks {
			fmt.Fprintf(&b, "{Name: %q, Kind: manifest.KindCheck},\n", check.name)
		}
		for _, fk := range table.foreign {
			fmt.Fprintf(&b, "{Name: %q, Kind: manifest.KindForeignKey, References: %q},\n", fk.name, "public."+fk.table)
		}
		b.WriteString("},\nIndexes: []manifest.Index{\n")
		fmt.Fprintf(&b, "{Name: %q, Columns: %#v},\n", primaryName(table.name), append(append([]string{}, table.lead...), keyColumn))
		for _, columns := range table.unique {
			fmt.Fprintf(&b, "{Name: %q, Columns: %#v},\n", uniqueName(table.name, columns), append(append([]string{}, table.lead...), columns...))
		}
		b.WriteString("},\n")
		if table.owned {
			expression := tenantPolicyExpression(key)
			fmt.Fprintf(&b, "RowSecurity: true,\nPolicies: []manifest.Policy{{Name: %q, Command: \"*\", Using: %q, WithCheck: %q}},\n",
				TenantPolicy, expression, expression)
		}
		b.WriteString("},\n")
	}
	b.WriteString("}}")
	return b.String()
}

// generateSchemaManifest gives schema_manifest.go of the runtime data path: the variable SchemaManifest, which the
// service runtime takes in its parts and checks at startup (ST-SUP-RC01-OC12).
func generateSchemaManifest(ctx gen.Context) (gen.File, error) {
	models, err := tableModels(ctx)
	if err != nil {
		return gen.File{}, err
	}
	source := fmt.Sprintf(`package %s

import %q

// %s is what the commands and queries of the service need of the tables of the declaration: the columns with
// their types, the constraints, the primary key and unique indexes and, on an owned table, the row-level security
// with its tenant policy. The service runtime reads the catalog as the runtime role before the service serves, and
// refuses to start on a database that does not hold it (ST-SUP-RC01-OC12).
var %s = %s
`, path.Base(ctx.OutputNamespace), manifestImport, schemaManifestVariable, schemaManifestVariable, manifestSource(models, tenantColumn(ctx)))
	formatted, err := format.Source([]byte(source))
	if err != nil {
		return gen.File{}, fmt.Errorf("formatting %s: %w", schemaManifestFile, err)
	}
	return gen.File{Path: path.Join(ctx.OutputNamespace, schemaManifestFile), Content: formatted}, nil
}
