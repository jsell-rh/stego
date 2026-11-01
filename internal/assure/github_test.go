package assure

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestGitHubAttestations(t *testing.T) {
	t.Run("ST-INT01-T01 GitHubAttestations fetches every proof run bundle GitHub keeps for a commit by its subject digest, and refuses what it cannot trust", func(t *testing.T) {
		paged := strings.Repeat("a1", 20)
		none := strings.Repeat("b2", 20)
		failing := strings.Repeat("c3", 20)
		elsewhere := strings.Repeat("d4", 20)
		var server *httptest.Server
		var asked []string
		server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			asked = append(asked, r.URL.RequestURI())
			if r.Header.Get("Authorization") != "Bearer job-token" || r.Header.Get("Accept") != "application/vnd.github+json" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if r.URL.Query().Get("predicate_type") != ProofRunPredicateType || r.URL.Query().Get("per_page") != "100" {
				http.Error(w, "bad query", http.StatusBadRequest)
				return
			}
			switch r.URL.Path {
			case "/repos/jsell-rh/stego/attestations/" + SubjectDigest(paged):
				if r.URL.Query().Get("after") == "" {
					w.Header().Set("Link", `<`+server.URL+`/repos/jsell-rh/stego/attestations/`+SubjectDigest(paged)+`?per_page=100&predicate_type=`+
						strings.ReplaceAll(ProofRunPredicateType, "/", "%2F")+`&after=x>; rel="next"`)
					w.Write([]byte(`{"attestations": [{"bundle": {"first": 1}, "repository_id": 1}, {"bundle": null, "bundle_url": "https://example.com/b.json.sn"}]}`))
					return
				}
				w.Write([]byte(`{"attestations": [{"bundle": {"second": 2}}]}`))
			case "/repos/jsell-rh/stego/attestations/" + SubjectDigest(none):
				http.Error(w, `{"message": "Not Found"}`, http.StatusNotFound)
			case "/repos/jsell-rh/stego/attestations/" + SubjectDigest(elsewhere):
				w.Header().Set("Link", `<https://example.com/next>; rel="next"`)
				w.Write([]byte(`{"attestations": []}`))
			default:
				http.Error(w, "broken", http.StatusBadGateway)
			}
		}))
		defer server.Close()
		store := GitHubAttestations{API: server.URL, Slug: "jsell-rh/stego", Token: "job-token", Client: server.Client()}

		bundles, err := store.Fetch(paged)
		if err != nil {
			t.Fatal(err)
		}
		want := [][]byte{[]byte(`{"first": 1}`), nil, []byte(`{"second": 2}`)}
		if !reflect.DeepEqual(bundles, want) {
			t.Fatalf("got the bundles %q, want %q", bundles, want)
		}
		if len(asked) != 2 || !strings.HasPrefix(asked[0], "/repos/jsell-rh/stego/attestations/"+SubjectDigest(paged)+"?") {
			t.Fatalf("GitHub was asked %v, want two pages by the subject digest", asked)
		}
		if bundles, err := store.Fetch(none); err != nil || len(bundles) != 0 {
			t.Fatalf("a commit GitHub keeps nothing for gives %q, %v; want none", bundles, err)
		}
		for name, check := range map[string]struct {
			store  GitHubAttestations
			commit string
		}{
			"an answer that is not 200 or 404":    {store, failing},
			"a next page on another host":         {store, elsewhere},
			"a short commit ID":                   {store, paged[:7]},
			"an API that is not https":            {GitHubAttestations{API: "http://api.github.com", Slug: "jsell-rh/stego"}, paged},
			"a repository that is not owner/name": {GitHubAttestations{API: server.URL, Slug: "jsell-rh/stego/../x", Client: server.Client()}, paged},
			"a token GitHub does not take":        {GitHubAttestations{API: server.URL, Slug: "jsell-rh/stego", Token: "other", Client: server.Client()}, paged},
		} {
			if bundles, err := check.store.Fetch(check.commit); err == nil {
				t.Fatalf("%s gives %q and no error", name, bundles)
			}
		}
	})
}
