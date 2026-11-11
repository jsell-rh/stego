package rhssoauth

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/oidcauth"
)

// The Red Hat SSO configuration of oidc-auth on the runtime data path (ST-SUP-RC21-OC15): the issuer form of Red
// Hat SSO, https://<host>/auth/realms/<realm>, the subject claim sub, the tenant claim org_id and the algorithm
// RS256, which Red Hat SSO signs with.
const (
	// SubjectClaim and TenantClaim are the claims of Red Hat SSO that name the caller and the caller's organization.
	SubjectClaim = "sub"
	TenantClaim  = "org_id"
	// Algorithm is the signing algorithm of Red Hat SSO.
	Algorithm = "RS256"
	// defaultKeysURL is the default of jwk_cert_url, the key document of the store path.
	defaultKeysURL = "https://sso.redhat.com/auth/realms/redhat-external/protocol/openid-connect/certs"
)

// realmPath is the path of a Red Hat SSO issuer: /auth/realms/<realm>.
var realmPath = regexp.MustCompile(`^/auth/realms/[A-Za-z0-9._-]{1,64}$`)

// RuntimeAuth gives the settings of rh-sso-auth on the runtime data path as a configuration of oidc-auth: no
// verifier of its own (ST-SUP-RC21-OC15). It refuses an issuer that is not of the Red Hat SSO form, a key file or a
// key URL, because the key set comes from the discovery document of the issuer, and public paths other than the
// defaults, because the probes of the runtime data path are served outside the verifier.
func (*Generator) RuntimeAuth(ctx gen.Context) (gen.RuntimeAuth, error) {
	text := func(key string) (string, error) {
		value, present := ctx.ComponentConfig[key]
		if !present || value == nil {
			return "", nil
		}
		s, ok := value.(string)
		if !ok {
			return "", fmt.Errorf("the setting %s of rh-sso-auth must be text", key)
		}
		return s, nil
	}
	issuer, err := text("issuer")
	if err != nil {
		return gen.RuntimeAuth{}, err
	}
	if u, err := url.Parse(issuer); err != nil || u.Scheme != "https" || !realmPath.MatchString(u.Path) || u.RawPath != "" {
		return gen.RuntimeAuth{}, fmt.Errorf("the setting issuer of rh-sso-auth must be a Red Hat SSO issuer, https://<host>/auth/realms/<realm>")
	}
	if file, err := text("jwk_cert_file"); err != nil || file != "" {
		return gen.RuntimeAuth{}, fmt.Errorf("the setting jwk_cert_file of rh-sso-auth is not served on the runtime data path: the key set comes from the discovery document of the issuer; a fixed key file is jwt-auth's (ST-SUP-RC11-OC32)")
	}
	if keys, err := text("jwk_cert_url"); err != nil || keys != "" && keys != defaultKeysURL {
		return gen.RuntimeAuth{}, fmt.Errorf("the setting jwk_cert_url of rh-sso-auth is not served on the runtime data path: the key set comes from the discovery document of the issuer")
	}
	if raw, present := ctx.ComponentConfig["public_paths"]; present {
		list, ok := raw.([]any)
		if !ok || !slices.Equal(list, []any{"/healthcheck", "/metrics"}) {
			return gen.RuntimeAuth{}, fmt.Errorf("the setting public_paths of rh-sso-auth is not served on the runtime data path, whose probes answer outside the verifier")
		}
	}
	audience, err := text("audience")
	if err != nil {
		return gen.RuntimeAuth{}, err
	}
	caFile, err := text("jwk_ca_file")
	if err != nil {
		return gen.RuntimeAuth{}, err
	}
	auth, err := oidcauth.Configure("rh-sso-auth", oidcauth.Config{Issuer: issuer, Audience: audience, CAFile: caFile,
		SubjectClaim: SubjectClaim, Algorithms: []string{Algorithm}})
	if err != nil {
		return gen.RuntimeAuth{}, err
	}
	auth.TenantClaim = TenantClaim
	return auth, nil
}
