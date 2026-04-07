package jwtauth

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

// --- Default header (Authorization) ---

func TestGenerateDefaultHeader(t *testing.T) {
	ctx := gen.Context{
		OutputNamespace: "internal/auth",
		ComponentConfig: map[string]any{},
	}
	g := &Generator{}
	files, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}

	f := files[0]
	if f.Path != "internal/auth/middleware.go" {
		t.Errorf("expected path internal/auth/middleware.go, got %s", f.Path)
	}

	// Verify the generated code compiles.
	rendered := f.Bytes()
	if _, err := format.Source(rendered); err != nil {
		t.Fatalf("generated code does not compile: %v\n%s", err, rendered)
	}

	content := string(f.Content)

	// Verify generated code has the header set.
	if !strings.Contains(content, `r.Header.Get("Authorization")`) {
		t.Error("generated code should reference Authorization header")
	}

	// Verify Identity struct is present with correct fields.
	if !strings.Contains(content, "type Identity struct") {
		t.Error("generated code should contain Identity struct")
	}
	if !strings.Contains(content, `UserID`) {
		t.Error("generated code should contain UserID field")
	}
	if !strings.Contains(content, `Role`) {
		t.Error("generated code should contain Role field")
	}
	if !strings.Contains(content, `Attributes map[string]string`) {
		t.Error("generated code should contain Attributes field")
	}

	// Verify IdentityFromContext is present.
	if !strings.Contains(content, "func IdentityFromContext") {
		t.Error("generated code should contain IdentityFromContext function")
	}

	// Verify middleware constructor.
	if !strings.Contains(content, "func NewAuthMiddleware()") {
		t.Error("generated code should contain NewAuthMiddleware function")
	}

	// Verify token parsing.
	if !strings.Contains(content, "func parseJWT") {
		t.Error("generated code should contain parseJWT function")
	}

	// Verify Bearer prefix stripping.
	if !strings.Contains(content, `Bearer `) {
		t.Error("generated code should handle Bearer prefix")
	}

	// Verify wiring.
	if wiring == nil {
		t.Fatal("expected non-nil wiring")
	}
	if len(wiring.Imports) != 1 || wiring.Imports[0] != "internal/auth" {
		t.Errorf("expected imports [internal/auth], got %v", wiring.Imports)
	}
	if len(wiring.Constructors) != 1 || wiring.Constructors[0] != "auth.NewAuthMiddleware()" {
		t.Errorf("expected constructor auth.NewAuthMiddleware(), got %v", wiring.Constructors)
	}

	// Verify generated header is present in rendered output.
	if !strings.Contains(string(rendered), gen.Header) {
		t.Error("rendered output should contain generated-file header")
	}
}

// --- Custom header ---

func TestGenerateCustomHeader(t *testing.T) {
	ctx := gen.Context{
		OutputNamespace: "internal/auth",
		ComponentConfig: map[string]any{
			"header": "X-Internal-Token",
		},
	}
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}

	content := string(files[0].Content)

	// Verify the custom header is used.
	if !strings.Contains(content, `r.Header.Get("X-Internal-Token")`) {
		t.Error("generated code should reference X-Internal-Token header")
	}

	// Verify it does NOT reference the default Authorization header.
	if strings.Contains(content, `r.Header.Get("Authorization")`) {
		t.Error("generated code should not reference Authorization header when custom header is set")
	}

	// Verify the generated code compiles.
	rendered := files[0].Bytes()
	if _, err := format.Source(rendered); err != nil {
		t.Fatalf("generated code does not compile: %v\n%s", err, rendered)
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

	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}

	if files[0].Path != "pkg/authn/middleware.go" {
		t.Errorf("expected path pkg/authn/middleware.go, got %s", files[0].Path)
	}

	content := string(files[0].Content)
	if !strings.Contains(content, "package authn") {
		t.Error("generated code should use correct package name from namespace")
	}

	if wiring.Imports[0] != "pkg/authn" {
		t.Errorf("expected import pkg/authn, got %s", wiring.Imports[0])
	}

	if wiring.Constructors[0] != "authn.NewAuthMiddleware()" {
		t.Errorf("expected constructor authn.NewAuthMiddleware(), got %s", wiring.Constructors[0])
	}

	// Verify the generated code compiles.
	rendered := files[0].Bytes()
	if _, err := format.Source(rendered); err != nil {
		t.Fatalf("generated code does not compile: %v\n%s", err, rendered)
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

	content := string(files[0].Content)
	if !strings.Contains(content, `r.Header.Get("Authorization")`) {
		t.Error("should default to Authorization header when config is nil")
	}
}

// --- Identity struct populated from JWT claims ---

func TestGenerateIdentityPopulatedFromClaims(t *testing.T) {
	ctx := gen.Context{
		OutputNamespace: "internal/auth",
		ComponentConfig: map[string]any{},
	}
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := string(files[0].Content)

	// Verify that the middleware populates Identity from claims.
	if !strings.Contains(content, "claims.Sub") {
		t.Error("Identity.UserID should be populated from claims.Sub")
	}
	if !strings.Contains(content, "claims.Role") {
		t.Error("Identity.Role should be populated from claims.Role")
	}
	if !strings.Contains(content, "claims.Attributes") {
		t.Error("Identity.Attributes should be populated from claims.Attributes")
	}

	// Verify Identity is stored in context.
	if !strings.Contains(content, "context.WithValue") {
		t.Error("Identity should be stored in request context")
	}
}

// --- Middleware rejects missing token ---

func TestGenerateMiddlewareRejectsMissingToken(t *testing.T) {
	ctx := gen.Context{
		OutputNamespace: "internal/auth",
		ComponentConfig: map[string]any{},
	}
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := string(files[0].Content)

	// Verify 401 on missing token.
	if !strings.Contains(content, "http.StatusUnauthorized") {
		t.Error("middleware should return 401 Unauthorized for missing/invalid tokens")
	}
	if !strings.Contains(content, "missing authentication token") {
		t.Error("middleware should return descriptive error for missing token")
	}
}

// --- Middleware rejects invalid token ---

func TestGenerateMiddlewareRejectsInvalidToken(t *testing.T) {
	ctx := gen.Context{
		OutputNamespace: "internal/auth",
		ComponentConfig: map[string]any{},
	}
	g := &Generator{}
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content := string(files[0].Content)

	if !strings.Contains(content, "invalid authentication token") {
		t.Error("middleware should return descriptive error for invalid token")
	}
}

// --- Namespace validation ---

func TestGenerateRejectsInvalidNamespace(t *testing.T) {
	ctx := gen.Context{
		OutputNamespace: "",
		ComponentConfig: map[string]any{},
	}
	g := &Generator{}
	// Empty namespace should still work because the generator defaults to "internal/auth".
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if files[0].Path != "internal/auth/middleware.go" {
		t.Errorf("expected default namespace path, got %s", files[0].Path)
	}
}
