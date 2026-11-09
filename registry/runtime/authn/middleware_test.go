package authn

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"stego.dev/runtime/authn/authntest"
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
