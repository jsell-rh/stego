package authz

import (
	"errors"
	"fmt"

	"stego.dev/runtime/internal/seal"
)

// ServiceAccountType is the subject type of a system principal: a worker, a controller or the relay calls with the
// subject service_account:<name> (ST-SUP-RP06-LC07).
const ServiceAccountType = "service_account"

// Subject is the subject of an evaluation or of a tuple, in the shape of OpenID AuthZEN 1.0: a type and an ID.
// There is no wildcard subject: an evaluation or a tuple with the ID * is refused.
type Subject struct {
	Type string
	ID   string
}

// String returns the subject as type:id.
func (s Subject) String() string { return s.Type + ":" + s.ID }

// IsZero reports whether s is the zero value, which no evaluation accepts.
func (s Subject) IsZero() bool { return s.Type == "" && s.ID == "" }

func (s Subject) valid() error {
	if s.IsZero() {
		return errors.New("authz: the request has no subject")
	}
	if err := validType("subject", s.Type); err != nil {
		return err
	}
	return validID("subject", s.ID)
}

// Identity is a verified identity of the authentication runtime with its user ID, the verified subject of the token
// (ST-SUP-RP13-LC01). It is sealed: only the runtime module makes one.
type Identity interface {
	seal.Identity
	UserID() string
}

// SubjectOf maps a verified identity to the subject of the declared subject type, with the identity's user ID as
// the subject ID (ST-SUP-RP06-LC07).
func SubjectOf(subjectType string, identity Identity) (Subject, error) {
	if identity == nil {
		return Subject{}, errors.New("authz: no verified identity")
	}
	subject := Subject{Type: subjectType, ID: identity.UserID()}
	if err := subject.valid(); err != nil {
		return Subject{}, fmt.Errorf("authz: map the identity to a subject: %w", err)
	}
	return subject, nil
}

// ServiceAccount returns the subject service_account:<name> of a worker, a controller or the relay.
func ServiceAccount(name string) (Subject, error) {
	subject := Subject{Type: ServiceAccountType, ID: name}
	if err := subject.valid(); err != nil {
		return Subject{}, fmt.Errorf("authz: the service account: %w", err)
	}
	return subject, nil
}

// Principal is who acts in a transaction: a person, mapped from a verified identity, or a system, as its service
// account. Its field is unexported, so a principal comes only from Person or System, and its subject is never
// zero or a wildcard.
type Principal struct {
	subject Subject
}

// Person is the principal of a verified identity, as the subject of the declared subject type.
func Person(subjectType string, identity Identity) (Principal, error) {
	subject, err := SubjectOf(subjectType, identity)
	if err != nil {
		return Principal{}, err
	}
	if subject.Type == ServiceAccountType {
		return Principal{}, errors.New("authz: a person cannot have the subject type service_account")
	}
	return Principal{subject: subject}, nil
}

// System is the principal of a worker, a controller or the relay named name: its subject is
// service_account:<name> (ST-SUP-RP06-LC07, ST-SUP-RP06-LC10).
func System(name string) (Principal, error) {
	subject, err := ServiceAccount(name)
	if err != nil {
		return Principal{}, err
	}
	return Principal{subject: subject}, nil
}

// Subject returns the subject of the principal.
func (p Principal) Subject() Subject { return p.subject }

// IsZero reports whether p is the zero value, which Bind refuses.
func (p Principal) IsZero() bool { return p.subject.IsZero() }

// Context is the caveat context of an evaluation: a string-keyed map of the declared context fields
// (ST-SUP-RP06-LC08).
type Context map[string]any

// NewContext builds the caveat context of the declared fields from values: it holds each declared field that values
// gives, and no other key, so a value the declaration does not name never reaches the provider. A declared field
// that values lacks stays out, and the provider then answers that the field is missing, which is an error.
func NewContext(declared []string, values map[string]any) Context {
	context := Context{}
	for _, name := range declared {
		if value, ok := values[name]; ok {
			context[name] = value
		}
	}
	return context
}

// ContextError is the error of an evaluation whose caveat lacks context fields: the provider could not decide, so
// the declaration forgot a context field. It is never a deny (ST-SUP-RP06-LC08).
type ContextError struct {
	// Missing names the caveat fields that the context lacks; it is empty when the provider names none.
	Missing []string
}

func (e *ContextError) Error() string {
	if len(e.Missing) == 0 {
		return "authz: the permission is conditional on caveat context that the request does not give"
	}
	return fmt.Sprintf("authz: the caveat context lacks the fields %v", e.Missing)
}
