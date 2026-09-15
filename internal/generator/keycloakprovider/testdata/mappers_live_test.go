package keycloak

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"
)

func testLiveMapperPolicy(t *testing.T, c *Client, ctx context.Context, owner, target ClientBinding, subject string, roles RolePolicy, groupID string) {
	t.Helper()
	request := func(method, path string, body []byte, status int) []byte {
		t.Helper()
		response, err := c.admin(ctx, method, path, body)
		if err != nil || response.StatusCode != status {
			t.Fatal("real mapper fixture request failed", method, status, err)
		}
		return response.Body
	}
	claim := "catalog.roles"
	metadata := true
	if target.ClientID == "batch-worker" {
		claim = "pipeline.permissions"
		metadata = false
	}
	p := TokenClaimsPolicy{AudienceClients: []ClientBinding{target}, CustomAudiences: []string{"urn:" + target.ClientID}, ClientRoles: []ClientRoleClaim{{Client: target, Claim: claim}}, RealmRolesClaim: "access.realm_roles", ClientMetadata: metadata}
	path := "/clients/" + owner.ID + "/protocol-mappers/models"
	// The fixture introduces a real excess claim. Only the common provider
	// removes it and installs the declared token policy.
	request(http.MethodPost, path, []byte(`{"name":"excess-claim","protocol":"openid-connect","protocolMapper":"oidc-hardcoded-claim-mapper","config":{"claim.name":"unwanted","claim.value":"private","jsonType.label":"String","access.token.claim":"true"}}`), http.StatusCreated)
	if err := c.ReconcileTokenMappers(ctx, owner, p); err != nil {
		t.Fatal("real mapper reconciliation failed", err)
	}
	before, err := c.readMappers(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.ReconcileTokenMappers(ctx, owner, p); err != nil {
		t.Fatal(err)
	}
	after, err := c.readMappers(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, m := range before {
		ids[m.Name] = m.ID
	}
	for _, m := range after {
		if ids[m.Name] != m.ID {
			t.Fatal("real converged mapper identity changed")
		}
	}
	for _, m := range after {
		if m.Type == "oidc-usermodel-client-role-mapper" {
			m.Config["userinfo.token.claim"] = "true"
			body, e := json.Marshal(m)
			if e != nil {
				t.Fatal(e)
			}
			request(http.MethodPut, path+"/"+m.ID, body, http.StatusNoContent)
		}
	}
	if err = c.InspectTokenMappers(ctx, owner, p); !errors.Is(err, ErrMapperPolicy) {
		t.Fatal("real changed mapper accepted", err)
	}
	if err = c.ReconcileTokenMappers(ctx, owner, p); err != nil {
		t.Fatal("real mapper repair failed", err)
	}
	if err = c.InspectClientScopes(ctx, owner, roles); err != nil {
		t.Fatal("mapper operation changed role scopes", err)
	}
	accessPolicy := ServiceAccountAccessPolicy{Client: ServiceAccountPolicy{DisplayName: "Role worker", AccessTokenLifetimeSeconds: 300}, Subject: subject, Roles: roles, Scopes: roles, Claims: p}
	if err = c.ReconcileServiceAccountAccess(ctx, owner, accessPolicy); err != nil {
		reportServiceAccessDifference(t, c, ctx, owner, accessPolicy)
		t.Fatal("real checked service-account enablement failed", err)
	}
	request(http.MethodPut, "/users/"+subject+"/groups/"+groupID, nil, http.StatusNoContent)
	request(http.MethodPut, "/clients/"+owner.ID, []byte(`{"name":"drift","attributes":{"stego.test.unwanted":"remove"}}`), http.StatusNoContent)
	if err = c.InspectServiceAccountAccess(ctx, owner, accessPolicy); err == nil {
		t.Fatal("real service-account access drift accepted")
	}
	for i := 0; i < 2; i++ {
		if err = c.ReconcileServiceAccountAccess(ctx, owner, accessPolicy); err != nil {
			reportServiceAccessDifference(t, c, ctx, owner, accessPolicy)
			t.Fatal("real checked service-account repair failed", err)
		}
	}
	if err = c.InspectServiceAccountAccess(ctx, owner, accessPolicy); err != nil {
		t.Fatal("real service-account access not confirmed", err)
	}
	t.Log("Real checked service-account access passed; configuration and group drift repaired; repeated reconciliation and signed token proof passed")
	if err = c.InspectTokenMappers(ctx, owner, p); err != nil {
		t.Fatal("real enabled mapper inspection failed", err)
	}
	if err = c.InspectClientScopes(ctx, owner, roles); err != nil {
		t.Fatal("enablement changed scopes", err)
	}
	proof := ServiceAccountTokenPolicy{Subject: subject, AccessTokenLifetimeSeconds: 300, Audiences: []string{target.ClientID, "urn:" + target.ClientID}, RoleClaims: map[string][]string{claim: roles.Clients[0].Names, "access.realm_roles": roles.Realm}}
	if err = c.VerifyServiceAccountToken(ctx, owner, proof); err != nil {
		t.Fatal("real service-account token proof failed", err)
	}
	wrong := proof
	wrong.Subject = "other-subject"
	if err = c.VerifyServiceAccountToken(ctx, owner, wrong); !errors.Is(err, ErrTokenPolicy) {
		t.Fatal("wrong service-account subject accepted", err)
	}
	wrong = proof
	wrong.Audiences = []string{"wrong-audience"}
	if err = c.VerifyServiceAccountToken(ctx, owner, wrong); !errors.Is(err, ErrTokenPolicy) {
		t.Fatal("wrong service-account audience accepted", err)
	}
	t.Log("Real common service-account token proof passed; wrong subject and audience denied")
	secret, err := c.GetClientSecret(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {owner.ClientID}, "client_secret": {secret.Reveal()}}
	response, err := c.http.Do(ctx, http.MethodPost, "/realms/provider-test/protocol/openid-connect/token", http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, []byte(form.Encode()))
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatal("real mapped token grant failed", err)
	}
	var grant struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}
	if decode(response.Body, &grant) != nil || !strings.EqualFold(grant.TokenType, "Bearer") || grant.RefreshToken != "" || grant.IDToken != "" {
		t.Fatal("real token grant shape differs")
	}
	claims := verifyLiveMapperToken(t, c, ctx, grant.AccessToken)
	if claims["iss"] != c.Issuer() || claims["sub"] != subject || claims["azp"] != owner.ClientID {
		t.Fatal("real token identity differs")
	}
	var aud []string
	if value, ok := claims["aud"].([]any); ok {
		for _, entry := range value {
			s, valid := entry.(string)
			if !valid {
				t.Fatal("invalid audience")
			}
			aud = append(aud, s)
		}
	} else {
		t.Fatal("real audience set differs")
	}
	sort.Strings(aud)
	expected := []string{target.ClientID, "urn:" + target.ClientID}
	sort.Strings(expected)
	if strings.Join(aud, "\n") != strings.Join(expected, "\n") {
		t.Fatal("real token audiences differ")
	}
	exactRoles := func(path string, wanted []string) {
		t.Helper()
		var value any = claims
		for _, part := range strings.Split(path, ".") {
			object, ok := value.(map[string]any)
			if !ok {
				t.Fatal("role claim path is absent")
			}
			value = object[part]
		}
		entries, ok := value.([]any)
		if !ok || len(entries) != len(wanted) {
			t.Fatal("role claim shape differs")
		}
		actual := []string{}
		for _, entry := range entries {
			s, ok := entry.(string)
			if !ok {
				t.Fatal("role claim is not a string")
			}
			actual = append(actual, s)
		}
		sort.Strings(actual)
		want := append([]string{}, wanted...)
		sort.Strings(want)
		if strings.Join(actual, "\n") != strings.Join(want, "\n") {
			t.Fatal("real token roles differ")
		}
	}
	exactRoles(claim, roles.Clients[0].Names)
	exactRoles("access.realm_roles", roles.Realm)
	for _, key := range []string{"unwanted", "resource_access", "realm_access", "email", "name"} {
		if _, ok := claims[key]; ok {
			t.Fatal("unexpected inherited token claim", key)
		}
	}
	iat, ok := claims["iat"].(float64)
	if !ok {
		t.Fatal("issued time absent")
	}
	exp, ok := claims["exp"].(float64)
	now := float64(time.Now().Unix())
	if !ok || exp-iat < 295 || exp-iat > 305 || exp <= now || iat > now+5 || iat < now-30 {
		t.Fatal("real token lifetime differs")
	}
	if metadata {
		if claims["client_id"] != owner.ClientID {
			t.Fatal("client metadata differs")
		}
		for _, key := range []string{"clientHost", "clientAddress"} {
			if value, ok := claims[key].(string); !ok || value == "" {
				t.Fatal("client metadata is absent")
			}
		}
	} else {
		for _, key := range []string{"client_id", "clientHost", "clientAddress"} {
			if _, ok := claims[key]; ok {
				t.Fatal("undeclared client metadata")
			}
		}
	}
	if err = c.DisableClient(ctx, owner); err != nil {
		t.Fatal(err)
	}
	testLiveOwnershipMigration(t, c, ctx, owner)
	if err = c.ReconcileServiceAccountAccess(ctx, owner, accessPolicy); err != nil {
		t.Fatal("service-account access after migration failed", err)
	}
	if err = c.DisableClient(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if err = c.ReconcileClientScopes(ctx, owner, roles); err != nil {
		t.Fatal("scope repair after disablement failed", err)
	}
	if err = c.ReconcileTokenMappers(ctx, owner, p); err != nil {
		t.Fatal(err)
	}
	t.Log("Real mapper checks: exact configuration; stable IDs; drift repair; signed token identity, audiences, roles, lifetime, and metadata; no excess inherited claims")
}

// This verifier belongs only to the test. It independently checks signatures
// from the fixed TLS issuer. It does not use mapper comparison code as proof.
func verifyLiveMapperToken(t *testing.T, c *Client, ctx context.Context, token string) map[string]any {
	t.Helper()
	if len(token) > 16384 {
		t.Fatal("test token is too large")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatal("invalid test JWT")
	}
	read := func(part string) []byte {
		t.Helper()
		value, err := base64.RawURLEncoding.Strict().DecodeString(part)
		if err != nil {
			t.Fatal("invalid JWT encoding")
		}
		return value
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if decode(read(parts[0]), &header) != nil || header.Alg != "RS256" || header.Kid == "" {
		t.Fatal("unexpected test signing key")
	}
	response, err := c.http.Do(ctx, http.MethodGet, "/realms/provider-test/protocol/openid-connect/certs", nil, nil)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatal("read test signing keys", err)
	}
	var keys struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			Alg string `json:"alg"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if decode(response.Body, &keys) != nil || len(keys.Keys) > 32 {
		t.Fatal("invalid test signing keys")
	}
	var key *rsa.PublicKey
	for _, entry := range keys.Keys {
		if entry.Kid == header.Kid {
			if key != nil || entry.Kty != "RSA" || entry.Alg != "RS256" || entry.Use != "sig" {
				t.Fatal("ambiguous test signing key")
			}
			n := new(big.Int).SetBytes(read(entry.N))
			e := new(big.Int).SetBytes(read(entry.E))
			if n.BitLen() < 2048 || n.BitLen() > 4096 || !e.IsInt64() || e.Int64() != 65537 {
				t.Fatal("invalid test RSA key")
			}
			key = &rsa.PublicKey{N: n, E: 65537}
		}
	}
	if key == nil {
		t.Fatal("test signing key missing")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], read(parts[2])) != nil {
		t.Fatal("test token signature differs")
	}
	var claims map[string]any
	if decode(read(parts[1]), &claims) != nil || claims == nil {
		t.Fatal("invalid signed claims")
	}
	return claims
}

func reportServiceAccessDifference(t *testing.T, c *Client, ctx context.Context, b ClientBinding, p ServiceAccountAccessPolicy) {
	t.Helper()
	value, _, err := c.boundClient(ctx, b)
	if err != nil {
		t.Log("service-account diagnostic read failed", err)
		return
	}
	desired, _, err := p.configuration(b)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range desired.Attributes {
		if value.Attributes[key] != want {
			t.Log("service-account attribute differs:", key)
		}
	}
	for key := range value.Attributes {
		if _, ok := desired.Attributes[key]; !ok && key != "client.secret.creation.time" {
			t.Log("extra service-account attribute:", key)
		}
	}
}
