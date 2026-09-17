package compiler

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jsell-rh/stego/internal/parser"
	"github.com/jsell-rh/stego/internal/types"
	"gopkg.in/yaml.v3"
)

func initTestFile(t *testing.T, project, name, content string) {
	t.Helper()
	path := filepath.Join(project, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func initTestRegistry(t *testing.T, project, directory, archetype string) {
	t.Helper()
	initTestFile(t, project, directory+"/archetypes/"+archetype+"/archetype.yaml", "kind: archetype\nname: "+archetype+"\nlanguage: go\nversion: 1.0.0\ncomponents: []\n")
}

func assertInitService(t *testing.T, project, archetype string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(project, "service.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var declaration types.ServiceDeclaration
	if err := parser.DecodeStrict(data, "service.yaml", &declaration); err != nil {
		t.Fatal(err)
	}
	if declaration.Name != filepath.Base(project) || declaration.Kind != "service" || declaration.Archetype != archetype || declaration.Language != "go" {
		t.Fatalf("unexpected declaration: %+v", declaration)
	}
	if info, err := os.Stat(filepath.Join(project, "fills")); err != nil || !info.IsDir() {
		t.Fatalf("fills is not a directory: %v", err)
	}
}

func TestInitializeComposedRegistry(t *testing.T) {
	t.Setenv("STEGO_REGISTRY", "")
	for _, archetype := range []string{"common", "application"} {
		t.Run(archetype, func(t *testing.T) {
			project := t.TempDir()
			initTestRegistry(t, project, "common-registry", "common")
			initTestRegistry(t, project, "application-registry", "application")
			config := "# Keep the operator's configuration.\nregistry:\n  - url: ./common-registry\n    ref: local\n  - url: ./application-registry\n    ref: application\n"
			initTestFile(t, project, ".stego/config.yaml", config)
			initTestFile(t, project, "fills/custom.go", "human source")
			result, err := Initialize(InitOptions{ProjectDir: project, Archetype: archetype})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Created) != 1 || result.Created[0] != "service.yaml" {
				t.Fatalf("unexpected created files: %v", result.Created)
			}
			assertInitService(t, project, archetype)
			for name, want := range map[string]string{".stego/config.yaml": config, "fills/custom.go": "human source"} {
				data, err := os.ReadFile(filepath.Join(project, name))
				if err != nil || string(data) != want {
					t.Fatalf("%s changed: %q, %v", name, data, err)
				}
			}
		})
	}
}

func TestInitializeDefaultRegistry(t *testing.T) {
	t.Setenv("STEGO_REGISTRY", "")
	project := t.TempDir()
	initTestRegistry(t, project, "registry", "example")
	result, err := Initialize(InitOptions{ProjectDir: project, Archetype: "example"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Created) != 3 {
		t.Fatalf("unexpected created files: %v", result.Created)
	}
	assertInitService(t, project, "example")
	data, err := os.ReadFile(filepath.Join(project, ".stego/config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var config types.RegistryConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if len(config.Registry) != 1 || config.Registry[0].URL != filepath.Join(project, "registry") || config.Registry[0].Ref != "local" {
		t.Fatalf("unexpected registry config: %+v", config)
	}
}

func TestInitializeRejectsInvalidConfiguration(t *testing.T) {
	t.Setenv("STEGO_REGISTRY", "")
	for _, config := range []string{"", "registry: [", "registry: []\n", "registry:\n  - url: ./registry\n    ref: local\nunknown: true\n", strings.Repeat(" ", parser.MaxDocumentBytes+1)} {
		t.Run("invalid", func(t *testing.T) {
			project := t.TempDir()
			initTestRegistry(t, project, "registry", "example")
			initTestFile(t, project, ".stego/config.yaml", config)
			if _, err := Initialize(InitOptions{ProjectDir: project, Archetype: "example"}); err == nil {
				t.Fatal("invalid config accepted")
			}
			for _, name := range []string{"service.yaml", "fills", ".stego/apply.lock"} {
				if _, err := os.Lstat(filepath.Join(project, name)); !os.IsNotExist(err) {
					t.Fatalf("initialization wrote %s: %v", name, err)
				}
			}
			data, err := os.ReadFile(filepath.Join(project, ".stego/config.yaml"))
			if err != nil || string(data) != config {
				t.Fatalf("configuration changed: %v", err)
			}
		})
	}
}

func TestInitializeRejectsRegistryReplacement(t *testing.T) {
	t.Setenv("STEGO_REGISTRY", "")
	project := t.TempDir()
	for _, directory := range []string{"common", "application"} {
		initTestRegistry(t, project, directory, "example")
	}
	initTestFile(t, project, ".stego/config.yaml", "registry:\n  - url: ./common\n    ref: local\n  - url: ./application\n    ref: local\n")
	if _, err := Initialize(InitOptions{ProjectDir: project, Archetype: "example"}); err == nil || !strings.Contains(err.Error(), "more than one source") {
		t.Fatalf("expected duplicate rejection, got %v", err)
	}
	if _, err := os.Lstat(filepath.Join(project, "service.yaml")); !os.IsNotExist(err) {
		t.Fatalf("service was written: %v", err)
	}
}

func TestInitializeRejectsExistingPaths(t *testing.T) {
	t.Setenv("STEGO_REGISTRY", "")
	for _, name := range []string{"service.yaml", ".stego/config.yaml", ".stego", "fills"} {
		for _, kind := range []string{"file", "directory", "link", "dangling-link"} {
			if (name == ".stego/config.yaml" && kind == "file") || ((name == ".stego" || name == "fills") && kind == "directory") {
				continue
			}
			t.Run(name+"/"+kind, func(t *testing.T) {
				project, outside := t.TempDir(), t.TempDir()
				initTestRegistry(t, project, "registry", "example")
				initTestFile(t, outside, "protected", "human source")
				path := filepath.Join(project, name)
				if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "file":
					initTestFile(t, project, name, "human source")
				case "directory":
					if err := os.Mkdir(path, 0755); err != nil {
						t.Fatal(err)
					}
				default:
					target := outside
					if kind == "dangling-link" {
						target = filepath.Join(outside, "missing")
					}
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := Initialize(InitOptions{ProjectDir: project, Archetype: "example"}); err == nil {
					t.Fatal("existing path accepted")
				}
				entries, err := os.ReadDir(outside)
				if err != nil || len(entries) != 1 {
					t.Fatalf("outside files changed: %v, %v", entries, err)
				}
				data, err := os.ReadFile(filepath.Join(outside, "protected"))
				if err != nil || string(data) != "human source" {
					t.Fatalf("outside content changed: %q, %v", data, err)
				}
				if kind == "file" {
					data, err := os.ReadFile(path)
					if err != nil || string(data) != "human source" {
						t.Fatalf("existing file changed: %q, %v", data, err)
					}
				}
			})
		}
	}
}

func TestInitializeConcurrent(t *testing.T) {
	t.Setenv("STEGO_REGISTRY", "")
	project := t.TempDir()
	initTestRegistry(t, project, "registry", "example")
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, err := Initialize(InitOptions{ProjectDir: project, Archetype: "example", Stderr: io.Discard})
			results <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	passed := 0
	for err := range results {
		if err == nil {
			passed++
		}
	}
	if passed != 1 {
		t.Fatalf("expected one successful initializer, got %d", passed)
	}
	assertInitService(t, project, "example")
	for _, directory := range []string{".", ".stego"} {
		entries, err := os.ReadDir(filepath.Join(project, directory))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".stego-write-") {
				t.Fatalf("staging file remains: %s", entry.Name())
			}
		}
	}
}

func TestInitializeUsesProjectLock(t *testing.T) {
	t.Setenv("STEGO_REGISTRY", "")
	project := t.TempDir()
	initTestRegistry(t, project, "registry", "example")
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	lock, err := lockProject(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := Initialize(InitOptions{ProjectDir: project, Archetype: "example"}); err == nil || !strings.Contains(err.Error(), "cannot lock project") {
		t.Fatalf("project lock ignored: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(project, "service.yaml")); !os.IsNotExist(err) {
		t.Fatalf("service was written: %v", err)
	}
}

func TestRootCreateDoesNotReplaceFiles(t *testing.T) {
	project := t.TempDir()
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	original := bytes.Repeat([]byte("complete content\n"), 100)
	if err := createRootFile(root, "nested/config.yaml", original); err != nil {
		t.Fatal(err)
	}
	if err := createRootFile(root, "nested/config.yaml", []byte("replacement")); err == nil {
		t.Fatal("existing file replaced")
	}
	data, err := root.ReadFile("nested/config.yaml")
	if err != nil || !bytes.Equal(data, original) {
		t.Fatalf("original content changed: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(project, "nested"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("staging file remains: %v, %v", entries, err)
	}
}
