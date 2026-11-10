package postgresadapter

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

// tenantField is the Go name of the tenant column in the model of an owned entity, and tenantMeta the base model
// that an owned entity embeds in place of Meta (ST-SUP-RC01-OC40).
const (
	tenantField = "StegoTenant"
	tenantMeta  = "TenantMeta"
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
// is not a plain SQL name, a declared field with the key's name or with the Go name of the tenant column, and a
// reference from a shared or a system entity to an owned one, whose row would hold a key of one tenant.
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
			goName := toPascalCase(field.Name)
			if field.Name == key || goName == tenantField || goName == tenantMeta {
				return fmt.Errorf("entity %s: field %q collides with the tenant column %q of pooled tenancy; remove the field, because the tenant comes from the verified identity", entity.Name, field.Name, key)
			}
			if field.Type == types.FieldTypeRef && !ownedEntity(entity) && owned[field.To] {
				return fmt.Errorf("entity %s: field %q refers to the owned entity %s, but %s is %s and holds no tenant", entity.Name, field.Name, field.To, entity.Name, entity.Tenant)
			}
		}
	}
	return nil
}

// leadIndexes rewrites the index entries of the field tags of an owned entity so that each index has a name, and
// gives the tag of the tenant column, which joins every one of those indexes with priority 1 and so leads it; the
// tenant field is the first field of the model, so it also leads an index whose other fields have priority 1. The
// deleted_at index of the entity's own DeletedAt field is one of them.
func leadIndexes(entity types.Entity, key string, tags map[string]string) string {
	table := tableName(entity.Name)
	parts := []string{"column:" + key, "primaryKey", "type:text", "not null", "default:current_setting('app.tenant')"}
	seen := map[string]bool{}
	join := func(kind, name, where string) {
		if seen[kind+":"+name] {
			return
		}
		seen[kind+":"+name] = true
		entry := kind + ":" + name + ",priority:1"
		if where != "" {
			entry += ",where:" + where
		}
		parts = append(parts, entry)
	}
	join("index", "idx_"+table+"_deleted_at", "")
	for _, field := range entity.Fields {
		tag, ok := tags[field.Name]
		if !ok {
			continue
		}
		entries := strings.Split(tag, ";")
		for i, entry := range entries {
			kind, options, _ := strings.Cut(entry, ":")
			if kind != "index" && kind != "uniqueIndex" {
				continue
			}
			name, rest, _ := strings.Cut(options, ",")
			if name == "" {
				name = "idx_" + table + "_" + field.Name
				entries[i] = kind + ":" + name
				if rest != "" {
					entries[i] += "," + rest
				}
			}
			where := ""
			for _, option := range strings.Split(rest, ",") {
				if value, found := strings.CutPrefix(option, "where:"); found {
					where = value
				}
			}
			join(kind, name, where)
		}
		tags[field.Name] = strings.Join(entries, ";")
	}
	return strings.Join(parts, ";")
}

// tenantForeignKey gives the foreignKey tag of a reference field of an owned entity: to another owned entity the
// key is the tenant column and the field, so a row refers only to a row of its own tenant; to a shared or a
// system entity it is the field alone.
func tenantForeignKey(goName, target string, owned map[string]bool) string {
	if !owned[target] {
		return "foreignKey:" + goName
	}
	return "foreignKey:" + tenantField + "," + goName + ";references:" + tenantField + ",ID"
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
