package types

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Command is one business command that the service's own code carries out in one transaction (ST-DECL-EN18): its
// name, the entities it writes, the permission it needs, the one fill that carries it out, the effects it may stage,
// the route and the refusals it may answer with.
type Command struct {
	// Name is the command's name, unique in the declaration, such as reserve-and-charge.
	Name string `yaml:"name"`
	// Writes names the entities that the command handle may read and write.
	Writes []string `yaml:"writes"`
	// ResourceType is the type in the authorization schema that the command acts on.
	ResourceType string `yaml:"resource_type,omitempty"`
	// Permission is the permission the caller needs on the resource before the fill runs.
	Permission string `yaml:"permission,omitempty"`
	// Fill names the one fill that carries out the command at the command slot. The declaration may give a list, so
	// that checking can refuse more than one fill by name.
	Fill FillNames `yaml:"fill"`
	// RelationshipsWritten names the relations, as resource_type#relation, that the fill may write.
	RelationshipsWritten []string `yaml:"relationships_written,omitempty"`
	// Effects are the effects the fill may stage, each an outbox destination and an effect name.
	Effects []Effect `yaml:"effects,omitempty"`
	// ResourceMember is the request member that holds the ID of the resource the permission is decided on.
	ResourceMember string `yaml:"resource_member,omitempty"`
	// Route is the method and path the command is served at under the base path, such as
	// POST /orders/{orderId}/reservations.
	Route string `yaml:"route"`
	// Refusals are the refusals that the fill may return.
	Refusals []Refusal `yaml:"refusals,omitempty"`
	// Method is the unary protobuf method, as package.Service/Method, that serves the command over gRPC.
	Method string `yaml:"method,omitempty"`
	// RetryOnConflict is how many times the handler runs the fill again after a conflict, as the declaration writes
	// it, so that checking can name a value that is not a whole number. Empty is 0.
	RetryOnConflict string `yaml:"retry_on_conflict,omitempty"`
	// LockTimeout is how long a statement waits for a row lock, as a duration such as 500ms. Empty is 2s.
	LockTimeout string `yaml:"lock_timeout,omitempty"`
}

// Effect is one effect that a command or a worker may stage: the outbox destination and the effect name, which is
// part of each outgoing idempotency key.
type Effect struct {
	Destination string `yaml:"destination"`
	Name        string `yaml:"name"`
}

// String writes the effect as destination: name, as the specification writes it.
func (e Effect) String() string { return e.Destination + ": " + e.Name }

// Refusal is one refusal that the fill of a command or a query may return by name: its name in lower snake case,
// the HTTP status and the fixed code that the handler answers with.
type Refusal struct {
	Name   string `yaml:"name"`
	Status int    `yaml:"status"`
	Code   string `yaml:"code"`
}

// FillNames holds the fills that an operation names. The declaration writes one name, or a list of names.
type FillNames []string

// UnmarshalYAML reads one fill name or a list of fill names.
func (f *FillNames) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		if value.Tag == "!!null" || value.Value == "" {
			*f = nil
			return nil
		}
		*f = FillNames{value.Value}
		return nil
	case yaml.SequenceNode:
		var names []string
		if err := value.Decode(&names); err != nil {
			return err
		}
		*f = names
		return nil
	}
	return fmt.Errorf("line %d: a fill is one name or a list of names", value.Line)
}
