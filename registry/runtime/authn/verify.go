package authn

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jsell-rh/stego/registry/runtime/internal/seal"
)

// The bounds of a token (ST-SUP-RP13-LC05, ST-SUP-RP13-LC06).
const (
	// MaxTokenBytes is the largest token that Verify reads.
	MaxTokenBytes = 16 << 10
	// MaxDepth is the most levels of nesting of the header or the payload; the top object is level 1.
	MaxDepth = 32
	// MaxSkew is the largest clock skew that New accepts.
	MaxSkew = 60 * time.Second
	// AlgorithmRS256 and AlgorithmES256 are the algorithms of the fixed allow list (ST-SUP-RP13-LC03).
	AlgorithmRS256 = "RS256"
	AlgorithmES256 = "ES256"
	// Algorithm is the algorithm that a verifier accepts when its settings name none.
	Algorithm = AlgorithmRS256
)

// allowList is the fixed allow list of the algorithms (ST-SUP-RP13-LC03): no none, no HMAC and no other algorithm.
var allowList = []string{AlgorithmRS256, AlgorithmES256}

// subjectClaims is the allow list of the subject claim (ST-SUP-RP13-LC13): sub, and oid, the immutable object ID
// of Microsoft Entra ID. A claim that the user or the provider can change, such as email, preferred_username or
// upn, could map two accounts to one subject, so it is refused (security review of PR #202).
var subjectClaims = []string{"sub", "oid"}

// tokenTypes are the typ header values of an access token that Verify accepts: JWT, and at+jwt in its short and its
// media type form (RFC 9068, ST-SUP-RP13-LC03).
var tokenTypes = []string{"JWT", "at+jwt", "application/at+jwt"}

// idTokenClaims are the claims that mark an ID token, which is not an access token, so Verify refuses a token that
// holds one (ST-SUP-RP13-LC03).
var idTokenClaims = []string{"at_hash", "c_hash", "nonce"}

// ErrDenied is the error of each refused token: errors.Is(err, ErrDenied) holds for every refusal of Verify and of
// the key set. Its text and the text of each refusal are fixed and hold no token, claim, key ID, key document,
// source URL, file path or provider text (ST-SUP-RP13-LC12).
var ErrDenied = errors.New("authn: token refused")

// ErrSettings is the error of settings that New or NewKeySet refuses.
var ErrSettings = errors.New("authn: settings refused")

// refusal is one fixed reason of a refused token.
type refusal string

func (r refusal) Error() string        { return "authn: token refused: " + string(r) }
func (r refusal) Is(target error) bool { return target == ErrDenied }

// setting is one fixed reason of refused settings.
type setting string

func (s setting) Error() string        { return "authn: settings refused: " + string(s) }
func (s setting) Is(target error) bool { return target == ErrSettings }

const (
	errNotReady   = refusal("the verifier is not ready")
	errStopped    = refusal("the verifier is stopped")
	errSize       = refusal("size")
	errCharacter  = refusal("character")
	errProfile    = refusal("profile claim")
	errCredential = refusal("credential")
	errFormat     = refusal("format")
	errEncoding   = refusal("encoding")
	errJSON       = refusal("JSON")
	errDuplicate  = refusal("duplicate member")
	errDepth      = refusal("nesting")
	errUnicode    = refusal("Unicode")
	errAlgorithm  = refusal("algorithm")
	errType       = refusal("type")
	errIDToken    = refusal("id token")
	errHeader     = refusal("header member")
	errKeyID      = refusal("key ID")
	errUnknownKey = refusal("unknown key")
	errSignature  = refusal("signature")
	errIssuer     = refusal("issuer")
	errAudience   = refusal("audience")
	errSubject    = refusal("subject")
	errIssuedAt   = refusal("issue time")
	errExpiry     = refusal("expiry")
	errNotBefore  = refusal("not before")
	errRoles      = refusal("role claim")
	errTenant     = refusal("tenant claim")
	errContext    = refusal("context")
)

// Settings are the trust settings of one verifier, which generated code passes from the declaration
// (ST-STD-RUNTIME-R05). Every trust setting is explicit: New takes no default issuer, audience or key set.
type Settings struct {
	// Issuer is the HTTPS URL that the iss claim must equal exactly.
	Issuer string
	// Audience is the value that the aud claim, or one member of it, must equal exactly.
	Audience string
	// Algorithms are the algorithms that the verifier accepts, each from the fixed allow list RS256 and ES256 and
	// each once. Empty: RS256 alone (ST-SUP-RP13-LC03).
	Algorithms []string
	// SubjectClaim is the name of the top-level claim that holds the subject. Empty: sub (ST-SUP-RP13-LC13).
	SubjectClaim string
	// Skew is the clock skew that the expiry, issue time and not-before checks allow, from 0 to MaxSkew.
	Skew time.Duration
	// RoleClaim is the dotted path of the role claim, such as realm_access.roles. Empty: the identity has no roles.
	RoleClaim string
	// TenantClaim is the dotted path of the tenant claim. Empty: the identity has no tenant claim.
	TenantClaim string
	// AttributeClaims are the names of the top-level claims that the Identity carries as attributes, at most
	// MaxAttributes (ST-SUP-RP13-LC18).
	AttributeClaims []string
	// Header is the request header that Middleware reads. Empty: Authorization.
	Header string
	// BareToken opts the configured header in to a bare token with no Bearer scheme, for a proxy such as
	// oauth2-proxy that forwards the token alone (ST-SUP-RP13-LC14). New refuses it for Authorization.
	BareToken bool
	// ProblemType is the declared error type base, such as https://api.example.com/errors/; the problem type of a
	// refusal is the base joined to unauthorized as a URL path, with or without a trailing slash on the base. Empty:
	// the type is about:blank (ST-SUP-RP13-LC17).
	ProblemType string
	// ProblemCode is the declared service prefix of the problem code, such as SVC; the code of a refusal is
	// SVC-AUT-001 (ST-SUP-RP13-LC17).
	ProblemCode string
	// Keys is the key set that holds the verification keys.
	Keys *KeySet
}

// Verifier verifies JWT bearer tokens against one issuer, one audience and one key set. It is safe for concurrent
// use (ST-SUP-RP13-LC15). A zero Verifier denies every token.
type Verifier struct {
	issuer     string
	audience   string
	algorithms []string
	subject    string
	skew       time.Duration
	rolePath   []string
	tenantPath []string
	attributes []string
	header     string
	bare       bool
	problem    string
	code       string
	keys       *KeySet
	now        func() time.Time
}

// New checks the settings and gives a verifier (ST-SUP-RP13-LC05). It refuses an issuer that is not an HTTPS URL, an
// empty audience, a negative skew or one more than MaxSkew, a claim path that is not valid and a nil key set.
func New(s Settings) (*Verifier, error) { return newVerifier(s, time.Now) }

func newVerifier(s Settings, now func() time.Time) (*Verifier, error) {
	issuer, err := url.Parse(s.Issuer)
	if err != nil || issuer.Scheme != "https" || issuer.Host == "" || issuer.User != nil || issuer.Fragment != "" ||
		issuer.RawQuery != "" || issuer.Opaque != "" || !httpsURL(s.Issuer) {
		return nil, setting("the issuer is not an HTTPS URL")
	}
	if s.Audience == "" || !clean(s.Audience) {
		return nil, setting("the audience is empty")
	}
	if s.Skew < 0 || s.Skew > MaxSkew {
		return nil, setting("the clock skew is out of range")
	}
	algorithms := slices.Clone(s.Algorithms)
	if len(algorithms) == 0 {
		algorithms = []string{Algorithm}
	}
	for i, alg := range algorithms {
		if !slices.Contains(allowList, alg) || slices.Contains(algorithms[:i], alg) {
			return nil, setting("an algorithm is not in the allow list")
		}
	}
	subject := s.SubjectClaim
	if subject == "" {
		subject = "sub"
	}
	if !slices.Contains(subjectClaims, subject) {
		return nil, setting("the subject claim is not sub or oid")
	}
	rolePath, err := claimPath(s.RoleClaim)
	if err != nil {
		return nil, err
	}
	tenantPath, err := claimPath(s.TenantClaim)
	if err != nil {
		return nil, err
	}
	header := s.Header
	if header == "" {
		header = "Authorization"
	}
	if !headerName(header) {
		return nil, setting("the header name is not valid")
	}
	header = http.CanonicalHeaderKey(header)
	if s.BareToken && header == "Authorization" {
		return nil, setting("a bare token is only for a custom header")
	}
	if len(s.AttributeClaims) > MaxAttributes {
		return nil, setting("too many attribute claims")
	}
	for _, name := range s.AttributeClaims {
		if path, err := claimPath(name); err != nil || len(path) != 1 {
			return nil, setting("an attribute claim name is not valid")
		}
	}
	problem := "about:blank"
	if s.ProblemType != "" {
		base, err := url.Parse(s.ProblemType)
		if err != nil || !base.IsAbs() || base.Host == "" || base.User != nil || base.RawQuery != "" ||
			base.Fragment != "" || !clean(s.ProblemType) {
			return nil, setting("the problem type base is not an absolute URL")
		}
		// JoinPath adds the one slash that a base with no trailing slash lacks.
		joined, err := url.JoinPath(s.ProblemType, "unauthorized")
		if err != nil {
			return nil, setting("the problem type base is not an absolute URL")
		}
		problem = joined
	}
	if !problemPrefix(s.ProblemCode) {
		return nil, setting("the problem code prefix is not valid")
	}
	if s.Keys == nil {
		return nil, setting("the key set is missing")
	}
	return &Verifier{issuer: s.Issuer, audience: s.Audience, algorithms: algorithms, subject: subject, skew: s.Skew,
		rolePath: rolePath, tenantPath: tenantPath, attributes: slices.Clone(s.AttributeClaims), header: header,
		bare: s.BareToken, problem: problem, code: s.ProblemCode + "-AUT-001", keys: s.Keys, now: now}, nil
}

// problemPrefix reports whether the prefix is 1 to 32 upper-case letters and digits (ST-SUP-RC20-OC03).
func problemPrefix(prefix string) bool {
	if prefix == "" || len(prefix) > 32 {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		if c := prefix[i]; !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// Stop stops the key set of the verifier: after Stop, Verify denies every token, and a key retrieval that runs is
// cancelled (ST-SUP-RP13-LC11). A repeat Stop is safe.
func (v *Verifier) Stop() {
	if v == nil {
		return
	}
	v.keys.Stop()
}

// Verify verifies the token and gives its identity. It reads no claim before the signature, the issuer, the
// audience, the subject, the issue time and the expiry all pass, and it gives a zero Identity with every error
// (ST-SUP-RP13-LC02). It is the only way to make an Identity that holds a claim (ST-SUP-RP13-LC01).
func (v *Verifier) Verify(ctx context.Context, raw string) (Identity, error) {
	if v == nil || v.keys == nil || v.now == nil {
		return Identity{}, errNotReady
	}
	if ctx == nil || ctx.Err() != nil {
		return Identity{}, errContext
	}
	if len(raw) == 0 || len(raw) > MaxTokenBytes {
		return Identity{}, errSize
	}
	if !tokenText(raw) {
		return Identity{}, errCharacter
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return Identity{}, errFormat
	}
	headerJSON, err := decodeSegment(parts[0])
	if err != nil {
		return Identity{}, err
	}
	payloadJSON, err := decodeSegment(parts[1])
	if err != nil {
		return Identity{}, err
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil {
		return Identity{}, errEncoding
	}
	alg, kid, hasKid, err := checkHeader(headerJSON, v.algorithms)
	if err != nil {
		return Identity{}, err
	}
	key, err := v.keys.lookup(ctx, kid, hasKid)
	if err != nil {
		return Identity{}, err
	}
	// The algorithm of the token is the one algorithm of its key (ST-SUP-RP13-LC03).
	if key.alg != alg {
		return Identity{}, errAlgorithm
	}
	digest := sha256.Sum256([]byte(raw[:len(parts[0])+1+len(parts[1])]))
	if !key.verifies(digest[:], signature) {
		return Identity{}, errSignature
	}
	// The signature holds: the claims may now be read.
	claims, err := members(payloadJSON)
	if err != nil {
		return Identity{}, err
	}
	for _, name := range idTokenClaims {
		if _, present := claims[name]; present {
			return Identity{}, errIDToken
		}
	}
	issuer, subject, expires, err := v.registered(claims)
	if err != nil {
		return Identity{}, err
	}
	roles, err := readRoles(claims, v.rolePath)
	if err != nil {
		return Identity{}, err
	}
	tenant, hasTenant, err := readTenant(claims, v.tenantPath)
	if err != nil {
		return Identity{}, err
	}
	p, err := readProfile(claims, v.attributes)
	if err != nil {
		return Identity{}, err
	}
	return Identity{mark: seal.NewIdentityMark(tenant, hasTenant), issuer: issuer, subject: subject, roles: roles,
		attributes: p.attributes, username: p.username, email: p.email, givenName: p.givenName,
		familyName: p.familyName, expires: expires}, nil
}

// tokenText reports whether the raw token holds only the base64url alphabet and dots (ST-SUP-RP13-LC06).
func tokenText(raw string) bool {
	for i := 0; i < len(raw); i++ {
		if c := raw[i]; !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' ||
			c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// decodeSegment decodes one base64url segment and checks that it is one JSON object that is not ambiguous.
func decodeSegment(segment string) ([]byte, error) {
	data, err := base64.RawURLEncoding.Strict().DecodeString(segment)
	if err != nil {
		return nil, errEncoding
	}
	if err := checkJSON(data); err != nil {
		return nil, err
	}
	return data, nil
}

// checkHeader checks the header and gives its algorithm, its key ID, and whether it names one: the algorithm must be
// one that the verifier accepts (ST-SUP-RP13-LC03).
func checkHeader(data []byte, algorithms []string) (string, string, bool, error) {
	header, err := members(data)
	if err != nil {
		return "", "", false, err
	}
	alg, ok := stringMember(header, "alg")
	if !ok || !slices.Contains(algorithms, alg) {
		return "", "", false, errAlgorithm
	}
	typ, ok := stringMember(header, "typ")
	if !ok || !slices.Contains(tokenTypes, typ) {
		return "", "", false, errType
	}
	for _, name := range []string{"crit", "jku", "jwk", "x5u"} {
		if _, present := header[name]; present {
			return "", "", false, errHeader
		}
	}
	if _, present := header["kid"]; !present {
		return alg, "", false, nil
	}
	kid, ok := stringMember(header, "kid")
	if !ok || kid == "" || len(kid) > maxKeyIDBytes {
		return "", "", false, errKeyID
	}
	return alg, kid, true, nil
}

// verifies reports whether the signature over the SHA-256 digest holds with the key: RSASSA-PKCS1-v1_5 for RS256,
// and for ES256 the 64 bytes of r and s, each of 32 bytes, as RFC 7518 section 3.4 states (ST-SUP-RP13-LC22).
func (k publicKey) verifies(digest, signature []byte) bool {
	switch {
	case k.alg == AlgorithmRS256 && k.rsa != nil:
		return rsa.VerifyPKCS1v15(k.rsa, crypto.SHA256, digest, signature) == nil
	case k.alg == AlgorithmES256 && k.ec != nil:
		if len(signature) != 2*ECCoordinateBytes {
			return false
		}
		r := new(big.Int).SetBytes(signature[:ECCoordinateBytes])
		s := new(big.Int).SetBytes(signature[ECCoordinateBytes:])
		return ecdsa.Verify(k.ec, digest, r, s)
	}
	return false
}

// registered checks the issuer, the audience, the subject, the issue time, the expiry and the not-before time
// (ST-SUP-RP13-LC05).
func (v *Verifier) registered(claims map[string]json.RawMessage) (string, string, time.Time, error) {
	issuer, ok := stringMember(claims, "iss")
	if !ok || issuer != v.issuer {
		return "", "", time.Time{}, errIssuer
	}
	if !v.audienceHolds(claims["aud"]) {
		return "", "", time.Time{}, errAudience
	}
	subject, ok := stringMember(claims, v.subject)
	if !ok || subject == "" || len(subject) > maxClaimBytes || !clean(subject) {
		return "", "", time.Time{}, errSubject
	}
	now := v.now()
	issued, ok := numericDate(claims, "iat")
	if !ok || issued.After(now.Add(v.skew)) {
		return "", "", time.Time{}, errIssuedAt
	}
	expires, ok := numericDate(claims, "exp")
	if !ok || !issued.Before(expires) || !now.Before(expires.Add(v.skew)) {
		return "", "", time.Time{}, errExpiry
	}
	if _, present := claims["nbf"]; present {
		notBefore, ok := numericDate(claims, "nbf")
		if !ok || notBefore.After(now.Add(v.skew)) {
			return "", "", time.Time{}, errNotBefore
		}
	}
	return issuer, subject, expires, nil
}

func (v *Verifier) audienceHolds(raw json.RawMessage) bool {
	if raw == nil {
		return false
	}
	if len(raw) > 0 && raw[0] == '"' {
		var one string
		return json.Unmarshal(raw, &one) == nil && one == v.audience
	}
	var many []json.RawMessage
	if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &many) != nil {
		return false
	}
	found := false
	for _, member := range many {
		var audience string
		if len(member) == 0 || member[0] != '"' || json.Unmarshal(member, &audience) != nil {
			return false
		}
		found = found || audience == v.audience
	}
	return found
}

// numericDate reads a NumericDate claim: a finite JSON number of seconds from 0 to the end of the year 9999.
func numericDate(claims map[string]json.RawMessage, name string) (time.Time, bool) {
	raw, ok := claims[name]
	if !ok || len(raw) == 0 || (raw[0] != '-' && (raw[0] < '0' || raw[0] > '9')) {
		return time.Time{}, false
	}
	seconds, err := strconv.ParseFloat(string(raw), 64)
	if err != nil || math.IsNaN(seconds) || seconds < 0 || seconds > 253402300799 {
		return time.Time{}, false
	}
	whole, fraction := math.Modf(seconds)
	return time.Unix(int64(whole), int64(fraction*1e9)), true
}

// members decodes one JSON object into its members. checkJSON has already refused a duplicate member.
func members(data []byte) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return nil, errJSON
	}
	return object, nil
}

func stringMember(object map[string]json.RawMessage, name string) (string, bool) {
	raw, ok := object[name]
	if !ok || len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

// checkJSON refuses data that is not one JSON object, that holds invalid UTF-8 or an unpaired surrogate escape,
// a duplicate member at any level, more than MaxDepth levels of nesting, or data after the object
// (ST-SUP-RP13-LC06). Two readers of the same token then cannot read different claims.
func checkJSON(data []byte) error {
	if !utf8.Valid(data) || !json.Valid(data) {
		return errJSON
	}
	if !pairedEscapes(data) {
		return errUnicode
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return errJSON
	}
	if err := readValue(decoder, first, 1); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errJSON
	}
	return nil
}

func readValue(decoder *json.Decoder, start json.Token, depth int) error {
	delim, ok := start.(json.Delim)
	if !ok {
		return nil
	}
	if depth > MaxDepth {
		return errDepth
	}
	switch delim {
	case '{':
		names := make(map[string]bool)
		for decoder.More() {
			name, err := decoder.Token()
			if err != nil {
				return errJSON
			}
			key, ok := name.(string)
			if !ok {
				return errJSON
			}
			if names[key] {
				return errDuplicate
			}
			names[key] = true
			value, err := decoder.Token()
			if err != nil {
				return errJSON
			}
			if err := readValue(decoder, value, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			value, err := decoder.Token()
			if err != nil {
				return errJSON
			}
			if err := readValue(decoder, value, depth+1); err != nil {
				return err
			}
		}
	default:
		return errJSON
	}
	if _, err := decoder.Token(); err != nil {
		return errJSON
	}
	return nil
}

// pairedEscapes reports whether each \u escape of a surrogate in valid JSON is one half of a pair. In valid JSON a
// backslash occurs only in a string, so the scan needs no string state.
func pairedEscapes(data []byte) bool {
	for i := 0; i < len(data); i++ {
		if data[i] != '\\' {
			continue
		}
		if i+1 >= len(data) {
			return false
		}
		if data[i+1] != 'u' {
			i++
			continue
		}
		unit, ok := hexUnit(data, i+2)
		if !ok {
			return false
		}
		switch {
		case unit >= 0xDC00 && unit <= 0xDFFF:
			return false
		case unit >= 0xD800 && unit <= 0xDBFF:
			if i+11 >= len(data) || data[i+6] != '\\' || data[i+7] != 'u' {
				return false
			}
			low, ok := hexUnit(data, i+8)
			if !ok || low < 0xDC00 || low > 0xDFFF {
				return false
			}
			i += 11
		default:
			i += 5
		}
	}
	return true
}

func hexUnit(data []byte, at int) (uint64, bool) {
	if at+4 > len(data) {
		return 0, false
	}
	unit, err := strconv.ParseUint(string(data[at:at+4]), 16, 16)
	return unit, err == nil
}

// headerName reports whether the name is an HTTP header field name (an RFC 9110 token).
func headerName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0) {
			return false
		}
	}
	return true
}
