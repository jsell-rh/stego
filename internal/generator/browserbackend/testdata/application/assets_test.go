package browser

import (
	"net/http"
	"strings"
	"testing"
)

func TestCapturedApplicationAssets(t *testing.T) {
	f := applicationFixture(t)
	if len(f.backend.config.Assets) == 0 {
		t.Skip("The application serves its own assets.")
	}
	c, csrf := login(t, f)
	second := f.start(t)
	for _, backend := range []*Backend{f.backend, second} {
		for _, path := range []string{"/", "/records/record-1", "/index.html", "/assets/main.js", "/assets/symbols.ttf"} {
			w := send(backend, "GET", path, "", []*http.Cookie{c}, nil)
			require(t, w.Code == 200, "captured asset was not served after restart")
			require(t, w.Header().Get("Cache-Control") == "no-store", "authenticated asset permits cache storage")
			require(t, !strings.Contains(w.Body.String(), "initial-access-value"), "captured asset disclosed a token")
			if !strings.HasPrefix(path, "/assets/") {
				require(t, strings.Contains(w.Body.String(), `<meta name="stego-runtime-config"`), "captured document has no runtime settings")
				require(t, strings.Contains(w.Header().Get("Content-Security-Policy"), "'sha256-"), "captured document lost its checked script hash")
			} else if path == "/assets/symbols.ttf" {
				require(t, w.Header().Get("Content-Type") == "font/ttf" && w.Header().Get("X-Content-Type-Options") == "nosniff", "font response controls differ")
				require(t, w.Body.String() == string([]byte{0, 1, 0, 0, 255, 128}), "captured font bytes changed")
			} else {
				require(t, w.Body.String() == `"use strict";`, "captured asset was replaced by an upstream response")
			}
			headers := http.Header{"If-None-Match": {w.Header().Get("ETag")}}
			require(t, send(backend, "GET", path, "", []*http.Cookie{c}, headers).Code == 304, "authenticated conditional request failed")
			require(t, send(backend, "GET", path, "", nil, headers).Code != 304, "conditional request bypassed authentication")
			require(t, send(backend, "HEAD", path, "", nil, nil).Code == 401, "anonymous HEAD disclosed a captured asset")
			require(t, send(backend, "GET", path, "", []*http.Cookie{c}, http.Header{"Authorization": {"Bearer attacker"}}).Code == 400, "captured asset accepted a browser token")
		}
		require(t, send(backend, "GET", "/assets/unlisted.js", "", []*http.Cookie{c}, nil).Code == 404, "unlisted asset reached the upstream application")
	}
	expireAccess(t, f, c)
	require(t, send(second, "GET", "/assets/main.js", "", []*http.Cookie{c}, nil).Code == 200, "captured asset did not refresh its session")
	w := send(f.backend, "POST", "/auth/logout", "", []*http.Cookie{c}, http.Header{"Origin": {origin}, CSRFHeader: {csrf}})
	require(t, w.Code == 204, "captured application logout failed")
	for _, path := range []string{"/assets/main.js", "/assets/symbols.ttf"} {
		require(t, send(second, "GET", path, "", []*http.Cookie{c}, nil).Code == 401, "captured asset survived logout on another backend")
	}
}
