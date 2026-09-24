package appbuild

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// ModuleCacheRecord describes the download cache produced by Download. The
// cache holds only the download cache (zips) that the Go toolchain extracts
// again during each offline build. It never holds extracted module sources,
// because the toolchain trusts those without checking go.sum.
type ModuleCacheRecord struct {
	Format          int       `json:"format"`
	SourceRevision  string    `json:"source_revision"`
	Module          string    `json:"module"`
	Target          string    `json:"target"`
	GoVersion       string    `json:"go_version"`
	GitSHA256       string    `json:"git_sha256"`
	DownloadCache   Inventory `json:"download_cache"`
	DependencyProxy string    `json:"dependency_proxy"`
}

// DownloadOptions selects the source revision and the new cache directory.
type DownloadOptions struct {
	Source   string
	Revision string
	Module   string
	Target   string
	Go       string
	Output   string
}

// Download fills a new directory with the module download cache for one
// source revision and writes a record that binds the cache contents to that
// revision. The cache directory is suitable for offline builds. The network
// phase uses the same fixed environment as a normal build.
func Download(ctx context.Context, options DownloadOptions) (*ModuleCacheRecord, error) {
	if ctx == nil {
		return nil, errors.New("module cache download requires a context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	o, err := prepareDownload(options)
	if err != nil {
		return nil, err
	}
	sdk := filepath.Dir(filepath.Dir(o.Go))
	toolchain, _, err := inventory(sdk, sdkIdentity.Files, sdkIdentity.Bytes)
	if err != nil {
		return nil, fmt.Errorf("Go SDK inventory inspection failed: %w", err)
	}
	if toolchain != sdkIdentity {
		return nil, fmt.Errorf("Go SDK differs from the pinned official release: files=%d bytes=%d SHA-256=%s", toolchain.Files, toolchain.Bytes, toolchain.SHA256)
	}
	git := "/usr/bin/git"
	gitHash, _, err := fileDigest(git)
	if err != nil {
		return nil, err
	}
	if err := os.Mkdir(o.Output, 0700); err != nil {
		return nil, err
	}
	// Use a private tree for the network phase so the recorded cache only
	// holds the download cache, never any toolchain state.
	root, err := os.MkdirTemp(filepath.Dir(o.Output), ".download-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(root)
	for _, name := range []string{"source", "home", "tmp", "modules", "cache", "gopath"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
			return nil, err
		}
	}
	env := environment(root, sdk, false)
	archive, err := os.OpenFile(filepath.Join(root, "source.tar"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	err = command(ctx, o.Source, env, archive, maxBytes+(32<<20), git, "archive", "--format=tar", o.Revision)
	if err == nil {
		_, err = archive.Seek(0, io.SeekStart)
	}
	if err == nil {
		err = unpack(archive, filepath.Join(root, "source"))
	}
	closeErr := archive.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	snapshot := filepath.Join(root, "source")
	module := filepath.Join(snapshot, filepath.FromSlash(o.Module))
	if err := checkModules(snapshot, module, map[string]bool{}); err != nil {
		return nil, err
	}
	// Download every module with the fixed environment. The Go toolchain
	// checks every zip against the sum database and go.sum.
	if _, err = capture(ctx, module, env, o.Go, "mod", "download"); err != nil {
		return nil, fmt.Errorf("module download: %w", err)
	}
	cacheRoot := filepath.Join(root, "modules")
	download, _, err := inventory(filepath.Join(cacheRoot, "cache", "download"), maxFiles, maxBytes)
	if err != nil {
		return nil, err
	}
	if download.Files == 0 {
		return nil, errors.New("module download produced no cache")
	}
	record := &ModuleCacheRecord{Format: 1, SourceRevision: o.Revision, Module: o.Module, Target: o.Target, GoVersion: GoVersion, GitSHA256: gitHash, DownloadCache: download, DependencyProxy: "https://proxy.golang.org"}
	if err := validateModuleCacheRecord(record); err != nil {
		return nil, err
	}
	if err := copyTree(filepath.Join(cacheRoot, "cache", "download"), filepath.Join(o.Output, "cache", "download")); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(o.Output, "module-cache.json"), data, 0600); err != nil {
		return nil, err
	}
	return record, nil
}

// validateModuleCacheRecord checks the recorded cache policy.
func validateModuleCacheRecord(record *ModuleCacheRecord) error {
	if record.Format != 1 || record.GoVersion != GoVersion || !revisionPattern.MatchString(record.SourceRevision) || !hashPattern.MatchString(record.GitSHA256) || record.DependencyProxy != "https://proxy.golang.org" {
		return errors.New("module cache record violates the build policy")
	}
	if record.Module != "." && !safePath(record.Module) {
		return errors.New("module cache record violates the build policy")
	}
	if !safePath(record.Target) {
		return errors.New("module cache record violates the build policy")
	}
	if record.DownloadCache.Files < 1 || record.DownloadCache.Bytes < 1 || record.DownloadCache.Bytes > maxBytes || !hashPattern.MatchString(record.DownloadCache.SHA256) {
		return errors.New("module cache record violates the build policy")
	}
	return nil
}

// readModuleCacheRecord reads and validates a cache record file.
func readModuleCacheRecord(recordPath string) (*ModuleCacheRecord, error) {
	data, err := os.ReadFile(recordPath)
	if err != nil || len(data) > 16<<20 {
		return nil, errors.New("cannot read the module cache record")
	}
	var record ModuleCacheRecord
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return nil, errors.New("invalid module cache record")
	}
	canonical, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return nil, err
	}
	canonical = append(canonical, '\n')
	if !bytes.Equal(data, canonical) {
		return nil, errors.New("module cache record is not canonical")
	}
	if err := validateModuleCacheRecord(&record); err != nil {
		return nil, err
	}
	return &record, nil
}

// loadModuleCache reads the record from a cache directory and verifies the
// download cache inventory against it.
func loadModuleCache(root string) (*ModuleCacheRecord, error) {
	record, err := readModuleCacheRecord(filepath.Join(root, "module-cache.json"))
	if err != nil {
		return nil, err
	}
	actual, _, err := inventory(filepath.Join(root, "cache", "download"), maxFiles, maxBytes)
	if err != nil {
		return nil, err
	}
	if actual != record.DownloadCache {
		return nil, errors.New("module cache differs from its record")
	}
	return record, nil
}

func prepareDownload(options DownloadOptions) (DownloadOptions, error) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return options, errors.New("module cache download requires Linux amd64")
	}
	if !revisionPattern.MatchString(options.Revision) || (options.Module != "." && !safePath(options.Module)) || !safePath(options.Target) {
		return options, errors.New("module cache download requires a commit, relative module, and relative target")
	}
	for _, pointer := range []*string{&options.Source, &options.Go, &options.Output} {
		if *pointer == "" {
			return options, errors.New("module cache download paths are required")
		}
		absolute, err := filepath.Abs(*pointer)
		if err != nil {
			return options, err
		}
		*pointer = absolute
	}
	var err error
	options.Source, err = filepath.EvalSymlinks(options.Source)
	if err != nil {
		return options, err
	}
	options.Go, err = filepath.EvalSymlinks(options.Go)
	if err != nil {
		return options, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(options.Output))
	if err != nil {
		return options, errors.New("module cache output parent must already exist")
	}
	options.Output = filepath.Join(parent, filepath.Base(options.Output))
	if _, err = os.Lstat(options.Output); !errors.Is(err, os.ErrNotExist) {
		return options, errors.New("module cache output directory must not exist")
	}
	sdk := filepath.Dir(filepath.Dir(options.Go))
	if options.Go != filepath.Join(sdk, "bin", "go") || !outside(options.Source, options.Output) || !outside(sdk, options.Output) {
		return options, errors.New("module cache output overlaps the source or SDK")
	}
	return options, nil
}

// copyTree copies a directory tree as data. It refuses links and special
// files, which also rejects extracted module sources with go.sum unchecked.
func copyTree(source, destination string) error {
	return filepath.WalkDir(source, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, name)
		if err != nil {
			return err
		}
		if relative == "." {
			if err := os.MkdirAll(destination, 0700); err != nil {
				return err
			}
			return nil
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("module cache contains a link or special file")
		}
		input, err := os.Open(name)
		if err != nil {
			return err
		}
		defer input.Close()
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, err = io.Copy(output, input)
		if err == nil {
			err = output.Sync()
		}
		closeErr := output.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
}
