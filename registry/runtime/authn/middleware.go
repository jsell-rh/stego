package authn

import (
	"context"
	"net/http"
	"strings"
)

type contextKey struct{}

// FromContext gives the verified Identity that Middleware put in the context, and false when the context holds
// none. It makes no Identity: only Middleware, after Verify passes, puts one in a context, under a key that no
// other package can name.
func FromContext(ctx context.Context) (Identity, bool) {
	if ctx == nil {
		return Identity{}, false
	}
	id, ok := ctx.Value(contextKey{}).(Identity)
	return id, ok
}

// Middleware verifies the one Bearer credential of the configured header and serves the request with the verified
// Identity in its context. It answers every other request with 401, Cache-Control no-store, a WWW-Authenticate
// header and a fixed problem document that holds no token (ST-SUP-RP13-LC14). A nil or zero Verifier denies every
// request.
func (v *Verifier) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v == nil || next == nil {
			deny(w, challengeNone)
			return
		}
		raw, challenge := bearer(r.Header.Values(v.header))
		if challenge != "" {
			deny(w, challenge)
			return
		}
		id, err := v.Verify(r.Context(), raw)
		if err != nil {
			deny(w, challengeToken)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, id)))
	})
}

// The WWW-Authenticate challenges of RFC 6750: no Bearer credential, a malformed request, a refused token.
const (
	challengeNone    = `Bearer`
	challengeRequest = `Bearer error="invalid_request"`
	challengeToken   = `Bearer error="invalid_token"`
)

// bearer gives the credential of the one header value "Bearer <token>". For no value or another scheme it gives
// the plain challenge, and for more than one value or a value that is not exactly one Bearer credential it gives
// the invalid_request challenge.
func bearer(values []string) (string, string) {
	if len(values) == 0 {
		return "", challengeNone
	}
	if len(values) != 1 || len(values[0]) > MaxTokenBytes+len("Bearer ") {
		return "", challengeRequest
	}
	scheme, credential, _ := strings.Cut(values[0], " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return "", challengeNone
	}
	if credential == "" {
		return "", challengeRequest
	}
	for i := 0; i < len(credential); i++ {
		c := credential[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return "", challengeRequest
		}
	}
	return credential, ""
}

// deny writes the 401 answer with the challenge and a fixed problem document.
func deny(w http.ResponseWriter, challenge string) {
	header := w.Header()
	header.Set("WWW-Authenticate", challenge)
	header.Set("Cache-Control", "no-store")
	header.Set("Content-Type", "application/problem+json")
	header.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"type":"about:blank","title":"Unauthorized","status":401}` + "\n"))
}
