package appbuild

import (
	"debug/buildinfo"
	"errors"
	"path"
	"runtime/debug"
	"sort"

	"golang.org/x/mod/module"
)

func compiledModulePaths(binary string) ([]string, error) {
	info, err := buildinfo.ReadFile(binary)
	if err != nil {
		return nil, errors.New("cannot inspect compiled modules")
	}
	if info.Main.Path == "" || len(info.Deps) > 4095 {
		return nil, errors.New("invalid executable module count")
	}
	paths := []string{info.Main.Path}
	for _, dep := range info.Deps {
		if dep == nil {
			return nil, errors.New("invalid executable dependency")
		}
		paths = append(paths, dep.Path)
	}
	seen := map[string]bool{}
	for _, name := range paths {
		if len(name) > 4096 || module.CheckImportPath(name) != nil || seen[name] {
			return nil, errors.New("invalid executable module path")
		}
		seen[name] = true
	}
	sort.Strings(paths)
	return paths, nil
}

func checkCompiledModules(binary string, record *Record) error {
	info, err := buildinfo.ReadFile(binary)
	if err != nil {
		return errors.New("cannot inspect executable modules")
	}
	return matchCompiledModules(info, record)
}

// Every compiled module must have recorded source content. A go.mod checksum
// alone cannot qualify compiled code from that module.
func matchCompiledModules(info *debug.BuildInfo, record *Record) error {
	failure := errors.New("executable modules differ from the build record")
	if info == nil {
		return failure
	}
	modules := map[string]Module{}
	mainCount := 0
	for _, m := range record.Modules {
		if _, present := modules[m.Path]; present {
			return failure
		}
		modules[m.Path] = m
		if m.LocalPath == record.Module && m.Replacement == nil && m.Version == "" {
			mainCount++
			if info.Main.Path != m.Path || (info.Main.Version != "" && info.Main.Version != "(devel)") || info.Main.Replace != nil || info.Main.Sum != "" || info.Path != path.Join(m.Path, record.Target) {
				return failure
			}
		}
	}
	if mainCount != 1 || len(modules) != len(info.Deps)+1 {
		return failure
	}
	seen := map[string]bool{info.Main.Path: true}
	for _, actual := range info.Deps {
		if actual == nil || seen[actual.Path] {
			return failure
		}
		seen[actual.Path] = true
		expected, present := modules[actual.Path]
		if !present || actual.Version != expected.Version {
			return failure
		}
		if actual.Replace != nil {
			if expected.Replacement == nil || actual.Replace.Replace != nil || actual.Replace.Path != expected.Replacement.Path {
				return failure
			}
			actual = actual.Replace
			expected = *expected.Replacement
		} else if expected.Replacement != nil {
			return failure
		}
		if expected.LocalPath != "" {
			if actual.Version != "" && actual.Version != "(devel)" {
				return failure
			}
			if actual.Sum != "" {
				return failure
			}
		} else if actual.Version != expected.Version || actual.Sum != expected.Sum || !validModuleSum(actual.Sum) {
			return failure
		}
	}
	return nil
}
