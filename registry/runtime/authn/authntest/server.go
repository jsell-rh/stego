// Package authntest is the local key server and token minter of the authentication runtime, for tests only
// (ST-SUP-RP13-LC16). The key server serves a key document over TLS on 127.0.0.1 with a test certificate authority
// that it makes, and stops when the test ends. The minter signs the header and the claims that a test gives with
// the key that the test gives, also when they are not valid. No hosted identity provider and no network is used.
//
// Only a test file may import this package.
package authntest

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Host is the DNS name that the server certificate holds beside 127.0.0.1. It resolves nowhere: a test that dials
// it must map it to the server address itself.
const Host = "keys.authn.test"

// Server is a local key server: an HTTPS server on 127.0.0.1 with a certificate of its own test certificate
// authority. It serves the key document that the test sets, or the failure, redirect or hang that the test asks for.
type Server struct {
	server   *httptest.Server
	roots    *x509.CertPool
	requests atomic.Int64
	ended    atomic.Int64
	closing  chan struct{}
	once     sync.Once

	mu       sync.Mutex
	document []byte
	status   int
	redirect string
	hang     bool
	arrived  chan struct{}
}

// NewServer starts a key server that serves an empty key set, and stops it when the test ends.
func NewServer(t testing.TB) *Server {
	t.Helper()
	certificate, roots := testAuthority(t)
	s := &Server{roots: roots, closing: make(chan struct{}), document: Document(), status: http.StatusOK,
		arrived: make(chan struct{}, 64)}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("authntest: listen on 127.0.0.1: %v", err)
	}
	s.server = httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	_ = s.server.Listener.Close()
	s.server.Listener = listener
	s.server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	s.server.StartTLS()
	t.Cleanup(s.Close)
	return s
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.requests.Add(1)
	s.mu.Lock()
	document, status, redirect, hang := s.document, s.status, s.redirect, s.hang
	s.mu.Unlock()
	select {
	case s.arrived <- struct{}{}:
	default:
	}
	switch {
	case hang:
		select {
		case <-r.Context().Done():
			s.ended.Add(1)
		case <-s.closing:
		}
		return
	case redirect != "" && r.URL.Path == "/keys":
		http.Redirect(w, r, redirect, http.StatusFound)
		return
	case status != http.StatusOK:
		http.Error(w, "authntest failure", status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(document)
}

// URL is the HTTPS URL of the key document, with the address 127.0.0.1.
func (s *Server) URL() string { return s.server.URL + "/keys" }

// OtherURL is a second URL of the same server that serves the key document too, also while Redirect holds, as the
// target of a redirect.
func (s *Server) OtherURL() string { return s.server.URL + "/other" }

// Addr is the host and port that the server listens on.
func (s *Server) Addr() string { return s.server.Listener.Addr().String() }

// Roots holds only the test certificate authority of the server.
func (s *Server) Roots() *x509.CertPool { return s.roots }

// Requests is the number of requests that the server got.
func (s *Server) Requests() int { return int(s.requests.Load()) }

// Abandoned is the number of held requests that the client gave up before the server stopped.
func (s *Server) Abandoned() int { return int(s.ended.Load()) }

// Arrived receives one value for each request that reaches the server (up to 64 not yet received).
func (s *Server) Arrived() <-chan struct{} { return s.arrived }

// SetDocument makes the server serve the document as it is, also when it is not valid.
func (s *Server) SetDocument(document []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.document, s.status, s.redirect, s.hang = bytes.Clone(document), http.StatusOK, "", false
}

// Publish makes the server serve the key document of the keys.
func (s *Server) Publish(keys ...*Key) {
	jwks := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		jwks = append(jwks, key.JWK())
	}
	s.SetDocument(Document(jwks...))
}

// Fail makes the server answer each request with the status and a body of its own text.
func (s *Server) Fail(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status, s.redirect, s.hang = status, "", false
}

// Redirect makes the server answer each request for URL with a redirect to the location.
func (s *Server) Redirect(location string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.redirect, s.hang = location, false
}

// Hang makes the server hold each request with no answer until the client goes away or the server stops.
func (s *Server) Hang() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hang = true
}

// Close stops the server. A repeat Close is safe.
func (s *Server) Close() {
	s.once.Do(func() {
		close(s.closing)
		s.server.Close()
	})
}

// testAuthority makes a certificate authority and a server certificate for 127.0.0.1 and Host, signed by it.
func testAuthority(t testing.TB) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("authntest: make the authority key: %v", err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "authntest authority"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true, MaxPathLenZero: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("authntest: make the authority certificate: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("authntest: read the authority certificate: %v", err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("authntest: make the server key: %v", err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "authntest server"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, DNSNames: []string{Host},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("authntest: make the server certificate: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	return tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}, roots
}

// Key is an RSA signing key with its key ID.
type Key struct {
	ID      string
	Private *rsa.PrivateKey
}

// NewKey makes a 2048-bit RSA key with the key ID.
func NewKey(t testing.TB, id string) *Key {
	t.Helper()
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("authntest: make an RSA key: %v", err)
	}
	return &Key{ID: id, Private: private}
}

// JWK is the public key as a JSON Web Key member of a key document.
func (k *Key) JWK() map[string]any { return RSAJWK(k.ID, &k.Private.PublicKey) }

// PublicPEM is the public key in a PEM block, as a key file holds it.
func (k *Key) PublicPEM() []byte {
	der, err := x509.MarshalPKIXPublicKey(&k.Private.PublicKey)
	if err != nil {
		panic("authntest: encode the public key: " + err.Error())
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// RSAJWK is the JSON Web Key of an RSA public key with the key ID, for use sig and alg RS256.
func RSAJWK(id string, public *rsa.PublicKey) map[string]any {
	return map[string]any{
		"kid": id, "kty": "RSA", "alg": "RS256", "use": "sig",
		"n": base64.RawURLEncoding.EncodeToString(public.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(public.E)).Bytes()),
	}
}

// Document is the key document that holds the keys.
func Document(keys ...map[string]any) []byte {
	if keys == nil {
		keys = []map[string]any{}
	}
	data, err := json.Marshal(map[string]any{"keys": keys})
	if err != nil {
		panic("authntest: encode the key document: " + err.Error())
	}
	return data
}

// Claims are the claims of a valid token of the issuer and the audience for the subject, issued now and expiring in
// five minutes. A test changes them to make a token that is not valid.
func Claims(issuer, audience, subject string, now time.Time) map[string]any {
	return map[string]any{
		"iss": issuer, "aud": audience, "sub": subject,
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
	}
}

// Header is the header of an RS256 token with the key ID.
func Header(kid string) map[string]any {
	return map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}
}

// Mint makes a compact token of the header and the claims, signed with the key by the alg member of the header.
// The header and the claims are raw JSON bytes, used as they are, or a value that Mint encodes as JSON. The key is
// a *Key or an *rsa.PrivateKey for RS256 and PS256, an *ecdsa.PrivateKey for ES256, a []byte secret for HS256, and
// nil for none. Mint signs with the key it is given whatever key ID the header names.
func Mint(t testing.TB, header, claims any, key any) string {
	t.Helper()
	head, err := encode(header)
	if err != nil {
		t.Fatalf("authntest: encode the header: %v", err)
	}
	body, err := encode(claims)
	if err != nil {
		t.Fatalf("authntest: encode the claims: %v", err)
	}
	var alg struct {
		Alg string `json:"alg"`
	}
	_ = json.Unmarshal(head, &alg)
	input := base64.RawURLEncoding.EncodeToString(head) + "." + base64.RawURLEncoding.EncodeToString(body)
	signature, err := sign(alg.Alg, []byte(input), key)
	if err != nil {
		t.Fatalf("authntest: sign the token: %v", err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func encode(value any) ([]byte, error) {
	if raw, ok := value.([]byte); ok {
		return raw, nil
	}
	return json.Marshal(value)
}

type signError string

func (e signError) Error() string { return string(e) }

func sign(alg string, input []byte, key any) ([]byte, error) {
	if k, ok := key.(*Key); ok {
		key = k.Private
	}
	digest := sha256.Sum256(input)
	switch alg {
	case "none":
		return nil, nil
	case "RS256", "PS256":
		private, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, signError(alg + " needs an RSA key")
		}
		if alg == "PS256" {
			return rsa.SignPSS(rand.Reader, private, crypto.SHA256, digest[:], nil)
		}
		return rsa.SignPKCS1v15(nil, private, crypto.SHA256, digest[:])
	case "ES256":
		private, ok := key.(*ecdsa.PrivateKey)
		if !ok {
			return nil, signError("ES256 needs an ECDSA key")
		}
		r, s, err := ecdsa.Sign(rand.Reader, private, digest[:])
		if err != nil {
			return nil, err
		}
		signature := make([]byte, 64)
		r.FillBytes(signature[:32])
		s.FillBytes(signature[32:])
		return signature, nil
	case "HS256":
		secret, ok := key.([]byte)
		if !ok {
			return nil, signError("HS256 needs a secret")
		}
		mac := hmac.New(sha256.New, secret)
		mac.Write(input)
		return mac.Sum(nil), nil
	}
	return nil, signError("unknown alg " + alg)
}
