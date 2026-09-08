package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

func TestApplyRejectsSymbolicLinksBeforeWriting(t *testing.T) {
	for _, link := range []string{"out", "out/internal", "out/main.go", "go.mod", ".stego"} {
		t.Run(link, func(t *testing.T) {
			project, target := t.TempDir(), t.TempDir()
			protected := filepath.Join(target, "protected.go")
			if err := os.WriteFile(protected, []byte("human source"), 0600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(project, link)
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			linkTarget := target
			if strings.HasSuffix(link, ".go") || link == "go.mod" {
				linkTarget = protected
			}
			if err := os.Symlink(linkTarget, path); err != nil {
				t.Fatal(err)
			}
			plan := &Plan{
				GeneratedFiles: []gen.File{
					{Path: "go.mod", Content: []byte("new module")},
					{Path: "main.go", Content: []byte("new main")},
					{Path: "internal/api.go", Content: []byte("new API")},
				},
				NewState: &State{},
			}
			if err := Apply(plan, project, filepath.Join(project, "out")); err == nil || !strings.Contains(err.Error(), "symbolic link") {
				t.Fatalf("expected symbolic link error, got %v", err)
			}
			data, err := os.ReadFile(protected)
			if err != nil || string(data) != "human source" {
				t.Fatalf("link target changed: %q, %v", data, err)
			}
			entries, err := os.ReadDir(target)
			if err != nil || len(entries) != 1 {
				t.Fatalf("files appeared outside output: %v, %v", entries, err)
			}
			if link != "go.mod" {
				if _, err := os.Stat(filepath.Join(project, "go.mod")); !os.IsNotExist(err) {
					t.Fatalf("apply wrote before preflight: %v", err)
				}
			}
		})
	}
}

func TestRootWriteReplacesCompleteFile(t *testing.T) {
	directory := t.TempDir()
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, content := range []string{"long initial content", "short"} {
		if err := writeRootFile(root, "nested/file.go", []byte(content)); err != nil {
			t.Fatal(err)
		}
		data, err := root.ReadFile("nested/file.go")
		if err != nil || string(data) != content {
			t.Fatalf("incomplete file: %q, %v", data, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(directory, "nested"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary files remain: %v, %v", entries, err)
	}
}

func TestApplyRejectsReservedOutputRoots(t *testing.T) {
	for _, name := range []string{"fills", "fills/generated", ".stego", ".git/objects"} {
		t.Run(name, func(t *testing.T) {
			project := t.TempDir()
			plan := &Plan{GeneratedFiles: []gen.File{{Path: "main.go"}}, NewState: &State{}}
			if err := Apply(plan, project, filepath.Join(project, name)); err == nil {
				t.Fatal("reserved root was accepted")
			}
			entries, err := os.ReadDir(project)
			if err != nil || len(entries) != 0 {
				t.Fatalf("project changed: %v, %v", entries, err)
			}
		})
	}
}
