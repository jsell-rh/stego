package authn

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/jsell-rh/stego/registry/runtime/authn/authntest"
	"github.com/jsell-rh/stego/registry/runtime/internal/seal"
)

const (
	testIssuer   = "https://issuer.authn.test/realms/r1"
	testAudience = "orders-api"
)

// clock is a fake clock that is safe for concurrent use.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Now()} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// staticVerifier gives a verifier of a fixed key set that holds the keys, with the role and tenant paths of
// ST-SUP-RP13-LC13-T01 and a skew of 60 s. The change function, when given, changes the settings first.
func staticVerifier(t *testing.T, change func(*Settings), keys ...*authntest.Key) *Verifier {
	t.Helper()
	jwks := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		jwks = append(jwks, key.JWK())
	}
	set, err := ParseKeys(authntest.Document(jwks...))
	if err != nil {
		t.Fatalf("ParseKeys: %v", err)
	}
	settings := Settings{Issuer: testIssuer, Audience: testAudience, ProblemCode: "SVC", Skew: MaxSkew,
		RoleClaim: "realm_access.roles", TenantClaim: "tenant", Keys: set}
	if change != nil {
		change(&settings)
	}
	v, err := New(settings)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return v
}

// goodClaims are valid claims with roles and a tenant at the configured paths.
func goodClaims() map[string]any {
	claims := authntest.Claims(testIssuer, testAudience, "user-1", time.Now())
	claims["realm_access"] = map[string]any{"roles": []string{"reader", "writer"}}
	claims["tenant"] = "t1"
	return claims
}

func isZero(id Identity) bool {
	_, hasTenant := seal.TenantOf(id)
	return id.issuer == "" && id.subject == "" && id.roles == nil && !hasTenant && id.attributes == nil &&
		id.username == "" && id.email == "" && id.givenName == "" && id.familyName == "" && id.expires.IsZero()
}

// expectRefused fails unless the error is the refusal want and the identity is zero.
func expectRefused(t *testing.T, id Identity, err error, want error) {
	t.Helper()
	if err == nil {
		t.Fatalf("the token was accepted, want the refusal %q", want)
	}
	if !errors.Is(err, ErrDenied) {
		t.Errorf("the error %q is not ErrDenied", err)
	}
	if !errors.Is(err, want) {
		t.Errorf("the error is %q, want %q", err, want)
	}
	if !isZero(id) {
		t.Errorf("the refusal gave a partial identity: %+v", id)
	}
}

func expectAccepted(t *testing.T, v *Verifier, token string) Identity {
	t.Helper()
	id, err := v.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("the control token was refused: %v", err)
	}
	return id
}

func TestVerifyChecks(t *testing.T) {
	t.Run("ST-SUP-RP13-LC02-T01 A failing check gives no claim", func(t *testing.T) {
		key := authntest.NewKey(t, "k1")
		other := authntest.NewKey(t, "k1")
		v := staticVerifier(t, nil, key)
		header := authntest.Header("k1")
		expectAccepted(t, v, authntest.Mint(t, header, goodClaims(), key))
		cases := []struct {
			name   string
			change func(map[string]any)
			signer *authntest.Key
			want   error
		}{
			{"signature", nil, other, errSignature},
			{"issuer", func(c map[string]any) { c["iss"] = "https://other.authn.test/realms/r1" }, key, errIssuer},
			{"audience", func(c map[string]any) { c["aud"] = "billing-api" }, key, errAudience},
			{"subject", func(c map[string]any) { delete(c, "sub") }, key, errSubject},
			{"issue time", func(c map[string]any) { c["iat"] = time.Now().Add(10 * time.Minute).Unix() }, key, errIssuedAt},
			{"expiry", func(c map[string]any) {
				c["iat"] = time.Now().Add(-time.Hour).Unix()
				c["exp"] = time.Now().Add(-10 * time.Minute).Unix()
			}, key, errExpiry},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				claims := goodClaims()
				if c.change != nil {
					c.change(claims)
				}
				id, err := v.Verify(context.Background(), authntest.Mint(t, header, claims, c.signer))
				expectRefused(t, id, err, c.want)
			})
		}
	})
}

func TestVerifyAlgorithm(t *testing.T) {
	key := authntest.NewKey(t, "k1")
	v := staticVerifier(t, nil, key)

	t.Run("ST-SUP-RP13-LC03-T01 Only RS256 is accepted", func(t *testing.T) {
		ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		withAlg := func(alg string) map[string]any {
			h := authntest.Header("k1")
			h["alg"] = alg
			return h
		}
		claims := goodClaims()
		expectAccepted(t, v, authntest.Mint(t, authntest.Header("k1"), claims, key))
		cases := []struct {
			name  string
			token string
			want  error
		}{
			// alg none with no signature, as an attacker sends it.
			{"none unsigned", authntest.Mint(t, withAlg("none"), claims, nil), errFormat},
			// alg none over a valid RS256 signature: only the allow list can refuse it.
			{"none signed", rs256Under(t, withAlg("none"), claims, key), errAlgorithm},
			// The algorithm confusion attack: HS256 with the RSA public key as the HMAC secret.
			{"HS256 confusion", authntest.Mint(t, withAlg("HS256"), claims, key.PublicPEM()), errAlgorithm},
			{"ES256", authntest.Mint(t, withAlg("ES256"), claims, ecKey), errAlgorithm},
			{"PS256", authntest.Mint(t, withAlg("PS256"), claims, key), errAlgorithm},
			{"RS256 in lower case", rs256Under(t, withAlg("rs256"), claims, key), errAlgorithm},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				id, err := v.Verify(context.Background(), c.token)
				expectRefused(t, id, err, c.want)
			})
		}

		// A verifier whose settings name both algorithms of the allow list, with an RSA and a P-256 key: each
		// algorithm passes with its own key only.
		ec := authntest.NewECKey(t, "e1")
		set, err := ParseKeys(authntest.Document(key.JWK(), ec.JWK()))
		if err != nil {
			t.Fatal(err)
		}
		both, err := New(Settings{Issuer: testIssuer, Audience: testAudience, ProblemCode: "SVC", Keys: set,
			Algorithms: []string{AlgorithmRS256, AlgorithmES256}})
		if err != nil {
			t.Fatal(err)
		}
		expectAccepted(t, both, authntest.Mint(t, authntest.HeaderOf("RS256", "k1"), claims, key))
		expectAccepted(t, both, authntest.Mint(t, authntest.HeaderOf("ES256", "e1"), claims, ec))
		for _, c := range []struct {
			name  string
			token string
		}{
			// An ES256 token that names the RSA key, and an RS256 token that names the P-256 key.
			{"ES256 with the RSA key ID", authntest.Mint(t, authntest.HeaderOf("ES256", "k1"), claims, ec)},
			{"RS256 with the P-256 key ID", rs256Under(t, authntest.HeaderOf("RS256", "e1"), claims, key)},
			{"PS256", authntest.Mint(t, authntest.HeaderOf("PS256", "k1"), claims, key)},
			{"HS256 with the RSA key as the secret", authntest.Mint(t, authntest.HeaderOf("HS256", "k1"), claims, key.PublicPEM())},
			{"none signed", rs256Under(t, authntest.HeaderOf("none", "k1"), claims, key)},
		} {
			t.Run("both: "+c.name, func(t *testing.T) {
				id, err := both.Verify(context.Background(), c.token)
				expectRefused(t, id, err, errAlgorithm)
			})
		}
		// An ES256 verifier refuses an RS256 token, and New refuses every algorithm outside the allow list.
		esOnly, err := New(Settings{Issuer: testIssuer, Audience: testAudience, ProblemCode: "SVC", Keys: set,
			Algorithms: []string{AlgorithmES256}})
		if err != nil {
			t.Fatal(err)
		}
		id, err := esOnly.Verify(context.Background(), authntest.Mint(t, authntest.HeaderOf("RS256", "k1"), claims, key))
		expectRefused(t, id, err, errAlgorithm)
		for _, algorithms := range [][]string{{"none"}, {"HS256"}, {"PS256"}, {"rs256"}, {"RS256", "RS256"}, {""}} {
			if _, err := New(Settings{Issuer: testIssuer, Audience: testAudience, ProblemCode: "SVC", Keys: set,
				Algorithms: algorithms}); !errors.Is(err, ErrSettings) {
				t.Errorf("New with the algorithms %q gave %v, want a refusal of the settings", algorithms, err)
			}
		}
	})

	t.Run("ST-SUP-RP13-LC03-T02 Unsupported headers are refused", func(t *testing.T) {
		cases := []struct {
			name   string
			member string
			value  any
			want   error
		}{
			{"typ JOSE", "typ", "JOSE", errType},
			{"typ at+JWT in another case", "typ", "AT+JWT", errType},
			{"typ not text", "typ", 7, errType},
			{"crit", "crit", []string{"exp"}, errHeader},
			{"jku", "jku", "https://attacker.authn.test/keys", errHeader},
			{"jwk", "jwk", key.JWK(), errHeader},
			{"x5u", "x5u", "https://attacker.authn.test/cert", errHeader},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				header := authntest.Header("k1")
				header[c.member] = c.value
				id, err := v.Verify(context.Background(), authntest.Mint(t, header, goodClaims(), key))
				expectRefused(t, id, err, c.want)
			})
		}
	})
}

func TestTokenKind(t *testing.T) {
	t.Run("ST-SUP-RP13-LC03-T03 A token shaped as an ID token is refused, and an at+jwt access token passes", func(t *testing.T) {
		key := authntest.NewKey(t, "k1")
		v := staticVerifier(t, nil, key)
		// An access token of RFC 9068 names its type at+jwt, in the short or the media type form.
		for _, typ := range []string{"JWT", "at+jwt", "application/at+jwt"} {
			header := authntest.Header("k1")
			header["typ"] = typ
			expectAccepted(t, v, authntest.Mint(t, header, goodClaims(), key))
		}
		// An ID token holds at_hash, c_hash or nonce; each marks the token as no access token.
		for _, marker := range []string{"at_hash", "c_hash", "nonce"} {
			t.Run(marker, func(t *testing.T) {
				claims := goodClaims()
				claims[marker] = "abc"
				id, err := v.Verify(context.Background(), authntest.Mint(t, authntest.Header("k1"), claims, key))
				expectRefused(t, id, err, errIDToken)
			})
		}
	})
}

// rs256Under signs the header and claims with RS256 whatever alg the header names.
func rs256Under(t *testing.T, header map[string]any, claims map[string]any, key *authntest.Key) string {
	t.Helper()
	head, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	input := base64.RawURLEncoding.EncodeToString(head) + "." + base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(input))
	signature, err := rsa.SignPKCS1v15(nil, key.Private, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func TestVerifySettings(t *testing.T) {
	key := authntest.NewKey(t, "k1")
	set, err := ParseKeys(authntest.Document(key.JWK()))
	if err != nil {
		t.Fatal(err)
	}
	valid := Settings{Issuer: testIssuer, Audience: testAudience, ProblemCode: "SVC", Skew: 60 * time.Second, Keys: set}

	t.Run("ST-SUP-RP13-LC05-T01 Trust settings are checked at construction", func(t *testing.T) {
		if v, err := New(valid); err != nil || v == nil {
			t.Fatalf("New refused valid settings: %v", err)
		}
		cases := map[string]func(*Settings){
			"http issuer":                  func(s *Settings) { s.Issuer = "http://issuer.authn.test/realms/r1" },
			"issuer with a trailing space": func(s *Settings) { s.Issuer = "https://issuer.authn.test/realms/r1 " },
			"empty audience":               func(s *Settings) { s.Audience = "" },
			"skew of 61 s":                 func(s *Settings) { s.Skew = 61 * time.Second },
			"skew of -1 s":                 func(s *Settings) { s.Skew = -time.Second },
		}
		for name, change := range cases {
			t.Run(name, func(t *testing.T) {
				s := valid
				change(&s)
				v, err := New(s)
				if err == nil || v != nil {
					t.Fatalf("New gave %v, %v, want an error", v, err)
				}
				if !errors.Is(err, ErrSettings) {
					t.Errorf("the error %q is not ErrSettings", err)
				}
			})
		}
	})

	t.Run("ST-SUP-RP13-LC05-T02 Issuer, audience and expiry are exact and use the skew", func(t *testing.T) {
		v, err := New(valid)
		if err != nil {
			t.Fatal(err)
		}
		header := authntest.Header("k1")
		now := time.Now()
		claims := func(change func(map[string]any)) map[string]any {
			c := authntest.Claims(testIssuer, testAudience, "user-1", now)
			change(c)
			return c
		}
		expired := func(ago time.Duration) func(map[string]any) {
			return func(c map[string]any) {
				c["iat"] = now.Add(-10 * time.Minute).Unix()
				c["exp"] = now.Add(-ago).Unix()
			}
		}
		issued := func(ahead time.Duration) func(map[string]any) {
			return func(c map[string]any) { c["iat"] = now.Add(ahead).Unix() }
		}
		for name, change := range map[string]func(map[string]any){
			"expired 30 s ago":   expired(30 * time.Second),
			"issued 30 s ahead":  issued(30 * time.Second),
			"audience in a list": func(c map[string]any) { c["aud"] = []string{"billing-api", testAudience} },
		} {
			t.Run(name, func(t *testing.T) {
				expectAccepted(t, v, authntest.Mint(t, header, claims(change), key))
			})
		}
		cases := []struct {
			name   string
			change func(map[string]any)
			want   error
		}{
			{"issuer with a trailing slash", func(c map[string]any) { c["iss"] = testIssuer + "/" }, errIssuer},
			{"audience that is a prefix", func(c map[string]any) { c["aud"] = "orders" }, errAudience},
			{"audience in other case", func(c map[string]any) { c["aud"] = "Orders-API" }, errAudience},
			{"expired 90 s ago", expired(90 * time.Second), errExpiry},
			{"issued 90 s ahead, outside the skew", issued(90 * time.Second), errIssuedAt},
			{"not before 90 s ahead", func(c map[string]any) { c["nbf"] = now.Add(90 * time.Second).Unix() }, errNotBefore},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				id, err := v.Verify(context.Background(), authntest.Mint(t, header, claims(c.change), key))
				expectRefused(t, id, err, c.want)
			})
		}
	})
}

func TestVerifyShape(t *testing.T) {
	key := authntest.NewKey(t, "k1")
	v := staticVerifier(t, nil, key)
	header := `{"alg":"RS256","typ":"JWT","kid":"k1"}`
	payload := func(extra string) []byte {
		now := time.Now()
		return fmt.Appendf(nil, `{"iss":%q,"aud":%q,"sub":"user-1","iat":%d,"exp":%d,"realm_access":{"roles":["reader"]},"tenant":"t1"%s}`,
			testIssuer, testAudience, now.Unix(), now.Add(5*time.Minute).Unix(), extra)
	}
	mint := func(head string, body []byte) string { return authntest.Mint(t, []byte(head), body, key) }

	t.Run("ST-SUP-RP13-LC06-T01 Oversize and ambiguous tokens are refused", func(t *testing.T) {
		// A token of exactly MaxTokenBytes is read; one byte more is refused.
		sized := func(size int) string {
			// The RS256 signature of a 2048-bit key is 342 characters; find the padding without signing.
			for hpad := 0; hpad < 3; hpad++ {
				head := header[:len(header)-1] + `,"p":"` + strings.Repeat("h", hpad) + `"}`
				for pad := 0; pad < MaxTokenBytes; pad++ {
					body := payload(`,"pad":"` + strings.Repeat("x", pad) + `"`)
					n := base64.RawURLEncoding.EncodedLen(len(head)) + 1 + base64.RawURLEncoding.EncodedLen(len(body)) + 1 + 342
					if n == size {
						token := mint(head, body)
						if len(token) != size {
							t.Fatalf("the token has %d bytes, want %d", len(token), size)
						}
						return token
					}
					if n > size {
						break
					}
				}
			}
			t.Fatalf("no token of %d bytes", size)
			return ""
		}
		expectAccepted(t, v, sized(MaxTokenBytes))
		nest := func(levels int) string {
			// The top object is level 1, so the member holds levels-1 nested arrays.
			return `,"deep":` + strings.Repeat("[", levels-1) + strings.Repeat("]", levels-1)
		}
		expectAccepted(t, v, mint(header, payload(nest(MaxDepth))))
		expectAccepted(t, v, mint(header, payload(`,"name":"😀"`)))
		cases := []struct {
			name  string
			token string
			want  error
		}{
			{"16 KiB plus one byte", sized(MaxTokenBytes + 1), errSize},
			{"duplicate payload member", mint(header, payload(`,"sub":"admin"`)), errDuplicate},
			{"duplicate nested member", mint(header, payload(`,"x":{"a":1,"a":2}`)), errDuplicate},
			{"duplicate header member", mint(`{"alg":"RS256","typ":"JWT","kid":"k1","kid":"k2"}`, payload("")), errDuplicate},
			{"33 levels of nesting", mint(header, payload(nest(MaxDepth+1))), errDepth},
			{"unpaired high surrogate", mint(header, payload(`,"name":"\ud800"`)), errUnicode},
			{"unpaired low surrogate", mint(header, payload(`,"name":"\udc00x"`)), errUnicode},
			{"trailing data", mint(header, append(payload(""), []byte(`{}`)...)), errJSON},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				id, err := v.Verify(context.Background(), c.token)
				expectRefused(t, id, err, c.want)
			})
		}
	})
}

// recorder records the spans of a tracer.
func recorder() (*tracetest.SpanRecorder, *sdktrace.TracerProvider) {
	r := tracetest.NewSpanRecorder()
	return r, sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(r))
}

func spanText(span sdktrace.ReadOnlySpan) string {
	var b strings.Builder
	b.WriteString(span.Name() + " " + span.Status().Description)
	for _, a := range span.Attributes() {
		b.WriteString(" " + string(a.Key) + "=" + a.Value.Emit())
	}
	for _, e := range span.Events() {
		b.WriteString(" " + e.Name)
		for _, a := range e.Attributes {
			b.WriteString(" " + string(a.Key) + "=" + a.Value.Emit())
		}
	}
	return b.String()
}

func TestVerifySecrets(t *testing.T) {
	t.Run("ST-SUP-RP13-LC12-T01 No error or telemetry holds a secret value", func(t *testing.T) {
		const (
			secretKID     = "kid-s3cr3t-4711"
			secretSubject = "subject-s3cr3t-4711"
			secretTenant  = "tenant-s3cr3t-4711"
			secretRole    = "role-s3cr3t-4711"
			secretBody    = "body-s3cr3t-4711"
		)
		spans, provider := recorder()
		tracer := provider.Tracer("authn-test")
		server := authntest.NewServer(t)
		key := &authntest.Key{ID: secretKID, Private: authntest.NewKey(t, "x").Private}
		server.Publish(key)
		c := newClock()
		set, err := newKeySet(context.Background(), KeySource{URL: server.URL(), Tracer: tracer},
			keyOptions{now: c.now, roots: rootsOf(server)})
		if err != nil {
			t.Fatal(err)
		}
		v, err := newVerifier(Settings{Issuer: testIssuer, Audience: testAudience, ProblemCode: "SVC", RoleClaim: "realm_access.roles",
			TenantClaim: "tenant", Keys: set}, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		secretPath := filepath.Join(t.TempDir(), "path-s3cr3t-4711.json")
		secrets := []string{secretKID, secretSubject, secretTenant, secretRole, secretBody, secretPath,
			server.URL(), server.Addr(), "authntest failure"}

		var errs []error
		claims := func(change func(map[string]any)) map[string]any {
			c := authntest.Claims(testIssuer, testAudience, secretSubject, time.Now())
			c["realm_access"] = map[string]any{"roles": []string{secretRole}}
			c["tenant"] = secretTenant
			if change != nil {
				change(c)
			}
			return c
		}
		header := authntest.Header(secretKID)
		good := authntest.Mint(t, header, claims(nil), key)
		secrets = append(secrets, good)
		if _, err := v.Verify(context.Background(), good); err != nil {
			t.Fatalf("the control token was refused: %v", err)
		}
		before := len(spans.Ended())
		for i := 0; i < 3; i++ {
			if _, err := v.Verify(context.Background(), good); err != nil {
				t.Fatal(err)
			}
		}
		if n := len(spans.Ended()) - before; n != 0 {
			t.Errorf("a cache hit emitted %d refresh spans, want none", n)
		}
		// Each refusal of Verify, with the secret values in the token.
		for _, change := range []func(map[string]any){
			func(c map[string]any) { c["iss"] = "https://" + secretSubject + ".authn.test" },
			func(c map[string]any) { c["aud"] = secretTenant },
			func(c map[string]any) { c["sub"] = secretSubject + "\x00" },
			func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() },
			func(c map[string]any) { c["realm_access"] = map[string]any{"roles": secretRole} },
			func(c map[string]any) { c["tenant"] = []string{secretTenant} },
		} {
			token := authntest.Mint(t, header, claims(change), key)
			secrets = append(secrets, token)
			_, err := v.Verify(context.Background(), token)
			errs = append(errs, err)
		}
		other := authntest.NewKey(t, secretKID)
		forged := authntest.Mint(t, header, claims(nil), other)
		secrets = append(secrets, forged)
		_, err = v.Verify(context.Background(), forged)
		errs = append(errs, err)
		// The first unknown key ID after the cooldown starts a refresh that fails with the server body; the next
		// one, in the cooldown, emits no refresh span.
		c.add(Cooldown + time.Second)
		server.Fail(http.StatusInternalServerError)
		unknown := authntest.Mint(t, authntest.Header(secretKID+"-2"), claims(nil), key)
		_, err = v.Verify(context.Background(), unknown)
		errs = append(errs, err)
		before = len(spans.Ended())
		_, err = v.Verify(context.Background(), unknown)
		errs = append(errs, err)
		if !errors.Is(err, errUnknownKey) {
			t.Errorf("the cooldown refusal gave %v", err)
		}
		if n := len(spans.Ended()) - before; n != 0 {
			t.Errorf("a cooldown refusal emitted %d refresh spans, want none", n)
		}
		// The key source refusals: the server failure, a bad document, a redirect, a missing file and a file
		// that is a folder.
		for _, prepare := range []func(){
			func() { server.Fail(http.StatusBadGateway) },
			func() {
				server.SetDocument([]byte(`{"keys":[{"kid":"` + secretKID + `","kid":"` + secretBody + `"}]}`))
			},
			func() { server.SetDocument([]byte(secretBody)) },
			func() { server.Redirect("https://" + secretBody + ".authn.test/") },
		} {
			prepare()
			_, err := NewKeySet(context.Background(), KeySource{URL: server.URL() + "?q=" + secretBody,
				CAFile: server.CAFile(t), Tracer: tracer})
			errs = append(errs, err)
		}
		_, err = NewKeySet(context.Background(), KeySource{File: secretPath, Tracer: tracer})
		errs = append(errs, err)
		_, err = NewKeySet(context.Background(), KeySource{File: filepath.Dir(secretPath), Tracer: tracer})
		errs = append(errs, err)
		if err := os.WriteFile(secretPath, []byte(`{"keys":[`+secretBody+`]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err = NewKeySet(context.Background(), KeySource{File: secretPath, Tracer: tracer})
		errs = append(errs, err)
		_, err = NewKeySet(context.Background(), KeySource{URL: "http://" + secretBody + ".authn.test/keys"})
		errs = append(errs, err)
		_, err = ParseKeys([]byte(`{"keys":[{"kid":"` + secretKID + `","kty":"RSA","n":"AQAB","e":"AQAB"}]}`))
		errs = append(errs, err)
		// A stopped verifier emits no refresh span.
		v.Stop()
		before = len(spans.Ended())
		_, err = v.Verify(context.Background(), unknown)
		errs = append(errs, err)
		if !errors.Is(err, errStopped) {
			t.Errorf("the stopped verifier gave %v", err)
		}
		if n := len(spans.Ended()) - before; n != 0 {
			t.Errorf("a stopped verifier emitted %d refresh spans, want none", n)
		}

		for i, err := range errs {
			if err == nil {
				t.Errorf("error path %d gave no error", i)
				continue
			}
			for _, secret := range secrets {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error path %d holds a secret value: %q", i, err)
				}
			}
		}
		ended := spans.Ended()
		if len(ended) < 8 {
			t.Fatalf("recorded %d refresh spans, want one for each load", len(ended))
		}
		outcomes := map[string]bool{}
		for _, span := range ended {
			text := spanText(span)
			for _, secret := range secrets {
				if strings.Contains(text, secret) {
					t.Errorf("the span %q holds a secret value", text)
				}
			}
			for _, a := range span.Attributes() {
				if a.Key == attribute.Key("authn.keys.outcome") {
					outcomes[a.Value.AsString()] = true
				}
			}
		}
		if !outcomes["success"] || !outcomes["failure"] {
			t.Errorf("the spans hold the outcomes %v, want success and failure", outcomes)
		}
	})
}

func TestVerifyConcurrent(t *testing.T) {
	t.Run("ST-SUP-RP13-LC15-T01 Concurrent verification gives one identity and one refresh", func(t *testing.T) {
		server := authntest.NewServer(t)
		k1, k2 := authntest.NewKey(t, "k1"), authntest.NewKey(t, "k2")
		server.Publish(k1)
		c := newClock()
		set, err := newKeySet(context.Background(), KeySource{URL: server.URL()},
			keyOptions{now: c.now, roots: rootsOf(server)})
		if err != nil {
			t.Fatal(err)
		}
		v, err := newVerifier(Settings{Issuer: testIssuer, Audience: testAudience, ProblemCode: "SVC", RoleClaim: "realm_access.roles",
			TenantClaim: "tenant", Keys: set}, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(v.Stop)
		server.Publish(k1, k2)
		c.add(Cooldown + time.Second)
		token := authntest.Mint(t, authntest.Header("k2"), goodClaims(), k2)
		before := server.Requests()
		const callers = 64
		ids := make([]Identity, callers)
		errs := make([]error, callers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				ids[i], errs[i] = v.Verify(context.Background(), token)
			}()
		}
		close(start)
		wg.Wait()
		if n := server.Requests() - before; n != 1 {
			t.Errorf("the key server got %d requests, want 1", n)
		}
		for i := range callers {
			if errs[i] != nil {
				t.Fatalf("caller %d was refused: %v", i, errs[i])
			}
			a, b := ids[0], ids[i]
			if a.issuer != b.issuer || a.subject != b.subject || !slices.Equal(a.roles, b.roles) ||
				!sameTenant(a, b) || !a.expires.Equal(b.expires) {
				t.Errorf("caller %d got %+v, caller 0 got %+v", i, b, a)
			}
		}
		if tenantID, _ := ids[0].TenantClaim(); ids[0].subject != "user-1" || tenantID != "t1" {
			t.Errorf("the identity is %+v", ids[0])
		}
	})
}

func sameTenant(a, b Identity) bool {
	at, aok := a.TenantClaim()
	bt, bok := b.TenantClaim()
	return at == bt && aok == bok
}

func TestVerifyExpiry(t *testing.T) {
	t.Run("ST-SUP-RP13-LC05-T03 A token is refused at exactly exp, and every aud member must be a string", func(t *testing.T) {
		key := authntest.NewKey(t, "k1")
		v := staticVerifier(t, func(s *Settings) { s.Skew, s.TenantClaim = 0, "" }, key)
		at := time.Unix(time.Now().Unix(), 0)
		claims := authntest.Claims(testIssuer, testAudience, "user-1", at.Add(-time.Minute))
		claims["exp"] = at.Unix()
		token := authntest.Mint(t, authntest.Header("k1"), claims, key)
		v.now = func() time.Time { return at.Add(-time.Second) }
		expectAccepted(t, v, token)
		v.now = func() time.Time { return at }
		id, err := v.Verify(context.Background(), token)
		expectRefused(t, id, err, errExpiry)

		v.now = time.Now
		for name, aud := range map[string]any{
			"an array with a number": []any{testAudience, 7},
			"an array with null":     []any{testAudience, nil},
			"an object":              map[string]any{"aud": testAudience},
		} {
			t.Run(name, func(t *testing.T) {
				c := authntest.Claims(testIssuer, testAudience, "user-1", time.Now())
				c["aud"] = aud
				id, err := v.Verify(context.Background(), authntest.Mint(t, authntest.Header("k1"), c, key))
				expectRefused(t, id, err, errAudience)
			})
		}
		c := authntest.Claims(testIssuer, testAudience, "user-1", time.Now())
		c["aud"] = []string{"billing-api", testAudience}
		expectAccepted(t, v, authntest.Mint(t, authntest.Header("k1"), c, key))
	})
}

func TestVerifyCharacters(t *testing.T) {
	t.Run("ST-SUP-RP13-LC06-T02 A raw token with a character outside base64url is refused", func(t *testing.T) {
		key := authntest.NewKey(t, "k1")
		v := staticVerifier(t, nil, key)
		token := authntest.Mint(t, authntest.Header("k1"), goodClaims(), key)
		expectAccepted(t, v, token)
		// A stopped key set answers every lookup with errStopped, so errCharacter shows that the check comes
		// before any key lookup.
		stopped := staticVerifier(t, nil, key)
		stopped.Stop()
		cut := strings.LastIndex(token, ".")
		for name, insert := range map[string]string{"padding": "=", "space": " ", "plus": "+", "newline": "\n"} {
			t.Run(name, func(t *testing.T) {
				bad := token[:cut] + insert + token[cut:]
				id, err := stopped.Verify(context.Background(), bad)
				expectRefused(t, id, err, errCharacter)
				id, err = v.Verify(context.Background(), bad)
				expectRefused(t, id, err, errCharacter)
			})
		}
	})
}

func TestVerifyES256(t *testing.T) {
	t.Run("ST-SUP-RP13-LC22-T01 An ES256 token is verified with a P-256 key of 32-byte coordinates on the curve", func(t *testing.T) {
		ec := authntest.NewECKey(t, "e1")
		set, err := ParseKeys(authntest.Document(ec.JWK()))
		if err != nil {
			t.Fatal(err)
		}
		v, err := New(Settings{Issuer: testIssuer, Audience: testAudience, ProblemCode: "SVC", Keys: set,
			Algorithms: []string{AlgorithmES256}})
		if err != nil {
			t.Fatal(err)
		}
		claims := goodClaims()
		good := authntest.Mint(t, authntest.HeaderOf("ES256", "e1"), claims, ec)
		expectAccepted(t, v, good)

		// The signature is the 64 bytes of r and s; a DER signature, a short one and one of another key are refused.
		head, body, _ := strings.Cut(good, ".")
		body, _, _ = strings.Cut(body, ".")
		input := head + "." + body
		digest := sha256.Sum256([]byte(input))
		der, err := ecdsa.SignASN1(rand.Reader, ec.Private, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := base64.RawURLEncoding.DecodeString(good[len(input)+1:])
		other := authntest.NewECKey(t, "e1")
		for name, token := range map[string]string{
			"DER signature":     input + "." + base64.RawURLEncoding.EncodeToString(der),
			"63-byte signature": input + "." + base64.RawURLEncoding.EncodeToString(raw[:63]),
			"another key":       authntest.Mint(t, authntest.HeaderOf("ES256", "e1"), claims, other),
		} {
			t.Run(name, func(t *testing.T) {
				id, err := v.Verify(context.Background(), token)
				expectRefused(t, id, err, errSignature)
			})
		}

		// The key document: a point off the curve, a short coordinate and a coordinate that is not base64url are
		// refused; a key of P-384 and a key whose alg is RS256 are skipped.
		jwk := func(change func(map[string]any)) map[string]any {
			k := ec.JWK()
			change(k)
			return k
		}
		y, _ := base64.RawURLEncoding.DecodeString(ec.JWK()["y"].(string))
		y[31] ^= 1
		for name, key := range map[string]map[string]any{
			"a point off the curve": jwk(func(k map[string]any) { k["y"] = base64.RawURLEncoding.EncodeToString(y) }),
			"a 31-byte x":           jwk(func(k map[string]any) { k["x"] = base64.RawURLEncoding.EncodeToString(y[:31]) }),
			"an x with padding":     jwk(func(k map[string]any) { k["x"] = base64.URLEncoding.EncodeToString(y) }),
		} {
			t.Run(name, func(t *testing.T) {
				if _, err := ParseKeys(authntest.Document(key)); !errors.Is(err, errECKey) {
					t.Fatalf("the key document gave %v, want the refusal %q", err, errECKey)
				}
			})
		}
		for name, key := range map[string]map[string]any{
			"P-384":     jwk(func(k map[string]any) { k["crv"] = "P-384" }),
			"alg RS256": jwk(func(k map[string]any) { k["alg"] = "RS256" }),
			"use enc":   jwk(func(k map[string]any) { k["use"] = "enc" }),
		} {
			t.Run(name, func(t *testing.T) {
				if _, err := ParseKeys(authntest.Document(key)); !errors.Is(err, errNoKey) {
					t.Fatalf("the key document gave %v, want the key skipped and the refusal %q", err, errNoKey)
				}
			})
		}
	})
}
