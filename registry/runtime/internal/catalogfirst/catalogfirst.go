// Package catalogfirst gives the catalog-first forms of the statements of a migration text that take a strong lock
// on a table even when they change nothing: ENABLE and FORCE ROW LEVEL SECURITY, DROP and CREATE POLICY, CREATE
// TRIGGER and CREATE INDEX IF NOT EXISTS. Each form is a DO block that reads the catalog first and runs the
// statements only when the catalog does not hold the object as the text makes it, so a run of the migration job
// with nothing to change takes no lock on a table that conflicts with the reads and writes of the running release
// (followups, orchestrator 2026-11-14; ST-SUP-RP14-LC03). A catalog that differs, such as a policy that an owner
// changed or a dropped index, gets the statements, so a run again still repairs the object.
//
// A relation, a role and a function are SQL text expressions, such as 'stego_outbox.messages' or a placeholder that
// the text replaces with a SQL string, so a name that a caller quotes is cast in the catalog query and never
// concatenated into it raw.
package catalogfirst

import (
	"strconv"
	"strings"
)

// TenantForm is the USING and WITH CHECK expression of a tenant policy, tenant_id =
// pg_catalog.current_setting('app.tenant', true), as pg_get_expr gives it.
const TenantForm = "(tenant_id = current_setting('app.tenant'::text, true))"

// TrueForm is the expression true as pg_get_expr gives it.
const TrueForm = "true"

// EveryCommand is the command of a policy for every command (FOR ALL), as pg_policy.polcmd gives it.
const EveryCommand = "*"

// The bits of pg_trigger.tgtype.
const (
	Row      = 1 << 0
	Before   = 1 << 1
	Insert   = 1 << 2
	Delete   = 1 << 3
	Update   = 1 << 4
	Truncate = 1 << 5
)

// Unless gives a DO block that runs statements only when condition, a SQL boolean expression that reads the
// catalog, is not true.
func Unless(condition, statements string) string {
	return "DO $catalog$ BEGIN\nIF NOT coalesce((" + condition + "), false) THEN\n" + strings.TrimRight(statements, "\n") +
		"\nEND IF;\nEND $catalog$;\n"
}

// Text gives the SQL string of s.
func Text(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// RowSecurity tells whether row-level security is enabled and forced on relation.
func RowSecurity(relation string) string {
	return "EXISTS (SELECT 1 FROM pg_catalog.pg_class AS c WHERE c.oid = (" + relation +
		")::pg_catalog.regclass AND c.relrowsecurity AND c.relforcerowsecurity)"
}

// Exists tells whether relation, such as an index, exists.
func Exists(relation string) string {
	return "pg_catalog.to_regclass(" + relation + ") IS NOT NULL"
}

// Policy is one permissive row-level security policy as the catalog holds it: its relation and name, its command as
// pg_policy.polcmd gives it (* for all, r, a, w or d), the roles it applies to (none for PUBLIC), and its USING and
// WITH CHECK expressions as pg_get_expr gives them (empty for none).
type Policy struct {
	Relation, Name, Command string
	Roles                   []string
	Using, Check            string
}

// Holds tells whether the catalog holds the policy p, on the relation p names, with this command, these roles and
// no other, and these expressions.
func (p Policy) Holds() string {
	roles := "'{0}'::pg_catalog.oid[]"
	if len(p.Roles) > 0 {
		var cast []string
		for _, r := range p.Roles {
			cast = append(cast, "("+r+")::pg_catalog.regrole::pg_catalog.oid")
		}
		roles = "ARRAY[" + strings.Join(cast, ", ") + "]"
	}
	return "EXISTS (SELECT 1 FROM pg_catalog.pg_policy AS p WHERE p.polrelid = (" + p.Relation + ")::pg_catalog.regclass" +
		" AND p.polname = " + Text(p.Name) + " AND p.polcmd = " + Text(p.Command) + " AND p.polpermissive" +
		" AND (SELECT pg_catalog.array_agg(r ORDER BY r) FROM pg_catalog.unnest(p.polroles) AS r)" +
		" = (SELECT pg_catalog.array_agg(r ORDER BY r) FROM pg_catalog.unnest(" + roles + ") AS r)" +
		" AND pg_catalog.pg_get_expr(p.polqual, p.polrelid) IS NOT DISTINCT FROM " + form(p.Using) +
		" AND pg_catalog.pg_get_expr(p.polwithcheck, p.polrelid) IS NOT DISTINCT FROM " + form(p.Check) + ")"
}

// form gives the SQL string of an expression form, or NULL for none.
func form(s string) string {
	if s == "" {
		return "NULL"
	}
	return Text(s)
}

// Trigger is one trigger as the catalog holds it: its relation and name, the function it runs (a regprocedure,
// such as 'public.f()'), its pg_trigger.tgtype, and for a constraint trigger that is deferrable and initially
// deferred, Deferred. It is enabled, with no argument, no column list and no WHEN condition.
type Trigger struct {
	Relation, Name, Function string
	Type                     int
	Deferred                 bool
}

// Holds tells whether the catalog holds the trigger t as it states it.
func (t Trigger) Holds() string {
	deferred := strconv.FormatBool(t.Deferred)
	return "EXISTS (SELECT 1 FROM pg_catalog.pg_trigger AS g WHERE g.tgrelid = (" + t.Relation + ")::pg_catalog.regclass" +
		" AND g.tgname = " + Text(t.Name) + " AND NOT g.tgisinternal AND g.tgfoid = (" + t.Function + ")::pg_catalog.regprocedure" +
		" AND g.tgtype = " + strconv.Itoa(t.Type) + " AND g.tgenabled = 'O' AND g.tgnargs = 0 AND g.tgqual IS NULL" +
		" AND pg_catalog.cardinality(g.tgattr::pg_catalog.int2[]) = 0" +
		" AND (g.tgconstraint <> 0) = " + deferred + " AND g.tgdeferrable = " + deferred + " AND g.tginitdeferred = " + deferred + ")"
}
