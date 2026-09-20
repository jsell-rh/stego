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

const GoVersion = "go1.26.8"

var sdkIdentity = Inventory{"94168e19a28c7bdeaf3c281f88e3a3efab13f7d80e2694ae2dd4d71378da9289", 15036, 232512886}
var revisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

type Options struct {
	Source   string
	Revision string
	Module   string
	Target   string
	Go       string
	Work     string
	Output   string
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
	GoTelemetry           string               `json:"go_telemetry"`
	BuildFlags            []string             `json:"build_flags"`
	Modules               []Module             `json:"modules"`
	BinarySettings        map[string]string    `json:"binary_settings"`
	Artifact              Artifact             `json:"artifact"`
	IndependentBuilds     int                  `json:"independent_builds"`
}

func fixedEnvironment() map[string]string {
	return map[string]string{
		"GOENV": "off", "GOWORK": "off", "GOFLAGS": "", "GOTOOLCHAIN": "local",
		"GOOS": "linux", "GOARCH": "amd64", "GOAMD64": "v1", "CGO_ENABLED": "0",
		"GOEXPERIMENT": "", "GOFIPS140": "off", "GOCACHEPROG": "",
		"GOPROXY": "https://proxy.golang.org", "GOSUMDB": "sum.golang.org",
		"GOPRIVATE": "", "GONOPROXY": "", "GONOSUMDB": "", "GOINSECURE": "",
		"GOAUTH": "off", "GOVCS": "*:off", "GOMAXPROCS": "2", "GOMEMLIMIT": "1536MiB",
		"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_COUNT": "0",
		"GIT_TERMINAL_PROMPT": "0", "GIT_NO_REPLACE_OBJECTS": "1",
		"LANG": "C", "LC_ALL": "C", "TZ": "UTC",
	}
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

func outside(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && (relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func prepare(options Options) (Options, error) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return options, errors.New("application builds require Linux amd64")
	}
	if !revisionPattern.MatchString(options.Revision) || (options.Module != "." && !safePath(options.Module)) || !safePath(options.Target) {
		return options, errors.New("build requires a commit, relative module, and relative target")
	}
	for _, pointer := range []*string{&options.Source, &options.Go, &options.Work, &options.Output} {
		if *pointer == "" {
			return options, errors.New("build paths are required")
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
		} else if !validModuleSum(value.Sum) || !validModuleSum(value.GoModSum) {
			return Module{}, errors.New("module content checksums are missing")
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
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	o, err := prepare(options)
	if err != nil {
		return nil, err
	}
	sdk := filepath.Dir(filepath.Dir(o.Go))
	toolchain, _, err := inventory(sdk, sdkIdentity.Files, sdkIdentity.Bytes)
	if err != nil || toolchain != sdkIdentity {
		return nil, errors.New("Go SDK differs from the pinned official release")
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
		env := environment(root, sdk, false)
		snapshot := filepath.Join(root, "source")

		tree, err := capture(ctx, o.Source, env, git, "ls-tree", "-r", "-z", o.Revision)
		if err != nil {
			return nil, err
		}
		for _, entry := range bytes.Split(tree, []byte{0}) {
			if bytes.HasPrefix(entry, []byte("160000 ")) {
				return nil, errors.New("application source contains a submodule; use recorded regular source files")
			}
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
		if _, err = capture(ctx, module, env, o.Go, "mod", "download", "all"); err != nil {
			return nil, fmt.Errorf("module download: %w", err)
		}
		env = environment(root, sdk, true)
		if _, err = capture(ctx, module, env, o.Go, "mod", "verify"); err != nil {
			return nil, fmt.Errorf("module content verification: %w", err)
		}
		moduleJSON, err := capture(ctx, module, env, o.Go, "list", "-mod=readonly", "-m", "-json", "all")
		if err != nil {
			return nil, err
		}
		modules, err := moduleInventory(moduleJSON, snapshot)
		if err != nil {
			return nil, err
		}
		binary := filepath.Join(root, "application")
		flags := []string{"build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-pgo=off", "-p=2", "-o", binary, "./" + o.Target}
		if _, err = capture(ctx, module, env, append([]string{o.Go}, flags...)...); err != nil {
			return nil, fmt.Errorf("application compilation: %w", err)
		}
		if _, err = capture(ctx, module, env, o.Go, "mod", "verify"); err != nil {
			return nil, fmt.Errorf("module content verification: %w", err)
		}
		settings, err := binarySettings(binary)
		if err != nil {
			return nil, err
		}
		hash, size, err := fileDigest(binary)
		if err != nil {
			return nil, err
		}
		after, _, err := inventory(snapshot, maxFiles, maxBytes)
		if err != nil || after != inputs {
			return nil, errors.New("application source changed during the build")
		}
		flags[7] = "<artifact>"
		record := &Record{Format: 1, SourceRevision: o.Revision, Source: inputs, Inputs: files, Module: o.Module, Target: o.Target, GenerationStateSHA256: stateHash, GenerationCompiler: generator, BuildCompiler: buildidentity.Current(), BuildCompilerArtifact: Artifact{compilerHash, compilerSize}, Toolchain: toolchain, GoVersion: GoVersion, GitSHA256: gitHash, Environment: buildEnvironmentRecord(), DependencyProxy: "https://proxy.golang.org", GoTelemetry: "off", BuildFlags: flags, Modules: modules, BinarySettings: settings, Artifact: Artifact{hash, size}, IndependentBuilds: 2}
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
	if err := os.Mkdir(o.Output, 0700); err != nil {
		return nil, err
	}
	input, err := os.Open(firstBinary)
	if err != nil {
		return nil, err
	}
	defer input.Close()
	output, err := os.OpenFile(filepath.Join(o.Output, "application"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0500)
	if err != nil {
		return nil, err
	}
	_, err = io.Copy(output, input)
	if err == nil {
		err = output.Sync()
	}
	closeErr := output.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	hash, size, err := fileDigest(filepath.Join(o.Output, "application"))
	if err != nil || (Artifact{hash, size}) != result.Artifact {
		return nil, errors.New("saved application artifact differs")
	}
	if err := validateRecord(result); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(o.Output, "build.json"), data, 0600); err != nil {
		return nil, err
	}
	checksums := fmt.Sprintf("%s  application\n%s  build.json\n", hash, digest(data))
	if err := os.WriteFile(filepath.Join(o.Output, "SHA256SUMS"), []byte(checksums), 0600); err != nil {
		return nil, err
	}
	return result, nil
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
