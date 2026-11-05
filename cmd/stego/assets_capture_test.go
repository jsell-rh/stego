package main

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// These tests run stego assets through the binary in a temporary folder, as a
// service author captures a frontend build. No registry is needed.

// frontendBuild writes a build folder of index.html and two assets, in
// reverse path order, and gives its path.
func frontendBuild(t *testing.T) string {
	t.Helper()
	folder := filepath.Join(t.TempDir(), "build")
	if err := os.MkdirAll(filepath.Join(folder, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, folder, "assets/app.js", "document.body.dataset.ready = 'yes';\n", 0o644)
	writeFile(t, folder, "assets/app.css", "body { margin: 0; }\n", 0o644)
	writeFile(t, folder, "index.html", `<!doctype html><html><head><link rel="stylesheet" href="/assets/app.css"></head><body><script src="/assets/app.js"></script></body></html>`, 0o644)
	return folder
}

// bundleEntries reads the entries of a bundle in their order.
func bundleEntries(t *testing.T, data []byte) []*zip.File {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	return reader.File
}

func captureBundle(t *testing.T, folder, output string) {
	t.Helper()
	mustStego(t, "assets", "--directory", folder, "--output", output)
}

func TestSameFolderGivesSameBundle(t *testing.T) {
	t.Run("ST-GEN-UC07-R01-T01 Two captures of one frontend build give byte-identical bundles with sorted entries and fixed metadata", func(t *testing.T) {
		folder := frontendBuild(t)
		one := filepath.Join(t.TempDir(), "one.zip")
		captureBundle(t, folder, one)

		later := time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC)
		for _, name := range []string{"index.html", "assets/app.js", "assets/app.css"} {
			if err := os.Chtimes(filepath.Join(folder, name), later, later); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(folder, name), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		copied := copyFolder(t, folder)
		two := filepath.Join(t.TempDir(), "two.zip")
		captureBundle(t, copied, two)

		first, err := os.ReadFile(one)
		if err != nil {
			t.Fatal(err)
		}
		second, err := os.ReadFile(two)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, second) {
			t.Fatal("two captures of one build give two bundles")
		}
		for _, data := range [][]byte{first, second} {
			entries := bundleEntries(t, data)
			var paths []string
			for _, entry := range entries {
				paths = append(paths, entry.Name)
				if !entry.Modified.Equal(entries[0].Modified) || entry.Mode() != entries[0].Mode() {
					t.Errorf("entry %s has modification time %v and mode %v, not the fixed %v and %v", entry.Name, entry.Modified, entry.Mode(), entries[0].Modified, entries[0].Mode())
				}
			}
			if !slices.IsSorted(paths) || len(paths) != 3 {
				t.Errorf("the entries %v are not the three files in byte order of their paths", paths)
			}
			if entries[0].Modified.Equal(later) {
				t.Error("the bundle keeps the modification time of the files")
			}
		}
	})
}

// refusedCapture runs stego assets on a folder that holds a refused entry,
// and checks that it names the entry and leaves the bundle as it was.
func refusedCapture(t *testing.T, folder, output, entry string) {
	t.Helper()
	before, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := runStego(t, "assets", "--directory", folder, "--output", output)
	if err == nil || !strings.Contains(stdout+stderr, entry) {
		t.Fatalf("stego assets did not refuse and name %s: %v\n%s%s", entry, err, stdout, stderr)
	}
	after, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("a refused capture changed the bundle: %v", err)
	}
	if now, err := os.Stat(output); err != nil || !now.ModTime().Equal(info.ModTime()) {
		t.Fatalf("a refused capture changed the bundle's modification time: %v", err)
	}
	siblings, err := os.ReadDir(filepath.Dir(output))
	if err != nil {
		t.Fatal(err)
	}
	if len(siblings) != 1 {
		t.Fatalf("a refused capture left a file beside the bundle: %v", siblings)
	}
}

func TestRefusedEntryChangesNothing(t *testing.T) {
	t.Run("ST-GEN-UC07-R02-T01 A refused entry leaves the output bundle as it was", func(t *testing.T) {
		folder := frontendBuild(t)
		output := filepath.Join(t.TempDir(), "bundle.zip")
		captureBundle(t, folder, output)

		linked := copyFolder(t, folder)
		if err := os.Symlink("index.html", filepath.Join(linked, "zz-link")); err != nil {
			t.Fatal(err)
		}
		refusedCapture(t, linked, output, "zz-link")

		remote := copyFolder(t, folder)
		writeFile(t, remote, "index.html", `<!doctype html><html><body><script src="https://example.test/app.js"></script></body></html>`, 0o644)
		refusedCapture(t, remote, output, "index.html")
	})
}

func TestCaptureFlow(t *testing.T) {
	t.Run("ST-GEN-UC07-FL-T A frontend build folder becomes one checked bundle, and a bad entry changes nothing", func(t *testing.T) {
		folder := frontendBuild(t)
		output := filepath.Join(t.TempDir(), "bundle.zip")
		captureBundle(t, folder, output)
		first, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		captureBundle(t, folder, output)
		second, err := os.ReadFile(output)
		if err != nil || !bytes.Equal(first, second) {
			t.Fatalf("two captures of one folder wrote two bundles: %v", err)
		}
		if err := os.Symlink("index.html", filepath.Join(folder, "zz-link")); err != nil {
			t.Fatal(err)
		}
		refusedCapture(t, folder, output, "zz-link")
	})
}
