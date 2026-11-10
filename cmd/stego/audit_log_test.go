package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/auditlog"
	"github.com/jsell-rh/stego/internal/registry"
)

// compiledService writes a test registry with the archetype audit-arch, which has the runtime pin of the shipped
// rest-crud archetype and no component, and the shipped audit-log component at stage alpha, and a project of the
// service customers on it whose declaration is service. Its audit setting selects audit-log (ST-GEN-INV37). It
// gives the reconciler input and the pin.
func compiledService(t *testing.T, service string) (compiler.ReconcilerInput, registry.RuntimePin) {
	t.Helper()
	shipped, err := registry.Load(filepath.Join("..", "..", "registry"))
	if err != nil {
		t.Fatal(err)
	}
	pin := *shipped.Archetype("rest-crud").Runtime
	component, err := os.ReadFile(filepath.Join("..", "..", "registry", "components", "audit-log", "component.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	common, project := filepath.Join(parent, "registry"), filepath.Join(parent, "customers")
	files := map[string]string{
		filepath.Join(common, "archetypes/audit-arch/archetype.yaml"): "kind: archetype\nname: audit-arch\nlanguage: go\nversion: 1.0.0\n" +
			"runtime:\n  module: " + pin.Module + "\n  version: " + pin.Version + "\n  sum: " + pin.Sum + "\n" +
			"components: []\nconventions:\n  layout: flat\n",
		filepath.Join(common, "components/audit-log/component.yaml"): string(component),
		filepath.Join(project, "service.yaml"):                       service,
	}
	for name, data := range files {
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return compiler.ReconcilerInput{
		ProjectDir: project, RegistryDir: common, GoVersion: "1.26.10", ModuleName: "example.com/customers",
		Generators: map[string]gen.Generator{"audit-log": new(auditlog.Generator)},
	}, pin
}

// customerService is the declaration of the RC28 test records as service.yaml: audit at level metadata with pii
// keep and a retention of 6 months, which selects audit-log, an experimental acceptance of audit-log, the
// audit-sink port left to the platform, and the entity Customer.
var customerService = `kind: service
name: customers
archetype: audit-arch
language: go
audit:
  level: metadata
  pii: keep
  retention: 6 months
entities:
  - name: Customer
    fields:
      - { name: name, type: string, sensitivity: pii }
      - { name: api_token, type: string, sensitivity: secret }
      - { name: plan, type: string }
` + acceptanceOf("audit-log", "The audit-log builder proves the compiler path")

// TestAuditLogCompiledService generates the audit wiring through the compiler from a service declaration, on the runtime
// pin of the archetype, and builds it against registry/runtime through the work file of
// scripts/runtime-workspace.sh.
func TestAuditLogCompiledService(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a generated service")
	}
	input, pin := compiledService(t, customerService)
	plan, err := compiler.Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := compiler.Apply(plan, input.ProjectDir, ""); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(input.ProjectDir, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	t.Run("ST-SUP-RC28-OC01-T01 The emitted redaction settings come from the declaration through the compiler", func(t *testing.T) {
		source := read("out/internal/auditlog/audit.go")
		for _, want := range []string{
			"var Settings = audit.Settings{Level: audit.LevelMetadata, PII: audit.PIIKeep}",
			`var CustomerFields = audit.Fields{"api_token": audit.Secret, "name": audit.PII}`,
			`"Customer": true,`,
			"const RetentionMonths = 6",
			"const AnchorPeriod = 3600 * time.Second",
		} {
			if !strings.Contains(source, want) {
				t.Errorf("the emitted audit wiring lacks %q:\n%s", want, source)
			}
		}
	})

	t.Run("ST-SUP-RC28-OC06-T02 The database access contract grants SELECT and INSERT only on audit_event", func(t *testing.T) {
		var access struct {
			Objects []gen.DatabaseObject `json:"objects"`
		}
		if err := json.Unmarshal([]byte(read("out/contracts/databaseaccess/access.json")), &access); err != nil {
			t.Fatal(err)
		}
		var events []string
		for _, o := range access.Objects {
			if o.Name == "audit_event" {
				events = o.Privileges
			}
		}
		if !slices.Equal(events, []string{"INSERT", "SELECT"}) {
			t.Fatalf("the contract grants %v on audit_event, want INSERT and SELECT only", events)
		}
	})

	t.Run("ST-SUP-RC28-OC01-T01 The emitted redaction settings build on the runtime pin of the archetype", func(t *testing.T) {
		module, err := modfile.Parse("go.mod", []byte(read("go.mod")), nil)
		if err != nil {
			t.Fatal(err)
		}
		required := ""
		for _, r := range module.Require {
			if registry.IsStegoModule(r.Mod.Path) {
				required += r.Mod.Path + " " + r.Mod.Version + ";"
			}
		}
		if required != pin.Module+" "+pin.Version+";" || len(module.Replace) != 0 {
			t.Fatalf("go.mod requires %q with %d replace directives, want only the pin %s %s", required, len(module.Replace), pin.Module, pin.Version)
		}
		work := filepath.Join(t.TempDir(), "runtime.work")
		script := exec.Command(filepath.Join("..", "..", "scripts", "runtime-workspace.sh"), input.ProjectDir, work)
		if out, err := script.CombinedOutput(); err != nil {
			t.Fatalf("runtime-workspace.sh: %v\n%s", err, out)
		}
		name := os.Getenv("STEGO_TEST_PINNED_GO")
		if name == "" {
			name = filepath.Join(runtime.GOROOT(), "bin", "go")
		}
		build := exec.Command(name, "vet", "./out/internal/auditlog")
		build.Dir = input.ProjectDir
		build.Env = append(os.Environ(), "GOWORK="+work, "GOFLAGS=", "GOTOOLCHAIN=local")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("go vet of the compiled audit wiring: %v\n%s", err, out)
		}
	})

	t.Run("ST-SUP-RC28-OC07-T02 The compiler leaves audit-sink to the platform and refuses an audit.sink that names no binding", func(t *testing.T) {
		// The service above binds no port: the platform binds audit-sink (ST-GEN-INV37), so it generated.
		unbound := strings.Replace(customerService, "  retention: 6 months\n", "  retention: 6 months\n  sink: audit-archive\n", 1)
		input, _ := compiledService(t, unbound)
		if _, err := compiler.Reconcile(input); err == nil || !strings.Contains(err.Error(), "audit-archive") {
			t.Fatalf("Reconcile of a service whose audit.sink names no binding: %v, want a refusal that names audit-archive", err)
		}
	})
}

// acceptanceOf gives the experimental_acceptances block that accepts the shipped component name until its own
// sunset, which it reads from registry/components, so that a new sunset of the component does not break a test
// (ST-DECL-EN07, ST-GEN-INV37).
func acceptanceOf(name, reason string) string {
	c, err := registry.ParseComponent(filepath.Join("..", "..", "registry", "components", name, "component.yaml"))
	if err != nil {
		panic(fmt.Sprintf("read the component %s: %v", name, err))
	}
	if c.Sunset == "" {
		panic("the component " + name + " has no sunset date")
	}
	return "experimental_acceptances:\n  - {component: " + name + ", accepted_until: " + c.Sunset + ", reason: " + reason + "}\n"
}
