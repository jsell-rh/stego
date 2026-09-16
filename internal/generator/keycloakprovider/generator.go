// Package keycloakprovider generates a realm-bound Keycloak provider.
package keycloakprovider

import (
	"bytes"
	"embed"
	"fmt"
	"github.com/jsell-rh/stego/internal/gen"
	"go/format"
	"path"
	"text/template"
)

//go:embed *.tmpl
var sources embed.FS

type Generator struct{}

func (*Generator) MinimumGoVersion() string { return "1.26.8" }
func (*Generator) ValidateContext(ctx gen.Context) error {
	if err := gen.ValidateGoPackageNamespace(ctx.OutputNamespace); err != nil {
		return err
	}
	if len(ctx.ComponentConfig) != 0 {
		return fmt.Errorf("keycloak-provider accepts no component settings")
	}
	peer := ctx.PeerNamespaces["http-application"]
	if ctx.ModuleName == "" || peer == "" || gen.ValidateGoPackageNamespace(peer) != nil {
		return fmt.Errorf("keycloak-provider requires the generated HTTP application client")
	}
	if ns := ctx.PeerNamespaces["controller"]; ns != "" {
		if err := gen.ValidateGoPackageNamespace(ns); err != nil {
			return err
		}
	}
	trusted := false
	for _, name := range []string{"jwt-auth", "rh-sso-auth"} {
		ns := ctx.PeerNamespaces[name]
		if ns != "" && gen.ValidateGoPackageNamespace(ns) == nil && ctx.AuthPackage == path.Join(ctx.ModuleName, ctx.OutDirName, ns) {
			trusted = true
		}
	}
	if !trusted {
		return fmt.Errorf("keycloak-provider requires the generated JWT verifier")
	}
	return nil
}
func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := g.ValidateContext(ctx); err != nil {
		return nil, nil, err
	}
	data := struct{ Package, Transport, UnicodeValidation, Auth, Controller string }{path.Base(ctx.OutputNamespace), path.Join(ctx.ModuleName, ctx.OutDirName, ctx.PeerNamespaces["http-application"], "client"), gen.UnicodeEscapeValidation, ctx.AuthPackage, ""}
	var files []gen.File
	names := []string{"client.go", "models.go", "clients.go", "service_accounts.go", "roles.go", "scopes.go", "mappers.go", "client_configuration.go", "native_clients.go", "native_access.go", "service_account_tokens.go", "access_lifecycle.go", "service_account_access.go", "ownership_migration.go"}
	if ns := ctx.PeerNamespaces["controller"]; ns != "" {
		data.Controller = path.Join(ctx.ModuleName, ctx.OutDirName, ns)
		names = append(names, "client_cursor.go", "client_lifecycle.go", "lifecycle_gate.go", "native_lifecycle.go", "service_account_lifecycle.go")
	}
	for _, name := range names {
		input, err := sources.ReadFile(name + ".tmpl")
		if err != nil {
			return nil, nil, err
		}
		t, err := template.New(name).Parse(string(input))
		if err != nil {
			return nil, nil, err
		}
		var out bytes.Buffer
		if err = t.Execute(&out, data); err != nil {
			return nil, nil, err
		}
		code, err := format.Source(out.Bytes())
		if err != nil {
			return nil, nil, fmt.Errorf("keycloak-provider %s: %w", name, err)
		}
		files = append(files, gen.File{Path: path.Join(ctx.OutputNamespace, name), Content: code})
	}
	return files, &gen.Wiring{}, gen.ValidateNamespace(ctx.OutputNamespace, files)
}
