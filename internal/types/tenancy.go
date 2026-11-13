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
	// Provisioning is how an operator adds and retires a tenant of a pooled service (ST-DECL-EN27).
	Provisioning *TenantProvisioning `yaml:"provisioning,omitempty" json:"Provisioning,omitempty"`
}

// TenantProvisioning is the tenant provisioning of a pooled service (ST-DECL-EN27): the resource type of the
// service-level resource that Provision and Retire are decided on, whose ID is the service name, and the
// permission of each. The generated service serves Provision and Retire as two commands of its own, decided,
// audited and never public.
type TenantProvisioning struct {
	// ResourceType is the type in the authorization schema of the service-level resource, such as service.
	ResourceType string `yaml:"resource_type,omitempty" json:"ResourceType,omitempty"`
	// Provision is the permission that a principal needs on that resource to provision a tenant.
	Provision string `yaml:"provision,omitempty" json:"Provision,omitempty"`
	// Retire is the permission that a principal needs on that resource to retire a tenant.
	Retire string `yaml:"retire,omitempty" json:"Retire,omitempty"`
}
