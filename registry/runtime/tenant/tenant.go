package tenant

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"
)

// maxIDLength is the longest tenant ID the package accepts, in bytes.
const maxIDLength = 255

// ErrZero is the error of every function that takes a Tenant and gets the zero value (ST-SUP-RP04-LC01).
var ErrZero = errors.New("tenant: the tenant is zero")

// ErrNoTenant is the error of FromContext when the context carries no tenant (ST-SUP-RP04-LC02).
var ErrNoTenant = errors.New("tenant: the context carries no tenant")

// Tenant is the tenant of a request, a worker tick, a controller pass or a claimed outbox row. Its one field is
// unexported and its underlying type is a struct, so no code outside the package can build one from a string. The
// zero value is not a tenant: every function that takes a Tenant refuses it before it sends a statement.
type Tenant struct {
	id string
}

// ID returns the tenant ID. It is empty only for the zero value.
func (t Tenant) ID() string { return t.id }

// IsZero reports whether t is the zero value.
func (t Tenant) IsZero() bool { return t.id == "" }

// String returns the tenant ID.
func (t Tenant) String() string { return t.id }

// Source is one of the four sources of a tenant (ST-SUP-RP04-LC01). Its method is unexported, so only the values that
// this package builds satisfy it: Claim, Column and Requested. A message payload, a header or a body field is not a
// Source.
type Source interface {
	tenantID() (string, error)
}

// Identity is the verified identity that the authentication component gives after it checks a token. TenantClaim
// returns the tenant claim of the token and false when the token holds none.
type Identity interface {
	TenantClaim() (string, bool)
}

// Decision is the decided permission of a Provision or Retire request (ST-SUP-RC27-OC09). Allowed reports whether
// the authorization port allowed it.
type Decision interface {
	Allowed() bool
}

type claimSource struct {
	identity Identity
}

// Claim is the source of the tenant of a verified identity, for the authentication component.
func Claim(identity Identity) Source { return claimSource{identity: identity} }

func (s claimSource) tenantID() (string, error) {
	if s.identity == nil {
		return "", errors.New("tenant: no verified identity")
	}
	id, ok := s.identity.TenantClaim()
	if !ok || id == "" {
		return "", errors.New("tenant: the verified identity holds no tenant claim")
	}
	return id, nil
}

// Column is the source of a tenant column that a statement reads: an entry of the tenant list, which List reads, or
// the tenant column of a claimed outbox row, which the outbox worker reads. Give a pointer to it to Scan.
type Column struct {
	id    string
	valid bool
}

// Scan reads a tenant column value. It implements database/sql.Scanner. A NULL column gives a Column that New refuses.
func (c *Column) Scan(value any) error {
	switch v := value.(type) {
	case nil:
		*c = Column{}
	case string:
		*c = Column{id: v, valid: true}
	case []byte:
		*c = Column{id: string(v), valid: true}
	default:
		return fmt.Errorf("tenant: a tenant column of type %T is not text", value)
	}
	return nil
}

func (c Column) tenantID() (string, error) {
	if !c.valid {
		return "", errors.New("tenant: the tenant column is NULL or was not read")
	}
	return c.id, nil
}

type requestedSource struct {
	id       string
	decision Decision
}

// Requested is the source of the tenant that a Provision or Retire request names. New accepts it only when the
// decision allowed the request (ST-SUP-RC27-OC09).
func Requested(id string, decision Decision) Source {
	return requestedSource{id: id, decision: decision}
}

func (s requestedSource) tenantID() (string, error) {
	if s.decision == nil || !s.decision.Allowed() {
		return "", errors.New("tenant: the request that names the tenant is not decided as allowed")
	}
	return s.id, nil
}

// New is the one constructor of a Tenant. It builds the tenant of its source and refuses a source that gives no
// valid tenant ID, so a Tenant is never zero (ST-SUP-RP04-LC01).
func New(source Source) (Tenant, error) {
	if source == nil {
		return Tenant{}, errors.New("tenant: no source")
	}
	id, err := source.tenantID()
	if err != nil {
		return Tenant{}, err
	}
	if err := validID(id); err != nil {
		return Tenant{}, err
	}
	return Tenant{id: id}, nil
}

func validID(id string) error {
	switch {
	case id == "":
		return errors.New("tenant: the tenant ID is empty")
	case len(id) > maxIDLength:
		return fmt.Errorf("tenant: the tenant ID is longer than %d bytes", maxIDLength)
	case !utf8.ValidString(id):
		return errors.New("tenant: the tenant ID is not valid UTF-8")
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return errors.New("tenant: the tenant ID holds a control character")
		}
	}
	return nil
}

type contextKey struct{}

// WithTenant returns a copy of ctx that carries t. The authentication component calls it after it builds the tenant
// from the verified identity. It refuses the zero value.
func WithTenant(ctx context.Context, t Tenant) (context.Context, error) {
	if t.IsZero() {
		return ctx, ErrZero
	}
	return context.WithValue(ctx, contextKey{}, t), nil
}

// FromContext returns the tenant that WithTenant set in ctx, or ErrNoTenant when ctx carries none
// (ST-SUP-RP04-LC02). It never returns a zero tenant with a nil error.
func FromContext(ctx context.Context) (Tenant, error) {
	t, ok := ctx.Value(contextKey{}).(Tenant)
	if !ok || t.IsZero() {
		return Tenant{}, ErrNoTenant
	}
	return t, nil
}
