package appbuild

import (
	"bytes"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"reflect"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
)

var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Verify requires the expected record digest from a trusted source. It does not
// authenticate that source. The caller must verify provenance before this call.
// The executable is inspected as data and is never started. A difference
// fails the check, and the error names each digest and build setting that
// differs, with the recorded and the found value.
func Verify(recordPath, artifactPath, expectedSHA256 string) (*Record, error) {
	record, err := readBuildRecord(recordPath, expectedSHA256)
	if err != nil {
		return nil, err
	}
	actual, size, err := fileDigest(artifactPath)
	if err != nil {
		return nil, fmt.Errorf("cannot read the executable %s; the check failed", artifactPath)
	}
	var settings map[string]string
	var info *debug.BuildInfo
	var unreadable []string
	if settings, err = binarySettings(artifactPath); err != nil {
		unreadable = append(unreadable, "build settings: "+err.Error())
	}
	if info, err = buildinfo.ReadFile(artifactPath); err != nil {
		unreadable = append(unreadable, "compiled modules: cannot be read from the executable")
	}
	mismatches := executableMismatches(record, Artifact{actual, size}, settings, info)
	reader := newVerifyReader(expectedSHA256, append(mismatches, unreadable...))
	if err := reader.failure("the executable " + artifactPath); err != nil {
		return nil, err
	}
	return record, nil
}

// executableMismatches compares what was found in an executable with its
// record and names each difference: the executable's digest and size, each
// build setting, and each compiled module. A nil settings map or build info
// is not compared; the caller names it as unreadable.
func executableMismatches(record *Record, artifact Artifact, settings map[string]string, info *debug.BuildInfo) []string {
	var mismatches []string
	if artifact.SHA256 != record.Artifact.SHA256 {
		mismatches = append(mismatches, fmt.Sprintf("executable digest: recorded %s, found %s", record.Artifact.SHA256, artifact.SHA256))
	}
	if artifact.Size != record.Artifact.Size {
		mismatches = append(mismatches, fmt.Sprintf("executable size: recorded %d, found %d", record.Artifact.Size, artifact.Size))
	}
	if settings != nil {
		keys := map[string]bool{}
		for key := range settings {
			keys[key] = true
		}
		for key := range record.BinarySettings {
			keys[key] = true
		}
		names := make([]string, 0, len(keys))
		for key := range keys {
			names = append(names, key)
		}
		sort.Strings(names)
		for _, key := range names {
			recorded, inRecord := record.BinarySettings[key]
			found, inExecutable := settings[key]
			if recorded != found || inRecord != inExecutable {
				mismatches = append(mismatches, fmt.Sprintf("build setting %s: recorded %s, found %s", key, quoteSetting(recorded, inRecord), quoteSetting(found, inExecutable)))
			}
		}
	}
	if info != nil {
		mismatches = append(mismatches, moduleMismatches(info, record)...)
	}
	return mismatches
}

func quoteSetting(value string, present bool) string {
	if !present {
		return "none"
	}
	return strconv.Quote(value)
}

func readBuildRecord(recordPath, expectedSHA256 string) (*Record, error) {
	if !hashPattern.MatchString(expectedSHA256) {
		return nil, errors.New("a trusted build record digest is required")
	}

	f, err := openInput(recordPath)
	if err != nil {
		return nil, fmt.Errorf("cannot read the build record %s; the check failed", recordPath)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 16<<20 {
		return nil, fmt.Errorf("cannot read the build record %s as a file of at most 16 MiB; the check failed", recordPath)
	}
	data, err := io.ReadAll(io.LimitReader(f, (16<<20)+1))
	if err != nil || int64(len(data)) != info.Size() {
		return nil, fmt.Errorf("cannot read the build record %s; the check failed", recordPath)
	}
	if found := digest(data); found != expectedSHA256 {
		return nil, fmt.Errorf("the build record %s is not trusted: expected digest %s, found %s", recordPath, expectedSHA256, found)
	}
	var record Record
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return nil, errors.New("invalid application build record")
	}
	canonical, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return nil, err
	}
	canonical = append(canonical, '\n')
	// Canonical bytes also reject repeated fields, alternate field spelling,
	// invalid Unicode, and data after the record.
	if !bytes.Equal(data, canonical) {
		return nil, errors.New("application build record is not canonical")
	}
	if err := validateRecord(&record); err != nil {
		return nil, err
	}
	return &record, nil
}

func validateRecord(record *Record) error {
	failure := errors.New("application build record violates the build policy")
	if record.Format != 1 || record.IndependentBuilds != 2 || record.GoVersion != GoVersion || record.Toolchain != sdkIdentity || !revisionPattern.MatchString(record.SourceRevision) || !hashPattern.MatchString(record.GitSHA256) || !hashPattern.MatchString(record.GenerationStateSHA256) {
		return failure
	}
	if record.Module != "." && !safePath(record.Module) {
		return failure
	}
	if !safePath(record.Target) {
		return failure
	}
	if !reflect.DeepEqual(record.Environment, buildEnvironmentRecord()) || record.GoTelemetry != "off" || !reflect.DeepEqual(record.BuildFlags, []string{"build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-pgo=off", "-p=2", "-o", "<artifact>", "./" + record.Target}) {
		return failure
	}
	switch record.DependencyProxy {
	case "https://proxy.golang.org":
		if record.ModuleCache != nil {
			return failure
		}
	case "off":
		if record.ModuleCache == nil || !hashPattern.MatchString(record.ModuleCache.SHA256) || record.ModuleCache.Files < 1 || record.ModuleCache.Bytes < 1 || record.ModuleCache.Bytes > maxBytes {
			return failure
		}
	default:
		return failure
	}
	for _, artifact := range []Artifact{record.Artifact, record.BuildCompilerArtifact} {
		if !hashPattern.MatchString(artifact.SHA256) || artifact.Size < 1 || artifact.Size > maxFileBytes {
			return failure
		}
	}
	if record.BuildCompiler.Validate() != nil || record.GenerationCompiler.Validate() != nil {
		return failure
	}
	if len(record.Inputs) < 1 || len(record.Inputs) > maxFiles || record.Source.Files != len(record.Inputs) || record.Source.Bytes < 1 || record.Source.Bytes > maxBytes {
		return failure
	}
	tuples := make([][]any, 0, len(record.Inputs))
	last := ""
	for _, file := range record.Inputs {
		if !safePath(file.Path) || file.Path <= last || !hashPattern.MatchString(file.SHA256) {
			return failure
		}
		last = file.Path
		tuples = append(tuples, []any{file.Path, file.SHA256, file.Executable})
	}

	statePath := path.Join(record.Module, ".stego/state.yaml")
	foundState := false
	for _, input := range record.Inputs {
		if input.Path == statePath && input.SHA256 == record.GenerationStateSHA256 {
			foundState = true
		}
	}
	if !foundState {
		return failure
	}
	data, err := inventoryJSON(tuples)
	if err != nil || digest(data) != record.Source.SHA256 {
		return failure
	}
	if !reflect.DeepEqual(record.SBOMs, recordSBOMs()) {
		return failure
	}
	if len(record.Modules) == 0 || len(record.Modules) > 4096 {
		return failure
	}
	last = ""
	for _, module := range record.Modules {
		if module.Path <= last {
			return failure
		}
		last = module.Path
		if err := validateModule(module, false); err != nil {
			return failure
		}
	}
	return nil
}

func validateModule(module Module, replacement bool) error {
	if module.Path == "" || len(module.Path) > 4096 {
		return errors.New("invalid module path")
	}
	// Only the runtime library, as a remote module that nothing replaces, can
	// come from the runtime folder of the source revision (ST-BLD-UC02-R05).
	if module.Source != "" && (module.Source != RuntimeFolderSource || module.Path != RuntimeModule || replacement || module.Replacement != nil || module.LocalPath != "") {
		return errors.New("invalid module source")
	}
	if module.Replacement != nil {
		if replacement || module.LocalPath != "" {
			return errors.New("invalid module replacement")
		}
		return validateModule(*module.Replacement, true)
	}
	if module.LocalPath != "" {
		if module.Version != "" || module.Sum != "" || module.GoModSum != "" || (module.LocalPath != "." && !safePath(module.LocalPath)) {
			return errors.New("invalid local module record")
		}
		return nil
	}
	if module.Version == "" || !validModuleSum(module.Sum) || !validModuleSum(module.GoModSum) {
		return errors.New("invalid remote module record")
	}
	return nil
}
