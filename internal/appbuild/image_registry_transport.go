package appbuild

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	registrytransport "github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// RegistryAccess is operator policy. The registry CA is separate from the CA
// bundle inside the application image. No ambient keychain or proxy is used.
type RegistryAccess struct {
	Repository                string
	CAFile, CASHA256          string
	Credentials               string
	TokenOrigins, BlobOrigins []string
}

type registryCredentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type registryBoundary struct {
	base           http.RoundTripper
	origin, prefix string
	tokens, blobs  map[string]bool
	requests       atomic.Int64
	remaining      atomic.Int64
	layer          Artifact
	config         Artifact
	scope          string
}

func registryOrigin(value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Host != strings.ToLower(u.Host) || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.ForceQuery || strings.HasSuffix(u.Hostname(), ".") {
		return "", errors.New("registry origins must be explicit HTTPS origins without paths")
	}
	if u.Port() == "443" {
		return "", errors.New("registry origins must omit the default HTTPS port")
	}
	return u.String(), nil
}

func registryOriginSet(values []string) (map[string]bool, error) {
	if len(values) > 8 {
		return nil, errors.New("too many registry origins")
	}
	result := map[string]bool{}
	for _, value := range values {
		origin, err := registryOrigin(value)
		if err != nil {
			return nil, err
		}
		if result[origin] {
			return nil, errors.New("repeated registry origin")
		}
		result[origin] = true
	}
	return result, nil
}

// Apply the boundary below the library's authentication transport. This checks
// token exchanges and redirects before the connection can send credentials.
func (b *registryBoundary) RoundTrip(request *http.Request) (*http.Response, error) {
	// Redirects can contain signed query strings. Do not send those strings
	// to another endpoint through the automatic Referer header.
	request = request.Clone(request.Context())
	request.Header.Del("Referer")
	handedOff := false
	defer func() {
		if !handedOff && request.Body != nil {
			request.Body.Close()
		}
	}()
	u := request.URL
	if u == nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Opaque != "" || (request.Host != "" && request.Host != u.Host) || request.Header.Get("Proxy-Authorization") != "" || request.Header.Get("Cookie") != "" || u.RawPath != "" || (u.Path != "/" && path.Clean(u.Path) != strings.TrimSuffix(u.Path, "/")) {
		return nil, errors.New("registry request violates the HTTPS boundary")
	}
	origin := u.Scheme + "://" + u.Host
	limit := int64(imageMetadataLimit)
	switch {
	case origin == b.origin:
		if strings.HasPrefix(u.Path, "/v2/") && u.Path != "/v2/" && !strings.HasPrefix(u.Path, b.prefix) {
			return nil, errors.New("registry request leaves the selected repository")
		}
		if (request.Method == http.MethodGet || request.Method == http.MethodHead) && u.Path == b.prefix+"blobs/sha256:"+b.layer.SHA256 {
			limit = b.layer.Size
		}
		if (request.Method == http.MethodGet || request.Method == http.MethodHead) && u.Path == b.prefix+"blobs/sha256:"+b.config.SHA256 {
			limit = b.config.Size
		}
	case b.tokens[origin]:
		if request.Method != http.MethodGet && request.Method != http.MethodPost {
			return nil, errors.New("registry token method is not allowed")
		}
	case b.blobs[origin]:
		if (request.Method != http.MethodGet && request.Method != http.MethodHead) || request.Header.Get("Authorization") != "" || request.Body != nil {
			return nil, errors.New("registry blob redirect cannot receive credentials or a request body")
		}
		limit = b.layer.Size
	default:
		return nil, errors.New("registry destination is not approved")
	}
	isToken := b.tokens[origin] || (origin == b.origin && !strings.HasPrefix(u.Path, "/v2/"))
	if isToken || len(u.Query()["scope"]) > 0 {
		selected := u.Query()["scope"]
		if request.Method == http.MethodPost {
			if request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || request.Body == nil {
				return nil, errors.New("invalid registry token request body")
			}
			data, err := io.ReadAll(io.LimitReader(request.Body, (32<<10)+1))
			request.Body.Close()
			if err != nil || len(data) > 32<<10 {
				return nil, errors.New("registry token request size exceeded")
			}
			values, err := url.ParseQuery(string(data))
			if err != nil {
				return nil, errors.New("invalid registry token form")
			}
			selected = append(selected, values["scope"]...)
			request = request.Clone(request.Context())
			request.Body = io.NopCloser(bytes.NewReader(data))
		}
		if len(selected) != 1 || b.scope == "" || selected[0] != b.scope {
			return nil, errors.New("registry token scope differs from the selected repository")
		}
	}
	if b.requests.Add(1) > 128 {
		return nil, errors.New("registry request count exceeded")
	}
	handedOff = true
	response, err := b.base.RoundTrip(request)
	if err != nil {
		return nil, errors.New("registry connection failed")
	}
	if b.blobs[origin] && response.StatusCode >= 400 {
		// A public blob destination cannot start another authentication flow.
		// This also keeps its challenge and response body out of library logs.
		response.Body.Close()
		return nil, errors.New("registry blob destination rejected the request")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		limit = imageMetadataLimit
	}
	if response.ContentLength > limit {
		response.Body.Close()
		return nil, errors.New("registry response exceeds its size limit")
	}
	response.Body = &registryBody{body: response.Body, left: limit, budget: &b.remaining}
	return response, nil
}

type registryBody struct {
	body   io.ReadCloser
	left   int64
	budget *atomic.Int64
}

func (b *registryBody) Read(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if b.left < 0 || b.budget.Load() < 0 {
		return 0, errors.New("registry response byte limit exceeded")
	}
	// Read one byte beyond the permitted size only to detect an oversized body.
	if int64(len(data)) > b.left+1 {
		data = data[:b.left+1]
	}
	n, err := b.body.Read(data)
	b.left -= int64(n)
	if b.budget.Add(-int64(n)) < 0 || b.left < 0 {
		return 0, errors.New("registry response byte limit exceeded")
	}
	return n, err
}
func (b *registryBody) Close() error { return b.body.Close() }

type registryConnection struct {
	repository name.Repository
	transport  http.RoundTripper
	client     *http.Client
	close      func()
}

func connectRegistry(ctx context.Context, access RegistryAccess, record *ImageRecord, write bool) (*registryConnection, error) {
	repository, err := name.NewRepository(access.Repository, name.StrictValidation)
	if err != nil || repository.Name() != access.Repository || !strings.Contains(access.Repository, "/") {
		return nil, errors.New("select an explicit registry repository without a tag")
	}
	origin, err := registryOrigin("https://" + repository.RegistryStr())
	if err != nil {
		return nil, err
	}
	tokens, err := registryOriginSet(access.TokenOrigins)
	if err != nil {
		return nil, err
	}
	blobs, err := registryOriginSet(access.BlobOrigins)
	if err != nil {
		return nil, err
	}
	for value := range blobs {
		if tokens[value] || value == origin {
			return nil, errors.New("registry blob and credential origins must be separate")
		}
	}
	ca, err := readImageBytes(access.CAFile, trustStoreLimit)
	if err != nil || !hashPattern.MatchString(access.CASHA256) || digest(ca) != access.CASHA256 {
		return nil, errors.New("registry CA digest differs")
	}
	if err := validTrustStore(ca); err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, errors.New("registry CA bundle is invalid")
	}
	var auth authn.Authenticator = authn.Anonymous
	if access.Credentials != "" {
		file, err := openInput(access.Credentials)
		if err != nil {
			return nil, errors.New("cannot open registry credentials")
		}
		info, statErr := file.Stat()
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 1 || info.Size() > 16<<10 {
			file.Close()
			return nil, errors.New("registry credentials must be a private bounded regular file")
		}
		data, readErr := io.ReadAll(io.LimitReader(file, (16<<10)+1))
		file.Close()
		var credentials registryCredentials
		if readErr != nil || int64(len(data)) != info.Size() || json.Unmarshal(data, &credentials) != nil || credentials.Username == "" || credentials.Password == "" || strings.ContainsAny(credentials.Username, ":\r\n") || strings.ContainsAny(credentials.Password, "\r\n") {
			return nil, errors.New("invalid registry credentials")
		}
		canonical, _ := json.Marshal(credentials)
		// Requiring canonical JSON also rejects duplicate or unknown fields.
		if string(data) != string(canonical)+"\n" {
			return nil, errors.New("registry credentials must use canonical username and password JSON")
		}
		auth = &authn.Basic{Username: credentials.Username, Password: credentials.Password}
	} else if write {
		return nil, errors.New("registry publication requires explicit credentials")
	}
	base := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 20 * time.Second, MaxResponseHeaderBytes: 32 << 10, MaxIdleConns: 4, MaxIdleConnsPerHost: 2, MaxConnsPerHost: 2, IdleConnTimeout: 30 * time.Second, DisableCompression: true}
	boundary := &registryBoundary{base: base, origin: origin, prefix: "/v2/" + repository.RepositoryStr() + "/", tokens: tokens, blobs: blobs, layer: record.Layer, config: record.Config}
	boundary.remaining.Store(2*record.Layer.Size + (4 << 20))
	scope := "pull"
	if write {
		scope = "push,pull"
	}
	boundary.scope = repository.Scope(scope)
	authenticated, err := registrytransport.NewWithContext(ctx, repository.Registry, auth, boundary, []string{repository.Scope(scope)})
	if err != nil {
		base.CloseIdleConnections()
		return nil, errors.New("registry authentication failed")
	}
	client := &http.Client{Transport: authenticated, Timeout: 90 * time.Second, CheckRedirect: func(next *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("registry redirect limit exceeded")
		}
		// The transport checks origin, credentials, method, TLS, and body limits.
		return nil
	}}
	return &registryConnection{repository: repository, transport: authenticated, client: client, close: base.CloseIdleConnections}, nil
}
