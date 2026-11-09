package authn

import (
	"context"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
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

type keyFault string

func (k keyFault) Error() string        { return "authn: keys refused: " + string(k) }
func (k keyFault) Is(target error) bool { return target == ErrKeys }

const (
	errDocumentSize = keyFault("document size")
	errDocumentJSON = keyFault("document JSON")
	errKeyCount     = keyFault("key count")
	errDocumentKID  = keyFault("key ID")
	errKeySize      = keyFault("key size")
	errExponent     = keyFault("key exponent")
	errNoKey        = keyFault("no RSA signing key")
	errLoad         = keyFault("load")
	errFile         = keyFault("file")

	errOutage = refusal("the key set is out of date")
)

// KeySource names where the key set loads its key document: exactly one of URL and File. Generated code passes it
// from the declaration (ST-STD-RUNTIME-R05).
type KeySource struct {
	// URL is the HTTPS URL of the key document, loaded over verified TLS 1.2 or later with no redirect and no proxy.
	URL string
	// File is the path of a regular file that holds the key document.
	File string
	// Roots are the certificate authorities that the URL's server certificate must chain to. Nil: the system roots.
	Roots *x509.CertPool
	// Tracer records one span for each key load. Nil: no telemetry.
	Tracer trace.Tracer
}

// KeySet holds the RSA verification keys of one key document and selects a key by its exact key ID. A key set of a
// source refreshes its cache when a token names an unknown key ID, at most one load at a time and at least Cooldown
// apart. It is safe for concurrent use. A zero KeySet denies every token.
type KeySet struct {
	mu        sync.Mutex
	ready     bool
	keys      map[string]*rsa.PublicKey
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

// keyOptions are the test seams of a key set: the clock and the dialer.
type keyOptions struct {
	now  func() time.Time
	dial func(ctx context.Context, network, address string) (net.Conn, error)
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
	return newKeySet(ctx, source, keyOptions{now: time.Now})
}

func newKeySet(ctx context.Context, source KeySource, options keyOptions) (*KeySet, error) {
	if ctx == nil {
		return nil, setting("the context is missing")
	}
	if (source.URL == "") == (source.File == "") {
		return nil, setting("the key source needs exactly one of a URL and a file")
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
		location, err := url.Parse(source.URL)
		if err != nil || location.Scheme != "https" || location.Hostname() == "" || location.User != nil ||
			location.Fragment != "" || location.Opaque != "" {
			return nil, setting("the key source is not an HTTPS URL")
		}
		roots := source.Roots
		if roots == nil {
			if roots, err = x509.SystemCertPool(); err != nil {
				return nil, setting("the system roots are missing")
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
		client := &http.Client{Transport: s.transport, Timeout: LoadTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		target := location.String()
		s.source = "https"
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

// lookup gives the key of the exact key ID, with no fallback to another key (ST-SUP-RP13-LC07). A known key ID of a
// fresh cache needs no load; an unknown key ID, or a cache older than MaxAge, starts one load or waits for the one
// that runs, unless the last attempt is less than Cooldown ago (ST-SUP-RP13-LC09, LC15). No key is given
// OutageWindow after the last successful load (ST-SUP-RP13-LC10).
func (s *KeySet) lookup(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	if s == nil {
		return nil, errNotReady
	}
	s.mu.Lock()
	if !s.ready || s.now == nil {
		s.mu.Unlock()
		return nil, errNotReady
	}
	if s.closed {
		s.mu.Unlock()
		return nil, errStopped
	}
	key := s.keys[kid]
	if s.load == nil {
		s.mu.Unlock()
		if key == nil {
			return nil, errUnknownKey
		}
		return key, nil
	}
	now := s.now()
	if key != nil && now.Sub(s.loaded) < MaxAge {
		s.mu.Unlock()
		return key, nil
	}
	flight := s.flight
	if flight == nil {
		if now.Sub(s.attempted) < Cooldown {
			fresh := now.Sub(s.loaded) < OutageWindow
			s.mu.Unlock()
			switch {
			case key == nil:
				return nil, errUnknownKey
			case !fresh:
				return nil, errOutage
			}
			return key, nil
		}
		flight = make(chan struct{})
		s.flight = flight
		s.attempted = now
		go s.run(ctx, flight)
	}
	s.mu.Unlock()
	select {
	case <-flight:
	case <-ctx.Done():
		return nil, errContext
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errStopped
	}
	key = s.keys[kid]
	if key == nil {
		return nil, errUnknownKey
	}
	if s.now().Sub(s.loaded) >= OutageWindow {
		return nil, errOutage
	}
	return key, nil
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
	var keys map[string]*rsa.PublicKey
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

// jwk is one member of the keys array of a key document.
type jwk struct {
	KeyID      string   `json:"kid"`
	Type       string   `json:"kty"`
	Algorithm  string   `json:"alg"`
	Use        string   `json:"use"`
	Operations []string `json:"key_ops"`
	N          string   `json:"n"`
	E          string   `json:"e"`
}

// parseDocument reads a key document: at most MaxDocumentBytes, at most MaxKeys keys, each key ID once, and each
// RSA signing key of MinKeyBits to MaxKeyBits with the exponent 65537. A key of another type or use is skipped.
func parseDocument(document []byte) (map[string]*rsa.PublicKey, error) {
	if len(document) == 0 || len(document) > MaxDocumentBytes {
		return nil, errDocumentSize
	}
	if checkJSON(document) != nil {
		return nil, errDocumentJSON
	}
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if json.Unmarshal(document, &set) != nil || set.Keys == nil {
		return nil, errDocumentJSON
	}
	if len(set.Keys) == 0 || len(set.Keys) > MaxKeys {
		return nil, errKeyCount
	}
	seen := make(map[string]bool, len(set.Keys))
	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, raw := range set.Keys {
		var key jwk
		if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &key) != nil {
			return nil, errDocumentJSON
		}
		if key.KeyID == "" || len(key.KeyID) > maxKeyIDBytes || seen[key.KeyID] {
			return nil, errDocumentKID
		}
		seen[key.KeyID] = true
		if key.Type != "RSA" || (key.Algorithm != "" && key.Algorithm != Algorithm) ||
			(key.Use != "" && key.Use != "sig") ||
			(key.Operations != nil && (len(key.Operations) != 1 || key.Operations[0] != "verify")) {
			continue
		}
		e, err := base64.RawURLEncoding.Strict().DecodeString(key.E)
		if err != nil || string(e) != "\x01\x00\x01" {
			return nil, errExponent
		}
		n, err := base64.RawURLEncoding.Strict().DecodeString(key.N)
		if err != nil || len(n) == 0 || n[0] == 0 {
			return nil, errKeySize
		}
		modulus := new(big.Int).SetBytes(n)
		if bits := modulus.BitLen(); bits < MinKeyBits || bits > MaxKeyBits || modulus.Bit(0) == 0 {
			return nil, errKeySize
		}
		keys[key.KeyID] = &rsa.PublicKey{N: modulus, E: 65537}
	}
	if len(keys) == 0 {
		return nil, errNoKey
	}
	return keys, nil
}
