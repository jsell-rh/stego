package declaration

import (
	"github.com/jsell-rh/stego/internal/types"
)

// The audit values that STEGO always gives audit that is on: a SHA-256 hash chain over the redacted records, an
// anchor outside the service, the sink audit-sink, which the platform binds, and pii redact (ST-DECL-EN10).
const (
	auditChain  = "sha-256"
	auditSink   = "audit-sink"
	auditRedact = "redact"
)

// resolved gives the setting at the path with the value stated, or with the default when the declaration omits
// it, marked as a default.
func resolved(path, stated, byDefault string) Setting {
	if stated == "" {
		return Setting{Path: path, Value: byDefault, Default: true}
	}
	return Setting{Path: path, Value: stated}
}

// stated gives the setting at the path when the declaration states it, and nothing when it omits it.
func stated(path, value string) []Setting {
	if value == "" {
		return nil
	}
	return []Setting{{Path: path, Value: value}}
}

// tenancySettings resolves the tenancy setting (ST-DECL-INV18): an omitted tenancy is pooled, with the key and the
// claim path tenant_id. A default left, such as tenancy kind single, is a setting that is not a default, and its
// reason is the setting tenancy.reason.
func tenancySettings(service *types.ServiceDeclaration) []Setting {
	tenancy := service.Tenancy
	if tenancy == nil {
		return []Setting{
			{Path: "tenancy.kind", Value: tenancyPooled, Default: true},
			{Path: "tenancy.key", Value: tenantKey, Default: true},
			{Path: "tenancy.claim", Value: tenantClaim, Default: true},
		}
	}
	settings := []Setting{resolved("tenancy.kind", tenancy.Kind, tenancyPooled)}
	settings = append(settings, stated("tenancy.key", tenancy.Key)...)
	settings = append(settings, stated("tenancy.claim", tenancy.Claim)...)
	return append(settings, stated("tenancy.reason", tenancy.Reason)...)
}

// auditSettings resolves the audit setting (ST-DECL-INV18): omitted audit is on at level change, with a SHA-256
// chain and an anchor, which the declaration cannot change, so they are always defaults. Audit off is the setting
// audit.enabled false, and its reason is the setting audit.reason.
func auditSettings(service *types.ServiceDeclaration) []Setting {
	audit := types.Audit{}
	if service.Audit != nil {
		audit = *service.Audit
	}
	enabled := Setting{Path: "audit.enabled", Value: true, Default: true}
	if audit.Enabled != nil {
		enabled = Setting{Path: "audit.enabled", Value: *audit.Enabled}
	}
	settings := []Setting{enabled}
	if enabled.Value == true {
		settings = append(settings,
			resolved("audit.level", audit.Level, auditChange),
			Setting{Path: "audit.chain", Value: auditChain, Default: true},
			Setting{Path: "audit.anchor", Value: true, Default: true},
			resolved("audit.sink", audit.Sink, auditSink),
			resolved("audit.pii", audit.PII, auditRedact),
		)
		settings = append(settings, stated("audit.retention", audit.Retention)...)
	}
	return append(settings, stated("audit.reason", audit.Reason)...)
}

// entitySettings resolves the tenant of each entity, owned when omitted (ST-DECL-UC05-R04), and the sensitivity of
// each field, public when omitted (ST-DECL-UC05-R06), at the paths that the faults use.
func entitySettings(service *types.ServiceDeclaration) []Setting {
	var settings []Setting
	for i, entity := range service.Entities {
		settings = append(settings, resolved(entityPath(i)+".tenant", entity.Tenant, tenantOwned))
		for j, field := range entity.Fields {
			settings = append(settings, resolved(fieldPath(i, j)+".sensitivity", field.Sensitivity, "public"))
		}
	}
	return settings
}
