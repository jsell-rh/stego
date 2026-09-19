// Package rhssoauth adds SSO claim mapping to the shared verified JWT runtime.
package rhssoauth

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/format"
	"net/url"
	"path"
	"strings"
	"text/template"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/jwtauth"
)

//go:embed sso.go.tmpl
var ssoTemplate string

type Generator struct{}

// MinimumGoVersion includes the verified JWT dependency and cancellation API.
func (*Generator) MinimumGoVersion() string { return "1.21.0" }

func (*Generator) ValidateContext(ctx gen.Context) error {
	if err := new(jwtauth.Generator).ValidateContext(ctx); err != nil {
		return err
	}
	for _, name := range []string{"issuer", "audience", "jwk_cert_url", "jwk_cert_file", "jwk_ca_file"} {
		value, exists := ctx.ComponentConfig[name]
		if !exists {
			continue
		}
		s, ok := value.(string)
		if !ok || strings.TrimSpace(s) != s {
			return fmt.Errorf("%s must be a string without surrounding whitespace", name)
		}
		if s != "" && (name == "issuer" || name == "jwk_cert_url") {
			u, err := url.Parse(s)
			if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
				return fmt.Errorf("%s must be an HTTPS URL", name)
			}
		}
	}
	_, err := publicPaths(ctx)
	return err
}

func publicPaths(ctx gen.Context) ([]string, error) {
	paths := []string{"/healthcheck", "/metrics"}
	if value, exists := ctx.ComponentConfig["public_paths"]; exists {
		list, ok := value.([]any)
		if !ok {
			return nil, fmt.Errorf("public_paths must be a list")
		}
		paths = nil
		for _, value := range list {
			s, ok := value.(string)
			if !ok || !strings.HasPrefix(s, "/") || path.Clean(s) != s || strings.ContainsAny(s, "?%#\\\r\n\t ") {
				return nil, fmt.Errorf("public_paths must contain exact absolute URL paths")
			}
			paths = append(paths, s)
		}
	}
	for i, p := range paths {
		if ctx.BasePath != "" && p != ctx.BasePath && !strings.HasPrefix(p, ctx.BasePath+"/") {
			paths[i] = ctx.BasePath + p
		}
	}
	seen := map[string]bool{}
	unique := []string{}
	for _, p := range append(paths, ctx.BasePath+"/openapi") {
		if !seen[p] {
			unique = append(unique, p)
			seen[p] = true
		}
	}
	return unique, nil
}

func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := g.ValidateContext(ctx); err != nil {
		return nil, nil, err
	}
	ns := ctx.OutputNamespace
	if ns == "" {
		ns = "internal/auth"
	}
	shared := ctx
	// Retain the SSO error code prefix while using the common error response.
	shared.ServiceName = strings.TrimSuffix(strings.TrimSuffix(ctx.ServiceName, "-api"), "-service")
	files, wiring, err := new(jwtauth.Generator).Generate(shared)
	if err != nil {
		return nil, nil, err
	}
	paths, err := publicPaths(ctx)
	if err != nil {
		return nil, nil, err
	}
	setting := func(name, fallback string) string {
		if s, ok := ctx.ComponentConfig[name].(string); ok && s != "" {
			return s
		}
		return fallback
	}
	data := struct {
		Package, Issuer, Audience, URL, File, CAFile, Tracing string
		PublicPaths                                           []string
	}{
		path.Base(ns), setting("issuer", ""), setting("audience", ""),
		setting("jwk_cert_url", "https://sso.redhat.com/auth/realms/redhat-external/protocol/openid-connect/certs"),
		setting("jwk_cert_file", ""), setting("jwk_ca_file", ""), "", paths,
	}
	if peer := ctx.PeerNamespaces["otel-tracing"]; peer != "" {
		data.Tracing = path.Join(ctx.ModuleName, ctx.OutDirName, peer)
	}
	tmpl, err := template.New("sso").Parse(ssoTemplate)
	if err != nil {
		return nil, nil, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, nil, err
	}
	source, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, nil, err
	}
	files = append(files, gen.File{Path: path.Join(ns, "sso.go"), Content: source})
	contextFile, err := generateContext(ns, path.Base(ns))
	if err != nil {
		return nil, nil, err
	}
	files = append(files, contextFile)
	index := 0
	wiring.Constructors = []string{path.Base(ns) + ".NewJWTHandlerWithContext()"}
	wiring.ConstructorResources = map[int][]gen.Resource{0: {gen.ServiceContext}}
	if data.Tracing != "" {
		wiring.Constructors = []string{path.Base(ns) + ".NewJWTHandlerWithTelemetry(tracingRuntime)"}
		wiring.ConstructorDeps = map[int][]string{0: {"tracingRuntime"}}
	}
	wiring.ConstructorReturnsError = map[int]bool{0: true}
	wiring.MiddlewareConstructor = &index
	wiring.MiddlewareWrapExpr = "%s.Build()(%s)"
	wiring.ConstructorDeferCalls = map[int]string{0: "Stop()"}
	if err := gen.ValidateNamespace(ns, files); err != nil {
		return nil, nil, err
	}
	return files, wiring, nil
}

func deriveErrorPrefix(serviceName string) string {
	name := strings.TrimSuffix(strings.TrimSuffix(serviceName, "-api"), "-service")
	return strings.ToUpper(strings.ReplaceAll(name, "-", ""))
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
	fmt.Fprintf(&buf, "\tjwt \"github.com/golang-jwt/jwt/v5\"\n")
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
