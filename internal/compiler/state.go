package compiler

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// State represents the contents of .stego/state.yaml, tracking the last
// successful apply for change detection.
type State struct {
	// LastApplied records the state of the last successful apply.
	LastApplied *AppliedState `yaml:"last_applied,omitempty"`
}

// AppliedState captures the details of a single apply operation.
type AppliedState struct {
	// ServiceHash is the SHA-256 hash of the service.yaml content at apply time.
	ServiceHash string `yaml:"service_hash"`

	// RegistrySHA is the registry ref from .stego/config.yaml.
	RegistrySHA string `yaml:"registry_sha,omitempty"`

	// Components records the version and SHA of each component used.
	Components map[string]ComponentState `yaml:"components,omitempty"`

	// Entities records the entity names and their field names at apply time,
	// enabling plan to show entity field changes.
	Entities map[string][]string `yaml:"entities,omitempty"`

	// Files records the SHA-256 hash of each generated file.
	Files map[string]string `yaml:"files,omitempty"`
}

// ComponentState records a component's version and SHA at apply time.
type ComponentState struct {
	Version string `yaml:"version"`
	SHA     string `yaml:"sha,omitempty"`
}

// LoadState reads and parses a .stego/state.yaml file.
// Returns a zero State (no LastApplied) if the file does not exist.
func LoadState(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &State{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading state file: %w", err)
	}
	var s State
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parsing state file %s: %w", path, err)
	}
	return &s, nil
}

// SaveState writes a State to the given path, creating parent directories
// as needed.
func SaveState(path string, state *State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}
	data, err := yaml.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing state file: %w", err)
	}
	return nil
}

// HashBytes returns the hex-encoded SHA-256 hash of data.
func HashBytes(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h)
}
