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
	Name       string
	Change     types.SchemaChange
	Phase      string
	Body       string
	Index      string
	Definition string
	Validate   bool
}

// stepAddition is what an add step gives: the SQL of its own migration, each index that it builds on a table with
// rows, and the statements that validate its constraints.
type stepAddition struct {
	body      strings.Builder
	indexes   []stepIndex
	validates []string
}

// stepIndex is one index of a step on a table with rows: its qualified name, its CREATE INDEX CONCURRENTLY and its
// definition as pg_get_indexdef writes it.
type stepIndex struct{ name, statement, definition string }

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
		if (change.Kind != "add" && change.Kind != "rename") || step.Phase != "expand" {
			return nil, fmt.Errorf("schema_history[%d]: the step %s of the schema change %s (%s) is not built; generation carries out only the expand step of an add and of a rename", i, step.Phase, change.ID, change.Kind)
		}
		after, err := at(i + 1)
		if err != nil {
			return nil, err
		}
		var added *stepAddition
		if change.Kind == "rename" {
			added, err = renameSQL(ctx, before, after, change)
		} else {
			added, err = addedSQL(before, after, key, security != "")
		}
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
			steps = append(steps, schemaStep{Name: IndexMigrationName(name, n+1), Change: change, Phase: step.Phase, Body: index.statement, Index: index.name, Definition: index.definition})
		}
		if len(added.validates) > 0 {
			steps = append(steps, schemaStep{Name: ValidateMigrationName(name), Change: change, Phase: step.Phase, Body: strings.Join(added.validates, ""), Validate: true})
		}
		if change.Kind == "rename" {
			// The old column stays in the table until the contract step, so the model of the next step keeps it.
			after = withColumnOf(after, before, change)
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
			out.indexes = append(out.indexes, stepIndex{name: "public." + sqlIdentifier(searchName(next.name, column)), statement: concurrently(searchIndexSQL(next, column)),
				definition: searchIndexDefinition(next, column)})
		}
	}
	for _, columns := range next.unique {
		if !slices.ContainsFunc(old.unique, func(u []string) bool { return slices.Equal(u, columns) }) {
			out.indexes = append(out.indexes, stepIndex{name: "public." + sqlIdentifier(uniqueName(next.name, columns)), statement: concurrently(uniqueIndexSQL(next, columns)),
				definition: uniqueIndexDefinition(next, columns)})
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

// SyncTriggerName gives the name of the sync trigger, and of its function in the schema StepSchema, of the expand
// step of a rename, a retype or a require that adds the column column to the table of the entity:
// <table>_<column>_sync, cut as sqlName cuts a name (ST-SUP-RC01-OC23).
func SyncTriggerName(entity, column string) string { return sqlName(tableName(entity), column, "sync") }

// BackfillLease gives the lease of the backfill of a schema change, which the controller replica that moves its
// rows holds (ST-GEN-UC13-R01).
func BackfillLease(change string) string { return "stego.backfill." + change }

// StepTrigger and StepBackfill give the sync trigger and the backfill lease that the step of a change at expand
// adds, or empty when the kind adds none: only a rename adds them yet (ST-GEN-UC12-R01).
func StepTrigger(change types.SchemaChange, phase string) string {
	if change.Kind == "rename" && phase == "expand" {
		return SyncTriggerName(change.Entity, change.Field)
	}
	return ""
}

// StepBackfill gives the backfill lease of the step, or empty.
func StepBackfill(change types.SchemaChange, phase string) string {
	if change.Kind == "rename" && phase == "expand" {
		return BackfillLease(change.ID)
	}
	return ""
}

// syncTemplate is version 1 of the sync function template of the runtime library
// (registry/runtime/manifest.SyncSource, ST-SUP-RP15-LC02): %[1]s is K and %[2]s is U, as quote_ident gives them,
// %[3]s the conversion of NEW.K to the type of U and %[4]s the conversion of NEW.U to the type of K, %[5]s the change
// ID and %[6]s the backfill role, each as a string literal. The compiler does not depend on the runtime module, so
// this is a copy: the release before a step accepts the trigger only when its source is byte for byte the runtime's
// text. TestSyncTemplateCopy compares the two texts, and the generated test of a rename proves the release before
// accepts the trigger with the runtime's Check (ST-GEN-UC12-R03).
const syncTemplate = `
BEGIN
  IF pg_catalog.current_setting('stego.backfill_change', true) = %[5]s AND CURRENT_USER = %[6]s THEN
    RETURN NEW;
  END IF;
  IF pg_catalog.strpos(',' || pg_catalog.current_setting('stego.schema_step', true) || ',', ',' || %[5]s || ',') > 0 THEN
    IF TG_OP = 'INSERT' THEN
      NEW.%[1]s := %[4]s;
    ELSIF NEW.%[1]s IS DISTINCT FROM OLD.%[1]s OR NEW.%[2]s IS DISTINCT FROM OLD.%[2]s THEN
      IF NEW.%[1]s IS DISTINCT FROM OLD.%[1]s AND NEW.%[2]s IS DISTINCT FROM OLD.%[2]s AND NEW.%[1]s IS DISTINCT FROM %[4]s THEN
        RAISE EXCEPTION USING ERRCODE = '23514', MESSAGE = 'stego sync trigger: the write sets the old and the new column to values that differ';
      END IF;
      NEW.%[1]s := %[4]s;
    END IF;
  ELSIF TG_OP = 'INSERT' THEN
    IF NEW.%[2]s IS NOT NULL AND NEW.%[2]s IS DISTINCT FROM %[3]s THEN
      RAISE EXCEPTION USING ERRCODE = '23514', MESSAGE = 'stego sync trigger: the write sets the old and the new column to values that differ';
    END IF;
    NEW.%[2]s := %[3]s;
  ELSIF NEW.%[1]s IS DISTINCT FROM OLD.%[1]s OR NEW.%[2]s IS DISTINCT FROM OLD.%[2]s THEN
    IF NEW.%[1]s IS DISTINCT FROM OLD.%[1]s AND NEW.%[2]s IS DISTINCT FROM OLD.%[2]s AND NEW.%[2]s IS DISTINCT FROM %[3]s THEN
      RAISE EXCEPTION USING ERRCODE = '23514', MESSAGE = 'stego sync trigger: the write sets the old and the new column to values that differ';
    END IF;
    NEW.%[2]s := %[3]s;
  END IF;
  RETURN NEW;
END;
`

// syncSource gives the source of the sync function of a rename of k to u, with the identity conversion, for the
// change and the backfill role owner, as registry/runtime/manifest.SyncSource gives it for version 1.
func syncSource(change, owner, k, u string) string {
	qk, qu := deparsedName(k), deparsedName(u)
	return fmt.Sprintf(syncTemplate, qk, qu, "NEW."+qk, "NEW."+qu, sqlString(change), sqlString(owner))
}

// syncFunction gives the CREATE FUNCTION statement of the sync function StepSchema.<function>, as
// registry/runtime/manifest.SyncFunction gives it.
func syncFunction(function, source string) string {
	return "CREATE FUNCTION " + deparsedName(StepSchema) + "." + deparsedName(function) +
		"() RETURNS trigger LANGUAGE plpgsql SECURITY INVOKER VOLATILE SET search_path = pg_catalog, pg_temp AS $stego_sync$" +
		source + "$stego_sync$"
}

// backfillsTable is the table of the backfill checkpoints of the runtime library
// (registry/runtime/lease.BackfillsTable, ST-SUP-RC01-OC25), one row for each schema change, and backfillFunctions
// are its two functions (registry/runtime/lease.BackfillFunctions); copies, as the template is, which
// TestSyncTemplateCopy compares with the runtime's text.
const backfillsTable = `CREATE TABLE IF NOT EXISTS stego_schema.backfills (
	change_id text PRIMARY KEY,
	last_key bytea,
	last_key_type text,
	rows_moved bigint NOT NULL DEFAULT 0,
	failed_rows bigint NOT NULL DEFAULT 0,
	duplicate_groups bigint NOT NULL DEFAULT 0,
	fill_digest text NOT NULL DEFAULT '',
	token bigint NOT NULL DEFAULT 0,
	completed_at timestamptz
)`

const backfillFunctions = `CREATE OR REPLACE FUNCTION stego_schema.backfill_read(text)
 RETURNS TABLE (last_key bytea, last_key_type text, rows_moved bigint, failed_rows bigint, duplicate_groups bigint, fill_digest text, token bigint, completed boolean)
 LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
 AS $stego_backfill$SELECT b.last_key, b.last_key_type, b.rows_moved, b.failed_rows, b.duplicate_groups, b.fill_digest, b.token, b.completed_at IS NOT NULL
  FROM stego_schema.backfills b WHERE b.change_id = $1 FOR UPDATE$stego_backfill$;
CREATE OR REPLACE FUNCTION stego_schema.backfill_checkpoint(text, bytea, bigint, bigint, bigint, text, boolean, bigint, text)
 RETURNS bigint LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
 AS $stego_backfill$WITH changed AS (UPDATE stego_schema.backfills b SET last_key = $2, last_key_type = $9, rows_moved = $3, failed_rows = $4,
  duplicate_groups = $5, fill_digest = $6, completed_at = CASE WHEN $7 THEN coalesce(b.completed_at, pg_catalog.now()) END, token = $8
  WHERE b.change_id = $1 AND b.token <= $8 RETURNING 1) SELECT pg_catalog.count(*) FROM changed$stego_backfill$;
`

// renameSQL gives the migrations of the expand step of a rename (ST-GEN-UC12-R02, ST-GEN-UC12-R03,
// ST-SUP-RC01-OC23, ST-SUP-RC01-OC25): its own migration adds the new column U nullable and with no default, and the
// CHECK of its field after the change NOT VALID, which a later migration validates; it changes nothing that is
// there, so the old column K keeps its type, NOT NULL, CHECK and indexes. It makes the schema StepSchema, the sync
// function of the fixed template with the identity, which the release before accepts by its text
// (ST-SUP-RP15-LC02), and the BEFORE row trigger on INSERT OR UPDATE that runs it. Then it makes the table of the
// backfill checkpoints, its two SECURITY DEFINER functions, which alone change a row for a role other than the
// owner, and the row of the change. The search index and the unique indexes of U are built CONCURRENTLY by
// migrations of their own (decision on finding F2, orchestrator under the owner's delegation, 2026-11-13). The
// migration job runs it as the owner role, which owns each object it makes.
func renameSQL(ctx gen.Context, before, after []tableModel, change types.SchemaChange) (*stepAddition, error) {
	runtime, owner, err := serviceRoles(ctx)
	if err != nil {
		return nil, err
	}
	name := tableName(change.Entity)
	old, okOld := findTable(before, name)
	next, okNext := findTable(after, name)
	if !okOld || !okNext {
		return nil, fmt.Errorf("the rename names the entity %s, whose table is not there", change.Entity)
	}
	k, okK := findColumn(old, change.From)
	u, okU := findColumn(next, change.Field)
	if !okK || !okU {
		return nil, fmt.Errorf("the rename of %s.%s to %s finds no column %s before the step or %s after it", change.Entity, change.From, change.Field, change.From, change.Field)
	}
	if _, clash := findColumn(old, change.Field); clash {
		return nil, fmt.Errorf("the table %s has a column %s already, so the rename cannot add it", name, change.Field)
	}
	// The schema check of the release before accepts the sync trigger only for a column K that is in no unique key
	// (ST-SUP-RP15-LC02 rule 2); whether a rename of a unique field may keep its unique key is question ST-GEN-Q18.
	for _, columns := range old.unique {
		if slices.Contains(columns, change.From) {
			return nil, fmt.Errorf("the rename of %s.%s is of a field in a unique key, whose sync trigger the schema check of the release before refuses (ST-SUP-RP15-LC02); a rename of a unique field waits for ST-GEN-Q18", change.Entity, change.From)
		}
	}
	for _, fk := range old.foreign {
		if slices.Contains(fk.from, change.From) {
			return nil, fmt.Errorf("the rename of %s.%s is of a reference, whose foreign key a rename does not carry yet", change.Entity, change.From)
		}
	}
	if k.typ != u.typ {
		return nil, fmt.Errorf("the rename of %s.%s changes the column type from %s to %s; a rename keeps the type, and a retype changes it", change.Entity, change.From, k.typ, u.typ)
	}
	trigger := SyncTriggerName(change.Entity, change.Field)
	if !validTriggerName(trigger) {
		return nil, fmt.Errorf("the sync trigger name %s is not a name the schema check accepts", trigger)
	}
	table := "public." + sqlIdentifier(name)
	out := &stepAddition{}
	fmt.Fprintf(&out.body, "ALTER TABLE %s ADD COLUMN %s %s;\n", table, sqlIdentifier(u.name), u.typ)
	for _, check := range next.checks {
		if check.name == sqlName(name, change.Field, "check") {
			fmt.Fprintf(&out.body, "ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s) NOT VALID;\n", table, sqlIdentifier(check.name), check.expression)
			out.validates = append(out.validates, fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s;\n", table, sqlIdentifier(check.name)))
		}
	}
	fmt.Fprintf(&out.body, "CREATE SCHEMA IF NOT EXISTS %s;\n", StepSchema)
	out.body.WriteString(syncFunction(trigger, syncSource(change.ID, owner, change.From, change.Field)) + ";\n")
	fmt.Fprintf(&out.body, "CREATE TRIGGER %s BEFORE INSERT OR UPDATE ON %s FOR EACH ROW EXECUTE FUNCTION %s.%s();\n",
		sqlIdentifier(trigger), table, StepSchema, sqlIdentifier(trigger))
	out.body.WriteString(backfillsTable + ";\n" + backfillFunctions)
	for _, function := range backfillFunctionSignatures {
		fmt.Fprintf(&out.body, "REVOKE ALL ON FUNCTION %s FROM PUBLIC;\nGRANT EXECUTE ON FUNCTION %s TO %s;\n", function, function, sqlIdentifier(runtime))
	}
	fmt.Fprintf(&out.body, "GRANT USAGE ON SCHEMA stego_schema TO %s;\n", sqlIdentifier(runtime))
	fmt.Fprintf(&out.body, "INSERT INTO stego_schema.backfills (change_id) VALUES (%s) ON CONFLICT (change_id) DO NOTHING;\n", sqlString(change.ID))
	if slices.Contains(next.search, change.Field) {
		out.indexes = append(out.indexes, stepIndex{name: "public." + sqlIdentifier(searchName(next.name, change.Field)),
			statement: concurrently(searchIndexSQL(next, change.Field)), definition: searchIndexDefinition(next, change.Field)})
	}
	for _, columns := range next.unique {
		if slices.Contains(columns, change.Field) {
			out.indexes = append(out.indexes, stepIndex{name: "public." + sqlIdentifier(uniqueName(next.name, columns)),
				statement: concurrently(uniqueIndexSQL(next, columns)), definition: uniqueIndexDefinition(next, columns)})
		}
	}
	return out, nil
}

// backfillFunctionSignatures are the two functions of backfillFunctions with their argument types, as a GRANT names
// them.
var backfillFunctionSignatures = []string{
	"stego_schema.backfill_read(text)",
	"stego_schema.backfill_checkpoint(text, bytea, bigint, bigint, bigint, text, boolean, bigint, text)",
}

// validTriggerName tells whether a trigger name has 63 bytes at most and no control character.
func validTriggerName(name string) bool {
	return len(name) > 0 && len(name) <= 63 && !strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 })
}

// withColumnOf gives the tables after a rename with the old column of the change, its CHECK and its indexes from
// the tables before, which stay in the database until the contract step.
func withColumnOf(after, before []tableModel, change types.SchemaChange) []tableModel {
	name := tableName(change.Entity)
	old, _ := findTable(before, name)
	out := slices.Clone(after)
	for i := range out {
		if out[i].name != name {
			continue
		}
		t := out[i]
		t.columns = slices.Clone(t.columns)
		if k, ok := findColumn(old, change.From); ok {
			t.columns = append(t.columns, k)
		}
		for _, check := range old.checks {
			if check.name == sqlName(name, change.From, "check") {
				t.checks = append(slices.Clone(t.checks), check)
			}
		}
		if slices.Contains(old.search, change.From) {
			t.search = append(slices.Clone(t.search), change.From)
		}
		for _, columns := range old.unique {
			if slices.Contains(columns, change.From) {
				t.unique = append(slices.Clone(t.unique), columns)
			}
		}
		out[i] = t
	}
	return out
}

// findTable gives the table of the name among tables.
func findTable(tables []tableModel, name string) (tableModel, bool) {
	i := slices.IndexFunc(tables, func(t tableModel) bool { return t.name == name })
	if i < 0 {
		return tableModel{}, false
	}
	return tables[i], true
}

// findColumn gives the column of the name in the table.
func findColumn(table tableModel, name string) (columnModel, bool) {
	i := slices.IndexFunc(table.columns, func(c columnModel) bool { return c.name == name })
	if i < 0 {
		return columnModel{}, false
	}
	return table.columns[i], true
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
