package appbuild

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

type registryRoundTripFunc func(*http.Request) (*http.Response, error)

func (f registryRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func registryTLSFixture(t *testing.T, handler http.Handler) (*httptest.Server, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Registry Test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	raw, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{raw}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw})
}

func registryFixtureAccess(t *testing.T, server *httptest.Server, ca []byte) RegistryAccess {
	t.Helper()
	root := t.TempDir()
	put(t, root, "ca.pem", string(ca))
	credentials := filepath.Join(root, "credentials.json")
	if err := os.WriteFile(credentials, []byte("{\"username\":\"test\",\"password\":\"fixture-credential\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return RegistryAccess{Repository: strings.TrimPrefix(server.URL, "https://") + "/test/application", CAFile: filepath.Join(root, "ca.pem"), CASHA256: digest(ca), Credentials: credentials}
}

func TestRegistryTLSRoundTripRetainsExactImage(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	handler := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	var authenticated atomic.Int64
	server, ca := registryTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "test" || password != "fixture-credential" {
			w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		authenticated.Add(1)
		handler.ServeHTTP(w, r)
	}))
	root, record := imageFixture(t, imageTestCA(t, true))
	access := registryFixtureAccess(t, server, ca)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, err := connectRegistry(ctx, access, record, true)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.close()
	captured := filepath.Join(t.TempDir(), "captured")
	if err := captureRegistryLayout(ctx, filepath.Join(root, "oci"), captured, record); err != nil {
		t.Fatal(err)
	}
	// Later source changes must not change the publication input.
	put(t, root, "oci/blobs/sha256/"+record.Layer.SHA256, "changed after capture")
	store, err := layout.FromPath(captured)
	if err != nil {
		t.Fatal(err)
	}
	image, err := store.Image(v1.Hash{Algorithm: "sha256", Hex: record.Manifest.SHA256})
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(connection.repository.Tag("sha256-"+record.Manifest.SHA256), image, remote.WithContext(ctx), remote.WithTransport(connection.transport), remote.WithJobs(1), remote.WithRetryBackoff(remote.Backoff{Steps: 1})); err != nil {
		t.Fatal(err)
	}
	retrieved := filepath.Join(t.TempDir(), "retrieved")
	if err := connection.retrieve(ctx, retrieved, record); err != nil {
		t.Fatal(err)
	}
	first, _, err := inventory(captured, 5, imageLayerLimit)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := inventory(retrieved, 5, imageLayerLimit)
	if err != nil || first != second {
		t.Fatal("registry bytes differ", err)
	}
	if authenticated.Load() < 5 {
		t.Fatal("registry authentication was not exercised")
	}
	// Repeated publication exercises HEAD requests for existing large blobs.
	if err := remote.Write(connection.repository.Tag("sha256-"+record.Manifest.SHA256), image, remote.WithContext(ctx), remote.WithTransport(connection.transport), remote.WithJobs(1)); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryBearerRealmNeedsExplicitApproval(t *testing.T) {
	for _, approved := range []bool{false, true} {
		t.Run(map[bool]string{false: "unapproved", true: "approved"}[approved], func(t *testing.T) {
			var tokenCalls atomic.Int64
			token, tokenCA := registryTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tokenCalls.Add(1)
				user, password, ok := r.BasicAuth()
				if !ok || user != "test" || password != "fixture-credential" || r.URL.Query().Get("scope") != "repository:test/application:pull" {
					t.Error("token credentials or scope differ")
					w.WriteHeader(403)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"token":"fixture-access-token","expires_in":60}`)
			}))
			tokenURL, _ := url.Parse(token.URL)
			tokenURL.Host = "localhost:" + tokenURL.Port()
			server, ca := registryTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+tokenURL.String()+`/token",service="fixture"`)
				w.WriteHeader(401)
			}))
			_, record := imageFixture(t, imageTestCA(t, true))
			access := registryFixtureAccess(t, server, append(ca, tokenCA...))
			if approved {
				access.TokenOrigins = []string{tokenURL.String()}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			connection, err := connectRegistry(ctx, access, record, false)
			if approved {
				if err != nil {
					t.Fatal(err)
				}
				connection.close()
				if tokenCalls.Load() != 1 {
					t.Fatal("approved token request missing")
				}
			} else {
				if err == nil {
					connection.close()
					t.Fatal("unapproved token realm accepted")
				}
				if tokenCalls.Load() != 0 {
					t.Fatal("credentials reached an unapproved token origin")
				}
			}
		})
	}
}

func TestRegistryBoundaryRejectsCredentialLeaksAndPathEscapes(t *testing.T) {
	var calls int
	boundary := &registryBoundary{origin: "https://registry.example", prefix: "/v2/test/app/", tokens: map[string]bool{"https://auth.example": true}, blobs: map[string]bool{"https://blobs.example": true}, base: registryRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
	})}
	boundary.remaining.Store(1000)
	for _, test := range []struct{ url, method, auth string }{
		{"http://registry.example/v2/", "GET", ""}, {"https://unapproved.example/token", "GET", "Basic fixture"},
		{"https://registry.example/v2/other/app/blobs/x", "GET", ""}, {"https://registry.example/v2/test/app/../other", "GET", ""},
		{"https://registry.example/v2/test/app/%2e%2e/other", "GET", ""}, {"https://user:password@registry.example/v2/", "GET", ""},
		{"https://blobs.example/path", "GET", "Bearer fixture"}, {"https://blobs.example/path", "POST", ""},
		{"https://auth.example/path", "PUT", "Basic fixture"},
	} {
		r, err := http.NewRequest(test.method, test.url, nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", test.auth)
		if _, err := boundary.RoundTrip(r); err == nil {
			t.Fatal("unsafe registry destination accepted", test.url)
		}
	}
	if calls != 0 {
		t.Fatal("unsafe request reached the network")
	}
	r, _ := http.NewRequest("GET", "https://blobs.example/path", nil)
	response, err := boundary.RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if calls != 1 {
		t.Fatal("approved public blob request was rejected")
	}
}

func TestRegistryResponseAndRequestBounds(t *testing.T) {
	boundary := &registryBoundary{origin: "https://registry.example", prefix: "/v2/test/app/", layer: Artifact{strings.Repeat("a", 64), 4}, base: registryRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: -1, Body: io.NopCloser(strings.NewReader("oversized"))}, nil
	})}
	boundary.remaining.Store(100)
	r, _ := http.NewRequest("GET", "https://registry.example/v2/test/app/blobs/sha256:"+boundary.layer.SHA256, nil)
	response, err := boundary.RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(response.Body); err == nil {
		t.Fatal("oversized chunked body accepted")
	}
	if _, err := response.Body.Read(make([]byte, 1)); err == nil {
		t.Fatal("failed body resumed reading")
	}
	response.Body.Close()
	boundary.requests.Store(128)
	if _, err := boundary.RoundTrip(r); err == nil {
		t.Fatal("unbounded request count")
	}
	boundary.requests.Store(0)
	boundary.remaining.Store(2)
	response, err = boundary.RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(response.Body); err == nil {
		t.Fatal("total read budget ignored")
	}
	response.Body.Close()
}

func TestRegistryTLSCredentialsAndPolicyFailClosed(t *testing.T) {
	server, ca := registryTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	_, record := imageFixture(t, imageTestCA(t, true))
	base := registryFixtureAccess(t, server, ca)
	for _, name := range []string{"CA digest", "untrusted CA", "credential mode", "credential link", "credential fields", "missing credentials", "token scheme", "blob overlap", "repository tag"} {
		t.Run(name, func(t *testing.T) {
			access := base
			root := t.TempDir()
			switch name {
			case "CA digest":
				access.CASHA256 = strings.Repeat("0", 64)
			case "untrusted CA":
				data := imageTestCA(t, true)
				put(t, root, "ca.pem", string(data))
				access.CAFile = filepath.Join(root, "ca.pem")
				access.CASHA256 = digest(data)
			case "credential mode":
				put(t, root, "credentials", "{\"username\":\"test\",\"password\":\"fixture\"}\n")
				access.Credentials = filepath.Join(root, "credentials")
				os.Chmod(access.Credentials, 0644)
			case "credential link":
				access.Credentials = filepath.Join(root, "link")
				if err := os.Symlink(base.Credentials, access.Credentials); err != nil {
					t.Fatal(err)
				}
			case "credential fields":
				access.Credentials = filepath.Join(root, "credentials")
				if err := os.WriteFile(access.Credentials, []byte("{\"username\":\"test\",\"password\":\"fixture\",\"extra\":true}\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing credentials":
				access.Credentials = ""
			case "token scheme":
				access.TokenOrigins = []string{"http://auth.example"}
			case "blob overlap":
				access.TokenOrigins = []string{"https://same.example"}
				access.BlobOrigins = access.TokenOrigins
			case "repository tag":
				access.Repository += ":tag"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			connection, err := connectRegistry(ctx, access, record, true)
			if err == nil {
				connection.close()
				t.Fatal("unsafe registry access accepted", name)
			}
		})
	}
}

func TestRegistryRejectsCorruptDownloadAndHonorsCancellation(t *testing.T) {
	server, ca := registryTLSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(200)
			return
		}
		io.WriteString(w, "changed")
	}))
	_, record := imageFixture(t, imageTestCA(t, true))
	access := registryFixtureAccess(t, server, ca)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, err := connectRegistry(ctx, access, record, false)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.close()
	if err := connection.retrieve(ctx, filepath.Join(t.TempDir(), "retrieved"), record); err == nil || !strings.Contains(err.Error(), "bytes differ") {
		t.Fatal("corrupt bytes accepted", err)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, err := PublishImage(canceled, RegistryImageOptions{}); err != context.Canceled {
		t.Fatal("publish cancellation lost", err)
	}
	if _, err := RetrieveImage(canceled, RegistryImageOptions{}); err != context.Canceled {
		t.Fatal("retrieve cancellation lost", err)
	}
}

func TestRegistryTokenScopeCannotExpandCredentials(t *testing.T) {
	calls := 0
	b := &registryBoundary{origin: "https://registry.example", prefix: "/v2/test/app/", tokens: map[string]bool{"https://auth.example": true}, scope: "repository:test/app:pull", base: registryRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
	})}
	b.remaining.Store(1000)
	for _, scope := range []string{"", "repository:other/app:pull", "repository:test/app:push,pull", "registry:catalog:*"} {
		for _, method := range []string{"GET", "POST"} {
			address := "https://auth.example/token"
			var body io.Reader
			values := url.Values{"scope": {scope}}
			if method == "GET" {
				address += "?" + values.Encode()
			} else {
				body = strings.NewReader(values.Encode())
			}
			request, _ := http.NewRequest(method, address, body)
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if _, err := b.RoundTrip(request); err == nil {
				t.Fatal("token scope expansion accepted", scope, method)
			}
		}
	}
	if calls != 0 {
		t.Fatal("unapproved token scope reached the server")
	}
	request, _ := http.NewRequest("POST", "https://auth.example/token", strings.NewReader(url.Values{"scope": {b.scope}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := b.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if calls != 1 {
		t.Fatal("selected token scope was rejected")
	}
}

type registryCloseCheck struct {
	io.Reader
	closed bool
}

func (r *registryCloseCheck) Close() error { r.closed = true; return nil }

func TestRegistryBlobCannotStartAuthenticationOrReceiveReferer(t *testing.T) {
	body := &registryCloseCheck{Reader: strings.NewReader("private challenge detail")}
	b := &registryBoundary{origin: "https://registry.example", prefix: "/v2/test/app/", blobs: map[string]bool{"https://blobs.example": true}, base: registryRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Referer") != "" {
			t.Fatal("signed redirect query reached the blob destination")
		}
		return &http.Response{StatusCode: 401, Header: http.Header{"Www-Authenticate": {`Bearer realm="https://unapproved.example/token?private=value"`}}, Body: body}, nil
	})}
	b.remaining.Store(1000)
	request, _ := http.NewRequest("GET", "https://blobs.example/layer", nil)
	request.Header.Set("Referer", "https://registry.example/blob?private=value")
	response, err := b.RoundTrip(request)
	if err == nil || response != nil || !body.closed {
		t.Fatal("blob authentication challenge escaped the boundary", err)
	}
	if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "unapproved.example") {
		t.Fatal("challenge details escaped through an error")
	}
	if request.Header.Get("Referer") == "" {
		t.Fatal("the boundary changed the caller's header")
	}
}
