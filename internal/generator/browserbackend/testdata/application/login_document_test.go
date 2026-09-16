package browser

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestApplicationLoginDocumentTargets(t *testing.T) {
	b := &Backend{routes: []*regexp.Regexp{regexp.MustCompile(`^/$`), regexp.MustCompile(`^/records/[^/]+$`)}}
	const query = `/records/record-1?label="><script>private</script>&tab=history`
	for _, test := range []struct{ name, target, want string }{
		{"local", "/records/record-1", "/records/record-1"},
		{"quoted query", query, query},
		{"absolute", "https://other.example/", "/"},
		{"network path", "//other.example/", "/"},
		{"auth route", "/auth/logout", "/"},
		{"fragment", "/records/record-1#fragment", "/"},
		{"encoded path", "/records/a%2fb", "/"},
		{"header injection", "/records/a\r\nLocation: https://other.example", "/"},
		{"length", "/records/" + strings.Repeat("x", 2048), "/"},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			b.completeApplicationLogin(w, test.target)
			body := w.Body.String()
			refresh := regexp.MustCompile(`<meta http-equiv="refresh" content="([^"]*)">`).FindStringSubmatch(body)
			link := regexp.MustCompile(`<a href="([^"]*)">`).FindStringSubmatch(body)
			require(t, w.Code == 200 && w.Header().Get("Location") == "", "completion did not commit a document")
			require(t, len(refresh) == 2 && html.UnescapeString(refresh[1]) == "0; URL="+test.want, "completion refresh changed or accepted an unsafe target")
			require(t, len(link) == 2, "completion fallback link is missing")
			actual, err := url.Parse(html.UnescapeString(link[1]))
			want, wantErr := url.Parse(test.want)
			require(t, err == nil && wantErr == nil && !actual.IsAbs() && actual.Host == "" && actual.Fragment == "" && actual.Path == want.Path && actual.Query().Encode() == want.Query().Encode(), "completion fallback link changed or accepted an unsafe target")
			require(t, !strings.Contains(body, "<script") && strings.Count(body, "<a ") == 1 && strings.Count(body, "<meta http-equiv=") == 1, "return path injected active markup")
			require(t, w.Header().Get("Set-Cookie") == "", "completion renderer changed a cookie")
			require(t, w.Header().Get("Cache-Control") == "no-store" && w.Header().Get("Referrer-Policy") == "no-referrer" && w.Header().Get("Cross-Origin-Opener-Policy") == "same-origin", "completion lost its privacy headers")
			require(t, w.Header().Get("Content-Security-Policy") == "default-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'", "completion permits active or framed content")
		})
	}
}

func TestApplicationLoginDocumentSessionAndReplay(t *testing.T) {
	f := applicationFixture(t)
	loginCookie, callback := startLogin(t, f)
	w := send(f.backend, "GET", callback, "", []*http.Cookie{loginCookie}, http.Header{"Sec-Fetch-Site": {"cross-site"}})
	require(t, w.Code == 200 && strings.Contains(w.Body.String(), `content="0; URL=/records/record-1"`), "verified callback did not return the completion document")
	active := cookie(t, w, SessionCookie)
	require(t, active.SameSite == http.SameSiteStrictMode && active.Secure && active.HttpOnly && active.Domain == "" && active.Path == "/", "completion weakened the session cookie")
	for _, private := range []string{active.Value, loginCookie.Value, "initial-access-value", "refresh-value", "code=", "state="} {
		require(t, !strings.Contains(w.Body.String(), private), "completion exposed private state")
	}
	// Model cookie omission on a cross-site request, then the new document's
	// same-origin navigation. Real browser enforcement has a separate live gate.
	w = send(f.backend, "GET", "/records/record-1", "", nil, http.Header{"Sec-Fetch-Site": {"cross-site"}})
	require(t, w.Code == 200 && strings.Contains(w.Body.String(), "Sign in to continue"), "anonymous cross-site request reached the application")
	w = send(f.backend, "GET", "/records/record-1", "", []*http.Cookie{active}, http.Header{"Sec-Fetch-Site": {"same-origin"}})
	require(t, w.Code == 200 && !strings.Contains(w.Body.String(), "Sign in to continue"), "completion session cannot open its protected document")
	w = send(f.backend, "GET", callback, "", []*http.Cookie{loginCookie}, http.Header{"Sec-Fetch-Site": {"cross-site"}})
	require(t, w.Code == 400 && !strings.Contains(w.Body.String(), "http-equiv"), "replayed callback produced a completion document")
}
