package compiler

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadState_NonExistent(t *testing.T) {
	s, err := LoadState("/nonexistent/state.yaml")
	if err != nil {
		t.Fatalf("expected nil error for nonexistent file, got: %v", err)
	}
	if s.LastApplied != nil {
		t.Error("expected nil LastApplied for nonexistent state file")
	}
}

func TestSaveAndLoadState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".stego", "state.yaml")

	state := &State{
		LastApplied: &AppliedState{
			ServiceHash: "abc123",
			Components: map[string]ComponentState{
				"rest-api": {Version: "2.1.0", SHA: "f4e5d6"},
			},
			Files: map[string]string{
				"main.go":           "hash1",
				"internal/api/handler.go": "hash2",
			},
		},
	}

	if err := SaveState(path, state); err != nil {
		t.Fatalf("SaveState failed: %v", err)
	}

	// Verify the file exists.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file not written: %v", err)
	}

	// Reload and verify.
	loaded, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState failed: %v", err)
	}
	if loaded.LastApplied == nil {
		t.Fatal("expected LastApplied to be set")
	}
	if loaded.LastApplied.ServiceHash != "abc123" {
		t.Errorf("expected service hash abc123, got %s", loaded.LastApplied.ServiceHash)
	}
	if len(loaded.LastApplied.Components) != 1 {
		t.Fatalf("expected 1 component, got %d", len(loaded.LastApplied.Components))
	}
	comp := loaded.LastApplied.Components["rest-api"]
	if comp.Version != "2.1.0" {
		t.Errorf("expected version 2.1.0, got %s", comp.Version)
	}
	if comp.SHA != "f4e5d6" {
		t.Errorf("expected SHA f4e5d6, got %s", comp.SHA)
	}
	if len(loaded.LastApplied.Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(loaded.LastApplied.Files))
	}
}

func TestHashBytes(t *testing.T) {
	h1 := HashBytes([]byte("hello"))
	h2 := HashBytes([]byte("hello"))
	h3 := HashBytes([]byte("world"))

	if h1 != h2 {
		t.Error("same input should produce same hash")
	}
	if h1 == h3 {
		t.Error("different input should produce different hash")
	}
	if len(h1) != 64 {
		t.Errorf("expected 64-char hex hash, got %d chars", len(h1))
	}
}

func TestSaveAndLoadState_WithEntityFieldState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".stego", "state.yaml")

	state := &State{
		LastApplied: &AppliedState{
			ServiceHash: "abc123",
			Entities: map[string][]EntityFieldState{
				"User": {
					{Name: "email", Type: "string", Hash: "hash1"},
					{Name: "score", Type: "int32", Hash: "hash2"},
				},
			},
			Files: map[string]string{"main.go": "hash3"},
		},
	}

	if err := SaveState(path, state); err != nil {
		t.Fatalf("SaveState failed: %v", err)
	}

	loaded, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState failed: %v", err)
	}
	if loaded.LastApplied == nil {
		t.Fatal("expected LastApplied")
	}
	fields := loaded.LastApplied.Entities["User"]
	if len(fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(fields))
	}
	if fields[0].Name != "email" || fields[0].Type != "string" || fields[0].Hash != "hash1" {
		t.Errorf("unexpected first field: %+v", fields[0])
	}
	if fields[1].Name != "score" || fields[1].Type != "int32" || fields[1].Hash != "hash2" {
		t.Errorf("unexpected second field: %+v", fields[1])
	}
}

func TestSaveState_CreatesParentDirectories(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deep", "nested", "state.yaml")

	state := &State{}
	if err := SaveState(path, state); err != nil {
		t.Fatalf("SaveState failed: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file not created at nested path: %v", err)
	}
}
