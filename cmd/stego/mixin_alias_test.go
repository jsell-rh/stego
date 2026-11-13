package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestMixinAlias(t *testing.T) {
	t.Run("ST-SUP-UC10-R04-T01 Binding event-publisher gives durable-events and kafka-events", func(t *testing.T) {
		// tree gives each file of the generated output of a project and of its go.mod, by path, after stego apply.
		tree := func(service string) map[string]string {
			t.Helper()
			project := td02Project(t, service)
			if run := stegoIn(t, project, nil, "apply"); run.code != 0 {
				t.Fatalf("stego apply of %q: exit %d\n%s%s", service[strings.LastIndex(service, "mixins:"):], run.code, run.stdout, run.stderr)
			}
			files := map[string]string{}
			out := filepath.Join(project, "out")
			if err := filepath.WalkDir(out, func(path string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				relative, _ := filepath.Rel(out, path)
				// The version of the declaration is the digest of its text, which names the mixins in two ways.
				files[relative] = declarationVersion.ReplaceAllString(string(data), "DeclarationVersion = \"\"")
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			mod, err := os.ReadFile(filepath.Join(project, "go.mod"))
			if err != nil {
				t.Fatal(err)
			}
			files["go.mod"] = strings.Replace(string(mod), filepath.Base(project), "project", -1)
			return files
		}
		alias := tree(td02 + "mixins: [event-publisher]\n")
		named := tree(td02 + "mixins: [durable-events, kafka-events]\n")
		if len(alias) == 0 {
			t.Fatal("the plan of event-publisher made no file")
		}
		for path, data := range alias {
			if other, ok := named[path]; !ok {
				t.Errorf("the plan of event-publisher makes %s, and the plan of durable-events and kafka-events does not", path)
			} else if other != data {
				a, b := strings.Split(data, "\n"), strings.Split(other, "\n")
				for i := range min(len(a), len(b)) {
					if a[i] != b[i] {
						t.Errorf("the plans make %s with other content, first at line %d:\n%s\n%s", path, i+1, a[i], b[i])
						break
					}
				}
			}
		}
		for path := range named {
			if _, ok := alias[path]; !ok {
				t.Errorf("the plan of durable-events and kafka-events makes %s, and the plan of event-publisher does not", path)
			}
		}
		// The alias adds the outbox and the Kafka destination, as the two mixins do.
		if _, ok := alias["internal/kafka/doc.go"]; !ok && !strings.Contains(strings.Join(treePaths(alias), " "), "outbox") {
			t.Errorf("the plan of event-publisher has no file of the outbox: %v", treePaths(alias))
		}
	})
}

// declarationVersion is the line of the generated code that holds the digest of the declaration's text.
var declarationVersion = regexp.MustCompile(`DeclarationVersion = "[0-9a-f]{64}"`)

// treePaths gives the paths of a tree.
func treePaths(files map[string]string) []string {
	var paths []string
	for path := range files {
		paths = append(paths, path)
	}
	return paths
}
