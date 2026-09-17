package registry

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jsell-rh/stego/internal/types"
)

// ResolveResult contains the resolved registry directory and metadata.
type ResolveResult struct {
	// Dir is the local filesystem path to the registry directory.
	Dir string
	// Dirs contains all resolved sources, in declaration order.
	Dirs []string
	// Ref is the registry ref from config.yaml (recorded in state).
	Ref string
	// EnvOverride is true when STEGO_REGISTRY was used.
	EnvOverride bool
}

// ResolveOptions controls registry resolution behavior.
type ResolveOptions struct {
	// ProjectDir is the project root (where .stego/ lives).
	ProjectDir string
	// ConfigData supplies captured configuration bytes. Nil reads config.yaml.
	ConfigData []byte
	// Stderr receives warning messages (e.g. STEGO_REGISTRY override).
	// If nil, os.Stderr is used.
	Stderr io.Writer
	// CacheDir overrides stego/registries in the operating system's user cache.
	// Used for testing.
	CacheDir string
}

// ResolveRegistry determines the registry directory from config and environment.
//
// Resolution order:
//  1. STEGO_REGISTRY env var — overrides everything, prints a warning to stderr.
//  2. All .stego/config.yaml registry sources — local paths or pinned Git URLs.
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
			Dirs:        []string{envReg},
			Ref:         "env-override",
			EnvOverride: true,
		}, nil
	}

	// 2. Load config.yaml.
	configPath := filepath.Join(opts.ProjectDir, ".stego", "config.yaml")
	var cfg *types.RegistryConfig
	var err error
	if opts.ConfigData == nil {
		cfg, err = LoadConfig(configPath)
	} else {
		cfg, err = ParseConfig(opts.ConfigData, configPath)
	}
	if err != nil {
		return nil, fmt.Errorf("resolving registry: %w", err)
	}

	if len(cfg.Registry) == 0 {
		return nil, fmt.Errorf("resolving registry: no registry sources in %s", configPath)
	}

	result := &ResolveResult{}
	for i, src := range cfg.Registry {
		local := src.URL
		if !filepath.IsAbs(local) {
			local = filepath.Join(opts.ProjectDir, local)
		}
		var dir string
		if src.Vendor != "" {
			dir, err = sourceSubdirectory(opts.ProjectDir, src.Vendor)
			if err != nil {
				return nil, fmt.Errorf("registry[%d]: vendor: %w", i, err)
			}
			if !isValidCheckout(dir, src.Ref) {
				return nil, fmt.Errorf("registry[%d]: vendored checkout differs from the pinned commit", i)
			}
		} else if !strings.Contains(src.URL, "://") && !strings.HasPrefix(src.URL, "git@") && isLocalDir(local) {
			dir = local
		} else {
			dir, err = resolveGitRegistry(src.URL, src.Ref, opts.CacheDir)
			if err != nil {
				return nil, err
			}
		}
		if src.Path != "" {
			dir, err = sourceSubdirectory(dir, src.Path)
			if err != nil {
				return nil, fmt.Errorf("registry[%d]: path: %w", i, err)
			}
		}
		result.Dirs = append(result.Dirs, dir)
	}
	result.Dir = result.Dirs[0]
	result.Ref = cfg.Registry[0].Ref
	if len(cfg.Registry) > 1 {
		encoded, _ := json.Marshal(cfg.Registry)
		result.Ref = fmt.Sprintf("%x", sha256.Sum256(encoded))
	}
	return result, nil
}

// Read each path segment under its declared root. A symbolic link cannot
// redirect a registry subdirectory or an explicit vendored checkout.
func sourceSubdirectory(base, relative string) (string, error) {
	root, err := os.OpenRoot(base)
	if err != nil {
		return "", err
	}
	defer root.Close()
	current := ""
	for _, part := range strings.Split(relative, "/") {
		current = filepath.Join(current, part)
		info, err := root.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("path must contain only directories without symbolic links")
		}
	}
	return filepath.Join(base, filepath.FromSlash(relative)), nil
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
var commitID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func resolveGitRegistry(url, ref, cacheDir string) (resolved string, resultErr error) {
	if !commitID.MatchString(ref) {
		return "", fmt.Errorf("registry ref %q must be a full lowercase Git commit SHA", ref)
	}
	if cacheDir == "" {
		userCache, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("determining cache directory: %w", err)
		}
		cacheDir = filepath.Join(userCache, "stego", "registries")
	}

	urlHash := hashURL(url)
	checkoutDir := filepath.Join(cacheDir, urlHash, ref)
	if info, err := os.Lstat(filepath.Dir(checkoutDir)); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return "", fmt.Errorf("registry cache parent must be a directory without symbolic links")
	} else if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("checking registry cache: %w", err)
	}

	// If cached checkout exists and is valid, reuse it.
	if isValidCheckout(checkoutDir, ref) {
		return checkoutDir, nil
	}
	if _, err := os.Lstat(checkoutDir); err == nil {
		return "", fmt.Errorf("registry cache %s is modified or incomplete; restore the pinned checkout before retrying", checkoutDir)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("checking registry cache: %w", err)
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

	// Prepare a private checkout. Never remove an existing cache entry.
	temporary, err := os.MkdirTemp(filepath.Dir(checkoutDir), ".checkout-")
	if err != nil {
		return "", fmt.Errorf("creating temporary registry checkout: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(temporary); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("removing temporary registry checkout: %w", err))
		}
	}()

	// Clone and checkout at the specific ref.
	cmd := exec.Command(gitPath, "clone", "--no-checkout", "--", url, temporary)
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git clone %s at ref %s failed: %w\n%s", url, ref, err, strings.TrimSpace(string(output)))
	}

	cmd = exec.Command(gitPath, "checkout", "--detach", ref, "--")
	cmd.Dir = temporary
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git checkout %s at ref %s failed: %w\n%s", url, ref, err, strings.TrimSpace(string(output)))
	}
	if !isValidCheckout(temporary, ref) {
		return "", fmt.Errorf("registry checkout does not match the pinned commit %s", ref)
	}
	if err := os.Rename(temporary, checkoutDir); err != nil {
		// Another process can publish the same immutable checkout first.
		if isValidCheckout(checkoutDir, ref) {
			return checkoutDir, nil
		}
		return "", fmt.Errorf("publishing registry checkout: %w", err)
	}

	return checkoutDir, nil
}

// isValidCheckout returns true if the directory exists and has a .git directory
// (indicating a successful previous checkout).
func isValidCheckout(dir, ref string) bool {
	for _, path := range []string{dir, filepath.Join(dir, ".git")} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}

	// Verify the HEAD matches the expected ref.
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	head := strings.TrimSpace(string(output))
	if head != ref {
		return false
	}
	cmd = exec.Command("git", "status", "--porcelain", "--untracked-files=all", "--ignored")
	cmd.Dir = dir
	output, err = cmd.Output()
	return err == nil && strings.TrimSpace(string(output)) == ""
}

// hashURL returns a deterministic, filesystem-safe hash of a URL.
func hashURL(url string) string {
	h := sha256.Sum256([]byte(url))
	return fmt.Sprintf("%x", h)
}
