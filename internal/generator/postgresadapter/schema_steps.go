package postgresadapter

import (
	"fmt"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

// The schema steps of the runtime data path (ST-SUP-RC01-OC22): 001_initial is the baseline, the tables of the
// first release, and each step of the schema history is one SQL migration after the component's own migrations,
// named step_<position>_<change ID>_<phase>, from the declaration alone. A database that the migrations of each
// release made in turn and a new one that the migrations of the last release made get the same schema
// (ST-GEN-INV35). Generation carries out the add kind only: its step at expand creates the table of a new entity,
// with its row-level security in a pooled service, or adds the column of a new field, and changes nothing that is
// there (ST-GEN-UC12-R02).

// maxSchemaSteps is the most steps that the four digits of a step position name, so the byte order of the names
// is the order of the steps.
const maxSchemaSteps = 9999

// schemaStep is the migration of one step of the schema history: its ledger name, its change, its phase and its
// SQL body.
type schemaStep struct {
	Name   string
	Change types.SchemaChange
	Phase  string
	Body   string
}

// StepMigrationName gives the ledger name of the migration of a step: step_<position>_<change ID>_<phase>, with the
// position from 1 in four digits (ST-SUP-RC01-OC22).
func StepMigrationName(position int, change, phase string) string {
	return fmt.Sprintf("step_%04d_%s_%s", position, change, phase)
}

// stepName is the grammar of the ledger name of a step migration.
var stepName = regexp.MustCompile(`^step_[0-9]{4}_[a-z0-9]+(-[a-z0-9]+)*_(expand|contract)$`)

// CheckLedgerName refuses a ledgered migration whose name is not 001_initial, 100_tenancy_row_security or the name of
// a schema step (ST-SUP-RC01-OC43): the ledger applies and checks migrations in the byte order of their names, so a
// component migration that a later release adds, such as 101_x, would sort before the step rows that an upgraded
// database holds, and that database would fail the ledger check. Such a migration waits for its own schema step.
func CheckLedgerName(name string) error {
	if name == "001_initial" || name == tenancyMigration || stepName.MatchString(name) {
		return nil
	}
	return fmt.Errorf("the ledgered migration %q is not 001_initial, %s or a schema step; a component migration of a later release would sort before the step rows of an upgraded ledger (ST-SUP-RC01-OC43)", name, tenancyMigration)
}

// registration finds the name of each migration that a generated file registers in the ledger.
var registration = regexp.MustCompile(`\bRegister(?:SQL|Step)?\(\s*"([^"]*)"`)

// checkLedgerNames refuses a file of the namespace that registers a ledgered migration that CheckLedgerName
// refuses, so that no generated file, in any place, adds one.
func checkLedgerNames(ns string, files []gen.File) error {
	for _, file := range files {
		if path.Dir(file.Path) != ns || path.Ext(file.Path) != ".go" {
			continue
		}
		for _, match := range registration.FindAllSubmatch(file.Content, -1) {
			if err := CheckLedgerName(string(match[1])); err != nil {
				return fmt.Errorf("%s: %w", file.Path, err)
			}
		}
	}
	return nil
}

// tenancyMigration is the ledger name of the row-level security of the tenancy component.
const tenancyMigration = "100_tenancy_row_security"

// StepMigrationFile gives the path, in the output namespace of the adapter, of the SQL file of a step migration.
func StepMigrationFile(ns, name string) string { return path.Join(ns, "migrations", name+".sql") }

// schemaSteps gives the migration of each step of the schema history, in its order: the SQL that takes the tables
// of the shape before the step to the shape after it (gen.Context.AtSchemaStep). It refuses a step that is not the
// expand step of an add, an add that changes or removes what is there, a column that is NOT NULL and has no
// default, and a foreign key to a table that a later step adds.
func schemaSteps(ctx gen.Context) ([]schemaStep, error) {
	if len(ctx.SchemaHistory) > maxSchemaSteps {
		return nil, fmt.Errorf("the schema history has %d steps; at most %d steps are allowed", len(ctx.SchemaHistory), maxSchemaSteps)
	}
	changes := map[string]types.SchemaChange{}
	for _, change := range ctx.SchemaChanges {
		changes[change.ID] = change
	}
	at := func(steps int) ([]tableModel, error) {
		shaped, err := ctx.AtSchemaStep(steps)
		if err != nil {
			return nil, err
		}
		return tableModels(shaped)
	}
	before, err := at(0)
	if err != nil {
		return nil, err
	}
	key := tenantColumn(ctx)
	security, err := rowSecurityImport(ctx)
	if err != nil {
		return nil, err
	}
	var steps []schemaStep
	for i, step := range ctx.SchemaHistory {
		change, ok := changes[step.Change]
		if !ok {
			return nil, fmt.Errorf("schema_history[%d] names the schema change %q, which schema_changes does not declare", i, step.Change)
		}
		if change.Kind != "add" || step.Phase != "expand" {
			return nil, fmt.Errorf("schema_history[%d]: the step %s of the schema change %s (%s) is not built; generation carries out only the expand step of an add", i, step.Phase, change.ID, change.Kind)
		}
		after, err := at(i + 1)
		if err != nil {
			return nil, err
		}
		body, err := addedSQL(before, after, key, security != "")
		if err != nil {
			return nil, fmt.Errorf("schema change %s: %w", change.ID, err)
		}
		if body == "" {
			return nil, fmt.Errorf("schema change %s adds nothing to the tables", change.ID)
		}
		steps = append(steps, schemaStep{Name: StepMigrationName(i+1, change.ID, step.Phase), Change: change, Phase: step.Phase, Body: body})
		before = after
	}
	return steps, nil
}

// addedSQL gives the SQL that takes the tables before to the tables after, which may only add: a new table with its
// indexes and, when rowSecurity, the row-level security of an owned table; and on a table that is there, a column
// that is nullable or has a default, a CHECK, a foreign key and an index. It refuses every other difference.
func addedSQL(before, after []tableModel, key string, rowSecurity bool) (string, error) {
	present := map[string]tableModel{}
	for _, table := range before {
		present[table.name] = table
	}
	known := map[string]bool{}
	for _, table := range after {
		known[table.name] = true
	}
	for _, table := range before {
		if !known[table.name] {
			return "", fmt.Errorf("the step removes the table %s, and an add step only adds", table.name)
		}
	}
	var out strings.Builder
	for _, table := range after {
		for _, fk := range table.foreign {
			if !known[fk.table] {
				return "", fmt.Errorf("the table %s refers to the table %s, which a later step adds; put the step that adds %s first", table.name, fk.table, fk.table)
			}
		}
		old, there := present[table.name]
		if !there {
			out.WriteString(createTableSQL(table))
			if rowSecurity && table.owned {
				out.WriteString(TenantRowSecurity(table.entity, key))
			}
			continue
		}
		text, err := addedToTable(old, table)
		if err != nil {
			return "", err
		}
		out.WriteString(text)
	}
	return out.String(), nil
}

// addedToTable gives the statements that add to the table old what the table next holds in addition: each new
// column, CHECK, foreign key, search index and unique index, in that order. It refuses a change or a removal of
// what old holds, and a new column that is NOT NULL and has no default, which the rows there cannot take.
func addedToTable(old, next tableModel) (string, error) {
	if old.owned != next.owned || !slices.Equal(old.lead, next.lead) {
		return "", fmt.Errorf("the step changes the tenant column of the table %s, and an add step only adds", old.name)
	}
	table := "public." + sqlIdentifier(next.name)
	var out strings.Builder
	columns := map[string]columnModel{}
	for _, column := range next.columns {
		columns[column.name] = column
	}
	for _, column := range old.columns {
		if got, ok := columns[column.name]; !ok || got != column {
			return "", fmt.Errorf("the step changes or removes the column %s.%s, and an add step only adds", old.name, column.name)
		}
	}
	for _, column := range next.columns {
		if slices.ContainsFunc(old.columns, func(c columnModel) bool { return c.name == column.name }) {
			continue
		}
		if column.notNull && column.defaultClause == "" {
			return "", fmt.Errorf("the step adds the column %s.%s as NOT NULL with no default, which the rows there cannot take; make the field optional or give it a default", next.name, column.name)
		}
		fmt.Fprintf(&out, "ALTER TABLE %s ADD COLUMN %s;\n", table, columnSQL(column))
	}
	if !subset(old.checks, next.checks) || !subset(old.foreign, next.foreign) || !subset(old.search, next.search) || !subset(old.unique, next.unique) {
		return "", fmt.Errorf("the step changes or removes a constraint or an index of the table %s, and an add step only adds", old.name)
	}
	for _, check := range next.checks {
		if !slices.Contains(old.checks, check) {
			fmt.Fprintf(&out, "ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s);\n", table, sqlIdentifier(check.name), check.expression)
		}
	}
	for _, fk := range next.foreign {
		if !slices.ContainsFunc(old.foreign, func(f foreignModel) bool { return reflect.DeepEqual(f, fk) }) {
			fmt.Fprintf(&out, "ALTER TABLE %s ADD CONSTRAINT %s %s;\n", table, sqlIdentifier(fk.name), foreignSQL(fk))
		}
	}
	for _, column := range next.search {
		if !slices.Contains(old.search, column) {
			out.WriteString(searchIndexSQL(next, column))
		}
	}
	for _, columns := range next.unique {
		if !slices.ContainsFunc(old.unique, func(u []string) bool { return slices.Equal(u, columns) }) {
			out.WriteString(uniqueIndexSQL(next, columns))
		}
	}
	return out.String(), nil
}

// subset tells whether each item of old is in new.
func subset[T any](old, new []T) bool {
	for _, item := range old {
		if !slices.ContainsFunc(new, func(n T) bool { return reflect.DeepEqual(n, item) }) {
			return false
		}
	}
	return true
}

// generateSteps gives the SQL file of each step migration, for a reader and for an operator who runs the migrations
// outside the service; migrate.go registers the same text.
func generateSteps(ns string, steps []schemaStep) []gen.File {
	var files []gen.File
	for _, step := range steps {
		files = append(files, gen.File{Path: StepMigrationFile(ns, step.Name), Content: []byte(step.Body)})
	}
	return files
}
