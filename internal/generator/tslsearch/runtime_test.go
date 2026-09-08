package tslsearch

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGeneratedSearchRuntime(t *testing.T) {
	files, _, err := new(Generator).Generate(testContext())
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	for _, file := range files {
		if err := os.WriteFile(filepath.Join(project, filepath.Base(file.Path)), file.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	module := "module example.com/search-test\n\ngo 1.26.8\n\nrequire github.com/yaacov/tree-search-language/v5 v5.2.12\n"
	if err := os.WriteFile(filepath.Join(project, "go.mod"), []byte(module), 0644); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile("testdata/search_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "search_test.go"), source, 0644); err != nil {
		t.Fatal(err)
	}
	commands := [][]string{{"mod", "tidy"}, {"test", "-race", "-count=1", "-mod=readonly", "-timeout=30s", "./..."}}
	if os.Getenv("STEGO_BENCH_SEARCH") == "1" {
		commands = append(commands, []string{"test", "-run", "^$", "-bench", "BenchmarkSearch", "-benchtime=1000x", "-count=1", "-mod=readonly"})
	}
	for _, args := range commands {
		command := exec.Command("go", args...)
		command.Dir = project
		command.Env = append(os.Environ(), "GOWORK=off")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("generated search: %v\n%s", err, output)
		}
		if os.Getenv("STEGO_BENCH_SEARCH") == "1" {
			t.Log(string(output))
		}
	}
}
