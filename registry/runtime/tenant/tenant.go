package tenant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"unicode/utf8"

	"stego.dev/runtime/internal/seal"
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

// Source is a source of a tenant that New accepts (ST-SUP-RP04-LC01): Claim of a verified Identity, or Requested
// with a Decision. Its method is unexported, so only the values that this package builds satisfy it. The
// two other sources, an entry of the tenant list and the tenant column of a claimed outbox row, are columns that a
// statement reads; List and ScanRows build those tenants. A message payload, a header or a body field is not a
// source.
type Source interface {
	tenantID() (string, error)
}

// Identity is a verified identity of the authentication runtime. It is sealed: only a package of the runtime
// module can make one (ST-SUP-RP04-LC01).
type Identity = seal.Identity

// Decision is the decided permission of a Provision or Retire request, from the authorization runtime. It is
// sealed: only a package of the runtime module can make one (ST-SUP-RC27-OC09).
type Decision = seal.Decision

type claimSource struct {
	identity Identity
}

// Claim is the source of the tenant of a verified identity, for the authentication component.
func Claim(identity Identity) Source { return claimSource{identity: identity} }

// tenantID reads the tenant claim from the identity's seal mark only, through seal.TenantOf, never from a method
// that an outer type can declare again.
func (s claimSource) tenantID() (string, error) {
	id, ok := seal.TenantOf(s.identity)
	if !ok {
		return "", errors.New("tenant: no verified identity with a tenant claim")
	}
	return id, nil
}

type requestedSource struct {
	id       string
	decision Decision
}

// Requested is the source of the tenant that a Provision or Retire request names. New accepts it only when the
// decision allowed the request for that same tenant (ST-SUP-RC27-OC09).
func Requested(id string, decision Decision) Source {
	return requestedSource{id: id, decision: decision}
}

// tenantID reads the allowed tenant from the decision's seal mark only, through seal.DecisionOf, and accepts the
// request only for that tenant.
func (s requestedSource) tenantID() (string, error) {
	allowed, ok := seal.DecisionOf(s.decision)
	if !ok || allowed != s.id {
		return "", errors.New("tenant: the request that names the tenant is not decided as allowed")
	}
	return s.id, nil
}

// column is the source of a tenant column that a statement reads. Only List and ScanRows build it.
type column struct {
	id    string
	valid bool
}

// Scan reads a tenant column value; it implements database/sql.Scanner. A NULL column gives a column that New
// refuses.
func (c *column) Scan(value any) error {
	switch v := value.(type) {
	case nil:
		*c = column{}
	case string:
		*c = column{id: v, valid: true}
	case []byte:
		*c = column{id: string(v), valid: true}
	default:
		return fmt.Errorf("tenant: a tenant column of type %T is not text", value)
	}
	return nil
}

func (c column) tenantID() (string, error) {
	if !c.valid {
		return "", errors.New("tenant: the tenant column is NULL")
	}
	return c.id, nil
}

// ScanRows reads the current row of rows: its first column is the tenant column, such as the tenant_id of a claimed
// outbox row, and the other columns go to dest, as rows.Scan puts them. It builds the tenant through New, so the
// tenant column of a row is a source only through a statement that read it.
func ScanRows(rows *sql.Rows, dest ...any) (Tenant, error) {
	if rows == nil {
		return Tenant{}, errors.New("tenant: no rows")
	}
	var c column
	if err := rows.Scan(append([]any{&c}, dest...)...); err != nil {
		return Tenant{}, fmt.Errorf("tenant: read the tenant column: %w", err)
	}
	return New(c)
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

// Verifier is the authentication component's check of a request: it verifies the token of r and returns the
// verified identity, or an error when the request is not authenticated. The service's own code passes it to
// Middleware.
type Verifier func(r *http.Request) (Identity, error)

// Middleware returns the HTTP wrapper that sets the tenant of each request from the verified identity that verify
// returns, through Claim and New, before next runs (ST-SUP-RP04-LC01, ST-SUP-RP04-LC02). It reads no header and no
// body field of the request itself, so a client-supplied tenant is never a source. A request that verify refuses,
// or whose identity holds no tenant claim, gets 401 and does not reach next.
func Middleware(verify Verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if verify == nil {
				http.Error(w, "unauthenticated", http.StatusUnauthorized)
				return
			}
			identity, err := verify(r)
			if err != nil {
				http.Error(w, "unauthenticated", http.StatusUnauthorized)
				return
			}
			t, err := New(Claim(identity))
			if err != nil {
				http.Error(w, "unauthenticated", http.StatusUnauthorized)
				return
			}
			ctx, err := WithTenant(r.Context(), t)
			if err != nil {
				http.Error(w, "unauthenticated", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
