package compiler

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jsell-rh/stego/internal/gen"
)

const maxManifestFiles = 1024

// InputManifest records the project inputs read for one generation. Registry
// content and compiler build records are separate fields in the applied state.
// This is an input inventory, not a signature or an application build manifest.
type InputManifest struct {
	Version int                  `yaml:"version"`
	Options InputOptions         `yaml:"options"`
	Files   map[string]InputFile `yaml:"files"`
	SHA256  string               `yaml:"sha256"`
}

type InputOptions struct {
	ModuleName  string `yaml:"module_name"`
	GoVersion   string `yaml:"go_version"`
	OutputDir   string `yaml:"output_dir"`
	RegistryRef string `yaml:"registry_ref"`
}

type InputFile struct {
	Exists bool   `yaml:"exists"`
	SHA256 string `yaml:"sha256,omitempty"`
	Mode   uint32 `yaml:"mode,omitempty"`
}

func newInputManifest(input ReconcilerInput, out string, snapshots map[string]fileSnapshot) (*InputManifest, error) {
	m := &InputManifest{Version: 1, Options: InputOptions{input.ModuleName, input.GoVersion, out, input.RegistrySHA}, Files: make(map[string]InputFile, len(snapshots))}
	for name, snapshot := range snapshots {
		if name == ".stego/state.yaml" {
			continue
		}
		m.Files[name] = InputFile{Exists: snapshot.Exists, SHA256: snapshot.Hash, Mode: uint32(snapshot.Mode.Perm())}
	}
	if err := m.validateFields(); err != nil {
		return nil, err
	}
	m.SHA256 = m.digest()
	return m, nil
}

func validManifestText(value string) bool {
	return len(value) <= 1024 && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}
func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (m *InputManifest) validateFields() error {
	if m == nil || m.Version != 1 {
		return errors.New("unsupported project input manifest version")
	}
	if len(m.Files) < 4 || len(m.Files) > maxManifestFiles {
		return errors.New("invalid project input manifest file count")
	}
	for _, value := range []string{m.Options.ModuleName, m.Options.GoVersion, m.Options.OutputDir, m.Options.RegistryRef} {
		if !validManifestText(value) {
			return errors.New("invalid project input manifest option")
		}
	}
	if gen.ValidatePath(m.Options.OutputDir) != nil || m.Options.OutputDir == "" {
		return errors.New("invalid manifest output directory")
	}
	for _, name := range []string{"service.yaml", "go.mod", "go.sum", ".stego/config.yaml"} {
		if _, ok := m.Files[name]; !ok {
			return fmt.Errorf("input manifest is missing %s", name)
		}
	}
	if !m.Files["service.yaml"].Exists {
		return errors.New("input manifest requires service.yaml")
	}
	for name, file := range m.Files {
		if !validManifestText(name) || gen.ValidatePath(name) != nil || name == m.Options.OutputDir || strings.HasPrefix(name, m.Options.OutputDir+"/") || name == ".stego" || (strings.HasPrefix(name, ".stego/") && name != ".stego/config.yaml") {
			return fmt.Errorf("invalid input manifest path %q", name)
		}
		if !file.Exists {
			if file.SHA256 != "" || file.Mode != 0 {
				return errors.New("absent manifest input has content")
			}
			if name != "go.mod" && name != "go.sum" && name != ".stego/config.yaml" {
				return errors.New("required manifest input is absent")
			}
		} else if !validDigest(file.SHA256) || file.Mode > 0777 {
			return errors.New("invalid manifest input hash or mode")
		}
	}
	return nil
}
func (m *InputManifest) validate() error {
	if err := m.validateFields(); err != nil {
		return err
	}
	if !validDigest(m.SHA256) || m.SHA256 != m.digest() {
		return errors.New("project input manifest hash does not match its records")
	}
	return nil
}

func (m *InputManifest) digest() string {
	const prefix = "stego-project-inputs-v1\x00"
	options := []string{m.Options.ModuleName, m.Options.GoVersion, m.Options.OutputDir, m.Options.RegistryRef}
	size := len(prefix) + 8
	for _, value := range options {
		size += 8 + len(value)
	}
	for name, file := range m.Files {
		size += 8 + len(name) + 1 + 4 + 8 + len(file.SHA256)
	}
	data := make([]byte, 0, size)
	data = append(data, prefix...)
	field := func(value string) {
		data = binary.BigEndian.AppendUint64(data, uint64(len(value)))
		data = append(data, value...)
	}
	for _, value := range options {
		field(value)
	}
	data = binary.BigEndian.AppendUint64(data, uint64(len(m.Files)))
	for _, name := range sortedKeys(m.Files) {
		file := m.Files[name]
		field(name)
		exists := byte(0)
		if file.Exists {
			exists = 1
		}
		data = append(data, exists)
		data = binary.BigEndian.AppendUint32(data, file.Mode)
		field(file.SHA256)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
