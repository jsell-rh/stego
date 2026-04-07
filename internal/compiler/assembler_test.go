package compiler

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

func TestAssemble_MinimalService(t *testing.T) {
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports:             []string{"internal/api"},
					Constructors:        []string{"api.NewUserHandler(store)"},
					ConstructorEntities: map[int]string{0: "User"},
					Routes: []string{
						`mux.HandleFunc("POST /users", userHandler.Create)`,
						`mux.HandleFunc("GET /users/{id}", userHandler.Read)`,
					},
				},
			},
			{
				Name: "postgres-adapter",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/storage"},
					Constructors: []string{"storage.NewStore(db)"},
					NeedsDB:      true,
				},
			},
		},
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	if len(files) != 2 {
		t.Fatalf("expected 2 files (main.go, go.mod), got %d", len(files))
	}

	// Find main.go and go.mod.
	var mainGo, goMod gen.File
	for _, f := range files {
		switch f.Path {
		case "cmd/main.go":
			mainGo = f
		case "go.mod":
			goMod = f
		}
	}

	if mainGo.Path == "" {
		t.Fatal("missing cmd/main.go in output")
	}
	if goMod.Path == "" {
		t.Fatal("missing go.mod in output")
	}

	// Verify main.go has the generated header.
	fullOutput := string(mainGo.Bytes())
	if !strings.HasPrefix(fullOutput, gen.Header) {
		t.Errorf("main.go missing generated header")
	}

	// Verify main.go is valid Go.
	code := string(mainGo.Content)
	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse as Go:\n%s\nerror: %v", code, err)
	}

	// Verify it contains expected elements.
	if !strings.Contains(code, "package main") {
		t.Errorf("missing package main")
	}
	if !strings.Contains(code, "http.NewServeMux()") {
		t.Errorf("missing mux creation")
	}
	if !strings.Contains(code, "userHandler.Create") {
		t.Errorf("missing route registration")
	}
	if !strings.Contains(code, "storage.NewStore(db)") {
		t.Errorf("missing store constructor")
	}
	if !strings.Contains(code, "sql.Open") {
		t.Errorf("missing DB setup — NeedsDB is true")
	}

	// Verify go.mod — assert on rendered output (Bytes), not intermediate Content.
	modRendered := string(goMod.Bytes())
	if !strings.Contains(modRendered, "module github.com/myorg/svc") {
		t.Errorf("go.mod missing module declaration")
	}
	if !strings.Contains(modRendered, "go 1.22") {
		t.Errorf("go.mod missing go version")
	}
	if !strings.Contains(modRendered, gen.Header) {
		t.Errorf("go.mod missing generated header comment")
	}
}

func TestAssemble_WithSlotBindings(t *testing.T) {
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports:             []string{"internal/api"},
					Constructors:        []string{"api.NewUserHandler(store)"},
					ConstructorEntities: map[int]string{0: "User"},
					Routes: []string{
						`mux.HandleFunc("POST /users", userHandler.Create)`,
					},
				},
			},
			{
				Name: "postgres-adapter",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/storage"},
					Constructors: []string{"storage.NewStore(db)"},
					NeedsDB:      true,
				},
			},
		},
		SlotBindings: []types.SlotDeclaration{
			{
				Slot:   "before_create",
				Entity: "User",
				Gate:   []string{"rbac-policy", "admin-creation-policy"},
			},
			{
				Slot:   "on_entity_changed",
				Entity: "User",
				FanOut: []string{"user-change-notifier", "audit-logger"},
			},
		},
		SlotsPackage: "internal/slots",
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	// Verify it parses as Go.
	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// Verify slot wiring is present.
	if !strings.Contains(code, "slots.NewBeforeCreateGate") {
		t.Errorf("missing gate operator for before_create in:\n%s", code)
	}
	if !strings.Contains(code, "slots.NewOnEntityChangedFanOut") {
		t.Errorf("missing fan-out operator for on_entity_changed in:\n%s", code)
	}

	// Verify fill imports.
	if !strings.Contains(code, "rbacpolicy") {
		t.Errorf("missing rbac-policy fill import in:\n%s", code)
	}
	if !strings.Contains(code, "admincreationpolicy") {
		t.Errorf("missing admin-creation-policy fill import in:\n%s", code)
	}
	if !strings.Contains(code, "userchangenotifier") {
		t.Errorf("missing user-change-notifier fill import in:\n%s", code)
	}
	if !strings.Contains(code, "auditlogger") {
		t.Errorf("missing audit-logger fill import in:\n%s", code)
	}

	// Verify fills are passed to operator constructors.
	if !strings.Contains(code, "rbacpolicy.New()") {
		t.Errorf("missing rbac-policy fill constructor in:\n%s", code)
	}
	if !strings.Contains(code, "admincreationpolicy.New()") {
		t.Errorf("missing admin-creation-policy fill constructor in:\n%s", code)
	}

	// Verify slots package import.
	if !strings.Contains(code, `"github.com/myorg/svc/internal/slots"`) {
		t.Errorf("missing slots package import in:\n%s", code)
	}

	// Verify entity annotation in comments.
	if !strings.Contains(code, "for User") {
		t.Errorf("missing entity annotation in slot wiring comments")
	}

	// Verify operators are wired into handler constructor (not discarded).
	if strings.Contains(code, "_ =") {
		t.Errorf("slot operators should not be discarded with _ = in:\n%s", code)
	}
	// Verify the handler constructor includes slot operator arguments
	// with entity-scoped variable names.
	if !strings.Contains(code, "api.NewUserHandler(store, beforeCreateUserGate, onEntityChangedUserFanOut)") {
		t.Errorf("slot operators not injected into handler constructor in:\n%s", code)
	}
}

func TestAssemble_ChainWithShortCircuit(t *testing.T) {
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        9090,
		Wirings: []ComponentWiring{
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/api"},
					Constructors: []string{"api.NewHandler(store)"},
					Routes:       []string{`mux.HandleFunc("POST /items", handler.Create)`},
				},
			},
			{
				Name: "postgres-adapter",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/storage"},
					Constructors: []string{"storage.NewStore(db)"},
					NeedsDB:      true,
				},
			},
		},
		SlotBindings: []types.SlotDeclaration{
			{
				Slot:         "process_adapter_status",
				Chain:        []string{"validate-conditions", "discard-stale", "persist-status"},
				ShortCircuit: true,
			},
		},
		SlotsPackage: "internal/slots",
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	// Verify it parses.
	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// Verify chain operator with short-circuit enabled.
	if !strings.Contains(code, "slots.NewProcessAdapterStatusChain(true") {
		t.Errorf("missing chain with short_circuit=true in:\n%s", code)
	}

	// Verify custom port.
	if !strings.Contains(code, "9090") {
		t.Errorf("missing custom port 9090 in:\n%s", code)
	}

	// This slot has no Entity, so the operator can't be injected into a
	// handler constructor and _ = is used to suppress unused variable errors.
	if !strings.Contains(code, "_ = processAdapterStatusChain") {
		t.Errorf("entity-less operator should use _ = to suppress unused var in:\n%s", code)
	}
}

func TestAssemble_WithAuthMiddleware(t *testing.T) {
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "jwt-auth",
				Wiring: &gen.Wiring{
					Imports:               []string{"internal/auth"},
					Constructors:          []string{"auth.NewAuthMiddleware()"},
					MiddlewareConstructor: intPtr(0),
				},
			},
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports:             []string{"internal/api"},
					Constructors:        []string{"api.NewUserHandler(store)"},
					ConstructorEntities: map[int]string{0: "User"},
					Routes: []string{
						`mux.HandleFunc("GET /users", userHandler.List)`,
					},
				},
			},
			{
				Name: "postgres-adapter",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/storage"},
					Constructors: []string{"storage.NewStore(db)"},
					NeedsDB:      true,
				},
			},
		},
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// Verify auth middleware wraps the mux.
	if !strings.Contains(code, "authMiddleware.Middleware(mux)") {
		t.Errorf("missing auth middleware wrapping mux in:\n%s", code)
	}
}

func TestAssemble_NoRoutes(t *testing.T) {
	// A service with no routes should still produce valid main.go,
	// and should NOT import "fmt" (which is only used in writeServerStart).
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Wirings: []ComponentWiring{
			{
				Name: "postgres-adapter",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/storage"},
					Constructors: []string{"storage.NewStore(db)"},
					NeedsDB:      true,
				},
			},
		},
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// No routes means no mux and no server start.
	if strings.Contains(code, "http.NewServeMux") {
		t.Errorf("should not have mux when no routes exist")
	}

	// fmt should not be imported when there are no routes.
	if strings.Contains(code, `"fmt"`) {
		t.Errorf("fmt should not be imported when there are no routes in:\n%s", code)
	}
}

func TestAssemble_FillsDirectoryNeverTouched(t *testing.T) {
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/api"},
					Constructors: []string{"api.NewHandler(store)"},
					Routes:       []string{`mux.HandleFunc("GET /items", handler.List)`},
				},
			},
			{
				Name:   "postgres-adapter",
				Wiring: &gen.Wiring{Imports: []string{"internal/storage"}, Constructors: []string{"storage.NewStore(db)"}, NeedsDB: true},
			},
		},
		SlotBindings: []types.SlotDeclaration{
			{Slot: "validate", Gate: []string{"my-fill"}},
		},
		SlotsPackage: "internal/slots",
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	for _, f := range files {
		if strings.HasPrefix(f.Path, "fills/") {
			t.Errorf("assembler generated file under fills/: %s", f.Path)
		}
	}
}

func TestAssemble_EmptyModuleName(t *testing.T) {
	_, err := Assemble(AssemblerInput{GoVersion: "1.22"})
	if err == nil {
		t.Fatal("expected error for empty ModuleName")
	}
}

func TestAssemble_EmptyGoVersion(t *testing.T) {
	_, err := Assemble(AssemblerInput{ModuleName: "mod"})
	if err == nil {
		t.Fatal("expected error for empty GoVersion")
	}
}

func TestAssemble_DefaultPort(t *testing.T) {
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Wirings: []ComponentWiring{
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/api"},
					Constructors: []string{"api.NewHandler()"},
					Routes:       []string{`mux.HandleFunc("GET /", handler.Index)`},
				},
			},
		},
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)
	if !strings.Contains(code, "8080") {
		t.Errorf("expected default port 8080 in:\n%s", code)
	}
}

func TestAssemble_NilWiring(t *testing.T) {
	// A component with nil wiring should be skipped gracefully.
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{Name: "stub-component", Wiring: nil},
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/api"},
					Constructors: []string{"api.NewHandler()"},
					Routes:       []string{`mux.HandleFunc("GET /", handler.Index)`},
				},
			},
		},
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", string(mainGo.Content), err)
	}
}

func TestAssemble_DeduplicatesFillImports(t *testing.T) {
	// Same fill referenced in two slot bindings should only be imported once.
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/api"},
					Constructors: []string{"api.NewHandler()"},
					Routes:       []string{`mux.HandleFunc("GET /", handler.Index)`},
				},
			},
		},
		SlotBindings: []types.SlotDeclaration{
			{Slot: "before_create", Gate: []string{"shared-fill"}},
			{Slot: "validate", Gate: []string{"shared-fill"}},
		},
		SlotsPackage: "internal/slots",
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	// Count occurrences of the fill import — should appear exactly once.
	count := strings.Count(code, `"github.com/myorg/svc/fills/shared-fill"`)
	if count != 1 {
		t.Errorf("fill import should appear once, got %d occurrences in:\n%s", count, code)
	}
}

func TestAssemble_MultipleEntitiesFullWiring(t *testing.T) {
	// Full wiring with multiple entities, slots, and auth middleware.
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/user-service",
		ServiceName: "user-management",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "postgres-adapter",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/storage"},
					Constructors: []string{"storage.NewStore(db)"},
					NeedsDB:      true,
				},
			},
			{
				Name: "jwt-auth",
				Wiring: &gen.Wiring{
					Imports:               []string{"internal/auth"},
					Constructors:          []string{"auth.NewAuthMiddleware()"},
					MiddlewareConstructor: intPtr(0),
				},
			},
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports: []string{"internal/api"},
					Constructors: []string{
						"api.NewOrganizationHandler(store)",
						"api.NewUserHandler(store)",
					},
					ConstructorEntities: map[int]string{
						0: "Organization",
						1: "User",
					},
					Routes: []string{
						`mux.HandleFunc("POST /organizations", organizationHandler.Create)`,
						`mux.HandleFunc("GET /organizations/{id}", organizationHandler.Read)`,
						`mux.HandleFunc("POST /users", userHandler.Create)`,
						`mux.HandleFunc("GET /users/{id}", userHandler.Read)`,
						`mux.HandleFunc("PUT /users/{id}", userHandler.Update)`,
						`mux.HandleFunc("GET /users", userHandler.List)`,
					},
				},
			},
		},
		SlotBindings: []types.SlotDeclaration{
			{
				Slot:   "before_create",
				Entity: "User",
				Gate:   []string{"rbac-policy", "admin-creation-policy"},
			},
			{
				Slot:   "on_entity_changed",
				Entity: "User",
				FanOut: []string{"user-change-notifier", "audit-logger"},
			},
		},
		SlotsPackage: "internal/slots",
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// Verify all entities' handlers.
	if !strings.Contains(code, "organizationHandler") {
		t.Errorf("missing organizationHandler")
	}
	if !strings.Contains(code, "userHandler") {
		t.Errorf("missing userHandler")
	}

	// Verify auth middleware wraps server.
	if !strings.Contains(code, "authMiddleware.Middleware(mux)") {
		t.Errorf("missing auth middleware in server start")
	}

	// Verify all slots are wired.
	if !strings.Contains(code, "NewBeforeCreateGate") {
		t.Errorf("missing gate operator")
	}
	if !strings.Contains(code, "NewOnEntityChangedFanOut") {
		t.Errorf("missing fan-out operator")
	}

	// Verify slot operators are injected into handler constructor
	// with entity-scoped variable names.
	if !strings.Contains(code, "api.NewUserHandler(store, beforeCreateUserGate, onEntityChangedUserFanOut)") {
		t.Errorf("slot operators not injected into User handler constructor in:\n%s", code)
	}

	// Organization handler should NOT have slot operators.
	if !strings.Contains(code, "api.NewOrganizationHandler(store)") {
		t.Errorf("Organization handler should have unmodified constructor in:\n%s", code)
	}

	// Verify no discarded operators.
	if strings.Contains(code, "_ =") {
		t.Errorf("slot operators should not be discarded with _ = in:\n%s", code)
	}

	// Verify all routes.
	if !strings.Contains(code, "POST /organizations") {
		t.Errorf("missing POST /organizations route")
	}
	if !strings.Contains(code, "GET /users") {
		t.Errorf("missing GET /users route")
	}
}

func TestConstructorVarName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"api.NewUserHandler(store)", "userHandler"},
		{"storage.NewStore(db)", "store"},
		{"auth.NewAuthMiddleware()", "authMiddleware"},
		{"api.NewOrganizationHandler(store)", "organizationHandler"},
		{"pkg.New()", "v"},
	}
	for _, tt := range tests {
		got := rawConstructorVarName(tt.input)
		if got != tt.want {
			t.Errorf("rawConstructorVarName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFillImportAlias(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"admin-creation-policy", "admincreationpolicy"},
		{"audit-logger", "auditlogger"},
		{"rbac_policy", "rbacpolicy"},
		{"simple", "simple"},
	}
	for _, tt := range tests {
		got := rawFillImportAlias(tt.input)
		if got != tt.want {
			t.Errorf("rawFillImportAlias(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSnakeToPascal(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"before_create", "BeforeCreate"},
		{"on_entity_changed", "OnEntityChanged"},
		{"validate", "Validate"},
		{"process_adapter_status", "ProcessAdapterStatus"},
	}
	for _, tt := range tests {
		got := snakeToPascal(tt.input)
		if got != tt.want {
			t.Errorf("snakeToPascal(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestDisambiguateAlias(t *testing.T) {
	counts := make(map[string]int)
	used := make(map[string]bool)

	// First use: no suffix.
	got := disambiguateAlias("models", counts, used)
	if got != "models" {
		t.Errorf("first use: got %q, want %q", got, "models")
	}

	// Second use: gets numeric suffix.
	got = disambiguateAlias("models", counts, used)
	if got != "models2" {
		t.Errorf("second use: got %q, want %q", got, "models2")
	}

	// Third use.
	got = disambiguateAlias("models", counts, used)
	if got != "models3" {
		t.Errorf("third use: got %q, want %q", got, "models3")
	}

	// Different base: no suffix.
	got = disambiguateAlias("api", counts, used)
	if got != "api" {
		t.Errorf("different base: got %q, want %q", got, "api")
	}
}

func TestAssemble_ConstructorVarNameCollision(t *testing.T) {
	// Two constructors that derive the same variable name should be
	// disambiguated with numeric suffixes.
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "component-a",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/a"},
					Constructors: []string{"a.NewStore(db)"},
					NeedsDB:      true,
				},
			},
			{
				Name: "component-b",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/b"},
					Constructors: []string{"b.NewStore(db)"},
				},
			},
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/api"},
					Constructors: []string{"api.NewHandler()"},
					Routes:       []string{`mux.HandleFunc("GET /", handler.Index)`},
				},
			},
		},
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	// Should parse — disambiguated variable names.
	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// Verify both store variables exist with distinct names.
	if !strings.Contains(code, "store :=") {
		t.Errorf("missing first store variable in:\n%s", code)
	}
	if !strings.Contains(code, "store2 :=") {
		t.Errorf("missing disambiguated store2 variable in:\n%s", code)
	}
}

func TestAssemble_FillImportAliasCollision(t *testing.T) {
	// Fill names "a-b" and "ab" both produce raw alias "ab".
	// They should be disambiguated.
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/api"},
					Constructors: []string{"api.NewHandler()"},
					Routes:       []string{`mux.HandleFunc("GET /", handler.Index)`},
				},
			},
		},
		SlotBindings: []types.SlotDeclaration{
			{Slot: "slot_a", Gate: []string{"a-b"}},
			{Slot: "slot_b", Gate: []string{"ab"}},
		},
		SlotsPackage: "internal/slots",
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	// Should parse — disambiguated aliases.
	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// Both fill imports should be present with different aliases.
	if !strings.Contains(code, `"github.com/myorg/svc/fills/a-b"`) {
		t.Errorf("missing a-b fill import in:\n%s", code)
	}
	if !strings.Contains(code, `"github.com/myorg/svc/fills/ab"`) {
		t.Errorf("missing ab fill import in:\n%s", code)
	}
}

func TestAssemble_ComponentImportAliasCollision(t *testing.T) {
	// Two components with different import paths sharing the same base
	// (e.g. "internal/api/models" and "internal/storage/models") should get
	// disambiguated aliases.
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "component-a",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/api/models"},
					Constructors: []string{"models.NewFoo()"},
					Routes:       []string{`mux.HandleFunc("GET /foo", foo.Get)`},
				},
			},
			{
				Name: "component-b",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/storage/models"},
					Constructors: []string{"models.NewBar()"},
				},
			},
		},
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	// Should parse — disambiguated import aliases.
	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// Both imports should be present.
	if !strings.Contains(code, `"github.com/myorg/svc/internal/api/models"`) {
		t.Errorf("missing api/models import in:\n%s", code)
	}
	if !strings.Contains(code, `"github.com/myorg/svc/internal/storage/models"`) {
		t.Errorf("missing storage/models import in:\n%s", code)
	}

	// Second should have a disambiguated alias.
	if !strings.Contains(code, "models2") {
		t.Errorf("missing disambiguated models2 alias in:\n%s", code)
	}

	// Finding 14: Constructor expressions must use the disambiguated alias.
	// Component-b's constructor should reference "models2", not "models".
	if !strings.Contains(code, "models2.NewBar()") {
		t.Errorf("component-b constructor should use disambiguated alias models2 in:\n%s", code)
	}
}

func TestAssemble_NeedsDB_StructuredMetadata(t *testing.T) {
	// NeedsDB should be detected from the structured NeedsDB field,
	// not from constructor expression string matching.
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/api"},
					Constructors: []string{"api.NewHandler()"},
					Routes:       []string{`mux.HandleFunc("GET /", handler.Index)`},
				},
			},
			{
				Name: "postgres-adapter",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/storage"},
					Constructors: []string{"storage.NewStore(connection)"},
					NeedsDB:      true,
				},
			},
		},
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)
	if !strings.Contains(code, "sql.Open") {
		t.Errorf("DB setup should be present when NeedsDB is true, regardless of constructor expression:\n%s", code)
	}
}

func TestAssemble_NoDB_WhenNeedsDBFalse(t *testing.T) {
	// Even with "(db)" in constructor string, NeedsDB=false should skip DB setup.
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/api"},
					Constructors: []string{"api.NewHandler(db)"},
					Routes:       []string{`mux.HandleFunc("GET /", handler.Index)`},
					NeedsDB:      false,
				},
			},
		},
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)
	if strings.Contains(code, "sql.Open") {
		t.Errorf("DB setup should NOT be present when NeedsDB is false:\n%s", code)
	}
}

func TestSlotVarName(t *testing.T) {
	tests := []struct {
		slot, entity, suffix string
		want                 string
	}{
		{"before_create", "User", "Gate", "beforeCreateUserGate"},
		{"before_create", "Organization", "Gate", "beforeCreateOrganizationGate"},
		{"on_entity_changed", "User", "FanOut", "onEntityChangedUserFanOut"},
		{"process_adapter_status", "", "Chain", "processAdapterStatusChain"},
	}
	for _, tt := range tests {
		got := slotVarName(tt.slot, tt.entity, tt.suffix)
		if got != tt.want {
			t.Errorf("slotVarName(%q, %q, %q) = %q, want %q",
				tt.slot, tt.entity, tt.suffix, got, tt.want)
		}
	}
}

func TestAssemble_SameSlotDifferentEntities(t *testing.T) {
	// Finding 10: Two slot bindings with the same slot name but different
	// entities must produce distinct operator variable names.
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports: []string{"internal/api"},
					Constructors: []string{
						"api.NewUserHandler(store)",
						"api.NewOrganizationHandler(store)",
					},
					ConstructorEntities: map[int]string{
						0: "User",
						1: "Organization",
					},
					Routes: []string{
						`mux.HandleFunc("POST /users", userHandler.Create)`,
						`mux.HandleFunc("POST /orgs", organizationHandler.Create)`,
					},
				},
			},
			{
				Name: "postgres-adapter",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/storage"},
					Constructors: []string{"storage.NewStore(db)"},
					NeedsDB:      true,
				},
			},
		},
		SlotBindings: []types.SlotDeclaration{
			{
				Slot:   "before_create",
				Entity: "User",
				Gate:   []string{"user-policy"},
			},
			{
				Slot:   "before_create",
				Entity: "Organization",
				Gate:   []string{"org-policy"},
			},
		},
		SlotsPackage: "internal/slots",
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	// Must parse as valid Go — without entity in var names, both produce
	// "beforeCreateGate" which is a duplicate declaration compile error.
	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// Verify distinct variable names per entity.
	if !strings.Contains(code, "beforeCreateUserGate :=") {
		t.Errorf("missing beforeCreateUserGate in:\n%s", code)
	}
	if !strings.Contains(code, "beforeCreateOrganizationGate :=") {
		t.Errorf("missing beforeCreateOrganizationGate in:\n%s", code)
	}

	// Verify each handler gets its own operator injected.
	if !strings.Contains(code, "api.NewUserHandler(store, beforeCreateUserGate)") {
		t.Errorf("User handler should receive beforeCreateUserGate in:\n%s", code)
	}
	if !strings.Contains(code, "api.NewOrganizationHandler(store, beforeCreateOrganizationGate)") {
		t.Errorf("Organization handler should receive beforeCreateOrganizationGate in:\n%s", code)
	}
}

func TestAssemble_SlotVarCollidesWithConstructorVar(t *testing.T) {
	// Finding 11: A constructor whose derived variable name matches a slot
	// operator variable name must be disambiguated.
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports:             []string{"internal/api"},
					Constructors:        []string{"api.NewUserHandler(store)"},
					ConstructorEntities: map[int]string{0: "User"},
					Routes: []string{
						`mux.HandleFunc("POST /users", userHandler.Create)`,
					},
				},
			},
			{
				Name: "slot-component",
				Wiring: &gen.Wiring{
					Imports: []string{"internal/slotops"},
					// This constructor produces var name "beforeCreateUserGate"
					// which collides with the slot operator variable.
					Constructors: []string{"slotops.NewBeforeCreateUserGate()"},
				},
			},
			{
				Name: "postgres-adapter",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/storage"},
					Constructors: []string{"storage.NewStore(db)"},
					NeedsDB:      true,
				},
			},
		},
		SlotBindings: []types.SlotDeclaration{
			{
				Slot:   "before_create",
				Entity: "User",
				Gate:   []string{"user-policy"},
			},
		},
		SlotsPackage: "internal/slots",
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	// Must parse — the constructor var should be disambiguated to avoid
	// colliding with the slot operator var.
	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// The constructor should get a disambiguated name.
	if !strings.Contains(code, "beforeCreateUserGate2 :=") {
		t.Errorf("constructor var should be disambiguated to beforeCreateUserGate2 in:\n%s", code)
	}
}

func TestAssemble_StructuredEntityMatching(t *testing.T) {
	// Finding 12: Slot operators are injected via structured ConstructorEntities
	// metadata, not by matching "New<Entity>Handler" naming convention.
	// This test uses a non-standard constructor name to verify.
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports: []string{"internal/api"},
					// Non-standard naming: "Controller" instead of "Handler".
					Constructors:        []string{"api.NewUserController(store)"},
					ConstructorEntities: map[int]string{0: "User"},
					Routes: []string{
						`mux.HandleFunc("POST /users", userController.Create)`,
					},
				},
			},
			{
				Name: "postgres-adapter",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/storage"},
					Constructors: []string{"storage.NewStore(db)"},
					NeedsDB:      true,
				},
			},
		},
		SlotBindings: []types.SlotDeclaration{
			{
				Slot:   "before_create",
				Entity: "User",
				Gate:   []string{"user-policy"},
			},
		},
		SlotsPackage: "internal/slots",
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// Even with non-standard constructor name, slot operators should be
	// injected because ConstructorEntities provides the entity mapping.
	if !strings.Contains(code, "api.NewUserController(store, beforeCreateUserGate)") {
		t.Errorf("slot operators not injected into non-standard controller constructor in:\n%s", code)
	}

	// Operators should be wired, not discarded.
	if strings.Contains(code, "_ =") {
		t.Errorf("slot operators should not be discarded in:\n%s", code)
	}
}

func TestAssemble_UnifiedImportAliasNamespace(t *testing.T) {
	// Finding 7: Component import "internal/storage" produces alias "storage",
	// and a fill named "storage" also produces alias "storage". With separate
	// disambiguation maps these collide; with a unified namespace the fill
	// gets disambiguated to "storage2".
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/api"},
					Constructors: []string{"api.NewHandler()"},
					Routes:       []string{`mux.HandleFunc("GET /", handler.Index)`},
				},
			},
			{
				Name: "postgres-adapter",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/storage"},
					Constructors: []string{"storage.NewStore(db)"},
					NeedsDB:      true,
				},
			},
		},
		SlotBindings: []types.SlotDeclaration{
			{Slot: "validate", Gate: []string{"storage"}},
		},
		SlotsPackage: "internal/slots",
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	// Must parse as valid Go — separate maps would produce duplicate "storage" alias.
	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// Both imports should be present with distinct aliases.
	if !strings.Contains(code, `storage "github.com/myorg/svc/internal/storage"`) {
		t.Errorf("missing component storage import in:\n%s", code)
	}
	if !strings.Contains(code, `"github.com/myorg/svc/fills/storage"`) {
		t.Errorf("missing fill storage import in:\n%s", code)
	}
	// The fill alias should be disambiguated.
	if !strings.Contains(code, "storage2") {
		t.Errorf("fill 'storage' alias should be disambiguated to storage2 in:\n%s", code)
	}
}

func TestAssemble_SlotsAliasCollisionWithComponent(t *testing.T) {
	// The hardcoded "slots" alias should be reserved before component
	// imports, so a component with base path "slots" gets disambiguated.
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/api"},
					Constructors: []string{"api.NewHandler()"},
					Routes:       []string{`mux.HandleFunc("GET /", handler.Index)`},
				},
			},
			{
				Name: "slots-component",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/slots"},
					Constructors: []string{"slots.NewProcessor()"},
				},
			},
		},
		SlotBindings: []types.SlotDeclaration{
			{Slot: "validate", Gate: []string{"my-fill"}},
		},
		SlotsPackage: "pkg/slots",
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// The slots package should get the "slots" alias, and the component
	// import should be disambiguated.
	if !strings.Contains(code, "slots2") {
		t.Errorf("component 'internal/slots' alias should be disambiguated when slots package is present in:\n%s", code)
	}

	// Finding 14: Constructor expression must use the disambiguated alias.
	if !strings.Contains(code, "slots2.NewProcessor()") {
		t.Errorf("component constructor should use disambiguated alias slots2 in:\n%s", code)
	}
}

func TestAssemble_FillAliasCollisionWithComponent(t *testing.T) {
	// A fill named "api" should not collide with a component import alias "api".
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/api"},
					Constructors: []string{"api.NewHandler()"},
					Routes:       []string{`mux.HandleFunc("GET /", handler.Index)`},
				},
			},
		},
		SlotBindings: []types.SlotDeclaration{
			{Slot: "validate", Gate: []string{"api"}},
		},
		SlotsPackage: "internal/slots",
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// The fill named "api" should get a disambiguated alias since the
	// component already claimed "api".
	if !strings.Contains(code, "api2") {
		t.Errorf("fill 'api' alias should be disambiguated to api2 in:\n%s", code)
	}
}

func TestAssemble_HandlerConstructorCollisionUpdatesRoutes(t *testing.T) {
	// Finding 8: When two handler constructors produce the same base variable
	// name, routes from the second component must reference the disambiguated
	// variable name, not the original.
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "component-a",
				Wiring: &gen.Wiring{
					Imports:             []string{"internal/a"},
					Constructors:        []string{"a.NewUserHandler(store)"},
					ConstructorEntities: map[int]string{0: "User"},
					Routes: []string{
						`mux.HandleFunc("POST /a/users", userHandler.Create)`,
					},
				},
			},
			{
				Name: "component-b",
				Wiring: &gen.Wiring{
					Imports:             []string{"internal/b"},
					Constructors:        []string{"b.NewUserHandler(store)"},
					ConstructorEntities: map[int]string{0: "User"},
					Routes: []string{
						`mux.HandleFunc("POST /b/users", userHandler.Create)`,
					},
				},
			},
		},
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// First handler keeps original name.
	if !strings.Contains(code, "userHandler :=") {
		t.Errorf("first handler should keep original variable name in:\n%s", code)
	}
	// Second handler gets disambiguated.
	if !strings.Contains(code, "userHandler2 :=") {
		t.Errorf("second handler should be disambiguated to userHandler2 in:\n%s", code)
	}
	// First route should reference original name.
	if !strings.Contains(code, `userHandler.Create)`) {
		t.Errorf("first route should reference userHandler in:\n%s", code)
	}
	// Second route should reference disambiguated name.
	if !strings.Contains(code, `userHandler2.Create)`) {
		t.Errorf("second route should reference userHandler2 in:\n%s", code)
	}
}

func TestAssemble_NoRoutesNoDB_NoLogImport(t *testing.T) {
	// Finding 9: When neither routes nor DB are present, "log" should not be
	// imported since it is only used in writeDBSetup and writeServerStart.
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Wirings: []ComponentWiring{
			{
				Name: "stub",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/stub"},
					Constructors: []string{"stub.NewProcessor()"},
				},
			},
		},
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// log should NOT be imported when neither routes nor DB are present.
	if strings.Contains(code, `"log"`) {
		t.Errorf("log should not be imported when no routes and no DB in:\n%s", code)
	}
}

func TestInjectConstructorArgs(t *testing.T) {
	tests := []struct {
		expr string
		args []string
		want string
	}{
		{"api.NewUserHandler(store)", []string{"gateOp"}, "api.NewUserHandler(store, gateOp)"},
		{"api.NewUserHandler(store)", []string{"gateOp", "fanOutOp"}, "api.NewUserHandler(store, gateOp, fanOutOp)"},
		{"api.NewHandler()", []string{"gateOp"}, "api.NewHandler(gateOp)"},
		{"api.NewHandler(store)", nil, "api.NewHandler(store)"},
	}
	for _, tt := range tests {
		got := injectConstructorArgs(tt.expr, tt.args)
		if got != tt.want {
			t.Errorf("injectConstructorArgs(%q, %v) = %q, want %q", tt.expr, tt.args, got, tt.want)
		}
	}
}

func TestAssemble_ConstructorCollidesWithAssemblerInternalVars(t *testing.T) {
	// Finding 13: Assembler-internal variables (mux, addr, db, dsn, err)
	// must be reserved in the constructor disambiguation namespace.
	tests := []struct {
		name        string
		constructor string
		wantVar     string
		hasRoutes   bool
		hasDB       bool
	}{
		{
			name:        "mux collision with routes",
			constructor: "muxutil.NewMux()",
			wantVar:     "mux2 :=",
			hasRoutes:   true,
		},
		{
			name:        "addr collision with routes",
			constructor: "config.NewAddr()",
			wantVar:     "addr2 :=",
			hasRoutes:   true,
		},
		{
			name:        "db collision with database",
			constructor: "pool.NewDb()",
			wantVar:     "db2 :=",
			hasDB:       true,
		},
		{
			name:        "dsn collision with database",
			constructor: "config.NewDsn()",
			wantVar:     "dsn2 :=",
			hasDB:       true,
		},
		{
			name:        "err collision with database",
			constructor: "errutil.NewErr()",
			wantVar:     "err2 :=",
			hasDB:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wirings := []ComponentWiring{
				{
					Name: "colliding-component",
					Wiring: &gen.Wiring{
						Imports:      []string{"internal/colliding"},
						Constructors: []string{tt.constructor},
					},
				},
			}

			if tt.hasRoutes {
				wirings = append(wirings, ComponentWiring{
					Name: "rest-api",
					Wiring: &gen.Wiring{
						Imports:      []string{"internal/api"},
						Constructors: []string{"api.NewHandler()"},
						Routes:       []string{`mux.HandleFunc("GET /", handler.Index)`},
					},
				})
			}

			if tt.hasDB {
				wirings = append(wirings, ComponentWiring{
					Name: "postgres-adapter",
					Wiring: &gen.Wiring{
						Imports:      []string{"internal/storage"},
						Constructors: []string{"storage.NewStore(db)"},
						NeedsDB:      true,
					},
				})
				// Need routes to make the output non-trivial, or at least
				// the DB setup emits variables.
			}

			input := AssemblerInput{
				ModuleName:  "github.com/myorg/svc",
				ServiceName: "svc",
				GoVersion:   "1.22",
				Port:        8080,
				Wirings:     wirings,
			}

			files, err := Assemble(input)
			if err != nil {
				t.Fatalf("Assemble: %v", err)
			}

			var mainGo gen.File
			for _, f := range files {
				if f.Path == "cmd/main.go" {
					mainGo = f
				}
			}

			code := string(mainGo.Content)

			// Must parse as valid Go — without seeding assembler-internal
			// vars, the constructor would produce a duplicate declaration.
			fset := token.NewFileSet()
			_, parseErr := parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
			if parseErr != nil {
				t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, parseErr)
			}

			// Verify the constructor variable was disambiguated.
			if !strings.Contains(code, tt.wantVar) {
				t.Errorf("expected disambiguated variable %q in:\n%s", tt.wantVar, code)
			}
		})
	}
}

func TestReplaceIdentRef(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		oldName string
		newName string
		want    string
	}{
		{
			name:    "basic replacement",
			input:   "store.Get()",
			oldName: "store",
			newName: "store2",
			want:    "store2.Get()",
		},
		{
			name:    "does not corrupt longer identifier",
			input:   "datastore.Get()",
			oldName: "store",
			newName: "store2",
			want:    "datastore.Get()",
		},
		{
			name:    "replaces at start of string",
			input:   "store.Get()",
			oldName: "store",
			newName: "store2",
			want:    "store2.Get()",
		},
		{
			name:    "replaces after non-ident char",
			input:   "(store.Get())",
			oldName: "store",
			newName: "store2",
			want:    "(store2.Get())",
		},
		{
			name:    "multiple occurrences",
			input:   "store.Get(), store.Put()",
			oldName: "store",
			newName: "store2",
			want:    "store2.Get(), store2.Put()",
		},
		{
			name:    "mixed match and non-match",
			input:   "datastore.Get(), store.Put()",
			oldName: "store",
			newName: "store2",
			want:    "datastore.Get(), store2.Put()",
		},
		{
			name:    "no-op when same name",
			input:   "store.Get()",
			oldName: "store",
			newName: "store",
			want:    "store.Get()",
		},
		{
			name:    "underscore prefix is ident boundary",
			input:   "_store.Get()",
			oldName: "store",
			newName: "store2",
			want:    "_store.Get()",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := replaceIdentRef(tt.input, tt.oldName, tt.newName)
			if got != tt.want {
				t.Errorf("replaceIdentRef(%q, %q, %q) = %q, want %q",
					tt.input, tt.oldName, tt.newName, got, tt.want)
			}
		})
	}
}

func TestAssemble_VarRenameWordBoundary(t *testing.T) {
	// Finding 15: Variable rename "store" → "store2" must not corrupt
	// "datastore.Get" into "datastore2.Get".
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "component-a",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/a"},
					Constructors: []string{"a.NewStore(db)"},
					NeedsDB:      true,
				},
			},
			{
				Name: "component-b",
				Wiring: &gen.Wiring{
					Imports: []string{"internal/b"},
					Constructors: []string{
						"b.NewStore(db)",
						"b.NewDatastore()",
					},
					Routes: []string{
						`mux.HandleFunc("GET /data", datastore.Get)`,
					},
				},
			},
		},
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// The second "store" constructor should be disambiguated to "store2".
	if !strings.Contains(code, "store2 :=") {
		t.Errorf("missing disambiguated store2 in:\n%s", code)
	}

	// The "datastore" variable must NOT be corrupted to "datastore2".
	if strings.Contains(code, "datastore2") {
		t.Errorf("datastore should NOT be renamed — word boundary violation in:\n%s", code)
	}

	// The route referencing datastore must remain intact.
	if !strings.Contains(code, "datastore.Get") {
		t.Errorf("route should still reference datastore.Get in:\n%s", code)
	}
}

func TestAssemble_StdlibImportAliasShadowing(t *testing.T) {
	// Finding 16: Constructors that derive stdlib import alias names (log,
	// fmt, http) must be disambiguated to prevent shadowing.
	tests := []struct {
		name        string
		constructor string
		wantVar     string
	}{
		{
			name:        "log collision",
			constructor: "logger.NewLog()",
			wantVar:     "log2 :=",
		},
		{
			name:        "fmt collision",
			constructor: "formatter.NewFmt()",
			wantVar:     "fmt2 :=",
		},
		{
			name:        "http collision",
			constructor: "client.NewHttp()",
			wantVar:     "http2 :=",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := AssemblerInput{
				ModuleName:  "github.com/myorg/svc",
				ServiceName: "svc",
				GoVersion:   "1.22",
				Port:        8080,
				Wirings: []ComponentWiring{
					{
						Name: "colliding-component",
						Wiring: &gen.Wiring{
							Imports:      []string{"internal/colliding"},
							Constructors: []string{tt.constructor},
						},
					},
					{
						Name: "rest-api",
						Wiring: &gen.Wiring{
							Imports:      []string{"internal/api"},
							Constructors: []string{"api.NewHandler()"},
							Routes:       []string{`mux.HandleFunc("GET /", handler.Index)`},
						},
					},
				},
			}

			files, err := Assemble(input)
			if err != nil {
				t.Fatalf("Assemble: %v", err)
			}

			var mainGo gen.File
			for _, f := range files {
				if f.Path == "cmd/main.go" {
					mainGo = f
				}
			}

			code := string(mainGo.Content)

			fset := token.NewFileSet()
			_, parseErr := parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
			if parseErr != nil {
				t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, parseErr)
			}

			if !strings.Contains(code, tt.wantVar) {
				t.Errorf("expected disambiguated variable %q in:\n%s", tt.wantVar, code)
			}
		})
	}
}

func TestAssemble_ImportAliasRenameInRoutes(t *testing.T) {
	// Finding 14: When a component's import alias is disambiguated,
	// route expressions from that component must also be updated.
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "component-a",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/api/handler"},
					Constructors: []string{"handler.NewFoo()"},
					Routes:       []string{`mux.HandleFunc("GET /foo", foo.Get)`},
				},
			},
			{
				Name: "component-b",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/other/handler"},
					Constructors: []string{"handler.NewBar()"},
					Routes:       []string{`mux.HandleFunc("GET /bar", bar.Get)`},
				},
			},
		},
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// Component-b's import should be disambiguated.
	if !strings.Contains(code, "handler2") {
		t.Errorf("missing disambiguated handler2 alias in:\n%s", code)
	}

	// Component-b's constructor should use the disambiguated alias.
	if !strings.Contains(code, "handler2.NewBar()") {
		t.Errorf("component-b constructor should use handler2 in:\n%s", code)
	}

	// Component-a's constructor should keep the original alias.
	if !strings.Contains(code, "handler.NewFoo()") {
		t.Errorf("component-a constructor should keep handler alias in:\n%s", code)
	}
}

func TestAssemble_IntraWiringConstructorCollision(t *testing.T) {
	// Finding 17: Two constructors in the same wiring that derive the same
	// base variable name must be rejected — routes cannot unambiguously
	// reference either constructor when both have the same derived name.
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports: []string{"internal/api"},
					Constructors: []string{
						"api.NewHandler(storeA)",
						"api.NewHandler(storeB)",
					},
					Routes: []string{
						`mux.HandleFunc("GET /users", handler.ListUsers)`,
					},
				},
			},
		},
	}

	_, err := Assemble(input)
	if err == nil {
		t.Fatal("expected error for intra-wiring constructor name collision, got nil")
	}
	if !strings.Contains(err.Error(), "multiple constructors") {
		t.Errorf("expected 'multiple constructors' error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "handler") {
		t.Errorf("error should identify the colliding variable name, got: %v", err)
	}
	if !strings.Contains(err.Error(), "rest-api") {
		t.Errorf("error should identify the component name, got: %v", err)
	}
}

func TestAssemble_IntraWiringMiddlewareCollision(t *testing.T) {
	// Finding 17 (middleware aspect): Two constructors in the same wiring
	// with the same derived name is rejected as ambiguous.
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "auth-and-logging",
				Wiring: &gen.Wiring{
					Imports: []string{"internal/middleware"},
					Constructors: []string{
						"middleware.NewMiddleware()",
						"middleware.NewMiddleware(logCfg)",
					},
					MiddlewareConstructor: intPtr(0),
					Routes: []string{
						`mux.HandleFunc("GET /", root.Index)`,
					},
				},
			},
		},
	}

	_, err := Assemble(input)
	if err == nil {
		t.Fatal("expected error for intra-wiring constructor name collision, got nil")
	}
	if !strings.Contains(err.Error(), "multiple constructors") {
		t.Errorf("expected 'multiple constructors' error, got: %v", err)
	}
}

func TestAssemble_InterWiringMiddlewareRename(t *testing.T) {
	// The middleware constructor's variable is correctly resolved when
	// it collides with a constructor in a DIFFERENT wiring (inter-wiring
	// disambiguation works correctly via per-constructor-index tracking).
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "component-a",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/a"},
					Constructors: []string{"a.NewMiddleware()"},
				},
			},
			{
				Name: "auth",
				Wiring: &gen.Wiring{
					Imports:               []string{"internal/auth"},
					Constructors:          []string{"auth.NewMiddleware()"},
					MiddlewareConstructor: intPtr(0),
				},
			},
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/api"},
					Constructors: []string{"api.NewHandler()"},
					Routes:       []string{`mux.HandleFunc("GET /", handler.Index)`},
				},
			},
		},
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// Component-a gets "middleware", auth gets "middleware2".
	// The middleware wrapping should use "middleware2" (the auth component).
	if !strings.Contains(code, "middleware2.Middleware(mux)") {
		t.Errorf("server start should reference disambiguated middleware2 in:\n%s", code)
	}
}

func TestAssemble_DuplicateSlotBinding(t *testing.T) {
	// Finding 18: Duplicate (slot, entity, operator) triples must be
	// rejected with a clear error.
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/api"},
					Constructors: []string{"api.NewHandler()"},
					Routes:       []string{`mux.HandleFunc("GET /", handler.Index)`},
				},
			},
		},
		SlotBindings: []types.SlotDeclaration{
			{
				Slot:   "before_create",
				Entity: "User",
				Gate:   []string{"policy-a"},
			},
			{
				Slot:   "before_create",
				Entity: "User",
				Gate:   []string{"policy-b"},
			},
		},
		SlotsPackage: "internal/slots",
	}

	_, err := Assemble(input)
	if err == nil {
		t.Fatal("expected error for duplicate slot binding, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate slot binding") {
		t.Errorf("expected 'duplicate slot binding' error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "before_create") {
		t.Errorf("error should identify the slot name, got: %v", err)
	}
	if !strings.Contains(err.Error(), "User") {
		t.Errorf("error should identify the entity, got: %v", err)
	}
}

func TestAssemble_DuplicateSlotBindingDifferentOperator(t *testing.T) {
	// Same (slot, entity) with DIFFERENT operators is valid — only
	// same (slot, entity, operator) is a duplicate.
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports:             []string{"internal/api"},
					Constructors:        []string{"api.NewUserHandler(store)"},
					ConstructorEntities: map[int]string{0: "User"},
					Routes:              []string{`mux.HandleFunc("POST /users", userHandler.Create)`},
				},
			},
			{
				Name: "postgres-adapter",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/storage"},
					Constructors: []string{"storage.NewStore(db)"},
					NeedsDB:      true,
				},
			},
		},
		SlotBindings: []types.SlotDeclaration{
			{
				Slot:   "before_create",
				Entity: "User",
				Gate:   []string{"policy-a"},
			},
			{
				Slot:   "before_create",
				Entity: "User",
				FanOut: []string{"notifier"},
			},
		},
		SlotsPackage: "internal/slots",
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble should succeed for different operators: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// Both operators should be present with distinct variable names.
	if !strings.Contains(code, "beforeCreateUserGate") {
		t.Errorf("missing gate operator in:\n%s", code)
	}
	if !strings.Contains(code, "beforeCreateUserFanOut") {
		t.Errorf("missing fan-out operator in:\n%s", code)
	}
}

func TestAssemble_DuplicateSlotBindingNoEntity(t *testing.T) {
	// Duplicate (slot, "", operator) — entity-less duplicate.
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/api"},
					Constructors: []string{"api.NewHandler()"},
					Routes:       []string{`mux.HandleFunc("GET /", handler.Index)`},
				},
			},
		},
		SlotBindings: []types.SlotDeclaration{
			{
				Slot:  "validate",
				Chain: []string{"step-a"},
			},
			{
				Slot:  "validate",
				Chain: []string{"step-b"},
			},
		},
		SlotsPackage: "internal/slots",
	}

	_, err := Assemble(input)
	if err == nil {
		t.Fatal("expected error for duplicate entity-less slot binding, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate slot binding") {
		t.Errorf("expected 'duplicate slot binding' error, got: %v", err)
	}
}

func TestAssemble_StdlibImportAliasShadowingByComponent(t *testing.T) {
	// Finding 19: A component with import path base "sql" should NOT get
	// alias "sql" when "database/sql" is also imported (hasDB=true),
	// because the explicit alias shadows the implicit stdlib alias.
	tests := []struct {
		name      string
		importDir string
		hasDB     bool
		hasRoutes bool
		wantAlias string
	}{
		{
			name:      "component base sql with hasDB",
			importDir: "internal/sql",
			hasDB:     true,
			wantAlias: "sql2",
		},
		{
			name:      "component base os with hasDB",
			importDir: "internal/os",
			hasDB:     true,
			wantAlias: "os2",
		},
		{
			name:      "component base fmt with hasRoutes",
			importDir: "internal/fmt",
			hasRoutes: true,
			wantAlias: "fmt2",
		},
		{
			name:      "component base http with hasRoutes",
			importDir: "internal/http",
			hasRoutes: true,
			wantAlias: "http2",
		},
		{
			name:      "component base log with hasDB",
			importDir: "internal/log",
			hasDB:     true,
			wantAlias: "log2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wirings := []ComponentWiring{
				{
					Name: "colliding-component",
					Wiring: &gen.Wiring{
						Imports:      []string{tt.importDir},
						Constructors: []string{"pkg.NewProcessor()"},
					},
				},
			}

			if tt.hasRoutes {
				wirings = append(wirings, ComponentWiring{
					Name: "rest-api",
					Wiring: &gen.Wiring{
						Imports:      []string{"internal/api"},
						Constructors: []string{"api.NewHandler()"},
						Routes:       []string{`mux.HandleFunc("GET /", handler.Index)`},
					},
				})
			}

			if tt.hasDB {
				wirings = append(wirings, ComponentWiring{
					Name: "postgres-adapter",
					Wiring: &gen.Wiring{
						Imports:      []string{"internal/storage"},
						Constructors: []string{"storage.NewStore(db)"},
						NeedsDB:      true,
					},
				})
			}

			// Need routes for a valid main.go in some cases.
			if !tt.hasRoutes {
				wirings = append(wirings, ComponentWiring{
					Name: "rest-api",
					Wiring: &gen.Wiring{
						Imports:      []string{"internal/api"},
						Constructors: []string{"api.NewHandler()"},
						Routes:       []string{`mux.HandleFunc("GET /", handler.Index)`},
					},
				})
			}

			input := AssemblerInput{
				ModuleName:  "github.com/myorg/svc",
				ServiceName: "svc",
				GoVersion:   "1.22",
				Port:        8080,
				Wirings:     wirings,
			}

			files, err := Assemble(input)
			if err != nil {
				t.Fatalf("Assemble: %v", err)
			}

			var mainGo gen.File
			for _, f := range files {
				if f.Path == "cmd/main.go" {
					mainGo = f
				}
			}

			code := string(mainGo.Content)

			fset := token.NewFileSet()
			_, parseErr := parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
			if parseErr != nil {
				t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, parseErr)
			}

			// The component import should get a disambiguated alias.
			if !strings.Contains(code, tt.wantAlias) {
				t.Errorf("expected disambiguated alias %q in:\n%s", tt.wantAlias, code)
			}
		})
	}
}

func TestAssemble_StdlibAliasShadowingByFill(t *testing.T) {
	// Finding 19: A fill named "log" should get alias "log2" when
	// hasDB || hasRoutes (since "log" is imported as a stdlib package).
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/api"},
					Constructors: []string{"api.NewHandler()"},
					Routes:       []string{`mux.HandleFunc("GET /", handler.Index)`},
				},
			},
		},
		SlotBindings: []types.SlotDeclaration{
			{Slot: "validate", Gate: []string{"log"}},
		},
		SlotsPackage: "internal/slots",
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// The fill alias should be disambiguated to avoid shadowing stdlib "log".
	if !strings.Contains(code, "log2") {
		t.Errorf("fill 'log' alias should be disambiguated to log2 in:\n%s", code)
	}
}

func TestAssemble_SlotVarNameNormalizationCollision(t *testing.T) {
	// Finding 20: Two slot names that differ only in underscore structure
	// (e.g. "before_create" and "before__create") normalize to the same
	// camelCase identifier and must be rejected.
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/api"},
					Constructors: []string{"api.NewHandler()"},
					Routes:       []string{`mux.HandleFunc("GET /", handler.Index)`},
				},
			},
		},
		SlotBindings: []types.SlotDeclaration{
			{
				Slot:   "before_create",
				Entity: "User",
				Gate:   []string{"policy-a"},
			},
			{
				Slot:   "before__create",
				Entity: "User",
				Gate:   []string{"policy-b"},
			},
		},
		SlotsPackage: "internal/slots",
	}

	_, err := Assemble(input)
	if err == nil {
		t.Fatal("expected error for slot names that normalize to the same variable name, got nil")
	}
	if !strings.Contains(err.Error(), "same variable name") {
		t.Errorf("expected 'same variable name' error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "before_create") {
		t.Errorf("error should identify first slot name, got: %v", err)
	}
	if !strings.Contains(err.Error(), "before__create") {
		t.Errorf("error should identify second slot name, got: %v", err)
	}
}

func TestAssemble_SlotVarNameNormalizationCollisionDifferentEntities(t *testing.T) {
	// Slot names that normalize the same but have different entities produce
	// different variable names (entity is part of the name), so should succeed.
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports: []string{"internal/api"},
					Constructors: []string{
						"api.NewUserHandler(store)",
						"api.NewOrgHandler(store)",
					},
					ConstructorEntities: map[int]string{
						0: "User",
						1: "Org",
					},
					Routes: []string{
						`mux.HandleFunc("POST /users", userHandler.Create)`,
						`mux.HandleFunc("POST /orgs", orgHandler.Create)`,
					},
				},
			},
			{
				Name: "postgres-adapter",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/storage"},
					Constructors: []string{"storage.NewStore(db)"},
					NeedsDB:      true,
				},
			},
		},
		SlotBindings: []types.SlotDeclaration{
			{
				Slot:   "before_create",
				Entity: "User",
				Gate:   []string{"policy-a"},
			},
			{
				Slot:   "before__create",
				Entity: "Org",
				Gate:   []string{"policy-b"},
			},
		},
		SlotsPackage: "internal/slots",
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble should succeed when normalized names differ by entity: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// Both should have distinct variable names because entities differ.
	if !strings.Contains(code, "beforeCreateUserGate") {
		t.Errorf("missing beforeCreateUserGate in:\n%s", code)
	}
	if !strings.Contains(code, "beforeCreateOrgGate") {
		t.Errorf("missing beforeCreateOrgGate in:\n%s", code)
	}
}

func TestAssemble_NonStdlibImportAliasShadowingByConstructor(t *testing.T) {
	// Finding 21: A constructor whose derived variable name matches a non-stdlib
	// import alias must be disambiguated. Without this, the constructor variable
	// shadows the import alias, and later constructors referencing that import
	// resolve to the local variable instead — producing an unused-import compile
	// error or wrong-package reference.
	//
	// Reproduction: wiring 0 has import "internal/cache" and constructor
	// cache.NewStorage() → var "storage". Wiring 1 has import "internal/storage"
	// (alias "storage") and constructor storage.NewStore(db). Without seeding,
	// "storage := cache.NewStorage()" shadows the "storage" import alias.
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "cache-component",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/cache"},
					Constructors: []string{"cache.NewStorage()"},
				},
			},
			{
				Name: "postgres-adapter",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/storage"},
					Constructors: []string{"storage.NewStore(db)"},
					NeedsDB:      true,
				},
			},
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/api"},
					Constructors: []string{"api.NewHandler()"},
					Routes:       []string{`mux.HandleFunc("GET /", handler.Index)`},
				},
			},
		},
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// The constructor cache.NewStorage() derives var name "storage", which
	// collides with the import alias "storage" for "internal/storage".
	// It must be disambiguated to "storage2" (or similar).
	if !strings.Contains(code, "storage2 :=") {
		t.Errorf("constructor var 'storage' should be disambiguated to avoid shadowing import alias in:\n%s", code)
	}

	// The import alias "storage" must remain intact for the postgres-adapter
	// constructor to reference correctly.
	if !strings.Contains(code, "storage.NewStore(db)") {
		t.Errorf("postgres-adapter constructor should still reference 'storage' import alias in:\n%s", code)
	}
}

func TestAssemble_NonStdlibImportAliasShadowingByFillAlias(t *testing.T) {
	// Finding 21 (fill alias variant): A constructor whose derived variable
	// name matches a fill import alias must be disambiguated.
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "component-a",
				Wiring: &gen.Wiring{
					Imports: []string{"internal/maker"},
					// Derives var name "validator" which matches fill alias.
					Constructors: []string{"maker.NewValidator()"},
				},
			},
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/api"},
					Constructors: []string{"api.NewHandler()"},
					Routes:       []string{`mux.HandleFunc("GET /", handler.Index)`},
				},
			},
		},
		SlotBindings: []types.SlotDeclaration{
			// Fill name "validator" produces alias "validator".
			{Slot: "before_create", Gate: []string{"validator"}},
		},
		SlotsPackage: "internal/slots",
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// The constructor var "validator" should be disambiguated because a fill
	// import alias "validator" exists in the same scope.
	if !strings.Contains(code, "validator2 :=") {
		t.Errorf("constructor var 'validator' should be disambiguated to avoid shadowing fill import alias in:\n%s", code)
	}
}

func TestAssemble_NonStdlibSlotsAliasShadowingByConstructor(t *testing.T) {
	// Finding 21 (slots alias variant): A constructor whose derived variable
	// name matches the slots package alias must be disambiguated.
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "component-a",
				Wiring: &gen.Wiring{
					Imports: []string{"internal/maker"},
					// Derives var name "slots" which matches slots package alias.
					Constructors: []string{"maker.NewSlots()"},
				},
			},
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports:             []string{"internal/api"},
					Constructors:        []string{"api.NewUserHandler(store)"},
					ConstructorEntities: map[int]string{0: "User"},
					Routes:              []string{`mux.HandleFunc("POST /users", userHandler.Create)`},
				},
			},
			{
				Name: "postgres-adapter",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/storage"},
					Constructors: []string{"storage.NewStore(db)"},
					NeedsDB:      true,
				},
			},
		},
		SlotBindings: []types.SlotDeclaration{
			{
				Slot:   "before_create",
				Entity: "User",
				Gate:   []string{"my-policy"},
			},
		},
		SlotsPackage: "internal/slots",
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// The constructor var "slots" should be disambiguated because the
	// slots package import uses alias "slots" in the same scope.
	if !strings.Contains(code, "slots2 :=") {
		t.Errorf("constructor var 'slots' should be disambiguated to avoid shadowing slots import alias in:\n%s", code)
	}

	// Slot wiring should still reference the original "slots" import alias.
	if !strings.Contains(code, "slots.NewBeforeCreateGate") {
		t.Errorf("slot wiring should reference 'slots' import alias in:\n%s", code)
	}
}

func TestAssemble_SharedImportPathAcrossWirings(t *testing.T) {
	// Finding 22: When two wirings declare the same import path and that path's
	// alias was disambiguated, the rename must be propagated to ALL wirings —
	// not just the first one processed.
	//
	// Wiring 0: imports "internal/api" (gets alias "api")
	// Wiring 1: imports "internal/other/api" (gets alias "api2", rename stored)
	// Wiring 2: also imports "internal/other/api" (skipped by seen map — must
	//           get the same rename "api" → "api2" propagated)
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "component-a",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/api"},
					Constructors: []string{"api.NewFoo()"},
					Routes:       []string{`mux.HandleFunc("GET /foo", foo.Get)`},
				},
			},
			{
				Name: "component-b",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/other/api"},
					Constructors: []string{"api.NewBar()"},
					Routes:       []string{`mux.HandleFunc("GET /bar", bar.Get)`},
				},
			},
			{
				Name: "component-c",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/other/api"},
					Constructors: []string{"api.NewBaz()"},
					Routes:       []string{`mux.HandleFunc("GET /baz", baz.Get)`},
				},
			},
		},
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// Component-a keeps alias "api" — its constructor should be unchanged.
	if !strings.Contains(code, "api.NewFoo()") {
		t.Errorf("component-a constructor should use 'api' alias in:\n%s", code)
	}

	// Component-b gets alias "api2" — its constructor must be updated.
	if !strings.Contains(code, "api2.NewBar()") {
		t.Errorf("component-b constructor should use disambiguated 'api2' alias in:\n%s", code)
	}

	// Component-c shares the same import path as component-b — its constructor
	// must ALSO be updated to use "api2". Without the fix, this would still
	// reference "api" (the wrong package).
	if !strings.Contains(code, "api2.NewBaz()") {
		t.Errorf("component-c constructor should use disambiguated 'api2' alias (propagated from component-b) in:\n%s", code)
	}

	// The import for "internal/other/api" should appear exactly once (deduplicated).
	count := strings.Count(code, `"github.com/myorg/svc/internal/other/api"`)
	if count != 1 {
		t.Errorf("shared import should appear exactly once, got %d in:\n%s", count, code)
	}
}

func TestAssemble_SharedImportPathNoDisambiguation(t *testing.T) {
	// When shared import paths do NOT require disambiguation (alias == base),
	// no rename should be propagated — constructors should stay unchanged.
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "component-a",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/storage"},
					Constructors: []string{"storage.NewFoo()"},
					Routes:       []string{`mux.HandleFunc("GET /foo", foo.Get)`},
				},
			},
			{
				Name: "component-b",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/storage"},
					Constructors: []string{"storage.NewBar()"},
				},
			},
		},
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// Both constructors should reference "storage" — no disambiguation needed.
	if !strings.Contains(code, "storage.NewFoo()") {
		t.Errorf("component-a constructor should use 'storage' in:\n%s", code)
	}
	if !strings.Contains(code, "storage.NewBar()") {
		t.Errorf("component-b constructor should use 'storage' in:\n%s", code)
	}
}

func TestAssemble_MultiPassRenameInterference(t *testing.T) {
	// Finding 23: When a constructor's derived variable name equals the base
	// name of a disambiguated import in the same wiring, import alias renames
	// and constructor variable renames conflict. Route expressions reference
	// constructor variables, not packages — only constructor variable renames
	// should be applied to routes.
	//
	// Wiring 0: imports "internal/handler" (alias "handler")
	// Wiring 1: imports "internal/other/handler" (alias "handler2", import rename),
	//   constructor handler.NewHandler() → var "handler" → disambiguated to "handler3"
	//   (because "handler" and "handler2" are both reserved in NonStdlibAliases),
	//   route references "handler.Get" → should become "handler3.Get" (the constructor var)
	//
	// Without the fix: import rename transforms "handler.Get" → "handler2.Get",
	// then constructor rename can't find "handler." → final route is "handler2.Get"
	// (references the import alias, not the constructor variable).
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "component-a",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/handler"},
					Constructors: []string{"handler.NewFoo()"},
					Routes:       []string{`mux.HandleFunc("GET /foo", foo.Get)`},
				},
			},
			{
				Name: "component-b",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/other/handler"},
					Constructors: []string{"handler.NewHandler()"},
					Routes:       []string{`mux.HandleFunc("GET /bar", handler.Get)`},
				},
			},
		},
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// Component-b's constructor "handler.NewHandler()" derives var "handler".
	// "handler" is reserved (import alias for component-a), "handler2" is
	// reserved (import alias for component-b), so the var is disambiguated.
	if !strings.Contains(code, "handler3 :=") {
		t.Errorf("constructor var should be disambiguated to handler3 in:\n%s", code)
	}

	// Component-b's route must reference the constructor variable "handler3",
	// NOT the import alias "handler2".
	if !strings.Contains(code, "handler3.Get") {
		t.Errorf("route should reference constructor var handler3, not import alias in:\n%s", code)
	}

	// Verify the route does NOT reference handler2 (the import alias) —
	// that would be the multi-pass rename interference bug.
	if strings.Contains(code, "handler2.Get") {
		t.Errorf("route should NOT reference import alias handler2 — multi-pass rename interference in:\n%s", code)
	}
}

func TestAssemble_PreReservedRenameDoesNotCorruptRoutes(t *testing.T) {
	// Finding 24: When a constructor's derived variable name collides with an
	// assembler-internal variable (e.g. "mux"), the constructor is disambiguated
	// (mux → mux2). But the rename must NOT be applied to route expressions
	// because routes reference the assembler's own "mux" variable (from
	// http.NewServeMux()), not the constructor. A naive rename turns
	// mux.HandleFunc(...) into mux2.HandleFunc(...), binding routes to the
	// constructor variable instead of the HTTP mux.
	//
	// Critical: the colliding constructor and routes are in the SAME wiring.
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "colliding-component",
				Wiring: &gen.Wiring{
					Imports: []string{"internal/muxutil"},
					Constructors: []string{
						"muxutil.NewMux()",
						"muxutil.NewHealthHandler()",
					},
					Routes: []string{
						`mux.HandleFunc("GET /health", healthHandler.Check)`,
					},
				},
			},
		},
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// The constructor muxutil.NewMux() derives var "mux", which collides
	// with the assembler's own "mux := http.NewServeMux()". The constructor
	// should be disambiguated to "mux2".
	if !strings.Contains(code, "mux2 :=") {
		t.Errorf("constructor var should be disambiguated to mux2 in:\n%s", code)
	}

	// The route must still reference the assembler's "mux" variable, NOT
	// the constructor variable "mux2".
	if !strings.Contains(code, `mux.HandleFunc("GET /health", healthHandler.Check)`) {
		t.Errorf("route should reference assembler's mux, not constructor mux2 in:\n%s", code)
	}

	// The route must NOT have been corrupted to reference mux2.
	if strings.Contains(code, `mux2.HandleFunc`) {
		t.Errorf("route should NOT reference mux2 — pre-reserved rename corruption in:\n%s", code)
	}
}

func TestAssemble_PreReservedRename_StdlibAlias(t *testing.T) {
	// Verify that constructor renames caused by stdlib import alias
	// collisions are also excluded from route expression rewriting.
	// Constructor derives var "log", which collides with the stdlib "log"
	// import alias. The rename log → log2 must NOT corrupt route
	// expressions that reference other variables.
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "logging-component",
				Wiring: &gen.Wiring{
					Imports: []string{"internal/logging"},
					Constructors: []string{
						"logging.NewLog()",
						"logging.NewLogHandler()",
					},
					Routes: []string{
						`mux.HandleFunc("GET /logs", logHandler.List)`,
					},
				},
			},
		},
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// Constructor logging.NewLog() derives var "log", which collides with
	// stdlib "log" import alias. Should be disambiguated.
	if !strings.Contains(code, "log2 :=") {
		t.Errorf("constructor var should be disambiguated to log2 in:\n%s", code)
	}

	// Routes must NOT be corrupted by the pre-reserved rename.
	if strings.Contains(code, "log2Handler") {
		t.Errorf("route should NOT be corrupted by pre-reserved log rename in:\n%s", code)
	}
}

func TestAssemble_NonStdlibAliasRenameAppliedToRoutes(t *testing.T) {
	// Verify that constructor renames caused by non-stdlib import alias
	// collisions ARE applied to route expressions. Non-stdlib import aliases
	// are NOT pre-reserved for route exclusion because routes reference
	// constructor variables, not import aliases (finding 23 removed import
	// renames from routes). The constructor rename must update the route.
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "cache-component",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/cache"},
					Constructors: []string{"cache.NewStorage()"},
					Routes: []string{
						`mux.HandleFunc("GET /cache", storage.Get)`,
					},
				},
			},
			{
				Name: "postgres-adapter",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/storage"},
					Constructors: []string{"storage.NewStore(db)"},
					NeedsDB:      true,
				},
			},
			{
				Name: "rest-api",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/api"},
					Constructors: []string{"api.NewHandler()"},
					Routes:       []string{`mux.HandleFunc("GET /", handler.Index)`},
				},
			},
		},
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// Constructor cache.NewStorage() derives var "storage", which collides
	// with the import alias "storage" for internal/storage. Should be
	// disambiguated to "storage2".
	if !strings.Contains(code, "storage2 :=") {
		t.Errorf("constructor var should be disambiguated to storage2 in:\n%s", code)
	}

	// The route must reference the disambiguated constructor var "storage2",
	// because the rename is NOT pre-reserved (non-stdlib import aliases
	// don't appear in routes).
	if !strings.Contains(code, "storage2.Get") {
		t.Errorf("route should reference disambiguated constructor var storage2 in:\n%s", code)
	}
}

func TestAssemble_InterConstructorRenameStillAppliedToRoutes(t *testing.T) {
	// Verify that renames caused by inter-constructor collisions (NOT
	// pre-reserved) ARE still applied to route expressions. This ensures
	// the fix for finding 24 doesn't regress finding 8.
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		Wirings: []ComponentWiring{
			{
				Name: "component-a",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/a"},
					Constructors: []string{"a.NewItemHandler(store)"},
					Routes: []string{
						`mux.HandleFunc("POST /a/items", itemHandler.Create)`,
					},
				},
			},
			{
				Name: "component-b",
				Wiring: &gen.Wiring{
					Imports:      []string{"internal/b"},
					Constructors: []string{"b.NewItemHandler(store)"},
					Routes: []string{
						`mux.HandleFunc("POST /b/items", itemHandler.Create)`,
					},
				},
			},
		},
	}

	files, err := Assemble(input)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	var mainGo gen.File
	for _, f := range files {
		if f.Path == "cmd/main.go" {
			mainGo = f
		}
	}

	code := string(mainGo.Content)

	fset := token.NewFileSet()
	_, err = parser.ParseFile(fset, "main.go", mainGo.Content, parser.AllErrors)
	if err != nil {
		t.Fatalf("main.go does not parse:\n%s\nerror: %v", code, err)
	}

	// Second handler should be disambiguated.
	if !strings.Contains(code, "itemHandler2 :=") {
		t.Errorf("second handler should be disambiguated in:\n%s", code)
	}
	// Second route should reference disambiguated name (inter-constructor
	// rename IS applied to routes).
	if !strings.Contains(code, "itemHandler2.Create") {
		t.Errorf("second route should reference itemHandler2 in:\n%s", code)
	}
}

func TestGenerateGoMod_IncludesReplaceDirectivesForFills(t *testing.T) {
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
		SlotBindings: []types.SlotDeclaration{
			{
				Slot:   "before_create",
				Entity: "User",
				Gate:   []string{"admin-creation-policy"},
			},
			{
				Slot:   "on_entity_changed",
				Entity: "User",
				FanOut: []string{"audit-logger", "user-change-notifier"},
			},
		},
	}

	goMod := generateGoMod(input)

	content := string(goMod.Content)

	// Each fill should have a replace directive mapping from the module-qualified
	// import path to a relative path from out/ to the project root's fills.
	expectedReplaces := []string{
		"replace github.com/myorg/svc/fills/admin-creation-policy => ../fills/admin-creation-policy",
		"replace github.com/myorg/svc/fills/audit-logger => ../fills/audit-logger",
		"replace github.com/myorg/svc/fills/user-change-notifier => ../fills/user-change-notifier",
	}
	for _, expected := range expectedReplaces {
		if !strings.Contains(content, expected) {
			t.Errorf("go.mod missing expected replace directive %q\ngot:\n%s", expected, content)
		}
	}
}

func TestGenerateGoMod_NoReplacesWithoutSlots(t *testing.T) {
	input := AssemblerInput{
		ModuleName:  "github.com/myorg/svc",
		ServiceName: "svc",
		GoVersion:   "1.22",
		Port:        8080,
	}

	goMod := generateGoMod(input)
	content := string(goMod.Content)

	if strings.Contains(content, "replace") {
		t.Errorf("go.mod should have no replace directives without slot bindings, got:\n%s", content)
	}
}

// intPtr returns a pointer to an int value, for use in test literals.
func intPtr(v int) *int { return &v }
