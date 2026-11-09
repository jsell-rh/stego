package types

// Tenancy is the tenancy setting of a declaration (ST-DECL-EN09): how the service keeps tenants apart. A
// declaration that omits it gets pooled tenancy.
type Tenancy struct {
	// Kind is single or pooled. Omitted means pooled.
	Kind string `yaml:"kind,omitempty"`
	// Key is the field that holds the tenant identifier, such as org_id.
	Key string `yaml:"key,omitempty"`
	// Claim is the token claim path in the verified identity token that the tenant comes from, such as org.id.
	Claim string `yaml:"claim,omitempty"`
	// Reason is why the service leaves the default. Kind single needs one.
	Reason string `yaml:"reason,omitempty"`
}
