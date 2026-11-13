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

// schemaStep is one migration of a step of the schema history: its ledger name, its change, its phase and its SQL
// body. A step on a table with rows has more than one: its own migration, which adds each CHECK and foreign key NOT
// VALID, so that it scans no row under its strong lock; then, for an index, Index, the qualified name of the index,
// which the migration builds CONCURRENTLY outside a transaction (ST-SUP-RC01-OC38); then, when Validate, the
// migration that validates those constraints in its own transaction (ST-SUP-RC01-OC22, decision on finding F2,
// orchestrator under the owner's delegation, 2026-11-13).
type schemaStep struct {
	Name     string
	Change   types.SchemaChange
	Phase    string
	Body     string
	Index    string
	Validate bool
}

// stepAddition is what an add step gives: the SQL of its own migration, each index that it builds on a table with
// rows, and the statements that validate its constraints.
type stepAddition struct {
	body      strings.Builder
	indexes   []stepIndex
	validates []string
}

// stepIndex is one index of a step on a table with rows: its qualified name and its CREATE INDEX CONCURRENTLY.
type stepIndex struct{ name, statement string }

// IndexMigrationName and ValidateMigrationName give the ledger names of the migrations after the migration name of
// a step: the build of its index number n from 1, in two digits, and the validation of its constraints. They sort
// after name and before the next step.
func IndexMigrationName(name string, n int) string { return fmt.Sprintf("%s_index_%02d", name, n) }

// ValidateMigrationName gives the ledger name of the migration that validates the constraints of the step name.
func ValidateMigrationName(name string) string { return name + "_validate" }

// maxStepIndexes is the most indexes that the two digits of an index migration name number.
const maxStepIndexes = 99

// StepMigrationName gives the ledger name of the migration of a step: step_<position>_<change ID>_<phase>, with the
// position from 1 in four digits (ST-SUP-RC01-OC22).
func StepMigrationName(position int, change, phase string) string {
	return fmt.Sprintf("step_%04d_%s_%s", position, change, phase)
}

// stepName is the grammar of the ledger name of a step migration.
var stepName = regexp.MustCompile(`^step_[0-9]{4}_[a-z0-9]+(-[a-z0-9]+)*_(expand|contract)(_index_[0-9]{2}|_validate)?$`)

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
		added, err := addedSQL(before, after, key, security != "")
		if err != nil {
			return nil, fmt.Errorf("schema change %s: %w", change.ID, err)
		}
		if added.body.Len() == 0 && len(added.indexes) == 0 {
			return nil, fmt.Errorf("schema change %s adds nothing to the tables", change.ID)
		}
		if len(added.indexes) > maxStepIndexes {
			return nil, fmt.Errorf("schema change %s builds %d indexes; at most %d are allowed", change.ID, len(added.indexes), maxStepIndexes)
		}
		name := StepMigrationName(i+1, change.ID, step.Phase)
		steps = append(steps, schemaStep{Name: name, Change: change, Phase: step.Phase, Body: added.body.String()})
		for n, index := range added.indexes {
			steps = append(steps, schemaStep{Name: IndexMigrationName(name, n+1), Change: change, Phase: step.Phase, Body: index.statement, Index: index.name})
		}
		if len(added.validates) > 0 {
			steps = append(steps, schemaStep{Name: ValidateMigrationName(name), Change: change, Phase: step.Phase, Body: strings.Join(added.validates, ""), Validate: true})
		}
		before = after
	}
	return steps, nil
}

// addedSQL gives the SQL that takes the tables before to the tables after, which may only add: a new table with its
// indexes and, when rowSecurity, the row-level security of an owned table; and on a table that is there, a column
// that is nullable or has a default, a CHECK, a foreign key and an index. It refuses every other difference.
func addedSQL(before, after []tableModel, key string, rowSecurity bool) (*stepAddition, error) {
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
			return nil, fmt.Errorf("the step removes the table %s, and an add step only adds", table.name)
		}
	}
	out := &stepAddition{}
	for _, table := range after {
		for _, fk := range table.foreign {
			if !known[fk.table] {
				return nil, fmt.Errorf("the table %s refers to the table %s, which a later step adds; put the step that adds %s first", table.name, fk.table, fk.table)
			}
		}
		old, there := present[table.name]
		if !there {
			out.body.WriteString(createTableSQL(table))
			if rowSecurity && table.owned {
				out.body.WriteString(TenantRowSecurity(table.entity, key))
			}
			continue
		}
		if err := addedToTable(old, table, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// addedToTable adds to out what takes the table old, which has rows, to the table next: each new column, then each
// new CHECK and foreign key NOT VALID, so that the step scans no row while it holds its lock, and a statement that
// validates each of them later in its own transaction; and each new search index and unique index, built
// CONCURRENTLY by a migration of its own (decision on finding F2). It refuses a change or a removal of what old
// holds, and a new column that is NOT NULL and has no default, which the rows there cannot take.
func addedToTable(old, next tableModel, out *stepAddition) error {
	if old.owned != next.owned || !slices.Equal(old.lead, next.lead) {
		return fmt.Errorf("the step changes the tenant column of the table %s, and an add step only adds", old.name)
	}
	table := "public." + sqlIdentifier(next.name)
	columns := map[string]columnModel{}
	for _, column := range next.columns {
		columns[column.name] = column
	}
	for _, column := range old.columns {
		if got, ok := columns[column.name]; !ok || got != column {
			return fmt.Errorf("the step changes or removes the column %s.%s, and an add step only adds", old.name, column.name)
		}
	}
	for _, column := range next.columns {
		if slices.ContainsFunc(old.columns, func(c columnModel) bool { return c.name == column.name }) {
			continue
		}
		if column.notNull && column.defaultClause == "" {
			return fmt.Errorf("the step adds the column %s.%s as NOT NULL with no default, which the rows there cannot take; make the field optional or give it a default", next.name, column.name)
		}
		fmt.Fprintf(&out.body, "ALTER TABLE %s ADD COLUMN %s;\n", table, columnSQL(column))
	}
	if !subset(old.checks, next.checks) || !subset(old.foreign, next.foreign) || !subset(old.search, next.search) || !subset(old.unique, next.unique) {
		return fmt.Errorf("the step changes or removes a constraint or an index of the table %s, and an add step only adds", old.name)
	}
	validate := func(name string) {
		out.validates = append(out.validates, fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s;\n", table, sqlIdentifier(name)))
	}
	for _, check := range next.checks {
		if !slices.Contains(old.checks, check) {
			fmt.Fprintf(&out.body, "ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s) NOT VALID;\n", table, sqlIdentifier(check.name), check.expression)
			validate(check.name)
		}
	}
	for _, fk := range next.foreign {
		if !slices.ContainsFunc(old.foreign, func(f foreignModel) bool { return reflect.DeepEqual(f, fk) }) {
			fmt.Fprintf(&out.body, "ALTER TABLE %s ADD CONSTRAINT %s %s NOT VALID;\n", table, sqlIdentifier(fk.name), foreignSQL(fk))
			validate(fk.name)
		}
	}
	for _, column := range next.search {
		if !slices.Contains(old.search, column) {
			out.indexes = append(out.indexes, stepIndex{name: "public." + sqlIdentifier(searchName(next.name, column)), statement: concurrently(searchIndexSQL(next, column))})
		}
	}
	for _, columns := range next.unique {
		if !slices.ContainsFunc(old.unique, func(u []string) bool { return slices.Equal(u, columns) }) {
			out.indexes = append(out.indexes, stepIndex{name: "public." + sqlIdentifier(uniqueName(next.name, columns)), statement: concurrently(uniqueIndexSQL(next, columns))})
		}
	}
	return nil
}

// concurrently gives the CREATE INDEX or CREATE UNIQUE INDEX statement as CREATE INDEX CONCURRENTLY IF NOT EXISTS, so
// that a rerun after a build that succeeded and wrote no ledger row recovers.
func concurrently(statement string) string {
	for _, head := range []string{"CREATE INDEX ", "CREATE UNIQUE INDEX "} {
		if strings.HasPrefix(statement, head) {
			return head + "CONCURRENTLY IF NOT EXISTS " + strings.TrimPrefix(statement, head)
		}
	}
	return statement
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
