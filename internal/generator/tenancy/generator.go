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
	for _, role := range []string{worker, relay} {
		if !roleName.MatchString(role) {
			return nil, nil, fmt.Errorf("the role %q is not a lower-case SQL name of at most 63 characters; set a shorter runtime_role", role)
		}
	}
	roleText := roleMigration(runtime, owner, worker, relay)
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

// RowSecurity enables and forces row-level security on every owned table, with one policy on the tenant column
// (ST-SUP-RC27-OC03). The PostgreSQL adapter registers it in its migration ledger after the tables.
const RowSecurity = %q

// Roles creates the runtime, owner, worker and relay roles, the owner's membership in the outbox relay role and
// that role's CREATE on the schema public (ST-SUP-RC27-OC04). The database administrator runs it once, before the
// migration job; a deployment gives each role its login and credential.
const Roles = %q
`, path.Base(ns), runtime, owner, worker, relay, security, roleText)
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

// outboxRelayRole is the NOLOGIN role of the outbox runtime that owns its relay functions (ST-SUP-RP01-LC19).
const outboxRelayRole = "stego_outbox_relay"

// roleMigration gives the migration that creates the roles of the service: the owner, runtime, worker and relay
// roles (ST-SUP-RC27-OC04). None can log in until a deployment gives it a login, is a superuser, creates roles or
// databases, replicates or bypasses row-level security; the runtime, worker and relay roles inherit nothing. The
// owner may create in the database and in the schema public, and the public role may neither create in public nor
// make temporary tables, so the runtime role gets only the grants of the database access contract. It makes the
// outbox relay role of the runtime when it is missing, gives the owner role membership in it WITH SET TRUE,
// INHERIT TRUE, so the outbox migration can make the relay functions, and gives it CREATE on public, so the
// authorization migration can give it its state hook function (ST-SUP-RP06-LC21). So the first migration job needs
// no role or grant that the database administrator makes by hand.
func roleMigration(runtime, owner, worker, relay string) string {
	flags := "NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"
	var text strings.Builder
	text.WriteString("DO $$\nBEGIN\n")
	for _, role := range []string{owner, runtime, worker, relay, outboxRelayRole} {
		fmt.Fprintf(&text, "\tIF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = '%s') THEN\n\t\tCREATE ROLE %s %s;\n\tEND IF;\n", role, quote(role), flags)
	}
	fmt.Fprintf(&text, "\tEXECUTE pg_catalog.format('REVOKE CREATE, TEMPORARY ON DATABASE %%I FROM PUBLIC', pg_catalog.current_database());\n")
	fmt.Fprintf(&text, "\tEXECUTE pg_catalog.format('GRANT CONNECT, CREATE ON DATABASE %%I TO %s', pg_catalog.current_database());\n", quote(owner))
	for _, role := range []string{runtime, worker, relay} {
		fmt.Fprintf(&text, "\tEXECUTE pg_catalog.format('GRANT CONNECT ON DATABASE %%I TO %s', pg_catalog.current_database());\n", quote(role))
	}
	text.WriteString("END\n$$;\n")
	fmt.Fprintf(&text, "ALTER ROLE %s %s;\n", quote(owner), flags)
	for _, role := range []string{runtime, worker, relay} {
		fmt.Fprintf(&text, "ALTER ROLE %s %s NOINHERIT;\n", quote(role), flags)
	}
	text.WriteString("REVOKE CREATE ON SCHEMA public FROM PUBLIC;\n")
	fmt.Fprintf(&text, "GRANT USAGE, CREATE ON SCHEMA public TO %s;\n", quote(owner))
	for _, role := range []string{runtime, worker, relay} {
		fmt.Fprintf(&text, "GRANT USAGE ON SCHEMA public TO %s;\n", quote(role))
	}
	fmt.Fprintf(&text, "GRANT %s TO %s WITH SET TRUE, INHERIT TRUE;\n", quote(outboxRelayRole), quote(owner))
	fmt.Fprintf(&text, "GRANT CREATE ON SCHEMA public TO %s;\n", quote(outboxRelayRole))
	return text.String()
}

// quote quotes one SQL name. The names it gets are checked plain names, so it only adds the quotes.
func quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }
