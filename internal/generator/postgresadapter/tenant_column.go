package postgresadapter

import (
	"fmt"
	"path"
	"regexp"

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
			if field.Name == key {
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
