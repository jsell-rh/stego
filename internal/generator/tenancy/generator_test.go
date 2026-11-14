package tenancy

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
	"github.com/jsell-rh/stego/internal/types"
)

//go:embed testdata/tenancy_test.go
var tenancyTests []byte

const module = "example.com/tenancy-check"

// declaration is the service of the tenancy test records: the owned entities Widget and Order, each with one
// secondary index, and Order with a reference to Widget; the shared entity Catalog and the system entity Mark.
func declaration() []types.Entity {
	return []types.Entity{
		{Name: "Widget", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString, Unique: true}}},
		{Name: "Order", Fields: []types.Field{{Name: "number", Type: types.FieldTypeString, Unique: true}, {Name: "widget_id", Type: types.FieldTypeRef, To: "Widget"}}},
		{Name: "Catalog", Tenant: "shared", Fields: []types.Field{{Name: "code", Type: types.FieldTypeString}}},
		{Name: "Mark", Tenant: "system", Fields: []types.Field{{Name: "note", Type: types.FieldTypeString}}},
	}
}

func contexts() (gen.Context, gen.Context) {
	peers := map[string]string{"postgres-adapter": "storage", gen.TenancyComponent: "tenancy", "command": "commands", "query": "queries"}
	tenancy := &gen.Tenancy{Key: "tenant_id"}
	storage := gen.Context{OutputNamespace: "storage", ModuleName: module, ServiceName: "tenancy-check", Entities: declaration(), PeerNamespaces: peers, Tenancy: tenancy, ComponentConfig: map[string]any{"schema_generation": "tenancy-check-v1"}}
	own := storage
	own.OutputNamespace = "tenancy"
	return storage, own
}

func file(t *testing.T, files []gen.File, name string) string {
	t.Helper()
	for _, f := range files {
		if f.Path == name {
			return string(f.Content)
		}
	}
	t.Fatalf("no file %s", name)
	return ""
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("testdata", "golden", name))
	if os.Getenv("STEGO_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(filepath.Join("testdata", "golden", name), []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s differs from its golden file:\n%s", name, got)
	}
}

func TestGenerator(t *testing.T) {
	t.Run("ST-SUP-RC27-OC04-T09 A role prefix whose role names can be another service's role names is refused", func(t *testing.T) {
		_, ctx := contexts()
		// The service name shop-outbox gives the prefix shop_outbox, whose relay role shop_outbox_relay is the outbox
		// relay role of the prefix shop; a runtime_role setting gives the same prefix.
		named := ctx
		named.ServiceName = "shop-outbox"
		set := ctx
		set.ComponentConfig = map[string]any{"runtime_role": "shop_outbox_runtime", "owner_role": "shop_outbox_owner"}
		for name, c := range map[string]gen.Context{"service name": named, "runtime_role": set} {
			if err := new(Generator).ValidateContext(c); err == nil || !strings.Contains(err.Error(), "shop_outbox_relay") || !strings.Contains(err.Error(), `"shop"`) {
				t.Errorf("the prefix shop_outbox from the %s is not refused with the clash it names: %v", name, err)
			}
			if _, _, err := new(Generator).Generate(c); err == nil {
				t.Errorf("Generate takes the prefix shop_outbox from the %s", name)
			}
		}
		// A prefix that only holds outbox, not at its end, is taken, and so are the prefixes of the other services.
		for _, prefix := range []string{"outbox_shop", "shop_outboxes", "shop", "outbox"} {
			c := ctx
			c.ComponentConfig = map[string]any{"runtime_role": prefix + "_runtime", "owner_role": prefix + "_owner"}
			if err := new(Generator).ValidateContext(c); err != nil {
				t.Errorf("the prefix %s is refused: %v", prefix, err)
			}
		}
		// A set runtime_role or owner_role is <prefix>_runtime and <prefix>_owner of one prefix, so it cannot name a
		// role of another service, such as the roles of the prefix shop.
		for _, set := range []map[string]any{
			{"runtime_role": "shop_outbox_relay"}, {"runtime_role": "shop_worker"}, {"runtime_role": "shop_owner"},
			{"owner_role": "shop_outbox_relay"}, {"owner_role": "shop_owner"},
			{"runtime_role": "x_runtime", "owner_role": "shop_owner"}, {"runtime_role": "shop_runtime", "owner_role": "app_owner"},
			{"runtime_role": "_runtime", "owner_role": "_owner"},
		} {
			c := ctx
			c.ComponentConfig = set
			if err := new(Generator).ValidateContext(c); err == nil || !strings.Contains(err.Error(), "of one prefix") {
				t.Errorf("the settings %v are not refused: %v", set, err)
			}
		}
	})

	t.Run("ST-SUP-RC27-OC03-T04 The row-level security migration of the declaration is its golden text", func(t *testing.T) {
		_, ctx := contexts()
		files, _, err := new(Generator).Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		security := file(t, files, "tenancy/migrations/row_security.sql")
		golden(t, "row_security.sql", security)
		for _, table := range []string{"catalogs", "marks"} {
			if strings.Contains(security, `"`+table+`"`) {
				t.Errorf("the row-level security covers the table %s, which is not owned", table)
			}
		}
		if !strings.Contains(file(t, files, "tenancy/row_security.go"), "const RowSecurity = ") {
			t.Error("the package does not hold RowSecurity")
		}
		single := ctx
		single.Tenancy = nil
		if _, _, err := new(Generator).Generate(single); err == nil || !strings.Contains(err.Error(), "pooled tenancy") {
			t.Errorf("a service without pooled tenancy is not refused: %v", err)
		}
	})

	t.Run("ST-SUP-RC27-OC04-T04 The role migration is its golden text, and one name for both roles is refused", func(t *testing.T) {
		_, ctx := contexts()
		files, _, err := new(Generator).Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		roles := file(t, files, "tenancy/migrations/roles.sql")
		golden(t, "roles.sql", roles)
		for _, banned := range []string{" BYPASSRLS", " SUPERUSER", "OWNER TO"} {
			if strings.Contains(roles, banned) {
				t.Errorf("the role migration holds %q", banned)
			}
		}
		same := ctx
		same.ComponentConfig = map[string]any{"runtime_role": "svc", "owner_role": "svc"}
		if _, _, err := new(Generator).Generate(same); err == nil || !strings.Contains(err.Error(), "of one prefix") {
			t.Errorf("one name for both roles is not refused: %v", err)
		}
	})
}

// testEvent is one line of go test -json.
type testEvent struct {
	Action string
	Test   string
	Output string
}

// TestMigrations generates the service, applies its migrations to PostgreSQL in the generated module's own test,
// and passes each test record that the generated test proves only when its subtest passed there.
func TestMigrations(t *testing.T) {
	if os.Getenv("STEGO_TEST_POSTGRES_DSN") == "" {
		if os.Getenv("STEGO_REQUIRE_POSTGRES") == "1" {
			t.Fatal("PostgreSQL fixture is required")
		}
		t.Skip("PostgreSQL fixture is not configured")
	}
	project := t.TempDir()
	storageCtx, tenancyCtx := contexts()
	// Role names of their own, so a role that an earlier run left on the server cannot stand in for one that the
	// role migration fails to create.
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	tenancyCtx.ComponentConfig = map[string]any{"runtime_role": "check_" + suffix + "_runtime", "owner_role": "check_" + suffix + "_owner"}
	adapterFiles, adapterWiring, err := new(postgresadapter.Generator).Generate(storageCtx)
	if err != nil {
		t.Fatal(err)
	}
	tenancyFiles, _, err := new(Generator).Generate(tenancyCtx)
	if err != nil {
		t.Fatal(err)
	}
	// A second service in the same cluster, with roles of its own (ST-SUP-RC27-OC04-T06).
	otherCtx := tenancyCtx
	otherCtx.OutputNamespace = "tenancyb"
	otherCtx.ComponentConfig = map[string]any{"runtime_role": "checkb_" + suffix + "_runtime", "owner_role": "checkb_" + suffix + "_owner"}
	otherFiles, _, err := new(Generator).Generate(otherCtx)
	if err != nil {
		t.Fatal(err)
	}
	tenancyFiles = append(tenancyFiles, otherFiles...)
	for _, f := range append(adapterFiles, tenancyFiles...) {
		target := filepath.Join(project, f.Path)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, f.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The check runs the outbox and authorization migrations of the runtime library of this repository for the roles
	// of each service (ST-SUP-RC27-OC04-T08), so the module requires it from the registry/runtime folder.
	runtimeDir, err := filepath.Abs(filepath.Join("..", "..", "..", "registry", "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	sums, err := os.ReadFile(filepath.Join(runtimeDir, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "go.sum"), sums, 0o644); err != nil {
		t.Fatal(err)
	}
	goMod := "module " + module + "\n\ngo 1.25.0\n\nrequire (\n github.com/google/uuid v1.6.0\n github.com/jackc/pgx/v5 v5.11.0\n" +
		" github.com/jsell-rh/stego/registry/runtime v0.2.0\n)\n\nreplace github.com/jsell-rh/stego/registry/runtime => " + runtimeDir + "\n"
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, "check"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "check", "tenancy_test.go"), tenancyTests, 0o644); err != nil {
		t.Fatal(err)
	}
	// The access that the adapter declares, which the database access contract grants the runtime role.
	access, err := json.Marshal(adapterWiring.DatabaseAccess)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "check", "access.json"), access, 0o644); err != nil {
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
	passed, ran := map[string]bool{}, map[string]bool{}
	var log strings.Builder
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 1<<20), 1<<24)
	for scanner.Scan() {
		var event testEvent
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			log.WriteString(scanner.Text() + "\n")
			continue
		}
		log.WriteString(event.Output)
		if event.Test != "" {
			ran[event.Test] = true
		}
		if event.Action == "pass" && event.Test != "" {
			passed[event.Test] = true
		}
	}
	for _, id := range []string{"ST-SUP-RC27-OC03-T01", "ST-SUP-RC27-OC03-T02", "ST-SUP-RC27-OC04-T01", "ST-SUP-RC27-OC04-T03", "ST-SUP-RC27-OC04-T05", "ST-SUP-RC01-OC40-T01", "ST-SUP-RC01-OC40-T02", "ST-SUP-RC27-OC07-T01", "ST-SUP-RC27-OC07-T02", "ST-SUP-RC27-OC04-T06", "ST-SUP-RC27-OC04-T07", "ST-SUP-RC27-OC04-T08", "ST-SUP-RC27-OC04-T10", "ST-SUP-RC27-OC04-T11", "ST-SUP-RC27-OC04-T12"} {
		t.Run(id+" passes in the generated service", func(t *testing.T) {
			if own := ownSubtest(ran, func(name string) bool {
				return strings.HasPrefix(name, "TestTenancy/"+id+"_") || strings.HasPrefix(name, "TestTwoServices/"+id+"_") || strings.HasPrefix(name, "TestOpenSessions/"+id+"_") ||
					strings.HasPrefix(name, "TestRelayRoles/"+id+"_") || strings.HasPrefix(name, "TestForeignRoles/"+id+"_") ||
					// T11 runs inside T10, on the roles that T10 leaves.
					(strings.HasPrefix(name, "TestForeignRoles/") && strings.Contains(name, "/"+id+"_"))
			}); own != "" && passed[own] {
				return
			}
			t.Errorf("the generated test of %s did not pass:\n%s", id, log.String())
		})
	}
}

// ownSubtest gives the name of the subtest of id among ran, the names of the tests that ran, as match finds it: the
// shortest name that matches, because each subtest under it has its name and more. So a pass of a subtest under it
// is never taken for its own pass (ST-STD-CODE-R07). It gives "" when no name matches.
func ownSubtest(ran map[string]bool, match func(string) bool) string {
	own := ""
	for name := range ran {
		if match(name) && (own == "" || len(name) < len(own)) {
			own = name
		}
	}
	return own
}

func TestOwnSubtest(t *testing.T) {
	t.Run("ST-STD-CODE-R07-T06 In the tenancy wrapper, a pass of a subtest under the subtest of a record is not its pass", func(t *testing.T) {
		ran := map[string]bool{"TestTenancy/ST-X-T01_a": true, "TestTenancy/ST-X-T01_a/child": true, "TestTenancy/ST-X-T02_b/c": true, "TestTenancy/ST-X-T02_b/c/d": true}
		passed := map[string]bool{"TestTenancy/ST-X-T01_a/child": true, "TestTenancy/ST-X-T02_b/c": true}
		match := func(id string) func(string) bool {
			return func(name string) bool { return strings.HasPrefix(name, "TestTenancy/"+id+"_") }
		}
		if own := ownSubtest(ran, match("ST-X-T01")); own != "TestTenancy/ST-X-T01_a" || passed[own] {
			t.Fatalf("the own subtest of T01 is %q, want the failed parent and not its child", own)
		}
		if own := ownSubtest(ran, match("ST-X-T02")); own != "TestTenancy/ST-X-T02_b/c" || !passed[own] {
			t.Fatalf("the own subtest of T02, whose title holds a slash, is %q, want the passed one", own)
		}
		if own := ownSubtest(ran, match("ST-X-T09")); own != "" {
			t.Fatalf("a record with no subtest gave %q", own)
		}
	})
}
