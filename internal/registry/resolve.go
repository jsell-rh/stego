package registry

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ResolveResult contains the resolved registry directory and metadata.
type ResolveResult struct {
	// Dir is the local filesystem path to the registry directory.
	Dir string
	// Ref is the registry ref from config.yaml (recorded in state).
	Ref string
	// EnvOverride is true when STEGO_REGISTRY was used.
	EnvOverride bool
}

// ResolveOptions controls registry resolution behavior.
type ResolveOptions struct {
	// ProjectDir is the project root (where .stego/ lives).
	ProjectDir string
	// Stderr receives warning messages (e.g. STEGO_REGISTRY override).
	// If nil, os.Stderr is used.
	Stderr io.Writer
	// CacheDir overrides the default cache directory (~/.cache/stego/registries).
	// Used for testing.
	CacheDir string
}

// ResolveRegistry determines the registry directory from config and environment.
//
// Resolution order:
//  1. STEGO_REGISTRY env var — overrides everything, prints a warning to stderr.
//  2. .stego/config.yaml registry[0] — local path or git URL.
//
// For git URLs (url is not an existing local directory), the repo is cloned into
// a cache directory and checked out at the exact ref SHA. For local paths, the
// path is used directly.
func ResolveRegistry(opts ResolveOptions) (*ResolveResult, error) {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	// 1. Check STEGO_REGISTRY env var override.
	if envReg := os.Getenv("STEGO_REGISTRY"); envReg != "" {
		fmt.Fprintf(stderr, "WARNING: using STEGO_REGISTRY override: %s (config.yaml registry settings ignored)\n", envReg)
		return &ResolveResult{
			Dir:         envReg,
			Ref:         "env-override",
			EnvOverride: true,
		}, nil
	}

	// 2. Load config.yaml.
	configPath := filepath.Join(opts.ProjectDir, ".stego", "config.yaml")
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("resolving registry: %w", err)
	}

	if len(cfg.Registry) == 0 {
		return nil, fmt.Errorf("resolving registry: no registry sources in %s", configPath)
	}

	// Warn about multiple registries (only first is used, per spec).
	if len(cfg.Registry) > 1 {
		fmt.Fprintf(stderr, "WARNING: multiple registry sources configured; only the first is used (multi-registry support is deferred to post-MVP)\n")
	}

	src := cfg.Registry[0]

	// Determine if URL is a local directory or a git remote.
	if isLocalDir(src.URL) {
		return &ResolveResult{
			Dir: src.URL,
			Ref: src.Ref,
		}, nil
	}

	// Git-based resolution: clone/fetch into cache, checkout at ref.
	dir, err := resolveGitRegistry(src.URL, src.Ref, opts.CacheDir)
	if err != nil {
		return nil, err
	}

	return &ResolveResult{
		Dir: dir,
		Ref: src.Ref,
	}, nil
}

// isLocalDir returns true if the path is an existing local directory.
func isLocalDir(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// resolveGitRegistry clones or fetches a git repo and checks out a specific ref.
// The cache layout is: <cacheDir>/<url-hash>/<ref>/
func resolveGitRegistry(url, ref, cacheDir string) (string, error) {
	if cacheDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("determining cache directory: %w", err)
		}
		cacheDir = filepath.Join(home, ".cache", "stego", "registries")
	}

	urlHash := hashURL(url)
	checkoutDir := filepath.Join(cacheDir, urlHash, ref)

	// If cached checkout exists and is valid, reuse it.
	if isValidCheckout(checkoutDir, ref) {
		return checkoutDir, nil
	}

	// Ensure the git binary is available.
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return "", fmt.Errorf("git binary not found for registry %s at ref %s: %w (required for git-based registry resolution)", url, ref, err)
	}

	// Clone the repo into the checkout directory.
	if err := os.MkdirAll(filepath.Dir(checkoutDir), 0755); err != nil {
		return "", fmt.Errorf("creating cache directory: %w", err)
	}

	// Remove any partial/failed checkout.
	os.RemoveAll(checkoutDir)

	// Clone and checkout at the specific ref.
	cmd := exec.Command(gitPath, "clone", "--no-checkout", url, checkoutDir)
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git clone %s at ref %s failed: %w\n%s", url, ref, err, strings.TrimSpace(string(output)))
	}

	cmd = exec.Command(gitPath, "checkout", ref)
	cmd.Dir = checkoutDir
	if output, err := cmd.CombinedOutput(); err != nil {
		// Clean up failed checkout.
		os.RemoveAll(checkoutDir)
		return "", fmt.Errorf("git checkout %s at ref %s failed: %w\n%s", url, ref, err, strings.TrimSpace(string(output)))
	}

	return checkoutDir, nil
}

// isValidCheckout returns true if the directory exists and has a .git directory
// (indicating a successful previous checkout).
func isValidCheckout(dir, ref string) bool {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	if err != nil {
		return false
	}
	if !info.IsDir() {
		return false
	}

	// Verify the HEAD matches the expected ref.
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	head := strings.TrimSpace(string(output))
	// For full SHA refs, require exact match. For short refs or branch names,
	// check prefix match.
	return strings.HasPrefix(head, ref) || head == ref
}

// hashURL returns a deterministic, filesystem-safe hash of a URL.
func hashURL(url string) string {
	h := sha256.Sum256([]byte(url))
	return fmt.Sprintf("%x", h[:8]) // 16 hex chars — enough to avoid collisions
}
