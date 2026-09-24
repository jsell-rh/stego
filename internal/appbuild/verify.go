package appbuild

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path"
	"reflect"
	"regexp"
)

var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Verify requires the expected record digest from a trusted source. It does not
// authenticate that source. The caller must verify provenance before this call.
// The executable is inspected as data and is never started.
func Verify(recordPath, artifactPath, expectedSHA256 string) (*Record, error) {
	record, err := readBuildRecord(recordPath, expectedSHA256)
	if err != nil {
		return nil, err
	}
	actual, bytes, err := fileDigest(artifactPath)
	if err != nil || (Artifact{actual, bytes}) != record.Artifact {
		return nil, errors.New("application executable differs from the build record")
	}
	settings, err := binarySettings(artifactPath)
	if err != nil || !reflect.DeepEqual(settings, record.BinarySettings) {
		return nil, errors.New("application executable settings differ from the build record")
	}
	if err := checkCompiledModules(artifactPath, record); err != nil {
		return nil, err
	}
	return record, nil
}

func readBuildRecord(recordPath, expectedSHA256 string) (*Record, error) {
	if !hashPattern.MatchString(expectedSHA256) {
		return nil, errors.New("a trusted build record digest is required")
	}

	f, err := openInput(recordPath)
	if err != nil {
		return nil, errors.New("cannot read application build record")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 16<<20 {
		return nil, errors.New("application build record size differs")
	}
	data, err := io.ReadAll(io.LimitReader(f, (16<<20)+1))
	if err != nil || int64(len(data)) != info.Size() || digest(data) != expectedSHA256 {
		return nil, errors.New("application build record digest or size differs")
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
