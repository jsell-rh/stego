package types

// Controller is one controller that the service runs in controller mode (ST-DECL-EN16): what it reconciles from,
// how often it resyncs, how many replicas run and the lease that keeps one of them active.
type Controller struct {
	// Name is the controller's name.
	Name string `yaml:"name"`
	// Sources are where the controller reads changes from: scan or watch.
	Sources []string `yaml:"sources"`
	// Resync is how often the controller reconciles every instance again, such as 10m.
	Resync string `yaml:"resync"`
	// Replicas is how many instances of the controller run.
	Replicas int `yaml:"replicas"`
	// LeaseTTL is how long the lease of the active instance holds, such as 15s.
	LeaseTTL string `yaml:"lease_ttl"`
	// Tenants is how the controller goes over the tenants: each.
	Tenants string `yaml:"tenants"`
}
