package appbuild

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"go/version"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/jsell-rh/stego/internal/buildidentity"
	"github.com/jsell-rh/stego/internal/compiler"
	"golang.org/x/mod/modfile"
)

const GoVersion = "go1.26.11"

var sdkIdentity = Inventory{"70f525efa7ced94a0e551446237611a4185c8fba26433f67dd1be13894f30c84", 15048, 232821816}
var revisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

type Options struct {
	Source      string
	Revision    string
	Module      string
	Target      string
	Go          string
	Work        string
	Output      string
	ModuleCache string
}

type Artifact struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}
type Module struct {
	Path        string  `json:"path"`
	Version     string  `json:"version,omitempty"`
	Sum         string  `json:"sum,omitempty"`
	GoModSum    string  `json:"go_mod_sum,omitempty"`
	Replacement *Module `json:"replacement,omitempty"`
	LocalPath   string  `json:"local_path,omitempty"`
	// Source names the common registry's runtime folder of the source
	// revision when it supplied the module (ST-BLD-UC02-R05); it is empty
	// when the public module proxy did.
	Source string `json:"source,omitempty"`
}
type Record struct {
	Format                int                  `json:"format"`
	SourceRevision        string               `json:"source_revision"`
	Source                Inventory            `json:"source"`
	Inputs                []File               `json:"inputs"`
	Module                string               `json:"module"`
	Target                string               `json:"target"`
	GenerationStateSHA256 string               `json:"generation_state_sha256"`
	GenerationCompiler    buildidentity.Record `json:"generation_compiler"`
	BuildCompiler         buildidentity.Record `json:"build_compiler"`
	BuildCompilerArtifact Artifact             `json:"build_compiler_artifact"`
	Toolchain             Inventory            `json:"toolchain"`
	GoVersion             string               `json:"go_version"`
	GitSHA256             string               `json:"git_sha256"`
	Environment           map[string]string    `json:"environment"`
	DependencyProxy       string               `json:"dependency_proxy"`
	ModuleCache           *Inventory           `json:"module_cache,omitempty"`
	GoTelemetry           string               `json:"go_telemetry"`
	BuildFlags            []string             `json:"build_flags"`
	Modules               []Module             `json:"modules"`
	BinarySettings        map[string]string    `json:"binary_settings"`
	Artifact              Artifact             `json:"artifact"`
	SBOMs                 []string             `json:"sboms"`
	IndependentBuilds     int                  `json:"independent_builds"`
}

// recordSBOMs names the SBOM files a build writes beside its record, in the
// fixed order SPDX, then CycloneDX. A record cannot hold their digests, since
// the SBOMs are written from the record; SHA256SUMS holds them.
func recordSBOMs() []string {
	names := make([]string, 0, len(sbomFormats))
	for _, format := range sbomFormats {
		names = append(names, format.FileName())
	}
	return names
}

// fixedEnvironment is the environment of every build step: the target settings that the fill check shares
// (compiler.TargetBuildSettings) and the settings of the build alone.
func fixedEnvironment() map[string]string {
	values := map[string]string{
		"GOFLAGS": "", "GOCACHEPROG": "",
		"GOPROXY": "https://proxy.golang.org", "GOSUMDB": "sum.golang.org",
		"GOPRIVATE": "", "GONOPROXY": "", "GONOSUMDB": "", "GOINSECURE": "",
		"GOAUTH": "off", "GOVCS": "*:off", "GOMAXPROCS": "2", "GOMEMLIMIT": "1536MiB",
		"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_COUNT": "0",
		"GIT_TERMINAL_PROMPT": "0", "GIT_NO_REPLACE_OBJECTS": "1",
		"LANG": "C", "LC_ALL": "C", "TZ": "UTC",
	}
	for key, value := range compiler.TargetBuildSettings() {
		values[key] = value
	}
	return values
}

func environment(root, sdk string, offline bool) []string {
	values := fixedEnvironment()
	values["PATH"] = filepath.Join(sdk, "bin") + ":/usr/bin:/bin"
	values["GOROOT"] = sdk
	for key, name := range map[string]string{"HOME": "home", "GOPATH": "gopath", "GOMODCACHE": "modules", "GOCACHE": "cache", "TMPDIR": "tmp"} {
		values[key] = filepath.Join(root, name)
	}
	if offline {
		values["GOPROXY"] = "off"
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

// offlineCache enforces ST-BLD-INV04: a build runs with the module proxy off,
// from the module cache that stego build download recorded for the same
// revision, module and target. It refuses a cache whose zips differ from its
// record, or lack one the record lists; the Go toolchain then refuses a module
// the cache does not hold, or whose zip differs from go.sum.
func offlineCache(o Options) (*ModuleCacheRecord, error) {
	if o.ModuleCache == "" {
		return nil, errors.New("build requires --module-cache: a module cache that stego build download recorded; a build never downloads modules")
	}
	cache, err := loadModuleCache(o.ModuleCache)
	if err != nil {
		return nil, fmt.Errorf("module cache %s: %w", o.ModuleCache, err)
	}
	if cache.SourceRevision != o.Revision || cache.Module != o.Module || cache.Target != o.Target || cache.GoVersion != GoVersion {
		return nil, fmt.Errorf("the module cache %s serves revision %s, module %s and target %s, not the build's", o.ModuleCache, cache.SourceRevision, cache.Module, cache.Target)
	}
	return cache, nil
}

// checkSDK refuses a Go SDK whose contents differ from the pinned official
// release, and names the inventory it found and the one it expected.
func checkSDK(sdk string) (Inventory, error) {
	toolchain, _, err := inventory(sdk, sdkIdentity.Files, sdkIdentity.Bytes)
	if err != nil {
		return Inventory{}, fmt.Errorf("Go SDK %s is not the pinned official release: its inventory cannot be read (%v); expected files=%d bytes=%d SHA-256=%s", sdk, err, sdkIdentity.Files, sdkIdentity.Bytes, sdkIdentity.SHA256)
	}
	if toolchain != sdkIdentity {
		return Inventory{}, fmt.Errorf("Go SDK %s is not the pinned official release: found files=%d bytes=%d SHA-256=%s; expected files=%d bytes=%d SHA-256=%s", sdk, toolchain.Files, toolchain.Bytes, toolchain.SHA256, sdkIdentity.Files, sdkIdentity.Bytes, sdkIdentity.SHA256)
	}
	return toolchain, nil
}

func outside(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && (relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

// shownSource is a --source value as a refusal may quote it: the user info of a URL, which can hold a token, is
// replaced with xxxxx, and the rest is kept, so the author still sees which value was refused.
func shownSource(source string) string {
	scheme, rest, ok := strings.Cut(source, "://")
	if !ok {
		return source
	}
	authority, tail := rest, ""
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		authority, tail = rest[:i], rest[i:]
	}
	if i := strings.LastIndex(authority, "@"); i >= 0 {
		authority = "xxxxx" + authority[i:]
	}
	return scheme + "://" + authority + tail
}

// checkLocation checks the source, revision, module and target of a download or a build, and gives each check a
// refusal that names the flag, what it found and what it expects (ST-BLD-UC02-R04). A target or a module that starts
// with ./, as go build takes ./cmd/server, is taken as the same path without it, and a --source that is a file://
// URL is taken as its local path; a URL of another scheme is refused, since the source is a local repository.
func checkLocation(source, revision, module, target string) (string, string, string, error) {
	if rest, ok := strings.CutPrefix(source, "file://"); ok {
		if !strings.HasPrefix(rest, "/") {
			return "", "", "", fmt.Errorf("--source %q: expected a file URL with an absolute path and no host, such as file:///home/me/service", shownSource(source))
		}
		source = rest
	} else if scheme, _, ok := strings.Cut(source, "://"); ok && !strings.ContainsAny(scheme, "/") {
		return "", "", "", fmt.Errorf("--source %q: expected a local Git repository folder or a file:// URL, not a %s URL", shownSource(source), scheme)
	}
	if source != "" {
		source = filepath.Clean(source)
	}
	if !revisionPattern.MatchString(revision) {
		return "", "", "", fmt.Errorf("--revision %q: expected a full commit ID of 40 lower-case hexadecimal digits", revision)
	}
	module = strings.TrimPrefix(module, "./")
	if module == "" {
		module = "."
	}
	if module != "." && !safePath(module) {
		return "", "", "", fmt.Errorf("--module %q: expected . or a clean relative path in the source with no .. or .git part", module)
	}
	target = strings.TrimPrefix(target, "./")
	if !safePath(target) {
		return "", "", "", fmt.Errorf("--target %q: expected a clean relative path of the program in the module, such as cmd/server or ./cmd/server, with no .. or .git part", target)
	}
	return source, module, target, nil
}

func prepare(options Options) (Options, error) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return options, errors.New("application builds require Linux amd64")
	}
	var err error
	if options.Source, options.Module, options.Target, err = checkLocation(options.Source, options.Revision, options.Module, options.Target); err != nil {
		return options, fmt.Errorf("build: %w", err)
	}
	if options.ModuleCache == "" {
		return options, errors.New("build requires --module-cache: a module cache that stego build download recorded; a build never downloads modules")
	}
	for _, pointer := range []*string{&options.Source, &options.Go, &options.Work, &options.Output, &options.ModuleCache} {
		if *pointer == "" {
			return options, errors.New("build paths are required")
		}
		absolute, err := filepath.Abs(*pointer)
		if err != nil {
			return options, err
		}
		*pointer = absolute
	}
	options.Source, err = filepath.EvalSymlinks(options.Source)
	if err != nil {
		return options, err
	}
	options.Go, err = filepath.EvalSymlinks(options.Go)
	if err != nil {
		return options, err
	}
	options.ModuleCache, err = filepath.EvalSymlinks(options.ModuleCache)
	if err != nil {
		return options, fmt.Errorf("cannot read the module cache %s", options.ModuleCache)
	}
	for _, pointer := range []*string{&options.Work, &options.Output} {
		parent, err := filepath.EvalSymlinks(filepath.Dir(*pointer))
		if err != nil {
			return options, errors.New("build output parents must already exist")
		}
		*pointer = filepath.Join(parent, filepath.Base(*pointer))
		if _, err = os.Lstat(*pointer); !errors.Is(err, os.ErrNotExist) {
			return options, errors.New("build and result directories must not exist")
		}
	}
	sdk := filepath.Dir(filepath.Dir(options.Go))
	if options.Go != filepath.Join(sdk, "bin", "go") || !outside(options.Source, options.Work) || !outside(options.Source, options.Output) || !outside(options.Work, options.Output) || !outside(options.Output, options.Work) || !outside(sdk, options.Work) || !outside(sdk, options.Output) {
		return options, errors.New("build, source, SDK, and result paths overlap")
	}
	if !outside(options.Source, options.ModuleCache) || !outside(options.Work, options.ModuleCache) || !outside(options.Output, options.ModuleCache) || !outside(sdk, options.ModuleCache) {
		return options, errors.New("the module cache overlaps another build path")
	}
	return options, nil
}

func generatedState(module string) (string, buildidentity.Record, error) {
	statePath := filepath.Join(module, ".stego/state.yaml")
	state, err := compiler.LoadState(statePath)
	if err != nil || state.LastApplied == nil || state.LastApplied.Inputs == nil || state.LastApplied.CompilerBuild == nil || len(state.LastApplied.Files) == 0 {
		return "", buildidentity.Record{}, errors.New("application build requires complete generation state")
	}

	_, outputFiles, err := inventory(filepath.Join(module, filepath.FromSlash(state.LastApplied.Inputs.Options.OutputDir)), maxFiles, maxBytes)
	if err != nil || len(outputFiles) != len(state.LastApplied.Files) {
		return "", buildidentity.Record{}, errors.New("generated output file set differs from its recorded state")
	}
	for name, expected := range state.LastApplied.Files {
		if !safePath(name) {
			return "", buildidentity.Record{}, errors.New("invalid generated file path")
		}
		actual, _, err := fileDigest(filepath.Join(module, filepath.FromSlash(state.LastApplied.Inputs.Options.OutputDir), filepath.FromSlash(name)))
		if err != nil || actual != expected {
			return "", buildidentity.Record{}, errors.New("generated file differs from its recorded state")
		}
	}
	for name, expected := range state.LastApplied.Inputs.Files {
		full := filepath.Join(module, filepath.FromSlash(name))
		if !expected.Exists {
			if _, err := os.Lstat(full); !errors.Is(err, os.ErrNotExist) {
				return "", buildidentity.Record{}, errors.New("absent generation input now exists")
			}
			continue
		}
		actual, _, err := fileDigest(full)
		if err != nil || actual != expected.SHA256 {
			return "", buildidentity.Record{}, errors.New("generation input differs from its recorded state")
		}
	}
	hash, _, err := fileDigest(statePath)
	return hash, *state.LastApplied.CompilerBuild, err
}

// Check every local replacement before Go can read it. The full source snapshot
// includes local modules; a replacement must not escape that snapshot.
func checkModules(root, directory string, seen map[string]bool) error {
	if seen[directory] {
		return nil
	}
	seen[directory] = true
	if len(seen) > 256 {
		return errors.New("too many local application modules")
	}
	data, err := os.ReadFile(filepath.Join(directory, "go.mod"))
	if err != nil {
		return err
	}
	m, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return errors.New("invalid application module")
	}
	if m.Go == nil || !version.IsValid("go"+m.Go.Version) || version.Compare("go"+m.Go.Version, GoVersion) > 0 || (m.Toolchain != nil && (!version.IsValid(m.Toolchain.Name) || version.Compare(m.Toolchain.Name, GoVersion) > 0)) {
		return errors.New("application module requires the selected Go release")
	}
	for _, r := range m.Replace {
		if r.New.Version != "" {
			continue
		}
		if filepath.IsAbs(r.New.Path) || strings.ContainsAny(r.New.Path, "\\:") {
			return errors.New("local module replacement must stay in the source snapshot")
		}
		replacement := filepath.Clean(filepath.Join(directory, filepath.FromSlash(r.New.Path)))
		if replacement != root && outside(root, replacement) {
			return errors.New("local module replacement escapes the source snapshot")
		}
		if err := checkModules(root, replacement, seen); err != nil {
			return err
		}
	}
	return nil
}

type goModule struct {
	Path, Version, Sum, GoModSum, Dir string
	Main                              bool
	Replace                           *goModule
	Error                             json.RawMessage
}

// recordModuleSources names, in the build record, the module that the cache
// record says the common registry's runtime folder supplied
// (ST-BLD-UC02-R05). It matches path, version and hash, and only a remote
// module that nothing replaces.
func recordModuleSources(modules []Module, cache *ModuleCacheRecord) {
	for index := range modules {
		current := &modules[index]
		if current.Replacement != nil || current.LocalPath != "" {
			continue
		}
		for _, cached := range cache.Modules {
			if runtimeSource(cached.Source) && cached.Path == current.Path && cached.Version == current.Version && cached.Sum == current.Sum {
				current.Source = cached.Source
			}
		}
	}
}

func moduleInventory(data []byte, source string) ([]Module, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var result []Module
	seen := map[string]bool{}
	var convert func(goModule) (Module, error)
	convert = func(value goModule) (Module, error) {
		if (len(value.Error) > 0 && !bytes.Equal(value.Error, []byte("null"))) || value.Path == "" {
			return Module{}, errors.New("incomplete application module record")
		}
		r := Module{Path: value.Path, Version: value.Version, Sum: value.Sum, GoModSum: value.GoModSum}
		if value.Replace != nil {
			if value.Replace.Replace != nil {
				return Module{}, errors.New("nested module replacement")
			}
			x, err := convert(*value.Replace)
			if err != nil {
				return Module{}, err
			}
			r.Replacement = &x
			return r, nil
		}
		if value.Version == "" {
			rel, err := filepath.Rel(source, value.Dir)
			if err != nil || (rel != "." && !safePath(filepath.ToSlash(rel))) {
				return Module{}, errors.New("module directory escapes the source snapshot")
			}
			r.LocalPath = filepath.ToSlash(rel)
		} else {
			if !validModuleSum(value.GoModSum) {
				return Module{}, errors.New("module metadata checksum is missing")
			}
			if !validModuleSum(value.Sum) {
				return Module{}, errors.New("module content checksum is missing")
			}
		}
		return r, nil
	}
	for {
		var value goModule
		err := decoder.Decode(&value)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("invalid module inventory")
		}
		if len(result) >= 4096 || seen[value.Path] {
			return nil, errors.New("module inventory is too large or has repeated paths")
		}
		seen[value.Path] = true
		r, err := convert(value)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	if len(result) == 0 {
		return nil, errors.New("empty module inventory")
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
}

func binarySettings(name string) (map[string]string, error) {
	info, err := buildinfo.ReadFile(name)
	if err != nil || info.GoVersion != GoVersion {
		return nil, errors.New("application executable has an unexpected Go build record")
	}
	settings := map[string]string{}
	for _, entry := range info.Settings {
		if _, ok := settings[entry.Key]; ok {
			return nil, errors.New("repeated application build setting")
		}
		settings[entry.Key] = entry.Value
	}
	for key, value := range map[string]string{"-buildmode": "exe", "-compiler": "gc", "-trimpath": "true", "CGO_ENABLED": "0", "GOARCH": "amd64", "GOOS": "linux", "GOAMD64": "v1"} {
		if settings[key] != value {
			return nil, errors.New("application executable has unexpected build settings")
		}
	}
	for _, key := range []string{"-ldflags", "-gcflags", "-asmflags", "-tags", "-race", "-msan", "-asan", "-cover", "-covermode", "vcs.revision", "vcs.modified"} {
		if _, ok := settings[key]; ok {
			return nil, errors.New("application executable has forbidden build settings")
		}
	}
	return settings, nil
}

// Build makes two fresh source trees, module caches, and build caches. It never
// executes the application. The caller supplies CI isolation and record signing.
func Build(ctx context.Context, options Options) (*Record, error) {
	if ctx == nil {
		return nil, errors.New("application build requires a context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	o, err := prepare(options)
	if err != nil {
		return nil, err
	}
	sdk := filepath.Dir(filepath.Dir(o.Go))
	toolchain, err := checkSDK(sdk)
	if err != nil {
		return nil, err
	}
	git := "/usr/bin/git"
	gitHash, _, err := fileDigest(git)
	if err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	compilerHash, compilerSize, err := fileDigest(executable)
	if err != nil {
		return nil, err
	}
	cache, err := offlineCache(o)
	if err != nil {
		return nil, err
	}
	if err := os.Mkdir(o.Work, 0700); err != nil {
		return nil, err
	}
	var result *Record
	var firstBinary string
	for index, label := range []string{"first", "second-with-a-different-path"} {
		root := filepath.Join(o.Work, label)
		for _, name := range []string{"source", "home", "tmp", "modules", "cache", "gopath"} {
			if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
				return nil, err
			}
		}
		// Each round extracts again from the recorded download cache.
		// Extraction checks every zip against go.sum, so the copy only
		// moves verified data between private directories.
		if err := copyTree(filepath.Join(o.ModuleCache, "cache", "download"), filepath.Join(root, "modules", "cache", "download")); err != nil {
			return nil, fmt.Errorf("module cache copy: %w", err)
		}
		env := environment(root, sdk, true)
		snapshot := filepath.Join(root, "source")

		tree, err := capture(ctx, o.Source, env, git, "ls-tree", "-r", "-z", o.Revision)
		if err != nil {
			return nil, err
		}
		entries, err := sourceTree(tree)
		if err != nil {
			return nil, err
		}
		archive, err := os.OpenFile(filepath.Join(root, "source.tar"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		err = command(ctx, o.Source, env, archive, maxBytes+(32<<20), git, "archive", "--format=tar", o.Revision)
		if err == nil {
			_, err = archive.Seek(0, io.SeekStart)
		}
		if err == nil {
			err = unpack(archive, snapshot)
		}
		closeErr := archive.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		inputs, files, err := inventory(snapshot, maxFiles, maxBytes)
		if err != nil {
			return nil, err
		}
		if err := verifySourceTree(snapshot, entries, files); err != nil {
			return nil, err
		}
		module := filepath.Join(snapshot, filepath.FromSlash(o.Module))
		stateHash, generator, err := generatedState(module)
		if err != nil {
			return nil, err
		}
		if err = checkModules(snapshot, module, map[string]bool{}); err != nil {
			return nil, err
		}
		if _, err = capture(ctx, module, env, o.Go, "telemetry", "off"); err != nil {
			return nil, fmt.Errorf("Go telemetry setup: %w", err)
		}
		mode, err := capture(ctx, module, env, o.Go, "env", "GOTELEMETRY")
		if err != nil || strings.TrimSpace(string(mode)) != "off" {
			return nil, errors.New("Go telemetry is not disabled")
		}
		// An explicit "all" request can add sums for pruned modules. Default
		// download fills the cache without changing the recorded go.sum. With
		// a recorded module cache this same command runs with GOPROXY=off: it
		// proves the cache holds every module, and the toolchain extracts
		// each zip again under go.sum enforcement.
		if _, err = capture(ctx, module, env, o.Go, "mod", "download"); err != nil {
			return nil, fmt.Errorf("module download: %w", err)
		}
		env = environment(root, sdk, true)
		if _, err = capture(ctx, module, env, o.Go, "mod", "verify"); err != nil {
			return nil, fmt.Errorf("module content verification: %w", err)
		}
		binary := filepath.Join(root, "application")
		flags := []string{"build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-pgo=off", "-p=2", "-o", binary, "./" + o.Target}
		if _, err = capture(ctx, module, env, append([]string{o.Go}, flags...)...); err != nil {
			return nil, fmt.Errorf("application compilation: %w", err)
		}
		if _, err = capture(ctx, module, env, o.Go, "mod", "verify"); err != nil {
			return nil, fmt.Errorf("module content verification: %w", err)
		}
		selected, err := compiledModulePaths(binary)
		if err != nil {
			return nil, err
		}
		query := append([]string{o.Go, "list", "-mod=readonly", "-m", "-json", "--"}, selected...)
		moduleJSON, err := capture(ctx, module, env, query...)
		if err != nil {
			return nil, fmt.Errorf("compiled module inventory: %w", err)
		}
		modules, err := moduleInventory(moduleJSON, snapshot)
		if err != nil {
			return nil, err
		}
		recordModuleSources(modules, cache)
		settings, err := binarySettings(binary)
		if err != nil {
			return nil, err
		}
		hash, size, err := fileDigest(binary)
		if err != nil {
			return nil, err
		}
		after, afterFiles, err := inventory(snapshot, maxFiles, maxBytes)
		if err != nil || after != inputs {
			if err == nil {
				_ = saveSourceChange(root, files, afterFiles)
			}
			return nil, errors.New("application source changed during the build")
		}
		flags[7] = "<artifact>"
		proxy := "off"
		recorded := cache.DownloadCache
		cacheInventory := &recorded
		record := &Record{Format: 1, SourceRevision: o.Revision, Source: inputs, Inputs: files, Module: o.Module, Target: o.Target, GenerationStateSHA256: stateHash, GenerationCompiler: generator, BuildCompiler: buildidentity.Current(), BuildCompilerArtifact: Artifact{compilerHash, compilerSize}, Toolchain: toolchain, GoVersion: GoVersion, GitSHA256: gitHash, Environment: buildEnvironmentRecord(), DependencyProxy: proxy, ModuleCache: cacheInventory, GoTelemetry: "off", BuildFlags: flags, Modules: modules, BinarySettings: settings, Artifact: Artifact{hash, size}, SBOMs: recordSBOMs(), IndependentBuilds: 2}
		if err := checkCompiledModules(binary, record); err != nil {
			return nil, err
		}
		if index == 0 {
			result = record
			firstBinary = binary
		} else if !reflect.DeepEqual(result, record) {
			return nil, errors.New("independent application builds differ")
		}
	}
	afterSDK, _, err := inventory(sdk, sdkIdentity.Files, sdkIdentity.Bytes)
	if err != nil || afterSDK != toolchain {
		return nil, errors.New("Go SDK changed during the build")
	}
	afterGit, _, err := fileDigest(git)
	if err != nil || afterGit != gitHash {
		return nil, errors.New("Git executable changed during the build")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, saveResult(ctx, o.Output, firstBinary, result)
}

// saveResult writes the executable, build.json and SHA256SUMS into the new
// result folder. A failure removes the folder, so a partial result is never
// left as a result (ST-BLD-INV15).
func saveResult(ctx context.Context, output, firstBinary string, result *Record) (err error) {
	if err := os.Mkdir(output, 0700); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(output)
		}
	}()
	input, err := os.Open(firstBinary)
	if err != nil {
		return err
	}
	defer input.Close()
	file, err := os.OpenFile(filepath.Join(output, "application"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0500)
	if err != nil {
		return err
	}
	_, err = io.Copy(file, input)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	hash, size, err := fileDigest(filepath.Join(output, "application"))
	if err != nil || (Artifact{hash, size}) != result.Artifact {
		return errors.New("saved application artifact differs")
	}
	if err := validateRecord(result); err != nil {
		return err
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > 16<<20 {
		return errors.New("application build record exceeds its size limit")
	}
	if err := os.WriteFile(filepath.Join(output, "build.json"), data, 0600); err != nil {
		return err
	}
	// Write both SBOMs from the record alone (ST-BLD-DS01, ST-BLD-CMD03).
	sboms, err := writeSBOMFiles(output, result)
	if err != nil {
		return fmt.Errorf("SBOMs: %w", err)
	}
	checksums := fmt.Sprintf("%s  application\n%s  build.json\n", hash, digest(data))
	for _, sbom := range sboms {
		checksums += fmt.Sprintf("%s  %s\n", sbom.Digest, sbom.Format.FileName())
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(output, "SHA256SUMS"), []byte(checksums), 0600); err != nil {
		return err
	}
	return nil
}

func saveSourceChange(root string, before, after []File) error {
	left := map[string]File{}
	right := map[string]File{}
	for _, file := range before {
		left[file.Path] = file
	}
	for _, file := range after {
		right[file.Path] = file
	}
	changed := map[string]map[string]*File{}
	for name, file := range left {
		if other, present := right[name]; !present || other != file {
			entry := map[string]*File{"before": &file}
			if present {
				entry["after"] = &other
			}
			changed[name] = entry
		}
	}
	for name, file := range right {
		if _, present := left[name]; !present {
			changed[name] = map[string]*File{"after": &file}
		}
	}
	data, err := json.MarshalIndent(changed, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "source-change.json"), append(data, '\n'), 0600)
}

func validModuleSum(value string) bool {
	if !strings.HasPrefix(value, "h1:") {
		return false
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(value[3:])
	return err == nil && len(raw) == 32 && base64.StdEncoding.EncodeToString(raw) == value[3:]
}

func buildEnvironmentRecord() map[string]string {
	result := fixedEnvironment()
	result["GOPROXY"] = "off"
	return result
}
