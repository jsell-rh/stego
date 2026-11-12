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
}

// Table is one table that the code reads or writes, in its schema.
type Table struct {
	Schema, Name string
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
}

// Column is one column: its name, its type as format_type gives it, such as character varying(255), and whether
// the code needs it NOT NULL. A column that the code takes as nullable may be NOT NULL in the database.
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

// Constraint is one constraint of a table, by name: its kind and, for a foreign key, the table it refers to, as
// schema.table. The constraint must be validated.
type Constraint struct {
	Name, Kind, References string
}

// Index is one unique index of a table: its key columns in order and its predicate as pg_get_expr gives it, or
// empty for an index on every row. The index must be valid, ready, not deferrable, and have no expression column.
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
// which events (INSERT, UPDATE, DELETE, TRUNCATE), and the function it runs, as schema.function. The trigger must
// be enabled.
type Trigger struct {
	Name, Timing string
	Row          bool
	Events       []string
	Function     string
}

// Join gives one manifest with the tables of each manifest, in order.
func Join(manifests ...Manifest) Manifest {
	var joined Manifest
	for _, m := range manifests {
		joined.Tables = append(joined.Tables, m.Tables...)
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
			if (c.Kind == KindForeignKey) != (c.References != "") {
				return fmt.Errorf("manifest: table %s: the constraint %s names a table only when it is a foreign key", id, c.Name)
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
		for _, tr := range t.Triggers {
			if err := named("trigger", tr.Name); err != nil {
				return err
			}
			if _, err := tr.tgtype(); err != nil {
				return fmt.Errorf("manifest: table %s: the trigger %s: %w", id, tr.Name, err)
			}
			if tr.Function == "" {
				return fmt.Errorf("manifest: table %s: the trigger %s has no function", id, tr.Name)
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

// Check reads the catalog of the database in one read-only transaction on db, with the search path pg_catalog, and
// gives a *Mismatch that names each table, column, constraint, index, policy and trigger of the manifest that the
// database does not hold as the manifest states it (ST-SUP-RC01-OC12). It reads only the catalog, so the runtime
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
	if _, err := tx.ExecContext(ctx, "SET LOCAL search_path = pg_catalog"); err != nil {
		return fmt.Errorf("manifest: read the catalog: %w", err)
	}
	var faults []string
	for _, t := range m.Tables {
		found, err := checkTable(ctx, tx, t)
		if err != nil {
			return fmt.Errorf("manifest: read the catalog of the table %s.%s: %w", t.Schema, t.Name, err)
		}
		faults = append(faults, found...)
	}
	if len(faults) > 0 {
		return &Mismatch{Faults: faults}
	}
	return nil
}

// checkTable gives the faults of one table.
func checkTable(ctx context.Context, tx *sql.Tx, t Table) ([]string, error) {
	id := t.Schema + "." + t.Name
	var (
		oid                    uint32
		rowSecurity, forceRows bool
	)
	err := tx.QueryRowContext(ctx, `SELECT c.oid, c.relrowsecurity, c.relforcerowsecurity FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind IN ('r', 'p')`,
		t.Schema, t.Name).Scan(&oid, &rowSecurity, &forceRows)
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

	columns := map[string]Column{}
	rows, err := tx.QueryContext(ctx, `SELECT attname, format_type(atttypid, atttypmod), attnotnull FROM pg_attribute
  WHERE attrelid = $1 AND attnum > 0 AND NOT attisdropped`, oid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c Column
		if err := rows.Scan(&c.Name, &c.Type, &c.NotNull); err != nil {
			rows.Close()
			return nil, err
		}
		columns[c.Name] = c
	}
	if err := closed(rows); err != nil {
		return nil, err
	}
	for _, want := range t.Columns {
		got, ok := columns[want.Name]
		switch {
		case !ok:
			fault("column %s is missing", want.Name)
		case got.Type != want.Type:
			fault("column %s has the type %s, and the manifest needs %s", want.Name, got.Type, want.Type)
		case want.NotNull && !got.NotNull:
			fault("column %s is nullable, and the manifest needs NOT NULL", want.Name)
		}
	}

	type constraint struct {
		kind, references string
		validated        bool
	}
	constraints := map[string]constraint{}
	rows, err = tx.QueryContext(ctx, `SELECT c.conname, c.contype::text, COALESCE(rn.nspname || '.' || r.relname, ''), c.convalidated
  FROM pg_constraint c LEFT JOIN pg_class r ON r.oid = c.confrelid LEFT JOIN pg_namespace rn ON rn.oid = r.relnamespace
  WHERE c.conrelid = $1`, oid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n string
		var c constraint
		if err := rows.Scan(&n, &c.kind, &c.references, &c.validated); err != nil {
			rows.Close()
			return nil, err
		}
		constraints[n] = c
	}
	if err := closed(rows); err != nil {
		return nil, err
	}
	for _, want := range t.Constraints {
		got, ok := constraints[want.Name]
		switch {
		case !ok:
			fault("constraint %s is missing", want.Name)
		case got.kind != want.Kind || got.references != want.References:
			fault("constraint %s is not the constraint of the manifest", want.Name)
		case !got.validated:
			fault("constraint %s is not validated", want.Name)
		}
	}

	type index struct {
		columns                         []string
		predicate                       string
		unique, valid, ready, immediate bool
		expressions                     bool
	}
	indexes := map[string]index{}
	rows, err = tx.QueryContext(ctx, `SELECT ic.relname, i.indisunique, i.indisvalid, i.indisready, i.indimmediate,
  i.indexprs IS NOT NULL, COALESCE(pg_get_expr(i.indpred, i.indrelid), ''),
  COALESCE((SELECT string_agg(COALESCE(a.attname, ''), chr(31) ORDER BY k.o)
    FROM unnest(i.indkey::int2[]) WITH ORDINALITY AS k(n, o)
    LEFT JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.n AND k.n > 0
    WHERE k.o <= i.indnkeyatts), '')
  FROM pg_index i JOIN pg_class ic ON ic.oid = i.indexrelid WHERE i.indrelid = $1`, oid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n, keys string
		var i index
		if err := rows.Scan(&n, &i.unique, &i.valid, &i.ready, &i.immediate, &i.expressions, &i.predicate, &keys); err != nil {
			rows.Close()
			return nil, err
		}
		i.columns = strings.Split(keys, "\x1f")
		indexes[n] = i
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
	rows, err = tx.QueryContext(ctx, `SELECT polname, polcmd::text, polpermissive, polroles = '{0}'::oid[],
  COALESCE(pg_get_expr(polqual, polrelid), ''), COALESCE(pg_get_expr(polwithcheck, polrelid), '')
  FROM pg_policy WHERE polrelid = $1`, oid)
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

	type trigger struct {
		tgtype            int16
		enabled, function string
	}
	triggers := map[string]trigger{}
	rows, err = tx.QueryContext(ctx, `SELECT t.tgname, t.tgtype, t.tgenabled::text, pn.nspname || '.' || p.proname
  FROM pg_trigger t JOIN pg_proc p ON p.oid = t.tgfoid JOIN pg_namespace pn ON pn.oid = p.pronamespace
  WHERE t.tgrelid = $1 AND NOT t.tgisinternal`, oid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n string
		var tr trigger
		if err := rows.Scan(&n, &tr.tgtype, &tr.enabled, &tr.function); err != nil {
			rows.Close()
			return nil, err
		}
		triggers[n] = tr
	}
	if err := closed(rows); err != nil {
		return nil, err
	}
	for _, want := range t.Triggers {
		got, ok := triggers[want.Name]
		bits, _ := want.tgtype()
		switch {
		case !ok:
			fault("trigger %s is missing", want.Name)
		case got.tgtype != bits || got.function != want.Function:
			fault("trigger %s is not the trigger of the manifest", want.Name)
		case got.enabled != "O" && got.enabled != "A":
			fault("trigger %s is disabled", want.Name)
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
