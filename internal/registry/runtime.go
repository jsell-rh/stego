package registry

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	"golang.org/x/mod/sumdb/dirhash"
	modzip "golang.org/x/mod/zip"
)

// RuntimeModule is the module path of the runtime library, the Go module in the folder runtime of the common
// registry (ST-SUP-TOOL11).
const RuntimeModule = "stego.dev/runtime"

// RuntimePin is one version of the runtime library (ST-VO58): its module path, a tagged version and the go.sum
// hash of that version's contents. An archetype pins exactly one (ST-SUP-INV10).
type RuntimePin struct {
	Module  string `yaml:"module"`
	Version string `yaml:"version"`
	Sum     string `yaml:"sum"`
}

var moduleSum = regexp.MustCompile(`^h1:[A-Za-z0-9+/]{43}=$`)

// Validate enforces ST-VO58-C01: the pin names a module path, a tagged version and the module checksum of that
// version, never a branch, a pseudo-version or a replace directive. A replace directive has no field in a pin, so
// the strict decoder refuses it by its key. Each error names the field.
func (p RuntimePin) Validate() error {
	if p.Module == "" {
		return errors.New("runtime.module is missing: a runtime pin names the module path of the runtime library")
	}
	if err := module.CheckPath(p.Module); err != nil {
		return fmt.Errorf("runtime.module %q is not a module path: %w", p.Module, err)
	}
	if p.Version == "" {
		return errors.New("runtime.version is missing: a runtime pin names a tagged version")
	}
	if !semver.IsValid(p.Version) || semver.Canonical(p.Version) != p.Version || semver.Build(p.Version) != "" {
		return fmt.Errorf("runtime.version %q is not a tagged version such as v1.4.2; a branch is not a version", p.Version)
	}
	if module.IsPseudoVersion(p.Version) {
		return fmt.Errorf("runtime.version %q is a pseudo-version; a runtime pin names a tagged version", p.Version)
	}
	if err := module.Check(p.Module, p.Version); err != nil {
		return fmt.Errorf("runtime.version %q does not fit runtime.module %q: %w", p.Version, p.Module, err)
	}
	if p.Sum == "" {
		return errors.New("runtime.sum is missing: a runtime pin names the go.sum hash of its version")
	}
	if !moduleSum.MatchString(p.Sum) {
		return fmt.Errorf("runtime.sum %q is not a go.sum hash h1:<44 base64 characters>", p.Sum)
	}
	return nil
}

// checkRuntimePin enforces ST-SUP-INV10 at load: an archetype pins exactly one runtime library, by module path,
// version and module checksum. The YAML decoder refuses a second runtime key, and a runtime value that is a list.
func checkRuntimePin(a *Archetype) error {
	if a.Runtime == nil {
		return fmt.Errorf("archetype %s %s pins no runtime library: add runtime with its module, version and sum", a.Name, a.Version)
	}
	if err := a.Runtime.Validate(); err != nil {
		return fmt.Errorf("archetype %s %s: %w", a.Name, a.Version, err)
	}
	return nil
}

// IsStegoModule says whether a module path is under one of STEGO's own module paths: stego.dev, or the compiler's
// repository github.com/jsell-rh/stego. A generated service may require only the runtime pin of these
// (ST-SUP-INV10, ST-GEN-INV13).
func IsStegoModule(path string) bool {
	for _, root := range []string{"stego.dev", "github.com/jsell-rh/stego"} {
		if path == root || strings.HasPrefix(path, root+"/") {
			return true
		}
	}
	return false
}

// InModule says whether an import path is a package of the module at modulePath.
func InModule(importPath, modulePath string) bool {
	return importPath == modulePath || strings.HasPrefix(importPath, modulePath+"/")
}

// RuntimeFolder gives the folder of the runtime library in the common registry, the first source, or "" when that
// source has no runtime folder with a go.mod. The runtime library is released with the common registry
// (ST-STD-RUNTIME-R04).
func (r *Registry) RuntimeFolder() string {
	if len(r.captured) == 0 {
		return ""
	}
	folder := filepath.Join(r.captured[0].dir, "runtime")
	if info, err := os.Stat(filepath.Join(folder, "go.mod")); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return folder
}

// RuntimeArchive is the module zip of a runtime folder at one version, with the go.sum hashes of the zip and of
// its go.mod.
type RuntimeArchive struct {
	Sum    string
	ModSum string
}

// WriteRuntimeArchive writes the module zip of the folder as module@version to zipPath, as the Go module proxy
// serves it for the tag registry/runtime/<version>, and gives its go.sum hashes. The folder holds its own LICENSE,
// so the zip is the same as the one the proxy makes from the tag.
func WriteRuntimeArchive(folder, modulePath, version, zipPath string) (RuntimeArchive, error) {
	file, err := os.OpenFile(zipPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return RuntimeArchive{}, err
	}
	err = modzip.CreateFromDir(file, module.Version{Path: modulePath, Version: version}, folder)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return RuntimeArchive{}, fmt.Errorf("module zip of %s: %w", folder, err)
	}
	sum, err := dirhash.HashZip(zipPath, dirhash.Hash1)
	if err != nil {
		return RuntimeArchive{}, err
	}
	modSum, err := dirhash.Hash1([]string{"go.mod"}, func(string) (io.ReadCloser, error) {
		return os.Open(filepath.Join(folder, "go.mod"))
	})
	if err != nil {
		return RuntimeArchive{}, err
	}
	return RuntimeArchive{Sum: sum, ModSum: modSum}, nil
}

// RuntimeSum gives the go.sum hash of the folder as module@version: the sum an archetype's runtime pin must hold
// for the folder to serve that version.
func RuntimeSum(folder, modulePath, version, scratch string) (string, error) {
	archive, err := WriteRuntimeArchive(folder, modulePath, version, filepath.Join(scratch, "runtime.zip"))
	if err != nil {
		return "", err
	}
	return archive.Sum, nil
}
