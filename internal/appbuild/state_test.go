package appbuild

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestGeneratedStateRejectsInputDriftAndExtraOutput(t *testing.T) {
	source := filepath.Join("..", "..", "examples", "user-management")
	if _, _, err := generatedState(source); err != nil {
		t.Fatal("committed generated example is not accepted", err)
	}
	for _, name := range []string{"out/main.go", "out/extra.go", "service.yaml"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			err := filepath.WalkDir(source, func(name string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if entry.IsDir() {
					return nil
				}
				rel, err := filepath.Rel(source, name)
				if err != nil {
					return err
				}
				data, err := os.ReadFile(name)
				if err != nil {
					return err
				}
				put(t, root, rel, string(data))
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			put(t, root, name, "changed input")
			if _, _, err := generatedState(root); err == nil {
				t.Fatal("changed source was accepted")
			}
		})
	}
}
