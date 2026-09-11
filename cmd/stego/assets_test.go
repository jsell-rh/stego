package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAssetsCommand(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(t.TempDir(), "assets.zip")
	if err := os.WriteFile(filepath.Join(directory, "index.html"), []byte("<html><body>Records</body></html>"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := runAssets([]string{"--directory", directory, "--output", output}); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(output)
	if err != nil || len(original) == 0 {
		t.Fatal("missing bundle", err)
	}
	if err := os.WriteFile(filepath.Join(directory, "index.html"), []byte(`<script src="https://example.test/x.js"></script>`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := runAssets([]string{"--directory", directory, "--output", output}); err == nil {
		t.Fatal("invalid HTML accepted")
	}
	after, err := os.ReadFile(output)
	if err != nil || string(original) != string(after) {
		t.Fatal("invalid input changed bundle")
	}
}
