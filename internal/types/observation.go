package types

// Observation is one thing that the service's own code can observe and
// report (ST-DECL-EN20): its name, its kind, its severity, its typed fields
// and the commands, queries, workers, process methods or slot bindings whose
// fills emit it. The domain-probe generator makes one typed probe method from
// it.
type Observation struct {
	// Name is the observation's name in lower snake case. Each signal carries
	// it as domain.<name>.
	Name string `yaml:"name"`
	// Kind is event, counter or histogram.
	Kind string `yaml:"kind"`
	// Severity is debug, info, warn or error.
	Severity string `yaml:"severity"`
	// Fields are the values that each emission carries, in order.
	Fields []ObservationField `yaml:"fields,omitempty"`
	// Value is, for a histogram only, the number that each emission records,
	// as name: unit, such as wait: ms.
	Value string `yaml:"value,omitempty"`
	// EmittedBy names the commands, queries, workers and process methods, and
	// the slot bindings as collection.slot, whose fills may emit it.
	EmittedBy []string `yaml:"emitted_by"`
	// OutcomeMeasured is the ID of the service's own business outcome whose
	// measure counts the observation, or empty.
	OutcomeMeasured string `yaml:"outcome_measured,omitempty"`
	// Counts is attempt or committed; empty is attempt.
	Counts string `yaml:"counts,omitempty"`
}

// ObservationField is one field of an observation, as name: type. The type
// is Entity.field, enum(a | b), bool, or measure(unit, min..max).
type ObservationField struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`
}
