package compiler

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"path"
	"slices"
	"strings"
	"text/template"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/registry"
	"github.com/jsell-rh/stego/internal/types"
)

//go:embed runtime_wiring.go.tmpl
var runtimeWiringSource string

// runtimeWiringComponent is the name of the compiler's runtime wiring among the component wirings of main.go.
const runtimeWiringComponent = "stego-runtime"

// RuntimeWiringNamespace is the output namespace of the runtime wiring package that the compiler writes for a
// service on the runtime data path.
const RuntimeWiringNamespace = "internal/stegoruntime"

// The components whose packages the runtime wiring calls: each must be in the service on the runtime data path,
// because command.New needs the authorization runtime, the audit writer and the outbox queue (ST-SUP-RP10-LC30), and
// the tables and the tenant wall come from the PostgreSQL adapter and the tenancy component.
var runtimeWiringComponents = []string{adapterComponent, gen.TenancyComponent, authorizationComponent, auditComponent, "outbox"}

// restAPIComponent is the component of the collection handlers (ST-SUP-RC20).
const restAPIComponent = "rest-api"

// runtimeWiringInput is what the compiler reads to write the runtime wiring of a service.
type runtimeWiringInput struct {
	ModuleName, OutDirName string
	Service                *types.ServiceDeclaration
	ServiceData            []byte
	Components             map[string]*registry.Component
	Names                  []string
	// Wirings are the wirings of the component generators, whose database objects the runtime role is granted.
	Wirings []ComponentWiring
	// QueryStreams are the queries that a server stream serves over gRPC only, which get no HTTP route
	// (ST-SUP-RC33-OC08).
	QueryStreams map[string]bool
	// Generators and Contexts are the generators of the components and their resolved contexts: an auth provider
	// whose generator is a gen.RuntimeAuthProvider gives the verifier its settings (ST-SUP-RC37).
	Generators map[string]gen.Generator
	Contexts   map[string]gen.Context
}

// runtimeWiringData is the data of the template of the runtime wiring package.
type runtimeWiringData struct {
	Audit, Authz, Outbox, Storage, Tenancy, Idempotency, API, Commands, Queries     string
	Service, DeclarationVersion, SubjectType, TenantClaim, ProblemType, ProblemCode string
	Issuer, Audience, Header, KeyFile, RoleClaim                                    string
	// Discovery, CAFile, SubjectClaim and Algorithms are the settings of an auth provider that the verifier serves
	// through the discovery document of its issuer, such as oidc-auth (ST-SUP-RC37).
	Discovery            bool
	CAFile, SubjectClaim string
	Algorithms           []string
	Grants               []string
}

// runtimeWiring gives the runtime wiring package of a service on the runtime data path and its wiring of main.go
// (ST-SUP-RC01-OC41, ST-SUP-RC20-OC37): one constructor, NewService, whose value is the variable service of main.go;
// the verifier as the auth middleware; one literal route for each declared command and query, each served by the
// routes of the command and the query handlers, but none of a stream query, which gRPC alone serves
// (ST-SUP-RC33-OC08); and the migration job. The collection handlers take service.CollectionRuntime() through the
// rest-api wiring, and the gRPC application takes the service itself (ST-SUP-RC07-OC19). It refuses a service that
// lacks a component the wiring calls.
func runtimeWiring(in runtimeWiringInput) ([]gen.File, ComponentWiring, error) {
	svc := in.Service
	for _, name := range runtimeWiringComponents {
		if !slices.Contains(in.Names, name) {
			return nil, ComponentWiring{}, fmt.Errorf("the runtime data path needs the component %s, which the runtime wiring calls; add it to the archetype or select it by its setting", name)
		}
	}
	if svc.Authorization == nil || svc.Authorization.SubjectFromIdentity == "" {
		return nil, ComponentWiring{}, fmt.Errorf("the runtime data path needs authorization with subject_from_identity, which maps a verified identity to its subject")
	}
	if svc.Tenancy == nil || svc.Tenancy.Claim == "" {
		return nil, ComponentWiring{}, fmt.Errorf("the runtime data path needs tenancy with a claim, which names the caller's tenant in the bearer token")
	}
	pkg := func(component string, sub ...string) string {
		c := in.Components[component]
		if c == nil || c.OutputNamespace == "" {
			return ""
		}
		return generatedImportPath(in.ModuleName, in.OutDirName, path.Join(append([]string{c.OutputNamespace}, sub...)...))
	}
	digest := sha256.Sum256(in.ServiceData)
	data := runtimeWiringData{
		Audit: pkg(auditComponent), Authz: pkg(authorizationComponent), Outbox: pkg("outbox"), Storage: pkg(adapterComponent),
		Tenancy: pkg(gen.TenancyComponent), Service: svc.Name, DeclarationVersion: hex.EncodeToString(digest[:]),
		SubjectType: svc.Authorization.SubjectFromIdentity, TenantClaim: svc.Tenancy.Claim, ProblemType: svc.ErrorTypeBase,
		ProblemCode: problemCode(svc.Name),
	}
	if slices.Contains(in.Names, idempotencyComponent) && svc.Idempotency != nil {
		data.Idempotency = pkg(idempotencyComponent)
	}
	if slices.Contains(in.Names, restAPIComponent) && len(svc.Collections) > 0 {
		data.API = pkg(restAPIComponent)
	}
	if len(svc.Commands) > 0 {
		data.Commands = pkg(commandComponent, "handlers")
	}
	if len(svc.Queries) > 0 {
		data.Queries = pkg(queryComponent, "serve")
	}
	if data.ProblemCode == "" {
		return nil, ComponentWiring{}, fmt.Errorf("the service name %q gives no problem code prefix", svc.Name)
	}
	if err := declaredAuthentication(in, &data); err != nil {
		return nil, ComponentWiring{}, err
	}
	data.Grants = runtimeGrants(in.Wirings)
	source, err := renderRuntimeWiring(data)
	if err != nil {
		return nil, ComponentWiring{}, err
	}
	wiring := &gen.Wiring{
		Imports:                 []string{RuntimeWiringNamespace},
		Constructors:            []string{path.Base(RuntimeWiringNamespace) + ".NewService()"},
		ConstructorResources:    map[int][]gen.Resource{0: {gen.ServiceContext}},
		ConstructorReturnsError: map[int]bool{0: true},
		ConstructorDeferCalls:   map[int]string{0: "Close()"},
		// A start that the key source refused names its stage, with no error text (ST-SUP-RP14-LC05).
		ConstructorStages: map[int]string{0: path.Base(RuntimeWiringNamespace) + ".StartStage(err)"},
		// Run starts the workers of the selected modes and stops them when the process stops (ST-SUP-RP14-LC04).
		BackgroundTasks: []int{0},
		// The readiness of the modes, outside the request wrappers (ST-SUP-RP09-LC05).
		DiscoveryRoutes:       []string{`topMux.Handle("GET /readyz/modes", service.Readiness())`},
		MiddlewareConstructor: new(int),
		MiddlewareWrapExpr:    "%s.Middleware(%s)",
		MigrationJob:          &gen.MigrationJobSpec{Namespace: RuntimeWiringNamespace, Function: "Migrate", Stage: "MigrationStage"},
		GoModRequires: map[string]string{
			"google.golang.org/grpc": "v1.83.2",
		},
	}
	base := strings.TrimSuffix(svc.BasePath, "/")
	var routes []string
	for _, c := range svc.Commands {
		routes = append(routes, c.Route)
	}
	for _, q := range svc.Queries {
		if !in.QueryStreams[q.Name] {
			routes = append(routes, q.Route)
		}
	}
	for _, route := range routes {
		method, target, ok := strings.Cut(route, " ")
		if !ok {
			return nil, ComponentWiring{}, fmt.Errorf("the route %q has no method and path", route)
		}
		wiring.Routes = append(wiring.Routes, fmt.Sprintf("mux.Handle(%q, service.Routes())", method+" "+base+target))
	}
	files := []gen.File{{Path: path.Join(RuntimeWiringNamespace, "runtime.go"), Content: source}}
	return files, ComponentWiring{Name: runtimeWiringComponent, Wiring: wiring}, nil
}

// renderRuntimeWiring renders the runtime wiring package.
func renderRuntimeWiring(data runtimeWiringData) ([]byte, error) {
	tmpl, err := template.New("runtime-wiring").Parse(runtimeWiringSource)
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	if err := tmpl.Execute(&output, data); err != nil {
		return nil, err
	}
	source, err := format.Source(output.Bytes())
	if err != nil {
		return nil, fmt.Errorf("formatting the runtime wiring: %w", err)
	}
	return source, nil
}

// runtimeGrants gives the GRANT statements of the runtime role, each without its grantee: USAGE on each schema but
// public of the database objects that the component wirings declare, and their privileges on those objects, in the
// order of the objects; then the key table of the idempotency runtime, which every command runs in
// (ST-SUP-RP07-LC01), with no DELETE: the idempotency migration gives it EXECUTE on the purge function, the only way
// to delete the keys whose retention ended across tenants (ST-SUP-RP07-LC11).
func runtimeGrants(wirings []ComponentWiring) []string {
	var grants []string
	schemas := map[string]bool{"public": true}
	for _, component := range wirings {
		if component.Wiring == nil {
			continue
		}
		for _, object := range component.Wiring.DatabaseAccess {
			if !schemas[object.Schema] {
				schemas[object.Schema] = true
				grants = append(grants, "GRANT USAGE ON SCHEMA "+quoteName(object.Schema))
			}
			if len(object.Privileges) == 0 {
				continue
			}
			grants = append(grants, "GRANT "+strings.Join(object.Privileges, ", ")+" ON "+strings.ToUpper(object.Kind)+" "+
				quoteName(object.Schema)+"."+quoteName(object.Name))
		}
	}
	return append(grants, "GRANT SELECT, INSERT, UPDATE ON TABLE \"public\".\"idempotency_keys\"")
}

// quoteName quotes one SQL name.
func quoteName(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// problemCode gives the problem code prefix of a refused credential: the service name in upper case, without the
// suffix -api, -service or -server and without any character but a letter or a digit, at most 32 characters, as the
// rest-api component derives its error prefix.
func problemCode(service string) string {
	for _, suffix := range []string{"-api", "-service", "-server"} {
		if strings.HasSuffix(service, suffix) {
			service = strings.TrimSuffix(service, suffix)
			break
		}
	}
	var code strings.Builder
	for _, r := range strings.ToUpper(service) {
		if (r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') && code.Len() < 32 {
			code.WriteRune(r)
		}
	}
	return code.String()
}

// migrationJobExpression gives the call of the migration job that a consumed wiring selects and the expression of
// the stage of its failure, with the import alias that main.go gives its package, or "" when no wiring selects
// one. It refuses two migration jobs and one whose package the wiring does not import.
func migrationJobExpression(input AssemblerInput, imports importResult, consumed map[int]bool) (string, string, error) {
	call, stage := "", ""
	for i, component := range input.Wirings {
		if component.Wiring == nil || component.Wiring.MigrationJob == nil {
			continue
		}
		job := component.Wiring.MigrationJob
		if call != "" {
			return "", "", fmt.Errorf("multiple components declare a migration job")
		}
		valid := func(name string) bool { return token.IsIdentifier(name) && ast.IsExported(name) }
		if !consumed[i] || !slices.Contains(component.Wiring.Imports, job.Namespace) || !valid(job.Function) || (job.Stage != "" && !valid(job.Stage)) {
			return "", "", fmt.Errorf("component %q has an invalid migration job", component.Name)
		}
		name := path.Base(job.Namespace)
		if renamed := imports.Renames[i][name]; renamed != "" {
			name = renamed
		}
		call = name + "." + job.Function + "(context.Background())"
		stage = `"migrate"`
		if job.Stage != "" {
			stage = name + "." + job.Stage + "(err)"
		}
	}
	return call, stage, nil
}

// writeMigrationJob writes the start of main that runs the migration job, and not the service, when the process has
// the one argument migrate (tenant.MigrationJob). A failure is reported with its stage and with no error text, as
// every failure of the service is.
func writeMigrationJob(buf *bytes.Buffer, call, stage string) {
	if call == "" {
		return
	}
	fmt.Fprintf(buf, "\tif len(os.Args) == 2 && os.Args[1] == %q {\n\t\tif err := %s; err != nil {\n\t\t\tstegoReportFailure(os.Stderr, &stegoServiceFailure{stage: %s, cause: err})\n\t\t\tos.Exit(1)\n\t\t}\n\t\treturn\n\t}\n", "migrate", call, stage)
}

// jwtAuthComponent is the auth provider whose settings the verifier of the runtime data path takes.
const jwtAuthComponent = "jwt-auth"

// authProviders gives the components of the service that provide auth-provider.
func authProviders(names []string, components map[string]*registry.Component) []string {
	var providers []string
	for _, name := range names {
		if c := components[name]; c != nil {
			for _, p := range c.Provides {
				if p.Name == "auth-provider" {
					providers = append(providers, name)
				}
			}
		}
	}
	return providers
}

// declaredAuthentication gives the verifier of the runtime data path the settings that the declaration states for
// its auth provider, which win over the settings of the deployment: issuer, audience, header, public_key_file and
// roles_claim of jwt-auth; or the settings that an auth provider such as oidc-auth or rh-sso-auth gives as a
// gen.RuntimeAuthProvider, whose key set the verifier finds through the discovery document of the issuer
// (ST-SUP-RC37, ST-SUP-RC21-OC15). On the runtime data path the verifier of the runtime library authenticates each
// request and binds the identity that the command and query runtimes read (ST-SUP-RP13), so the compiler does not
// generate the auth provider's own package; it refuses another auth provider, whose settings the verifier cannot
// take, and a provider whose tenant claim is not the declared tenancy claim.
func declaredAuthentication(in runtimeWiringInput, data *runtimeWiringData) error {
	for _, name := range authProviders(in.Names, in.Components) {
		if provider, ok := in.Generators[name].(gen.RuntimeAuthProvider); ok {
			auth, err := provider.RuntimeAuth(in.Contexts[name])
			if err != nil {
				return fmt.Errorf("component %q: %w", name, err)
			}
			if auth.TenantClaim != "" && auth.TenantClaim != data.TenantClaim {
				return fmt.Errorf("the auth provider %s gives the tenant in the claim %s, so the tenancy claim of the declaration must be %s", name, auth.TenantClaim, auth.TenantClaim)
			}
			data.Issuer, data.Audience, data.Header, data.RoleClaim = auth.Issuer, auth.Audience, auth.Header, auth.RoleClaim
			data.Discovery, data.CAFile, data.SubjectClaim, data.Algorithms = true, auth.CAFile, auth.SubjectClaim, auth.Algorithms
			continue
		}
		if name != jwtAuthComponent {
			return fmt.Errorf("the runtime data path authenticates with the verifier of the runtime library, which takes the settings of %s, %s and rh-sso-auth only; the auth provider %s is not served on it", jwtAuthComponent, "oidc-auth", name)
		}
		config := resolveComponentConfig(in.Components[name], in.Service)
		text := func(key string) (string, error) {
			value, present := config[key]
			if !present || value == nil {
				return "", nil
			}
			s, ok := value.(string)
			if !ok {
				return "", fmt.Errorf("the setting %s of %s must be text", key, name)
			}
			return s, nil
		}
		for key, target := range map[string]*string{"issuer": &data.Issuer, "audience": &data.Audience, "header": &data.Header,
			"public_key_file": &data.KeyFile, "roles_claim": &data.RoleClaim} {
			value, err := text(key)
			if err != nil {
				return err
			}
			*target = value
		}
	}
	return nil
}
