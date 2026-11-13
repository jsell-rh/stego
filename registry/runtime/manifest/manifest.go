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
	// Schemas are the schemas that the runtime holds whole, such as stego_outbox: each belongs to Owner, gives
	// USAGE with no grant option to its Usage roles and to no other role, PUBLIC included, gives no other privilege
	// to a role but its owner, and holds no function that the manifest does not name, so no overload of a function
	// of the manifest can stand in for it.
	Schemas []Schema
	// Confined are the roles that own no object but the functions of the manifest that name them as their Owner: no
	// other function, relation, type, schema, large object, operator, operator class or family, collation,
	// conversion, text search configuration or dictionary, statistics object, foreign-data wrapper, foreign
	// server, event trigger, extension, language, publication, subscription, database or tablespace, and no default
	// privileges; that are a member of no role, and have no member but the Owner of the manifest WITH SET TRUE,
	// INHERIT FALSE and no ADMIN; and that hold no privilege on a relation or a column of a relation
	// that is not a table of the manifest. A SECURITY DEFINER function runs with the rights of its owner, so a
	// confined role has the rights that the manifest names only.
	Confined []string
}

// Schema is one schema that the runtime holds whole, and the roles that may use it. A Shared schema, such as
// public, holds the objects of others too: it is not held whole, and only its CREATE is checked, which goes to no
// role but the owner of the schema and the owner of the manifest, so no other role can make a function or a table
// in it.
//
// A Steps schema is the held schema of the sync functions of schema steps (ST-SUP-RP15-LC02): it may be missing
// until a step makes it, and besides the functions of the manifest it holds only sync functions in their strict
// template form, each bound to the one sync trigger that Check accepts.
type Schema struct {
	Name   string
	Usage  []string
	Shared bool
	Steps  bool
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
	// CheckGrants tells that the privileges of the table and its columns, for each role but its owner, must be
	// exactly Grants: each privilege that Grants does not name, PUBLIC included, is a fault, and so is each one that
	// it names and the table does not grant. Without CheckGrants the grants are not read.
	CheckGrants bool
	Grants      []Grant
	// PartitionTriggers are the triggers that each partition of a partitioned table has of its own, each named by
	// the partition's name and the trigger's Name as a suffix, such as audit_event_p202611 and _reject_statement.
	PartitionTriggers []Trigger
	// OptionalTriggers are triggers that the table may lack, such as the sync trigger of a schema step that a later
	// contract step drops: when the table has a trigger of that exact name, it must be the trigger of the manifest,
	// as one of Triggers must; when it lacks it, that is no fault. A name matches only as a whole.
	OptionalTriggers []Trigger
	// Entity tells that the table is the table of a declared entity, which later schema steps change: on it, and only
	// on it, Check accepts a sync trigger, a foreign key and a column default that the manifest does not name, each
	// only in the exact form that a later step gives (ST-SUP-RP15-LC02, ST-SUP-RP15-LC03, ST-GEN-Q15). A runtime table
	// is never an entity table.
	Entity bool
}

// runtimeTable is the grammar of the names of the tables of the runtime library, which are never the table of a
// declared entity (ST-SUP-RP15-LC02 rule 1).
var runtimeTable = regexp.MustCompile(`^(?:messages|idempotency_keys|leases|audit_.*|authz_.*)$`)

// Column is one column: its name, its type as format_type gives it, such as character varying(255), and whether
// the code needs it NOT NULL. A column that the code takes as nullable may be NOT NULL in the database. A column of a
// type with a collation must have the default or the C collation, so equality is the byte equality that the tenant
// wall and the keys rely on.
//
// Default is the column's default as pg_get_expr gives it with the search path pg_catalog, such as now() or
// current_setting('app.tenant'::text), or a literal of the column's type, such as ”::text; empty is no default. An
// INSERT that leaves a column out takes its default, so a column of a table of the manifest, listed or not, has the
// default that the manifest states and no other: a default that differs, or one on a column that the manifest
// states none for, is a fault. A literal default is compared by its value, so the manifest may write it in any form
// that PostgreSQL reads as the same value of the column's type.
//
// Identity is the identity of the column as pg_attribute.attidentity gives it: a for GENERATED ALWAYS, d for
// GENERATED BY DEFAULT, or empty for none. A column of a table of the manifest, listed or not, has the identity that
// the manifest states and no other, and the sequence of an identity counts up by 1 from 1 to the maximum of its type
// and does not cycle, so its values keep the order of the inserts.
type Column struct {
	Name, Type string
	NotNull    bool
	Default    string
	Identity   string
}

// builtinType is a column type of pg_catalog whose input and comparison functions are built in and have no side
// effect, so Check may read a literal of it as a value: no domain, enum or other type of a schema of the database.
var builtinType = regexp.MustCompile(`^(?:text|character varying(?:\([0-9]+\))?|uuid|jsonb|bytea|boolean|smallint|integer|bigint|real|double precision|numeric(?:\([0-9, ]+\))?|timestamp with time zone|date)$`)

// literalDefault is a default that is a constant: a number, true or false, or a quoted literal with a cast to a
// type of pg_catalog that has no schema in its name, so its value is read by a built-in input function only.
var literalDefault = regexp.MustCompile(`^(?:(-?[0-9]+(?:\.[0-9]+)?)|(true|false)|'((?:[^']|'')*)'::([a-z][a-z0-9 ]*(?:\([0-9, ]+\))?))$`)

// literal gives the text and the type of a literal default: a number is numeric and true or false boolean. ok is
// false for a default that is no literal or whose type is not built in.
func literal(expr string) (text, typ string, ok bool) {
	m := literalDefault.FindStringSubmatch(expr)
	switch {
	case m == nil:
		return "", "", false
	case m[1] != "":
		return m[1], "numeric", true
	case m[2] != "":
		return m[2], "boolean", true
	}
	return strings.ReplaceAll(m[3], "''", "'"), m[4], builtinType.MatchString(m[4])
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

// CommandAll is the command of a policy for every command.
const CommandAll = "*"

// Policy is one permissive row-level security policy: its command (* for all, or r, a, w and d as pg_policy.polcmd
// gives them), its USING and WITH CHECK expressions as pg_get_expr gives them with the search path pg_catalog, and
// the roles it applies to. No Roles is a policy for every role (PUBLIC); with Roles, the policy must apply to these
// roles and to no other.
type Policy struct {
	Name, Command, Using, WithCheck string
	Roles                           []string
}

// Grant is one grant of a table to a role other than its owner: the privileges, such as SELECT and INSERT, on the
// whole table, or with Columns on these columns only. A grant with the grant option is never the manifest's.
type Grant struct {
	Role       string
	Privileges []string
	Columns    []string
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
// and belong to the owner of the table, or to the role that its Owner names.
type Function struct {
	Name, Language, Source string
	SecurityDefiner        bool
	Config                 []string
	// Execute, for a function of Manifest.Functions, are the roles that may run it besides its owner: EXECUTE must
	// go to these roles, with no grant option, and to no other grantee, PUBLIC included.
	Execute []string
	// Args, for a function of Manifest.Functions, are the types of its arguments as regprocedure takes them, such as
	// timestamptz, integer; empty is a function of no argument.
	Args string
	// Owner is the role that must own the function. Empty is, for a function of Manifest.Functions, the Owner of
	// the manifest, and for a trigger function, the owner of the table.
	Owner string
	// Returns, for a function of Manifest.Functions, is its result as pg_get_function_result gives it, such as
	// bigint or SETOF stego_outbox.messages; Strict tells that it is STRICT, and Volatility is its volatility as
	// pg_proc.provolatile gives it: i, s, or v, which empty means.
	Returns    string
	Strict     bool
	Volatility string
}

// argsText is the grammar of the argument types of a function of the manifest, such as timestamptz, integer.
var argsText = regexp.MustCompile(`^[a-z0-9_ ,]{0,512}$`)

// Join gives one manifest with the tables of each manifest, in order.
func Join(manifests ...Manifest) Manifest {
	var joined Manifest
	for _, m := range manifests {
		joined.Tables = append(joined.Tables, m.Tables...)
		joined.Functions = append(joined.Functions, m.Functions...)
		for _, sc := range m.Schemas {
			// A shared schema that two manifests name is checked once.
			if sc.Shared && slices.ContainsFunc(joined.Schemas, func(o Schema) bool { return o.Shared && o.Name == sc.Name }) {
				continue
			}
			joined.Schemas = append(joined.Schemas, sc)
		}
		for _, role := range m.Confined {
			if !slices.Contains(joined.Confined, role) {
				joined.Confined = append(joined.Confined, role)
			}
		}
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
		if !argsText.MatchString(f.Args) || (f.Owner != "" && !name.MatchString(f.Owner)) {
			return fmt.Errorf("manifest: the function %s needs argument types of letters, digits, spaces and commas, and an owner of at most 63 bytes", f.Name)
		}
		if f.Returns == "" || !slices.Contains([]string{"", "i", "s", "v"}, f.Volatility) {
			return fmt.Errorf("manifest: the function %s needs its result and a volatility of i, s or v", f.Name)
		}
		for _, r := range f.Execute {
			if !name.MatchString(r) || r == "PUBLIC" {
				return fmt.Errorf("manifest: the function %s gives EXECUTE to %q; only named roles may run it", f.Name, r)
			}
		}
		id := f.Name + "(" + f.Args + ")"
		if functions[id] {
			return fmt.Errorf("manifest: the function %s is named twice", id)
		}
		functions[id] = true
	}
	schemas := map[string]bool{}
	for _, sc := range m.Schemas {
		if !name.MatchString(sc.Name) || schemas[sc.Name] {
			return fmt.Errorf("manifest: the schema %q needs a name of at most 63 bytes, named once", sc.Name)
		}
		schemas[sc.Name] = true
		if sc.Shared && len(sc.Usage) > 0 {
			return fmt.Errorf("manifest: the shared schema %s names USAGE, which only a held schema checks", sc.Name)
		}
		for _, r := range sc.Usage {
			if !name.MatchString(r) || r == "PUBLIC" {
				return fmt.Errorf("manifest: the schema %s gives USAGE to %q; only named roles may use it", sc.Name, r)
			}
		}
	}
	if len(m.Schemas) > 0 && m.Owner == "" {
		return errors.New("manifest: a schema of the manifest needs the owner of the manifest")
	}
	steps := 0
	for _, sc := range m.Schemas {
		if sc.Steps {
			steps++
			if sc.Shared {
				return fmt.Errorf("manifest: the schema %s of the sync functions is shared; it must be held whole", sc.Name)
			}
		}
	}
	if steps > 1 {
		return errors.New("manifest: more than one schema holds the sync functions of the schema steps")
	}
	for _, r := range m.Confined {
		if !name.MatchString(r) || r == "PUBLIC" {
			return fmt.Errorf("manifest: the confined role %q needs a name of at most 63 bytes", r)
		}
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
			for _, r := range p.Roles {
				if !name.MatchString(r) || r == "PUBLIC" {
					return fmt.Errorf("manifest: table %s: the policy %s names the role %q; a policy for every role names none", id, p.Name, r)
				}
			}
		}
		if !t.CheckGrants && len(t.Grants) > 0 {
			return fmt.Errorf("manifest: table %s names grants but does not check them", id)
		}
		for _, g := range t.Grants {
			if !name.MatchString(g.Role) || g.Role == "PUBLIC" || len(g.Privileges) == 0 {
				return fmt.Errorf("manifest: table %s: a grant needs a role that is not PUBLIC and a privilege", id)
			}
			for _, privilege := range g.Privileges {
				if !slices.Contains([]string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER", "MAINTAIN"}, privilege) {
					return fmt.Errorf("manifest: table %s: the grant to %s has the unknown privilege %q", id, g.Role, privilege)
				}
			}
			for _, c := range g.Columns {
				if !name.MatchString(c) {
					return fmt.Errorf("manifest: table %s: the grant to %s names the column %q", id, g.Role, c)
				}
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
		if t.Entity && (t.Partitioned || runtimeTable.MatchString(t.Name)) {
			return fmt.Errorf("manifest: table %s is partitioned or a runtime table, so it is not the table of a declared entity", id)
		}
		if t.Partitioned && len(t.OptionalTriggers) > 0 {
			return fmt.Errorf("manifest: table %s: a partitioned table has no optional trigger", id)
		}
		for _, tr := range slices.Concat(t.Triggers, t.OptionalTriggers) {
			if err := named("trigger", tr.Name); err != nil {
				return err
			}
			if _, err := tr.tgtype(); err != nil {
				return fmt.Errorf("manifest: table %s: the trigger %s: %w", id, tr.Name, err)
			}
			if tr.Function.Name == "" || tr.Function.Language == "" || tr.Function.Source == "" {
				return fmt.Errorf("manifest: table %s: the trigger %s has no function name, language or source", id, tr.Name)
			}
			if tr.Function.Owner != "" && !name.MatchString(tr.Function.Owner) {
				return fmt.Errorf("manifest: table %s: the trigger %s names the owner %q of its function", id, tr.Name, tr.Function.Owner)
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
	// The search path of the session, and the role that it runs as, before the check fixes its own path.
	var path, role string
	if err := tx.QueryRowContext(ctx, "SELECT pg_catalog.array_to_string(pg_catalog.current_schemas(true), chr(31)), current_user").Scan(&path, &role); err != nil {
		return fmt.Errorf("manifest: read the catalog: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "SET LOCAL search_path = pg_catalog, pg_temp"); err != nil {
		return fmt.Errorf("manifest: read the catalog: %w", err)
	}
	var faults []string
	found, err := checkSearchPath(ctx, tx, m, strings.Split(path, "\x1f"), role)
	if err != nil {
		return fmt.Errorf("manifest: read the catalog of the search path: %w", err)
	}
	faults = append(faults, found...)
	// session_replication_role replica or local skips each ordinary trigger, so the triggers of the manifest
	// would not run in this session or in the sessions of its role.
	var replication string
	if err := tx.QueryRowContext(ctx, "SELECT pg_catalog.current_setting('session_replication_role')").Scan(&replication); err != nil {
		return fmt.Errorf("manifest: read the catalog: %w", err)
	}
	if replication != "origin" {
		faults = append(faults, "the session runs with session_replication_role "+replication+", which skips the triggers")
	}
	tables, entities, shared := map[uint32]bool{}, map[uint32]bool{}, map[uint32]bool{}
	for _, t := range m.Tables {
		var oid sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT pg_catalog.to_regclass(pg_catalog.quote_ident($1) || '.' || pg_catalog.quote_ident($2))::pg_catalog.oid`,
			t.Schema, t.Name).Scan(&oid); err != nil {
			return fmt.Errorf("manifest: read the catalog of the table %s.%s: %w", t.Schema, t.Name, err)
		}
		if oid.Valid {
			tables[uint32(oid.Int64)] = true
			entities[uint32(oid.Int64)] = t.Entity
			shared[uint32(oid.Int64)] = t.Entity && !t.RowSecurity
		}
	}
	steps := &stepState{owner: m.Owner, entities: entities, shared: shared, accepted: map[uint32]bool{}}
	for _, sc := range m.Schemas {
		if sc.Steps {
			steps.schema = sc.Name
		}
	}
	for _, t := range m.Tables {
		found, err := checkTable(ctx, tx, t, m.Owner, steps)
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
	named, err := namedFunctions(ctx, tx, m)
	if err != nil {
		return fmt.Errorf("manifest: read the catalog of the functions: %w", err)
	}
	for _, sc := range m.Schemas {
		found, err := checkSchema(ctx, tx, sc, m.Owner, named, steps.accepted)
		if err != nil {
			return fmt.Errorf("manifest: read the catalog of the schema %s: %w", sc.Name, err)
		}
		faults = append(faults, found...)
	}
	found, err = checkNames(ctx, tx, m, named)
	if err != nil {
		return fmt.Errorf("manifest: read the catalog of the functions: %w", err)
	}
	faults = append(faults, found...)
	for _, role := range m.Confined {
		found, err := checkConfined(ctx, tx, role, m.Owner, named, tables)
		if err != nil {
			return fmt.Errorf("manifest: read the catalog of the objects of the role %s: %w", role, err)
		}
		faults = append(faults, found...)
	}
	if len(faults) > 0 {
		return &Mismatch{Faults: faults}
	}
	return nil
}

// namedFunctions gives the oids of the functions of the manifest that the database holds, the functions of
// Manifest.Functions by their signature and the trigger functions by their name and no argument, each with the role
// that the manifest names as its owner, or "" when it names none.
func namedFunctions(ctx context.Context, tx *sql.Tx, m Manifest) (map[uint32]string, error) {
	signatures := map[string]string{}
	for _, f := range m.Functions {
		signatures[f.Name+"("+f.Args+")"] = f.Owner
	}
	for _, t := range m.Tables {
		for _, tr := range slices.Concat(t.Triggers, t.PartitionTriggers, t.OptionalTriggers) {
			signatures[tr.Function.Name+"()"] = tr.Function.Owner
		}
	}
	named := map[uint32]string{}
	for signature, owner := range signatures {
		var oid sql.NullInt64
		// A function of no argument is found by its schema and name in the catalog, which needs no USAGE on its
		// schema, as a trigger function in the held schema of the sync functions has none.
		query, args := `SELECT pg_catalog.to_regprocedure($1)::pg_catalog.oid`, []any{signature}
		if function, ok := strings.CutSuffix(signature, "()"); ok {
			schema, fn, _ := strings.Cut(function, ".")
			query = `SELECT (SELECT p.oid FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
  WHERE n.nspname = $1 AND p.proname = $2 AND p.pronargs = 0)`
			args = []any{schema, fn}
		}
		if err := tx.QueryRowContext(ctx, query, args...).Scan(&oid); err != nil {
			return nil, err
		}
		if oid.Valid {
			named[uint32(oid.Int64)] = owner
		}
	}
	return named, nil
}

// checkSearchPath gives a fault for each relation that has the name of a table of the manifest in a schema of the
// search path of the session (path, as current_schemas(true) gives it) that comes before the schema of the table, or
// in any schema of the path when the path does not hold that schema: a name with no schema resolves to it first. It
// gives a fault for each function that has the name of a function of the manifest in a schema of the path before the
// schema of that function, when the path holds that schema. It also refuses a schema
// named after the role of the session (role), which the default search path "$user", public puts first.
func checkSearchPath(ctx context.Context, tx *sql.Tx, m Manifest, path []string, role string) ([]string, error) {
	var faults []string
	var own bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname = $1)`, role).Scan(&own); err != nil {
		return nil, err
	}
	if own {
		faults = append(faults, "the schema "+role+" has the name of the role of the session, which a search path of \"$user\" puts first")
	}
	// earlier gives the schemas of the path before schema, or the whole path when it does not hold schema.
	earlier := func(schema string) []string {
		if i := slices.Index(path, schema); i >= 0 {
			return path[:i]
		}
		return path
	}
	type object struct{ kind, schema, name string }
	var objects []object
	for _, t := range m.Tables {
		objects = append(objects, object{"relation", t.Schema, t.Name})
	}
	for _, f := range m.Functions {
		schema, fn, _ := strings.Cut(f.Name, ".")
		objects = append(objects, object{"function", schema, fn})
	}
	for _, o := range objects {
		// A function of the manifest is called with its schema (ST-STD-TXN-R19), so only a function earlier on the
		// path than its own schema, when that schema is on it, could stand in for a call with no schema.
		if o.kind == "function" && !slices.Contains(path, o.schema) {
			continue
		}
		before := earlier(o.schema)
		if len(before) == 0 {
			continue
		}
		query := `SELECT n.nspname FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
  WHERE c.relname = $1 AND n.nspname = ANY ($2::text[]) AND n.nspname <> $3 ORDER BY 1`
		if o.kind == "function" {
			query = `SELECT DISTINCT n.nspname FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
  WHERE p.proname = $1 AND n.nspname = ANY ($2::text[]) AND n.nspname <> $3 ORDER BY 1`
		}
		rows, err := tx.QueryContext(ctx, query, o.name, before, o.schema)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var schema string
			if err := rows.Scan(&schema); err != nil {
				rows.Close()
				return nil, err
			}
			faults = append(faults, fmt.Sprintf("the %s %s.%s comes before %s.%s on the search path of the session", o.kind, schema, o.name, o.schema, o.name))
		}
		if err := closed(rows); err != nil {
			return nil, err
		}
	}
	return faults, nil
}

// checkNames gives a fault for each function that has the schema and the name of a function of the manifest and that
// is not one of the manifest, such as an overload with other argument types, which a call with untyped arguments
// could run in its place. A function in a held schema is the fault of that schema.
func checkNames(ctx context.Context, tx *sql.Tx, m Manifest, named map[uint32]string) ([]string, error) {
	held := map[string]bool{}
	for _, sc := range m.Schemas {
		if !sc.Shared {
			held[sc.Name] = true
		}
	}
	seen := map[string]bool{}
	var names []string
	add := func(f string) {
		if schema, _, _ := strings.Cut(f, "."); !held[schema] && !seen[f] {
			seen[f] = true
			names = append(names, f)
		}
	}
	for _, f := range m.Functions {
		add(f.Name)
	}
	for _, t := range m.Tables {
		for _, tr := range slices.Concat(t.Triggers, t.PartitionTriggers, t.OptionalTriggers) {
			add(tr.Function.Name)
		}
	}
	slices.Sort(names)
	var faults []string
	for _, f := range names {
		schema, fn, _ := strings.Cut(f, ".")
		rows, err := tx.QueryContext(ctx, `SELECT p.oid, p.oid::pg_catalog.regprocedure::text FROM pg_catalog.pg_proc p
  JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = $1 AND p.proname = $2 ORDER BY 2`, schema, fn)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var oid uint32
			var signature string
			if err := rows.Scan(&oid, &signature); err != nil {
				rows.Close()
				return nil, err
			}
			if _, ok := named[oid]; !ok {
				faults = append(faults, "function "+signature+" has the name of a function of the manifest, and is not one of it")
			}
		}
		if err := closed(rows); err != nil {
			return nil, err
		}
	}
	return faults, nil
}

// checkSchema gives the faults of one schema that the runtime holds whole: missing, of another owner, a privilege
// of a role other than its owner that the manifest does not give, PUBLIC included, one with the grant option, a
// USAGE of the manifest that it does not give, and each function in it that the manifest does not name.
func checkSchema(ctx context.Context, tx *sql.Tx, sc Schema, wantOwner string, named map[uint32]string, accepted map[uint32]bool) ([]string, error) {
	var oid uint32
	var owner string
	err := tx.QueryRowContext(ctx, `SELECT oid, pg_catalog.pg_get_userbyid(nspowner) FROM pg_catalog.pg_namespace WHERE nspname = $1`, sc.Name).Scan(&oid, &owner)
	if errors.Is(err, sql.ErrNoRows) {
		if sc.Steps {
			// The held schema of the sync functions is made by the first step that needs it.
			return nil, nil
		}
		return []string{"schema " + sc.Name + " is missing"}, nil
	}
	if err != nil {
		return nil, err
	}
	var faults []string
	fault := func(format string, args ...any) {
		faults = append(faults, "schema "+sc.Name+": "+fmt.Sprintf(format, args...))
	}
	if sc.Shared {
		rows, err := tx.QueryContext(ctx, `SELECT CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_catalog.pg_get_userbyid(a.grantee) END
  FROM pg_catalog.pg_namespace n, pg_catalog.aclexplode(COALESCE(n.nspacl, pg_catalog.acldefault('n', n.nspowner))) a
  WHERE n.oid = $1 AND a.grantee <> n.nspowner AND a.privilege_type = 'CREATE' ORDER BY 1`, oid)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var role string
			if err := rows.Scan(&role); err != nil {
				rows.Close()
				return nil, err
			}
			if role != wantOwner {
				fault("the privilege CREATE to %s is granted, and the manifest gives it to no role but the owner", role)
			}
		}
		return faults, closed(rows)
	}
	if owner != wantOwner {
		fault("the schema belongs to the role %s, and the manifest needs %s", owner, wantOwner)
	}
	rows, err := tx.QueryContext(ctx, `SELECT CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_catalog.pg_get_userbyid(a.grantee) END, a.privilege_type, a.is_grantable
  FROM pg_catalog.pg_namespace n, pg_catalog.aclexplode(COALESCE(n.nspacl, pg_catalog.acldefault('n', n.nspowner))) a
  WHERE n.oid = $1 AND a.grantee <> n.nspowner`, oid)
	if err != nil {
		return nil, err
	}
	got := map[string]bool{}
	for rows.Next() {
		var role, privilege string
		var grantable bool
		if err := rows.Scan(&role, &privilege, &grantable); err != nil {
			rows.Close()
			return nil, err
		}
		key := grantKey(role, privilege, "")
		if grantable {
			fault("the privilege %s is granted with the grant option", key)
		}
		got[key] = true
	}
	if err := closed(rows); err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, role := range sc.Usage {
		want[grantKey(role, "USAGE", "")] = true
	}
	var extra, missing []string
	for key := range got {
		if !want[key] {
			extra = append(extra, key)
		}
	}
	for key := range want {
		if !got[key] {
			missing = append(missing, key)
		}
	}
	slices.Sort(extra)
	slices.Sort(missing)
	for _, key := range extra {
		fault("the privilege %s is granted, and the manifest does not grant it", key)
	}
	for _, key := range missing {
		fault("the privilege %s of the manifest is not granted", key)
	}
	rows, err = tx.QueryContext(ctx, `SELECT p.oid, p.oid::pg_catalog.regprocedure::text FROM pg_catalog.pg_proc p WHERE p.pronamespace = $1 ORDER BY 2`, oid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var fn uint32
		var signature string
		if err := rows.Scan(&fn, &signature); err != nil {
			rows.Close()
			return nil, err
		}
		if _, ok := named[fn]; !ok && !(sc.Steps && accepted[fn]) {
			fault("function %s is a function that the manifest does not name", signature)
		}
	}
	return faults, closed(rows)
}

// checkConfined gives the faults of one confined role: missing, or the owner of a function that the manifest does
// not name with the role as its owner, of a relation, of a type that is no row or array type of another object, or
// of a schema.
func checkConfined(ctx context.Context, tx *sql.Tx, role, owner string, named map[uint32]string, tables map[uint32]bool) ([]string, error) {
	var oid uint32
	err := tx.QueryRowContext(ctx, `SELECT oid FROM pg_catalog.pg_roles WHERE rolname = $1`, role).Scan(&oid)
	if errors.Is(err, sql.ErrNoRows) {
		return []string{"the confined role " + role + " is missing"}, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT 'function', p.oid, p.oid::pg_catalog.regprocedure::text FROM pg_catalog.pg_proc p WHERE p.proowner = $1
  UNION ALL SELECT 'relation', c.oid, c.oid::pg_catalog.regclass::text FROM pg_catalog.pg_class c WHERE c.relowner = $1
  UNION ALL SELECT 'type', t.oid, t.oid::pg_catalog.regtype::text FROM pg_catalog.pg_type t WHERE t.typowner = $1 AND t.typrelid = 0
    AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_type e WHERE e.typarray = t.oid)
  UNION ALL SELECT 'schema', n.oid, n.nspname::text FROM pg_catalog.pg_namespace n WHERE n.nspowner = $1
  UNION ALL SELECT 'large object', l.oid, l.oid::text FROM pg_catalog.pg_largeobject_metadata l WHERE l.lomowner = $1
  UNION ALL SELECT 'operator', o.oid, o.oid::pg_catalog.regoperator::text FROM pg_catalog.pg_operator o WHERE o.oprowner = $1
  UNION ALL SELECT 'operator class', c.oid, c.opcname::text FROM pg_catalog.pg_opclass c WHERE c.opcowner = $1
  UNION ALL SELECT 'operator family', f.oid, f.opfname::text FROM pg_catalog.pg_opfamily f WHERE f.opfowner = $1
  UNION ALL SELECT 'collation', c.oid, c.oid::pg_catalog.regcollation::text FROM pg_catalog.pg_collation c WHERE c.collowner = $1
  UNION ALL SELECT 'conversion', c.oid, c.conname::text FROM pg_catalog.pg_conversion c WHERE c.conowner = $1
  UNION ALL SELECT 'text search configuration', c.oid, c.oid::pg_catalog.regconfig::text FROM pg_catalog.pg_ts_config c WHERE c.cfgowner = $1
  UNION ALL SELECT 'text search dictionary', d.oid, d.oid::pg_catalog.regdictionary::text FROM pg_catalog.pg_ts_dict d WHERE d.dictowner = $1
  UNION ALL SELECT 'statistics object', x.oid, x.stxname::text FROM pg_catalog.pg_statistic_ext x WHERE x.stxowner = $1
  UNION ALL SELECT 'foreign-data wrapper', w.oid, w.fdwname::text FROM pg_catalog.pg_foreign_data_wrapper w WHERE w.fdwowner = $1
  UNION ALL SELECT 'foreign server', v.oid, v.srvname::text FROM pg_catalog.pg_foreign_server v WHERE v.srvowner = $1
  UNION ALL SELECT 'event trigger', e.oid, e.evtname::text FROM pg_catalog.pg_event_trigger e WHERE e.evtowner = $1
  UNION ALL SELECT 'extension', x.oid, x.extname::text FROM pg_catalog.pg_extension x WHERE x.extowner = $1
  UNION ALL SELECT 'language', g.oid, g.lanname::text FROM pg_catalog.pg_language g WHERE g.lanowner = $1
  UNION ALL SELECT 'publication', b.oid, b.pubname::text FROM pg_catalog.pg_publication b WHERE b.pubowner = $1
  UNION ALL SELECT 'subscription', b.oid, b.subname::text FROM pg_catalog.pg_subscription b WHERE b.subowner = $1
  UNION ALL SELECT 'database', d.oid, d.datname::text FROM pg_catalog.pg_database d WHERE d.datdba = $1
  UNION ALL SELECT 'tablespace', t.oid, t.spcname::text FROM pg_catalog.pg_tablespace t WHERE t.spcowner = $1
  UNION ALL SELECT 'default privileges', d.oid, COALESCE(n.nspname || ' ', '') || 'for ' || d.defaclobjtype::text
    FROM pg_catalog.pg_default_acl d LEFT JOIN pg_catalog.pg_namespace n ON n.oid = d.defaclnamespace WHERE d.defaclrole = $1
  ORDER BY 1, 3`, oid)
	if err != nil {
		return nil, err
	}
	var faults []string
	for rows.Next() {
		var kind, object string
		var objectOID uint32
		if err := rows.Scan(&kind, &objectOID, &object); err != nil {
			rows.Close()
			return nil, err
		}
		if owner, ok := named[objectOID]; kind == "function" && ok && owner == role {
			continue
		}
		faults = append(faults, "the confined role "+role+" owns the "+kind+" "+object+", which the manifest does not name for it")
	}
	if err := closed(rows); err != nil {
		return nil, err
	}
	// A membership gives the role the rights of another role; a SECURITY DEFINER function of it would use them.
	rows, err = tx.QueryContext(ctx, `SELECT g.rolname FROM pg_catalog.pg_auth_members m JOIN pg_catalog.pg_roles g ON g.oid = m.roleid
  WHERE m.member = $1 ORDER BY 1`, oid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var group string
		if err := rows.Scan(&group); err != nil {
			rows.Close()
			return nil, err
		}
		faults = append(faults, "the confined role "+role+" is a member of the role "+group+", which the manifest does not name for it")
	}
	if err := closed(rows); err != nil {
		return nil, err
	}
	// A member of the role gets its rights, and through its SECURITY DEFINER functions the rows of every tenant: the
	// owner of the manifest alone may be one, to make the functions as the role, WITH SET TRUE, INHERIT FALSE and no
	// ADMIN, as the role migration of the service grants it.
	// LEFT JOIN: a membership whose member or grantor the catalog does not name still counts, and fails closed.
	rows, err = tx.QueryContext(ctx, `SELECT COALESCE(mr.rolname::text, m.member::text), m.admin_option, m.inherit_option, m.set_option,
  COALESCE(gr.rolname::text, ''), COALESCE(gr.rolsuper, false)
  FROM pg_catalog.pg_auth_members m LEFT JOIN pg_catalog.pg_roles mr ON mr.oid = m.member LEFT JOIN pg_catalog.pg_roles gr ON gr.oid = m.grantor
  WHERE m.roleid = $1 ORDER BY 1`, oid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var member, grantor string
		var admin, inherit, set, superuser bool
		if err := rows.Scan(&member, &admin, &inherit, &set, &grantor, &superuser); err != nil {
			rows.Close()
			return nil, err
		}
		if member != owner || admin || inherit || !set {
			fault := "the role " + member + " is a member of the confined role " + role + ", which the manifest grants only to its owner WITH SET TRUE, INHERIT FALSE and no ADMIN"
			// A login with CREATEROLE that made the role holds it WITH ADMIN, granted by the bootstrap superuser,
			// and cannot revoke it itself.
			if admin && superuser {
				// Each name is a quoted SQL identifier, so a role name cannot make the remedy run other SQL.
				fault += "; a superuser runs REVOKE " + quoteIdent(role) + " FROM " + quoteIdent(member) + " GRANTED BY " + quoteIdent(grantor) +
					", or the role migration runs as a superuser"
			}
			faults = append(faults, fault)
		}
	}
	if err := closed(rows); err != nil {
		return nil, err
	}
	// A privilege on a relation that is not a table of the manifest, which the grants of a table of the manifest do
	// not hold, would reach rows of another tenant or another service.
	rows, err = tx.QueryContext(ctx, `SELECT DISTINCT c.oid, c.oid::pg_catalog.regclass::text FROM pg_catalog.pg_class c, pg_catalog.aclexplode(c.relacl) a
  WHERE c.relacl IS NOT NULL AND a.grantee = $1 AND c.relowner <> $1
  UNION SELECT DISTINCT c.oid, c.oid::pg_catalog.regclass::text FROM pg_catalog.pg_attribute t JOIN pg_catalog.pg_class c ON c.oid = t.attrelid,
    pg_catalog.aclexplode(t.attacl) a
  WHERE t.attacl IS NOT NULL AND NOT t.attisdropped AND a.grantee = $1 AND c.relowner <> $1 ORDER BY 2`, oid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var rel uint32
		var name string
		if err := rows.Scan(&rel, &name); err != nil {
			rows.Close()
			return nil, err
		}
		if !tables[rel] {
			faults = append(faults, "the confined role "+role+" holds a privilege on the relation "+name+", which the manifest does not name for it")
		}
	}
	return faults, closed(rows)
}

// checkDefaults gives the faults of the defaults of the columns of the table oid against columns: a default of a
// column that the manifest states none for, listed or not, a generated column, and a default that is not the one of
// the manifest. Two literal defaults are the same when their values are; any other default must have the text of
// the manifest. The value of a default is read only when both are literals (literalDefault), so no function of the
// database runs.
//
// On the table of a declared entity (entity), a default of a column that the manifest does not list is accepted when
// it is a constant literal (literalDefault) whose cast is to a type of builtinType, on a column whose type is one of
// builtinType, which a later add step gives its new column, and no other default; Check reads no value of it. The
// default holds no function call, and checkTable refuses an unlisted column of any table whose type is no built-in
// base type of pg_catalog, such as a domain with a CHECK that calls a function, so an INSERT that leaves the column
// out runs no function of the database for it (decision on ST-GEN-Q15 (a); the type rule is the decision of the
// orchestrator under the owner's delegation, 2026-11-13). A listed column keeps the default of the manifest.
func checkDefaults(ctx context.Context, tx *sql.Tx, oid uint32, columns []Column, entity bool) ([]string, error) {
	want := map[string]string{}
	listed := map[string]bool{}
	for _, c := range columns {
		want[c.Name] = c.Default
		listed[c.Name] = true
	}
	type stored struct{ name, expr, typ, generated string }
	var defaults []stored
	rows, err := tx.QueryContext(ctx, `SELECT a.attname, pg_catalog.pg_get_expr(d.adbin, d.adrelid), pg_catalog.format_type(a.atttypid, a.atttypmod),
  a.attgenerated::text
  FROM pg_catalog.pg_attrdef d JOIN pg_catalog.pg_attribute a ON a.attrelid = d.adrelid AND a.attnum = d.adnum
  WHERE d.adrelid = $1 AND NOT a.attisdropped ORDER BY a.attnum`, oid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d stored
		if err := rows.Scan(&d.name, &d.expr, &d.typ, &d.generated); err != nil {
			rows.Close()
			return nil, err
		}
		defaults = append(defaults, d)
	}
	if err := closed(rows); err != nil {
		return nil, err
	}
	var faults []string
	got := map[string]bool{}
	for _, d := range defaults {
		got[d.name] = true
		expected := want[d.name]
		switch {
		case d.generated != "":
			faults = append(faults, "column "+d.name+" is a generated column, which the manifest does not state")
		case expected == "" && entity && !listed[d.name]:
			if _, _, ok := literal(d.expr); !ok || !builtinType.MatchString(d.typ) {
				faults = append(faults, "column "+d.name+" has a default that the manifest does not state, and that is not a constant of a built-in type")
			}
		case expected == "":
			faults = append(faults, "column "+d.name+" has a default that the manifest does not state")
		case d.expr == expected:
		case literalPair(d.expr, expected) && builtinType.MatchString(d.typ):
			// Both are constants of built-in types: compare their values as the column's type, with the literals as
			// parameters and only built-in type names in the statement.
			gotText, gotType, _ := literal(d.expr)
			wantText, wantType, _ := literal(expected)
			var same bool
			if err := tx.QueryRowContext(ctx, "SELECT ($1::"+gotType+")::"+d.typ+" IS NOT DISTINCT FROM ($2::"+wantType+")::"+d.typ,
				gotText, wantText).Scan(&same); err != nil {
				return nil, err
			}
			if !same {
				faults = append(faults, "column "+d.name+" has another default than the manifest states")
			}
		default:
			faults = append(faults, "column "+d.name+" has another default than the manifest states")
		}
	}
	for _, c := range columns {
		if c.Default != "" && !got[c.Name] {
			faults = append(faults, "column "+c.Name+" has no default, and the manifest states one")
		}
	}
	return faults, nil
}

// identityMax is the maximum of the sequence of an identity of each integer type.
var identityMax = map[string]int64{"smallint": 32767, "integer": 2147483647, "bigint": 9223372036854775807}

// checkIdentities gives the faults of the identity columns of the table oid against columns: an identity on a column
// that the manifest states none for, listed or not, another kind of identity, a stated identity that is missing, and
// an identity whose sequence does not count up by 1 from 1 to the maximum of its type with a cache of 1 and no cycle.
// A column of the manifest that a serial sequence fills (a default of nextval) needs the same checks of its
// sequence; no manifest states one yet. A RESTART is not seen here, as the role of the check cannot read the
// sequence: the migration job checks it (ST-SUP-RP14-LC03).
func checkIdentities(ctx context.Context, tx *sql.Tx, oid uint32, columns []Column) ([]string, error) {
	want := map[string]string{}
	for _, c := range columns {
		want[c.Name] = c.Identity
	}
	type identity struct{ name, kind, typ string }
	var identities []identity
	rows, err := tx.QueryContext(ctx, `SELECT a.attname, a.attidentity::text, pg_catalog.format_type(a.atttypid, a.atttypmod)
  FROM pg_catalog.pg_attribute a WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped AND a.attidentity <> '' ORDER BY a.attnum`, oid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var i identity
		if err := rows.Scan(&i.name, &i.kind, &i.typ); err != nil {
			rows.Close()
			return nil, err
		}
		identities = append(identities, i)
	}
	if err := closed(rows); err != nil {
		return nil, err
	}
	var faults []string
	got := map[string]bool{}
	for _, i := range identities {
		got[i.name] = true
		switch {
		case want[i.name] == "":
			faults = append(faults, "column "+i.name+" is an identity that the manifest does not state")
			continue
		case want[i.name] != i.kind:
			faults = append(faults, "column "+i.name+" is another identity than the manifest states")
			continue
		}
		var increment, minimum, maximum, cache int64
		var cycle bool
		var seqType string
		err := tx.QueryRowContext(ctx, `SELECT s.seqincrement, s.seqmin, s.seqmax, s.seqcache, s.seqcycle, pg_catalog.format_type(s.seqtypid, NULL)
  FROM pg_catalog.pg_sequence s WHERE s.seqrelid = pg_catalog.pg_get_serial_sequence(($1::pg_catalog.oid)::pg_catalog.regclass::text, $2)::pg_catalog.regclass`,
			oid, i.name).Scan(&increment, &minimum, &maximum, &cache, &cycle, &seqType)
		if errors.Is(err, sql.ErrNoRows) {
			faults = append(faults, "column "+i.name+" is an identity with no sequence")
			continue
		}
		if err != nil {
			return nil, err
		}
		// A cache above 1 gives each session a block of its own, so a later insert of another session can take a smaller
		// value than an earlier one.
		if top, ok := identityMax[i.typ]; !ok || increment != 1 || minimum != 1 || maximum != top || cache != 1 || cycle || seqType != i.typ {
			faults = append(faults, "column "+i.name+" is an identity whose sequence does not count up by 1 from 1 to the maximum of its type with a cache of 1 and no cycle")
		}
	}
	for _, c := range columns {
		if c.Identity != "" && !got[c.Name] {
			faults = append(faults, "column "+c.Name+" is no identity, and the manifest states one")
		}
	}
	return faults, nil
}

// literalPair tells that both defaults are literals of built-in types.
func literalPair(got, want string) bool {
	_, _, gotOK := literal(got)
	_, _, wantOK := literal(want)
	return gotOK && wantOK
}

// quoteIdent quotes a name as an SQL identifier, with each double quote doubled.
func quoteIdent(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// sortedJoin gives the names sorted and joined by the separator of the catalog reads, chr(31).
func sortedJoin(names []string) string {
	sorted := slices.Clone(names)
	slices.Sort(sorted)
	return strings.Join(sorted, "\x1f")
}

// checkGrants gives the faults of the privileges of the table oid and its columns against grants: each privilege
// that a role other than the owner holds, PUBLIC as grantee 0, and that grants does not name, each one with the grant
// option, and each privilege of grants that the table does not give. Each fault names the role, the privilege and the
// column, which are objects of the catalog.
func checkGrants(ctx context.Context, tx *sql.Tx, oid uint32, grants []Grant) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_catalog.pg_get_userbyid(a.grantee) END, a.privilege_type, '', a.is_grantable
  FROM pg_catalog.pg_class c, pg_catalog.aclexplode(c.relacl) a WHERE c.oid = $1 AND c.relacl IS NOT NULL AND a.grantee <> c.relowner
  UNION ALL
  SELECT CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_catalog.pg_get_userbyid(a.grantee) END, a.privilege_type, t.attname, a.is_grantable
  FROM pg_catalog.pg_attribute t JOIN pg_catalog.pg_class c ON c.oid = t.attrelid, pg_catalog.aclexplode(t.attacl) a
  WHERE t.attrelid = $1 AND t.attacl IS NOT NULL AND NOT t.attisdropped AND a.grantee <> c.relowner`, oid)
	if err != nil {
		return nil, err
	}
	got := map[string]bool{}
	var faults []string
	for rows.Next() {
		var role, privilege, column string
		var grantable bool
		if err := rows.Scan(&role, &privilege, &column, &grantable); err != nil {
			rows.Close()
			return nil, err
		}
		key := grantKey(role, privilege, column)
		if grantable {
			faults = append(faults, "the privilege "+key+" is granted with the grant option")
		}
		got[key] = true
	}
	if err := closed(rows); err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, g := range grants {
		for _, privilege := range g.Privileges {
			if len(g.Columns) == 0 {
				want[grantKey(g.Role, privilege, "")] = true
			}
			for _, column := range g.Columns {
				want[grantKey(g.Role, privilege, column)] = true
			}
		}
	}
	var extra, missing []string
	for key := range got {
		if !want[key] {
			extra = append(extra, key)
		}
	}
	for key := range want {
		if !got[key] {
			missing = append(missing, key)
		}
	}
	slices.Sort(extra)
	slices.Sort(missing)
	for _, key := range extra {
		faults = append(faults, "the privilege "+key+" is granted, and the manifest does not grant it")
	}
	for _, key := range missing {
		faults = append(faults, "the privilege "+key+" of the manifest is not granted")
	}
	return faults, nil
}

// grantKey names one privilege of a role, such as SELECT to runtime or INSERT (payload) to worker.
func grantKey(role, privilege, column string) string {
	if column != "" {
		return privilege + " (" + column + ") to " + role
	}
	return privilege + " to " + role
}

// checkFunction gives the faults of one function of the manifest that takes no argument: missing, or with another
// language, source, SECURITY DEFINER, settings or owner. Its fault names the function only.
func checkFunction(ctx context.Context, tx *sql.Tx, f Function, wantOwner string) ([]string, error) {
	schema, fn, _ := strings.Cut(f.Name, ".")
	var language, source, config, owner, grantees, volatility, result string
	var definer, strict bool
	// A function with arguments is found by its signature, and one with none by its name and no argument.
	where := `n.nspname = $1 AND p.proname = $2 AND p.pronargs = 0`
	args := []any{schema, fn}
	if f.Args != "" {
		where = `p.oid = pg_catalog.to_regprocedure($1)`
		args = []any{f.Name + "(" + f.Args + ")"}
	}
	// The grantees of EXECUTE other than the owner, by name, PUBLIC for grantee 0, each marked when it holds the grant
	// option, which would let it give EXECUTE on; a function with no ACL has the default one, which gives EXECUTE to
	// PUBLIC.
	err := tx.QueryRowContext(ctx, `SELECT l.lanname, p.prosrc, COALESCE(pg_catalog.array_to_string(p.proconfig, chr(31)), ''), p.prosecdef,
  pg_catalog.pg_get_userbyid(p.proowner),
  COALESCE((SELECT pg_catalog.string_agg(CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_catalog.pg_get_userbyid(a.grantee) END ||
      CASE WHEN a.is_grantable THEN ' WITH GRANT OPTION' ELSE '' END, chr(31))
    FROM pg_catalog.aclexplode(COALESCE(p.proacl, pg_catalog.acldefault('f', p.proowner))) a
    WHERE a.privilege_type = 'EXECUTE' AND a.grantee <> p.proowner), ''),
  p.proisstrict, p.provolatile::text, pg_catalog.pg_get_function_result(p.oid)
  FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace JOIN pg_catalog.pg_language l ON l.oid = p.prolang
  WHERE `+where, args...).Scan(&language, &source, &config, &definer, &owner, &grantees, &strict, &volatility, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return []string{"function " + f.Name + " is missing"}, nil
	}
	if err != nil {
		return nil, err
	}
	wantVolatility := f.Volatility
	if wantVolatility == "" {
		wantVolatility = "v"
	}
	switch {
	case language != f.Language || source != f.Source || definer != f.SecurityDefiner || config != strings.Join(f.Config, "\x1f"):
		return []string{"function " + f.Name + " is not the function of the manifest"}, nil
	case strict != f.Strict || volatility != wantVolatility || result != f.Returns:
		return []string{"function " + f.Name + " has another strictness, volatility or result than the manifest"}, nil
	case f.Owner != "" && owner != f.Owner, f.Owner == "" && wantOwner != "" && owner != wantOwner:
		return []string{"function " + f.Name + " has another owner"}, nil
	}
	// The grantees are sorted here, byte by byte, and not by the collation of the database.
	if sortedJoin(strings.Split(grantees, "\x1f")) != sortedJoin(f.Execute) {
		return []string{"function " + f.Name + " may be run by roles that the manifest does not name"}, nil
	}
	return nil, nil
}

// checkTable gives the faults of one table.
func checkTable(ctx context.Context, tx *sql.Tx, t Table, wantOwner string, steps *stepState) ([]string, error) {
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
	// The step rules hold only on a plain table that is no partition, inherits from no table and that no table
	// inherits from (ST-SUP-RP15-LC02 rule 1).
	plainTable := kind == "r" && !partition && !child && !parent
	var stepCols map[string]tableColumn
	var keyed map[string]bool
	if t.Entity && plainTable {
		if stepCols, keyed, err = stepColumns(ctx, tx, oid); err != nil {
			return nil, err
		}
	}

	type column struct {
		typ, collation string
		notNull        bool
		plainCollation bool
		builtin        bool
	}
	columns := map[string]column{}
	rows, err := tx.QueryContext(ctx, `SELECT a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod), a.attnotnull,
  a.attcollation IN (0, 100, 950), COALESCE(cn.nspname || '.' || co.collname, ''), `+builtinTypeSQL+`
  FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_type ty ON ty.oid = a.atttypid
  LEFT JOIN pg_catalog.pg_collation co ON co.oid = a.attcollation
  LEFT JOIN pg_catalog.pg_namespace cn ON cn.oid = co.collnamespace
  WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped`, oid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n string
		var c column
		if err := rows.Scan(&n, &c.typ, &c.notNull, &c.plainCollation, &c.collation, &c.builtin); err != nil {
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
	// A column that the manifest does not list is of a built-in base type of pg_catalog: a domain, an enum, a range or
	// a type of another schema can run a function of the database on each INSERT that leaves the column out, such as
	// the CHECK of a domain (decisions of the orchestrator under the owner's delegation, 2026-11-13: an entity table
	// refuses an unlisted column of a non-built-in type, and so does each other table of the manifest).
	{
		listed := map[string]bool{}
		for _, c := range t.Columns {
			listed[c.Name] = true
		}
		var unlisted []string
		for n, c := range columns {
			if !listed[n] && !c.builtin {
				unlisted = append(unlisted, n)
			}
		}
		slices.Sort(unlisted)
		for _, n := range unlisted {
			fault("column %s is not in the manifest and has the type %s, which is no built-in base type of pg_catalog", n, columns[n].typ)
		}
	}
	found, err := checkDefaults(ctx, tx, oid, t.Columns, t.Entity)
	if err != nil {
		return nil, err
	}
	for _, f := range found {
		fault("%s", f)
	}
	found, err = checkIdentities(ctx, tx, oid, t.Columns)
	if err != nil {
		return nil, err
	}
	for _, f := range found {
		fault("%s", f)
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
	if t.RowSecurity || t.CheckGrants {
		// A referential action runs with the rights of the table owner and passes row-level security and the
		// grants, so on a table with row-level security or that checks its grants each foreign key is the
		// manifest's.
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
			// On the table of a declared entity, the foreign key of a later add step is accepted in its exact form
			// (ST-SUP-RP15-LC03).
			if t.Entity && plainTable {
				ok, err := acceptForeignKey(ctx, tx, t, oid, n, steps, stepCols)
				if err != nil {
					return nil, err
				}
				if ok {
					continue
				}
			}
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
		command, using, withCheck, roles string
		permissive, public               bool
	}
	policies := map[string]policy{}
	rows, err = tx.QueryContext(ctx, `SELECT polname, polcmd::text, polpermissive, polroles = '{0}'::pg_catalog.oid[],
  COALESCE(pg_catalog.pg_get_expr(polqual, polrelid), ''), COALESCE(pg_catalog.pg_get_expr(polwithcheck, polrelid), ''),
  COALESCE((SELECT pg_catalog.string_agg(CASE WHEN r = 0 THEN 'PUBLIC' ELSE pg_catalog.pg_get_userbyid(r) END, chr(31))
    FROM pg_catalog.unnest(polroles) AS r), '')
  FROM pg_catalog.pg_policy WHERE polrelid = $1`, oid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n string
		var p policy
		if err := rows.Scan(&n, &p.command, &p.permissive, &p.public, &p.using, &p.withCheck, &p.roles); err != nil {
			rows.Close()
			return nil, err
		}
		// polroles keeps the order of the statement, so the names are sorted here, byte by byte as sortedJoin sorts.
		p.roles = sortedJoin(strings.Split(p.roles, "\x1f"))
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
		case !got.permissive || got.command != want.Command || got.using != want.Using || got.withCheck != want.WithCheck:
			fault("policy %s is not the policy of the manifest", want.Name)
		case len(want.Roles) == 0 && !got.public:
			fault("policy %s is not the policy of the manifest", want.Name)
		case len(want.Roles) > 0 && got.roles != sortedJoin(want.Roles):
			fault("policy %s applies to other roles than the manifest names", want.Name)
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

	if t.CheckGrants {
		found, err := checkGrants(ctx, tx, oid, t.Grants)
		if err != nil {
			return nil, err
		}
		for _, f := range found {
			fault("%s", f)
		}
	}

	triggers, err := readTriggers(ctx, tx, oid, owner)
	if err != nil {
		return nil, err
	}
	namedTriggers := map[string]bool{}
	namedRows := map[uint32]string{}
	// An optional trigger may be missing; when the table has a trigger of its name, it is the trigger of the manifest.
	// The columns that a present optional sync trigger moves are in no other sync trigger (ST-SUP-RP15-LC02 rule 2).
	used := map[string]bool{}
	for _, want := range t.OptionalTriggers {
		namedTriggers[want.Name] = true
		got, ok := triggers[want.Name]
		if !ok {
			continue
		}
		if f := triggerFault(want, got); f != "" {
			fault("trigger %s %s", want.Name, f)
		}
		if stepCols != nil {
			var pairs [][2]string
			for k := range stepCols {
				for u := range stepCols {
					if k != u {
						pairs = append(pairs, [2]string{k, u})
					}
				}
			}
			if k, u, _, _, ok := syncPair(got.source, pairs, stepCols); ok {
				used[k], used[u] = true, true
			}
		}
	}
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
	if t.RowSecurity || t.CheckGrants {
		// A trigger runs on every row that a role writes, and can drop, change or copy it, so on a table with
		// row-level security or that checks its grants, such as the revocation floor, each trigger is the
		// manifest's.
		var extra []string
		for n := range triggers {
			if !namedTriggers[n] {
				extra = append(extra, n)
			}
		}
		slices.Sort(extra)
		// On the table of a declared entity, one sync trigger of a later step is accepted in its strict template form
		// (ST-SUP-RP15-LC02); two or more are each refused.
		accepted := map[string]uint32{}
		if t.Entity && plainTable {
			for _, n := range extra {
				function, _, ok, err := acceptSync(ctx, tx, t, oid, n, steps, stepCols, keyed, used)
				if err != nil {
					return nil, err
				}
				if ok {
					accepted[n] = function
				}
			}
		}
		for _, n := range extra {
			if function, ok := accepted[n]; ok && len(accepted) == 1 {
				steps.accepted[function] = true
				continue
			}
			if _, ok := accepted[n]; ok {
				fault("trigger %s is one of more than one sync trigger that the manifest does not name", n)
				continue
			}
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
	config, functionOwner                   string
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
  p.proowner = $2, pg_catalog.pg_get_userbyid(p.proowner)
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
			&tr.securityDefiner, &tr.plain, &tr.fits, &tr.ownerSame, &tr.functionOwner); err != nil {
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
	case want.Function.Owner != "" && got.functionOwner != want.Function.Owner:
		return "runs the function " + want.Function.Name + ", which the role " + want.Function.Owner + " does not own"
	case want.Function.Owner == "" && !got.ownerSame:
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
