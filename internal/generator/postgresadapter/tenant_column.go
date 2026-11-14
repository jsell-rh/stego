package postgresadapter

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

// tenantKeyName is the grammar of a tenant column name: a lower-case SQL name that needs no quotes.
var tenantKeyName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// tenantColumn gives the name of the tenant column of the owned tables, or "" for a service without pooled
// tenancy, whose tables carry no tenant column.
func tenantColumn(ctx gen.Context) string {
	if ctx.Tenancy == nil {
		return ""
	}
	return ctx.Tenancy.Key
}

// ownedEntity reports whether the entity is owned by a tenant: its tenant is owned or omitted (ST-DECL-EN02).
func ownedEntity(entity types.Entity) bool {
	return entity.Tenant == "" || entity.Tenant == "owned"
}

// validateTenantColumn refuses a declaration that the tenant column cannot serve (ST-SUP-RC01-OC40): a key that
// is not a plain SQL name, a declared field with the key's name, and a reference from a shared or a system entity
// to an owned one, whose row would hold a key of one tenant.
func validateTenantColumn(ctx gen.Context) error {
	key := tenantColumn(ctx)
	if key == "" {
		return nil
	}
	if !tenantKeyName.MatchString(key) {
		return fmt.Errorf("tenancy key %q is not a lower-case SQL name of at most 63 characters", key)
	}
	owned := map[string]bool{}
	for _, entity := range ctx.Entities {
		owned[entity.Name] = ownedEntity(entity)
	}
	for _, entity := range ctx.Entities {
		for _, field := range entity.Fields {
			if field.Name == key || field.ColumnName() == key {
				return fmt.Errorf("entity %s: field %q collides with the tenant column %q of pooled tenancy; remove the field, because the tenant comes from the verified identity", entity.Name, field.Name, key)
			}
			if field.Type == types.FieldTypeRef && !ownedEntity(entity) && owned[field.To] {
				return fmt.Errorf("entity %s: field %q refers to the owned entity %s, but %s is %s and holds no tenant", entity.Name, field.Name, field.To, entity.Name, entity.Tenant)
			}
		}
	}
	return nil
}

// rowSecurityImport gives the import path of the tenancy component's package, whose RowSecurity migration the
// migration ledger registers after the tables, or "" for a service without pooled tenancy (ST-SUP-RC27-OC03).
func rowSecurityImport(ctx gen.Context) (string, error) {
	if tenantColumn(ctx) == "" {
		return "", nil
	}
	ns := ctx.PeerNamespaces[gen.TenancyComponent]
	if ns == "" {
		return "", fmt.Errorf("pooled tenancy needs the output namespace of the %s component", gen.TenancyComponent)
	}
	if err := gen.ValidateGoPackageNamespace(ns); err != nil {
		return "", err
	}
	if ctx.ModuleName == "" {
		return "", fmt.Errorf("pooled tenancy requires a module name")
	}
	return path.Join(ctx.ModuleName, ctx.OutDirName, ns), nil
}

// TableName gives the table of an entity: the snake case plural of its name. The tenancy component names the
// tables of its row-level security with it (ST-SUP-RC27-OC03).
func TableName(entity string) string { return tableName(entity) }

// runtimePrivileges gives the privileges of the runtime role on the table of an entity: SELECT only on a shared
// entity of a pooled service, which only the owner role in the migration job writes (ST-SUP-RC27-OC07), and
// every row privilege on any other table.
func runtimePrivileges(ctx gen.Context, entity types.Entity) []string {
	if tenantColumn(ctx) != "" && entity.Tenant == "shared" {
		return []string{"SELECT"}
	}
	return []string{"SELECT", "INSERT", "UPDATE", "DELETE"}
}

// TenantRowSecurity gives the row-level security of the table of one owned entity: it enables and forces row-level
// security, drops the tenant policy and creates it again, with USING and WITH CHECK that compare the tenant column
// key with the tenant setting of the transaction (ST-SUP-RC27-OC03). The tenancy component writes it for each owned
// entity of the baseline in its migration, and the step that adds an owned entity writes it after the table, so both
// give the same text for one table.
func TenantRowSecurity(entity, key string) string {
	setting := "pg_catalog.current_setting('app.tenant', true)"
	table := "public." + sqlIdentifier(tableName(entity))
	column := sqlIdentifier(key)
	return fmt.Sprintf("ALTER TABLE %s ENABLE ROW LEVEL SECURITY;\n", table) +
		fmt.Sprintf("ALTER TABLE %s FORCE ROW LEVEL SECURITY;\n", table) +
		fmt.Sprintf("DROP POLICY IF EXISTS %s ON %s;\n", TenantPolicy, table) +
		fmt.Sprintf("CREATE POLICY %s ON %s USING (%s = %s) WITH CHECK (%s = %s);\n", TenantPolicy, table, column, setting, column, setting)
}

// roleName is the grammar of a role name of the service: a lower-case SQL name of at most 63 characters.
var roleName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// serviceRoles gives the runtime role and the owner role of a pooled service as the tenancy component names them:
// its settings runtime_role and owner_role, or the service name in lower case with _runtime and _owner. The sync
// function of a step names the owner role as the backfill role (ST-SUP-RC01-OC32), and the migration of a step
// grants the backfill functions to the runtime role. It refuses a service without pooled tenancy.
func serviceRoles(ctx gen.Context) (string, string, error) {
	if ctx.Tenancy == nil {
		return "", "", fmt.Errorf("a step with a sync trigger and a backfill needs the tenancy component, whose owner role runs the backfill")
	}
	base := strings.ToLower(strings.NewReplacer("-", "_", ".", "_").Replace(ctx.ServiceName))
	runtime, owner := base+"_runtime", base+"_owner"
	for name, value := range map[string]*string{"runtime_role": &runtime, "owner_role": &owner} {
		if text, ok := ctx.PeerConfigs[gen.TenancyComponent][name].(string); ok {
			*value = text
		}
	}
	if !roleName.MatchString(runtime) || !roleName.MatchString(owner) {
		return "", "", fmt.Errorf("the runtime role %q or the owner role %q is not a lower-case SQL name of at most 63 characters", runtime, owner)
	}
	return runtime, owner, nil
}
