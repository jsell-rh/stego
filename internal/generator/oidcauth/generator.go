// Package oidcauth is the oidc-auth component: sign-in with any OpenID Connect provider on the runtime data path
// (ST-SUP-RC37). It emits no verifier, key set or key source: the verifier of the runtime library verifies each
// token (ST-SUP-RP13), and the component gives the compiler the settings that the declaration states for it, which
// the runtime wiring passes to the service runtime (ST-SUP-RC37-OC04).
package oidcauth

import (
	"fmt"
	"net/url"
	"path"
	"slices"
	"strings"
	"unicode"

	"github.com/jsell-rh/stego/internal/gen"
)

// Component is the name of the component.
const Component = "oidc-auth"

// Algorithms is the allow list of the runtime verifier (ST-SUP-RP13-LC03): a declaration may accept only these.
var Algorithms = []string{"RS256", "ES256"}

// SubjectClaims is the allow list of the subject claim, as the runtime verifier holds it (ST-SUP-RP13-LC13): sub,
// and oid, the immutable object ID of Microsoft Entra ID. A claim that the user or the provider can change, such as
// email, preferred_username or upn, could map two accounts to one subject (ST-SUP-RC37-OC03).
var SubjectClaims = []string{"sub", "oid"}

// Generator is the generator of oidc-auth.
type Generator struct{}

// Config is the sign-in that a declaration states for an OpenID Connect provider: the issuer that names the
// provider, the audience, the accepted signing algorithms, the subject claim, the role claim, the header and the CA
// file. An empty field takes the default of the runtime verifier, except the issuer and the audience, which every
// declaration states.
type Config struct {
	Issuer, Audience, Header, RoleClaim, SubjectClaim, CAFile string
	Algorithms                                                []string
}

// ValidateContext refuses a declaration of oidc-auth that RuntimeAuth refuses.
func (g *Generator) ValidateContext(ctx gen.Context) error {
	_, err := g.RuntimeAuth(ctx)
	return err
}

// RuntimeAuth reads the settings of oidc-auth from the declaration and gives them to the runtime wiring. It refuses
// a service that is not on the runtime data path, which oidc-auth alone serves.
func (*Generator) RuntimeAuth(ctx gen.Context) (gen.RuntimeAuth, error) {
	if !ctx.RuntimeData {
		return gen.RuntimeAuth{}, fmt.Errorf("%s serves the runtime data path only; a service on the GORM store signs in with jwt-auth or rh-sso-auth (ST-SUP-RC20-OC39)", Component)
	}
	c := Config{}
	for key, target := range map[string]*string{"issuer": &c.Issuer, "audience": &c.Audience, "header": &c.Header,
		"roles_claim": &c.RoleClaim, "subject_claim": &c.SubjectClaim, "ca_file": &c.CAFile} {
		value, err := text(ctx.ComponentConfig, key)
		if err != nil {
			return gen.RuntimeAuth{}, err
		}
		*target = value
	}
	if raw, present := ctx.ComponentConfig["algorithms"]; present && raw != nil {
		list, ok := raw.([]any)
		if !ok {
			return gen.RuntimeAuth{}, fmt.Errorf("the setting algorithms of %s must be a list of algorithm names", Component)
		}
		for _, item := range list {
			name, ok := item.(string)
			if !ok {
				return gen.RuntimeAuth{}, fmt.Errorf("the setting algorithms of %s must be a list of algorithm names", Component)
			}
			c.Algorithms = append(c.Algorithms, name)
		}
	}
	return Configure(Component, c)
}

// Generate checks the declaration and emits no file: the runtime verifier does the verification (ST-SUP-RC37-OC04),
// and the compiler passes the settings of RuntimeAuth to it.
func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := g.ValidateContext(ctx); err != nil {
		return nil, nil, err
	}
	return nil, &gen.Wiring{}, nil
}

// Configure checks the sign-in that a component states and gives its runtime settings: an issuer that is an HTTPS
// URL with no user, query or fragment (ST-SUP-RC37-OC01), an audience, accepted algorithms of the allow list each
// once (ST-SUP-RC37-OC02), a subject claim of the allow list sub and oid, a role claim that is a dotted path
// (ST-SUP-RC37-OC03), a header that is an HTTP field name and an absolute CA file.
// The checks are those of the runtime verifier, so a declaration it would refuse at start is refused at generation.
// The component name names the component in each refusal.
func Configure(component string, c Config) (gen.RuntimeAuth, error) {
	refuse := func(format string, args ...any) (gen.RuntimeAuth, error) {
		return gen.RuntimeAuth{}, fmt.Errorf("the setting "+format+" of "+component, args...)
	}
	issuer, err := url.Parse(c.Issuer)
	// The check of the runtime verifier: an HTTPS URL with a host and no user or fragment, which is the same text
	// after TrimSpace and holds no control character (ST-SUP-RP13-LC05), and no query.
	if c.Issuer == "" || err != nil || issuer.Scheme != "https" || issuer.Hostname() == "" || issuer.User != nil ||
		issuer.RawQuery != "" || issuer.ForceQuery || issuer.Fragment != "" || issuer.Opaque != "" || !clean(c.Issuer) ||
		strings.TrimSpace(c.Issuer) != c.Issuer {
		return refuse("issuer must be the HTTPS URL of the provider, with no user, query or fragment;")
	}
	if c.Audience == "" || !clean(c.Audience) || strings.TrimSpace(c.Audience) != c.Audience {
		return refuse("audience must be the audience that each token names;")
	}
	algorithms := slices.Clone(c.Algorithms)
	if len(algorithms) == 0 {
		algorithms = []string{"RS256"}
	}
	for i, name := range algorithms {
		if !slices.Contains(Algorithms, name) || slices.Contains(algorithms[:i], name) {
			return refuse("algorithms must name each accepted algorithm once, from %s (no none, HMAC or other algorithm);", strings.Join(Algorithms, " and "))
		}
	}
	subject := c.SubjectClaim
	if subject == "" {
		subject = "sub"
	}
	if !slices.Contains(SubjectClaims, subject) {
		return refuse("subject_claim must be sub or oid, a claim that the user cannot change;")
	}
	if c.RoleClaim != "" {
		segments := strings.Split(c.RoleClaim, ".")
		if len(c.RoleClaim) > 128 || len(segments) > 8 {
			return refuse("roles_claim must be a dotted claim path;")
		}
		for _, segment := range segments {
			if !claimName(segment) {
				return refuse("roles_claim must be a dotted claim path;")
			}
		}
	}
	header := c.Header
	if header == "" {
		header = "Authorization"
	}
	if !headerName(header) {
		return refuse("header must be an HTTP header name;")
	}
	if c.CAFile != "" && (!path.IsAbs(c.CAFile) || path.Clean(c.CAFile) != c.CAFile || !clean(c.CAFile)) {
		return refuse("ca_file must be an absolute clean path;")
	}
	return gen.RuntimeAuth{Issuer: c.Issuer, Audience: c.Audience, Header: c.Header, RoleClaim: c.RoleClaim,
		SubjectClaim: c.SubjectClaim, CAFile: c.CAFile, Algorithms: algorithms}, nil
}

// text gives a setting that is text, or empty when it is absent.
func text(config map[string]any, key string) (string, error) {
	value, present := config[key]
	if !present || value == nil {
		return "", nil
	}
	s, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("the setting %s of %s must be text", key, Component)
	}
	return s, nil
}

// clean reports whether the text holds no control, format or separator character other than the space.
func clean(s string) bool {
	for _, r := range s {
		if r != ' ' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r)) {
			return false
		}
	}
	return true
}

// claimName reports whether the name is a claim name that the runtime verifier reads: 1 to 64 letters, digits, _
// or -.
func claimName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// headerName reports whether the name is an HTTP header field name (an RFC 9110 token) of at most 64 bytes.
func headerName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", r)) {
			return false
		}
	}
	return true
}
