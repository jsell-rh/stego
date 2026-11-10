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
}

// runtimeWiringData is the data of the template of the runtime wiring package.
type runtimeWiringData struct {
	Audit, Authz, Outbox, Storage, Tenancy, Idempotency, API, Commands, Queries     string
	Service, DeclarationVersion, SubjectType, TenantClaim, ProblemType, ProblemCode string
	WorkerRole, RelayRole                                                           string
	Grants                                                                          []string
}

// runtimeWiring gives the runtime wiring package of a service on the runtime data path and its wiring of main.go
// (ST-SUP-RC01-OC41, ST-SUP-RC20-OC37): one constructor, NewService, whose value is the variable service of main.go;
// the verifier as the auth middleware; one literal route for each declared command and query, each served by the
// routes of the command and the query handlers; and the migration job. The collection handlers take
// service.CollectionRuntime() through the rest-api wiring. It refuses a service that lacks a component the wiring
// calls, and a command with a gRPC method, which the runtime data path does not serve yet (ST-SUP-RC07-OC19).
func runtimeWiring(in runtimeWiringInput) ([]gen.File, ComponentWiring, error) {
	svc := in.Service
	for _, name := range runtimeWiringComponents {
		if !slices.Contains(in.Names, name) {
			return nil, ComponentWiring{}, fmt.Errorf("the runtime data path needs the component %s, which the runtime wiring calls; add it to the archetype or select it by its setting", name)
		}
	}
	for _, c := range svc.Commands {
		if c.Method != "" {
			return nil, ComponentWiring{}, fmt.Errorf("command %q has the gRPC method %s, which the runtime data path does not register yet (ST-SUP-RC07-OC19)", c.Name, c.Method)
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
	role := strings.ToLower(strings.NewReplacer("-", "_", ".", "_").Replace(svc.Name))
	data := runtimeWiringData{
		Audit: pkg(auditComponent), Authz: pkg(authorizationComponent), Outbox: pkg("outbox"), Storage: pkg(adapterComponent),
		Tenancy: pkg(gen.TenancyComponent), Service: svc.Name, DeclarationVersion: hex.EncodeToString(digest[:]),
		SubjectType: svc.Authorization.SubjectFromIdentity, TenantClaim: svc.Tenancy.Claim, ProblemType: svc.ErrorTypeBase,
		ProblemCode: problemCode(svc.Name), WorkerRole: role + "_worker", RelayRole: role + "_relay",
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
		MiddlewareConstructor:   new(int),
		MiddlewareWrapExpr:      "%s.Middleware(%s)",
		MigrationJob:            &gen.MigrationJobSpec{Namespace: RuntimeWiringNamespace, Function: "Migrate"},
		GoModRequires: map[string]string{
			"google.golang.org/grpc":  "v1.83.2",
			"gorm.io/gorm":            "v1.25.12",
			"gorm.io/driver/postgres": "v1.5.11",
		},
	}
	base := strings.TrimSuffix(svc.BasePath, "/")
	var routes []string
	for _, c := range svc.Commands {
		routes = append(routes, c.Route)
	}
	for _, q := range svc.Queries {
		routes = append(routes, q.Route)
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
// (ST-SUP-RP07-LC01): its purge deletes the keys whose retention ended.
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
	return append(grants, "GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE \"public\".\"idempotency_keys\"")
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

// migrationJobExpression gives the call of the migration job that a consumed wiring selects, with the import alias
// that main.go gives its package, or "" when no wiring selects one. It refuses two migration jobs and one whose
// package the wiring does not import.
func migrationJobExpression(input AssemblerInput, imports importResult, consumed map[int]bool) (string, error) {
	expression := ""
	for i, component := range input.Wirings {
		if component.Wiring == nil || component.Wiring.MigrationJob == nil {
			continue
		}
		job := component.Wiring.MigrationJob
		if expression != "" {
			return "", fmt.Errorf("multiple components declare a migration job")
		}
		if !consumed[i] || !slices.Contains(component.Wiring.Imports, job.Namespace) || !token.IsIdentifier(job.Function) || !ast.IsExported(job.Function) {
			return "", fmt.Errorf("component %q has an invalid migration job", component.Name)
		}
		name := path.Base(job.Namespace)
		if renamed := imports.Renames[i][name]; renamed != "" {
			name = renamed
		}
		expression = name + "." + job.Function + "(context.Background())"
	}
	return expression, nil
}

// writeMigrationJob writes the start of main that runs the migration job, and not the service, when the process has
// the one argument migrate (tenant.MigrationJob). A failure is reported at the stage migrate, with no error text, as
// every failure of the service is.
func writeMigrationJob(buf *bytes.Buffer, expression string) {
	if expression == "" {
		return
	}
	fmt.Fprintf(buf, "\tif len(os.Args) == 2 && os.Args[1] == %q {\n\t\tif err := %s; err != nil {\n\t\t\tstegoReportFailure(os.Stderr, &stegoServiceFailure{stage: %q, cause: err})\n\t\t\tos.Exit(1)\n\t\t}\n\t\treturn\n\t}\n", "migrate", expression, "migrate")
}

// replaceAuthMiddleware takes the primary auth middleware off the wirings of the components, such as the auth
// provider of the archetype: on the runtime data path the verifier of the runtime library authenticates each
// request and binds the identity that the command and query runtimes read (ST-SUP-RP13), so the runtime wiring
// declares the one primary middleware.
func replaceAuthMiddleware(wirings []ComponentWiring) {
	for _, component := range wirings {
		if component.Wiring != nil && component.Wiring.MiddlewareConstructor != nil {
			component.Wiring.MiddlewareConstructor = nil
			component.Wiring.MiddlewareWrapExpr = ""
		}
	}
}
