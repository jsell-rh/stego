package types

// Controller is one controller of a declaration (ST-DECL-EN16): a loop that reconciles work under a lease.
type Controller struct {
	// Name is the controller's name, unique in the declaration.
	Name string `yaml:"name"`
	// Sources are scan, watch or both: how the controller learns of work.
	Sources []string `yaml:"sources,omitempty"`
	// Resync is how often the controller reconciles everything again, such as 10m.
	Resync string `yaml:"resync,omitempty"`
	// Replicas is the number of replicas that stand by under lease election.
	Replicas int `yaml:"replicas,omitempty"`
	// LeaseTTL is how long a lease holds before another replica may claim it, such as 15s.
	LeaseTTL string `yaml:"lease_ttl,omitempty"`
	// Tenants is each: the controller iterates tenants and opens one transaction per tenant.
	Tenants string `yaml:"tenants,omitempty"`
}
