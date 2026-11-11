package postgresadapter_test

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
	"github.com/jsell-rh/stego/internal/generator/tenancy"
	"github.com/jsell-rh/stego/internal/types"
)

//go:embed testdata/tables_test.go
var tablesTests []byte

const tablesModule = "example.com/tables-check"

// runtimeService gives the context of a pooled service on the runtime data path whose adapter writes to ns and
// whose tenancy component writes to tenancyNS.
func runtimeService(ns, tenancyNS string, entities []types.Entity, collections []types.Collection) gen.Context {
	return gen.Context{
		OutputNamespace: ns, ModuleName: tablesModule, ServiceName: "tables-check", RuntimeData: true,
		Tenancy:         &gen.Tenancy{Key: "tenant_id"},
		PeerNamespaces:  map[string]string{"postgres-adapter": ns, gen.TenancyComponent: tenancyNS},
		ComponentConfig: map[string]any{"schema_generation": "tables-v1", "writer_lease": "off"},
		Entities:        entities, Collections: collections,
	}
}

// orderService is the service of ST-SUP-RC01-OC41-T01: pooled tenancy and the protected, owned entity Order with
// the fields sku and qty (its key id is implicit), and the collection orders with create, read, list, patch and
// delete.
func orderService(ns string) gen.Context {
	return runtimeService(ns, "tenancy",
		[]types.Entity{{Name: "Order", EntityAuthorization: types.EntityAuthorization{ResourceType: "order"},
			Fields: []types.Field{{Name: "sku", Type: types.FieldTypeString, Unique: true}, {Name: "qty", Type: types.FieldTypeInt64}}}},
		[]types.Collection{{Name: "orders", Entity: "Order",
			Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList, types.OpPatch, types.OpDelete}}})
}

// widgetEntities are the entities of ST-SUP-RC01-OC41-T02: the protected, owned Widget with a field of each type
// and each constraint, the owned Order that refers to a Widget and to the shared Catalog, and the system Mark.
func widgetEntities() []types.Entity {
	n := func(v int) *int { return &v }
	f := func(v float64) *float64 { return &v }
	return []types.Entity{
		{Name: "Widget", EntityAuthorization: types.EntityAuthorization{ResourceType: "widget"}, Fields: []types.Field{
			{Name: "name", Type: types.FieldTypeString, Unique: true, MinLength: n(2), MaxLength: n(64)},
			{Name: "kind", Type: types.FieldTypeEnum, Values: []string{"small", "large"}, Default: "small"},
			{Name: "weight", Type: types.FieldTypeDouble, Min: f(0), Max: f(1000)},
			{Name: "ratio", Type: types.FieldTypeFloat, Optional: true, Min: f(0), Max: f(1)},
			{Name: "count", Type: types.FieldTypeInt32, Default: 0},
			{Name: "total", Type: types.FieldTypeInt64, Min: f(1)},
			{Name: "active", Type: types.FieldTypeBool, Default: true},
			{Name: "payload", Type: types.FieldTypeJsonb, Optional: true},
			{Name: "blob", Type: types.FieldTypeBytes, Optional: true},
			{Name: "seen", Type: types.FieldTypeTimestamp, Optional: true},
			{Name: "code", Type: types.FieldTypeString, UniqueComposite: []string{"code", "kind"}},
			{Name: "serial", Type: types.FieldTypeString, Unique: true, UniqueWhenLive: true},
			{Name: "a_long_field_name_that_pushes_the_check_name_past_the_limit", Type: types.FieldTypeString, Optional: true, MinLength: n(1)},
		}},
		{Name: "Order", Fields: []types.Field{{Name: "widget_id", Type: types.FieldTypeRef, To: "Widget"}, {Name: "catalog_id", Type: types.FieldTypeRef, To: "Catalog"}}},
		{Name: "Catalog", Tenant: "shared", Fields: []types.Field{{Name: "code", Type: types.FieldTypeString, Unique: true}}},
		{Name: "Mark", Tenant: "system", Fields: []types.Field{{Name: "note", Type: types.FieldTypeString, Optional: true}}},
	}
}

// widgetService is the service of ST-SUP-RC01-OC41-T02, with an upsert key over Widget.
func widgetService(entities []types.Entity) gen.Context {
	return runtimeService("widgets", "widgettenancy", entities,
		[]types.Collection{{Name: "widgets", Entity: "Widget", Operations: []types.Operation{types.OpUpsert}, UpsertKey: []string{"name", "code"}}})
}

// generated gives the content of the generated file at name, and fails the test when there is none.
func generated(t *testing.T, files []gen.File, name string) string {
	t.Helper()
	for _, f := range files {
		if f.Path == name {
			return string(f.Content)
		}
	}
	t.Fatalf("no file %s", name)
	return ""
}

// noStore fails the test when a generated Go file migrates models or declares a store or an entity model.
func noStore(t *testing.T, files []gen.File, entities []types.Entity) {
	t.Helper()
	banned := []string{"AutoMigrate", "type Store ", "type Store struct", "func NewStore", "type SessionFactory", "type GenericDao", "type Meta struct"}
	for _, entity := range entities {
		banned = append(banned, "type "+entity.Name+" struct")
	}
	for _, f := range files {
		if filepath.Ext(f.Path) != ".go" {
			continue
		}
		for _, text := range banned {
			if strings.Contains(string(f.Content), text) {
				t.Errorf("%s holds %q", f.Path, text)
			}
		}
		for _, name := range []string{"models.go", "store.go", "session_factory.go", "generic_dao.go", "transaction.go"} {
			if filepath.Base(f.Path) == name {
				t.Errorf("the runtime data path emits %s", f.Path)
			}
		}
	}
}

// tableEvent is one line of go test -json.
type tableEvent struct {
	Action string
	Test   string
	Output string
}

// tableModule writes the generated module of the order service, of the release before it, of the widget service
// and of their tenancy components, runs its test file on PostgreSQL and gives the subtests that passed there with
// the test log. Both tenancy components name the same roles, so one role migration serves both.
func tableModule(t *testing.T) (map[string]bool, string) {
	t.Helper()
	project := t.TempDir()
	write := func(files []gen.File) {
		for _, f := range files {
			target := filepath.Join(project, f.Path)
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, f.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	access := func(name string, wiring *gen.Wiring) {
		data, err := json.Marshal(wiring.DatabaseAccess)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(project, "check"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(project, "check", name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	roles := map[string]any{"runtime_role": "tables_runtime_" + suffix, "owner_role": "tables_owner_" + suffix}
	for _, service := range []struct {
		ctx    gen.Context
		access string
	}{{orderService("storage"), "access.json"}, {orderService("previous"), ""}, {widgetService(widgetEntities()), "widgets_access.json"}} {
		files, wiring, err := new(postgresadapter.Generator).Generate(service.ctx)
		if err != nil {
			t.Fatal(err)
		}
		write(files)
		if service.access != "" {
			access(service.access, wiring)
			own := service.ctx
			own.OutputNamespace = own.PeerNamespaces[gen.TenancyComponent]
			own.ComponentConfig = roles
			security, _, err := new(tenancy.Generator).Generate(own)
			if err != nil {
				t.Fatal(err)
			}
			write(security)
		}
	}
	// The runtime data path takes the connection configuration of the runtime library's connectors from the adapter
	// (runtime_access.go), so the module requires the runtime library of this repository's registry/runtime folder.
	runtimeDir, err := filepath.Abs(filepath.Join("..", "..", "..", "registry", "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	goMod := "module " + tablesModule + "\n\ngo 1.26.11\n\nrequire (\n github.com/google/uuid v1.6.0\n github.com/jackc/pgx/v5 v5.11.0\n gorm.io/gorm v1.25.12\n gorm.io/driver/postgres v1.5.11\n github.com/jsell-rh/stego/registry/runtime v0.27.0\n)\n\nreplace github.com/jsell-rh/stego/registry/runtime => " + runtimeDir + "\n"
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "check", "tables_test.go"), tablesTests, 0o644); err != nil {
		t.Fatal(err)
	}
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir, tidy.Env = project, append(os.Environ(), "GOWORK=off")
	if output, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, output)
	}
	run := exec.Command("go", "test", "-json", "-mod=readonly", "-count=1", "-timeout=120s", "./check")
	run.Dir, run.Env = project, append(os.Environ(), "GOWORK=off")
	output, _ := run.CombinedOutput()
	passed := map[string]bool{}
	var log strings.Builder
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 1<<20), 1<<24)
	for scanner.Scan() {
		var event tableEvent
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			log.WriteString(scanner.Text() + "\n")
			continue
		}
		log.WriteString(event.Output)
		if event.Action == "pass" && event.Test != "" {
			passed[event.Test] = true
		}
	}
	return passed, log.String()
}

// passedThere fails the test when the generated subtest of id did not pass.
func passedThere(t *testing.T, passed map[string]bool, log, id string) {
	t.Helper()
	for name := range passed {
		if strings.HasPrefix(name, "TestTables/"+id+"_") {
			return
		}
	}
	t.Errorf("the generated test of %s did not pass:\n%s", id, log)
}

// golden compares got with the golden file of name; STEGO_UPDATE_GOLDEN=1 writes it again.
func golden(t *testing.T, name, got string) {
	t.Helper()
	file := filepath.Join("testdata", "golden", name)
	if os.Getenv("STEGO_UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s differs from its golden file:\n%s", name, got)
	}
}

func TestTables(t *testing.T) {
	var passed map[string]bool
	var log string
	database := os.Getenv("STEGO_TEST_POSTGRES_DSN") != ""
	if database {
		passed, log = tableModule(t)
	}
	onPostgres := func(t *testing.T, id string) {
		t.Helper()
		if !database {
			if os.Getenv("STEGO_REQUIRE_POSTGRES") == "1" {
				t.Fatal("PostgreSQL fixture is required")
			}
			t.Skip("PostgreSQL fixture is not configured")
		}
		passedThere(t, passed, log, id)
	}

	t.Run("ST-SUP-RC01-OC41-T01 The tables are SQL from the declaration, with no store", func(t *testing.T) {
		current := orderService("storage")
		files, _, err := new(postgresadapter.Generator).Generate(current)
		if err != nil {
			t.Fatal(err)
		}
		noStore(t, files, current.Entities)
		if migrate := generated(t, files, "storage/migrate.go"); !strings.Contains(migrate, `RegisterSQL("001_initial", InitialTables)`) {
			t.Errorf("001_initial is not the SQL migration of the tables:\n%s", migrate)
		}
		// The release before this one is generated from the same declaration, as the base ref holds it: its
		// 001_initial must be the same body, so its ledger row is the prefix of this release's ledger.
		before, _, err := new(postgresadapter.Generator).Generate(orderService("previous"))
		if err != nil {
			t.Fatal(err)
		}
		if generated(t, before, "previous/migrations/001_initial.sql") != generated(t, files, "storage/migrations/001_initial.sql") {
			t.Error("the same declaration gives another body of 001_initial")
		}
		onPostgres(t, "ST-SUP-RC01-OC41-T01")
	})

	t.Run("ST-SUP-RC01-OC41-T02 Each field type and constraint is a column that PostgreSQL holds", func(t *testing.T) {
		entities := widgetEntities()
		files, _, err := new(postgresadapter.Generator).Generate(widgetService(entities))
		if err != nil {
			t.Fatal(err)
		}
		noStore(t, files, entities)
		body := generated(t, files, "widgets/migrations/001_initial.sql")
		golden(t, "001_initial.sql", body)
		if constant := generated(t, files, "widgets/tables.go"); !strings.Contains(constant, "const InitialTables = "+strconv.Quote(body)) {
			t.Errorf("InitialTables is not the text of 001_initial.sql:\n%s", constant)
		}
		reversed := make([]types.Entity, len(entities))
		for i, entity := range entities {
			reversed[len(entities)-1-i] = entity
		}
		again, _, err := new(postgresadapter.Generator).Generate(widgetService(reversed))
		if err != nil {
			t.Fatal(err)
		}
		if generated(t, again, "widgets/migrations/001_initial.sql") != body {
			t.Error("the order of the entities in the declaration changes the body of 001_initial")
		}
		onPostgres(t, "ST-SUP-RC01-OC41-T02")
	})

	t.Run("ST-SUP-RC01-OC41-T03 A service that needs the store is refused on the runtime data path", func(t *testing.T) {
		refused := map[string]func(*gen.Context){
			"schema_generation": func(ctx *gen.Context) {
				delete(ctx.ComponentConfig, "schema_generation")
				delete(ctx.ComponentConfig, "writer_lease")
			},
			"writer_lease off": func(ctx *gen.Context) { ctx.ComponentConfig["writer_lease"] = "single" },
			"storage contract": func(ctx *gen.Context) { ctx.StorageContract = tablesModule + "/out/internal/contracts/storage" },
			"tsl-search":       func(ctx *gen.Context) { ctx.PeerNamespaces["tsl-search"] = "search" },
			"generation_fields": func(ctx *gen.Context) {
				ctx.Entities[0].Versioned, ctx.Entities[0].GenerationFields = true, []string{"sku"}
			},
			"conditions": func(ctx *gen.Context) {
				ctx.Entities[0].Versioned, ctx.Entities[0].Conditions = true, map[string][]string{"Ready": {"sku"}}
			},
			"cleanup_owners": func(ctx *gen.Context) {
				ctx.Entities[0].Versioned, ctx.Entities[0].CleanupOwners = true, []string{"controller"}
			},
			"63 bytes": func(ctx *gen.Context) {
				ctx.Entities[0].Fields = append(ctx.Entities[0].Fields, types.Field{Name: strings.Repeat("x", 64), Type: types.FieldTypeString})
			},
		}
		for want, change := range refused {
			ctx := orderService("storage")
			change(&ctx)
			files, _, err := new(postgresadapter.Generator).Generate(ctx)
			if err == nil || !strings.Contains(err.Error(), want) || files != nil {
				t.Errorf("%s: files %d, error %v; want a refusal that names %s", want, len(files), err, want)
			}
		}
		store := orderService("storage")
		store.RuntimeData = false
		files, _, err := new(postgresadapter.Generator).Generate(store)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(generated(t, files, "storage/migrate.go"), "AutoMigrate") || !strings.Contains(generated(t, files, "storage/store.go"), "type Store struct") {
			t.Error("without the runtime data path the adapter does not emit the GORM store")
		}
	})
}
