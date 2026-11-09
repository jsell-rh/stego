package authn

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type contextKey struct{}

// FromContext gives the verified Identity that Middleware or Authenticate put in the context, and false when the
// context holds none. It makes no Identity: only Middleware and Authenticate, after Verify passes, put one in a
// context, under a key that no other package can name.
func FromContext(ctx context.Context) (Identity, bool) {
	if ctx == nil {
		return Identity{}, false
	}
	id, ok := ctx.Value(contextKey{}).(Identity)
	return id, ok
}

// Authenticate verifies the credential that a transport other than HTTP read, such as a gRPC metadata value, with
// the credential rules of Middleware and Verify, and gives a context that holds the verified Identity
// (ST-SUP-RP13-LC20). The credential is "Bearer <token>", or the bare token when the settings opt the header in.
func (v *Verifier) Authenticate(ctx context.Context, credential string) (context.Context, error) {
	if v == nil || v.keys == nil {
		return nil, errNotReady
	}
	if ctx == nil {
		return nil, errContext
	}
	raw, challenge := v.token(credential)
	if challenge != "" {
		return nil, errCredential
	}
	id, err := v.Verify(ctx, raw)
	if err != nil {
		return nil, err
	}
	return context.WithValue(ctx, contextKey{}, id), nil
}

// Middleware verifies the one credential of the configured header and serves the request with the verified Identity
// in its context. It answers every other request with 401, Cache-Control no-store, a WWW-Authenticate header and the
// problem document of ST-SUP-RP13-LC17, which holds no token (ST-SUP-RP13-LC14). A nil or zero Verifier denies
// every request.
func (v *Verifier) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v == nil || v.keys == nil || next == nil {
			v.deny(w, r, challengeNone)
			return
		}
		values := r.Header.Values(v.header)
		if len(values) == 0 {
			v.deny(w, r, challengeNone)
			return
		}
		if len(values) != 1 {
			v.deny(w, r, challengeRequest)
			return
		}
		raw, challenge := v.token(values[0])
		if challenge != "" {
			v.deny(w, r, challenge)
			return
		}
		id, err := v.Verify(r.Context(), raw)
		if err != nil {
			v.deny(w, r, challengeToken)
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

// token gives the token of one credential value: "Bearer <token>", or a bare token when the header is opted in. For
// no value or another scheme it gives the plain challenge, and for a value that is not exactly one credential the
// invalid_request challenge.
func (v *Verifier) token(value string) (string, string) {
	if value == "" {
		return "", challengeNone
	}
	if len(value) > MaxTokenBytes+len("Bearer ") {
		return "", challengeRequest
	}
	credential := value
	if scheme, rest, found := strings.Cut(value, " "); found {
		if !strings.EqualFold(scheme, "Bearer") {
			return "", challengeNone
		}
		credential = rest
	} else if !v.bare {
		if strings.EqualFold(value, "Bearer") {
			return "", challengeRequest
		}
		return "", challengeNone
	}
	if credential == "" || !tokenText(credential) {
		return "", challengeRequest
	}
	return credential, ""
}

// problem is the problem document of a refusal (ST-SUP-RC20-OC01, OC03, OC04).
type problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail"`
	Code      string `json:"code"`
	Instance  string `json:"instance"`
	Timestamp string `json:"timestamp"`
}

// deny writes the 401 answer with the challenge and the problem document of the settings. The document holds the
// request path and the time, and no token, claim or key data.
func (v *Verifier) deny(w http.ResponseWriter, r *http.Request, challenge string) {
	body := problem{Type: "about:blank", Title: "Unauthorized", Status: http.StatusUnauthorized,
		Detail: "The request is not authenticated.", Code: "AUT-001", Instance: r.URL.Path,
		Timestamp: time.Now().UTC().Format(time.RFC3339)}
	if v != nil && v.code != "" {
		body.Type, body.Code = v.problem, v.code
	}
	data, err := json.Marshal(body)
	if err != nil {
		data = []byte(`{"type":"about:blank","title":"Unauthorized","status":401}`)
	}
	header := w.Header()
	header.Set("WWW-Authenticate", challenge)
	header.Set("Cache-Control", "no-store")
	header.Set("Content-Type", "application/problem+json")
	header.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write(append(data, '\n'))
}
