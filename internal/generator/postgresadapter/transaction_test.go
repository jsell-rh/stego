package postgresadapter

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	queuegen "github.com/jsell-rh/stego/internal/generator/outbox"
	"github.com/jsell-rh/stego/internal/types"
)

//go:embed testdata/transaction_test.go
var transactionTests []byte

func TestGeneratedStoreTransactions(t *testing.T) {
	dsn, required := os.Getenv("STEGO_TEST_POSTGRES_DSN"), os.Getenv("STEGO_REQUIRE_POSTGRES")
	if required == "1" && dsn == "" {
		t.Fatal("PostgreSQL integration tests require STEGO_TEST_POSTGRES_DSN")
	}
	ctx := gen.Context{ModuleName: "example.com/transaction-test", OutputNamespace: "storage", StorageContract: "example.com/transaction-test/contracts/storage", PeerNamespaces: map[string]string{"outbox": "queue"}, Entities: []types.Entity{{Name: "Record", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString, Unique: true}, {Name: "value", Type: types.FieldTypeInt64}}}}}
	files, _, err := new(Generator).Generate(ctx)
	if err != nil {
		t.Fatal(err)
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

go 1.26.8

require (
 github.com/google/uuid v1.6.0
 github.com/jackc/pgx/v5 v5.11.0
 gorm.io/gorm v1.25.12
 gorm.io/driver/postgres v1.5.11
)
`
	for name, data := range map[string][]byte{"go.mod": []byte(module), "storage/transaction_test.go": transactionTests} {
		if err := os.WriteFile(filepath.Join(project, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	commands := [][]string{{"mod", "tidy"}, {"test", "-race", "-mod=readonly", "-timeout=60s", "-v", "./..."}}
	if os.Getenv("STEGO_BENCH_STORE") == "1" {
		commands = append(commands, []string{"test", "-mod=readonly", "-run=^$", "-bench=BenchmarkTransactionalCreateNotify", "-benchtime=100x", "-benchmem", "./storage"})
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
