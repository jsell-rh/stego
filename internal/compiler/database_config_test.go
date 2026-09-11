package compiler

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

//go:embed testdata/database_config_test.go
var databaseConfigTests []byte

func TestGeneratedDatabaseConfiguration(t *testing.T) {
	project := t.TempDir()
	files := map[string][]byte{
		"go.mod":         []byte("module example.com/databaseconfig\ngo 1.26.8\n"),
		"config.go":      []byte("package main\nimport(\"os\";\"errors\";\"path/filepath\";\"syscall\";\"io\";\"strings\")\n" + databaseConfigSource),
		"config_test.go": databaseConfigTests,
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(project, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command("go", "test", "-race", "-count=1", "-timeout=30s", "./...")
	command.Dir = project
	command.Env = append(os.Environ(), "GOWORK=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("database configuration: %v %s", err, output)
	}
}
