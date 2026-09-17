package compiler

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jsell-rh/stego/internal/parser"
	"github.com/jsell-rh/stego/internal/registry"
	"github.com/jsell-rh/stego/internal/types"
	"gopkg.in/yaml.v3"
)

type InitOptions struct {
	ProjectDir string
	Archetype  string
	Stderr     io.Writer
}

type InitResult struct {
	Name    string
	Created []string
}

// Initialize uses the configured registry and publishes service.yaml last.
// Existing files are never replaced. A failed or interrupted attempt can leave
// complete configuration files or empty directories, but no truncated output.
func Initialize(opts InitOptions) (result *InitResult, err error) {
	if opts.Archetype == "" {
		return nil, fmt.Errorf("archetype is required")
	}
	project, err := filepath.Abs(opts.ProjectDir)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(project)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	original, existed, err := inspectInitProject(root)
	if err != nil {
		return nil, err
	}
	config := original
	if !existed {
		directory := os.Getenv("STEGO_REGISTRY")
		if directory == "" {
			directory = filepath.Join(project, "registry")
		}
		config, err = yaml.Marshal(types.RegistryConfig{Registry: []types.RegistrySource{{URL: directory, Ref: "local"}}})
		if err != nil {
			return nil, err
		}
	}
	resolved, err := registry.ResolveRegistry(registry.ResolveOptions{ProjectDir: project, ConfigData: config, Stderr: opts.Stderr})
	if err != nil {
		return nil, err
	}
	reg, err := registry.LoadDirectories(resolved.Dirs...)
	if err != nil {
		return nil, err
	}
	arch := reg.Archetype(opts.Archetype)
	if arch == nil {
		available := make([]string, 0, len(reg.Archetypes()))
		for name := range reg.Archetypes() {
			available = append(available, name)
		}
		sort.Strings(available)
		return nil, fmt.Errorf("archetype %q not found in registry (available: %s)", opts.Archetype, strings.Join(available, ", "))
	}
	name := filepath.Base(project)
	declaration, err := yaml.Marshal(types.ServiceDeclaration{Kind: "service", Name: name, Archetype: opts.Archetype, Language: arch.Language, Entities: []types.Entity{}, Collections: []types.Collection{}})
	if err != nil {
		return nil, err
	}
	if err := reg.Verify(); err != nil {
		return nil, err
	}
	lock, err := lockProject(root)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	current, exists, err := inspectInitProject(root)
	if err != nil {
		return nil, err
	}
	if existed != exists || !bytes.Equal(original, current) {
		return nil, fmt.Errorf("registry configuration changed during initialization")
	}
	if err := reg.Verify(); err != nil {
		return nil, err
	}
	created := []string{}
	if err := root.Mkdir("fills", 0755); err == nil {
		created = append(created, "fills/")
	} else if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	if err := checkInitDirectory(root, "fills"); err != nil {
		return nil, err
	}
	if !existed {
		if err := createRootFile(root, ".stego/config.yaml", config); err != nil {
			return nil, err
		}
		created = append(created, ".stego/config.yaml")
	}
	if err := createRootFile(root, "service.yaml", declaration); err != nil {
		return nil, err
	}
	return &InitResult{Name: name, Created: append(created, "service.yaml")}, nil
}

func checkInitDirectory(root *os.Root, name string) error {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s must be a directory without symbolic links", name)
	}
	return nil
}

func inspectInitProject(root *os.Root) ([]byte, bool, error) {
	if _, err := root.Lstat("service.yaml"); err == nil {
		return nil, false, fmt.Errorf("service.yaml already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	for _, name := range []string{".stego", "fills"} {
		if err := checkInitDirectory(root, name); err != nil {
			return nil, false, err
		}
	}
	const name = ".stego/config.yaml"
	if err := checkFileTarget(root, name); err != nil {
		return nil, false, err
	}
	file, err := root.Open(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("registry config must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, parser.MaxDocumentBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > parser.MaxDocumentBytes {
		return nil, false, fmt.Errorf("registry config exceeds its byte limit")
	}
	return data, true, nil
}
