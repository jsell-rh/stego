// Package manifest is the schema manifest of the runtime library (ST-SUP-RP15): the tables, columns, constraints,
// unique indexes, row-level security policies and triggers that the code of a service needs in its database, and
// Check, which reads the catalog of the database and refuses one that does not hold them (ST-SUP-RC01-OC12).
//
// It is a library, not a framework (ST-STD-RUNTIME-R01): generated code gives the manifest of its declaration,
// and the package calls back into no generated code.
package manifest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Manifest is what the code of a service needs in its database. A database may hold more: Check refuses only
// what is missing or different, and a permissive policy that a table with row-level security does not name.
type Manifest struct {
	Tables []Table
	// Functions are the functions that the code calls and that are no trigger function, such as the SECURITY
	// DEFINER function audit_maintain of the audit record: each takes no argument, and its language, source,
	// SECURITY DEFINER and settings must be those of the manifest, and its owner Owner.
	Functions []Function
	// Owner is the role that must own each table and each trigger function of the manifest: the owner role of the
	// service, which the migration job runs as. Empty leaves the owner of the table unchecked.
	Owner string
}

// Table is one table that the code reads or writes, in its schema.
type Table struct {
	Schema, Name string
	// Partitioned tells that the table is partitioned, as the audit events table is; its partitions may inherit
	// from it. Every other table of the manifest is a plain table that is no partition, inherits from no table, and
	// that no table inherits from.
	Partitioned bool
	// Columns are the columns that the code reads or writes, each with its type and its NOT NULL.
	Columns []Column
	// Constraints are the primary key, check and foreign key constraints of the table, by name.
	Constraints []Constraint
	// Indexes are the unique indexes that the code relies on: the primary key and each unique and upsert key.
	Indexes []Index
	// RowSecurity tells that row-level security is enabled and forced on the table. Then each permissive policy
	// of the table is one of Policies.
	RowSecurity bool
	// Policies are the row-level security policies of the table.
	Policies []Policy
	// Triggers are the triggers of the table.
	Triggers []Trigger
	// PartitionTriggers are the triggers that each partition of a partitioned table has of its own, each named by
	// the partition's name and the trigger's Name as a suffix, such as audit_event_p202611 and _reject_statement.
	PartitionTriggers []Trigger
}

// Column is one column: its name, its type as format_type gives it, such as character varying(255), and whether
// the code needs it NOT NULL. A column that the code takes as nullable may be NOT NULL in the database. A column of a
// type with a collation must have the default or the C collation, so equality is the byte equality that the tenant
// wall and the keys rely on.
type Column struct {
	Name, Type string
	NotNull    bool
}

// The kinds of a constraint, as pg_constraint.contype gives them.
const (
	KindPrimaryKey = "p"
	KindCheck      = "c"
	KindForeignKey = "f"
	KindUnique     = "u"
)

// Constraint is one constraint of a table, by name: its kind and its definition as pg_get_constraintdef gives it
// with the search path pg_catalog, pg_temp, such as CHECK ((total >= 1)) or FOREIGN KEY (tenant_id, org_id)
// REFERENCES public.organizations(tenant_id, id). The definition holds the expression of a check, and the columns,
// the referenced table and columns, the actions, the match type and the deferrability of a foreign key. The
// constraint must be validated.
type Constraint struct {
	Name, Kind, Definition string
}

// Index is one unique index of a table: its key columns in order and its predicate as pg_get_expr gives it, or
// empty for an index on every row. The index must be valid, ready, not deferrable, treat nulls as distinct, and
// have no expression column, and each key column must have the default operator class and the default or the C
// collation.
type Index struct {
	Name      string
	Columns   []string
	Predicate string
}

// Policy is one permissive row-level security policy for every role: its command (* for all, or r, a, w and d
// as pg_policy.polcmd gives them) and its USING and WITH CHECK expressions as pg_get_expr gives them with the
// search path pg_catalog.
type Policy struct {
	Name, Command, Using, WithCheck string
}

// Trigger is one trigger of a table: when it fires (BEFORE or AFTER), for each row or for each statement, on
// which events (INSERT, UPDATE, DELETE, TRUNCATE), and the function it runs. The trigger must be enabled, with no
// WHEN condition, no column list and no argument. On a table with row-level security, each trigger that is not
// internal is one of the manifest.
type Trigger struct {
	Name, Timing string
	Row          bool
	Events       []string
	Function     Function
}

// Function is the trigger function that the DDL makes: its name as schema.function, its language, its source (the
// text between the dollar quotes), whether it is SECURITY DEFINER, and its settings as pg_proc.proconfig gives
// them, such as search_path=pg_catalog, pg_temp. The function must return trigger, take no argument, be volatile,
// and belong to the owner of the table.
type Function struct {
	Name, Language, Source string
	SecurityDefiner        bool
	Config                 []string
}

// Join gives one manifest with the tables of each manifest, in order.
func Join(manifests ...Manifest) Manifest {
	var joined Manifest
	for _, m := range manifests {
		joined.Tables = append(joined.Tables, m.Tables...)
		joined.Functions = append(joined.Functions, m.Functions...)
		if joined.Owner == "" {
			joined.Owner = m.Owner
		}
	}
	return joined
}

// name is the grammar of a name of the catalog in the manifest: what PostgreSQL keeps of a name, with no control
// character.
var name = regexp.MustCompile(`^[^\x00-\x1f]{1,63}$`)

// Validate refuses a manifest with a missing or long name, a table named twice, an object named twice in its
// table, an unknown constraint kind, policy command, trigger timing or event, a foreign key with no table, or an
// index with no column.
func (m Manifest) Validate() error {
	if m.Owner != "" && !name.MatchString(m.Owner) {
		return fmt.Errorf("manifest: the owner %q needs a name of at most 63 bytes", m.Owner)
	}
	functions := map[string]bool{}
	for _, f := range m.Functions {
		schema, fn, ok := strings.Cut(f.Name, ".")
		if !ok || !name.MatchString(schema) || !name.MatchString(fn) || f.Language == "" || f.Source == "" {
			return fmt.Errorf("manifest: the function %q needs a schema and a name of at most 63 bytes, a language and a source", f.Name)
		}
		if functions[f.Name] {
			return fmt.Errorf("manifest: the function %s is named twice", f.Name)
		}
		functions[f.Name] = true
	}
	tables := map[string]bool{}
	for _, t := range m.Tables {
		if !name.MatchString(t.Schema) || !name.MatchString(t.Name) {
			return fmt.Errorf("manifest: the table %q.%q needs a schema and a name of at most 63 bytes", t.Schema, t.Name)
		}
		id := t.Schema + "." + t.Name
		if tables[id] {
			return fmt.Errorf("manifest: the table %s is named twice", id)
		}
		tables[id] = true
		seen := map[string]bool{}
		named := func(kind, n string) error {
			if !name.MatchString(n) {
				return fmt.Errorf("manifest: table %s: the %s %q needs a name of at most 63 bytes", id, kind, n)
			}
			if seen[kind+"\x00"+n] {
				return fmt.Errorf("manifest: table %s: the %s %s is named twice", id, kind, n)
			}
			seen[kind+"\x00"+n] = true
			return nil
		}
		for _, c := range t.Columns {
			if err := named("column", c.Name); err != nil {
				return err
			}
			if c.Type == "" {
				return fmt.Errorf("manifest: table %s: the column %s has no type", id, c.Name)
			}
		}
		for _, c := range t.Constraints {
			if err := named("constraint", c.Name); err != nil {
				return err
			}
			if !slices.Contains([]string{KindPrimaryKey, KindCheck, KindForeignKey, KindUnique}, c.Kind) {
				return fmt.Errorf("manifest: table %s: the constraint %s has the unknown kind %q", id, c.Name, c.Kind)
			}
			if c.Definition == "" {
				return fmt.Errorf("manifest: table %s: the constraint %s has no definition", id, c.Name)
			}
		}
		for _, i := range t.Indexes {
			if err := named("index", i.Name); err != nil {
				return err
			}
			if len(i.Columns) == 0 {
				return fmt.Errorf("manifest: table %s: the index %s has no column", id, i.Name)
			}
		}
		for _, p := range t.Policies {
			if err := named("policy", p.Name); err != nil {
				return err
			}
			if !slices.Contains([]string{"*", "r", "a", "w", "d"}, p.Command) {
				return fmt.Errorf("manifest: table %s: the policy %s has the unknown command %q", id, p.Name, p.Command)
			}
		}
		for _, tr := range t.PartitionTriggers {
			if !t.Partitioned {
				return fmt.Errorf("manifest: table %s: only a partitioned table has partition triggers", id)
			}
			if _, err := tr.tgtype(); err != nil || tr.Name == "" || tr.Function.Name == "" || tr.Function.Source == "" {
				return fmt.Errorf("manifest: table %s: the partition trigger %q is not complete", id, tr.Name)
			}
		}
		for _, tr := range t.Triggers {
			if err := named("trigger", tr.Name); err != nil {
				return err
			}
			if _, err := tr.tgtype(); err != nil {
				return fmt.Errorf("manifest: table %s: the trigger %s: %w", id, tr.Name, err)
			}
			if tr.Function.Name == "" || tr.Function.Language == "" || tr.Function.Source == "" {
				return fmt.Errorf("manifest: table %s: the trigger %s has no function name, language or source", id, tr.Name)
			}
		}
	}
	return nil
}

// The bits of pg_trigger.tgtype.
const (
	triggerRow      = 1 << 0
	triggerBefore   = 1 << 1
	triggerInsert   = 1 << 2
	triggerDelete   = 1 << 3
	triggerUpdate   = 1 << 4
	triggerTruncate = 1 << 5
	triggerInstead  = 1 << 6
)

// tgtype gives the pg_trigger.tgtype of the trigger.
func (tr Trigger) tgtype() (int16, error) {
	var bits int16
	switch tr.Timing {
	case "BEFORE":
		bits |= triggerBefore
	case "AFTER":
	case "INSTEAD OF":
		bits |= triggerInstead
	default:
		return 0, fmt.Errorf("the timing %q is not BEFORE, AFTER or INSTEAD OF", tr.Timing)
	}
	if tr.Row {
		bits |= triggerRow
	}
	if len(tr.Events) == 0 {
		return 0, errors.New("it fires on no event")
	}
	for _, event := range tr.Events {
		switch event {
		case "INSERT":
			bits |= triggerInsert
		case "UPDATE":
			bits |= triggerUpdate
		case "DELETE":
			bits |= triggerDelete
		case "TRUNCATE":
			bits |= triggerTruncate
		default:
			return 0, fmt.Errorf("the event %q is not INSERT, UPDATE, DELETE or TRUNCATE", event)
		}
	}
	return bits, nil
}

// ErrNotSatisfied is the refusal of a database that does not satisfy the manifest. A *Mismatch matches it.
var ErrNotSatisfied = errors.New("the database does not satisfy the schema manifest")

// Mismatch is the refusal of Check: one fault for each missing or different object, each with the names of the
// catalog only and with no value of a row or of a setting.
type Mismatch struct {
	Faults []string
}

func (m *Mismatch) Error() string {
	return ErrNotSatisfied.Error() + ": " + strings.Join(m.Faults, "; ")
}

func (m *Mismatch) Is(target error) bool { return target == ErrNotSatisfied }

// Timeout bounds Check.
const Timeout = 30 * time.Second

// Check reads the catalog and the session_replication_role of the session in one read-only transaction on db, with the search path pg_catalog,
// pg_temp, so no temporary object stands in for a catalog object. It gives a *Mismatch that names each table,
// column, constraint, index, policy and trigger of the manifest that the database does not hold as the manifest
// states it, and each rule, extra permissive policy and extra trigger that it does not allow (ST-SUP-RC01-OC12). It reads only the catalog, so the runtime
// role of the service can run it, and it writes nothing. An invalid manifest, or a catalog it cannot read, is an
// error too, so the caller fails closed.
func Check(ctx context.Context, db *sql.DB, m Manifest) (err error) {
	if db == nil {
		return errors.New("manifest: Check needs a database")
	}
	if err := m.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("manifest: read the catalog: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "SET LOCAL search_path = pg_catalog, pg_temp"); err != nil {
		return fmt.Errorf("manifest: read the catalog: %w", err)
	}
	var faults []string
	// session_replication_role replica or local skips each ordinary trigger, so the triggers of the manifest
	// would not run in this session or in the sessions of its role.
	var replication string
	if err := tx.QueryRowContext(ctx, "SELECT pg_catalog.current_setting('session_replication_role')").Scan(&replication); err != nil {
		return fmt.Errorf("manifest: read the catalog: %w", err)
	}
	if replication != "origin" {
		faults = append(faults, "the session runs with session_replication_role "+replication+", which skips the triggers")
	}
	for _, t := range m.Tables {
		found, err := checkTable(ctx, tx, t, m.Owner)
		if err != nil {
			return fmt.Errorf("manifest: read the catalog of the table %s.%s: %w", t.Schema, t.Name, err)
		}
		faults = append(faults, found...)
	}
	for _, f := range m.Functions {
		found, err := checkFunction(ctx, tx, f, m.Owner)
		if err != nil {
			return fmt.Errorf("manifest: read the catalog of the function %s: %w", f.Name, err)
		}
		faults = append(faults, found...)
	}
	if len(faults) > 0 {
		return &Mismatch{Faults: faults}
	}
	return nil
}

// checkFunction gives the faults of one function of the manifest that takes no argument: missing, or with another
// language, source, SECURITY DEFINER, settings or owner. Its fault names the function only.
func checkFunction(ctx context.Context, tx *sql.Tx, f Function, wantOwner string) ([]string, error) {
	schema, fn, _ := strings.Cut(f.Name, ".")
	var language, source, config, owner string
	var definer bool
	err := tx.QueryRowContext(ctx, `SELECT l.lanname, p.prosrc, COALESCE(pg_catalog.array_to_string(p.proconfig, chr(31)), ''), p.prosecdef,
  pg_catalog.pg_get_userbyid(p.proowner)
  FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace JOIN pg_catalog.pg_language l ON l.oid = p.prolang
  WHERE n.nspname = $1 AND p.proname = $2 AND p.pronargs = 0`, schema, fn).Scan(&language, &source, &config, &definer, &owner)
	if errors.Is(err, sql.ErrNoRows) {
		return []string{"function " + f.Name + " is missing"}, nil
	}
	if err != nil {
		return nil, err
	}
	switch {
	case language != f.Language || source != f.Source || definer != f.SecurityDefiner || config != strings.Join(f.Config, "\x1f"):
		return []string{"function " + f.Name + " is not the function of the manifest"}, nil
	case wantOwner != "" && owner != wantOwner:
		return []string{"function " + f.Name + " has another owner"}, nil
	}
	return nil, nil
}

// checkTable gives the faults of one table.
func checkTable(ctx context.Context, tx *sql.Tx, t Table, wantOwner string) ([]string, error) {
	id := t.Schema + "." + t.Name
	var (
		oid                    uint32
		owner                  uint32
		ownerName, kind        string
		rowSecurity, forceRows bool
		partition              bool
		child, parent          bool
	)
	err := tx.QueryRowContext(ctx, `SELECT c.oid, c.relowner, pg_catalog.pg_get_userbyid(c.relowner), c.relkind::text, c.relrowsecurity,
  c.relforcerowsecurity, c.relispartition,
  EXISTS (SELECT 1 FROM pg_catalog.pg_inherits i WHERE i.inhrelid = c.oid),
  EXISTS (SELECT 1 FROM pg_catalog.pg_inherits i WHERE i.inhparent = c.oid)
  FROM pg_catalog.pg_class c
  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind IN ('r', 'p')`,
		t.Schema, t.Name).Scan(&oid, &owner, &ownerName, &kind, &rowSecurity, &forceRows, &partition, &child, &parent)
	if errors.Is(err, sql.ErrNoRows) {
		return []string{"table " + id + " is missing"}, nil
	}
	if err != nil {
		return nil, err
	}
	var faults []string
	fault := func(format string, args ...any) {
		faults = append(faults, "table "+id+": "+fmt.Sprintf(format, args...))
	}
	// A partition or a child of another table is read through its parent, which the tenant wall of the manifest
	// does not hold; a partitioned table routes its rows to partitions whose triggers the manifest does not name.
	switch {
	case kind == "p" && !t.Partitioned:
		fault("the table is partitioned, and the manifest needs a plain table")
	case kind != "p" && t.Partitioned:
		fault("the table is not partitioned, and the manifest needs a partitioned table")
	}
	if partition || child {
		fault("the table is a partition or a child of another table")
	}
	if parent && !t.Partitioned {
		fault("another table inherits from the table")
	}
	if wantOwner != "" && ownerName != wantOwner {
		fault("the table belongs to the role %s, and the manifest needs %s", ownerName, wantOwner)
	}

	type column struct {
		typ, collation string
		notNull        bool
		plainCollation bool
	}
	columns := map[string]column{}
	rows, err := tx.QueryContext(ctx, `SELECT a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod), a.attnotnull,
  a.attcollation IN (0, 100, 950), COALESCE(cn.nspname || '.' || co.collname, '')
  FROM pg_catalog.pg_attribute a LEFT JOIN pg_catalog.pg_collation co ON co.oid = a.attcollation
  LEFT JOIN pg_catalog.pg_namespace cn ON cn.oid = co.collnamespace
  WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped`, oid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n string
		var c column
		if err := rows.Scan(&n, &c.typ, &c.notNull, &c.plainCollation, &c.collation); err != nil {
			rows.Close()
			return nil, err
		}
		columns[n] = c
	}
	if err := closed(rows); err != nil {
		return nil, err
	}
	for _, want := range t.Columns {
		got, ok := columns[want.Name]
		switch {
		case !ok:
			fault("column %s is missing", want.Name)
		case got.typ != want.Type:
			fault("column %s has the type %s, and the manifest needs %s", want.Name, got.typ, want.Type)
		case want.NotNull && !got.notNull:
			fault("column %s is nullable, and the manifest needs NOT NULL", want.Name)
		case !got.plainCollation:
			fault("column %s has the collation %s, and the manifest needs the default or the C collation", want.Name, got.collation)
		}
	}

	type constraint struct {
		kind, definition string
		validated        bool
	}
	constraints := map[string]constraint{}
	rows, err = tx.QueryContext(ctx, `SELECT c.conname, c.contype::text, pg_catalog.pg_get_constraintdef(c.oid), c.convalidated
  FROM pg_catalog.pg_constraint c WHERE c.conrelid = $1`, oid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n string
		var c constraint
		if err := rows.Scan(&n, &c.kind, &c.definition, &c.validated); err != nil {
			rows.Close()
			return nil, err
		}
		constraints[n] = c
	}
	if err := closed(rows); err != nil {
		return nil, err
	}
	if t.RowSecurity {
		// A referential action runs with the rights of the table owner and passes row-level security, so on a table
		// with row-level security each foreign key is the manifest's.
		namedConstraints := map[string]bool{}
		for _, want := range t.Constraints {
			namedConstraints[want.Name] = true
		}
		var extra []string
		for n, c := range constraints {
			if c.kind == KindForeignKey && !namedConstraints[n] {
				extra = append(extra, n)
			}
		}
		slices.Sort(extra)
		for _, n := range extra {
			fault("constraint %s is a foreign key that the manifest does not name", n)
		}
	}
	// A foreign key of another table that refers to this one with an action that writes deletes or changes its rows
	// with the rights of its owner, past row-level security.
	rows, err = tx.QueryContext(ctx, `SELECT rn.nspname || '.' || r.relname || '.' || c.conname FROM pg_catalog.pg_constraint c
  JOIN pg_catalog.pg_class r ON r.oid = c.conrelid JOIN pg_catalog.pg_namespace rn ON rn.oid = r.relnamespace
  WHERE c.contype = 'f' AND c.confrelid = $1 AND c.conrelid <> $1
    AND (c.confdeltype NOT IN ('a', 'r') OR c.confupdtype NOT IN ('a', 'r')) ORDER BY 1`, oid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		fault("the foreign key %s refers to the table with an action that writes", n)
	}
	if err := closed(rows); err != nil {
		return nil, err
	}
	for _, want := range t.Constraints {
		got, ok := constraints[want.Name]
		switch {
		case !ok:
			fault("constraint %s is missing", want.Name)
		case got.kind != want.Kind || strings.TrimSuffix(got.definition, " NOT VALID") != want.Definition:
			fault("constraint %s is not the constraint of the manifest", want.Name)
		case !got.validated:
			fault("constraint %s is not validated", want.Name)
		}
	}

	type index struct {
		columns                                   []string
		predicate                                 string
		unique, valid, ready, immediate, distinct bool
		expressions, plainKeys                    bool
	}
	indexes := map[string]*index{}
	rows, err = tx.QueryContext(ctx, `SELECT ic.relname, i.indisunique, i.indisvalid, i.indisready, i.indimmediate,
  NOT i.indnullsnotdistinct, i.indexprs IS NOT NULL, COALESCE(pg_catalog.pg_get_expr(i.indpred, i.indrelid), ''),
  NOT EXISTS (SELECT 1 FROM pg_catalog.unnest(i.indclass::pg_catalog.oid[]) WITH ORDINALITY AS k(opclass, o)
    JOIN pg_catalog.pg_opclass oc ON oc.oid = k.opclass WHERE k.o <= i.indnkeyatts AND NOT oc.opcdefault)
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.unnest(i.indcollation::pg_catalog.oid[]) WITH ORDINALITY AS k(coll, o)
    WHERE k.o <= i.indnkeyatts AND k.coll NOT IN (0, 100, 950))
  FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class ic ON ic.oid = i.indexrelid WHERE i.indrelid = $1`, oid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n string
		i := &index{}
		if err := rows.Scan(&n, &i.unique, &i.valid, &i.ready, &i.immediate, &i.distinct, &i.expressions, &i.predicate, &i.plainKeys); err != nil {
			rows.Close()
			return nil, err
		}
		indexes[n] = i
	}
	if err := closed(rows); err != nil {
		return nil, err
	}
	// The key columns of each index, one row each in order, so no name of a column can stand for two.
	rows, err = tx.QueryContext(ctx, `SELECT ic.relname, COALESCE(a.attname, '')
  FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class ic ON ic.oid = i.indexrelid
  CROSS JOIN LATERAL pg_catalog.unnest(i.indkey::pg_catalog.int2[]) WITH ORDINALITY AS k(n, o)
  LEFT JOIN pg_catalog.pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.n AND k.n > 0
  WHERE i.indrelid = $1 AND k.o <= i.indnkeyatts ORDER BY ic.relname, k.o`, oid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n, column string
		if err := rows.Scan(&n, &column); err != nil {
			rows.Close()
			return nil, err
		}
		if i := indexes[n]; i != nil {
			i.columns = append(i.columns, column)
		}
	}
	if err := closed(rows); err != nil {
		return nil, err
	}
	for _, want := range t.Indexes {
		got, ok := indexes[want.Name]
		switch {
		case !ok:
			fault("index %s is missing", want.Name)
		case !got.unique:
			fault("index %s is not unique", want.Name)
		case got.expressions || !slices.Equal(got.columns, want.Columns):
			fault("index %s does not have the columns (%s) of the manifest", want.Name, strings.Join(want.Columns, ", "))
		case got.predicate != want.Predicate:
			fault("index %s does not have the predicate of the manifest", want.Name)
		case !got.valid || !got.ready:
			fault("index %s is not valid", want.Name)
		case !got.immediate:
			fault("index %s is deferrable", want.Name)
		case !got.distinct:
			fault("index %s treats nulls as not distinct", want.Name)
		case !got.plainKeys:
			fault("index %s has an operator class or a collation that is not the default", want.Name)
		}
	}

	if t.RowSecurity && (!rowSecurity || !forceRows) {
		fault("row-level security is not enabled and forced")
	}
	type policy struct {
		command, using, withCheck string
		permissive, public        bool
	}
	policies := map[string]policy{}
	rows, err = tx.QueryContext(ctx, `SELECT polname, polcmd::text, polpermissive, polroles = '{0}'::pg_catalog.oid[],
  COALESCE(pg_catalog.pg_get_expr(polqual, polrelid), ''), COALESCE(pg_catalog.pg_get_expr(polwithcheck, polrelid), '')
  FROM pg_catalog.pg_policy WHERE polrelid = $1`, oid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n string
		var p policy
		if err := rows.Scan(&n, &p.command, &p.permissive, &p.public, &p.using, &p.withCheck); err != nil {
			rows.Close()
			return nil, err
		}
		policies[n] = p
	}
	if err := closed(rows); err != nil {
		return nil, err
	}
	named := map[string]bool{}
	for _, want := range t.Policies {
		named[want.Name] = true
		got, ok := policies[want.Name]
		switch {
		case !ok:
			fault("policy %s is missing", want.Name)
		case !got.permissive || !got.public || got.command != want.Command || got.using != want.Using || got.withCheck != want.WithCheck:
			fault("policy %s is not the policy of the manifest", want.Name)
		}
	}
	if t.RowSecurity {
		// A permissive policy widens what a role sees, so with row-level security each one is the manifest's. A
		// restrictive policy only narrows it, and is allowed.
		var extra []string
		for n, p := range policies {
			if p.permissive && !named[n] {
				extra = append(extra, n)
			}
		}
		slices.Sort(extra)
		for _, n := range extra {
			fault("policy %s is a permissive policy that the manifest does not name", n)
		}
	}

	triggers, err := readTriggers(ctx, tx, oid, owner)
	if err != nil {
		return nil, err
	}
	namedTriggers := map[string]bool{}
	namedRows := map[uint32]string{}
	for _, want := range t.Triggers {
		namedTriggers[want.Name] = true
		got, ok := triggers[want.Name]
		if !ok {
			fault("trigger %s is missing", want.Name)
			continue
		}
		if want.Row {
			namedRows[got.oid] = want.Name
		}
		if f := triggerFault(want, got); f != "" {
			fault("trigger %s %s", want.Name, f)
		}
	}
	if t.RowSecurity {
		// A trigger runs on every row that a role writes, and can drop, change or copy it, so on a table with
		// row-level security each trigger is the manifest's.
		var extra []string
		for n := range triggers {
			if !namedTriggers[n] {
				extra = append(extra, n)
			}
		}
		slices.Sort(extra)
		for _, n := range extra {
			fault("trigger %s is a trigger that the manifest does not name", n)
		}
	}

	// A rule rewrites each statement on the table, and can copy or drop its rows, so a table of the manifest has none.
	rows, err = tx.QueryContext(ctx, `SELECT rulename FROM pg_catalog.pg_rewrite WHERE ev_class = $1 ORDER BY rulename`, oid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		fault("rule %s is a rule that the manifest does not allow", n)
	}
	if err := closed(rows); err != nil {
		return nil, err
	}
	if t.Partitioned {
		found, err := checkPartitions(ctx, tx, t, oid, ownerName, namedRows)
		if err != nil {
			return nil, err
		}
		for _, f := range found {
			fault("%s", f)
		}
	}
	return faults, nil
}

// catalogTrigger is one trigger of a table as the catalog holds it.
type catalogTrigger struct {
	oid, parent                             uint32
	tgtype                                  int16
	enabled, function, language, source     string
	config                                  string
	plain, securityDefiner, fits, ownerSame bool
}

// readTriggers reads the triggers of the table rel that are not internal, by name, with their functions; ownerSame
// tells that the function belongs to owner.
func readTriggers(ctx context.Context, tx *sql.Tx, rel, owner uint32) (map[string]catalogTrigger, error) {
	triggers := map[string]catalogTrigger{}
	rows, err := tx.QueryContext(ctx, `SELECT t.tgname, t.oid, t.tgparentid, t.tgtype, t.tgenabled::text, pn.nspname || '.' || p.proname, l.lanname,
  p.prosrc, COALESCE(pg_catalog.array_to_string(p.proconfig, chr(31)), ''), p.prosecdef,
  t.tgqual IS NULL AND pg_catalog.cardinality(t.tgattr::pg_catalog.int2[]) = 0 AND t.tgnargs = 0,
  p.prorettype = 'pg_catalog.trigger'::pg_catalog.regtype AND p.pronargs = 0 AND p.provolatile = 'v' AND NOT p.proretset,
  p.proowner = $2
  FROM pg_catalog.pg_trigger t JOIN pg_catalog.pg_proc p ON p.oid = t.tgfoid
  JOIN pg_catalog.pg_namespace pn ON pn.oid = p.pronamespace JOIN pg_catalog.pg_language l ON l.oid = p.prolang
  WHERE t.tgrelid = $1 AND NOT t.tgisinternal`, rel, owner)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n string
		var tr catalogTrigger
		if err := rows.Scan(&n, &tr.oid, &tr.parent, &tr.tgtype, &tr.enabled, &tr.function, &tr.language, &tr.source, &tr.config,
			&tr.securityDefiner, &tr.plain, &tr.fits, &tr.ownerSame); err != nil {
			rows.Close()
			return nil, err
		}
		triggers[n] = tr
	}
	return triggers, closed(rows)
}

// triggerFault gives what is wrong with the trigger got against want, such as "is disabled", or "".
func triggerFault(want Trigger, got catalogTrigger) string {
	bits, _ := want.tgtype()
	switch {
	case got.tgtype != bits || got.function != want.Function.Name || !got.plain:
		return "is not the trigger of the manifest"
	case got.enabled != "O" && got.enabled != "A":
		return "is disabled"
	case got.language != want.Function.Language || got.source != want.Function.Source ||
		got.securityDefiner != want.Function.SecurityDefiner || got.config != strings.Join(want.Function.Config, "\x1f") || !got.fits:
		return "runs the function " + want.Function.Name + ", which is not the function of the manifest"
	case !got.ownerSame:
		return "runs the function " + want.Function.Name + ", which the owner of the table does not own"
	}
	return ""
}

// checkPartitions gives the faults of the partitions of the partitioned table t (oid parent, owned by owner). A row
// written to the table is written to a partition, so each partition is a plain table of the owner with no rule; its
// triggers are the clones of the row triggers of the manifest on the table, each enabled, and the triggers that
// Table.PartitionTriggers names for each partition; it grants no privilege, on it or on a column, to another role
// than its owner, since a partition has no row-level security of its own; and it has a clone of each row trigger of
// the manifest.
// namedRows are the row triggers of the manifest on the table, by oid.
func checkPartitions(ctx context.Context, tx *sql.Tx, t Table, parent uint32, owner string, namedRows map[uint32]string) ([]string, error) {
	type partition struct {
		oid, relowner         uint32
		name, kind, ownerName string
		isPartition, granted  bool
	}
	var partitions []partition
	rows, err := tx.QueryContext(ctx, `SELECT c.oid, n.nspname || '.' || c.relname, c.relkind::text, c.relowner,
  pg_catalog.pg_get_userbyid(c.relowner), c.relispartition,
  EXISTS (SELECT 1 FROM pg_catalog.aclexplode(c.relacl) a WHERE a.grantee <> c.relowner)
  OR EXISTS (SELECT 1 FROM pg_catalog.pg_attribute att CROSS JOIN LATERAL pg_catalog.aclexplode(att.attacl) a
    WHERE att.attrelid = c.oid AND a.grantee <> c.relowner)
  FROM pg_catalog.pg_inherits i JOIN pg_catalog.pg_class c ON c.oid = i.inhrelid
  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE i.inhparent = $1 ORDER BY 2`, parent)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p partition
		if err := rows.Scan(&p.oid, &p.name, &p.kind, &p.relowner, &p.ownerName, &p.isPartition, &p.granted); err != nil {
			rows.Close()
			return nil, err
		}
		partitions = append(partitions, p)
	}
	if err := closed(rows); err != nil {
		return nil, err
	}
	var faults []string
	for _, p := range partitions {
		fault := func(format string, args ...any) {
			faults = append(faults, "partition "+p.name+": "+fmt.Sprintf(format, args...))
		}
		if p.kind != "r" || !p.isPartition {
			fault("the partition is not a plain table partition")
		}
		if p.ownerName != owner {
			fault("the partition belongs to the role %s, and the table belongs to %s", p.ownerName, owner)
		}
		// A partition has no row-level security of its own: a role with a grant on it reads or writes the rows of
		// every tenant past the policy of the table, so only the owner holds a privilege on it.
		if p.granted {
			fault("the partition grants a privilege on it or on a column to another role than its owner")
		}
		var rules int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM pg_catalog.pg_rewrite WHERE ev_class = $1`, p.oid).Scan(&rules); err != nil {
			return nil, err
		}
		if rules > 0 {
			fault("the partition has a rule")
		}
		triggers, err := readTriggers(ctx, tx, p.oid, p.relowner)
		if err != nil {
			return nil, err
		}
		_, relname, _ := strings.Cut(p.name, ".")
		own := map[string]Trigger{}
		for _, want := range t.PartitionTriggers {
			own[relname+want.Name] = want
		}
		cloned := map[uint32]bool{}
		var names []string
		for n := range triggers {
			names = append(names, n)
		}
		slices.Sort(names)
		for _, n := range names {
			got := triggers[n]
			if want, ok := own[n]; ok && got.parent == 0 {
				if f := triggerFault(want, got); f != "" {
					fault("trigger %s %s", n, f)
				}
				delete(own, n)
				continue
			}
			if _, named := namedRows[got.parent]; got.parent == 0 || !named {
				fault("trigger %s is a trigger that the manifest does not name", n)
				continue
			}
			cloned[got.parent] = true
			if got.enabled != "O" && got.enabled != "A" {
				fault("trigger %s is disabled", n)
			}
		}
		var missing []string
		for oid, n := range namedRows {
			if !cloned[oid] {
				missing = append(missing, n)
			}
		}
		for n := range own {
			missing = append(missing, n)
		}
		slices.Sort(missing)
		for _, n := range missing {
			fault("trigger %s is missing", n)
		}
	}
	return faults, nil
}

// closed closes rows and gives the error of the iteration.
func closed(rows *sql.Rows) error {
	err := rows.Err()
	if cerr := rows.Close(); err == nil {
		err = cerr
	}
	return err
}
