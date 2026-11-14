package postgresadapter

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"go/format"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

// The tables of the runtime data path (ST-SUP-RC01-OC41), the only data path (ST-SUP-RC20-OC39): the generated
// commands and queries carry all data access, the migration 001_initial is the SQL text of the tables of the
// declaration, and the adapter emits no store, no entity model and no migration of models. The migration ledger, the
// schema generation marker and the row-level security of the tenancy component go through the same ledger as every
// other migration (ST-SUP-RC01-OC11).
//
// The shape of each table is fixed by the declaration alone and in this order: the tenant column of an owned
// entity in a pooled service (ST-SUP-RC01-OC40), the key id, each declared field, created_time and updated_time,
// the resource version that a write carries (ST-SUP-RP10-LC24) and, for a protected entity, the
// authorization token (ST-SUP-RC29-OC06). The tables follow their references and then their names, never the order
// of the declaration, so that the same entities give the same body and the same ledger digest.

// tablesFile is the Go file of the runtime data path that holds the SQL text of 001_initial, and tablesConstant
// the name of that constant.
const (
	tablesFile     = "tables.go"
	tablesConstant = "InitialTables"
)

// tableColumns are the columns of every table that the declaration does not name, after its fields.
const (
	createdColumn = "created_time"
	updatedColumn = "updated_time"
	versionColumn = "version"
	tokenColumn   = "authz_token"
	keyColumn     = "id"
)

// storeOnly names the declaration settings of an entity that only the GORM store carried out: the runtime data
// path has no deleted rows, no observations and no conditions, so the adapter refuses them rather than drop them.
func storeOnly(entity types.Entity) string {
	switch {
	case len(entity.GenerationFields) > 0:
		return "generation_fields"
	case len(entity.Observations) > 0:
		return "observations"
	case len(entity.Conditions) > 0:
		return "conditions"
	case len(entity.CleanupOwners) > 0:
		return "cleanup_owners"
	case len(entity.CleanupTargets) > 0:
		return "cleanup_targets"
	}
	return ""
}

// removedStore is the cause that each refusal of a declaration of the removed GORM store names.
const removedStore = "STEGO serves data only through the generated commands and queries, and the GORM store that served it before is removed (ST-SUP-RC20-OC39)"

// validateRuntimeData refuses a service with entities that the runtime data path cannot serve, with the setting to
// change. Every such declaration had this component as its GORM store, so the refusal is here: a component set
// without the command or the query component, which carry all data access, and an entity setting that only the
// store carried out. The migration ledger exists only with schema_generation, and a SQL table cannot be created
// twice, so the path needs it.
func validateRuntimeData(ctx gen.Context) error {
	if ctx.PeerNamespaces["command"] == "" || ctx.PeerNamespaces["query"] == "" {
		return fmt.Errorf("postgres-adapter: the service declares entities, and its component set has no command or query component: %s; add the command and query components, or use an archetype that holds them", removedStore)
	}
	if ctx.ComponentConfig["schema_generation"] == nil {
		return fmt.Errorf("postgres-adapter needs schema_generation: its tables are SQL migrations, which only the migration ledger applies once")
	}
	for _, entity := range ctx.Entities {
		if setting := storeOnly(entity); setting != "" {
			return fmt.Errorf("postgres-adapter: entity %s: %s is a setting of the GORM store: %s; remove the setting", entity.Name, setting, removedStore)
		}
		// PostgreSQL cuts a longer name to 63 bytes, so two long names could become one column or one table.
		if table := tableName(entity.Name); len(table) > 63 {
			return fmt.Errorf("entity %s: the table name %s is longer than 63 bytes, which PostgreSQL cuts", entity.Name, table)
		}
		for _, field := range entity.Fields {
			if len(field.ColumnName()) > 63 {
				return fmt.Errorf("entity %s: the field name %s is longer than 63 bytes, which PostgreSQL cuts", entity.Name, field.ColumnName())
			}
		}
	}
	return nil
}

// sqlIdentifier quotes a name as a PostgreSQL identifier. The names that the declaration gives are checked to the
// characters of validFieldNamePattern, so a quote is the only character to double.
func sqlIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// sqlString quotes a value as a PostgreSQL string literal.
func sqlString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// sqlName joins the parts of a constraint or an index name. PostgreSQL keeps 63 bytes of a name, so a longer name
// keeps its first 46 bytes and adds 16 hexadecimal digits of the digest of the whole name, which keeps it unique.
func sqlName(parts ...string) string {
	name := strings.Join(parts, "_")
	if len(name) <= 63 {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return name[:46] + "_" + hex.EncodeToString(sum[:8])
}

// columnType gives the PostgreSQL type of a declared field. A reference holds the key of the row it refers to,
// which is a uuid.
func columnType(field types.Field) (string, error) {
	switch field.Type {
	case types.FieldTypeString:
		if field.MaxLength != nil {
			return fmt.Sprintf("character varying(%d)", *field.MaxLength), nil
		}
		return "text", nil
	case types.FieldTypeEnum:
		return "text", nil
	case types.FieldTypeRef:
		return "uuid", nil
	case types.FieldTypeInt32:
		return "integer", nil
	case types.FieldTypeInt64:
		return "bigint", nil
	case types.FieldTypeFloat:
		return "real", nil
	case types.FieldTypeDouble:
		return "double precision", nil
	case types.FieldTypeBool:
		return "boolean", nil
	case types.FieldTypeBytes:
		return "bytea", nil
	case types.FieldTypeTimestamp:
		return "timestamp with time zone", nil
	case types.FieldTypeJsonb:
		return "jsonb", nil
	}
	return "", fmt.Errorf("field %s: the type %q has no column type", field.Name, field.Type)
}

// columnDefault gives the DEFAULT clause of a declared field's default, or "" when it has none. Checking has
// matched the default to the field's type (ST-DECL-EN03); a text default of a timestamp or a jsonb field is a
// literal that PostgreSQL reads as the column's type when the migration runs.
func columnDefault(field types.Field) (string, error) {
	switch value := field.Default.(type) {
	case nil:
		return "", nil
	case string:
		return " DEFAULT " + sqlString(value), nil
	case bool:
		return " DEFAULT " + strconv.FormatBool(value), nil
	case int:
		return " DEFAULT " + strconv.Itoa(value), nil
	case int64:
		return " DEFAULT " + strconv.FormatInt(value, 10), nil
	case uint64:
		return " DEFAULT " + strconv.FormatUint(value, 10), nil
	case float64:
		return " DEFAULT " + strconv.FormatFloat(value, 'g', -1, 64), nil
	}
	return "", fmt.Errorf("field %s: the default %v has no SQL literal", field.Name, field.Default)
}

// literalDefault gives a declared field's default as the schema manifest states it: the value as a quoted literal
// with a cast to the column's type, which the check compares by its value with the default of the column; "" when
// the field has none.
func literalDefault(value any, columnType string) string {
	var text string
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		text = v
	case bool:
		text = strconv.FormatBool(v)
	case int:
		text = strconv.Itoa(v)
	case int64:
		text = strconv.FormatInt(v, 10)
	case uint64:
		text = strconv.FormatUint(v, 10)
	case float64:
		text = strconv.FormatFloat(v, 'g', -1, 64)
	default:
		return ""
	}
	return sqlString(text) + "::" + columnType
}

// checkItem is one CHECK expression of a field: its text in the migration, and its text as pg_get_constraintdef
// writes it, which the schema manifest holds (ST-SUP-RC01-OC12).
type checkItem struct{ source, deparsed string }

// columnChecks gives the CHECK expressions of a declared field of the column type typ: its length bounds, its
// numeric bounds, the finite value of a bounded float, and the declared values of an enum. A pattern stays with
// request validation, because PostgreSQL's regular expressions are not the RE2 expressions that the API checks.
func columnChecks(field types.Field, typ string) []checkItem {
	column := sqlIdentifier(field.ColumnName())
	ref := deparsedName(field.ColumnName())
	var checks []checkItem
	if field.MinLength != nil {
		length := ref
		if typ != "text" {
			length = "(" + ref + ")::text"
		}
		checks = append(checks, checkItem{fmt.Sprintf("char_length(%s) >= %d", column, *field.MinLength),
			fmt.Sprintf("(char_length(%s) >= %d)", length, *field.MinLength)})
	}
	for _, bound := range []struct {
		op    string
		value *float64
	}{{">=", field.Min}, {"<=", field.Max}} {
		if bound.value == nil {
			continue
		}
		literal := strconv.FormatFloat(*bound.value, 'g', -1, 64)
		checks = append(checks, checkItem{column + " " + bound.op + " " + literal, deparsedBound(ref, typ, bound.op, literal)})
	}
	if (field.Min != nil || field.Max != nil) && (field.Type == types.FieldTypeFloat || field.Type == types.FieldTypeDouble) {
		checks = append(checks, checkItem{column + " NOT IN ('NaN'::double precision, 'Infinity'::double precision, '-Infinity'::double precision)",
			"(" + ref + " <> ALL (ARRAY['NaN'::double precision, 'Infinity'::double precision, '-Infinity'::double precision]))"})
	}
	if field.Type == types.FieldTypeEnum && len(field.Values) > 0 {
		values := make([]string, len(field.Values))
		typed := make([]string, len(field.Values))
		for i, value := range field.Values {
			values[i] = sqlString(value)
			typed[i] = sqlString(value) + "::text"
		}
		deparsed := "(" + ref + " = ANY (ARRAY[" + strings.Join(typed, ", ") + "]))"
		if len(typed) == 1 {
			deparsed = "(" + ref + " = " + typed[0] + ")"
		}
		checks = append(checks, checkItem{column + " IN (" + strings.Join(values, ", ") + ")", deparsed})
	}
	return checks
}

// tableOrder gives the entities in the order of their tables: each table after the tables its references need,
// and else by table name, so the order of the declaration does not change the migration body.
func tableOrder(entities []types.Entity) ([]types.Entity, error) {
	named := append([]types.Entity(nil), entities...)
	sort.SliceStable(named, func(i, j int) bool { return tableName(named[i].Name) < tableName(named[j].Name) })
	return topoSortEntities(named)
}

// uniqueKeys gives the column lists of the unique indexes of an entity: each unique field, each unique_composite
// group once, and each upsert key of a collection over the entity with its scope and implicit fields
// (types.Collection.UpsertLookup), in that order. A unique_when_live field is
// unique over all rows, because a command's delete removes the row and leaves no deleted row to leave out.
func uniqueKeys(entity types.Entity, upsertKeys [][]string) [][]string {
	var keys [][]string
	seen := map[string]bool{}
	add := func(columns []string) {
		id := strings.Join(columns, "\x00")
		if seen[id] {
			return
		}
		seen[id] = true
		keys = append(keys, columns)
	}
	for _, field := range entity.Fields {
		if field.Unique {
			add([]string{field.ColumnName()})
		}
	}
	for _, field := range entity.Fields {
		if len(field.UniqueComposite) > 0 {
			add(field.UniqueComposite)
		}
	}
	for _, key := range upsertKeys {
		add(key)
	}
	return keys
}

// tableModel is the shape of one table of the runtime data path, from the declaration alone: the model that both
// the migration 001_initial (tablesSQL) and the schema manifest (schemaManifest) are made from, so the two cannot
// differ (ST-SUP-RC01-OC41, ST-SUP-RC01-OC12). Names are not quoted.
type tableModel struct {
	name string
	// entity is the entity whose table it is.
	entity string
	// owned tells that the table carries the tenant column of a pooled service, which leads the table, its
	// primary key and every index (ST-SUP-RC01-OC40), and has the row-level security of the tenancy component.
	owned bool
	// lead is the tenant column when owned, and else empty.
	lead    []string
	columns []columnModel
	checks  []checkModel
	foreign []foreignModel
	// search are the columns of the search indexes, each after lead; unique are the column lists of the unique
	// indexes, each after lead.
	search []string
	unique [][]string
}

// columnModel is one column: its name, its type, its NOT NULL, its DEFAULT clause, and its default as the schema
// manifest states it (ST-SUP-RC01-OC12).
type columnModel struct {
	name, typ, defaultClause, manifestDefault string
	notNull                                   bool
}

// checkModel is one CHECK constraint: its name, its expression, and its definition as pg_get_constraintdef
// writes it.
type checkModel struct{ name, expression, definition string }

// foreignModel is one FOREIGN KEY constraint: its name, its columns, the table it refers to and that table's
// columns.
type foreignModel struct {
	name, table string
	from, to    []string
}

// primaryName, uniqueName and searchName give the names of the primary key, a unique index and a search index of a
// table.
func primaryName(table string) string { return sqlName(table, "pkey") }

func uniqueName(table string, columns []string) string {
	return sqlName(append([]string{table}, append(append([]string{}, columns...), "key")...)...)
}

func searchName(table, column string) string { return sqlName(table, column, "search") }

// tableModels gives the model of each table of the declaration, in the order of tableOrder.
func tableModels(ctx gen.Context) ([]tableModel, error) {
	ordered, err := tableOrder(ctx.Entities)
	if err != nil {
		return nil, err
	}
	key := tenantColumn(ctx)
	owned := map[string]bool{}
	for _, entity := range ctx.Entities {
		owned[entity.Name] = key != "" && ownedEntity(entity)
	}
	upsertKeys := map[string][][]string{}
	for _, collection := range ctx.Collections {
		if len(collection.UpsertKey) > 0 {
			upsertKeys[collection.Entity] = append(upsertKeys[collection.Entity], collection.UpsertLookup())
		}
	}
	var models []tableModel
	for _, entity := range ordered {
		table := tableModel{name: tableName(entity.Name), entity: entity.Name, owned: owned[entity.Name]}
		if table.owned {
			table.lead = []string{key}
			table.columns = append(table.columns, columnModel{name: key, typ: "text", notNull: true, defaultClause: " DEFAULT current_setting('app.tenant')",
				manifestDefault: "current_setting('app.tenant'::text)"})
		}
		table.columns = append(table.columns, columnModel{name: keyColumn, typ: "uuid", notNull: true})
		for _, field := range entity.Fields {
			columnTypeName, err := columnType(field)
			if err != nil {
				return nil, fmt.Errorf("entity %s: %w", entity.Name, err)
			}
			fieldDefault, err := columnDefault(field)
			if err != nil {
				return nil, fmt.Errorf("entity %s: %w", entity.Name, err)
			}
			table.columns = append(table.columns, columnModel{name: field.ColumnName(), typ: columnTypeName,
				notNull: !field.Optional && !field.Computed, defaultClause: fieldDefault, manifestDefault: literalDefault(field.Default, columnTypeName)})
			if checks := columnChecks(field, columnTypeName); len(checks) > 0 {
				table.checks = append(table.checks, checkModel{name: sqlName(table.name, field.ColumnName(), "check"),
					expression: joinChecks(checks, false), definition: "CHECK (" + joinChecks(checks, true) + ")"})
			}
		}
		table.columns = append(table.columns,
			columnModel{name: createdColumn, typ: "timestamp with time zone", notNull: true, defaultClause: " DEFAULT now()", manifestDefault: "now()"},
			columnModel{name: updatedColumn, typ: "timestamp with time zone", notNull: true, defaultClause: " DEFAULT now()", manifestDefault: "now()"},
			columnModel{name: versionColumn, typ: "bigint", notNull: true, defaultClause: " DEFAULT 1", manifestDefault: "1"})
		if entity.ResourceType != "" {
			table.columns = append(table.columns, columnModel{name: tokenColumn, typ: "text", notNull: true, defaultClause: " DEFAULT ''", manifestDefault: "''::text"})
		}
		for _, field := range entity.Fields {
			if field.Type != types.FieldTypeRef || field.To == "" {
				continue
			}
			from, to := []string{field.ColumnName()}, []string{keyColumn}
			if table.owned && owned[field.To] {
				from, to = append(append([]string{}, table.lead...), from...), append(append([]string{}, table.lead...), to...)
			}
			table.foreign = append(table.foreign, foreignModel{name: sqlName(table.name, field.ColumnName(), "fkey"), table: tableName(field.To), from: from, to: to})
		}
		table.search = searchColumns(entity, ctx.Collections)
		table.unique = uniqueKeys(entity, upsertKeys[entity.Name])
		models = append(models, table)
	}
	return models, nil
}

// quoted quotes each name as a PostgreSQL identifier.
func quoted(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = sqlIdentifier(n)
	}
	return out
}

// tablesSQL gives the SQL text of 001_initial on the runtime data path: one CREATE TABLE for each entity of the
// baseline, the declaration with every schema step undone (gen.Context.AtSchemaStep), and one CREATE UNIQUE INDEX
// for each of its unique keys, from the declaration alone (ST-SUP-RC01-OC41). The steps add the rest in migrations
// of their own (schemaSteps), so the body and its ledger digest stay those of the first release. In a pooled service
// the tenant column leads the table, its primary key, every index and every foreign key to another owned table
// (ST-SUP-RC01-OC40).
func tablesSQL(ctx gen.Context) (string, error) {
	baseline, err := ctx.AtSchemaStep(0)
	if err != nil {
		return "", err
	}
	models, err := tableModels(baseline)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for _, table := range models {
		out.WriteString(createTableSQL(table))
	}
	return out.String(), nil
}

// BaselineTable is what the migration 001_initial holds for one table of the baseline: the entity whose table it is,
// the name of the table, its CREATE TABLE statement and its indexes.
type BaselineTable struct {
	Entity, Table, Create, Indexes string
}

// Baseline gives what a release with the declaration of ctx writes for its baseline: each table of 001_initial in
// its order (tablesSQL), and the digest of the stored shape that the schema marker holds (markerDefinitions). The
// check against the base compares it for the base and the head (ST-GEN-INV33), so an edit that changes either one,
// such as a new order of the fields or a field that leaves a search index, is refused before a release fails the
// ledger or the marker check of a database with rows.
func Baseline(ctx gen.Context) ([]BaselineTable, string, error) {
	baseline, err := ctx.AtSchemaStep(0)
	if err != nil {
		return nil, "", err
	}
	models, err := tableModels(baseline)
	if err != nil {
		return nil, "", err
	}
	tables := make([]BaselineTable, len(models))
	for i, table := range models {
		tables[i] = BaselineTable{Entity: table.entity, Table: table.name, Create: tableSQL(table), Indexes: indexSQL(table)}
	}
	marker, _, err := markerDefinitions(ctx)
	if err != nil {
		return nil, "", err
	}
	return tables, marker, nil
}

// columnSQL gives the definition of one column, as CREATE TABLE and ADD COLUMN write it.
func columnSQL(column columnModel) string {
	line := sqlIdentifier(column.name) + " " + column.typ
	if column.notNull {
		line += " NOT NULL"
	}
	return line + column.defaultClause
}

// foreignSQL gives the FOREIGN KEY clause of one foreign key, after its CONSTRAINT name.
func foreignSQL(fk foreignModel) string {
	return "FOREIGN KEY (" + strings.Join(quoted(fk.from), ", ") + ") REFERENCES public." + sqlIdentifier(fk.table) + " (" + strings.Join(quoted(fk.to), ", ") + ")"
}

// searchIndexSQL and uniqueIndexSQL give the statements of a search index and of a unique index of a table.
func searchIndexSQL(table tableModel, column string) string {
	return fmt.Sprintf("CREATE INDEX %s ON public.%s (%s);\n", sqlIdentifier(searchName(table.name, column)),
		sqlIdentifier(table.name), strings.Join(quoted(append(append([]string{}, table.lead...), column)), ", "))
}

func uniqueIndexSQL(table tableModel, columns []string) string {
	return fmt.Sprintf("CREATE UNIQUE INDEX %s ON public.%s (%s);\n", sqlIdentifier(uniqueName(table.name, columns)),
		sqlIdentifier(table.name), strings.Join(quoted(append(append([]string{}, table.lead...), columns...)), ", "))
}

// searchIndexDefinition and uniqueIndexDefinition give the definitions of a search index and of a unique index of a
// table as pg_get_indexdef writes them, which the migration of a step that builds the index checks before its ledger
// row (ST-SUP-RC01-OC38).
func searchIndexDefinition(table tableModel, column string) string {
	return "CREATE INDEX " + deparsedName(searchName(table.name, column)) + " ON public." + deparsedName(table.name) +
		" USING btree (" + deparsedList(append(append([]string{}, table.lead...), column)) + ")"
}

func uniqueIndexDefinition(table tableModel, columns []string) string {
	return "CREATE UNIQUE INDEX " + deparsedName(uniqueName(table.name, columns)) + " ON public." + deparsedName(table.name) +
		" USING btree (" + deparsedList(append(append([]string{}, table.lead...), columns...)) + ")"
}

// createTableSQL gives the CREATE TABLE of one table and its indexes: the text of the table in 001_initial, and in
// the step that adds its entity.
func createTableSQL(table tableModel) string {
	return tableSQL(table) + indexSQL(table)
}

// tableSQL gives the CREATE TABLE statement of one table, with its columns and constraints and no index.
func tableSQL(table tableModel) string {
	var out strings.Builder
	var lines []string
	for _, column := range table.columns {
		lines = append(lines, columnSQL(column))
	}
	lines = append(lines, "CONSTRAINT "+sqlIdentifier(primaryName(table.name))+" PRIMARY KEY ("+strings.Join(quoted(append(append([]string{}, table.lead...), keyColumn)), ", ")+")")
	for _, check := range table.checks {
		lines = append(lines, "CONSTRAINT "+sqlIdentifier(check.name)+" CHECK ("+check.expression+")")
	}
	for _, fk := range table.foreign {
		lines = append(lines, "CONSTRAINT "+sqlIdentifier(fk.name)+" "+foreignSQL(fk))
	}
	fmt.Fprintf(&out, "CREATE TABLE public.%s (\n  %s\n);\n", sqlIdentifier(table.name), strings.Join(lines, ",\n  "))
	return out.String()
}

// indexSQL gives the indexes of one table: its search indexes and its unique indexes.
func indexSQL(table tableModel) string {
	var out strings.Builder
	// A list filters a table on its scope field, its implicit fields and the fields of its search, each with the
	// tenant: an index of each keeps a filtered list from scanning the tenant's whole table (ST-SUP-RP11-LC20).
	for _, column := range table.search {
		out.WriteString(searchIndexSQL(table, column))
	}
	for _, columns := range table.unique {
		out.WriteString(uniqueIndexSQL(table, columns))
	}
	return out.String()
}

// searchColumns gives the columns of the entity that a list of a collection over it filters on, in the entity's
// order: the scope field and the implicit fields of each collection with list, and the fields that its search takes,
// which are the fields that are not pii or secret, of a text, enum, number or bool type.
func searchColumns(entity types.Entity, collections []types.Collection) []string {
	filtered := map[string]bool{}
	listed := false
	for _, c := range collections {
		if c.Entity != entity.Name || !slices.Contains(c.Operations, types.OpList) {
			continue
		}
		listed = true
		for field := range c.Scope {
			filtered[field] = true
		}
		for field := range c.Implicit {
			filtered[field] = true
		}
	}
	if !listed {
		return nil
	}
	var columns []string
	for _, f := range entity.Fields {
		searchable := f.Sensitivity == "" && (f.Type == types.FieldTypeString || f.Type == types.FieldTypeEnum ||
			f.Type == types.FieldTypeInt32 || f.Type == types.FieldTypeInt64 || f.Type == types.FieldTypeFloat ||
			f.Type == types.FieldTypeDouble || f.Type == types.FieldTypeBool)
		if filtered[f.Name] || searchable {
			columns = append(columns, f.ColumnName())
		}
	}
	return columns
}

// generateTables gives the files of the tables on the runtime data path: the SQL text of 001_initial as the
// constant InitialTables, which the migration ledger registers, and the same text as migrations/001_initial.sql
// for a reader and for an operator who runs the migrations outside the service.
func generateTables(ctx gen.Context) ([]gen.File, error) {
	body, err := tablesSQL(ctx)
	if err != nil {
		return nil, err
	}
	source := fmt.Sprintf("package %s\n\n// %s creates the table of each entity of the declaration, the body of the migration 001_initial.\nconst %s = %q\n",
		path.Base(ctx.OutputNamespace), tablesConstant, tablesConstant, body)
	formatted, err := format.Source([]byte(source))
	if err != nil {
		return nil, fmt.Errorf("formatting tables: %w", err)
	}
	return []gen.File{
		{Path: path.Join(ctx.OutputNamespace, tablesFile), Content: formatted},
		{Path: path.Join(ctx.OutputNamespace, "migrations", "001_initial.sql"), Content: []byte(body)},
	}, nil
}

//go:embed runtime_access.go.tmpl
var runtimeAccessSource string

// generateRuntimeAccess gives runtime_access.go of the runtime data path: TenantConfig, the configuration of the
// runtime library's connectors from the adapter's opener, and CheckLedger, the read-only check of the migration job
// that refuses a database whose ledger is not a prefix of this release's migrations or that holds a table of the
// declaration with no ledger.
func generateRuntimeAccess(ctx gen.Context) (gen.File, error) {
	t, err := template.New("runtime-access").Parse(runtimeAccessSource)
	if err != nil {
		return gen.File{}, err
	}
	var tables []string
	for _, entity := range ctx.Entities {
		tables = append(tables, tableName(entity.Name))
	}
	var output bytes.Buffer
	if err := t.Execute(&output, struct {
		Package string
		Tables  []string
	}{path.Base(ctx.OutputNamespace), tables}); err != nil {
		return gen.File{}, err
	}
	code, err := format.Source(output.Bytes())
	if err != nil {
		return gen.File{}, fmt.Errorf("formatting runtime_access.go: %w", err)
	}
	return gen.File{Path: path.Join(ctx.OutputNamespace, "runtime_access.go"), Content: code}, nil
}

// generateRuntimeData gives the files and the wiring of the runtime data path: the database opener, the schema
// generation marker, the migration ledger with 001_initial as the SQL of InitialTables and, in a pooled service,
// the row-level security of the tenancy component after it, and the schema manifest of the tables, which the service
// runtime checks at startup (ST-SUP-RC01-OC12). It emits no entity model and no store, so data access
// goes only through the generated commands and queries (ST-SUP-RC01-OC41). The ledger runs on database/sql, on the
// connection of the migration job; the runtime role gets the grants of ST-SUP-RC27-OC07 and ST-STD-TXN-R03.
func generateRuntimeData(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	files, err := generateTables(ctx)
	if err != nil {
		return nil, nil, err
	}
	rowSecurity, err := rowSecurityImport(ctx)
	if err != nil {
		return nil, nil, err
	}
	steps, err := schemaSteps(ctx)
	if err != nil {
		return nil, nil, err
	}
	files = append(files, generateSteps(ctx.OutputNamespace, steps)...)
	migrateFile, err := generateMigrate(ctx.OutputNamespace, rowSecurity, steps)
	if err != nil {
		return nil, nil, fmt.Errorf("generating migrate: %w", err)
	}
	schemaFile, err := generateSchemaGeneration(ctx)
	if err != nil {
		return nil, nil, err
	}
	opener, err := generateDatabaseOpener(ctx)
	if err != nil {
		return nil, nil, err
	}
	access, err := generateRuntimeAccess(ctx)
	if err != nil {
		return nil, nil, err
	}
	schemaManifest, err := generateSchemaManifest(ctx)
	if err != nil {
		return nil, nil, err
	}
	files = append(files, migrateFile, schemaFile, opener, access, schemaManifest)
	// The migration job of the runtime wiring runs the ledger (service.Migrate), so the wiring has no call after
	// the database opens.
	wiring := &gen.Wiring{
		Imports:        []string{ctx.OutputNamespace},
		NeedsDB:        true,
		DatabaseOpener: &gen.DatabaseOpenerSpec{Namespace: ctx.OutputNamespace, Function: "OpenDatabase"},
		GoModRequires: map[string]string{
			"github.com/google/uuid":  "v1.6.0",
			"github.com/jackc/pgx/v5": "v5.11.0",
		},
	}
	entityAccess(ctx, wiring)
	schemaGenerationAccess(ctx, wiring)
	if value, _ := ctx.ComponentConfig["migrations"].(string); value == "external" {
		wiring.VerifyDatabaseAccess = true
	}
	if err := sealMigrations(ctx.OutputNamespace, files); err != nil {
		return nil, nil, err
	}
	if err := gen.ValidateNamespace(ctx.OutputNamespace, files); err != nil {
		return nil, nil, err
	}
	return files, wiring, nil
}
