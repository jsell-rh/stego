package keycloak

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestBoundedClientNameSearch(t *testing.T) {
	var requests atomic.Int32
	var reads atomic.Int32
	fragment := "catalog & reports+?=/é"
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if authRequest(w, r) {
			return
		}
		reads.Add(1)
		query := r.URL.Query()
		if r.Method != "GET" || r.URL.Path != "/admin/realms/tenant/clients" || len(query) != 4 || query.Get("clientId") != fragment || query.Get("search") != "true" || query.Get("first") != "2" || query.Get("max") != "3" {
			t.Error("search escaped its selected realm, fragment, or page")
			w.WriteHeader(400)
			return
		}
		_, _ = w.Write([]byte(`[{"id":"candidate","clientId":"catalog & reports+?=/é"}]`))
	})
	page, err := c.SearchClients(context.Background(), fragment, Page{First: 2, Size: 3})
	if err != nil || len(page) != 1 || page[0].ID != "candidate" || reads.Load() != 1 {
		t.Fatal("candidate search failed", err)
	}
	before := requests.Load()
	for _, invalid := range []string{"", " ", "%", "_", `a\b`, "[a]", "name%", "name_", "name[", "name]", "a\x00b", string([]byte{255}), strings.Repeat("a", 256)} {
		if _, err := c.SearchClients(context.Background(), invalid, Page{Size: 1}); err == nil {
			t.Fatal("invalid fragment accepted")
		}
	}
	for _, invalid := range []Page{{Size: 0}, {First: -1, Size: 1}, {Size: 101}, {First: 9999, Size: 2}} {
		if _, err := c.SearchClients(context.Background(), "catalog", invalid); err == nil {
			t.Fatal("invalid search page accepted")
		}
	}
	if requests.Load() != before {
		t.Fatal("invalid search caused a network request")
	}
}

func TestClientNameSearchDoesNotExpandFailedQueries(t *testing.T) {
	for _, test := range []struct {
		name string
		code int
		body string
		want error
	}{
		{"denied", 403, "", nil},
		{"null", 200, "null", ErrResponse},
		{"too many", 200, `[{"id":"one","clientId":"catalog-one"},{"id":"two","clientId":"catalog-two"}]`, ErrResponse},
		{"duplicate", 200, `[{"id":"one","clientId":"catalog-one"},{"id":"one","clientId":"catalog-one"}]`, ErrResponse},
	} {
		t.Run(test.name, func(t *testing.T) {
			var reads atomic.Int32
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if authRequest(w, r) {
					return
				}
				reads.Add(1)
				if r.URL.Query().Get("clientId") != "catalog" || r.URL.Query().Get("search") != "true" {
					t.Error("search fell back to another query")
				}
				w.WriteHeader(test.code)
				_, _ = w.Write([]byte(test.body))
			})
			size := 1
			if test.name == "duplicate" {
				size = 2
			}
			if _, err := c.SearchClients(context.Background(), "catalog", Page{Size: size}); err == nil || (test.want != nil && !errors.Is(err, test.want)) {
				t.Fatal("unsafe search result", err)
			}
			if reads.Load() != 1 {
				t.Fatal("failed search was repeated or expanded")
			}
		})
	}
}
