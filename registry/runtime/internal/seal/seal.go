// Package seal holds the sealed tenant sources of the tenant runtime (ST-SUP-RP04-LC01). The data of an Identity
// and of a Decision lives in a mark of this package, and only the constructors of this package make a mark that is
// not zero. Only a package of stego.dev/runtime can import this package: the authentication runtime makes the
// marks of its Identity values and the authorization runtime the marks of its Decision values.
//
// TenantOf and DecisionOf read the data of the mark through an unexported method of this package, which returns the
// mark itself. An outer type that embeds a genuine value cannot declare that method again, so it cannot change the
// data; methods that an outer type can declare again, such as TenantClaim, are never read here. A zero mark, a nil
// embedded interface or pointer, and a nil value give no tenant.
//
// The limit of the seal: a mark proves that the authentication runtime of this module verified a token and read its
// tenant claim. It does not prove that the verifier the service declared did so. Generated code passes the trust
// settings (the issuer, the audience, the key source and the claim paths) to authn.New, so code that builds its own
// authn.Verifier with other settings gets genuine marks too. The seal stops a tenant that no verification gave; it
// does not check the settings of the verification. Those are proved where the generated code is checked against
// the declaration.
package seal

// identityData is the data of an IdentityMark. Only NewIdentityMark makes one.
type identityData struct {
	tenant  string
	present bool
}

// IdentityMark holds the verified tenant claim of an Identity, and whether the token holds one. Embed it in the
// Identity type. The zero IdentityMark holds no tenant.
type IdentityMark struct {
	data *identityData
}

// NewIdentityMark makes the mark of a verified identity with its tenant claim, for the authentication runtime only.
func NewIdentityMark(tenant string, present bool) IdentityMark {
	return IdentityMark{data: &identityData{tenant: tenant, present: present}}
}

func (m IdentityMark) sealedIdentity() IdentityMark { return m }

// Identity is a verified identity of the authentication runtime: a value whose type embeds an IdentityMark.
type Identity interface {
	sealedIdentity() IdentityMark
}

// TenantOf gives the tenant claim of the identity's mark, and false for a nil identity, a zero mark, a mark with no
// tenant claim or an empty claim, and an identity whose embedded value is nil.
func TenantOf(identity Identity) (tenant string, ok bool) {
	if identity == nil {
		return "", false
	}
	defer func() {
		if recover() != nil {
			tenant, ok = "", false
		}
	}()
	mark := identity.sealedIdentity()
	if mark.data == nil || !mark.data.present || mark.data.tenant == "" {
		return "", false
	}
	return mark.data.tenant, true
}

// decisionData is the data of a DecisionMark. Only NewDecisionMark makes one.
type decisionData struct {
	tenant string
}

// DecisionMark holds the tenant ID that a decided Provision or Retire request is allowed for. Embed it in the
// Decision type. The zero DecisionMark allows nothing.
type DecisionMark struct {
	data *decisionData
}

// NewDecisionMark makes the mark of a request that the authorization port allowed for the tenant, for the
// authorization runtime only. A denied request gets no mark: its Decision holds the zero DecisionMark.
func NewDecisionMark(tenant string) DecisionMark {
	return DecisionMark{data: &decisionData{tenant: tenant}}
}

func (m DecisionMark) sealedDecision() DecisionMark { return m }

// Decision is the decided permission of one Provision or Retire request: a value whose type embeds a DecisionMark.
type Decision interface {
	sealedDecision() DecisionMark
}

// DecisionOf gives the allowed tenant ID of the decision's mark, and false for a nil decision, a zero mark, an empty
// tenant ID, and a decision whose embedded value is nil.
func DecisionOf(decision Decision) (tenant string, ok bool) {
	if decision == nil {
		return "", false
	}
	defer func() {
		if recover() != nil {
			tenant, ok = "", false
		}
	}()
	mark := decision.sealedDecision()
	if mark.data == nil || mark.data.tenant == "" {
		return "", false
	}
	return mark.data.tenant, true
}
