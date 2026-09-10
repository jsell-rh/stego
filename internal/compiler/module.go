package compiler

import (
	"errors"
	"fmt"
	"go/version"
	"io"
	"os"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/parser"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// moduleRequirements uses Go's minimum version selection order. A component
// cannot lower the version required by another component.
func moduleRequirements(wirings []ComponentWiring) (map[string]string, error) {
	requires := make(map[string]string)
	for _, component := range wirings {
		if component.Wiring == nil {
			continue
		}
		for _, id := range component.Wiring.Contracts {
			definition, err := gen.ResolveContract(id)
			if err != nil {
				return nil, err
			}
			for path, v := range definition.GoModRequires {
				if semver.Compare(v, requires[path]) > 0 {
					requires[path] = v
				}
			}
		}
		for _, path := range sortedKeys(component.Wiring.GoModRequires) {
			v := component.Wiring.GoModRequires[path]
			if err := module.Check(path, v); err != nil {
				return nil, fmt.Errorf("component %q has an invalid module requirement: %w", component.Name, err)
			}
			if module.CanonicalVersion(v) != v {
				return nil, fmt.Errorf("component %q requires a canonical version for %s: %q", component.Name, path, v)
			}
			if semver.Compare(v, requires[path]) > 0 {
				requires[path] = v
			}
		}
	}
	consumed, hasDB := computeConsumedConstructors(AssemblerInput{Wirings: wirings}, hasAnyRoutes(AssemblerInput{Wirings: wirings}))
	gormDB := false
	for key := range consumed {
		if wirings[key.WiringIndex].Wiring.DBBackend == "gorm" {
			gormDB = true
		}
	}
	if hasDB && !gormDB && semver.Compare("v5.11.0", requires["github.com/jackc/pgx/v5"]) > 0 {
		requires["github.com/jackc/pgx/v5"] = "v5.11.0"
	}
	// These runtime dependencies can select an affected unicode/norm version.
	// Keep a direct minimum so module resolution cannot restore GO-2026-5970.
	for _, dependency := range []string{"github.com/jackc/pgx/v5", "gorm.io/gorm", "google.golang.org/grpc", "golang.org/x/text"} {
		if requires[dependency] != "" && semver.Compare(requires["golang.org/x/text"], "v0.40.0") < 0 {
			requires["golang.org/x/text"] = "v0.40.0"
		}
	}

	// These minimums also remove known defects outside the current call graph.
	// Keep them with the runtime requirements so new generated paths do not
	// inherit affected transitive versions.
	for _, minimum := range []struct{ parent, path, version string }{
		{"google.golang.org/grpc", "golang.org/x/net", "v0.56.0"},
		{"google.golang.org/grpc", "golang.org/x/sys", "v0.48.0"},
		{"gorm.io/datatypes", "filippo.io/edwards25519", "v1.1.1"},
		{"github.com/go-sql-driver/mysql", "filippo.io/edwards25519", "v1.1.1"},
	} {
		if requires[minimum.parent] != "" && semver.Compare(requires[minimum.path], minimum.version) < 0 {
			requires[minimum.path] = minimum.version
		}
	}
	return requires, nil
}

// mergeProjectModule preserves application dependencies, replacements, tools,
// and comments. It changes only unmet compiler requirements. It does not resolve
// dependencies or use the network.
func mergeProjectModule(projectDir string, required gen.File) (gen.File, error) {
	root, err := os.OpenRoot(projectDir)
	if err != nil {
		return gen.File{}, err
	}
	defer root.Close()
	if err := checkFileTarget(root, "go.mod"); err != nil {
		return gen.File{}, err
	}
	data, err := readModule(root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return gen.File{}, fmt.Errorf("reading project module: %w", err)
	}
	return mergeCapturedModule(data, !errors.Is(err, os.ErrNotExist), required)
}

// mergeCapturedModule uses the same immutable bytes as the input manifest.
func mergeCapturedModule(data []byte, exists bool, required gen.File) (gen.File, error) {
	want, err := modfile.Parse("generated go.mod", required.Content, nil)
	if err != nil {
		return gen.File{}, fmt.Errorf("invalid generated module: %w", err)
	}
	if !exists {
		return required, nil
	}
	current, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return gen.File{}, err
	}
	if current.Module == nil || current.Module.Mod.Path != want.Module.Mod.Path {
		return gen.File{}, fmt.Errorf("project module must match %q; check STEGO_MODULE", want.Module.Mod.Path)
	}
	changed := false
	if current.Go == nil || version.Compare("go"+current.Go.Version, "go"+want.Go.Version) < 0 {
		if err := current.AddGoStmt(want.Go.Version); err != nil {
			return gen.File{}, err
		}
		changed = true
	}
	existing := make(map[string]string)
	for _, req := range current.Require {
		if _, found := existing[req.Mod.Path]; found {
			return gen.File{}, fmt.Errorf("duplicate module requirement %q", req.Mod.Path)
		}
		existing[req.Mod.Path] = req.Mod.Version
	}
	for _, req := range want.Require {
		if semver.Compare(existing[req.Mod.Path], req.Mod.Version) >= 0 {
			continue
		}
		if err := current.AddRequire(req.Mod.Path, req.Mod.Version); err != nil {
			return gen.File{}, err
		}
		changed = true
	}
	if changed {
		data, err = current.Format()
		if err != nil {
			return gen.File{}, err
		}
	}
	return gen.File{Path: "go.mod", Content: data}, nil
}

func readModule(root *os.Root) ([]byte, error) {
	if err := checkFileTarget(root, "go.mod"); err != nil {
		return nil, err
	}
	f, err := root.Open("go.mod")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, parser.MaxDocumentBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > parser.MaxDocumentBytes {
		return nil, fmt.Errorf("go.mod exceeds the %d-byte limit", parser.MaxDocumentBytes)
	}
	return data, nil
}

// ProjectModuleSettings uses the existing module unless the user gives an
// explicit setting. It rejects conflicting module names before generation.
func ProjectModuleSettings(projectDir, name, goVersion string) (string, string, error) {
	root, err := os.OpenRoot(projectDir)
	if err != nil {
		return "", "", err
	}
	defer root.Close()
	data, err := readModule(root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	if err == nil {
		file, err := modfile.Parse("go.mod", data, nil)
		if err != nil {
			return "", "", err
		}
		if file.Module == nil {
			return "", "", fmt.Errorf("go.mod has no module directive")
		}
		if name != "" && name != file.Module.Mod.Path {
			return "", "", fmt.Errorf("STEGO_MODULE %q conflicts with project module %q", name, file.Module.Mod.Path)
		}
		name = file.Module.Mod.Path
		if goVersion == "" && file.Go != nil {
			goVersion = file.Go.Version
		}
	}
	if name == "" {
		name = "github.com/example/service"
	}
	if goVersion == "" {
		goVersion = "1.26.8"
	}
	if err := module.CheckImportPath(name); err != nil {
		return "", "", err
	}
	if err := new(modfile.File).AddGoStmt(goVersion); err != nil {
		return "", "", err
	}
	return name, goVersion, nil
}
