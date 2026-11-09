package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// yamlBlockPattern is a fenced YAML block of a Markdown doc, with its body.
var yamlBlockPattern = regexp.MustCompile("(?s)```ya?ml\n(.*?)```")

// userDocs reads the user doc inventory (ST-ASR-TOOL05): one path from the repository's root per line, a line that
// starts with # being a comment.
func userDocs(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "assure", "testdata", "user-docs.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var docs []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			docs = append(docs, line)
		}
	}
	return docs
}

func TestUserDocDeclarations(t *testing.T) {
	t.Run("ST-STD-DOCS-R02-T02 Each whole service declaration in a user doc passes stego validate against the repository's registry", func(t *testing.T) {
		registry, err := filepath.Abs(filepath.Join("..", "..", "registry"))
		if err != nil {
			t.Fatal(err)
		}
		found := 0
		for _, doc := range userDocs(t) {
			data, err := os.ReadFile(filepath.Join("..", "..", doc))
			if err != nil {
				t.Fatal(err)
			}
			for i, block := range yamlBlockPattern.FindAllStringSubmatch(string(data), -1) {
				if !strings.HasPrefix(block[1], "kind: service\n") {
					continue
				}
				found++
				project := t.TempDir()
				putFile(t, project, "service.yaml", block[1])
				if err := os.MkdirAll(filepath.Join(project, "fills"), 0o755); err != nil {
					t.Fatal(err)
				}
				run := stegoIn(t, project, []string{"STEGO_REGISTRY=" + registry}, "validate")
				if run.code != 0 {
					t.Errorf("%s, YAML block %d: stego validate exits %d:\n%s%s", doc, i+1, run.code, run.stdout, run.stderr)
				}
			}
		}
		if found == 0 {
			t.Fatal("no user doc holds a whole service declaration, so the rule proves nothing; README.md's quick start holds one")
		}
	})
}
