// Package decision is the seal of a decided permission between the packages of the runtime module. Because the
// package is internal, only a package of stego.dev/runtime can build a Decided value: the authorization runtime
// builds it from the decision port's answer, and the tenant runtime accepts it as the decision of a Provision or
// Retire request (ST-SUP-RP04-LC01). Generated code cannot import this package, so it cannot make a decision up.
package decision

// Decided is the decided permission of one request. The zero value is not allowed.
type Decided struct {
	allowed bool
}

// New returns the decision that the authorization port gave.
func New(allowed bool) Decided { return Decided{allowed: allowed} }

// Allowed reports whether the authorization port allowed the request.
func (d Decided) Allowed() bool { return d.allowed }
