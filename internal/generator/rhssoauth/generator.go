// Package rhssoauth implements the rh-sso-auth component Generator. It produces
// production-grade JWT authentication middleware with JWK key discovery, RSA
// signature verification, claim extraction with fallback chains, and
// configurable public paths following the rh-trex pattern.
package rhssoauth

import (
	"bytes"
	"fmt"
	"go/format"
	"path"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
)

// Generator produces the rh-sso-auth component's generated code.
type Generator struct{}

// Generate produces Go files for JWT authentication middleware and identity
// context helpers. Returns wiring instructions for main.go assembly.
func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	ns := ctx.OutputNamespace
	if ns == "" {
		ns = "internal/auth"
	}
	pkg := path.Base(ns)

	// Read component config.
	jwkCertURL := "https://sso.redhat.com/auth/realms/redhat-external/protocol/openid-connect/certs"
	if u, ok := ctx.ComponentConfig["jwk_cert_url"]; ok {
		if s, ok := u.(string); ok && s != "" {
			jwkCertURL = s
		}
	}

	var jwkCertFile string
	if f, ok := ctx.ComponentConfig["jwk_cert_file"]; ok {
		if s, ok := f.(string); ok && s != "" {
			jwkCertFile = s
		}
	}

	var publicPaths []string
	if pp, ok := ctx.ComponentConfig["public_paths"]; ok {
		if list, ok := pp.([]any); ok {
			for _, item := range list {
				if s, ok := item.(string); ok {
					publicPaths = append(publicPaths, s)
				}
			}
		}
	}
	if len(publicPaths) == 0 {
		publicPaths = []string{"/healthcheck", "/metrics"}
	}

	middlewareFile, err := generateMiddleware(ns, pkg, jwkCertURL, jwkCertFile, publicPaths, ctx.ServiceName, ctx.ErrorTypeBase, ctx.BasePath)
	if err != nil {
		return nil, nil, err
	}

	contextFile, err := generateContext(ns, pkg)
	if err != nil {
		return nil, nil, err
	}

	files := []gen.File{middlewareFile, contextFile}

	// Build the constructor expression, chaining .WithKeysFile() if configured.
	constructorExpr := fmt.Sprintf("%s.NewJWTHandler()", pkg)
	if jwkCertFile != "" {
		constructorExpr += fmt.Sprintf(".WithKeysFile(%q)", jwkCertFile)
	}
	constructorExpr += ".Build()"

	middlewareIdx := 0
	wiring := &gen.Wiring{
		Imports:               []string{ns},
		Constructors:          []string{constructorExpr},
		MiddlewareConstructor: &middlewareIdx,
		MiddlewareWrapExpr:    "%s(%s)",
		GoModRequires: map[string]string{
			"github.com/golang-jwt/jwt/v4": "v4.5.1",
		},
	}

	if err := gen.ValidateNamespace(ns, files); err != nil {
		return nil, nil, err
	}

	return files, wiring, nil
}

// deriveErrorPrefix converts a service name to an error code prefix by
// removing hyphens and uppercasing. For example, "hyperfleet-api" becomes
// "HYPERFLEET".
func deriveErrorPrefix(serviceName string) string {
	name := serviceName
	// Remove trailing "-api" or "-service" suffixes for cleaner prefixes.
	name = strings.TrimSuffix(name, "-api")
	name = strings.TrimSuffix(name, "-service")
	return strings.ToUpper(strings.ReplaceAll(name, "-", ""))
}

// generateMiddleware produces the middleware.go file containing the JWTHandler
// struct with builder pattern, JWK key management, RSA validation, and public
// path bypass.
func generateMiddleware(ns, pkg, jwkCertURL, jwkCertFile string, publicPaths []string, serviceName, errorTypeBase, basePath string) (gen.File, error) {
	var buf bytes.Buffer

	errorPrefix := deriveErrorPrefix(serviceName)
	errorTypeURI := "about:blank"
	if errorTypeBase != "" {
		errorTypeURI = errorTypeBase + "unauthorized"
	}

	// Also add the openapi path as always-public.
	openapiPath := "/openapi"
	if basePath != "" {
		openapiPath = basePath + "/openapi"
	}

	fmt.Fprintf(&buf, "package %s\n\n", pkg)
	fmt.Fprintf(&buf, "import (\n")
	fmt.Fprintf(&buf, "\t\"context\"\n")
	fmt.Fprintf(&buf, "\t\"crypto/rsa\"\n")
	fmt.Fprintf(&buf, "\t\"encoding/base64\"\n")
	fmt.Fprintf(&buf, "\t\"encoding/json\"\n")
	fmt.Fprintf(&buf, "\t\"fmt\"\n")
	fmt.Fprintf(&buf, "\t\"io\"\n")
	fmt.Fprintf(&buf, "\t\"math/big\"\n")
	fmt.Fprintf(&buf, "\t\"net/http\"\n")
	fmt.Fprintf(&buf, "\t\"os\"\n")
	fmt.Fprintf(&buf, "\t\"strings\"\n")
	fmt.Fprintf(&buf, "\t\"sync\"\n")
	fmt.Fprintf(&buf, "\t\"time\"\n")
	fmt.Fprintf(&buf, "\n")
	fmt.Fprintf(&buf, "\tjwt \"github.com/golang-jwt/jwt/v4\"\n")
	fmt.Fprintf(&buf, ")\n\n")

	// --- JWK types ---
	buf.WriteString("// jwkSet represents a JSON Web Key Set response.\n")
	buf.WriteString("type jwkSet struct {\n")
	buf.WriteString("\tKeys []jwk `json:\"keys\"`\n")
	buf.WriteString("}\n\n")

	buf.WriteString("// jwk represents a single JSON Web Key.\n")
	buf.WriteString("type jwk struct {\n")
	buf.WriteString("\tKid string `json:\"kid\"`\n")
	buf.WriteString("\tKty string `json:\"kty\"`\n")
	buf.WriteString("\tN   string `json:\"n\"`\n")
	buf.WriteString("\tE   string `json:\"e\"`\n")
	buf.WriteString("}\n\n")

	// --- JWTHandler ---
	buf.WriteString("// JWTHandler manages JWK key discovery, caching, and JWT validation.\n")
	buf.WriteString("type JWTHandler struct {\n")
	buf.WriteString("\tkeysURL     string\n")
	buf.WriteString("\tkeysFile    string\n")
	buf.WriteString("\tpublicPaths map[string]bool\n")
	buf.WriteString("\tkeys        map[string]*rsa.PublicKey\n")
	buf.WriteString("\tmu          sync.RWMutex\n")
	buf.WriteString("\tstopCh      chan struct{}\n")
	buf.WriteString("\tlastRefresh time.Time\n")
	buf.WriteString("}\n\n")

	// --- Builder pattern ---
	buf.WriteString("// NewJWTHandler creates a new JWTHandler with default configuration.\n")
	buf.WriteString("// Use the builder methods to customize, then call Build().\n")
	buf.WriteString("func NewJWTHandler() *JWTHandler {\n")
	buf.WriteString("\treturn &JWTHandler{\n")
	fmt.Fprintf(&buf, "\t\tkeysURL: %q,\n", jwkCertURL)
	if jwkCertFile != "" {
		fmt.Fprintf(&buf, "\t\tkeysFile: %q,\n", jwkCertFile)
	}
	buf.WriteString("\t\tpublicPaths: map[string]bool{\n")
	for _, p := range publicPaths {
		fullPath := p
		if basePath != "" && !strings.HasPrefix(p, basePath) {
			fullPath = basePath + p
		}
		fmt.Fprintf(&buf, "\t\t\t%q: true,\n", fullPath)
	}
	fmt.Fprintf(&buf, "\t\t\t%q: true,\n", openapiPath)
	buf.WriteString("\t\t},\n")
	buf.WriteString("\t\tkeys:   make(map[string]*rsa.PublicKey),\n")
	buf.WriteString("\t\tstopCh: make(chan struct{}),\n")
	buf.WriteString("\t}\n")
	buf.WriteString("}\n\n")

	buf.WriteString("// WithKeysURL sets the JWK endpoint URL for key discovery.\n")
	buf.WriteString("func (h *JWTHandler) WithKeysURL(url string) *JWTHandler {\n")
	buf.WriteString("\th.keysURL = url\n")
	buf.WriteString("\treturn h\n")
	buf.WriteString("}\n\n")

	buf.WriteString("// WithKeysFile sets the local JWK file path for key discovery.\n")
	buf.WriteString("// When set, takes priority over the URL source.\n")
	buf.WriteString("func (h *JWTHandler) WithKeysFile(path string) *JWTHandler {\n")
	buf.WriteString("\th.keysFile = path\n")
	buf.WriteString("\treturn h\n")
	buf.WriteString("}\n\n")

	buf.WriteString("// WithPublicPath adds a path that bypasses authentication.\n")
	buf.WriteString("// Path matching is exact (no prefix matching).\n")
	buf.WriteString("func (h *JWTHandler) WithPublicPath(path string) *JWTHandler {\n")
	buf.WriteString("\th.publicPaths[path] = true\n")
	buf.WriteString("\treturn h\n")
	buf.WriteString("}\n\n")

	// --- Build ---
	buf.WriteString("// Build initializes the key cache and starts the background refresh\n")
	buf.WriteString("// goroutine. Returns the middleware function.\n")
	buf.WriteString("// When AUTH_ENABLED=false, returns a passthrough middleware without starting\n")
	buf.WriteString("// the refresh goroutine.\n")
	buf.WriteString("func (h *JWTHandler) Build() func(http.Handler) http.Handler {\n")
	buf.WriteString("\t// Read runtime configuration from environment variables.\n")
	buf.WriteString("\t// JWK_CERT_URL overrides the config default.\n")
	buf.WriteString("\tif url := os.Getenv(\"JWK_CERT_URL\"); url != \"\" {\n")
	buf.WriteString("\t\th.keysURL = url\n")
	buf.WriteString("\t}\n")
	buf.WriteString("\t// JWK_CERT_FILE overrides URL if set.\n")
	buf.WriteString("\tif file := os.Getenv(\"JWK_CERT_FILE\"); file != \"\" {\n")
	buf.WriteString("\t\th.keysFile = file\n")
	buf.WriteString("\t}\n\n")
	buf.WriteString("\t// AUTH_ENABLED=false disables auth entirely (development mode).\n")
	buf.WriteString("\t// Return a passthrough middleware without starting key refresh.\n")
	buf.WriteString("\tif strings.EqualFold(os.Getenv(\"AUTH_ENABLED\"), \"false\") {\n")
	buf.WriteString("\t\treturn func(next http.Handler) http.Handler {\n")
	buf.WriteString("\t\t\treturn next\n")
	buf.WriteString("\t\t}\n")
	buf.WriteString("\t}\n\n")
	buf.WriteString("\t// Initial key load.\n")
	buf.WriteString("\th.refreshKeys()\n")
	buf.WriteString("\n")
	buf.WriteString("\t// Start background refresh goroutine.\n")
	buf.WriteString("\tgo h.refreshLoop()\n")
	buf.WriteString("\n")
	buf.WriteString("\treturn func(next http.Handler) http.Handler {\n")
	buf.WriteString("\t\treturn http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {\n")
	buf.WriteString("\t\t\t// Check if path is public.\n")
	buf.WriteString("\t\t\tif h.publicPaths[r.URL.Path] {\n")
	buf.WriteString("\t\t\t\tnext.ServeHTTP(w, r)\n")
	buf.WriteString("\t\t\t\treturn\n")
	buf.WriteString("\t\t\t}\n\n")
	buf.WriteString("\t\t\t// Extract Bearer token.\n")
	buf.WriteString("\t\t\tauthHeader := r.Header.Get(\"Authorization\")\n")
	buf.WriteString("\t\t\tif authHeader == \"\" {\n")
	buf.WriteString("\t\t\t\twriteAuthError(w, r, \"missing authentication token\")\n")
	buf.WriteString("\t\t\t\treturn\n")
	buf.WriteString("\t\t\t}\n")
	buf.WriteString("\t\t\tif !strings.HasPrefix(authHeader, \"Bearer \") {\n")
	buf.WriteString("\t\t\t\twriteAuthError(w, r, \"invalid authorization header format\")\n")
	buf.WriteString("\t\t\t\treturn\n")
	buf.WriteString("\t\t\t}\n")
	buf.WriteString("\t\t\ttokenStr := strings.TrimPrefix(authHeader, \"Bearer \")\n\n")
	buf.WriteString("\t\t\t// Parse and validate the JWT.\n")
	buf.WriteString("\t\t\ttoken, err := jwt.Parse(tokenStr, func(token *jwt.Token) (interface{}, error) {\n")
	buf.WriteString("\t\t\t\t// Verify signing method is RSA.\n")
	buf.WriteString("\t\t\t\tif _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {\n")
	buf.WriteString("\t\t\t\t\treturn nil, fmt.Errorf(\"unexpected signing method: %v\", token.Header[\"alg\"])\n")
	buf.WriteString("\t\t\t\t}\n\n")
	buf.WriteString("\t\t\t\t// Look up the key by kid.\n")
	buf.WriteString("\t\t\t\tkid, _ := token.Header[\"kid\"].(string)\n")
	buf.WriteString("\t\t\t\tkey := h.getKey(kid)\n")
	buf.WriteString("\t\t\t\tif key != nil {\n")
	buf.WriteString("\t\t\t\t\treturn key, nil\n")
	buf.WriteString("\t\t\t\t}\n\n")
	buf.WriteString("\t\t\t\t// Unknown kid: attempt one-shot refresh with cooldown.\n")
	buf.WriteString("\t\t\t\th.tryRefresh()\n")
	buf.WriteString("\t\t\t\tkey = h.getKey(kid)\n")
	buf.WriteString("\t\t\t\tif key != nil {\n")
	buf.WriteString("\t\t\t\t\treturn key, nil\n")
	buf.WriteString("\t\t\t\t}\n\n")
	buf.WriteString("\t\t\t\treturn nil, fmt.Errorf(\"unknown key id: %s\", kid)\n")
	buf.WriteString("\t\t\t})\n\n")
	buf.WriteString("\t\t\tif err != nil {\n")
	buf.WriteString("\t\t\t\twriteAuthError(w, r, \"invalid authentication token\")\n")
	buf.WriteString("\t\t\t\treturn\n")
	buf.WriteString("\t\t\t}\n\n")
	buf.WriteString("\t\t\t// Store the parsed token in context.\n")
	buf.WriteString("\t\t\tctx := context.WithValue(r.Context(), tokenContextKey, token)\n\n")
	buf.WriteString("\t\t\t// Extract and store username.\n")
	buf.WriteString("\t\t\tpayload := GetAuthPayloadFromContext(ctx)\n")
	buf.WriteString("\t\t\tctx = SetUsernameContext(ctx, payload.Username)\n\n")
	buf.WriteString("\t\t\tnext.ServeHTTP(w, r.WithContext(ctx))\n")
	buf.WriteString("\t\t})\n")
	buf.WriteString("\t}\n")
	buf.WriteString("}\n\n")

	// --- Stop ---
	buf.WriteString("// Stop terminates the background key refresh goroutine.\n")
	buf.WriteString("func (h *JWTHandler) Stop() {\n")
	buf.WriteString("\tclose(h.stopCh)\n")
	buf.WriteString("}\n\n")

	// --- Key management helpers ---
	buf.WriteString("// getKey retrieves an RSA public key by kid from the cache.\n")
	buf.WriteString("func (h *JWTHandler) getKey(kid string) *rsa.PublicKey {\n")
	buf.WriteString("\th.mu.RLock()\n")
	buf.WriteString("\tdefer h.mu.RUnlock()\n")
	buf.WriteString("\treturn h.keys[kid]\n")
	buf.WriteString("}\n\n")

	buf.WriteString("// tryRefresh attempts a one-shot key refresh with a 30-second cooldown\n")
	buf.WriteString("// to prevent refresh storms. The cooldown check and lastRefresh update\n")
	buf.WriteString("// are performed atomically under the same lock to prevent TOCTOU races\n")
	buf.WriteString("// where concurrent goroutines could all pass the cooldown check.\n")
	buf.WriteString("func (h *JWTHandler) tryRefresh() {\n")
	buf.WriteString("\th.mu.Lock()\n")
	buf.WriteString("\tif time.Since(h.lastRefresh) < 30*time.Second {\n")
	buf.WriteString("\t\th.mu.Unlock()\n")
	buf.WriteString("\t\treturn\n")
	buf.WriteString("\t}\n")
	buf.WriteString("\t// Claim the refresh slot while holding the lock so concurrent\n")
	buf.WriteString("\t// goroutines see the updated lastRefresh and bail out.\n")
	buf.WriteString("\th.lastRefresh = time.Now()\n")
	buf.WriteString("\th.mu.Unlock()\n")
	buf.WriteString("\th.refreshKeys()\n")
	buf.WriteString("}\n\n")

	buf.WriteString("// refreshLoop runs in the background and periodically refreshes keys.\n")
	buf.WriteString("func (h *JWTHandler) refreshLoop() {\n")
	buf.WriteString("\tvar interval time.Duration\n")
	buf.WriteString("\tif h.keysFile != \"\" {\n")
	buf.WriteString("\t\tinterval = 5 * time.Minute // file: every 5 minutes\n")
	buf.WriteString("\t} else {\n")
	buf.WriteString("\t\tinterval = 1 * time.Hour // URL: every hour\n")
	buf.WriteString("\t}\n")
	buf.WriteString("\tticker := time.NewTicker(interval)\n")
	buf.WriteString("\tdefer ticker.Stop()\n")
	buf.WriteString("\tfor {\n")
	buf.WriteString("\t\tselect {\n")
	buf.WriteString("\t\tcase <-h.stopCh:\n")
	buf.WriteString("\t\t\treturn\n")
	buf.WriteString("\t\tcase <-ticker.C:\n")
	buf.WriteString("\t\t\th.refreshKeys()\n")
	buf.WriteString("\t\t}\n")
	buf.WriteString("\t}\n")
	buf.WriteString("}\n\n")

	buf.WriteString("// refreshKeys fetches keys from the configured source and atomically\n")
	buf.WriteString("// replaces the key map.\n")
	buf.WriteString("func (h *JWTHandler) refreshKeys() {\n")
	buf.WriteString("\tvar data []byte\n")
	buf.WriteString("\tvar err error\n\n")
	buf.WriteString("\tif h.keysFile != \"\" {\n")
	buf.WriteString("\t\tdata, err = os.ReadFile(h.keysFile)\n")
	buf.WriteString("\t} else if h.keysURL != \"\" {\n")
	buf.WriteString("\t\tdata, err = fetchURL(h.keysURL)\n")
	buf.WriteString("\t} else {\n")
	buf.WriteString("\t\treturn\n")
	buf.WriteString("\t}\n\n")
	buf.WriteString("\tif err != nil {\n")
	buf.WriteString("\t\t// Log but do not crash; stale keys are better than no keys.\n")
	buf.WriteString("\t\treturn\n")
	buf.WriteString("\t}\n\n")
	buf.WriteString("\tvar jwks jwkSet\n")
	buf.WriteString("\tif err := json.Unmarshal(data, &jwks); err != nil {\n")
	buf.WriteString("\t\treturn\n")
	buf.WriteString("\t}\n\n")
	buf.WriteString("\tnewKeys := make(map[string]*rsa.PublicKey)\n")
	buf.WriteString("\tfor _, k := range jwks.Keys {\n")
	buf.WriteString("\t\tif k.Kty != \"RSA\" || k.Kid == \"\" {\n")
	buf.WriteString("\t\t\tcontinue\n")
	buf.WriteString("\t\t}\n")
	buf.WriteString("\t\tpub, err := parseRSAPublicKey(k)\n")
	buf.WriteString("\t\tif err != nil {\n")
	buf.WriteString("\t\t\tcontinue\n")
	buf.WriteString("\t\t}\n")
	buf.WriteString("\t\tnewKeys[k.Kid] = pub\n")
	buf.WriteString("\t}\n\n")
	buf.WriteString("\th.mu.Lock()\n")
	buf.WriteString("\th.keys = newKeys\n")
	buf.WriteString("\th.lastRefresh = time.Now()\n")
	buf.WriteString("\th.mu.Unlock()\n")
	buf.WriteString("}\n\n")

	buf.WriteString("// fetchURL retrieves the JWK set from a remote URL.\n")
	buf.WriteString("func fetchURL(url string) ([]byte, error) {\n")
	buf.WriteString("\tclient := &http.Client{Timeout: 10 * time.Second}\n")
	buf.WriteString("\tresp, err := client.Get(url)\n")
	buf.WriteString("\tif err != nil {\n")
	buf.WriteString("\t\treturn nil, err\n")
	buf.WriteString("\t}\n")
	buf.WriteString("\tdefer resp.Body.Close()\n")
	buf.WriteString("\treturn io.ReadAll(resp.Body)\n")
	buf.WriteString("}\n\n")

	buf.WriteString("// parseRSAPublicKey constructs an RSA public key from a JWK.\n")
	buf.WriteString("func parseRSAPublicKey(k jwk) (*rsa.PublicKey, error) {\n")
	buf.WriteString("\tnBytes, err := base64.RawURLEncoding.DecodeString(k.N)\n")
	buf.WriteString("\tif err != nil {\n")
	buf.WriteString("\t\treturn nil, fmt.Errorf(\"decoding modulus: %w\", err)\n")
	buf.WriteString("\t}\n")
	buf.WriteString("\teBytes, err := base64.RawURLEncoding.DecodeString(k.E)\n")
	buf.WriteString("\tif err != nil {\n")
	buf.WriteString("\t\treturn nil, fmt.Errorf(\"decoding exponent: %w\", err)\n")
	buf.WriteString("\t}\n")
	buf.WriteString("\tn := new(big.Int).SetBytes(nBytes)\n")
	buf.WriteString("\te := new(big.Int).SetBytes(eBytes)\n")
	buf.WriteString("\treturn &rsa.PublicKey{N: n, E: int(e.Int64())}, nil\n")
	buf.WriteString("}\n\n")

	// --- writeAuthError ---
	buf.WriteString("// writeAuthError writes an RFC 9457 Problem Details error response\n")
	buf.WriteString("// for authentication failures.\n")
	buf.WriteString("func writeAuthError(w http.ResponseWriter, r *http.Request, detail string) {\n")
	buf.WriteString("\tresp := map[string]any{\n")
	fmt.Fprintf(&buf, "\t\t\"type\":      %q,\n", errorTypeURI)
	buf.WriteString("\t\t\"title\":     \"Unauthorized\",\n")
	buf.WriteString("\t\t\"status\":    http.StatusUnauthorized,\n")
	buf.WriteString("\t\t\"detail\":    detail,\n")
	fmt.Fprintf(&buf, "\t\t\"code\":      %q,\n", errorPrefix+"-AUT-001")
	buf.WriteString("\t\t\"instance\":  r.URL.Path,\n")
	buf.WriteString("\t\t\"timestamp\": time.Now().UTC().Format(time.RFC3339),\n")
	buf.WriteString("\t}\n")
	buf.WriteString("\tw.Header().Set(\"Content-Type\", \"application/problem+json\")\n")
	buf.WriteString("\tw.WriteHeader(http.StatusUnauthorized)\n")
	buf.WriteString("\tjson.NewEncoder(w).Encode(resp)\n")
	buf.WriteString("}\n")

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return gen.File{}, fmt.Errorf("formatting middleware.go: %w", err)
	}

	return gen.File{
		Path:    path.Join(ns, "middleware.go"),
		Content: formatted,
	}, nil
}

// generateContext produces the context.go file containing the Payload struct,
// claim extraction with fallback chains, and context accessors.
func generateContext(ns, pkg string) (gen.File, error) {
	var buf bytes.Buffer

	fmt.Fprintf(&buf, "package %s\n\n", pkg)
	fmt.Fprintf(&buf, "import (\n")
	fmt.Fprintf(&buf, "\t\"context\"\n")
	fmt.Fprintf(&buf, "\t\"net/http\"\n")
	fmt.Fprintf(&buf, "\t\"strings\"\n")
	fmt.Fprintf(&buf, "\n")
	fmt.Fprintf(&buf, "\tjwt \"github.com/golang-jwt/jwt/v4\"\n")
	fmt.Fprintf(&buf, ")\n\n")

	// Context key types.
	buf.WriteString("type ctxKey int\n\n")
	buf.WriteString("const (\n")
	buf.WriteString("\ttokenContextKey ctxKey = iota\n")
	buf.WriteString("\tusernameContextKey\n")
	buf.WriteString(")\n\n")

	// --- Payload struct ---
	buf.WriteString("// Payload represents the caller's identity extracted from JWT claims.\n")
	buf.WriteString("// Fields are populated via fallback chains to support both Red Hat SSO\n")
	buf.WriteString("// and RHD (Red Hat Developer) JWT formats.\n")
	buf.WriteString("type Payload struct {\n")
	buf.WriteString("\tUsername  string // from \"username\" -> \"preferred_username\" -> \"sub\"\n")
	buf.WriteString("\tFirstName string // from \"first_name\" -> \"given_name\" -> split(\"name\")[0]\n")
	buf.WriteString("\tLastName  string // from \"last_name\" -> \"family_name\" -> split(\"name\")[1]\n")
	buf.WriteString("\tEmail     string // from \"email\"\n")
	buf.WriteString("\tClientID  string // from \"clientId\"\n")
	buf.WriteString("\tIssuer    string // from \"iss\"\n")
	buf.WriteString("}\n\n")

	// --- TokenFromContext ---
	buf.WriteString("// TokenFromContext retrieves the raw parsed JWT token from the request context.\n")
	buf.WriteString("// Returns nil if no token is present.\n")
	buf.WriteString("func TokenFromContext(ctx context.Context) *jwt.Token {\n")
	buf.WriteString("\ttoken, _ := ctx.Value(tokenContextKey).(*jwt.Token)\n")
	buf.WriteString("\treturn token\n")
	buf.WriteString("}\n\n")

	// --- GetAuthPayloadFromContext ---
	buf.WriteString("// GetAuthPayloadFromContext extracts a Payload from the JWT claims stored\n")
	buf.WriteString("// in the context. Returns a zero Payload if no token is present.\n")
	buf.WriteString("func GetAuthPayloadFromContext(ctx context.Context) Payload {\n")
	buf.WriteString("\ttoken := TokenFromContext(ctx)\n")
	buf.WriteString("\tif token == nil {\n")
	buf.WriteString("\t\treturn Payload{}\n")
	buf.WriteString("\t}\n")
	buf.WriteString("\tclaims, ok := token.Claims.(jwt.MapClaims)\n")
	buf.WriteString("\tif !ok {\n")
	buf.WriteString("\t\treturn Payload{}\n")
	buf.WriteString("\t}\n")
	buf.WriteString("\treturn extractPayload(claims)\n")
	buf.WriteString("}\n\n")

	// --- GetAuthPayload ---
	buf.WriteString("// GetAuthPayload is a convenience wrapper that extracts the Payload from\n")
	buf.WriteString("// an HTTP request's context.\n")
	buf.WriteString("func GetAuthPayload(r *http.Request) Payload {\n")
	buf.WriteString("\treturn GetAuthPayloadFromContext(r.Context())\n")
	buf.WriteString("}\n\n")

	// --- GetUsernameFromContext ---
	buf.WriteString("// GetUsernameFromContext retrieves the username stored in the context\n")
	buf.WriteString("// by the authentication middleware.\n")
	buf.WriteString("func GetUsernameFromContext(ctx context.Context) string {\n")
	buf.WriteString("\tusername, _ := ctx.Value(usernameContextKey).(string)\n")
	buf.WriteString("\treturn username\n")
	buf.WriteString("}\n\n")

	// --- SetUsernameContext ---
	buf.WriteString("// SetUsernameContext returns a new context with the username stored.\n")
	buf.WriteString("func SetUsernameContext(ctx context.Context, username string) context.Context {\n")
	buf.WriteString("\treturn context.WithValue(ctx, usernameContextKey, username)\n")
	buf.WriteString("}\n\n")

	// --- extractPayload ---
	buf.WriteString("// extractPayload builds a Payload from JWT MapClaims using fallback chains.\n")
	buf.WriteString("func extractPayload(claims jwt.MapClaims) Payload {\n")
	buf.WriteString("\tp := Payload{\n")
	buf.WriteString("\t\tUsername:  claimString(claims, \"username\", \"preferred_username\", \"sub\"),\n")
	buf.WriteString("\t\tEmail:    claimString(claims, \"email\"),\n")
	buf.WriteString("\t\tClientID: claimString(claims, \"clientId\"),\n")
	buf.WriteString("\t\tIssuer:   claimString(claims, \"iss\"),\n")
	buf.WriteString("\t}\n\n")
	buf.WriteString("\t// FirstName fallback chain.\n")
	buf.WriteString("\tp.FirstName = claimString(claims, \"first_name\", \"given_name\")\n")
	buf.WriteString("\tif p.FirstName == \"\" {\n")
	buf.WriteString("\t\tif name := claimString(claims, \"name\"); name != \"\" {\n")
	buf.WriteString("\t\t\tparts := strings.SplitN(name, \" \", 2)\n")
	buf.WriteString("\t\t\tp.FirstName = parts[0]\n")
	buf.WriteString("\t\t}\n")
	buf.WriteString("\t}\n\n")
	buf.WriteString("\t// LastName fallback chain.\n")
	buf.WriteString("\tp.LastName = claimString(claims, \"last_name\", \"family_name\")\n")
	buf.WriteString("\tif p.LastName == \"\" {\n")
	buf.WriteString("\t\tif name := claimString(claims, \"name\"); name != \"\" {\n")
	buf.WriteString("\t\t\tparts := strings.SplitN(name, \" \", 2)\n")
	buf.WriteString("\t\t\tif len(parts) > 1 {\n")
	buf.WriteString("\t\t\t\tp.LastName = parts[1]\n")
	buf.WriteString("\t\t\t}\n")
	buf.WriteString("\t\t}\n")
	buf.WriteString("\t}\n\n")
	buf.WriteString("\treturn p\n")
	buf.WriteString("}\n\n")

	// --- claimString helper ---
	buf.WriteString("// claimString returns the first non-empty string value found for the given\n")
	buf.WriteString("// claim keys, implementing the fallback chain.\n")
	buf.WriteString("func claimString(claims jwt.MapClaims, keys ...string) string {\n")
	buf.WriteString("\tfor _, key := range keys {\n")
	buf.WriteString("\t\tif v, ok := claims[key]; ok {\n")
	buf.WriteString("\t\t\tif s, ok := v.(string); ok && s != \"\" {\n")
	buf.WriteString("\t\t\t\treturn s\n")
	buf.WriteString("\t\t\t}\n")
	buf.WriteString("\t\t}\n")
	buf.WriteString("\t}\n")
	buf.WriteString("\treturn \"\"\n")
	buf.WriteString("}\n")

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return gen.File{}, fmt.Errorf("formatting context.go: %w", err)
	}

	return gen.File{
		Path:    path.Join(ns, "context.go"),
		Content: formatted,
	}, nil
}
