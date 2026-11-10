package authn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jsell-rh/stego/registry/runtime/authn/authntest"
)

func TestMiddleware(t *testing.T) {
	t.Run("ST-SUP-RP13-LC14-T01 The middleware takes exactly one Bearer credential and denies safely", func(t *testing.T) {
		key := authntest.NewKey(t, "k1")
		v := staticVerifier(t, nil, key)
		good := authntest.Mint(t, authntest.Header("k1"), goodClaims(), key)
		bad := authntest.Mint(t, authntest.Header("k1"), goodClaims(), authntest.NewKey(t, "k1"))
		var reached []Identity
		handler := v.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := FromContext(r.Context())
			if !ok {
				t.Error("the handler got no Identity in its context")
			}
			reached = append(reached, id)
			w.WriteHeader(http.StatusNoContent)
		}))
		serve := func(header http.Header) *http.Response {
			request := httptest.NewRequest(http.MethodGet, "/orders", nil)
			request.Header = header
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			return recorder.Result()
		}

		response := serve(http.Header{"Authorization": {"Bearer " + good}})
		if response.StatusCode != http.StatusNoContent || len(reached) != 1 {
			t.Fatalf("the good request got %d and reached the handler %d times", response.StatusCode, len(reached))
		}
		if reached[0].UserID() != "user-1" || reached[0].Issuer() != testIssuer {
			t.Errorf("the handler got the identity %+v", reached[0])
		}

		const (
			plain   = `Bearer`
			request = `Bearer error="invalid_request"`
			refused = `Bearer error="invalid_token"`
		)
		cases := []struct {
			name      string
			header    http.Header
			challenge string
		}{
			{"no header", http.Header{}, plain},
			{"two Authorization headers", http.Header{"Authorization": {"Bearer " + good, "Bearer " + good}}, request},
			{"a Basic credential", http.Header{"Authorization": {"Basic dXNlcjpwYXNz"}}, plain},
			{"two credentials in one header", http.Header{"Authorization": {"Bearer " + good + ", Bearer " + good}}, request},
			{"two tokens after one scheme", http.Header{"Authorization": {"Bearer " + good + " " + good}}, request},
			{"a bad token", http.Header{"Authorization": {"Bearer " + bad}}, refused},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				reached = nil
				response := serve(c.header)
				body, _ := io.ReadAll(response.Body)
				if response.StatusCode != http.StatusUnauthorized {
					t.Errorf("the status is %d, want 401", response.StatusCode)
				}
				if got := response.Header.Get("Cache-Control"); got != "no-store" {
					t.Errorf("Cache-Control is %q, want no-store", got)
				}
				if got := response.Header.Get("WWW-Authenticate"); got != c.challenge {
					t.Errorf("WWW-Authenticate is %q, want %q", got, c.challenge)
				}
				for _, token := range []string{good, bad, "dXNlcjpwYXNz"} {
					if strings.Contains(string(body), token) || strings.Contains(strings.Join(headerValues(response.Header), " "), token) {
						t.Error("the answer holds the token")
					}
				}
				if len(reached) != 0 {
					t.Error("the refused request reached the handler")
				}
			})
		}

		// A configured header is read in place of Authorization, still with exactly one Bearer credential.
		custom := staticVerifier(t, func(s *Settings) { s.Header = "X-Api-Token" }, key)
		handler = custom.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, _ := FromContext(r.Context())
			reached = append(reached, id)
		}))
		reached = nil
		if response := serve(http.Header{"X-Api-Token": {"Bearer " + good}}); response.StatusCode != http.StatusOK || len(reached) != 1 {
			t.Errorf("the configured header got %d", response.StatusCode)
		}
		for _, header := range []http.Header{{"X-Api-Token": {good}}, {"Authorization": {"Bearer " + good}}} {
			if response := serve(header); response.StatusCode != http.StatusUnauthorized {
				t.Errorf("the header %v got %d, want 401", header, response.StatusCode)
			}
		}
	})
}

func headerValues(header http.Header) []string {
	var out []string
	for _, values := range header {
		out = append(out, values...)
	}
	return out
}

// serveWith sends one request with the header through the middleware of the verifier and gives the answer and the
// Identity that reached the handler.
func serveWith(v *Verifier, path string, header http.Header) (*http.Response, []byte, *Identity) {
	var reached *Identity
	handler := v.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := FromContext(r.Context())
		reached = &id
	}))
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header = header
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	response := recorder.Result()
	body, _ := io.ReadAll(response.Body)
	return response, body, reached
}

func TestMiddlewareBare(t *testing.T) {
	t.Run("ST-SUP-RP13-LC14-T02 A custom header takes a bare token only when the declaration opts it in", func(t *testing.T) {
		key := authntest.NewKey(t, "k1")
		const name = "X-Forwarded-Access-Token"
		good := authntest.Mint(t, authntest.Header("k1"), goodClaims(), key)
		strict := staticVerifier(t, func(s *Settings) { s.Header = name }, key)
		opted := staticVerifier(t, func(s *Settings) { s.Header, s.BareToken = name, true }, key)
		for _, c := range []struct {
			name   string
			v      *Verifier
			value  string
			reach  bool
			status int
		}{
			{"bare token, no opt-in", strict, good, false, http.StatusUnauthorized},
			{"Bearer token, no opt-in", strict, "Bearer " + good, true, http.StatusOK},
			{"bare token, opt-in", opted, good, true, http.StatusOK},
			{"Bearer token, opt-in", opted, "Bearer " + good, true, http.StatusOK},
			{"two bare tokens, opt-in", opted, good + " " + good, false, http.StatusUnauthorized},
		} {
			t.Run(c.name, func(t *testing.T) {
				response, _, reached := serveWith(c.v, "/orders", http.Header{name: {c.value}})
				if response.StatusCode != c.status || (reached != nil) != c.reach {
					t.Errorf("the answer is %d and the handler reached is %v, want %d and %v",
						response.StatusCode, reached != nil, c.status, c.reach)
				}
				if reached != nil && reached.UserID() != "user-1" {
					t.Errorf("the handler got the user %q", reached.UserID())
				}
			})
		}
		set, err := ParseKeys(authntest.Document(key.JWK()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := New(Settings{Issuer: testIssuer, Audience: testAudience, ProblemCode: "SVC", BareToken: true, Keys: set}); !errors.Is(err, ErrSettings) {
			t.Errorf("a bare-token opt-in for Authorization gave %v, want refused settings", err)
		}
	})
}

func TestMiddlewareProblem(t *testing.T) {
	t.Run("ST-SUP-RP13-LC17-T01 The 401 body follows the declared problem type and code", func(t *testing.T) {
		key := authntest.NewKey(t, "k1")
		claims := goodClaims()
		claims["iat"], claims["exp"] = time.Now().Add(-time.Hour).Unix(), time.Now().Add(-30*time.Minute).Unix()
		expired := authntest.Mint(t, authntest.Header("k1"), claims, key)
		read := func(t *testing.T, v *Verifier, header http.Header) map[string]any {
			t.Helper()
			response, body, reached := serveWith(v, "/orders/7", header)
			if response.StatusCode != http.StatusUnauthorized || reached != nil {
				t.Fatalf("the answer is %d", response.StatusCode)
			}
			if got := response.Header.Get("Content-Type"); got != "application/problem+json" {
				t.Errorf("the content type is %q", got)
			}
			if strings.Contains(string(body), expired) || strings.Contains(string(body), "user-1") || strings.Contains(string(body), "k1") {
				t.Errorf("the body holds token, claim or key data: %s", body)
			}
			var document map[string]any
			if err := json.Unmarshal(body, &document); err != nil {
				t.Fatalf("the body is not JSON: %v", err)
			}
			if _, err := time.Parse(time.RFC3339, fmt.Sprint(document["timestamp"])); err != nil {
				t.Errorf("the timestamp %v is not RFC 3339", document["timestamp"])
			}
			delete(document, "timestamp")
			return document
		}
		declared := staticVerifier(t, func(s *Settings) { s.ProblemType = "https://api.example.com/errors/" }, key)
		missing := read(t, declared, http.Header{})
		bad := read(t, declared, http.Header{"Authorization": {"Bearer " + expired}})
		want := map[string]any{"type": "https://api.example.com/errors/unauthorized", "title": "Unauthorized",
			"status": float64(401), "detail": "The request is not authenticated.", "code": "SVC-AUT-001", "instance": "/orders/7"}
		for name, document := range map[string]map[string]any{"no token": missing, "an expired token": bad} {
			if !maps.Equal(document, want) {
				t.Errorf("the body for %s is %v, want %v", name, document, want)
			}
		}
		// A base with no trailing slash is joined with one.
		noSlash := read(t, staticVerifier(t, func(s *Settings) { s.ProblemType = "https://api.example.com/errors" }, key), http.Header{})
		if noSlash["type"] != "https://api.example.com/errors/unauthorized" {
			t.Errorf("with a base of no trailing slash the type is %v", noSlash["type"])
		}
		blank := read(t, staticVerifier(t, nil, key), http.Header{})
		if blank["type"] != "about:blank" || blank["code"] != "SVC-AUT-001" {
			t.Errorf("with no type base the body is %v", blank)
		}
		set, err := ParseKeys(authntest.Document(key.JWK()))
		if err != nil {
			t.Fatal(err)
		}
		for name, s := range map[string]Settings{
			"no code prefix":      {ProblemCode: ""},
			"a lower-case prefix": {ProblemCode: "svc"},
			"a relative type":     {ProblemCode: "SVC", ProblemType: "/errors/"},
		} {
			s.Issuer, s.Audience, s.Keys = testIssuer, testAudience, set
			if _, err := New(s); !errors.Is(err, ErrSettings) {
				t.Errorf("%s gave %v, want refused settings", name, err)
			}
		}
	})
}

func TestAuthenticate(t *testing.T) {
	t.Run("ST-SUP-RP13-LC20-T01 Authenticate gives the same Identity as the middleware", func(t *testing.T) {
		key := authntest.NewKey(t, "k1")
		good := authntest.Mint(t, authntest.Header("k1"), goodClaims(), key)
		bad := authntest.Mint(t, authntest.Header("k1"), goodClaims(), authntest.NewKey(t, "k1"))
		v := staticVerifier(t, nil, key)
		opted := staticVerifier(t, func(s *Settings) { s.Header, s.BareToken = "X-Forwarded-Access-Token", true }, key)

		_, _, viaMiddleware := serveWith(v, "/orders", http.Header{"Authorization": {"Bearer " + good}})
		ctx, err := v.Authenticate(context.Background(), "Bearer "+good)
		if err != nil || viaMiddleware == nil {
			t.Fatalf("Authenticate gave %v, the middleware reached %v", err, viaMiddleware)
		}
		id, ok := FromContext(ctx)
		if !ok || id.Principal() != viaMiddleware.Principal() || !slices.Equal(id.Roles(), viaMiddleware.Roles()) ||
			!sameTenant(id, *viaMiddleware) || !id.ExpiresAt().Equal(viaMiddleware.ExpiresAt()) {
			t.Errorf("Authenticate gave %+v, the middleware %+v", id, *viaMiddleware)
		}
		if ctx, err := opted.Authenticate(context.Background(), good); err != nil {
			t.Errorf("a bare credential with the opt-in was refused: %v", err)
		} else if id, _ := FromContext(ctx); id.UserID() != "user-1" {
			t.Errorf("a bare credential with the opt-in gave %+v", id)
		}
		for _, c := range []struct {
			name       string
			v          *Verifier
			credential string
			header     http.Header
			want       error
		}{
			{"a bare credential, no opt-in", v, good, http.Header{"Authorization": {good}}, errCredential},
			{"a bad token", v, "Bearer " + bad, http.Header{"Authorization": {"Bearer " + bad}}, errSignature},
			{"a Basic credential", v, "Basic dXNlcjpwYXNz", http.Header{"Authorization": {"Basic dXNlcjpwYXNz"}}, errCredential},
		} {
			t.Run(c.name, func(t *testing.T) {
				ctx, err := c.v.Authenticate(context.Background(), c.credential)
				if ctx != nil || !errors.Is(err, c.want) || !errors.Is(err, ErrDenied) {
					t.Errorf("Authenticate gave %v, %v, want %q", ctx, err, c.want)
				}
				if response, _, reached := serveWith(c.v, "/orders", c.header); response.StatusCode != http.StatusUnauthorized || reached != nil {
					t.Errorf("the middleware gave %d for the same credential", response.StatusCode)
				}
			})
		}
	})
}
