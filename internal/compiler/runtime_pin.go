package compiler

import (
	"fmt"
	goparser "go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/parser"
	"github.com/jsell-rh/stego/internal/registry"
)

// runtimeImports enforces ST-SUP-INV10 on the code that one component emits: a Go file may import packages of the
// runtime library at its archetype's pin, and no other STEGO module and no other major version of the runtime
// library. It says whether the files import the runtime library, so the module file requires the pin
// (ST-GEN-INV13).
func runtimeImports(owner string, files []gen.File, pin registry.RuntimePin) (bool, error) {
	imported := false
	for _, file := range files {
		if !strings.HasSuffix(file.Path, ".go") {
			continue
		}
		parsed, err := goparser.ParseFile(token.NewFileSet(), file.Path, file.Bytes(), goparser.ImportsOnly)
		if err != nil {
			return false, fmt.Errorf("%s emits %s, whose imports do not parse: %w", owner, file.Path, err)
		}
		for _, spec := range parsed.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return false, fmt.Errorf("%s emits %s with an import that is not a string: %w", owner, file.Path, err)
			}
			use, err := runtimeImport(path, pin)
			if err != nil {
				return false, fmt.Errorf("%s emits %s, which imports %s: %w", owner, file.Path, path, err)
			}
			imported = imported || use
		}
	}
	return imported, nil
}

// runtimeImport says whether an import path is a package of the pinned runtime library, and refuses one of
// another major version of it or of another STEGO module.
func runtimeImport(path string, pin registry.RuntimePin) (bool, error) {
	if !registry.IsStegoModule(path) {
		return false, nil
	}
	if registry.InModule(path, pin.Module) {
		rest := strings.TrimPrefix(strings.TrimPrefix(path, pin.Module), "/")
		first, _, _ := strings.Cut(rest, "/")
		if _, major, ok := module.SplitPathVersion(pin.Module + "/" + first); ok && major != "" {
			return false, fmt.Errorf("that is the runtime library at another version; the archetype pins %s %s", pin.Module, pin.Version)
		}
		return true, nil
	}
	return false, fmt.Errorf("that is a STEGO module other than the runtime library; the archetype pins only %s %s", pin.Module, pin.Version)
}

// runtimeRequirement enforces ST-GEN-INV13 on the requirements of the components: a component may name no STEGO
// module, and the runtime library only at its archetype's pin, so the module file requires the pin and nothing else
// of STEGO.
func runtimeRequirement(owner, path, version string, pin *registry.RuntimePin) error {
	if !registry.IsStegoModule(path) {
		return nil
	}
	if pin != nil && path == pin.Module && version == pin.Version {
		return nil
	}
	if pin != nil {
		return fmt.Errorf("component %q requires %s %s; a service requires only the runtime library at its archetype's pin %s %s", owner, path, version, pin.Module, pin.Version)
	}
	return fmt.Errorf("component %q requires %s %s, a STEGO module; a service requires only the runtime library at its archetype's pin", owner, path, version)
}

// mergeRuntimeSum gives go.sum with the line of the runtime pin (ST-GEN-INV13): it replaces a line for the pinned
// version with another hash, and adds the line where Go sorts it, so a later go mod tidy keeps the order. It keeps
// every other line as it is, and gives the same bytes when the line is there.
func mergeRuntimeSum(data []byte, pin registry.RuntimePin) []byte {
	want := pin.Module + " " + pin.Version + " " + pin.Sum
	lines := strings.SplitAfter(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > 0 && !strings.HasSuffix(lines[len(lines)-1], "\n") {
		lines[len(lines)-1] += "\n"
	}
	insert := len(lines)
	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		if fields[0] == pin.Module && fields[1] == pin.Version {
			if line == want+"\n" {
				return data
			}
			lines[i] = want + "\n"
			return []byte(strings.Join(lines, ""))
		}
		version := strings.TrimSuffix(fields[1], "/go.mod")
		if insert == len(lines) && (fields[0] > pin.Module || (fields[0] == pin.Module && semver.Compare(version, pin.Version) >= 0)) {
			insert = i
		}
	}
	lines = slices.Insert(lines, insert, want+"\n")
	return []byte(strings.Join(lines, ""))
}

// runtimeModuleDrift enforces the drift part of ST-GEN-INV13 on the module file, from the module file alone: it
// gives a reason when go.mod requires the runtime library at a version that is not tagged, such as a
// pseudo-version, requires another STEGO module, or replaces a STEGO module. The record does not hold the
// archetype's pin (ST-GEN-Q04), so drift cannot tell one tagged version from another.
func runtimeModuleDrift(data []byte) (string, error) {
	file, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return "", err
	}
	for _, replace := range file.Replace {
		if registry.IsStegoModule(replace.Old.Path) {
			return fmt.Sprintf("go.mod replaces %s, a STEGO module", replace.Old.Path), nil
		}
	}
	for _, require := range file.Require {
		if !registry.IsStegoModule(require.Mod.Path) {
			continue
		}
		if require.Mod.Path != registry.RuntimeModule {
			return fmt.Sprintf("go.mod requires %s %s, a STEGO module that is not the runtime library", require.Mod.Path, require.Mod.Version), nil
		}
		if err := registry.CheckTaggedVersion(require.Mod.Path, require.Mod.Version); err != nil {
			return fmt.Sprintf("go.mod requires %s %s, which is not a tagged version that an archetype can pin", require.Mod.Path, require.Mod.Version), nil
		}
	}
	return "", nil
}

// mergeProjectSums reads go.sum as the input snapshot recorded it and gives it with the line of the runtime pin
// (ST-GEN-INV13).
func mergeProjectSums(projectDir string, recorded fileSnapshot, pin registry.RuntimePin) (gen.File, error) {
	root, err := os.OpenRoot(projectDir)
	if err != nil {
		return gen.File{}, err
	}
	defer root.Close()
	data, snapshot, err := readSnapshot(root, "go.sum", parser.MaxDocumentBytes, true)
	if err != nil {
		return gen.File{}, err
	}
	if snapshot != recorded {
		return gen.File{}, fmt.Errorf("go.sum changed during compilation; run plan again")
	}
	return gen.File{Path: "go.sum", Content: mergeRuntimeSum(data, pin)}, nil
}

// stageRuntimeProxy is the local stand-in for the module proxy that serves the runtime pin (ST-STD-RUNTIME-R04,
// ST-PAT-LOCAL-TEST): when generated code imports the runtime library and the common registry's runtime folder is
// the pinned version, it writes that version in the proxy layout under stage, adds the pin and the hash of its
// go.mod to the staged go.sum, and gives the proxy folder. The zip has the pinned hash, so the go command reads the
// same bytes as from the release, with no fetch of the module path's host. It gives "" when the service imports no
// runtime library or the folder holds another version; the go command then reads the pin from its proxies.
func stageRuntimeProxy(plan *Plan, stageRoot *os.Root, stage string) (string, error) {
	if plan.runtime == nil || plan.registry == nil {
		return "", nil
	}
	folder := plan.registry.RuntimeFolder()
	if folder == "" {
		return "", nil
	}
	pin := *plan.runtime
	escaped, err := module.EscapePath(pin.Module)
	if err != nil {
		return "", err
	}
	version, err := module.EscapeVersion(pin.Version)
	if err != nil {
		return "", err
	}
	proxy := filepath.Join(stage, "runtime-proxy")
	versions := filepath.Join(proxy, filepath.FromSlash(escaped), "@v")
	if err := os.MkdirAll(versions, 0o700); err != nil {
		return "", err
	}
	archive, err := registry.WriteRuntimeArchive(folder, pin.Module, pin.Version, filepath.Join(versions, version+".zip"))
	if err != nil {
		return "", err
	}
	if archive.Sum != pin.Sum {
		return "", os.RemoveAll(proxy)
	}
	modData, err := os.ReadFile(filepath.Join(folder, "go.mod"))
	if err != nil {
		return "", err
	}
	info := fmt.Sprintf("{\"Version\":%q}\n", pin.Version)
	for name, data := range map[string][]byte{version + ".mod": modData, version + ".info": []byte(info), "list": []byte(pin.Version + "\n")} {
		if err := os.WriteFile(filepath.Join(versions, name), data, 0o600); err != nil {
			return "", err
		}
	}
	sums, snapshot, err := readSnapshot(stageRoot, "go.sum", maxTrackedFileBytes, true)
	if err != nil {
		return "", err
	}
	if !snapshot.Exists {
		sums = nil
	}
	sums = mergeRuntimeModSum(mergeRuntimeSum(sums, pin), pin, archive.ModSum)
	if err := writeRootFileMode(stageRoot, "go.sum", sums, 0o600); err != nil {
		return "", err
	}
	return proxy, nil
}

// mergeRuntimeModSum gives go.sum with the hash of the pinned version's go.mod on the line after the pin's line,
// where Go sorts it. mergeRuntimeSum has added the pin's line.
func mergeRuntimeModSum(data []byte, pin registry.RuntimePin, modSum string) []byte {
	key := pin.Module + " " + pin.Version + "/go.mod "
	lines := strings.SplitAfter(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	want := key + modSum + "\n"
	at := len(lines)
	for i, line := range lines {
		if strings.HasPrefix(line, key) {
			lines[i] = want
			return []byte(strings.Join(lines, ""))
		}
		if strings.HasPrefix(line, pin.Module+" "+pin.Version+" ") {
			at = i + 1
		}
	}
	return []byte(strings.Join(slices.Insert(lines, at, want), ""))
}
