package registry_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/registry"
	"github.com/jsell-rh/stego/internal/types"
	"gopkg.in/yaml.v3"
)

func TestResolveRegistryEnvOverride(t *testing.T) {
	tmp := t.TempDir()
	registryDir := filepath.Join(tmp, "my-registry")
	if err := os.MkdirAll(registryDir, 0755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("STEGO_REGISTRY", registryDir)

	var stderr bytes.Buffer
	result, err := registry.ResolveRegistry(registry.ResolveOptions{
		ProjectDir: tmp,
		Stderr:     &stderr,
	})
	if err != nil {
		t.Fatalf("ResolveRegistry() error: %v", err)
	}

	if result.Dir != registryDir {
		t.Errorf("Dir = %q, want %q", result.Dir, registryDir)
	}
	if !result.EnvOverride {
		t.Error("expected EnvOverride = true")
	}
	if result.Ref != "env-override" {
		t.Errorf("Ref = %q, want %q", result.Ref, "env-override")
	}

	// Verify warning was printed to stderr.
	warning := stderr.String()
	if !strings.Contains(warning, "WARNING") {
		t.Errorf("expected WARNING in stderr, got: %q", warning)
	}
	if !strings.Contains(warning, registryDir) {
		t.Errorf("expected registry dir in warning, got: %q", warning)
	}
	if !strings.Contains(warning, "config.yaml registry settings ignored") {
		t.Errorf("expected 'config.yaml registry settings ignored' in warning, got: %q", warning)
	}
}

func TestResolveRegistryLocalPath(t *testing.T) {
	tmp := t.TempDir()

	// Create a local registry directory.
	registryDir := filepath.Join(tmp, "local-registry")
	if err := os.MkdirAll(registryDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write config.yaml pointing to the local directory.
	writeConfig(t, tmp, types.RegistryConfig{
		Registry: []types.RegistrySource{
			{URL: registryDir, Ref: "local"},
		},
	})

	// Ensure STEGO_REGISTRY is not set.
	t.Setenv("STEGO_REGISTRY", "")

	var stderr bytes.Buffer
	result, err := registry.ResolveRegistry(registry.ResolveOptions{
		ProjectDir: tmp,
		Stderr:     &stderr,
	})
	if err != nil {
		t.Fatalf("ResolveRegistry() error: %v", err)
	}

	if result.Dir != registryDir {
		t.Errorf("Dir = %q, want %q", result.Dir, registryDir)
	}
	if result.Ref != "local" {
		t.Errorf("Ref = %q, want %q", result.Ref, "local")
	}
	if result.EnvOverride {
		t.Error("expected EnvOverride = false")
	}
	if stderr.Len() != 0 {
		t.Errorf("expected no warnings for local path, got: %q", stderr.String())
	}
}

func TestResolveRegistryMissingConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("STEGO_REGISTRY", "")

	_, err := registry.ResolveRegistry(registry.ResolveOptions{
		ProjectDir: tmp,
	})
	if err == nil {
		t.Fatal("expected error for missing config.yaml")
	}
	if !strings.Contains(err.Error(), "resolving registry") {
		t.Errorf("expected 'resolving registry' in error, got: %v", err)
	}
}

func TestResolveRegistryMultipleSourcesWarning(t *testing.T) {
	tmp := t.TempDir()

	// Create two local directories.
	reg1 := filepath.Join(tmp, "reg1")
	reg2 := filepath.Join(tmp, "reg2")
	if err := os.MkdirAll(reg1, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(reg2, 0755); err != nil {
		t.Fatal(err)
	}

	writeConfig(t, tmp, types.RegistryConfig{
		Registry: []types.RegistrySource{
			{URL: reg1, Ref: "local"},
			{URL: reg2, Ref: "local"},
		},
	})

	t.Setenv("STEGO_REGISTRY", "")

	var stderr bytes.Buffer
	result, err := registry.ResolveRegistry(registry.ResolveOptions{
		ProjectDir: tmp,
		Stderr:     &stderr,
	})
	if err != nil {
		t.Fatalf("ResolveRegistry() error: %v", err)
	}

	// Should use the first registry.
	if result.Dir != reg1 {
		t.Errorf("Dir = %q, want %q", result.Dir, reg1)
	}

	// Should warn about multiple registries.
	warning := stderr.String()
	if !strings.Contains(warning, "multiple registry sources") {
		t.Errorf("expected multi-registry warning, got: %q", warning)
	}
}

func TestResolveRegistryEnvOverrideSkipsConfig(t *testing.T) {
	tmp := t.TempDir()

	regDir := filepath.Join(tmp, "env-registry")
	if err := os.MkdirAll(regDir, 0755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("STEGO_REGISTRY", regDir)

	// No config.yaml exists — env override should succeed regardless.
	var stderr bytes.Buffer
	result, err := registry.ResolveRegistry(registry.ResolveOptions{
		ProjectDir: tmp,
		Stderr:     &stderr,
	})
	if err != nil {
		t.Fatalf("ResolveRegistry() error: %v", err)
	}
	if result.Dir != regDir {
		t.Errorf("Dir = %q, want %q", result.Dir, regDir)
	}
}

func TestResolveRegistryGitCloneAndCache(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available, skipping integration test")
	}

	tmp := t.TempDir()

	// Create a bare git repo with a registry tree.
	bareRepo := filepath.Join(tmp, "bare-registry.git")
	workTree := filepath.Join(tmp, "work")

	initBareRepo(t, bareRepo)
	cloneAndPopulate(t, bareRepo, workTree)

	// Get the commit SHA.
	commitSHA := getHeadSHA(t, workTree)

	// Set up project with config pointing to the bare repo via file:// URL.
	repoURL := "file://" + bareRepo
	projectDir := filepath.Join(tmp, "project")
	writeConfig(t, projectDir, types.RegistryConfig{
		Registry: []types.RegistrySource{
			{URL: repoURL, Ref: commitSHA},
		},
	})

	cacheDir := filepath.Join(tmp, "cache")

	t.Setenv("STEGO_REGISTRY", "")

	// First resolve: should clone.
	result, err := registry.ResolveRegistry(registry.ResolveOptions{
		ProjectDir: projectDir,
		CacheDir:   cacheDir,
	})
	if err != nil {
		t.Fatalf("ResolveRegistry() first call error: %v", err)
	}
	if result.Ref != commitSHA {
		t.Errorf("Ref = %q, want %q", result.Ref, commitSHA)
	}
	if result.EnvOverride {
		t.Error("expected EnvOverride = false")
	}

	// Verify the cached checkout has the expected content.
	archPath := filepath.Join(result.Dir, "archetypes", "test-arch", "archetype.yaml")
	if _, err := os.Stat(archPath); err != nil {
		t.Errorf("expected archetype.yaml at cached checkout: %v", err)
	}

	// Verify registry loads successfully from the cached checkout.
	reg, err := registry.Load(result.Dir)
	if err != nil {
		t.Fatalf("Load() from cached checkout error: %v", err)
	}
	if a := reg.Archetype("test-arch"); a == nil {
		t.Error("expected test-arch archetype in loaded registry")
	}

	// Second resolve: should reuse cache (no git operations).
	result2, err := registry.ResolveRegistry(registry.ResolveOptions{
		ProjectDir: projectDir,
		CacheDir:   cacheDir,
	})
	if err != nil {
		t.Fatalf("ResolveRegistry() second call error: %v", err)
	}
	if result2.Dir != result.Dir {
		t.Errorf("cached Dir changed: %q vs %q", result2.Dir, result.Dir)
	}
}

func TestResolveRegistryGitBadURL(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available, skipping integration test")
	}

	tmp := t.TempDir()
	projectDir := filepath.Join(tmp, "project")

	writeConfig(t, projectDir, types.RegistryConfig{
		Registry: []types.RegistrySource{
			{URL: "file:///nonexistent/no-such-repo.git", Ref: "abc123"},
		},
	})

	t.Setenv("STEGO_REGISTRY", "")

	_, err := registry.ResolveRegistry(registry.ResolveOptions{
		ProjectDir: projectDir,
		CacheDir:   filepath.Join(tmp, "cache"),
	})
	if err == nil {
		t.Fatal("expected error for bad git URL")
	}
	if !strings.Contains(err.Error(), "git clone") {
		t.Errorf("expected 'git clone' in error, got: %v", err)
	}
}

func TestResolveRegistryGitBadRef(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available, skipping integration test")
	}

	tmp := t.TempDir()

	// Create a bare repo with a valid commit.
	bareRepo := filepath.Join(tmp, "bare-registry.git")
	workTree := filepath.Join(tmp, "work")
	initBareRepo(t, bareRepo)
	cloneAndPopulate(t, bareRepo, workTree)

	// Use a ref that doesn't exist, via file:// URL.
	repoURL := "file://" + bareRepo
	projectDir := filepath.Join(tmp, "project")
	writeConfig(t, projectDir, types.RegistryConfig{
		Registry: []types.RegistrySource{
			{URL: repoURL, Ref: "0000000000000000000000000000000000000000"},
		},
	})

	t.Setenv("STEGO_REGISTRY", "")

	_, err := registry.ResolveRegistry(registry.ResolveOptions{
		ProjectDir: projectDir,
		CacheDir:   filepath.Join(tmp, "cache"),
	})
	if err == nil {
		t.Fatal("expected error for bad ref")
	}
	if !strings.Contains(err.Error(), "git checkout") {
		t.Errorf("expected 'git checkout' in error, got: %v", err)
	}
}

func TestResolveRegistryCacheIsolation(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available, skipping integration test")
	}

	tmp := t.TempDir()

	// Create a bare repo with two commits.
	bareRepo := filepath.Join(tmp, "bare-registry.git")
	workTree := filepath.Join(tmp, "work")
	initBareRepo(t, bareRepo)
	cloneAndPopulate(t, bareRepo, workTree)
	sha1 := getHeadSHA(t, workTree)

	// Add a second commit.
	compDir := filepath.Join(workTree, "components", "extra-comp")
	if err := os.MkdirAll(compDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(compDir, "component.yaml"), []byte("kind: component\nname: extra-comp\nversion: 1.0.0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workTree, "add", ".")
	gitCmd(t, workTree, "commit", "-m", "add extra component")
	gitCmd(t, workTree, "push")
	sha2 := getHeadSHA(t, workTree)

	if sha1 == sha2 {
		t.Fatal("expected different SHAs for the two commits")
	}

	cacheDir := filepath.Join(tmp, "cache")
	repoURL := "file://" + bareRepo
	t.Setenv("STEGO_REGISTRY", "")

	// Resolve with SHA1.
	projDir1 := filepath.Join(tmp, "proj1")
	writeConfig(t, projDir1, types.RegistryConfig{
		Registry: []types.RegistrySource{{URL: repoURL, Ref: sha1}},
	})
	result1, err := registry.ResolveRegistry(registry.ResolveOptions{
		ProjectDir: projDir1,
		CacheDir:   cacheDir,
	})
	if err != nil {
		t.Fatalf("resolve sha1: %v", err)
	}

	// Resolve with SHA2.
	projDir2 := filepath.Join(tmp, "proj2")
	writeConfig(t, projDir2, types.RegistryConfig{
		Registry: []types.RegistrySource{{URL: repoURL, Ref: sha2}},
	})
	result2, err := registry.ResolveRegistry(registry.ResolveOptions{
		ProjectDir: projDir2,
		CacheDir:   cacheDir,
	})
	if err != nil {
		t.Fatalf("resolve sha2: %v", err)
	}

	// Different refs should produce different cache directories.
	if result1.Dir == result2.Dir {
		t.Errorf("expected different cache dirs for different refs, got same: %q", result1.Dir)
	}

	// SHA2 checkout should have the extra component.
	extraPath := filepath.Join(result2.Dir, "components", "extra-comp", "component.yaml")
	if _, err := os.Stat(extraPath); err != nil {
		t.Errorf("SHA2 checkout should have extra-comp: %v", err)
	}

	// SHA1 checkout should NOT have the extra component.
	extraPath1 := filepath.Join(result1.Dir, "components", "extra-comp", "component.yaml")
	if _, err := os.Stat(extraPath1); !os.IsNotExist(err) {
		t.Errorf("SHA1 checkout should not have extra-comp")
	}
}

// --- Test helpers ---

func writeConfig(t *testing.T, projectDir string, cfg types.RegistryConfig) {
	t.Helper()
	stegoDir := filepath.Join(projectDir, ".stego")
	if err := os.MkdirAll(stegoDir, 0755); err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stegoDir, "config.yaml"), data, 0644); err != nil {
		t.Fatal(err)
	}
}

func initBareRepo(t *testing.T, path string) {
	t.Helper()
	gitCmd(t, "", "init", "--bare", path)
}

func cloneAndPopulate(t *testing.T, bareRepo, workTree string) {
	t.Helper()
	gitCmd(t, "", "clone", bareRepo, workTree)

	// Configure git user in the clone for commits.
	gitCmd(t, workTree, "config", "user.email", "test@test.com")
	gitCmd(t, workTree, "config", "user.name", "Test")

	// Create a minimal registry tree.
	archDir := filepath.Join(workTree, "archetypes", "test-arch")
	if err := os.MkdirAll(archDir, 0755); err != nil {
		t.Fatal(err)
	}
	archYAML := "kind: archetype\nname: test-arch\nversion: 1.0.0\nlanguage: go\n"
	if err := os.WriteFile(filepath.Join(archDir, "archetype.yaml"), []byte(archYAML), 0644); err != nil {
		t.Fatal(err)
	}

	gitCmd(t, workTree, "add", ".")
	gitCmd(t, workTree, "commit", "-m", "initial registry")
	gitCmd(t, workTree, "push")
}

func getHeadSHA(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
