package authn

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jsell-rh/stego/registry/runtime/authn/authntest"
)

// issuerPath is the path of the test issuer on the key server.
const issuerPath = "/realms/shop"

// discoveryVerifier gives a verifier of the issuer of the key server, whose key set comes from the server's
// discovery document, with the clock given, both algorithms of the allow list and the tenant claim org_id.
func discoveryVerifier(t *testing.T, server *authntest.Server, c *clock) (*Verifier, error) {
	t.Helper()
	issuer := server.Issuer(issuerPath)
	set, err := newKeySet(context.Background(), KeySource{Issuer: issuer}, keyOptions{now: c.now, roots: rootsOf(server)})
	if err != nil {
		return nil, err
	}
	v, err := newVerifier(Settings{Issuer: issuer, Audience: testAudience, ProblemCode: "SVC", TenantClaim: "org_id",
		Algorithms: []string{AlgorithmRS256, AlgorithmES256}, Keys: set}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(v.Stop)
	return v, nil
}

// issuerToken mints a token of the server's issuer for the test audience with the tenant org_id t1.
func issuerToken(t *testing.T, server *authntest.Server, alg, kid string, key any) string {
	t.Helper()
	claims := authntest.Claims(server.Issuer(issuerPath), testAudience, "user-1", time.Now())
	claims["org_id"] = "t1"
	return authntest.Mint(t, authntest.HeaderOf(alg, kid), claims, key)
}

func TestDiscovery(t *testing.T) {
	t.Run("ST-SUP-RP13-LC21-T01 An issuer is found through its discovery document, which is read once", func(t *testing.T) {
		server := authntest.NewServer(t)
		rsaKey, ecKey := authntest.NewKey(t, "r1"), authntest.NewECKey(t, "e1")
		server.Publish(rsaKey)
		c := newClock()
		v, err := discoveryVerifier(t, server, c)
		if err != nil {
			t.Fatalf("the key set of the issuer did not start: %v", err)
		}
		if server.Discoveries() != 1 || server.Requests() != 1 {
			t.Fatalf("the start made %d discovery and %d key requests, want 1 and 1", server.Discoveries(), server.Requests())
		}
		expectAccepted(t, v, issuerToken(t, server, AlgorithmRS256, "r1", rsaKey))

		// Key rotation: the provider adds the key e1 and drops r1. After the cooldown, a token of the unknown key ID
		// loads the key document from the jwks_uri of the discovery document, and not the discovery document.
		server.Publish(ecKey)
		c.add(Cooldown + time.Second)
		expectAccepted(t, v, issuerToken(t, server, AlgorithmES256, "e1", ecKey))
		id, err := v.Verify(context.Background(), issuerToken(t, server, AlgorithmRS256, "r1", rsaKey))
		expectRefused(t, id, err, errUnknownKey)
		if server.Discoveries() != 1 || server.Requests() != 2 {
			t.Errorf("after the rotation the server got %d discovery and %d key requests, want 1 and 2", server.Discoveries(), server.Requests())
		}

		// Each document that the key set must refuse at start, and the refusal that names it.
		issuer := server.Issuer(issuerPath)
		refused := []struct {
			name     string
			document string
			status   int
			want     error
		}{
			{"another issuer", `{"issuer":"https://other.authn.test/realms/shop","jwks_uri":"` + server.URL() + `"}`, 0, errDiscoveryIssuer},
			{"the issuer with a trailing slash", `{"issuer":"` + issuer + `/","jwks_uri":"` + server.URL() + `"}`, 0, errDiscoveryIssuer},
			{"no issuer", `{"jwks_uri":"` + server.URL() + `"}`, 0, errDiscoveryIssuer},
			{"an http key set URL", `{"issuer":"` + issuer + `","jwks_uri":"` + strings.Replace(server.URL(), "https:", "http:", 1) + `"}`, 0, errDiscoveryKeys},
			{"a key set URL with a user", `{"issuer":"` + issuer + `","jwks_uri":"https://u@` + server.Addr() + `/keys"}`, 0, errDiscoveryKeys},
			{"no key set URL", `{"issuer":"` + issuer + `"}`, 0, errDiscoveryKeys},
			{"a key set URL in other case", `{"issuer":"` + issuer + `","JWKS_URI":"` + server.URL() + `"}`, 0, errDiscoveryKeys},
			{"a duplicate member", `{"issuer":"` + issuer + `","jwks_uri":"` + server.URL() + `","issuer":"` + issuer + `"}`, 0, errDiscovery},
			{"not JSON", `<html>`, 0, errDiscovery},
			{"oversize", `{"issuer":"` + issuer + `","jwks_uri":"` + server.URL() + `","pad":"` + strings.Repeat("a", MaxDocumentBytes) + `"}`, 0, errDiscovery},
			{"a server failure", "", http.StatusInternalServerError, errDiscovery},
		}
		for _, r := range refused {
			t.Run(r.name, func(t *testing.T) {
				server.SetDiscovery([]byte(r.document))
				if r.status != 0 {
					server.Fail(r.status)
				}
				t.Cleanup(func() { server.SetDiscovery(nil); server.Publish(rsaKey) })
				_, err := discoveryVerifier(t, server, newClock())
				if !errors.Is(err, r.want) || !errors.Is(err, ErrDiscovery) || !errors.Is(err, ErrKeys) {
					t.Fatalf("the start gave %v, want the refusal %q of the discovery document", err, r.want)
				}
				if text := err.Error(); !strings.Contains(text, "discovery document") || strings.Contains(text, server.Addr()) {
					t.Errorf("the refusal %q does not name the discovery document, or holds the address", text)
				}
			})
		}

		t.Run("a redirect is not followed", func(t *testing.T) {
			// The discovery document of the issuer redirects to another path that serves the same valid document:
			// only the refused redirect stops the start.
			mux := authntest.NewServer(t)
			mux.Publish(rsaKey)
			redirected := mux.Issuer("/redirected")
			mux.SetDiscovery(authntest.Discovery(redirected, mux.URL()))
			set, err := newKeySet(context.Background(), KeySource{Issuer: redirected}, keyOptions{now: time.Now, roots: rootsOf(mux)})
			if err != nil {
				t.Fatalf("the control start failed: %v", err)
			}
			set.Stop()
			mux.RedirectDiscovery(mux.Issuer(issuerPath) + authntest.WellKnown)
			_, err = newKeySet(context.Background(), KeySource{Issuer: redirected}, keyOptions{now: time.Now, roots: rootsOf(mux)})
			if !errors.Is(err, ErrDiscovery) {
				t.Fatalf("a redirected discovery document gave %v, want a refusal of the discovery document", err)
			}
		})

		t.Run("the issuer must be an HTTPS URL with no query, and the CA file adds to the system roots", func(t *testing.T) {
			for _, bad := range []string{strings.Replace(issuer, "https:", "http:", 1), issuer + "?realm=shop", issuer + "#x"} {
				if _, err := NewKeySet(context.Background(), KeySource{Issuer: bad}); !errors.Is(err, ErrSettings) {
					t.Errorf("the issuer %q gave %v, want a refusal of the settings", bad, err)
				}
			}
			if _, err := NewKeySet(context.Background(), KeySource{Issuer: issuer, URL: server.URL()}); !errors.Is(err, ErrSettings) {
				t.Errorf("an issuer beside a URL gave %v, want a refusal of the settings", err)
			}
			if _, err := NewKeySet(context.Background(), KeySource{Issuer: issuer}); !errors.Is(err, ErrDiscovery) {
				t.Errorf("the issuer of a server that the system roots do not trust gave %v, want a refusal of the discovery document", err)
			}
			set, err := NewKeySet(context.Background(), KeySource{Issuer: issuer, CAFile: server.CAFile(t)})
			if err != nil {
				t.Fatalf("the issuer with the CA file gave %v", err)
			}
			set.Stop()
		})
	})
}
