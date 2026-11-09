package declaration

import (
	"fmt"
	"strings"

	"github.com/jsell-rh/stego/internal/types"
)

// Settings are the transactional settings of a declaration as checking reads them, with every default that the
// declaration omits filled in (ST-DECL-UC05-FL-S02), and each default that it leaves, with its reason.
type Settings struct {
	Tenancy      TenancySettings  `json:"tenancy"`
	Audit        AuditSettings    `json:"audit"`
	Entities     []EntitySettings `json:"entities"`
	DefaultsLeft []DefaultLeft    `json:"defaults_left"`
	// Defaults marks the settings that checking filled in with their default because the declaration omits them,
	// each by its path, such as tenancy.kind or entities.Todo.tenant.
	Defaults []string `json:"defaults"`
}

// fill gives the value stated, or the default when the declaration omits it, and marks the default at the path.
func (s *Settings) fill(path, stated, byDefault string) string {
	if stated != "" {
		return stated
	}
	s.Defaults = append(s.Defaults, path)
	return byDefault
}

// TenancySettings is the tenancy setting with its defaults filled in.
type TenancySettings struct {
	Kind   string `json:"kind"`
	Key    string `json:"key,omitempty"`
	Claim  string `json:"claim,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// AuditSettings is the audit setting with its defaults filled in. Audit that is on has a SHA-256 hash chain over
// the redacted records and an anchor outside the service.
type AuditSettings struct {
	Enabled   bool   `json:"enabled"`
	Level     string `json:"level,omitempty"`
	Chain     string `json:"chain,omitempty"`
	Anchored  bool   `json:"anchored"`
	Sink      string `json:"sink,omitempty"`
	Retention string `json:"retention,omitempty"`
	PII       string `json:"pii,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// EntitySettings is the tenant of one entity and the sensitivity of each of its fields, with the defaults owned
// and public filled in.
type EntitySettings struct {
	Name   string          `json:"name"`
	Tenant string          `json:"tenant"`
	Fields []FieldSettings `json:"fields"`
}

// FieldSettings is the sensitivity of one field.
type FieldSettings struct {
	Name        string `json:"name"`
	Sensitivity string `json:"sensitivity"`
}

// DefaultLeft is one default of every ga archetype that the declaration leaves, with the value it says and its
// reason, which the fleet shows.
type DefaultLeft struct {
	Setting string `json:"setting"`
	Value   string `json:"value"`
	Reason  string `json:"reason"`
}

// FilledSettings reads the declaration and gives its settings with the defaults filled in: pooled tenancy with the
// key and the claim path tenant_id, audit
// on at level change with a SHA-256 chain, an anchor, the sink audit-sink and pii redact, the tenant owned and the
// sensitivity public.
func FilledSettings(service *types.ServiceDeclaration) *Settings {
	settings := &Settings{Entities: []EntitySettings{}, DefaultsLeft: []DefaultLeft{}, Defaults: []string{}}
	tenancy := service.Tenancy
	if tenancy == nil {
		// An omitted tenancy is pooled, with the key and the claim path tenant_id (ST-DECL-INV18).
		settings.Tenancy.Kind = settings.fill("tenancy.kind", "", tenancyPooled)
		settings.Tenancy.Key = settings.fill("tenancy.key", "", tenantKey)
		settings.Tenancy.Claim = settings.fill("tenancy.claim", "", tenantClaim)
		tenancy = &types.Tenancy{}
	} else {
		settings.Tenancy.Kind = settings.fill("tenancy.kind", tenancy.Kind, tenancyPooled)
		settings.Tenancy.Key, settings.Tenancy.Claim, settings.Tenancy.Reason = tenancy.Key, tenancy.Claim, tenancy.Reason
	}
	if settings.Tenancy.Kind == tenancySingle {
		settings.DefaultsLeft = append(settings.DefaultsLeft, DefaultLeft{Setting: "tenancy.kind", Value: tenancySingle, Reason: tenancy.Reason})
	}
	audit := types.Audit{}
	if service.Audit != nil {
		audit = *service.Audit
	}
	settings.Audit = AuditSettings{Enabled: audit.Enabled == nil || *audit.Enabled, Retention: audit.Retention, Reason: audit.Reason}
	if audit.Enabled == nil {
		settings.Defaults = append(settings.Defaults, "audit.enabled")
	}
	if settings.Audit.Enabled {
		settings.Audit.Chain, settings.Audit.Anchored = "sha-256", true
		settings.Audit.Level = settings.fill("audit.level", audit.Level, auditChange)
		settings.Audit.Sink = settings.fill("audit.sink", audit.Sink, "audit-sink")
		settings.Audit.PII = settings.fill("audit.pii", audit.PII, "redact")
	} else {
		settings.DefaultsLeft = append(settings.DefaultsLeft, DefaultLeft{Setting: "audit.enabled", Value: "off", Reason: audit.Reason})
	}
	for _, entity := range service.Entities {
		filled := EntitySettings{Name: entity.Name, Tenant: settings.fill("entities."+entity.Name+".tenant", entity.Tenant, tenantOwned), Fields: []FieldSettings{}}
		for _, field := range entity.Fields {
			sensitivity := settings.fill("entities."+entity.Name+".fields."+field.Name+".sensitivity", field.Sensitivity, "public")
			filled.Fields = append(filled.Fields, FieldSettings{Name: field.Name, Sensitivity: sensitivity})
		}
		settings.Entities = append(settings.Entities, filled)
	}
	return settings
}

// Prose gives the settings for a person: the tenancy, the audit, each entity that is not owned, each field that
// is not public and each default left with its reason.
func (s *Settings) Prose() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Settings with the defaults filled in:\n  tenancy: %s", s.Tenancy.Kind)
	if s.Tenancy.Claim != "" {
		fmt.Fprintf(&sb, " (key %s, claim %s)", s.Tenancy.Key, s.Tenancy.Claim)
	}
	if s.Audit.Enabled {
		fmt.Fprintf(&sb, "\n  audit: on at level %s, %s chain, anchored, sink %s, pii %s", s.Audit.Level, strings.ToUpper(s.Audit.Chain), s.Audit.Sink, s.Audit.PII)
	} else {
		sb.WriteString("\n  audit: off")
	}
	for _, entity := range s.Entities {
		fmt.Fprintf(&sb, "\n  entity %s: %s", entity.Name, entity.Tenant)
		for _, field := range entity.Fields {
			if field.Sensitivity != "public" {
				fmt.Fprintf(&sb, ", field %s %s", field.Name, field.Sensitivity)
			}
		}
	}
	for _, left := range s.DefaultsLeft {
		fmt.Fprintf(&sb, "\n  default left: %s %s, reason: %s", left.Setting, left.Value, left.Reason)
	}
	return sb.String() + "\n"
}
