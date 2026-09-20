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

	"github.com/jsell-rh/stego/internal/browserapplication"
	"github.com/jsell-rh/stego/internal/browserassets"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/browserdom"
	"github.com/jsell-rh/stego/internal/generator/browseridentity"
	"github.com/jsell-rh/stego/internal/generator/httpclient"
	"github.com/jsell-rh/stego/internal/generator/typescriptsdk"
)

//go:embed *.tmpl
var sources embed.FS

// LocalApplicationPort is set by compiler integration code, not service YAML.
// The application must share the browser backend's network namespace and bind
// only to 127.0.0.1. Deployment integration must enforce that boundary.
type Generator struct{ LocalApplicationPort int }

const mountPattern = "/"

func (*Generator) HTTPRoutes(gen.Context) ([]gen.HTTPRoute, error) {
	return []gen.HTTPRoute{{Pattern: mountPattern}}, nil
}

type asset struct{ Source, Path, Hash string }
type settings struct {
	DynamicStyles                                                 bool
	RuntimeConfigOffset                                           int
	Prefix, RolesClaim, LogoutScope, TelemetryService, OAuthScope string
	Routes                                                        []string
	Assets                                                        []asset
	Bundle                                                        string `json:"-"`
	ScriptHashes                                                  []string
}

var publicPath = regexp.MustCompile(`^/[A-Za-z0-9_./{}-]*$`)
var scopeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._/-]{0,127}$`)

func (g *Generator) config(values map[string]any) (settings, error) {
	var s settings
	if g.LocalApplicationPort != 0 && (g.LocalApplicationPort < 1024 || g.LocalApplicationPort > 65535) {
		return s, fmt.Errorf("local application port must be 1024 through 65535")
	}
	for key := range values {
		if key != "dynamic_styles" && key != "additional_scopes" && key != "telemetry_service_name" && key != "asset_bundle" && key != "api_prefix" && key != "routes" && key != "assets" && key != "roles_claim" && key != "logout_scope" {
			return s, fmt.Errorf("unknown browser-backend setting %q", key)
		}
	}
	if value, present := values["dynamic_styles"]; present {
		var ok bool
		s.DynamicStyles, ok = value.(bool)
		if !ok {
			return s, fmt.Errorf("browser dynamic_styles requires a boolean")
		}
	}
	s.OAuthScope = "openid"
	if value, present := values["additional_scopes"]; present {
		entries, ok := value.([]any)
		if !ok || len(entries) > 7 {
			return s, fmt.Errorf("browser additional_scopes requires zero through seven scopes")
		}
		seen := map[string]bool{"openid": true, "offline_access": true}
		var scopes []string
		for _, raw := range entries {
			scope, ok := raw.(string)
			if !ok || !scopeName.MatchString(scope) || seen[scope] {
				return s, fmt.Errorf("invalid, duplicate, or unsupported browser scope")
			}
			seen[scope] = true
			scopes = append(scopes, scope)
		}
		sort.Strings(scopes)
		if len(scopes) > 0 {
			s.OAuthScope += " " + strings.Join(scopes, " ")
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
	_, hasAssets := values["assets"]
	_, hasBundle := values["asset_bundle"]
	if g.LocalApplicationPort != 0 && !hasAssets && !hasBundle {
		if s.TelemetryService != "" || s.DynamicStyles {
			return s, fmt.Errorf("browser telemetry and dynamic styles require captured assets")
		}
		sort.Strings(s.Routes)
		return s, nil
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
		case ".js", ".css", ".svg", ".png", ".ico", ".woff2", ".ttf":
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
func (g *Generator) InputFiles(values map[string]any) ([]gen.InputFile, error) {
	s, err := g.config(values)
	if err != nil {
		return nil, err
	}
	if s.Bundle != "" {
		return []gen.InputFile{{Path: s.Bundle, MaxBytes: browserassets.MaxBundle}}, nil
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
	result := gen.SourceInputs(names)
	for i := range result {
		result[i].MaxBytes = browserassets.MaxFile
	}
	return result, nil
}
func (g *Generator) configured(ctx gen.Context) (*Generator, error) {
	application, err := browserapplication.Resolve(ctx, ctx.PeerConfigs["kubernetes-service"])
	if err != nil {
		return nil, err
	}
	if application == nil {
		return g, nil
	}
	if g.LocalApplicationPort != 0 && g.LocalApplicationPort != application.Port {
		return nil, fmt.Errorf("local application port differs from deployment")
	}
	return &Generator{LocalApplicationPort: application.Port}, nil
}
func (g *Generator) ValidateContext(ctx gen.Context) error {
	configured, err := g.configured(ctx)
	if err != nil {
		return err
	}
	return configured.validateContext(ctx)
}
func (g *Generator) validateContext(ctx gen.Context) error {
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
	_, _, err := g.resolveAssets(ctx)
	return err
}
func (g *Generator) resolveAssets(ctx gen.Context) (settings, map[string][]byte, error) {
	s, err := g.config(ctx.ComponentConfig)
	if err != nil {
		return s, nil, err
	}
	client, present := ctx.PeerConfigs["browser-telemetry"]
	if present || ctx.PeerNamespaces["browser-telemetry"] != "" {
		s.TelemetryService, err = browseridentity.Resolve(ctx.ServiceName, client, ctx.ComponentConfig)
		if err != nil {
			return s, nil, err
		}
	}
	content := ctx.Inputs
	sources, err := g.InputFiles(ctx.ComponentConfig)
	if err != nil {
		return s, nil, err
	}
	for _, input := range sources {
		source := input.Path
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
	if s.TelemetryService != "" || s.DynamicStyles {
		s.RuntimeConfigOffset, err = runtimeConfigOffset(index)
		if err != nil {
			return s, nil, err
		}
	}
	if len(s.Assets) != 0 {
		s.ScriptHashes, err = browserassets.ScriptHashes(index, names)
	}
	return s, content, err
}

func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	configured, err := g.configured(ctx)
	if err != nil {
		return nil, nil, err
	}
	return configured.generate(ctx)
}
func (g *Generator) generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := g.validateContext(ctx); err != nil {
		return nil, nil, err
	}
	s, contentBySource, err := g.resolveAssets(ctx)
	if err != nil {
		return nil, nil, err
	}
	root := path.Join(ctx.ModuleName, ctx.OutDirName, ctx.OutputNamespace)
	var files []gen.File
	sessionFiles, err := typescriptsdk.BrowserSessionFiles(path.Join(ctx.OutputNamespace, "sessionclient"), s.Prefix)
	if err != nil {
		return nil, nil, err
	}
	files = append(files, sessionFiles...)
	if s.DynamicStyles {
		domFiles, err := browserdom.Files(path.Join(ctx.OutputNamespace, "dom"))
		if err != nil {
			return nil, nil, err
		}
		files = append(files, domFiles...)
	}
	for i, a := range s.Assets {
		content := contentBySource[a.Source]
		sum := sha256.Sum256(content)
		s.Assets[i].Hash = hex.EncodeToString(sum[:])
		files = append(files, gen.File{Path: path.Join(ctx.OutputNamespace, "public", strings.TrimPrefix(a.Path, "/")), Content: append([]byte(nil), content...)})
	}
	configuration, _ := json.Marshal(s)
	data := struct {
		Package, Client, Config, UnicodeValidation, Telemetry, Schema, SchemaSQL string
		LocalApplication, EmbeddedAssets                                         bool
	}{path.Base(ctx.OutputNamespace), root + "/client", string(configuration), gen.UnicodeEscapeValidation, path.Join(ctx.ModuleName, ctx.OutDirName, ctx.PeerNamespaces["otel-tracing"]), root + "/schema", "", g.LocalApplicationPort != 0, len(s.Assets) != 0}
	schemaSQL, err := sources.ReadFile("schema.sql.tmpl")
	if err != nil {
		return nil, nil, err
	}
	data.SchemaSQL = string(schemaSQL)
	entries, _ := sources.ReadDir(".")
	for _, entry := range entries {
		if entry.Name() == "socket.go.tmpl" && g.LocalApplicationPort == 0 {
			continue
		}
		source, err := sources.ReadFile(entry.Name())
		if err != nil {
			return nil, nil, err
		}
		tmpl, err := template.New(entry.Name()).Parse(string(source))
		if err != nil {
			return nil, nil, err
		}
		var out bytes.Buffer
		values := data
		if entry.Name() == "schema_package.go.tmpl" {
			values.Package = "schema"
		}
		if err := tmpl.Execute(&out, values); err != nil {
			return nil, nil, err
		}
		name := strings.TrimSuffix(entry.Name(), ".tmpl")
		if name == "schema_package.go" {
			name = "schema/schema.go"
		}
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
	if g.LocalApplicationPort != 0 {
		client, err = httpclient.RenderLocalApplication(path.Join(ctx.OutputNamespace, "client"), path.Join(ctx.ModuleName, ctx.OutDirName, ctx.PeerNamespaces["otel-tracing"]), g.LocalApplicationPort)
	}
	if err != nil {
		return nil, nil, err
	}
	files = append(files, client)
	if g.LocalApplicationPort != 0 {
		socket, err := httpclient.RenderWebSocket(path.Join(ctx.OutputNamespace, "client"), path.Join(ctx.ModuleName, ctx.OutDirName, ctx.PeerNamespaces["otel-tracing"]))
		if err != nil {
			return nil, nil, err
		}
		files = append(files, socket)
	}
	if err := gen.ValidateNamespace(ctx.OutputNamespace, files); err != nil {
		return nil, nil, err
	}
	wiring := &gen.Wiring{NeedsDB: true, Imports: []string{ctx.OutputNamespace}, Constructors: []string{path.Base(ctx.OutputNamespace) + ".NewBrowserBackendWithTelemetry(tracingRuntime)"}, ConstructorDeps: map[int][]string{0: {"tracingRuntime"}}, ConstructorResources: map[int][]gen.Resource{0: {gen.ServiceContext, gen.SQLDatabase}}, ConstructorReturnsError: map[int]bool{0: true}, ConstructorDeferCalls: map[int]string{0: "Close()"}, BackgroundTasks: []int{0}, Routes: []string{fmt.Sprintf("mux.Handle(%q, browserBackendWithTelemetry)", mountPattern)}, GoModRequires: map[string]string{"github.com/coreos/go-oidc/v3": "v3.21.0"}}
	if g.LocalApplicationPort != 0 {
		wiring.GoModRequires["github.com/coder/websocket"] = httpclient.WebSocketVersion
	}
	wiring.DatabaseAccess = []gen.DatabaseObject{
		{Schema: "public", Name: "stego_browser_sessions", Kind: "table", Privileges: []string{"SELECT", "INSERT", "UPDATE", "DELETE"}},
	}
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
				if strings.EqualFold(attr.Key, "name") && (strings.EqualFold(attr.Val, "stego-runtime-config") || strings.EqualFold(attr.Val, "stego-style-nonce")) {
					return 0, fmt.Errorf("browser runtime configuration is compiler-owned")
				}
			}
		}
	}
	if heads != 1 {
		return 0, fmt.Errorf("browser runtime metadata requires one explicit HTML head element")
	}
	return head, nil
}
