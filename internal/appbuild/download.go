package appbuild

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/module"
	"golang.org/x/mod/sumdb/dirhash"
)

// ModuleCacheRecord describes the download cache produced by Download. The
// cache holds only the download cache (zips) that the Go toolchain extracts
// again during each offline build. It never holds extracted module sources,
// because the toolchain trusts those without checking go.sum.
type ModuleCacheRecord struct {
	Format          int            `json:"format"`
	SourceRevision  string         `json:"source_revision"`
	Module          string         `json:"module"`
	Target          string         `json:"target"`
	GoVersion       string         `json:"go_version"`
	GitSHA256       string         `json:"git_sha256"`
	DownloadCache   Inventory      `json:"download_cache"`
	Modules         []CachedModule `json:"modules"`
	DependencyProxy string         `json:"dependency_proxy"`
}

// CachedModule is one module zip in a module cache: its path, its version and
// the hash that go.sum states for it, which its bytes match.
type CachedModule struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	Sum     string `json:"sum"`
}

// moduleCacheFormat is the format of module-cache.json. Format 2 lists each
// module zip by path, version and hash.
const moduleCacheFormat = 2

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
	if _, err := checkSDK(sdk); err != nil {
		return nil, err
	}
	git := "/usr/bin/git"
	gitHash, _, err := fileDigest(git)
	if err != nil {
		return nil, err
	}
	// Use a private tree for the network phase so the recorded cache only
	// holds the download cache, never any toolchain state. The result is
	// moved to the output folder only after every check passes, and the
	// private tree is removed on every path, so a failed or interrupted
	// download leaves no cache folder and no record.
	root, err := os.MkdirTemp(filepath.Dir(o.Output), ".download-")
	if err != nil {
		return nil, err
	}
	defer removeTree(root)
	for _, name := range []string{"source", "home", "tmp", "modules", "cache", "gopath", "single"} {
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
	sums, err := readSums(filepath.Join(module, "go.sum"))
	if err != nil {
		return nil, err
	}
	// Download every module with the fixed environment. The Go toolchain
	// checks every zip against the sum database and go.sum.
	if _, err = capture(ctx, module, env, o.Go, "mod", "download"); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		single := environment(filepath.Join(root, "single"), sdk, false)
		return nil, diagnoseDownload(ctx, filepath.Join(root, "single"), single, o.Go, sums)
	}
	cacheRoot := filepath.Join(root, "modules")
	modules, err := checkCachedModules(filepath.Join(cacheRoot, "cache", "download"), sums)
	if err != nil {
		return nil, err
	}
	download, _, err := inventory(filepath.Join(cacheRoot, "cache", "download"), maxFiles, maxBytes)
	if err != nil {
		return nil, err
	}
	if download.Files == 0 {
		return nil, errors.New("module download produced no cache")
	}
	record := &ModuleCacheRecord{Format: moduleCacheFormat, SourceRevision: o.Revision, Module: o.Module, Target: o.Target, GoVersion: GoVersion, GitSHA256: gitHash, DownloadCache: download, Modules: modules, DependencyProxy: "https://proxy.golang.org"}
	if err := validateModuleCacheRecord(record); err != nil {
		return nil, err
	}
	result := filepath.Join(root, "result")
	if err := copyTree(filepath.Join(cacheRoot, "cache", "download"), filepath.Join(result, "cache", "download")); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(result, "module-cache.json"), data, 0600); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := publishCache(result, o.Output); err != nil {
		return nil, err
	}
	return record, nil
}

// publishCache moves a complete cache into the new output folder. It creates
// the folder exclusively, moves the cache in, and writes the record last, so
// a folder with a record always holds its whole cache. A failure removes the
// folder it created.
func publishCache(result, output string) (err error) {
	if err := os.Mkdir(output, 0700); err != nil {
		return fmt.Errorf("module cache target %s: %w", output, err)
	}
	defer func() {
		if err != nil {
			_ = removeTree(output)
		}
	}()
	if err := os.Rename(filepath.Join(result, "cache"), filepath.Join(output, "cache")); err != nil {
		return err
	}
	return os.Rename(filepath.Join(result, "module-cache.json"), filepath.Join(output, "module-cache.json"))
}

// removeTree removes a folder that can hold the read-only module sources Go
// extracts. It makes each folder writable first.
func removeTree(root string) error {
	_ = filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err == nil && entry.IsDir() && entry.Type()&fs.ModeSymlink == 0 {
			_ = os.Chmod(name, 0700)
		}
		return nil
	})
	return os.RemoveAll(root)
}

// moduleVersion is one module of go.sum: its path and its version.
type moduleVersion struct{ path, version string }

// readSums reads the hashes of the module zips that go.sum states. Lines for
// go.mod files are not zips and are skipped.
func readSums(name string) (map[moduleVersion]string, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("cannot read the module requirements' checksums %s", filepath.Base(name))
	}
	sums := map[moduleVersion]string{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 3 || !validModuleSum(fields[2]) {
			return nil, errors.New("go.sum holds a malformed line")
		}
		if strings.HasSuffix(fields[1], "/go.mod") {
			continue
		}
		key := moduleVersion{fields[0], fields[1]}
		if previous, ok := sums[key]; ok && previous != fields[2] {
			return nil, fmt.Errorf("go.sum states two hashes for %s %s", key.path, key.version)
		}
		sums[key] = fields[2]
	}
	return sums, scanner.Err()
}

// checkCachedModules enforces ST-BLD-INV03: every module zip in the download
// cache must have the hash that go.sum states for it. It refuses a zip that
// go.sum does not state and a zip whose hash differs, and names the module,
// its version, the stated hash and the found hash. It returns the zips in
// path and version order.
func checkCachedModules(download string, sums map[moduleVersion]string) ([]CachedModule, error) {
	var modules []CachedModule
	err := filepath.WalkDir(download, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".zip") {
			return nil
		}
		relative, err := filepath.Rel(download, name)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		escapedPath, escapedVersion := path.Split(strings.TrimSuffix(relative, ".zip"))
		if !strings.HasSuffix(escapedPath, "/@v/") {
			return fmt.Errorf("module cache holds a zip outside a module version folder: %s", relative)
		}
		modulePath, err := module.UnescapePath(strings.TrimSuffix(escapedPath, "/@v/"))
		if err != nil {
			return fmt.Errorf("module cache holds a zip with an invalid module path: %s", relative)
		}
		version, err := module.UnescapeVersion(escapedVersion)
		if err != nil {
			return fmt.Errorf("module cache holds a zip with an invalid version: %s", relative)
		}
		stated, ok := sums[moduleVersion{modulePath, version}]
		if !ok {
			return fmt.Errorf("module %s %s is in the cache, but go.sum states no hash for it", modulePath, version)
		}
		found, err := dirhash.HashZip(name, dirhash.Hash1)
		if err != nil {
			return fmt.Errorf("module %s %s: the zip cannot be read", modulePath, version)
		}
		if found != stated {
			return fmt.Errorf("module %s %s differs from go.sum: go.sum states %s, the zip is %s", modulePath, version, stated, found)
		}
		modules = append(modules, CachedModule{Path: modulePath, Version: version, Sum: found})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(modules, func(i, j int) bool {
		if modules[i].Path != modules[j].Path {
			return modules[i].Path < modules[j].Path
		}
		return modules[i].Version < modules[j].Version
	})
	return modules, nil
}

// diagnoseDownload finds the module a failed download stopped at. Go withholds
// its diagnostics, so it downloads each module that go.sum states, one at a
// time and outside the service's module, where only the sum database checks
// it, and compares each zip with go.sum. It names the first module that the
// proxy cannot supply or whose hash differs.
func diagnoseDownload(ctx context.Context, root string, env []string, goTool string, sums map[moduleVersion]string) error {
	keys := make([]moduleVersion, 0, len(sums))
	for key := range sums {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].path != keys[j].path {
			return keys[i].path < keys[j].path
		}
		return keys[i].version < keys[j].version
	})
	for _, name := range []string{"home", "tmp", "modules", "cache", "gopath"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
			return err
		}
	}
	for _, key := range keys {
		if _, err := capture(ctx, filepath.Join(root, "tmp"), env, goTool, "mod", "download", "-json", key.path+"@"+key.version); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("module download stopped at %s %s: the module proxy cannot supply it", key.path, key.version)
		}
		escapedPath, err := module.EscapePath(key.path)
		if err != nil {
			return err
		}
		escapedVersion, err := module.EscapeVersion(key.version)
		if err != nil {
			return err
		}
		found, err := dirhash.HashZip(filepath.Join(root, "modules", "cache", "download", filepath.FromSlash(escapedPath), "@v", escapedVersion+".zip"), dirhash.Hash1)
		if err != nil {
			return fmt.Errorf("module download stopped at %s %s: the zip cannot be read", key.path, key.version)
		}
		if found != sums[key] {
			return fmt.Errorf("module download stopped at %s %s: go.sum states %s, the zip is %s", key.path, key.version, sums[key], found)
		}
	}
	return errors.New("module download failed, and each module that go.sum states downloads alone; a module that go.sum does not state is missing")
}

// validateModuleCacheRecord checks the recorded cache policy.
func validateModuleCacheRecord(record *ModuleCacheRecord) error {
	if record.Format != moduleCacheFormat || record.GoVersion != GoVersion || !revisionPattern.MatchString(record.SourceRevision) || !hashPattern.MatchString(record.GitSHA256) || record.DependencyProxy != "https://proxy.golang.org" {
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
	if len(record.Modules) < 1 || len(record.Modules) > 4096 {
		return errors.New("module cache record lists no module, or too many")
	}
	for index, cached := range record.Modules {
		if module.Check(cached.Path, cached.Version) != nil || !validModuleSum(cached.Sum) {
			return fmt.Errorf("module cache record lists an invalid module %s %s", cached.Path, cached.Version)
		}
		if index > 0 {
			previous := record.Modules[index-1]
			if previous.Path > cached.Path || previous.Path == cached.Path && previous.Version >= cached.Version {
				return errors.New("module cache record lists its modules out of order")
			}
		}
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
	sums := make(map[moduleVersion]string, len(record.Modules))
	for _, cached := range record.Modules {
		sums[moduleVersion{cached.Path, cached.Version}] = cached.Sum
	}
	modules, err := checkCachedModules(filepath.Join(root, "cache", "download"), sums)
	if err != nil {
		return nil, err
	}
	if len(modules) != len(record.Modules) {
		return nil, errors.New("module cache lacks a module zip that its record lists")
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
		return options, fmt.Errorf("module cache target %s: its parent folder must already exist", options.Output)
	}
	options.Output = filepath.Join(parent, filepath.Base(options.Output))
	if _, err = os.Lstat(options.Output); !errors.Is(err, os.ErrNotExist) {
		return options, fmt.Errorf("module cache target %s exists; expected a new folder", options.Output)
	}
	sdk := filepath.Dir(filepath.Dir(options.Go))
	if options.Go != filepath.Join(sdk, "bin", "go") {
		return options, fmt.Errorf("--go names %s; expected bin/go of a Go SDK", options.Go)
	}
	if !outside(options.Source, options.Output) {
		return options, fmt.Errorf("module cache target %s is inside the source %s; expected a folder outside it", options.Output, options.Source)
	}
	if !outside(sdk, options.Output) {
		return options, fmt.Errorf("module cache target %s is inside the Go SDK %s; expected a folder outside it", options.Output, sdk)
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
