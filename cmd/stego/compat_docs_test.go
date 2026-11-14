package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompatDocs(t *testing.T) {
	t.Parallel()
	t.Run("ST-GEN-UC14-R05-T01 The documentation of the check names the base ref as the running version and a skipped release as the operator's risk", func(t *testing.T) {
		t.Parallel()
		for _, page := range []string{"compat.md", "schema-changes.md"} {
			data, err := os.ReadFile(filepath.Join("..", "..", "docs", page))
			if err != nil {
				t.Fatal(err)
			}
			text := strings.Join(strings.Fields(string(data)), " ")
			for _, want := range []string{
				"The check takes the declaration at the base ref as the version that runs now.",
				"A release that is not deployed before the next one is the operator's risk.",
			} {
				if !strings.Contains(text, want) {
					t.Errorf("docs/%s does not state %q", page, want)
				}
			}
		}
	})
}
