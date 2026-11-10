package authn

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jsell-rh/stego/registry/runtime/internal/seal"
)

// The bounds of the mapped claims (ST-SUP-RP13-LC13, LC18).
const (
	// MaxRoles is the most roles that the role claim may hold.
	MaxRoles = 128
	// MaxRoleBytes is the longest role.
	MaxRoleBytes = 256
	// MaxTenantBytes is the longest tenant claim.
	MaxTenantBytes = 255
	// MaxProfileBytes is the longest profile claim or attribute.
	MaxProfileBytes = 256
	// MaxAttributes is the most attribute claims that the settings may name.
	MaxAttributes = 32

	maxClaimBytes   = 1024
	maxPathBytes    = 128
	maxPathSegments = 8
	maxKeyIDBytes   = 255
)

// mark is the seal mark of the module, embedded by an alias so that Identity has no exported field. It holds the
// verified tenant claim (ST-SUP-RP04-LC01).
type mark = seal.IdentityMark

// Identity is a verified identity. Only Verify makes one that holds a claim. Its tenant claim lives in the seal mark
// of the runtime module, made by the mark's internal constructor, so tenant.Claim reads it through seal.TenantOf: a
// type outside the runtime module that embeds an Identity and declares TenantClaim again cannot change the tenant
// (ST-SUP-RP13-LC01). It has no setter. The zero Identity holds no claim and no tenant.
type Identity struct {
	mark
	issuer     string
	subject    string
	roles      []string
	attributes map[string]string
	username   string
	email      string
	givenName  string
	familyName string
	expires    time.Time
}

// Principal is the verified issuer and subject pair that names the caller (ST-SUP-RP13-LC13).
type Principal struct {
	Issuer  string
	Subject string
}

// Issuer is the verified issuer.
func (id Identity) Issuer() string { return id.issuer }

// UserID is the verified subject.
func (id Identity) UserID() string { return id.subject }

// Principal is the verified issuer and subject pair.
func (id Identity) Principal() Principal { return Principal{Issuer: id.issuer, Subject: id.subject} }

// Roles is a copy of the roles of the configured role claim; nil when the claim is absent.
func (id Identity) Roles() []string { return slices.Clone(id.roles) }

// TenantClaim is the tenant of the configured tenant claim, read from the seal mark, and false when the verifier has
// no tenant claim path.
func (id Identity) TenantClaim() (string, bool) { return seal.TenantOf(id) }

// Attributes is a copy of the configured attribute claims that the token holds.
func (id Identity) Attributes() map[string]string { return maps.Clone(id.attributes) }

// Username is the preferred_username claim, empty when absent.
func (id Identity) Username() string { return id.username }

// Email is the email claim, empty when absent.
func (id Identity) Email() string { return id.email }

// GivenName is the given_name claim, empty when absent.
func (id Identity) GivenName() string { return id.givenName }

// FamilyName is the family_name claim, empty when absent.
func (id Identity) FamilyName() string { return id.familyName }

// ExpiresAt is the verified expiry.
func (id Identity) ExpiresAt() time.Time { return id.expires }

// claimPath reads a dotted claim path such as realm_access.roles; an empty path is no path.
func claimPath(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	parts := strings.Split(value, ".")
	if len(value) > maxPathBytes || len(parts) > maxPathSegments {
		return nil, setting("a claim path is not valid")
	}
	for _, part := range parts {
		if part == "" {
			return nil, setting("a claim path is not valid")
		}
		for i := 0; i < len(part); i++ {
			c := part[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
				return nil, setting("a claim path is not valid")
			}
		}
	}
	return parts, nil
}

// at gives the value at the path, and false when a member of the path is absent or not an object.
func at(claims map[string]json.RawMessage, path []string) (json.RawMessage, bool) {
	object := claims
	for i, name := range path {
		value, ok := object[name]
		if !ok {
			return nil, false
		}
		if i == len(path)-1 {
			return value, true
		}
		object = nil
		if len(value) == 0 || value[0] != '{' || json.Unmarshal(value, &object) != nil || object == nil {
			return nil, false
		}
	}
	return nil, false
}

// readRoles reads the roles only at the configured path, with no fallback (ST-SUP-RC11-OC08). A claim that is
// absent at the path gives no roles; a claim that is present must be an array of at most MaxRoles clean strings of
// at most MaxRoleBytes each. With no path, the identity has no roles.
func readRoles(claims map[string]json.RawMessage, path []string) ([]string, error) {
	if len(path) == 0 {
		return nil, nil
	}
	raw, ok := at(claims, path)
	if !ok {
		return nil, nil
	}
	if len(raw) == 0 || raw[0] != '[' {
		return nil, errRoles
	}
	var values []json.RawMessage
	if json.Unmarshal(raw, &values) != nil || len(values) > MaxRoles {
		return nil, errRoles
	}
	roles := make([]string, 0, len(values))
	for _, value := range values {
		var role string
		if len(value) == 0 || value[0] != '"' || json.Unmarshal(value, &role) != nil || role == "" ||
			len(role) > MaxRoleBytes || !clean(role) {
			return nil, errRoles
		}
		roles = append(roles, role)
	}
	return roles, nil
}

// readTenant reads the tenant only at the configured path, with no fallback: the claim must be one clean string of
// 1 to MaxTenantBytes bytes. With no path, the identity has no tenant claim.
func readTenant(claims map[string]json.RawMessage, path []string) (string, bool, error) {
	if len(path) == 0 {
		return "", false, nil
	}
	raw, ok := at(claims, path)
	if !ok || len(raw) == 0 || raw[0] != '"' {
		return "", false, errTenant
	}
	var tenant string
	if json.Unmarshal(raw, &tenant) != nil || tenant == "" || len(tenant) > MaxTenantBytes || !clean(tenant) {
		return "", false, errTenant
	}
	return tenant, true, nil
}

// profile is the profile claims of an Identity (ST-SUP-RP13-LC18).
type profile struct {
	attributes map[string]string
	username   string
	email      string
	givenName  string
	familyName string
}

// readProfile reads preferred_username, email, given_name, family_name and the configured attribute claims. Each is
// a clean string of at most MaxProfileBytes when present and empty when absent; a present claim of another type is
// refused.
func readProfile(claims map[string]json.RawMessage, attributes []string) (profile, error) {
	var p profile
	for name, field := range map[string]*string{"preferred_username": &p.username, "email": &p.email,
		"given_name": &p.givenName, "family_name": &p.familyName} {
		value, present, err := profileClaim(claims, name)
		if err != nil {
			return profile{}, err
		}
		if present {
			*field = value
		}
	}
	for _, name := range attributes {
		value, present, err := profileClaim(claims, name)
		if err != nil {
			return profile{}, err
		}
		if present {
			if p.attributes == nil {
				p.attributes = make(map[string]string, len(attributes))
			}
			p.attributes[name] = value
		}
	}
	return p, nil
}

func profileClaim(claims map[string]json.RawMessage, name string) (string, bool, error) {
	raw, present := claims[name]
	if !present {
		return "", false, nil
	}
	value, ok := stringMember(claims, name)
	if !ok || len(raw) == 0 || len(value) > MaxProfileBytes || !clean(value) {
		return "", false, errProfile
	}
	return value, true, nil
}

// clean reports whether the text is valid UTF-8 with no control character (NUL included), no format character
// (Unicode Cf, such as U+202E and U+200B), no line or paragraph separator (Zl, Zp), no replacement character and no
// space at either end.
func clean(text string) bool {
	if !utf8.ValidString(text) || strings.TrimSpace(text) != text {
		return false
	}
	for _, r := range text {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) || r == utf8.RuneError {
			return false
		}
	}
	return true
}
