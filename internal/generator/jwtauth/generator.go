// Package jwtauth generates verified JWT authentication.
package jwtauth

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/format"
	"net/textproto"
	"path"
	"strings"
	"text/template"

	"github.com/jsell-rh/stego/internal/gen"
)

//go:embed middleware.go.tmpl
var middlewareTemplate string

//go:embed jwks.go.tmpl
var jwksTemplate string

//go:embed grants.go.tmpl
var grantsTemplate string

// Generator produces the jwt-auth component.
type Generator struct{}

func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	mode := setting(ctx, "mode", "middleware")
	if mode != "middleware" && mode != "verifier" {
		return nil, nil, fmt.Errorf("authentication mode must be middleware or verifier")
	}
	rolesClaim := ""
	if value, present := ctx.ComponentConfig["roles_claim"]; present {
		var ok bool
		rolesClaim, ok = value.(string)
		if !ok || !validRolesClaim(rolesClaim) {
			return nil, nil, fmt.Errorf("roles_claim must be a dotted claim path or an empty string")
		}
	}
	ns := ctx.OutputNamespace
	if ns == "" {
		ns = "internal/auth"
	}
	header := setting(ctx, "header", "Authorization")
	if textproto.CanonicalMIMEHeaderKey(header) == "" || strings.ContainsAny(header, " \t\r\n:") {
		return nil, nil, fmt.Errorf("invalid authentication header %q", header)
	}
	for _, ch := range header {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", ch)) {
			return nil, nil, fmt.Errorf("invalid authentication header %q", header)
		}
	}
	header = textproto.CanonicalMIMEHeaderKey(header)
	errorType := "about:blank"
	if ctx.ErrorTypeBase != "" {
		errorType = ctx.ErrorTypeBase + "unauthorized"
	}
	data := struct{ Package, Header, Issuer, Audience, KeyFile, ErrorType, ErrorCode, RolesClaim string }{
		Package: path.Base(ns), Header: header,
		Issuer: setting(ctx, "issuer", ""), Audience: setting(ctx, "audience", ""),
		KeyFile: setting(ctx, "public_key_file", ""), ErrorType: errorType,
		ErrorCode:  strings.ToUpper(strings.ReplaceAll(ctx.ServiceName, "-", "")) + "-AUT-001",
		RolesClaim: rolesClaim,
	}
	tmpl, err := template.New("middleware").Parse(middlewareTemplate)
	if err != nil {
		return nil, nil, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, nil, err
	}
	source, err := format.Source(append(buf.Bytes(), []byte(gen.UnicodeEscapeValidation)...))
	if err != nil {
		return nil, nil, fmt.Errorf("formatting authentication code: %w", err)
	}
	files := []gen.File{{Path: path.Join(ns, "middleware.go"), Content: source}}
	keys, err := template.New("jwks").Parse(jwksTemplate)
	if err != nil {
		return nil, nil, err
	}
	buf.Reset()
	if err := keys.Execute(&buf, data); err != nil {
		return nil, nil, err
	}
	keySource, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, nil, err
	}
	files = append(files, gen.File{Path: path.Join(ns, "jwks.go"), Content: keySource})
	grants, err := template.New("grants").Parse(grantsTemplate)
	if err != nil {
		return nil, nil, err
	}
	buf.Reset()
	if err := grants.Execute(&buf, data); err != nil {
		return nil, nil, err
	}
	grantSource, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, nil, err
	}
	files = append(files, gen.File{Path: path.Join(ns, "grants.go"), Content: grantSource})
	if err := gen.ValidateNamespace(ns, files); err != nil {
		return nil, nil, err
	}
	middlewareIndex := 0
	wiring := &gen.Wiring{
		Imports: []string{ns}, Constructors: []string{data.Package + ".NewAuthMiddleware()"},
		ConstructorReturnsError: map[int]bool{0: true},
		MiddlewareConstructor:   &middlewareIndex, MiddlewareWrapExpr: "%s(%s)",
		GoModRequires: map[string]string{"github.com/golang-jwt/jwt/v5": "v5.3.1"},
	}
	if mode == "verifier" {
		wiring.Constructors = []string{data.Package + ".NewVerifierFromEnvironment()"}
		wiring.MiddlewareConstructor = nil
		wiring.MiddlewareWrapExpr = ""
	}
	return files, wiring, nil
}

func validRolesClaim(value string) bool {
	if value == "" {
		return true
	}
	parts := strings.Split(value, ".")
	if len(value) > 128 || len(parts) > 8 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, ch := range part {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-') {
				return false
			}
		}
	}
	return true
}

func setting(ctx gen.Context, name, fallback string) string {
	if value, ok := ctx.ComponentConfig[name].(string); ok && value != "" {
		return value
	}
	return fallback
}
