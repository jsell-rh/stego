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
// declaration, and the adapter emits no store, no entity model and no GORM AutoMigrate. The migration ledger, the
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
			if len(field.Name) > 63 {
				return fmt.Errorf("entity %s: the field name %s is longer than 63 bytes, which PostgreSQL cuts", entity.Name, field.Name)
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

// columnChecks gives the CHECK expressions of a declared field: its length bounds, its numeric bounds, the finite
// value of a bounded float, and the declared values of an enum. A pattern stays with request validation, because
// PostgreSQL's regular expressions are not the RE2 expressions that the API checks.
func columnChecks(field types.Field) []string {
	column := sqlIdentifier(field.Name)
	var checks []string
	if field.MinLength != nil {
		checks = append(checks, fmt.Sprintf("char_length(%s) >= %d", column, *field.MinLength))
	}
	if field.Min != nil {
		checks = append(checks, column+" >= "+strconv.FormatFloat(*field.Min, 'g', -1, 64))
	}
	if field.Max != nil {
		checks = append(checks, column+" <= "+strconv.FormatFloat(*field.Max, 'g', -1, 64))
	}
	if (field.Min != nil || field.Max != nil) && (field.Type == types.FieldTypeFloat || field.Type == types.FieldTypeDouble) {
		checks = append(checks, column+" NOT IN ('NaN'::double precision, 'Infinity'::double precision, '-Infinity'::double precision)")
	}
	if field.Type == types.FieldTypeEnum && len(field.Values) > 0 {
		values := make([]string, len(field.Values))
		for i, value := range field.Values {
			values[i] = sqlString(value)
		}
		checks = append(checks, column+" IN ("+strings.Join(values, ", ")+")")
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
			add([]string{field.Name})
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

// tablesSQL gives the SQL text of 001_initial on the runtime data path: one CREATE TABLE for each entity and one
// CREATE UNIQUE INDEX for each of its unique keys, from the declaration alone (ST-SUP-RC01-OC41). In a pooled
// service the tenant column leads the table, its primary key, every index and every foreign key to another owned
// table (ST-SUP-RC01-OC40).
func tablesSQL(ctx gen.Context) (string, error) {
	ordered, err := tableOrder(ctx.Entities)
	if err != nil {
		return "", err
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
	var out strings.Builder
	for _, entity := range ordered {
		table := tableName(entity.Name)
		lead := []string{}
		if owned[entity.Name] {
			lead = []string{sqlIdentifier(key)}
		}
		var lines []string
		if owned[entity.Name] {
			lines = append(lines, sqlIdentifier(key)+" text NOT NULL DEFAULT current_setting('app.tenant')")
		}
		lines = append(lines, sqlIdentifier(keyColumn)+" uuid NOT NULL")
		var constraints []string
		for _, field := range entity.Fields {
			columnTypeName, err := columnType(field)
			if err != nil {
				return "", fmt.Errorf("entity %s: %w", entity.Name, err)
			}
			line := sqlIdentifier(field.Name) + " " + columnTypeName
			if !field.Optional && !field.Computed {
				line += " NOT NULL"
			}
			fieldDefault, err := columnDefault(field)
			if err != nil {
				return "", fmt.Errorf("entity %s: %w", entity.Name, err)
			}
			lines = append(lines, line+fieldDefault)
			if checks := columnChecks(field); len(checks) > 0 {
				constraints = append(constraints, "CONSTRAINT "+sqlIdentifier(sqlName(table, field.Name, "check"))+" CHECK ("+strings.Join(checks, " AND ")+")")
			}
		}
		lines = append(lines,
			sqlIdentifier(createdColumn)+" timestamp with time zone NOT NULL DEFAULT now()",
			sqlIdentifier(updatedColumn)+" timestamp with time zone NOT NULL DEFAULT now()",
			sqlIdentifier(versionColumn)+" bigint NOT NULL DEFAULT 1")
		if entity.ResourceType != "" {
			lines = append(lines, sqlIdentifier(tokenColumn)+" text NOT NULL DEFAULT ''")
		}
		primary := append(append([]string{}, lead...), sqlIdentifier(keyColumn))
		lines = append(lines, "CONSTRAINT "+sqlIdentifier(sqlName(table, "pkey"))+" PRIMARY KEY ("+strings.Join(primary, ", ")+")")
		lines = append(lines, constraints...)
		for _, field := range entity.Fields {
			if field.Type != types.FieldTypeRef || field.To == "" {
				continue
			}
			from, to := []string{sqlIdentifier(field.Name)}, []string{sqlIdentifier(keyColumn)}
			if owned[entity.Name] && owned[field.To] {
				from, to = append(append([]string{}, lead...), from...), append(append([]string{}, lead...), to...)
			}
			lines = append(lines, "CONSTRAINT "+sqlIdentifier(sqlName(table, field.Name, "fkey"))+" FOREIGN KEY ("+strings.Join(from, ", ")+
				") REFERENCES public."+sqlIdentifier(tableName(field.To))+" ("+strings.Join(to, ", ")+")")
		}
		fmt.Fprintf(&out, "CREATE TABLE public.%s (\n  %s\n);\n", sqlIdentifier(table), strings.Join(lines, ",\n  "))
		// A list filters a table on its scope field, its implicit fields and the fields of its search, each with the
		// tenant: an index of each keeps a filtered list from scanning the tenant's whole table (ST-SUP-RP11-LC20).
		for _, column := range searchColumns(entity, ctx.Collections) {
			fmt.Fprintf(&out, "CREATE INDEX %s ON public.%s (%s);\n", sqlIdentifier(sqlName(table, column, "search")),
				sqlIdentifier(table), strings.Join(append(append([]string{}, lead...), sqlIdentifier(column)), ", "))
		}
		for _, columns := range uniqueKeys(entity, upsertKeys[entity.Name]) {
			quoted := append([]string{}, lead...)
			for _, column := range columns {
				quoted = append(quoted, sqlIdentifier(column))
			}
			fmt.Fprintf(&out, "CREATE UNIQUE INDEX %s ON public.%s (%s);\n", sqlIdentifier(sqlName(append([]string{table}, append(columns, "key")...)...)),
				sqlIdentifier(table), strings.Join(quoted, ", "))
		}
	}
	return out.String(), nil
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
			columns = append(columns, f.Name)
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
// the row-level security of the tenancy component after it. It emits no entity model and no store, so data access
// goes only through the generated commands and queries (ST-SUP-RC01-OC41). The ledger runner takes a *gorm.DB in the
// migration job; the runtime role gets the grants of ST-SUP-RC27-OC07 and ST-STD-TXN-R03.
func generateRuntimeData(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	files, err := generateTables(ctx)
	if err != nil {
		return nil, nil, err
	}
	rowSecurity, err := rowSecurityImport(ctx)
	if err != nil {
		return nil, nil, err
	}
	migrateFile, err := generateMigrate(ctx.OutputNamespace, rowSecurity)
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
	files = append(files, migrateFile, schemaFile, opener, access)
	base := path.Base(ctx.OutputNamespace)
	wiring := &gen.Wiring{
		Imports:        []string{ctx.OutputNamespace},
		NeedsDB:        true,
		DBBackend:      "gorm",
		PostDBCalls:    []string{base + ".Migrate(db)"},
		DatabaseOpener: &gen.DatabaseOpenerSpec{Namespace: ctx.OutputNamespace, Function: "OpenDatabase"},
		GoModRequires: map[string]string{
			"github.com/google/uuid":  "v1.6.0",
			"github.com/jackc/pgx/v5": "v5.11.0",
			"gorm.io/gorm":            "v1.25.12",
			"gorm.io/driver/postgres": "v1.5.11",
		},
	}
	entityAccess(ctx, wiring)
	schemaGenerationAccess(ctx, wiring)
	if value, _ := ctx.ComponentConfig["migrations"].(string); value == "external" {
		wiring.PostDBCalls = nil
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
