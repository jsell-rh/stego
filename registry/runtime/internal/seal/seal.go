// Package seal holds the sealed interfaces of the tenant sources (ST-SUP-RP04-LC01). Each interface has an
// unexported method of this package, so only a type that embeds the matching mark satisfies it, and only a package
// of stego.dev/runtime can import this package to embed one. The authentication runtime makes Identity values and
// the authorization runtime makes Decision values; generated code cannot make either up.
package seal

// Identity is a verified identity of the authentication runtime.
type Identity interface {
	// TenantClaim returns the tenant claim of the verified token, and false when the token holds none.
	TenantClaim() (string, bool)
	sealedIdentity()
}

// IdentityMark makes a type of the runtime module an Identity: embed it in the type.
type IdentityMark struct{}

func (IdentityMark) sealedIdentity() {}

// Decision is the decided permission of one Provision or Retire request, from the authorization runtime.
type Decision interface {
	// Allowed reports whether the authorization port allowed the request.
	Allowed() bool
	sealedDecision()
}

// DecisionMark makes a type of the runtime module a Decision: embed it in the type.
type DecisionMark struct{}

func (DecisionMark) sealedDecision() {}
