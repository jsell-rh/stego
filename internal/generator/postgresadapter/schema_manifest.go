package postgresadapter

import (
	"fmt"
	"go/format"
	"math"
	"math/big"
	"path"
	"regexp"
	"strconv"
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

// StepSchema is the held schema of the sync functions of the schema steps (ST-SUP-RP15-LC02): the manifest names it,
// so the schema check of a release accepts the sync trigger of a later step, and the step that first needs it makes
// it. Each table of the manifest is the table of a declared entity, on which the schema check accepts the sync
// trigger, the foreign key and the constant default of a later step in their exact form.
const StepSchema = "stego_steps"

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

// plainName is a name that PostgreSQL writes with no quotes when it is not a keyword.
var plainName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// deparsedName gives a column name as pg_get_expr and pg_get_constraintdef write it: with no quotes when it is a
// lower-case name that is not a keyword that is not unreserved, and else quoted.
func deparsedName(column string) string {
	if plainName.MatchString(column) && !keywords[column] {
		return column
	}
	return sqlIdentifier(column)
}

// joinChecks joins the CHECK expressions of one field with AND: the text of the migration, or the text that
// pg_get_constraintdef writes, which puts the expressions of an AND of more than one in one pair of parentheses.
func joinChecks(checks []checkItem, deparsed bool) string {
	parts := make([]string, len(checks))
	for i, c := range checks {
		parts[i] = c.source
		if deparsed {
			parts[i] = c.deparsed
		}
	}
	if deparsed && len(parts) > 1 {
		return "(" + strings.Join(parts, " AND ") + ")"
	}
	return strings.Join(parts, " AND ")
}

// The types that PostgreSQL gives a numeric literal of the migration.
const (
	literalInteger = "integer"
	literalBigint  = "bigint"
	literalNumeric = "numeric"
)

// literalConstant gives the type that PostgreSQL gives the literal text of a bound, and the constant as
// pg_get_constraintdef writes it: a literal with a point or an exponent is numeric, and else an integer, a bigint
// or a numeric by its size; a negative literal is the constant of its absolute value, negated. An integer of zero or
// more has no quotes, a numeric with a point and no sign has none, and every other constant is quoted with its
// type.
func literalConstant(literal string) (string, string) {
	digits := strings.TrimPrefix(literal, "-")
	negative := digits != literal
	typ := literalNumeric
	if !strings.ContainsAny(digits, ".eE") {
		if n, err := strconv.ParseInt(digits, 10, 64); err == nil {
			typ = literalBigint
			if n <= math.MaxInt32 {
				typ = literalInteger
			}
		}
	}
	text := digits
	if typ == literalNumeric {
		value, _, err := big.ParseFloat(digits, 10, 256, big.ToNearestEven)
		if err == nil {
			text = decimalText(digits, value)
		}
	}
	if negative {
		text = "-" + text
	}
	switch {
	case typ == literalInteger && !negative:
		return typ, text
	case typ == literalNumeric && !negative && strings.Contains(text, "."):
		return typ, text
	}
	return typ, "'" + text + "'::" + typ
}

// decimalText gives the text of a numeric literal as PostgreSQL writes the numeric value: its digits with the scale
// of the literal, which for an exponent is the digits after the point less the exponent.
func decimalText(literal string, value *big.Float) string {
	mantissa, exponent, _ := strings.Cut(strings.ToLower(literal), "e")
	scale := 0
	if _, fraction, found := strings.Cut(mantissa, "."); found {
		scale = len(fraction)
	}
	if exponent != "" {
		e, _ := strconv.Atoi(exponent)
		scale -= e
	}
	if scale < 0 {
		scale = 0
	}
	return value.Text('f', scale)
}

// deparsedBound gives a numeric bound of a column of the type typ as pg_get_constraintdef writes it: an integer
// column against an integer or a bigint compares as is, and against a numeric as numeric; a float column compares
// against the constant as double precision.
func deparsedBound(column, typ, op, literal string) string {
	constType, constant := literalConstant(literal)
	switch {
	case typ == "real" || typ == "double precision":
		return "(" + column + " " + op + " (" + constant + ")::double precision)"
	case constType == literalNumeric:
		return "((" + column + ")::numeric " + op + " " + constant + ")"
	}
	return "(" + column + " " + op + " " + constant + ")"
}

// tenantPolicyExpression is the USING and WITH CHECK expression of the tenant policy on the tenant column, as
// pg_get_expr gives it with the search path pg_catalog.
func tenantPolicyExpression(key string) string {
	return "(" + deparsedName(key) + " = current_setting('app.tenant'::text, true))"
}

// deparsedList gives column names as pg_get_constraintdef lists them.
func deparsedList(columns []string) string {
	out := make([]string, len(columns))
	for i, c := range columns {
		out[i] = deparsedName(c)
	}
	return strings.Join(out, ", ")
}

// manifestSource gives the Go source of the manifest of the tables: for each table its columns with their types
// and NOT NULL, its primary key, check and foreign key constraints with their definitions as pg_get_constraintdef
// writes them, its primary key and unique indexes, and on an
// owned table of a pooled service the row-level security and the tenant policy. The search indexes are left out:
// a list that has no search index is slower, not wrong.
func manifestSource(models []tableModel, key string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "manifest.Manifest{Schemas: []manifest.Schema{{Name: %q, Steps: true}},\nTables: []manifest.Table{\n", StepSchema)
	for _, table := range models {
		fmt.Fprintf(&b, "{Schema: %q, Name: %q, Entity: true,\n", "public", table.name)
		b.WriteString("Columns: []manifest.Column{\n")
		for _, column := range table.columns {
			if column.manifestDefault != "" {
				fmt.Fprintf(&b, "{Name: %q, Type: %q, NotNull: %t, Default: %q},\n", column.name, column.typ, column.notNull, column.manifestDefault)
				continue
			}
			fmt.Fprintf(&b, "{Name: %q, Type: %q, NotNull: %t},\n", column.name, column.typ, column.notNull)
		}
		b.WriteString("},\nConstraints: []manifest.Constraint{\n")
		fmt.Fprintf(&b, "{Name: %q, Kind: manifest.KindPrimaryKey, Definition: %q},\n", primaryName(table.name),
			"PRIMARY KEY ("+deparsedList(append(append([]string{}, table.lead...), keyColumn))+")")
		for _, check := range table.checks {
			fmt.Fprintf(&b, "{Name: %q, Kind: manifest.KindCheck, Definition: %q},\n", check.name, check.definition)
		}
		for _, fk := range table.foreign {
			fmt.Fprintf(&b, "{Name: %q, Kind: manifest.KindForeignKey, Definition: %q},\n", fk.name,
				"FOREIGN KEY ("+deparsedList(fk.from)+") REFERENCES public."+deparsedName(fk.table)+"("+deparsedList(fk.to)+")")
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
