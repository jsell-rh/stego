package browser

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestDynamicStylesUseFreshDocumentNonce(t *testing.T) {
	originURL, _ := url.Parse(origin)
	b := &Backend{origin: originURL, permits: make(chan struct{}, 1)}
	if err := json.Unmarshal([]byte(generatedConfiguration), &b.config); err != nil {
		t.Fatal(err)
	}
	before := send(b, "GET", "/index.html", "", nil, nil)
	require(t, before.Code == 200, "default document failed")
	require(t, !strings.Contains(before.Header().Get("Content-Security-Policy"), "nonce-"), "undeclared nonce enabled")
	body, err := assets.ReadFile("public/index.html")
	if err != nil {
		t.Fatal(err)
	}
	b.config.DynamicStyles = true
	b.config.RuntimeConfigOffset = bytes.Index(body, []byte("<head>")) + len("<head>")
	previousTag := before.Header().Get("ETag")
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		w := send(b, "GET", "/index.html", "", nil, http.Header{"If-None-Match": {previousTag}})
		require(t, w.Code == 200 && w.Header().Get("Cache-Control") == "no-store", "document nonce reused cached response")
		parts := strings.Split(w.Body.String(), `<meta name="stego-style-nonce" content="`)
		require(t, len(parts) == 2, "document nonce is missing or repeated")
		nonce, _, ok := strings.Cut(parts[1], `">`)
		raw, err := base64.RawURLEncoding.DecodeString(nonce)
		require(t, ok && err == nil && len(raw) == 32 && !seen[nonce], "document nonce is invalid or repeated")
		seen[nonce] = true
		policy := w.Header().Get("Content-Security-Policy")
		require(t, strings.Contains(policy, "style-src 'self' 'nonce-"+nonce+"'; style-src-attr 'none'"), "style nonce differs from document")
		require(t, !strings.Contains(policy, "unsafe-inline") && !strings.Contains(strings.Split(policy, "style-src")[0], "nonce-"), "style nonce weakened script policy")
		require(t, w.Header().Get("ETag") != previousTag, "document ETag was reused")
		previousTag = w.Header().Get("ETag")
	}
	head := send(b, "HEAD", "/index.html", "", nil, http.Header{"If-None-Match": {previousTag}})
	require(t, head.Code == 200 && head.Body.Len() == 0 && head.Header().Get("ETag") != previousTag, "HEAD reused a nonce or sent a body")
	asset := send(b, "GET", "/assets/main.js", "", nil, nil)
	require(t, asset.Code == 200 && !strings.Contains(asset.Header().Get("Content-Security-Policy"), "nonce-"), "asset acquired a document nonce")
	b.config.RuntimeConfigOffset = len(body) + 1
	bad := send(b, "GET", "/index.html", "", nil, nil)
	require(t, bad.Code == 500 && !strings.Contains(bad.Body.String(), "stego-style-nonce"), "invalid insertion offset did not fail closed")
}
