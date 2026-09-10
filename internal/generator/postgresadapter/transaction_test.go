package postgresadapter

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	queuegen "github.com/jsell-rh/stego/internal/generator/outbox"
	"github.com/jsell-rh/stego/internal/types"
)

//go:embed testdata/transaction_test.go
var transactionTests []byte

//go:embed testdata/versions_test.go
var versionTests []byte

//go:embed testdata/cleanup_test.go
var cleanupTests []byte

//go:embed testdata/conditions_test.go
var conditionTests []byte

//go:embed testdata/checkpoints_test.go
var checkpointTests []byte

//go:embed testdata/cursor_test.go
var cursorTests []byte

//go:embed testdata/cleanup_targets_test.go
var cleanupTargetTests []byte

//go:embed testdata/cleanup_summary_test.go
var cleanupSummaryTests []byte

func TestGeneratedStoreTransactions(t *testing.T) {
	dsn, required := os.Getenv("STEGO_TEST_POSTGRES_DSN"), os.Getenv("STEGO_REQUIRE_POSTGRES")
	if required == "1" && dsn == "" {
		t.Fatal("PostgreSQL integration tests require STEGO_TEST_POSTGRES_DSN")
	}
	pending := "Pending \\ ' ?"
	ctx := gen.Context{ModuleName: "example.com/transaction-test", OutputNamespace: "storage", StorageContract: "example.com/transaction-test/contracts/storage", PeerNamespaces: map[string]string{"outbox": "queue"}, Entities: []types.Entity{{Name: "Record", Versioned: true, Conditions: map[string][]string{"identity": {"Ready"}, "health": {"Ready"}}, CleanupOwners: []string{"workload", "identity"}, GenerationFields: []string{"name", "desired_config"}, Observations: map[string][]string{"health": {"health"}, "identity": {"identity"}, "evidence": {"certificate", "checked_at"}}, Fields: []types.Field{{Name: "name", Type: types.FieldTypeString, Unique: true}, {Name: "value", Type: types.FieldTypeInt64}, {Name: "health", Type: types.FieldTypeString, Optional: true, Unobserved: &pending}, {Name: "identity", Type: types.FieldTypeString, Optional: true}, {Name: "certificate", Type: types.FieldTypeBytes, Optional: true}, {Name: "checked_at", Type: types.FieldTypeTimestamp, Optional: true}, {Name: "desired_config", Type: types.FieldTypeJsonb, Optional: true}}}}}
	low, high, zero, hundred, ratioLow, ratioHigh, singleLow, singleHigh := -5.0, 9.0, 0.0, 100.0, 0.1, 0.9, -1.5, 1.5
	ctx.Entities = append(ctx.Entities, types.Entity{Name: "Measurement", Fields: []types.Field{
		{Name: "score", Type: types.FieldTypeInt32, Min: &zero, Max: &hundred},
		{Name: "limit", Type: types.FieldTypeInt64, Optional: true, Min: &zero},
		{Name: "lower", Type: types.FieldTypeInt64, Min: &low},
		{Name: "upper", Type: types.FieldTypeInt64, Max: &high},
		{Name: "ratio", Type: types.FieldTypeDouble, Optional: true, Min: &ratioLow, Max: &ratioHigh},
		{Name: "single", Type: types.FieldTypeFloat, Optional: true, Min: &singleLow, Max: &singleHigh},
		{Name: "floor", Type: types.FieldTypeDouble, Optional: true, Min: &zero},
		{Name: "ceiling", Type: types.FieldTypeDouble, Optional: true, Max: &hundred},
	}})
	ctx.Entities = append(ctx.Entities, types.Entity{Name: "Placement", Versioned: true, CleanupOwners: []string{"worker", "identity"}, CleanupTargets: map[string]string{"worker": "target"}, Fields: []types.Field{{Name: "target", Type: types.FieldTypeString}, {Name: "name", Type: types.FieldTypeString}}})
	files, _, err := new(Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for name, mapping := range map[string]map[string]string{"target_removed.sql": nil, "target_changed.sql": {"worker": "name"}, "target_same_mapping.sql": {"worker": "target"}} {
		next := ctx
		next.Entities = append([]types.Entity(nil), ctx.Entities...)
		last := len(next.Entities) - 1
		next.Entities[last].CleanupTargets = mapping
		if name == "target_same_mapping.sql" {
			next.Entities[last].Fields = append(append([]types.Field(nil), next.Entities[last].Fields...), types.Field{Name: "new_input", Type: types.FieldTypeString, Optional: true})
		}
		upgrades, err := generateVersions(next)
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range upgrades {
			if strings.HasSuffix(file.Path, "/000002_resource_versions.sql") {
				statement := strings.TrimSuffix(strings.TrimPrefix(string(file.Bytes()), "BEGIN;\n"), "COMMIT;\n")
				files = append(files, gen.File{Path: "storage/" + name, Content: []byte(statement)})
			}
		}
	}
	for name, owners := range map[string][]string{"cleanup_added.sql": {"workload", "identity", "archive"}, "cleanup_removed.sql": nil} {
		next := ctx
		next.Entities = append([]types.Entity(nil), ctx.Entities...)
		next.Entities[0].CleanupOwners = owners
		upgrades, err := generateVersions(next)
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range upgrades {
			if strings.HasSuffix(file.Path, "/000002_resource_versions.sql") {
				statement := strings.TrimSuffix(strings.TrimPrefix(string(file.Bytes()), "BEGIN;\n"), "COMMIT;\n")
				files = append(files, gen.File{Path: "storage/" + name, Content: []byte(statement)})
			}
		}
	}
	for name, conditions := range map[string]map[string][]string{"no_conditions.sql": nil, "removed_conditions.sql": {"health": {"Ready"}}} {
		next := ctx
		next.Entities = append([]types.Entity(nil), ctx.Entities...)
		next.Entities[0].Conditions = conditions
		versions, err := generateVersions(next)
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range versions {
			if strings.HasSuffix(file.Path, "/000002_resource_versions.sql") {
				statement := strings.TrimSuffix(strings.TrimPrefix(string(file.Bytes()), "BEGIN;\n"), "COMMIT;\n")
				files = append(files, gen.File{Path: "storage/" + name, Content: []byte(statement)})
			}
		}
	}
	queueFiles, _, err := new(queuegen.Generator).Generate(gen.Context{OutputNamespace: "queue", StorageContract: ctx.StorageContract})
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, queueFiles...)
	contract, err := gen.ResolveContract(gen.StorageV1)
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, contract.Files...)
	// Compile the transaction scope without the optional queue as well.
	ctx.OutputNamespace, ctx.PeerNamespaces = "plainstore", nil
	plain, _, err := new(Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, plain...)
	// Compile the older REST peer path without a public storage contract.
	ctx.OutputNamespace, ctx.StorageContract, ctx.PeerNamespaces = "legacystore", "", map[string]string{"rest-api": "sort"}
	legacy, _, err := new(Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, legacy...)
	files = append(files, gen.File{Path: "sort/storage.go", Content: contract.Files[0].Content})
	project := t.TempDir()
	for _, file := range files {
		name := filepath.Join(project, file.Path)
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, file.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
		if file.Path == "queue/migrations/000001_outbox.sql" {
			if err := os.WriteFile(filepath.Join(project, "storage/outbox.sql"), file.Bytes(), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	module := `module example.com/transaction-test

go ` + new(Generator).MinimumGoVersion() + `

require (
 github.com/google/uuid v1.6.0
 github.com/jackc/pgx/v5 v5.11.0
 gorm.io/datatypes v1.2.5
 gorm.io/gorm v1.25.12
 gorm.io/driver/postgres v1.5.11
)
`
	for name, data := range map[string][]byte{"go.mod": []byte(module), "storage/transaction_test.go": transactionTests, "storage/versions_test.go": versionTests, "storage/cleanup_test.go": cleanupTests, "storage/cleanup_summary_test.go": cleanupSummaryTests, "storage/cleanup_targets_test.go": cleanupTargetTests, "storage/cursor_test.go": cursorTests, "storage/checkpoints_test.go": checkpointTests, "storage/conditions_test.go": conditionTests} {
		if err := os.WriteFile(filepath.Join(project, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	commands := [][]string{{"mod", "tidy", "-go=" + new(Generator).MinimumGoVersion()}, {"vet", "-mod=readonly", "./..."}, {"test", "-race", "-mod=readonly", "-timeout=60s", "-v", "./..."}}
	if os.Getenv("STEGO_BENCH_STORE") == "1" {
		commands = append(commands, []string{"test", "-mod=readonly", "-run=^$", "-bench=Benchmark(TransactionalCreateNotify|TargetCleanupObservation)", "-benchtime=100x", "-benchmem", "./storage"})
	}
	if os.Getenv("STEGO_BENCH_CURSOR") == "1" {
		commands = append(commands, []string{"test", "-mod=readonly", "-run=^$", "-bench=^BenchmarkRecoveryCursorPage$", "-count=3", "-benchtime=100x", "-benchmem", "./storage"})
	}
	if os.Getenv("STEGO_BENCH_CLEANUP_SUMMARY") == "1" {
		commands = append(commands, []string{"test", "-mod=readonly", "-run=^$", "-bench=^BenchmarkCleanupSummary$", "-count=3", "-benchtime=100x", "-benchmem", "./storage"})
	}
	for _, args := range commands {
		cmd := exec.Command("go", args...)
		cmd.Dir = project
		cmd.Env = append(os.Environ(), "GOWORK=off", "STEGO_TEST_POSTGRES_DSN="+dsn, "STEGO_REQUIRE_POSTGRES="+required)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("generated transactions, go %v: %v\n%s", args, err, output)
		}
		t.Log(string(output))
	}
}

func TestTransactionReservedNames(t *testing.T) {
	for _, name := range []string{"ErrTransactionRequired", "ErrTransactionNested", "ErrTransactionClosed", "ErrNotificationLimit", "transactionState", "transactionTimeout", "sqlTransaction", "stegooutbox", "sync"} {
		ctx := gen.Context{OutputNamespace: "storage", Entities: []types.Entity{{Name: name}}}
		if _, _, err := new(Generator).Generate(ctx); err == nil {
			t.Fatalf("accepted reserved transaction name %q", name)
		}
	}
}

func TestTransactionPeerConfiguration(t *testing.T) {
	for _, ctx := range []gen.Context{
		{OutputNamespace: "storage", PeerNamespaces: map[string]string{"outbox": "queue"}},
		{OutputNamespace: "storage", ModuleName: "example.com/test", PeerNamespaces: map[string]string{"outbox": "../queue"}},
	} {
		if _, err := generateTransaction(ctx); err == nil {
			t.Fatal("accepted invalid transaction peer configuration")
		}
	}
}
