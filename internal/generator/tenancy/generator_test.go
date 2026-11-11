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
	peers := map[string]string{"postgres-adapter": "storage", gen.TenancyComponent: "tenancy"}
	tenancy := &gen.Tenancy{Key: "tenant_id"}
	storage := gen.Context{OutputNamespace: "storage", ModuleName: module, ServiceName: "tenancy-check", Entities: declaration(), PeerNamespaces: peers, Tenancy: tenancy, ComponentConfig: map[string]any{}}
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
		if _, _, err := new(Generator).Generate(same); err == nil || !strings.Contains(err.Error(), "must differ") {
			t.Errorf("one name for both roles is not refused: %v", err)
		}
		// The owner role may not take the name of a role that the migration names from the runtime role, such as the
		// outbox relay role of the service.
		for _, owner := range []string{"svc_outbox_relay", "svc_idempotency_purge", "svc_worker", "svc_relay"} {
			taken := ctx
			taken.ComponentConfig = map[string]any{"runtime_role": "svc_runtime", "owner_role": owner}
			if _, _, err := new(Generator).Generate(taken); err == nil || !strings.Contains(err.Error(), "two roles of the service") {
				t.Errorf("the owner role %s, a role of the service, is not refused: %v", owner, err)
			}
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
	tenancyCtx.ComponentConfig = map[string]any{"runtime_role": "check_runtime_" + suffix, "owner_role": "check_owner_" + suffix}
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
	goMod := "module " + module + "\n\ngo 1.25.0\n\nrequire (\n github.com/google/uuid v1.6.0\n github.com/jackc/pgx/v5 v5.11.0\n gorm.io/gorm v1.25.12\n gorm.io/driver/postgres v1.5.11\n" +
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
	passed := map[string]bool{}
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
		if event.Action == "pass" && event.Test != "" {
			passed[event.Test] = true
		}
	}
	for _, id := range []string{"ST-SUP-RC27-OC03-T01", "ST-SUP-RC27-OC03-T02", "ST-SUP-RC27-OC04-T01", "ST-SUP-RC27-OC04-T03", "ST-SUP-RC27-OC04-T05", "ST-SUP-RC01-OC40-T01", "ST-SUP-RC01-OC40-T02", "ST-SUP-RC27-OC07-T01", "ST-SUP-RC27-OC07-T02", "ST-SUP-RC27-OC04-T06", "ST-SUP-RC27-OC04-T07", "ST-SUP-RC27-OC04-T08"} {
		t.Run(id+" passes in the generated service", func(t *testing.T) {
			for name := range passed {
				if strings.HasPrefix(name, "TestTenancy/"+id+"_") || strings.HasPrefix(name, "TestTwoServices/"+id+"_") || strings.HasPrefix(name, "TestOpenSessions/"+id+"_") ||
					strings.HasPrefix(name, "TestRelayRoles/"+id+"_") {
					return
				}
			}
			t.Errorf("the generated test of %s did not pass:\n%s", id, log.String())
		})
	}
}
