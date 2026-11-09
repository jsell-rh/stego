package types

// Worker is one piece of background work that the service's own code carries out on a schedule or for each message
// it consumes (ST-DECL-EN25): its name, its trigger, the entities it writes, the permission it needs, the effects it
// stages and the one fill that carries it out with a worker handle.
type Worker struct {
	// Name is the worker's name in lower kebab case, unique among the commands, queries, workers and controllers.
	Name string `yaml:"name"`
	// Trigger is when the worker runs: every <interval>, such as every 1m, or consumes <destination>.
	Trigger string `yaml:"trigger"`
	// Writes names the entities that the worker handle may read and write.
	Writes []string `yaml:"writes"`
	// ResourceType is the type in the authorization schema that the worker acts on.
	ResourceType string `yaml:"resource_type,omitempty"`
	// Permission is the permission that the worker's service account needs on each resource it reads or writes.
	Permission string `yaml:"permission,omitempty"`
	// Fill names the one fill that carries out the worker at the worker slot.
	Fill FillNames `yaml:"fill"`
	// Effects are the effects the fill may stage, each an outbox destination and an effect name.
	Effects []Effect `yaml:"effects,omitempty"`
	// Single tells, for a worker with an every trigger, whether one instance of the service runs it at a time under
	// a lease. Nil is omitted, which is yes.
	Single *bool `yaml:"single,omitempty"`
	// LeaseTTL is how long the lease of a single worker holds, such as 15s. Empty is 15s.
	LeaseTTL string `yaml:"lease_ttl,omitempty"`
}
