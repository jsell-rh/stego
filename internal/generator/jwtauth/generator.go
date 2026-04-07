// Package jwtauth implements the jwt-auth component Generator. It produces
// JWT authentication middleware that validates tokens, extracts identity
// information from claims, and populates a context-scoped Identity struct.
package jwtauth

import (
	"bytes"
	"fmt"
	"go/format"
	"path"

	"github.com/jsell-rh/stego/internal/gen"
)

// Generator produces the jwt-auth component's generated code.
type Generator struct{}

// Generate produces a Go middleware file that validates JWT tokens from a
// configurable HTTP header and populates an Identity struct from the token's
// claims. Returns wiring instructions for main.go assembly.
func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	ns := ctx.OutputNamespace
	if ns == "" {
		ns = "internal/auth"
	}
	pkg := path.Base(ns)

	header := "Authorization"
	if h, ok := ctx.ComponentConfig["header"]; ok {
		if s, ok := h.(string); ok && s != "" {
			header = s
		}
	}

	f, err := generateMiddleware(ns, pkg, header)
	if err != nil {
		return nil, nil, err
	}

	files := []gen.File{f}

	middlewareIdx := 0
	wiring := &gen.Wiring{
		Imports:               []string{ns},
		Constructors:          []string{fmt.Sprintf("%s.NewAuthMiddleware()", pkg)},
		MiddlewareConstructor: &middlewareIdx,
	}

	if err := gen.ValidateNamespace(ns, files); err != nil {
		return nil, nil, err
	}

	return files, wiring, nil
}

// generateMiddleware produces the middleware.go file containing the Identity
// struct, JWT validation logic, context helpers, and the middleware constructor.
func generateMiddleware(ns, pkg, header string) (gen.File, error) {
	var buf bytes.Buffer

	fmt.Fprintf(&buf, "package %s\n\n", pkg)
	fmt.Fprintf(&buf, "import (\n")
	fmt.Fprintf(&buf, "\t\"context\"\n")
	fmt.Fprintf(&buf, "\t\"encoding/base64\"\n")
	fmt.Fprintf(&buf, "\t\"encoding/json\"\n")
	fmt.Fprintf(&buf, "\t\"fmt\"\n")
	fmt.Fprintf(&buf, "\t\"net/http\"\n")
	fmt.Fprintf(&buf, "\t\"strings\"\n")
	fmt.Fprintf(&buf, ")\n\n")

	// Identity struct matching stego.common.Identity proto.
	fmt.Fprintf(&buf, "// Identity represents the caller's identity extracted from a JWT token.\n")
	fmt.Fprintf(&buf, "// Matches the stego.common.Identity proto definition.\n")
	fmt.Fprintf(&buf, "type Identity struct {\n")
	fmt.Fprintf(&buf, "\tUserID     string            `json:\"user_id\"`\n")
	fmt.Fprintf(&buf, "\tRole       string            `json:\"role\"`\n")
	fmt.Fprintf(&buf, "\tAttributes map[string]string `json:\"attributes\"`\n")
	fmt.Fprintf(&buf, "}\n\n")

	// Context key type to avoid collisions.
	fmt.Fprintf(&buf, "type contextKey string\n\n")
	fmt.Fprintf(&buf, "const identityKey contextKey = \"stego.identity\"\n\n")

	// IdentityFromContext helper.
	fmt.Fprintf(&buf, "// IdentityFromContext retrieves the Identity from the request context.\n")
	fmt.Fprintf(&buf, "// Returns a zero Identity if none is present.\n")
	fmt.Fprintf(&buf, "func IdentityFromContext(ctx context.Context) Identity {\n")
	fmt.Fprintf(&buf, "\tif id, ok := ctx.Value(identityKey).(Identity); ok {\n")
	fmt.Fprintf(&buf, "\t\treturn id\n")
	fmt.Fprintf(&buf, "\t}\n")
	fmt.Fprintf(&buf, "\treturn Identity{}\n")
	fmt.Fprintf(&buf, "}\n\n")

	// jwtClaims struct for decoding the JWT payload.
	fmt.Fprintf(&buf, "// jwtClaims represents the JWT payload claims used for identity extraction.\n")
	fmt.Fprintf(&buf, "type jwtClaims struct {\n")
	fmt.Fprintf(&buf, "\tSub        string            `json:\"sub\"`\n")
	fmt.Fprintf(&buf, "\tRole       string            `json:\"role\"`\n")
	fmt.Fprintf(&buf, "\tAttributes map[string]string `json:\"attributes\"`\n")
	fmt.Fprintf(&buf, "}\n\n")

	// parseJWT decodes claims without cryptographic verification.
	fmt.Fprintf(&buf, "// parseJWT decodes the payload section of a JWT token and extracts claims.\n")
	fmt.Fprintf(&buf, "// Cryptographic signature verification is deferred to the deployment layer\n")
	fmt.Fprintf(&buf, "// (e.g. an API gateway or sidecar proxy).\n")
	fmt.Fprintf(&buf, "func parseJWT(token string) (jwtClaims, error) {\n")
	fmt.Fprintf(&buf, "\tparts := strings.Split(token, \".\")\n")
	fmt.Fprintf(&buf, "\tif len(parts) != 3 {\n")
	fmt.Fprintf(&buf, "\t\treturn jwtClaims{}, fmt.Errorf(\"invalid JWT format: expected 3 parts, got %%d\", len(parts))\n")
	fmt.Fprintf(&buf, "\t}\n")
	fmt.Fprintf(&buf, "\tpayload, err := base64.RawURLEncoding.DecodeString(parts[1])\n")
	fmt.Fprintf(&buf, "\tif err != nil {\n")
	fmt.Fprintf(&buf, "\t\treturn jwtClaims{}, fmt.Errorf(\"invalid JWT payload encoding: %%w\", err)\n")
	fmt.Fprintf(&buf, "\t}\n")
	fmt.Fprintf(&buf, "\tvar claims jwtClaims\n")
	fmt.Fprintf(&buf, "\tif err := json.Unmarshal(payload, &claims); err != nil {\n")
	fmt.Fprintf(&buf, "\t\treturn jwtClaims{}, fmt.Errorf(\"invalid JWT payload JSON: %%w\", err)\n")
	fmt.Fprintf(&buf, "\t}\n")
	fmt.Fprintf(&buf, "\treturn claims, nil\n")
	fmt.Fprintf(&buf, "}\n\n")

	// extractToken retrieves the token from the configured header.
	fmt.Fprintf(&buf, "// extractToken retrieves the JWT token from the %s header.\n", header)
	fmt.Fprintf(&buf, "// For tokens prefixed with \"Bearer \", the prefix is stripped.\n")
	fmt.Fprintf(&buf, "func extractToken(r *http.Request) string {\n")
	fmt.Fprintf(&buf, "\tv := r.Header.Get(%q)\n", header)
	fmt.Fprintf(&buf, "\tif v == \"\" {\n")
	fmt.Fprintf(&buf, "\t\treturn \"\"\n")
	fmt.Fprintf(&buf, "\t}\n")
	fmt.Fprintf(&buf, "\tif strings.HasPrefix(v, \"Bearer \") {\n")
	fmt.Fprintf(&buf, "\t\treturn strings.TrimPrefix(v, \"Bearer \")\n")
	fmt.Fprintf(&buf, "\t}\n")
	fmt.Fprintf(&buf, "\treturn v\n")
	fmt.Fprintf(&buf, "}\n\n")

	// NewAuthMiddleware constructor.
	fmt.Fprintf(&buf, "// NewAuthMiddleware returns HTTP middleware that validates JWT tokens from\n")
	fmt.Fprintf(&buf, "// the %s header and populates the request context with the caller's Identity.\n", header)
	fmt.Fprintf(&buf, "func NewAuthMiddleware() func(http.Handler) http.Handler {\n")
	fmt.Fprintf(&buf, "\treturn func(next http.Handler) http.Handler {\n")
	fmt.Fprintf(&buf, "\t\treturn http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {\n")
	fmt.Fprintf(&buf, "\t\t\ttoken := extractToken(r)\n")
	fmt.Fprintf(&buf, "\t\t\tif token == \"\" {\n")
	fmt.Fprintf(&buf, "\t\t\t\thttp.Error(w, \"missing authentication token\", http.StatusUnauthorized)\n")
	fmt.Fprintf(&buf, "\t\t\t\treturn\n")
	fmt.Fprintf(&buf, "\t\t\t}\n")
	fmt.Fprintf(&buf, "\t\t\tclaims, err := parseJWT(token)\n")
	fmt.Fprintf(&buf, "\t\t\tif err != nil {\n")
	fmt.Fprintf(&buf, "\t\t\t\thttp.Error(w, \"invalid authentication token\", http.StatusUnauthorized)\n")
	fmt.Fprintf(&buf, "\t\t\t\treturn\n")
	fmt.Fprintf(&buf, "\t\t\t}\n")
	fmt.Fprintf(&buf, "\t\t\tid := Identity{\n")
	fmt.Fprintf(&buf, "\t\t\t\tUserID:     claims.Sub,\n")
	fmt.Fprintf(&buf, "\t\t\t\tRole:       claims.Role,\n")
	fmt.Fprintf(&buf, "\t\t\t\tAttributes: claims.Attributes,\n")
	fmt.Fprintf(&buf, "\t\t\t}\n")
	fmt.Fprintf(&buf, "\t\t\tctx := context.WithValue(r.Context(), identityKey, id)\n")
	fmt.Fprintf(&buf, "\t\t\tnext.ServeHTTP(w, r.WithContext(ctx))\n")
	fmt.Fprintf(&buf, "\t\t})\n")
	fmt.Fprintf(&buf, "\t}\n")
	fmt.Fprintf(&buf, "}\n")

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return gen.File{}, fmt.Errorf("formatting middleware: %w", err)
	}

	return gen.File{
		Path:    path.Join(ns, "middleware.go"),
		Content: formatted,
	}, nil
}
