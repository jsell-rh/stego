package appbuild

import (
	"debug/buildinfo"
	"errors"
	"fmt"
	"path"
	"runtime/debug"
	"sort"
	"strings"

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
	if mismatches := moduleMismatches(info, record); len(mismatches) > 0 {
		return errors.New("executable modules differ from the build record: " + strings.Join(mismatches, "; "))
	}
	return nil
}

// moduleMismatches names each difference between the modules compiled into
// an executable and the modules its record lists.
func moduleMismatches(info *debug.BuildInfo, record *Record) []string {
	if info == nil {
		return []string{"compiled modules: none can be read"}
	}
	var mismatches []string
	modules := map[string]Module{}
	mainCount := 0
	for _, m := range record.Modules {
		if _, present := modules[m.Path]; present {
			mismatches = append(mismatches, "module "+m.Path+": the record lists it twice")
			continue
		}
		modules[m.Path] = m
		if m.LocalPath == record.Module && m.Replacement == nil && m.Version == "" {
			mainCount++
			if info.Main.Path != m.Path || (info.Main.Version != "" && info.Main.Version != "(devel)") || info.Main.Replace != nil || info.Main.Sum != "" || info.Path != path.Join(m.Path, record.Target) {
				mismatches = append(mismatches, fmt.Sprintf("main module: recorded %s, found %s in %s", path.Join(m.Path, record.Target), info.Main.Path, info.Path))
			}
		}
	}
	if mainCount != 1 {
		mismatches = append(mismatches, fmt.Sprintf("main module: the record lists %d", mainCount))
	}
	seen := map[string]bool{info.Main.Path: true}
	for _, actual := range info.Deps {
		if actual == nil {
			mismatches = append(mismatches, "compiled modules: an empty entry")
			continue
		}
		if seen[actual.Path] {
			mismatches = append(mismatches, "module "+actual.Path+": compiled twice")
			continue
		}
		seen[actual.Path] = true
		expected, present := modules[actual.Path]
		if !present {
			mismatches = append(mismatches, fmt.Sprintf("module %s %s: compiled, but not in the record", actual.Path, actual.Version))
			continue
		}
		if actual.Version != expected.Version {
			mismatches = append(mismatches, fmt.Sprintf("module %s: recorded version %s, found %s", actual.Path, expected.Version, actual.Version))
			continue
		}
		if actual.Replace != nil {
			if expected.Replacement == nil || actual.Replace.Replace != nil || actual.Replace.Path != expected.Replacement.Path {
				mismatches = append(mismatches, fmt.Sprintf("module %s: its replacement differs from the record", actual.Path))
				continue
			}
			actual = actual.Replace
			expected = *expected.Replacement
		} else if expected.Replacement != nil {
			mismatches = append(mismatches, fmt.Sprintf("module %s: the record replaces it, the executable does not", actual.Path))
			continue
		}
		if expected.LocalPath != "" {
			if (actual.Version != "" && actual.Version != "(devel)") || actual.Sum != "" {
				mismatches = append(mismatches, fmt.Sprintf("module %s: recorded as local source, found version %s %s", actual.Path, actual.Version, actual.Sum))
			}
		} else if actual.Version != expected.Version || actual.Sum != expected.Sum || !validModuleSum(actual.Sum) {
			mismatches = append(mismatches, fmt.Sprintf("module %s %s: recorded checksum %s, found %s", actual.Path, actual.Version, expected.Sum, actual.Sum))
		}
	}
	for _, m := range record.Modules {
		if !seen[m.Path] {
			mismatches = append(mismatches, fmt.Sprintf("module %s %s: in the record, but not compiled", m.Path, m.Version))
		}
	}
	return mismatches
}
