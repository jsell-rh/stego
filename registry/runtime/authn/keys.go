package authn

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// The bounds of the key set (ST-SUP-RP13-LC04, LC07, LC09, LC10).
const (
	// MaxDocumentBytes is the largest key document that the key set reads.
	MaxDocumentBytes = 64 << 10
	// MaxKeys is the most keys that a key document may hold.
	MaxKeys = 32
	// ECCoordinateBytes is the size of each coordinate of a P-256 key of ES256 (ST-SUP-RP13-LC22).
	ECCoordinateBytes = 32
	// MinKeyBits and MaxKeyBits bound the size of an RSA key.
	MinKeyBits = 2048
	MaxKeyBits = 8192
	// Cooldown is the least time between two key loads.
	Cooldown = 30 * time.Second
	// MaxAge is the age of the last load after which a token of a known key ID also refreshes the key set.
	MaxAge = 5 * time.Minute
	// OutageWindow is the time after the last successful load after which the key set denies every token.
	OutageWindow = 15 * time.Minute
	// LoadTimeout is the deadline of one key load.
	LoadTimeout = 5 * time.Second
)

// ErrKeys is the error of a key document or a key source that the key set refuses. The text of each refusal is
// fixed and holds no key ID, key document, source URL, file path or provider text (ST-SUP-RP13-LC12).
var ErrKeys = errors.New("authn: keys refused")

// ErrDiscovery is the error of a discovery document that the key set refuses at start; errors.Is(err, ErrKeys)
// holds for it too (ST-SUP-RP13-LC21).
var ErrDiscovery = errors.New("authn: discovery document refused")

type keyFault string

func (k keyFault) Error() string        { return "authn: keys refused: " + string(k) }
func (k keyFault) Is(target error) bool { return target == ErrKeys }

type discoveryFault string

func (d discoveryFault) Error() string        { return "authn: keys refused: " + string(d) }
func (d discoveryFault) Is(target error) bool { return target == ErrKeys || target == ErrDiscovery }

const (
	errDocumentSize = keyFault("document size")
	errDocumentJSON = keyFault("document JSON")
	errKeyCount     = keyFault("key count")
	errDocumentKID  = keyFault("key ID")
	errKeySize      = keyFault("key size")
	errExponent     = keyFault("key exponent")
	errNoKey        = keyFault("no signing key")
	errECKey        = keyFault("EC key")
	errLoad         = keyFault("load")
	errFile         = keyFault("file")
	errPEM          = keyFault("PEM key")
	// The refusals of a discovery document name it and hold no URL or provider text (ST-SUP-RP13-LC21).
	errDiscovery       = discoveryFault("discovery document")
	errDiscoveryIssuer = discoveryFault("discovery document issuer")
	errDiscoveryKeys   = discoveryFault("discovery document key set URL")

	errOutage = refusal("the key set is out of date")
)

// KeySource names where the key set loads its keys: exactly one of Issuer, URL, File and PEMFile. Generated code
// passes it from the declaration (ST-STD-RUNTIME-R05).
type KeySource struct {
	// Issuer is the HTTPS issuer URL of an OpenID Connect provider. The key set loads the discovery document
	// <issuer>/.well-known/openid-configuration once at start, under the rules of URL, refuses a document whose issuer
	// is not exactly Issuer or whose jwks_uri is not an HTTPS URL, and then loads the key document from that jwks_uri
	// as it loads URL. It never loads the discovery document again (ST-SUP-RP13-LC21).
	Issuer string
	// URL is the HTTPS URL of the key document, loaded over verified TLS 1.2 or later with no redirect and no proxy.
	URL string
	// CAFile is the path of a PEM file of certificate authorities that the key set adds to the system roots for the
	// URL, or for the discovery document and the key document of the Issuer; it never replaces them
	// (ST-SUP-RP13-LC08).
	CAFile string
	// File is the path of a regular file that holds the key document.
	File string
	// PEMFile is the path of a PEM file that holds one RSA public key, read once at start. With it, Verify accepts
	// only a token with no kid (ST-SUP-RP13-LC19).
	PEMFile string
	// Tracer records one span for each key load. Nil: no telemetry.
	Tracer trace.Tracer
}

// publicKey is one verification key and the one algorithm that it verifies: RS256 for an RSA key and ES256 for a
// P-256 key (ST-SUP-RP13-LC03, LC22).
type publicKey struct {
	alg string
	rsa *rsa.PublicKey
	ec  *ecdsa.PublicKey
}

// KeySet holds the verification keys of one key document and selects a key by its exact key ID. A key set of a
// source refreshes its cache when a token names an unknown key ID, at most one load at a time and at least Cooldown
// apart. It is safe for concurrent use. A zero KeySet denies every token.
type KeySet struct {
	mu        sync.Mutex
	ready     bool
	keys      map[string]publicKey
	single    *publicKey
	load      func(context.Context) ([]byte, error)
	source    string
	tracer    trace.Tracer
	now       func() time.Time
	loaded    time.Time
	attempted time.Time
	flight    chan struct{}
	closed    bool
	stop      context.Context
	cancel    context.CancelFunc
	transport *http.Transport
}

// keyOptions are the test seams of a key set: the clock, the dialer and the system roots.
type keyOptions struct {
	now   func() time.Time
	dial  func(ctx context.Context, network, address string) (net.Conn, error)
	roots func() (*x509.CertPool, error)
}

// ParseKeys gives a fixed key set of the key document, which it never refreshes (ST-SUP-RP13-LC04, LC07).
func ParseKeys(document []byte) (*KeySet, error) {
	keys, err := parseDocument(document)
	if err != nil {
		return nil, err
	}
	return &KeySet{ready: true, keys: keys, now: time.Now}, nil
}

// NewKeySet checks the source, loads the key document once and gives the key set. It refuses a URL that is not
// HTTPS (ST-SUP-RP13-LC08). Stop releases it.
func NewKeySet(ctx context.Context, source KeySource) (*KeySet, error) {
	return newKeySet(ctx, source, keyOptions{now: time.Now, roots: x509.SystemCertPool})
}

func newKeySet(ctx context.Context, source KeySource, options keyOptions) (*KeySet, error) {
	if ctx == nil {
		return nil, setting("the context is missing")
	}
	sources := 0
	for _, name := range []string{source.Issuer, source.URL, source.File, source.PEMFile} {
		if name != "" {
			sources++
		}
	}
	if sources != 1 {
		return nil, setting("the key source needs exactly one of an issuer, a URL, a key document file and a PEM file")
	}
	if source.CAFile != "" && source.URL == "" && source.Issuer == "" {
		return nil, setting("a CA file is only for a URL or an issuer")
	}
	if source.PEMFile != "" {
		data, err := readFile(source.PEMFile)
		if err != nil {
			return nil, errPEM
		}
		key, err := parsePEM(data)
		if err != nil {
			return nil, err
		}
		return &KeySet{ready: true, single: &publicKey{alg: AlgorithmRS256, rsa: key}, now: options.now}, nil
	}
	s := &KeySet{ready: true, now: options.now, tracer: source.Tracer}
	if s.tracer == nil {
		s.tracer = noop.NewTracerProvider().Tracer("")
	}
	s.stop, s.cancel = context.WithCancel(context.Background())
	if source.File != "" {
		file := source.File
		s.source = "file"
		s.load = func(context.Context) ([]byte, error) { return readFile(file) }
	} else {
		target := source.URL
		if source.Issuer != "" {
			if issuer, err := url.Parse(source.Issuer); err != nil || issuer.RawQuery != "" || issuer.ForceQuery {
				return nil, setting("the key source is not an HTTPS URL")
			}
			target = discoveryURL(source.Issuer)
		}
		if !httpsURL(target) {
			return nil, setting("the key source is not an HTTPS URL")
		}
		client, err := s.remoteClient(source.CAFile, options)
		if err != nil {
			return nil, err
		}
		s.source = "https"
		if source.Issuer != "" {
			// The discovery document names the key document once, at start (ST-SUP-RP13-LC21).
			s.source = "discovery"
			if target, err = s.discover(ctx, client, source.Issuer); err != nil {
				s.Stop()
				return nil, err
			}
		}
		s.load = func(ctx context.Context) ([]byte, error) { return fetch(ctx, client, target) }
	}
	s.mu.Lock()
	s.attempted = s.now()
	s.mu.Unlock()
	if err := s.refresh(ctx, true); err != nil {
		s.Stop()
		return nil, err
	}
	return s, nil
}

// httpsURL reports whether the text is an absolute HTTPS URL with a host and no user, fragment or opaque part.
func httpsURL(text string) bool {
	location, err := url.Parse(text)
	return err == nil && location.Scheme == "https" && location.Hostname() != "" && location.User == nil &&
		location.Fragment == "" && location.Opaque == "" && clean(text)
}

// remoteClient makes the HTTP client of a remote source: verified TLS 1.2 or later with the system roots and the
// certificates of the CA file added, no proxy, no redirect and bounded time and size (ST-SUP-RP13-LC08).
func (s *KeySet) remoteClient(caFile string, options keyOptions) (*http.Client, error) {
	system := options.roots
	if system == nil {
		system = x509.SystemCertPool
	}
	roots, err := system()
	if err != nil || roots == nil {
		return nil, setting("the system roots are missing")
	}
	if caFile != "" {
		data, err := readFile(caFile)
		roots = roots.Clone()
		if err != nil || !roots.AppendCertsFromPEM(data) {
			return nil, setting("the CA file is not valid")
		}
	}
	dial := (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	if options.dial != nil {
		dial = options.dial
	}
	s.transport = &http.Transport{
		Proxy:                  nil,
		DialContext:            dial,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		TLSHandshakeTimeout:    3 * time.Second,
		ResponseHeaderTimeout:  3 * time.Second,
		MaxResponseHeaderBytes: 8 << 10,
		MaxConnsPerHost:        1,
		MaxIdleConns:           1,
		MaxIdleConnsPerHost:    1,
		IdleConnTimeout:        30 * time.Second,
		DisableCompression:     true,
	}
	return &http.Client{Transport: s.transport, Timeout: LoadTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

// Stop denies every later token, drops the keys and cancels a key load that runs (ST-SUP-RP13-LC11). A repeat Stop
// and a Stop of a zero KeySet are safe.
func (s *KeySet) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closed = true
	s.keys = nil
	if s.cancel != nil {
		s.cancel()
	}
	transport := s.transport
	s.mu.Unlock()
	if transport != nil {
		transport.CloseIdleConnections()
	}
}

// lookup gives the key of the exact key ID, with no fallback to another key (ST-SUP-RP13-LC07). A key set of a PEM
// file gives its one key, only to a token that names no key ID (ST-SUP-RP13-LC19); a key document needs a key ID.
// A known key ID of a fresh cache needs no load. A known key ID of a cache older than MaxAge is answered at once,
// within OutageWindow, and starts one load unless one runs or the last attempt is less than Cooldown ago. An
// unknown key ID, or a cache older than OutageWindow, waits for the one load that runs or starts it, under the same
// cooldown (ST-SUP-RP13-LC09, LC10, LC15).
func (s *KeySet) lookup(ctx context.Context, kid string, hasKid bool) (publicKey, error) {
	if s == nil {
		return publicKey{}, errNotReady
	}
	s.mu.Lock()
	if !s.ready || s.now == nil {
		s.mu.Unlock()
		return publicKey{}, errNotReady
	}
	if s.closed {
		s.mu.Unlock()
		return publicKey{}, errStopped
	}
	if s.single != nil {
		key := *s.single
		s.mu.Unlock()
		if hasKid {
			return publicKey{}, errKeyID
		}
		return key, nil
	}
	if !hasKid {
		s.mu.Unlock()
		return publicKey{}, errKeyID
	}
	key, known := s.keys[kid]
	if s.load == nil {
		s.mu.Unlock()
		if !known {
			return publicKey{}, errUnknownKey
		}
		return key, nil
	}
	now := s.now()
	age := now.Sub(s.loaded)
	if known && age < MaxAge {
		s.mu.Unlock()
		return key, nil
	}
	cooling := now.Sub(s.attempted) < Cooldown
	if known && age < OutageWindow {
		if s.flight == nil && !cooling {
			s.start(ctx, now)
		}
		s.mu.Unlock()
		return key, nil
	}
	flight := s.flight
	if flight == nil {
		if cooling {
			s.mu.Unlock()
			if !known {
				return publicKey{}, errUnknownKey
			}
			return publicKey{}, errOutage
		}
		flight = s.start(ctx, now)
	}
	s.mu.Unlock()
	select {
	case <-flight:
	case <-ctx.Done():
		return publicKey{}, errContext
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return publicKey{}, errStopped
	}
	key, known = s.keys[kid]
	if !known {
		return publicKey{}, errUnknownKey
	}
	if s.now().Sub(s.loaded) >= OutageWindow {
		return publicKey{}, errOutage
	}
	return key, nil
}

// start starts the one load of a refresh. The caller holds the lock.
func (s *KeySet) start(ctx context.Context, now time.Time) chan struct{} {
	flight := make(chan struct{})
	s.flight = flight
	s.attempted = now
	go s.run(ctx, flight)
	return flight
}

// run is the one load of a refresh. Its deadline and cancellation come from the key set, not from the request that
// started it, so the requests that wait for it share one result.
func (s *KeySet) run(parent context.Context, flight chan struct{}) {
	_ = s.refresh(parent, false)
	s.mu.Lock()
	s.flight = nil
	s.mu.Unlock()
	close(flight)
}

// refresh loads and checks the key document and replaces the keys when it is valid. The span it records holds the
// source kind and the outcome only. The first load is also cancelled with the context of its caller.
func (s *KeySet) refresh(parent context.Context, first bool) error {
	ctx, cancel := context.WithTimeout(s.stop, LoadTimeout)
	defer cancel()
	if first {
		defer context.AfterFunc(parent, cancel)()
	}
	ctx = trace.ContextWithSpanContext(ctx, trace.SpanContextFromContext(parent))
	ctx, span := s.tracer.Start(ctx, "authn.keys.refresh", trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("authn.keys.source", s.source)))
	data, err := s.load(ctx)
	var keys map[string]publicKey
	if err == nil {
		keys, err = parseDocument(data)
	}
	s.mu.Lock()
	switch {
	case s.closed || errors.Is(ctx.Err(), context.Canceled):
		err = errLoad
		span.SetAttributes(attribute.String("authn.keys.outcome", "canceled"))
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		err = errLoad
		span.SetAttributes(attribute.String("authn.keys.outcome", "deadline"))
	case err != nil:
		span.SetAttributes(attribute.String("authn.keys.outcome", "failure"))
	default:
		s.keys = keys
		s.loaded = s.now()
		span.SetAttributes(attribute.String("authn.keys.outcome", "success"))
	}
	s.mu.Unlock()
	if err != nil {
		span.SetStatus(codes.Error, "")
	}
	span.End()
	return err
}

// fetch loads the key document from the URL. Every error is errLoad or a key document refusal, with no URL and no
// response text.
func fetch(ctx context.Context, client *http.Client, target string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, errLoad
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, errLoad
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > MaxDocumentBytes {
		return nil, errLoad
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxDocumentBytes+1))
	if err != nil {
		return nil, errLoad
	}
	if len(data) > MaxDocumentBytes {
		return nil, errDocumentSize
	}
	return data, nil
}

// readFile reads the key document from a regular file of at most MaxDocumentBytes.
func readFile(name string) ([]byte, error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, errFile
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errFile
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxDocumentBytes+1))
	if err != nil {
		return nil, errFile
	}
	if len(data) > MaxDocumentBytes {
		return nil, errDocumentSize
	}
	return data, nil
}

// parseDocument reads a key document by its exact member names, with no folding of case: at most MaxDocumentBytes,
// at most MaxKeys keys, each key ID once, each RSA signing key of MinKeyBits to MaxKeyBits with the exponent 65537,
// and each P-256 signing key a point of the curve. A key of another type, curve or use is skipped
// (ST-SUP-RP13-LC04, LC07, LC22).
func parseDocument(document []byte) (map[string]publicKey, error) {
	if len(document) == 0 || len(document) > MaxDocumentBytes {
		return nil, errDocumentSize
	}
	if checkJSON(document) != nil {
		return nil, errDocumentJSON
	}
	top, err := members(document)
	if err != nil {
		return nil, errDocumentJSON
	}
	var set []json.RawMessage
	if raw, ok := top["keys"]; !ok || len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &set) != nil {
		return nil, errDocumentJSON
	}
	if len(set) == 0 || len(set) > MaxKeys {
		return nil, errKeyCount
	}
	seen := make(map[string]bool, len(set))
	keys := make(map[string]publicKey, len(set))
	for _, raw := range set {
		key, err := members(raw)
		if err != nil {
			return nil, errDocumentJSON
		}
		kid, hasKid := stringMember(key, "kid")
		if _, present := key["kid"]; present {
			if !hasKid || kid == "" || len(kid) > maxKeyIDBytes || seen[kid] {
				return nil, errDocumentKID
			}
			seen[kid] = true
		}
		alg := signingKey(key)
		if alg == "" {
			continue
		}
		if !hasKid {
			return nil, errDocumentKID
		}
		public := publicKey{alg: alg}
		if alg == AlgorithmES256 {
			public.ec, err = ecKey(key)
		} else {
			public.rsa, err = rsaKey(key)
		}
		if err != nil {
			return nil, err
		}
		keys[kid] = public
	}
	if len(keys) == 0 {
		return nil, errNoKey
	}
	return keys, nil
}

// signingKey gives the one algorithm of a signing key, or empty for a key that the key set skips: RS256 for kty RSA
// and ES256 for kty EC with crv P-256, each with alg absent or that same algorithm, use sig or absent, and key_ops
// absent or exactly verify (ST-SUP-RP13-LC22).
func signingKey(key map[string]json.RawMessage) string {
	kty, ok := stringMember(key, "kty")
	if !ok {
		return ""
	}
	var alg string
	switch kty {
	case "RSA":
		alg = AlgorithmRS256
	case "EC":
		if crv, ok := stringMember(key, "crv"); !ok || crv != "P-256" {
			return ""
		}
		alg = AlgorithmES256
	default:
		return ""
	}
	if raw, present := key["alg"]; present {
		if named, ok := stringMember(key, "alg"); !ok || named != alg || len(raw) == 0 {
			return ""
		}
	}
	if _, present := key["use"]; present {
		if use, ok := stringMember(key, "use"); !ok || use != "sig" {
			return ""
		}
	}
	if raw, present := key["key_ops"]; present {
		var ops []string
		if json.Unmarshal(raw, &ops) != nil || len(ops) != 1 || ops[0] != "verify" {
			return ""
		}
	}
	return alg
}

// ecKey reads the x and y members of a P-256 key: each exactly ECCoordinateBytes of base64url, and together a point
// of the curve (ST-SUP-RP13-LC22).
func ecKey(key map[string]json.RawMessage) (*ecdsa.PublicKey, error) {
	x64, _ := stringMember(key, "x")
	y64, _ := stringMember(key, "y")
	x, errX := base64.RawURLEncoding.Strict().DecodeString(x64)
	y, errY := base64.RawURLEncoding.Strict().DecodeString(y64)
	if errX != nil || errY != nil || len(x) != ECCoordinateBytes || len(y) != ECCoordinateBytes {
		return nil, errECKey
	}
	point := append(append([]byte{4}, x...), y...)
	public, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), point)
	if err != nil {
		return nil, errECKey
	}
	return public, nil
}

// rsaKey reads the n and e members of an RSA key within the bounds.
func rsaKey(key map[string]json.RawMessage) (*rsa.PublicKey, error) {
	e64, _ := stringMember(key, "e")
	e, err := base64.RawURLEncoding.Strict().DecodeString(e64)
	if err != nil || string(e) != "\x01\x00\x01" {
		return nil, errExponent
	}
	n64, _ := stringMember(key, "n")
	n, err := base64.RawURLEncoding.Strict().DecodeString(n64)
	if err != nil || len(n) == 0 || n[0] == 0 {
		return nil, errKeySize
	}
	return boundedKey(new(big.Int).SetBytes(n), 65537)
}

// boundedKey refuses a modulus outside MinKeyBits to MaxKeyBits, an even modulus, and an exponent other than 65537.
func boundedKey(modulus *big.Int, exponent int) (*rsa.PublicKey, error) {
	if exponent != 65537 {
		return nil, errExponent
	}
	if bits := modulus.BitLen(); bits < MinKeyBits || bits > MaxKeyBits || modulus.Bit(0) == 0 {
		return nil, errKeySize
	}
	return &rsa.PublicKey{N: modulus, E: exponent}, nil
}

// parsePEM reads one PEM block of an RSA public key, PUBLIC KEY or RSA PUBLIC KEY, with nothing after it.
func parsePEM(data []byte) (*rsa.PublicKey, error) {
	block, rest := pem.Decode(data)
	if block == nil || len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errPEM
	}
	var public *rsa.PublicKey
	switch block.Type {
	case "PUBLIC KEY":
		key, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, errPEM
		}
		rsaKey, ok := key.(*rsa.PublicKey)
		if !ok {
			return nil, errPEM
		}
		public = rsaKey
	case "RSA PUBLIC KEY":
		key, err := x509.ParsePKCS1PublicKey(block.Bytes)
		if err != nil {
			return nil, errPEM
		}
		public = key
	default:
		return nil, errPEM
	}
	return boundedKey(new(big.Int).Set(public.N), public.E)
}
