package registry_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/registry"
	"github.com/jsell-rh/stego/internal/types"
)

func TestRegistryRejectsUnpinnedAndTraversalRefs(t *testing.T) {
	for _, ref := range []string{"main", "v1.0.0", "abc123", "../..", "../../protected", "/tmp/registry", "--orphan=other", strings.Repeat("A", 40)} {
		t.Run(ref, func(t *testing.T) {
			root := t.TempDir()
			project := filepath.Join(root, "project")
			cache := filepath.Join(root, "cache")
			protected := filepath.Join(root, "protected")
			if err := os.WriteFile(protected, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			writeConfig(t, project, types.RegistryConfig{Registry: []types.RegistrySource{{URL: "file:///missing.git", Ref: ref}}})
			t.Setenv("STEGO_REGISTRY", "")
			_, err := registry.ResolveRegistry(registry.ResolveOptions{ProjectDir: project, CacheDir: cache})
			if err == nil || !strings.Contains(err.Error(), "full lowercase Git commit SHA") {
				t.Fatalf("got %v, want pinned commit error", err)
			}
			if _, err := os.Stat(cache); !os.IsNotExist(err) {
				t.Fatalf("invalid ref changed the cache: %v", err)
			}
			data, err := os.ReadFile(protected)
			if err != nil || string(data) != "keep" {
				t.Fatalf("invalid ref changed a protected file: %q, %v", data, err)
			}
		})
	}
}

func TestRegistryRejectsModifiedCacheWithoutDeletingIt(t *testing.T) {
	root := t.TempDir()
	bare, work := filepath.Join(root, "bare.git"), filepath.Join(root, "work")
	initBareRepo(t, bare)
	cloneAndPopulate(t, bare, work)
	project := filepath.Join(root, "project")
	writeConfig(t, project, types.RegistryConfig{Registry: []types.RegistrySource{{URL: "file://" + bare, Ref: getHeadSHA(t, work)}}})
	t.Setenv("STEGO_REGISTRY", "")
	options := registry.ResolveOptions{ProjectDir: project, CacheDir: filepath.Join(root, "cache")}
	result, err := registry.ResolveRegistry(options)
	if err != nil {
		t.Fatal(err)
	}
	changed := filepath.Join(result.Dir, "archetypes/test-arch/archetype.yaml")
	if err := os.WriteFile(changed, []byte("modified source"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ResolveRegistry(options); err == nil || !strings.Contains(err.Error(), "modified or incomplete") {
		t.Fatalf("modified cache was reused: %v", err)
	}
	data, err := os.ReadFile(changed)
	if err != nil || string(data) != "modified source" {
		t.Fatalf("modified cache was deleted or replaced: %q, %v", data, err)
	}
}

func TestRegistryRejectsUntrackedCacheContent(t *testing.T) {
	root := t.TempDir()
	bare, work := filepath.Join(root, "bare.git"), filepath.Join(root, "work")
	initBareRepo(t, bare)
	cloneAndPopulate(t, bare, work)
	project := filepath.Join(root, "project")
	writeConfig(t, project, types.RegistryConfig{Registry: []types.RegistrySource{{URL: "file://" + bare, Ref: getHeadSHA(t, work)}}})
	t.Setenv("STEGO_REGISTRY", "")
	options := registry.ResolveOptions{ProjectDir: project, CacheDir: filepath.Join(root, "cache")}
	result, err := registry.ResolveRegistry(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(result.Dir, ".git/info"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(result.Dir, ".git/info/exclude"), []byte("injected.yaml\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(result.Dir, "injected.yaml"), []byte("ignored content"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ResolveRegistry(options); err == nil {
		t.Fatal("ignored cache content was accepted as pinned content")
	}
}

func TestConcurrentRegistryResolutionPublishesOneCheckout(t *testing.T) {
	root := t.TempDir()
	bare, work := filepath.Join(root, "bare.git"), filepath.Join(root, "work")
	initBareRepo(t, bare)
	cloneAndPopulate(t, bare, work)
	project := filepath.Join(root, "project")
	writeConfig(t, project, types.RegistryConfig{Registry: []types.RegistrySource{{URL: "file://" + bare, Ref: getHeadSHA(t, work)}}})
	t.Setenv("STEGO_REGISTRY", "")
	options := registry.ResolveOptions{ProjectDir: project, CacheDir: filepath.Join(root, "cache")}
	type outcome struct {
		result *registry.ResolveResult
		err    error
	}
	results := make(chan outcome, 2)
	for range 2 {
		go func() {
			result, err := registry.ResolveRegistry(options)
			results <- outcome{result, err}
		}()
	}
	first, second := <-results, <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent resolution failed: %v, %v", first.err, second.err)
	}
	if first.result.Dir != second.result.Dir {
		t.Fatalf("different cache entries: %s, %s", first.result.Dir, second.result.Dir)
	}
	entries, err := os.ReadDir(filepath.Dir(first.result.Dir))
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary or duplicate checkout remains: %v, %v", entries, err)
	}
}
