package rhssoauth

import (
	"go/format"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

// --- Interface compliance ---

func TestGeneratorImplementsInterface(t *testing.T) {
	var _ gen.Generator = (*Generator)(nil)
}

// --- Basic generation ---

func TestGenerateDefaultConfig(t *testing.T) {
	ctx := gen.Context{
		OutputNamespace: "internal/auth",
		ComponentConfig: map[string]any{},
		ServiceName:     "user-management",
	}
	g := &Generator{}
	files, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}

	// Verify file paths.
	if files[0].Path != "internal/auth/middleware.go" {
		t.Errorf("expected middleware.go path, got %s", files[0].Path)
	}
	if files[1].Path != "internal/auth/context.go" {
		t.Errorf("expected context.go path, got %s", files[1].Path)
	}

	// Verify both files compile.
	for _, f := range files {
		rendered := f.Bytes()
		if _, err := format.Source(rendered); err != nil {
			t.Fatalf("file %s does not compile: %v\n%s", f.Path, err, rendered)
		}
	}

	// Verify generated header is present in rendered output.
	for _, f := range files {
		if !strings.Contains(string(f.Bytes()), gen.Header) {
			t.Errorf("file %s: rendered output should contain generated-file header", f.Path)
		}
	}

	// Verify wiring.
	if wiring == nil {
		t.Fatal("expected non-nil wiring")
	}
	if len(wiring.Imports) != 1 || wiring.Imports[0] != "internal/auth" {
		t.Errorf("expected imports [internal/auth], got %v", wiring.Imports)
	}
	if len(wiring.Constructors) != 1 || wiring.Constructors[0] != "auth.NewJWTHandler().Build()" {
		t.Errorf("expected constructor auth.NewJWTHandler().Build(), got %v", wiring.Constructors)
	}
	if wiring.MiddlewareConstructor == nil {
		t.Fatal("expected non-nil MiddlewareConstructor")
	}
	if *wiring.MiddlewareConstructor != 0 {
		t.Errorf("expected MiddlewareConstructor=0, got %d", *wiring.MiddlewareConstructor)
	}
	if wiring.MiddlewareWrapExpr != "%s(%s)" {
		t.Errorf("expected MiddlewareWrapExpr=%%s(%%s), got %s", wiring.MiddlewareWrapExpr)
	}
}

// --- GoModRequires ---

func TestGenerateGoModRequires(t *testing.T) {
	ctx := gen.Context{
		OutputNamespace: "internal/auth",
		ComponentConfig: map[string]any{},
	}
	g := &Generator{}
	_, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if wiring.GoModRequires == nil {
		t.Fatal("expected non-nil GoModRequires")
	}
	jwtVer, ok := wiring.GoModRequires["github.com/golang-jwt/jwt/v4"]
	if !ok {
		t.Fatal("GoModRequires should include github.com/golang-jwt/jwt/v4")
	}
	if jwtVer == "" {
		t.Error("jwt/v4 version should not be empty")
	}
}

// --- Middleware file content ---

func TestGenerateMiddlewareContent(t *testing.T) {
	ctx := gen.Context{
		OutputNamespace: "internal/auth",
		ComponentConfig: map[string]any{},
		ServiceName:     "hyperfleet-api",
		ErrorTypeBase:   "https://api.hyperfleet.io/errors/",
	}
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := string(files[0].Content)

	// JWTHandler struct.
	if !strings.Contains(content, "type JWTHandler struct") {
		t.Error("middleware should contain JWTHandler struct")
	}

	// Builder pattern methods.
	if !strings.Contains(content, "func NewJWTHandler()") {
		t.Error("middleware should contain NewJWTHandler constructor")
	}
	if !strings.Contains(content, "func (h *JWTHandler) WithKeysURL(") {
		t.Error("middleware should contain WithKeysURL method")
	}
	if !strings.Contains(content, "func (h *JWTHandler) WithKeysFile(") {
		t.Error("middleware should contain WithKeysFile method")
	}
	if !strings.Contains(content, "func (h *JWTHandler) WithPublicPath(") {
		t.Error("middleware should contain WithPublicPath method")
	}
	if !strings.Contains(content, "func (h *JWTHandler) Build()") {
		t.Error("middleware should contain Build method")
	}

	// Build returns func(http.Handler) http.Handler.
	if !strings.Contains(content, "func(http.Handler) http.Handler") {
		t.Error("Build should return func(http.Handler) http.Handler")
	}

	// Stop method.
	if !strings.Contains(content, "func (h *JWTHandler) Stop()") {
		t.Error("middleware should contain Stop method")
	}

	// JWK key management.
	if !strings.Contains(content, "type jwkSet struct") {
		t.Error("middleware should contain jwkSet type")
	}
	if !strings.Contains(content, "type jwk struct") {
		t.Error("middleware should contain jwk type")
	}
	if !strings.Contains(content, "parseRSAPublicKey") {
		t.Error("middleware should contain parseRSAPublicKey function")
	}
	if !strings.Contains(content, "refreshKeys") {
		t.Error("middleware should contain refreshKeys method")
	}

	// RSA validation.
	if !strings.Contains(content, "jwt.SigningMethodRSA") {
		t.Error("middleware should verify RSA signing method")
	}

	// Token from Authorization header.
	if !strings.Contains(content, `r.Header.Get("Authorization")`) {
		t.Error("middleware should extract token from Authorization header")
	}
	if !strings.Contains(content, `Bearer `) {
		t.Error("middleware should handle Bearer prefix")
	}

	// Public path bypass.
	if !strings.Contains(content, "h.publicPaths[r.URL.Path]") {
		t.Error("middleware should check public paths with exact matching")
	}

	// Default public paths.
	if !strings.Contains(content, `"/healthcheck"`) {
		t.Error("middleware should include /healthcheck as default public path")
	}
	if !strings.Contains(content, `"/metrics"`) {
		t.Error("middleware should include /metrics as default public path")
	}
	if !strings.Contains(content, `"/openapi"`) {
		t.Error("middleware should include /openapi as always-public path")
	}

	// Unknown kid handling with cooldown.
	if !strings.Contains(content, "tryRefresh") {
		t.Error("middleware should handle unknown kid with one-shot refresh")
	}
	if !strings.Contains(content, "30*time.Second") {
		t.Error("middleware should have 30-second refresh cooldown")
	}

	// Thread safety.
	if !strings.Contains(content, "sync.RWMutex") {
		t.Error("middleware should use sync.RWMutex for thread safety")
	}
	if !strings.Contains(content, "h.mu.RLock()") {
		t.Error("middleware reads should use RLock")
	}
	if !strings.Contains(content, "h.mu.Lock()") {
		t.Error("middleware writes should use Lock")
	}

	// Default JWK cert URL.
	if !strings.Contains(content, "sso.redhat.com") {
		t.Error("middleware should contain default JWK cert URL")
	}

	// Error handling.
	if !strings.Contains(content, "application/problem+json") {
		t.Error("middleware should produce RFC 9457 errors")
	}
	if !strings.Contains(content, "HYPERFLEET-AUT-001") {
		t.Error("middleware should use correct error code prefix derived from service name")
	}
	if !strings.Contains(content, "https://api.hyperfleet.io/errors/unauthorized") {
		t.Error("middleware should use error type URI from ErrorTypeBase")
	}

	// AUTH_ENABLED env check.
	if !strings.Contains(content, `os.Getenv("AUTH_ENABLED")`) {
		t.Error("middleware should check AUTH_ENABLED environment variable")
	}

	// JWK_CERT_URL env override in Build().
	if !strings.Contains(content, `os.Getenv("JWK_CERT_URL")`) {
		t.Error("Build() should read JWK_CERT_URL from environment")
	}

	// JWK_CERT_FILE env override in Build().
	if !strings.Contains(content, `os.Getenv("JWK_CERT_FILE")`) {
		t.Error("Build() should read JWK_CERT_FILE from environment")
	}

	// Refresh intervals.
	if !strings.Contains(content, "5 * time.Minute") {
		t.Error("middleware should use 5-minute refresh for file source")
	}
	if !strings.Contains(content, "1 * time.Hour") {
		t.Error("middleware should use 1-hour refresh for URL source")
	}
}

// --- Context file content ---

func TestGenerateContextContent(t *testing.T) {
	ctx := gen.Context{
		OutputNamespace: "internal/auth",
		ComponentConfig: map[string]any{},
	}
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := string(files[1].Content)

	// Payload struct.
	if !strings.Contains(content, "type Payload struct") {
		t.Error("context.go should contain Payload struct")
	}
	if !strings.Contains(content, "Username") {
		t.Error("Payload should have Username field")
	}
	if !strings.Contains(content, "FirstName") {
		t.Error("Payload should have FirstName field")
	}
	if !strings.Contains(content, "LastName") {
		t.Error("Payload should have LastName field")
	}
	if !strings.Contains(content, "Email") {
		t.Error("Payload should have Email field")
	}
	if !strings.Contains(content, "ClientID") {
		t.Error("Payload should have ClientID field")
	}
	if !strings.Contains(content, "Issuer") {
		t.Error("Payload should have Issuer field")
	}

	// Context accessors.
	if !strings.Contains(content, "func GetAuthPayloadFromContext(") {
		t.Error("context.go should contain GetAuthPayloadFromContext")
	}
	if !strings.Contains(content, "func GetAuthPayload(r *http.Request)") {
		t.Error("context.go should contain GetAuthPayload")
	}
	if !strings.Contains(content, "func GetUsernameFromContext(") {
		t.Error("context.go should contain GetUsernameFromContext")
	}
	if !strings.Contains(content, "func SetUsernameContext(") {
		t.Error("context.go should contain SetUsernameContext")
	}
	if !strings.Contains(content, "func TokenFromContext(") {
		t.Error("context.go should contain TokenFromContext")
	}

	// Fallback chains.
	if !strings.Contains(content, `"username", "preferred_username", "sub"`) {
		t.Error("Username should use fallback chain: username -> preferred_username -> sub")
	}
	if !strings.Contains(content, `"first_name", "given_name"`) {
		t.Error("FirstName should use fallback chain: first_name -> given_name -> split(name)[0]")
	}
	if !strings.Contains(content, `"last_name", "family_name"`) {
		t.Error("LastName should use fallback chain: last_name -> family_name -> split(name)[1]")
	}
	if !strings.Contains(content, `SplitN(name, " ", 2)`) {
		t.Error("FirstName/LastName fallback should split name on space")
	}

	// claimString helper.
	if !strings.Contains(content, "func claimString(") {
		t.Error("context.go should contain claimString helper")
	}

	// JWT import.
	if !strings.Contains(content, `jwt "github.com/golang-jwt/jwt/v4"`) {
		t.Error("context.go should import golang-jwt/jwt/v4")
	}
}

// --- Custom output namespace ---

func TestGenerateCustomNamespace(t *testing.T) {
	ctx := gen.Context{
		OutputNamespace: "pkg/authn",
		ComponentConfig: map[string]any{},
	}
	g := &Generator{}
	files, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}

	if files[0].Path != "pkg/authn/middleware.go" {
		t.Errorf("expected middleware path under custom namespace, got %s", files[0].Path)
	}
	if files[1].Path != "pkg/authn/context.go" {
		t.Errorf("expected context path under custom namespace, got %s", files[1].Path)
	}

	// Verify package names.
	if !strings.Contains(string(files[0].Content), "package authn") {
		t.Error("middleware.go should use correct package from namespace")
	}
	if !strings.Contains(string(files[1].Content), "package authn") {
		t.Error("context.go should use correct package from namespace")
	}

	// Verify wiring uses correct namespace.
	if wiring.Imports[0] != "pkg/authn" {
		t.Errorf("expected import pkg/authn, got %s", wiring.Imports[0])
	}
	if wiring.Constructors[0] != "authn.NewJWTHandler().Build()" {
		t.Errorf("expected constructor authn.NewJWTHandler().Build(), got %s", wiring.Constructors[0])
	}

	// Verify compilation.
	for _, f := range files {
		rendered := f.Bytes()
		if _, err := format.Source(rendered); err != nil {
			t.Fatalf("file %s does not compile: %v\n%s", f.Path, err, rendered)
		}
	}
}

// --- Empty/nil component config ---

func TestGenerateNilComponentConfig(t *testing.T) {
	ctx := gen.Context{
		OutputNamespace: "internal/auth",
		ComponentConfig: nil,
	}
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}

	content := string(files[0].Content)

	// Should use default JWK URL.
	if !strings.Contains(content, "sso.redhat.com") {
		t.Error("should default to sso.redhat.com JWK URL when config is nil")
	}

	// Should use default public paths.
	if !strings.Contains(content, `"/healthcheck"`) {
		t.Error("should use default public paths when config is nil")
	}
}

// --- Custom JWK URL ---

func TestGenerateCustomJWKURL(t *testing.T) {
	ctx := gen.Context{
		OutputNamespace: "internal/auth",
		ComponentConfig: map[string]any{
			"jwk_cert_url": "https://custom.sso.com/keys",
		},
	}
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := string(files[0].Content)
	if !strings.Contains(content, "https://custom.sso.com/keys") {
		t.Error("middleware should use custom JWK URL from config")
	}
}

// --- Custom public paths ---

func TestGenerateCustomPublicPaths(t *testing.T) {
	ctx := gen.Context{
		OutputNamespace: "internal/auth",
		ComponentConfig: map[string]any{
			"public_paths": []any{"/health", "/ready"},
		},
	}
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := string(files[0].Content)
	if !strings.Contains(content, `"/health"`) {
		t.Error("middleware should include custom public path /health")
	}
	if !strings.Contains(content, `"/ready"`) {
		t.Error("middleware should include custom public path /ready")
	}
	// /openapi is always public.
	if !strings.Contains(content, `"/openapi"`) {
		t.Error("middleware should always include /openapi as public path")
	}
}

// --- Error code prefix derivation ---

func TestDeriveErrorPrefix(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"hyperfleet-api", "HYPERFLEET"},
		{"user-management", "USERMANAGEMENT"},
		{"simple", "SIMPLE"},
		{"my-cool-service", "MYCOOL"},
		{"test-api", "TEST"},
	}
	for _, tc := range tests {
		got := deriveErrorPrefix(tc.input)
		if got != tc.expected {
			t.Errorf("deriveErrorPrefix(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

// --- Error type URI ---

func TestGenerateErrorTypeURI(t *testing.T) {
	// With ErrorTypeBase set.
	ctx := gen.Context{
		OutputNamespace: "internal/auth",
		ComponentConfig: map[string]any{},
		ErrorTypeBase:   "https://api.example.com/errors/",
	}
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := string(files[0].Content)
	if !strings.Contains(content, "https://api.example.com/errors/unauthorized") {
		t.Error("should use ErrorTypeBase for error type URI")
	}

	// Without ErrorTypeBase (defaults to about:blank).
	ctx.ErrorTypeBase = ""
	files, _, err = g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content = string(files[0].Content)
	if !strings.Contains(content, "about:blank") {
		t.Error("should use about:blank when ErrorTypeBase is empty")
	}
}

// --- BasePath integration ---

func TestGenerateWithBasePath(t *testing.T) {
	ctx := gen.Context{
		OutputNamespace: "internal/auth",
		ComponentConfig: map[string]any{
			"public_paths": []any{"/healthcheck"},
		},
		BasePath: "/api/v1",
	}
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := string(files[0].Content)

	// Public paths should be prefixed with base path.
	if !strings.Contains(content, `"/api/v1/healthcheck"`) {
		t.Error("public paths should be prefixed with base path")
	}

	// OpenAPI path should be prefixed.
	if !strings.Contains(content, `"/api/v1/openapi"`) {
		t.Error("openapi path should be prefixed with base path")
	}
}

// --- Namespace validation ---

func TestGenerateDefaultNamespaceWhenEmpty(t *testing.T) {
	ctx := gen.Context{
		OutputNamespace: "",
		ComponentConfig: map[string]any{},
	}
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if files[0].Path != "internal/auth/middleware.go" {
		t.Errorf("expected default namespace path, got %s", files[0].Path)
	}
	if files[1].Path != "internal/auth/context.go" {
		t.Errorf("expected default namespace path, got %s", files[1].Path)
	}
}

// --- Middleware wiring is function-type ---

func TestMiddlewareWiringIsFunctionType(t *testing.T) {
	ctx := gen.Context{
		OutputNamespace: "internal/auth",
		ComponentConfig: map[string]any{},
	}
	g := &Generator{}
	_, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Build() returns func(http.Handler) http.Handler, so wrap expression is %s(%s).
	if wiring.MiddlewareWrapExpr != "%s(%s)" {
		t.Errorf("middleware wrap expression should be %%s(%%s) for function-type, got %s", wiring.MiddlewareWrapExpr)
	}
}

// --- Runtime env var configuration in Build() ---

func TestBuildReadsEnvVarsAtRuntime(t *testing.T) {
	ctx := gen.Context{
		OutputNamespace: "internal/auth",
		ComponentConfig: map[string]any{},
		ServiceName:     "test-service",
	}
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := string(files[0].Content)

	// Build() must read JWK_CERT_URL before the initial key load.
	urlIdx := strings.Index(content, `os.Getenv("JWK_CERT_URL")`)
	if urlIdx < 0 {
		t.Fatal("Build() must read JWK_CERT_URL from environment")
	}

	// Build() must read JWK_CERT_FILE before the initial key load.
	fileIdx := strings.Index(content, `os.Getenv("JWK_CERT_FILE")`)
	if fileIdx < 0 {
		t.Fatal("Build() must read JWK_CERT_FILE from environment")
	}

	// Both env var reads must appear before the initial key load (refreshKeys call).
	refreshIdx := strings.Index(content, "h.refreshKeys()")
	if refreshIdx < 0 {
		t.Fatal("Build() must call refreshKeys()")
	}
	if urlIdx > refreshIdx {
		t.Error("JWK_CERT_URL env read must occur before initial refreshKeys() call")
	}
	if fileIdx > refreshIdx {
		t.Error("JWK_CERT_FILE env read must occur before initial refreshKeys() call")
	}

	// Verify the env var reads set the handler fields.
	if !strings.Contains(content, "h.keysURL = url") {
		t.Error("JWK_CERT_URL env var should set h.keysURL")
	}
	if !strings.Contains(content, "h.keysFile = file") {
		t.Error("JWK_CERT_FILE env var should set h.keysFile")
	}
}

// --- TOCTOU race prevention in tryRefresh ---

func TestTryRefreshUpdatesLastRefreshBeforeUnlock(t *testing.T) {
	ctx := gen.Context{
		OutputNamespace: "internal/auth",
		ComponentConfig: map[string]any{},
		ServiceName:     "test-service",
	}
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := string(files[0].Content)

	// Find the tryRefresh method body.
	tryRefreshIdx := strings.Index(content, "func (h *JWTHandler) tryRefresh()")
	if tryRefreshIdx < 0 {
		t.Fatal("middleware must contain tryRefresh method")
	}

	// Extract tryRefresh method body (up to the next top-level function).
	tryRefreshBody := content[tryRefreshIdx:]
	nextFuncIdx := strings.Index(tryRefreshBody[1:], "\nfunc ")
	if nextFuncIdx > 0 {
		tryRefreshBody = tryRefreshBody[:nextFuncIdx+1]
	}

	// The lastRefresh update must occur BEFORE the Unlock that precedes refreshKeys.
	// Find the order: h.lastRefresh = time.Now() must come before h.mu.Unlock()
	// (after the cooldown check's early-return Unlock).
	//
	// The pattern should be:
	//   h.mu.Lock()
	//   if time.Since(h.lastRefresh) < 30*time.Second {
	//     h.mu.Unlock()
	//     return
	//   }
	//   h.lastRefresh = time.Now()  // <-- before unlock
	//   h.mu.Unlock()
	//   h.refreshKeys()

	// Verify lastRefresh is updated in tryRefresh (not just in refreshKeys).
	if !strings.Contains(tryRefreshBody, "h.lastRefresh = time.Now()") {
		t.Error("tryRefresh must update lastRefresh while holding the lock")
	}

	// Verify the order: lastRefresh update appears before the second Unlock.
	lastRefreshIdx := strings.Index(tryRefreshBody, "h.lastRefresh = time.Now()")

	// Find the second Unlock (first one is in the cooldown early-return).
	firstUnlockIdx := strings.Index(tryRefreshBody, "h.mu.Unlock()")
	secondUnlockStr := tryRefreshBody[firstUnlockIdx+len("h.mu.Unlock()"):]
	secondUnlockOffset := strings.Index(secondUnlockStr, "h.mu.Unlock()")
	if secondUnlockOffset < 0 {
		t.Fatal("tryRefresh must have two Unlock calls")
	}
	secondUnlockIdx := firstUnlockIdx + len("h.mu.Unlock()") + secondUnlockOffset

	if lastRefreshIdx > secondUnlockIdx {
		t.Error("lastRefresh update must occur before the second Unlock (TOCTOU prevention)")
	}
}
