package authn

import (
	"encoding/json"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"stego.dev/runtime/internal/seal"
)

// The bounds of the mapped claims (ST-SUP-RP13-LC13).
const (
	// MaxRoles is the most roles that the role claim may hold.
	MaxRoles = 128
	// MaxRoleBytes is the longest role.
	MaxRoleBytes = 256
	// MaxTenantBytes is the longest tenant claim.
	MaxTenantBytes = 255

	maxClaimBytes   = 1024
	maxPathBytes    = 128
	maxPathSegments = 8
	maxKeyIDBytes   = 255
)

// mark is the seal mark of the module, embedded by an alias so that Identity has no exported field.
type mark = seal.IdentityMark

// Identity is a verified identity. Only Verify makes one that holds a claim, and it embeds the seal mark of the
// runtime module, so tenant.Claim accepts it and no package outside the runtime module can make an Identity of its
// own (ST-SUP-RP13-LC01). It has no setter. The zero Identity holds no claim and no tenant.
type Identity struct {
	mark
	issuer    string
	subject   string
	roles     []string
	tenant    string
	hasTenant bool
	expires   time.Time
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

// Roles is a copy of the roles of the configured role claim.
func (id Identity) Roles() []string { return slices.Clone(id.roles) }

// TenantClaim is the tenant of the configured tenant claim, and false when the verifier has no tenant claim path.
func (id Identity) TenantClaim() (string, bool) { return id.tenant, id.hasTenant }

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

// readRoles reads the roles only at the configured path, with no fallback: the claim must be an array of at most
// MaxRoles clean strings of at most MaxRoleBytes each. With no path, the identity has no roles.
func readRoles(claims map[string]json.RawMessage, path []string) ([]string, error) {
	if len(path) == 0 {
		return nil, nil
	}
	raw, ok := at(claims, path)
	if !ok || len(raw) == 0 || raw[0] != '[' {
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

// clean reports whether the text is valid UTF-8 with no control character (NUL included), no replacement
// character and no space at either end.
func clean(text string) bool {
	if !utf8.ValidString(text) || strings.TrimSpace(text) != text {
		return false
	}
	for _, r := range text {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return false
		}
	}
	return true
}
