// Package browserbackend generates a separate browser session and API backend.
package browserbackend

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/format"
	htmlparser "golang.org/x/net/html"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"text/template"

	"github.com/jsell-rh/stego/internal/browserassets"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/httpclient"
)

//go:embed *.tmpl
var sources embed.FS

type Generator struct{}
type asset struct{ Source, Path, Hash string }
type settings struct {
	RuntimeConfigOffset                               int
	Prefix, RolesClaim, LogoutScope, TelemetryService string
	Routes                                            []string
	Assets                                            []asset
	Bundle                                            string `json:"-"`
	ScriptHashes                                      []string
}

var publicPath = regexp.MustCompile(`^/[A-Za-z0-9_./{}-]*$`)

func config(values map[string]any) (settings, error) {
	var s settings
	for key := range values {
		if key != "telemetry_service_name" && key != "asset_bundle" && key != "api_prefix" && key != "routes" && key != "assets" && key != "roles_claim" && key != "logout_scope" {
			return s, fmt.Errorf("unknown browser-backend setting %q", key)
		}
	}
	if value, present := values["telemetry_service_name"]; present {
		var ok bool
		s.TelemetryService, ok = value.(string)
		if !ok || !regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`).MatchString(s.TelemetryService) {
			return s, fmt.Errorf("invalid browser telemetry service name")
		}
	}
	s.Prefix, _ = values["api_prefix"].(string)
	s.LogoutScope = "console"
	if value, present := values["logout_scope"]; present {
		var ok bool
		s.LogoutScope, ok = value.(string)
		if !ok || (s.LogoutScope != "console" && s.LogoutScope != "identity_provider") {
			return s, fmt.Errorf("browser logout_scope must be console or identity_provider")
		}
	}
	s.RolesClaim = "roles"
	if value, present := values["roles_claim"]; present {
		var ok bool
		s.RolesClaim, ok = value.(string)
		if !ok || !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*){0,3}$`).MatchString(s.RolesClaim) || len(s.RolesClaim) > 128 {
			return s, fmt.Errorf("invalid browser roles_claim")
		}
	}
	if !canonicalPath(s.Prefix) || s.Prefix == "/" || strings.ContainsAny(s.Prefix, "{}") || reservedPath(s.Prefix) {
		return s, fmt.Errorf("browser-backend requires a distinct canonical api_prefix")
	}
	routes, ok := values["routes"].([]any)
	if !ok || len(routes) == 0 || len(routes) > 64 {
		return s, fmt.Errorf("browser-backend requires one through 64 routes")
	}
	seen := map[string]bool{}
	for _, raw := range routes {
		route, ok := raw.(string)
		if !ok || !canonicalPath(route) || reservedPath(route) || strings.HasPrefix(route, s.Prefix) || strings.HasPrefix(route, "/assets") || seen[route] {
			return s, fmt.Errorf("invalid or duplicate browser route")
		}
		for _, segment := range strings.Split(route, "/") {
			if strings.ContainsAny(segment, "{}") && segment != "{id}" {
				return s, fmt.Errorf("browser routes permit only the {id} parameter")
			}
		}
		seen[route] = true
		s.Routes = append(s.Routes, route)
	}
	if !seen["/"] {
		return s, fmt.Errorf("browser routes must include /")
	}
	if raw, present := values["asset_bundle"]; present {
		bundle, ok := raw.(string)
		if !ok || gen.ValidatePath(bundle) != nil || bundle == "" || path.Ext(bundle) != ".zip" {
			return s, fmt.Errorf("browser asset_bundle requires a relative ZIP path")
		}
		if _, present := values["assets"]; present {
			return s, fmt.Errorf("browser assets and asset_bundle are mutually exclusive")
		}
		s.Bundle = bundle
		sort.Strings(s.Routes)
		return s, nil
	}
	entries, ok := values["assets"].([]any)
	if !ok || len(entries) == 0 || len(entries) > 128 {
		return s, fmt.Errorf("browser-backend requires one through 128 assets")
	}
	seen = map[string]bool{}
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok || len(entry) != 2 {
			return s, fmt.Errorf("browser asset requires source and path")
		}
		source, ok := entry["source"].(string)
		target, targetOK := entry["path"].(string)
		if !ok || !targetOK || len(source) > 256 || source == "" || path.IsAbs(source) || path.Clean(source) != source || source == ".." || strings.HasPrefix(source, "../") || strings.ContainsAny(source, "\\\x00\r\n") || !canonicalPath(target) || strings.ContainsAny(target, "{}") || seen[target] {
			return s, fmt.Errorf("invalid browser asset")
		}
		if target != "/index.html" && !strings.HasPrefix(target, "/assets/") {
			return s, fmt.Errorf("browser assets must use /index.html or /assets/")
		}
		switch path.Ext(target) {
		case ".html":
			if target != "/index.html" {
				return s, fmt.Errorf("only index.html is supported")
			}
		case ".js", ".css", ".svg", ".png", ".ico", ".woff2":
		default:
			return s, fmt.Errorf("unsupported browser asset type")
		}
		seen[target] = true
		s.Assets = append(s.Assets, asset{Source: source, Path: target})
	}
	if !seen["/index.html"] {
		return s, fmt.Errorf("browser assets must include /index.html")
	}
	sort.Strings(s.Routes)
	sort.Slice(s.Assets, func(i, j int) bool { return s.Assets[i].Path < s.Assets[j].Path })
	return s, nil
}

func reservedPath(value string) bool {
	for _, prefix := range []string{"/telemetry", "/auth", "/assets", "/index.html", "/livez", "/readyz"} {
		if value == prefix || strings.HasPrefix(value, prefix+"/") {
			return true
		}
	}
	return false
}
func canonicalPath(value string) bool {
	return len(value) > 0 && len(value) <= 256 && publicPath.MatchString(value) && path.Clean(value) == value && !strings.Contains(value, "//")
}
func (*Generator) MinimumGoVersion() string { return "1.26.8" }
func (*Generator) InputFiles(values map[string]any) ([]string, error) {
	s, err := config(values)
	if err != nil {
		return nil, err
	}
	if s.Bundle != "" {
		return []string{s.Bundle}, nil
	}
	seen := map[string]bool{}
	var names []string
	for _, a := range s.Assets {
		if !seen[a.Source] {
			seen[a.Source] = true
			names = append(names, a.Source)
		}
	}
	sort.Strings(names)
	return names, nil
}
func (*Generator) ValidateContext(ctx gen.Context) error {
	if err := gen.ValidateGoPackageNamespace(ctx.OutputNamespace); err != nil {
		return err
	}
	if ctx.ModuleName == "" || ctx.OutDirName == "" || ctx.PeerNamespaces["postgres-adapter"] == "" || ctx.PeerNamespaces["otel-tracing"] == "" || ctx.PeerNamespaces["health-check"] == "" {
		return fmt.Errorf("browser-backend requires a module, PostgreSQL, telemetry, and health checks")
	}
	for _, name := range []string{"jwt-auth", "rest-api", "http-application", "grpc-application"} {
		if ctx.PeerNamespaces[name] != "" {
			return fmt.Errorf("browser-backend must run separately from bearer-token APIs")
		}
	}
	_, _, err := resolveAssets(ctx)
	return err
}
func resolveAssets(ctx gen.Context) (settings, map[string][]byte, error) {
	s, err := config(ctx.ComponentConfig)
	if err != nil {
		return s, nil, err
	}
	content := ctx.Inputs
	sources, err := new(Generator).InputFiles(ctx.ComponentConfig)
	if err != nil {
		return s, nil, err
	}
	for _, source := range sources {
		if source == ctx.OutDirName || strings.HasPrefix(source, ctx.OutDirName+"/") {
			return s, nil, fmt.Errorf("browser asset sources must be outside generated output")
		}
	}
	if s.Bundle != "" {
		bundle, err := browserassets.Decode(ctx.Inputs[s.Bundle])
		if err != nil {
			return s, nil, err
		}
		content = map[string][]byte{}
		for _, item := range bundle {
			source := "@bundle/" + item.Path
			s.Assets = append(s.Assets, asset{Source: source, Path: "/" + item.Path})
			content[source] = item.Data
		}
	}
	total := 0
	names := map[string]bool{}
	var index []byte
	for _, a := range s.Assets {
		data, ok := content[a.Source]
		total += len(data)
		if !ok || len(data) == 0 || len(data) > browserassets.MaxFile || total > browserassets.MaxTotal {
			return s, nil, fmt.Errorf("browser asset input is missing or exceeds its limit")
		}
		names[strings.TrimPrefix(a.Path, "/")] = true
		if a.Path == "/index.html" {
			index = data
		}
	}
	if s.TelemetryService != "" {
		s.RuntimeConfigOffset, err = runtimeConfigOffset(index)
		if err != nil {
			return s, nil, err
		}
	}
	s.ScriptHashes, err = browserassets.ScriptHashes(index, names)
	return s, content, err
}

func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := g.ValidateContext(ctx); err != nil {
		return nil, nil, err
	}
	s, contentBySource, err := resolveAssets(ctx)
	if err != nil {
		return nil, nil, err
	}
	root := path.Join(ctx.ModuleName, ctx.OutDirName, ctx.OutputNamespace)
	var files []gen.File
	for i, a := range s.Assets {
		content := contentBySource[a.Source]
		sum := sha256.Sum256(content)
		s.Assets[i].Hash = hex.EncodeToString(sum[:])
		files = append(files, gen.File{Path: path.Join(ctx.OutputNamespace, "public", strings.TrimPrefix(a.Path, "/")), Content: append([]byte(nil), content...)})
	}
	configuration, _ := json.Marshal(s)
	data := struct{ Package, Client, Config, UnicodeValidation, Telemetry string }{path.Base(ctx.OutputNamespace), root + "/client", string(configuration), gen.UnicodeEscapeValidation, path.Join(ctx.ModuleName, ctx.OutDirName, ctx.PeerNamespaces["otel-tracing"])}
	entries, _ := sources.ReadDir(".")
	for _, entry := range entries {
		source, err := sources.ReadFile(entry.Name())
		if err != nil {
			return nil, nil, err
		}
		tmpl, err := template.New(entry.Name()).Parse(string(source))
		if err != nil {
			return nil, nil, err
		}
		var out bytes.Buffer
		if err := tmpl.Execute(&out, data); err != nil {
			return nil, nil, err
		}
		name := strings.TrimSuffix(entry.Name(), ".tmpl")
		content := out.Bytes()
		if strings.HasSuffix(name, ".go") {
			content, err = format.Source(content)
			if err != nil {
				return nil, nil, fmt.Errorf("format browser backend %s: %w", name, err)
			}
		}
		files = append(files, gen.File{Path: path.Join(ctx.OutputNamespace, name), Content: content})
	}
	client, err := httpclient.Render(path.Join(ctx.OutputNamespace, "client"), path.Join(ctx.ModuleName, ctx.OutDirName, ctx.PeerNamespaces["otel-tracing"]))
	if err != nil {
		return nil, nil, err
	}
	files = append(files, client)
	if err := gen.ValidateNamespace(ctx.OutputNamespace, files); err != nil {
		return nil, nil, err
	}
	wiring := &gen.Wiring{NeedsDB: true, Imports: []string{ctx.OutputNamespace}, Constructors: []string{path.Base(ctx.OutputNamespace) + ".NewBrowserBackend()"}, ConstructorResources: map[int][]gen.Resource{0: {gen.ServiceContext, gen.SQLDatabase}}, ConstructorReturnsError: map[int]bool{0: true}, ConstructorDeferCalls: map[int]string{0: "Close()"}, BackgroundTasks: []int{0}, Routes: []string{`mux.Handle("/", browserBackend)`}, GoModRequires: map[string]string{"github.com/coreos/go-oidc/v3": "v3.21.0"}}
	return files, wiring, nil
}

func runtimeConfigOffset(data []byte) (int, error) {
	tokenizer := htmlparser.NewTokenizer(bytes.NewReader(data))
	offset, head, heads := 0, 0, 0
	for {
		kind := tokenizer.Next()
		offset += len(tokenizer.Raw())
		if kind == htmlparser.ErrorToken {
			if tokenizer.Err() != io.EOF {
				return 0, fmt.Errorf("invalid browser HTML")
			}
			break
		}
		if kind != htmlparser.StartTagToken && kind != htmlparser.SelfClosingTagToken {
			continue
		}
		token := tokenizer.Token()
		if token.Data == "head" {
			heads++
			head = offset
		}
		if token.Data == "meta" {
			for _, attr := range token.Attr {
				if strings.EqualFold(attr.Key, "name") && strings.EqualFold(attr.Val, "stego-runtime-config") {
					return 0, fmt.Errorf("browser runtime configuration is compiler-owned")
				}
			}
		}
	}
	if heads != 1 {
		return 0, fmt.Errorf("browser telemetry requires one explicit HTML head element")
	}
	return head, nil
}
