package compiler

import (
	"bytes"
	"go/format"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGeneratedGoIsFormatted(t *testing.T) {
	t.Run("ST-STD-GEN-R04-T01 The TLS open files that the compiler emits are as gofmt formats them", func(t *testing.T) {
		for _, file := range httpTLSFiles() {
			formatted, err := format.Source(file.Content)
			if err != nil {
				t.Fatalf("%s does not parse: %v", file.Path, err)
			}
			if !bytes.Equal(formatted, file.Content) {
				t.Errorf("%s is not as gofmt formats it:\n%s", file.Path, file.Content)
			}
		}
	})
	t.Run("ST-STD-GEN-R04-T02 Every generated Go file of the examples is as gofmt formats it", func(t *testing.T) {
		examples := filepath.Join("..", "..", "examples")
		count := 0
		err := filepath.WalkDir(examples, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || !strings.Contains(filepath.ToSlash(path), "/out/") {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			count++
			if formatted, err := format.Source(data); err != nil || !bytes.Equal(formatted, data) {
				t.Errorf("%s is not as gofmt formats it (%v)", path, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			t.Fatal("no generated Go file of an example was read")
		}
	})
}
