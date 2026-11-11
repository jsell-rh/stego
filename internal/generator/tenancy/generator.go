// Package tenancy generates the tenant wall of a pooled service on top of github.com/jsell-rh/stego/registry/runtime/tenant
// (ST-SUP-RC27): the row-level security migration of the owned tables and the role migration. The tenant column
// and its keys are the PostgreSQL adapter's (ST-SUP-RC01-OC40), the grants of the runtime role come from the
// database access contract, and the transaction of each read and write is the command and query runtime's.
package tenancy

import (
	"fmt"
	"go/format"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
	"github.com/jsell-rh/stego/internal/types"
)

// Generator is the generator of the tenancy component.
type Generator struct{}

// adapter is the component whose tables the row-level security covers and whose ledger runs the migration.
const adapter = "postgres-adapter"

// policyName is the name of the one tenant policy of each owned table.
const policyName = "stego_tenant"

// roleName is the grammar of a role name: a lower-case SQL name that the database access contract accepts.
var roleName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// MinimumGoVersion is the Go version of the emitted package.
func (*Generator) MinimumGoVersion() string { return "1.25.0" }

// ValidateContext refuses a service that the tenancy component cannot serve: one without pooled tenancy, one
// without the PostgreSQL adapter, and role names that are not plain SQL names or that are the same.
func (*Generator) ValidateContext(ctx gen.Context) error {
	if err := gen.ValidateGoPackageNamespace(ctx.OutputNamespace); err != nil {
		return err
	}
	if ctx.Tenancy == nil {
		return fmt.Errorf("the %s component needs pooled tenancy; a service with tenancy kind single keeps tenants apart by deployment", gen.TenancyComponent)
	}
	if ctx.PeerNamespaces[adapter] == "" {
		return fmt.Errorf("the %s component needs the %s component, whose tables it keeps apart", gen.TenancyComponent, adapter)
	}
	runtime, owner, err := roles(ctx)
	if err != nil {
		return err
	}
	if runtime == owner {
		return fmt.Errorf("the runtime role and the owner role are both %q; they must differ, so the runtime role owns nothing", runtime)
	}
	return checkPrefix(strings.TrimSuffix(runtime, "_runtime"))
}

// roleSuffixes are the endings that the role names of a service add to its role prefix.
var roleSuffixes = []string{"_runtime", "_owner", "_worker", "_relay", "_idempotency_purge", "_outbox_relay"}

// checkPrefix refuses a role prefix with which a role name of this service can be a role name of another service in
// the same cluster: when one suffix ends with another, such as _outbox_relay with _relay, a prefix that ends with the
// part before it, _outbox, gives the relay role shop_outbox_relay, which is the outbox relay role of the prefix shop
// (ST-SUP-RC27-OC04). Decided by the orchestrator under the Product owner's delegation, 2026-11-11 (ST-SUP-Q107, a).
func checkPrefix(prefix string) error {
	for _, long := range roleSuffixes {
		for _, short := range roleSuffixes {
			if long == short || !strings.HasSuffix(long, short) {
				continue
			}
			part := strings.TrimSuffix(long, short)
			if strings.HasSuffix(prefix, part) {
				return fmt.Errorf("the role prefix %q ends with %s, so its role %s%s is the role %s%s of the prefix %q of another service; set a runtime_role whose prefix does not end with %s",
					prefix, part, prefix, short, strings.TrimSuffix(prefix, part), long, strings.TrimSuffix(prefix, part), part)
			}
		}
	}
	return nil
}

// Generate emits the row-level security migration as Go text that the PostgreSQL adapter registers in its ledger
// and as a SQL file, and the role migration as a SQL file for the database administrator.
func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := g.ValidateContext(ctx); err != nil {
		return nil, nil, err
	}
	runtime, owner, err := roles(ctx)
	if err != nil {
		return nil, nil, err
	}
	security := rowSecurity(ctx.Entities, ctx.Tenancy.Key)
	worker, relay := workerRoles(runtime)
	purge, outboxRelay := purgeRole(runtime), outboxRelayRole(runtime)
	for _, role := range []string{worker, relay, purge, outboxRelay} {
		if !roleName.MatchString(role) {
			return nil, nil, fmt.Errorf("the role %q is not a lower-case SQL name of at most 63 characters; set a shorter runtime_role", role)
		}
	}
	all := []string{owner, runtime, worker, relay, purge, outboxRelay}
	for i, role := range all {
		if slices.Contains(all[i+1:], role) {
			return nil, nil, fmt.Errorf("the role %q is two roles of the service; set runtime_role and owner_role so each role has a name of its own", role)
		}
	}
	roleText := roleMigration(runtime, owner, worker, relay, purge, outboxRelay)
	ns := ctx.OutputNamespace
	source := fmt.Sprintf(`package %s

// RuntimeRole is the role the service runs as: it owns nothing and holds no BYPASSRLS (ST-SUP-RC27-OC04).
const RuntimeRole = %q

// OwnerRole is the role that owns the tables, for the migration job only.
const OwnerRole = %q

// WorkerRole is the role that the worker mode connects as, the only role that claims outbox messages, and
// RelayRole the role that the authorization relay connects as (ST-SUP-RC27-OC04).
const (
	WorkerRole = %q
	RelayRole  = %q
)

// PurgeRole is the NOLOGIN role of this service alone that owns the purge function of the idempotency key table
// (ST-SUP-RP07-LC11); the migration job gives it to the idempotency migration.
const PurgeRole = %q

// OutboxRelayRole is the NOLOGIN role of this service alone that owns the outbox relay functions and the state hook
// functions of the authorization tables (ST-SUP-RP01-LC19); the migration job gives it to the outbox and
// authorization migrations.
const OutboxRelayRole = %q

// RowSecurity enables and forces row-level security on every owned table, with one policy on the tenant column
// (ST-SUP-RC27-OC03). The PostgreSQL adapter registers it in its migration ledger after the tables.
const RowSecurity = %q

// Roles creates the runtime, owner, worker, relay, idempotency purge and outbox relay roles, gives CONNECT on the
// database only to the roles of the service, and the owner's SET membership, with no INHERIT, in the purge role and
// the outbox relay role (ST-SUP-RC27-OC04). The database administrator runs it once, before the migration job; a
// deployment gives each role its login and credential.
const Roles = %q
`, path.Base(ns), runtime, owner, worker, relay, purge, outboxRelay, security, roleText)
	formatted, err := format.Source([]byte(source))
	if err != nil {
		return nil, nil, fmt.Errorf("formatting the tenancy package: %w", err)
	}
	files := []gen.File{
		{Path: path.Join(ns, "row_security.go"), Content: formatted},
		{Path: path.Join(ns, "migrations/row_security.sql"), Content: []byte(security)},
		{Path: path.Join(ns, "migrations/roles.sql"), Content: []byte(roleText)},
	}
	if err := gen.ValidateNamespace(ns, files); err != nil {
		return nil, nil, err
	}
	return files, &gen.Wiring{}, nil
}

// roles gives the runtime role and the owner role: the settings runtime_role and owner_role, or the service name
// in lower case with _runtime and _owner.
func roles(ctx gen.Context) (string, string, error) {
	base := strings.ToLower(strings.NewReplacer("-", "_", ".", "_").Replace(ctx.ServiceName))
	runtime, owner := base+"_runtime", base+"_owner"
	for name, value := range map[string]*string{"runtime_role": &runtime, "owner_role": &owner} {
		setting, present := ctx.ComponentConfig[name]
		if !present || setting == nil {
			continue
		}
		text, ok := setting.(string)
		if !ok {
			return "", "", fmt.Errorf("the setting %s must be a role name", name)
		}
		*value = text
	}
	for _, role := range []string{runtime, owner} {
		if !roleName.MatchString(role) {
			return "", "", fmt.Errorf("the role %q is not a lower-case SQL name of at most 63 characters; set runtime_role and owner_role", role)
		}
	}
	return runtime, owner, nil
}

// rowSecurity gives the migration that enables and forces row-level security on the table of each owned entity,
// with one policy whose USING and WITH CHECK compare the tenant column with the tenant setting of the
// transaction. It drops the policy before it creates it, so a run on every start applies it again with no error.
func rowSecurity(entities []types.Entity, key string) string {
	var text strings.Builder
	setting := "pg_catalog.current_setting('app.tenant', true)"
	for _, entity := range entities {
		if entity.Tenant != "" && entity.Tenant != "owned" {
			continue
		}
		table := "public." + quote(postgresadapter.TableName(entity.Name))
		fmt.Fprintf(&text, "ALTER TABLE %s ENABLE ROW LEVEL SECURITY;\n", table)
		fmt.Fprintf(&text, "ALTER TABLE %s FORCE ROW LEVEL SECURITY;\n", table)
		fmt.Fprintf(&text, "DROP POLICY IF EXISTS %s ON %s;\n", policyName, table)
		fmt.Fprintf(&text, "CREATE POLICY %s ON %s USING (%s = %s) WITH CHECK (%s = %s);\n", policyName, table, quote(key), setting, quote(key), setting)
	}
	return text.String()
}

// workerRoles gives the worker role and the relay role of the runtime role: its name without the suffix _runtime,
// with _worker and _relay, such as shop_worker and shop_relay for shop_runtime.
func workerRoles(runtime string) (string, string) {
	base := strings.TrimSuffix(runtime, "_runtime")
	return base + "_worker", base + "_relay"
}

// outboxRelayRole gives the outbox relay role of the runtime role: its name without the suffix _runtime, with
// _outbox_relay, such as shop_outbox_relay for shop_runtime. It is one role for each service, which owns the outbox
// relay functions and the state hook functions of the authorization tables (ST-SUP-RP01-LC19).
func outboxRelayRole(runtime string) string {
	return strings.TrimSuffix(runtime, "_runtime") + "_outbox_relay"
}

// purgeRole gives the idempotency purge role of the runtime role: its name without the suffix _runtime, with
// _idempotency_purge, such as shop_idempotency_purge for shop_runtime. It is one role for each service, which owns
// the purge function of the idempotency key table (ST-SUP-RP07-LC11).
func purgeRole(runtime string) string {
	return strings.TrimSuffix(runtime, "_runtime") + "_idempotency_purge"
}

// roleMigration gives the migration that creates the roles of the service: the owner, runtime, worker, relay,
// idempotency purge and outbox relay roles (ST-SUP-RC27-OC04). None can log in until a deployment gives it a login,
// is a superuser, creates roles or databases, replicates or bypasses row-level security; the runtime, worker, relay,
// purge and outbox relay roles inherit nothing. CONNECT, CREATE and TEMPORARY on the database are revoked from PUBLIC, and CONNECT goes only to
// the owner, runtime, worker and relay roles of this service, so a role of another service in the same cluster
// cannot connect to its database. PostgreSQL checks CONNECT only when a session starts, so the text commits the
// revoke, with a COMMIT of its own, and then refuses to go on while a session of a role that is not a role of this
// service, nor a superuser, is open in the database, other than its own; the refusal names how many and no role,
// and the text runs again once they are closed. It terminates no session. The owner may create in the database and in the schema public, and may grant
// CREATE on public, which the idempotency migration gives the purge role only while it makes the purge function; the
// public role may not create in public, so the runtime role gets only the grants of the database access contract. It
// gives the owner membership in the purge role WITH SET TRUE, INHERIT FALSE, so the idempotency migration can make
// the purge function as that role, and the owner cannot read or delete the key rows of every tenant through it
// (ST-SUP-RP07-LC11). It gives the owner membership in the outbox relay role of the service WITH SET TRUE, INHERIT
// FALSE, so the outbox and authorization migrations can make the relay and state hook functions as that role, and
// the owner does not hold its rights on the messages of every tenant (ST-SUP-RP01-LC19); the outbox relay role gets
// no CREATE on public, which the authorization migration gives it only while it makes its state hook functions
// (ST-SUP-RP06-LC21). So the first migration job needs no role or grant that the database administrator makes by
// hand.
func roleMigration(runtime, owner, worker, relay, purge, outboxRelay string) string {
	flags := "NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"
	var text strings.Builder
	text.WriteString("DO $$\nBEGIN\n")
	for _, role := range []string{owner, runtime, worker, relay} {
		fmt.Fprintf(&text, "\tIF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = '%s') THEN\n\t\tCREATE ROLE %s %s;\n\tEND IF;\n", role, quote(role), flags)
	}
	for _, role := range []string{purge, outboxRelay} {
		fmt.Fprintf(&text, "\tIF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = '%s') THEN\n\t\tCREATE ROLE %s %s NOINHERIT;\n\tEND IF;\n", role, quote(role), flags)
	}
	fmt.Fprintf(&text, "\tEXECUTE pg_catalog.format('REVOKE CONNECT, CREATE, TEMPORARY ON DATABASE %%I FROM PUBLIC', pg_catalog.current_database());\n")
	fmt.Fprintf(&text, "\tEXECUTE pg_catalog.format('GRANT CONNECT, CREATE ON DATABASE %%I TO %s', pg_catalog.current_database());\n", quote(owner))
	for _, role := range []string{runtime, worker, relay} {
		fmt.Fprintf(&text, "\tEXECUTE pg_catalog.format('GRANT CONNECT ON DATABASE %%I TO %s', pg_catalog.current_database());\n", quote(role))
	}
	text.WriteString("END\n$$;\n")
	// CONNECT is checked only when a session starts, so the revoke is committed first, and then no session of
	// another role may still be open in the database: one opened before the revoke would keep its access.
	text.WriteString("COMMIT;\n")
	text.WriteString("DO $$\nDECLARE\n\tv_open bigint;\nBEGIN\n")
	fmt.Fprintf(&text, "\tSELECT pg_catalog.count(*) INTO v_open FROM pg_catalog.pg_stat_activity AS a\n"+
		"\t\tLEFT JOIN pg_catalog.pg_roles AS r ON r.oid = a.usesysid\n"+
		"\t\tWHERE a.datname = pg_catalog.current_database() AND a.pid <> pg_catalog.pg_backend_pid()\n"+
		"\t\t\tAND a.backend_type = 'client backend' AND NOT COALESCE(r.rolsuper, false)\n"+
		"\t\t\tAND COALESCE(r.rolname, '') NOT IN ('%s', '%s', '%s', '%s');\n", owner, runtime, worker, relay)
	text.WriteString("\tIF v_open > 0 THEN\n\t\tRAISE EXCEPTION 'tenancy: % sessions of roles that are not roles of this service are open in this database; close them, then run the role migration again', v_open;\n\tEND IF;\nEND\n$$;\n")
	fmt.Fprintf(&text, "ALTER ROLE %s %s;\n", quote(owner), flags)
	for _, role := range []string{runtime, worker, relay, purge, outboxRelay} {
		fmt.Fprintf(&text, "ALTER ROLE %s %s NOINHERIT;\n", quote(role), flags)
	}
	text.WriteString("REVOKE CREATE ON SCHEMA public FROM PUBLIC;\n")
	fmt.Fprintf(&text, "GRANT USAGE ON SCHEMA public TO %s;\n", quote(owner))
	fmt.Fprintf(&text, "GRANT CREATE ON SCHEMA public TO %s WITH GRANT OPTION;\n", quote(owner))
	for _, role := range []string{runtime, worker, relay} {
		fmt.Fprintf(&text, "GRANT USAGE ON SCHEMA public TO %s;\n", quote(role))
	}
	for _, role := range []string{purge, outboxRelay} {
		fmt.Fprintf(&text, "GRANT %s TO %s WITH SET TRUE, INHERIT FALSE;\n", quote(role), quote(owner))
	}
	return text.String()
}

// quote quotes one SQL name. The names it gets are checked plain names, so it only adds the quotes.
func quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }
