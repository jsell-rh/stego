package compiler

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/parser"
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

	// Entities records the entity names and their field snapshots at apply time,
	// enabling plan to show entity field changes including type and constraint
	// modifications.
	Entities map[string][]EntityFieldState `yaml:"entities,omitempty"`

	// Files records the SHA-256 hash of each generated file.
	Files map[string]string `yaml:"files,omitempty"`
}

// EntityFieldState captures a field's name, type, and a hash of its full
// definition for change detection (type changes, constraint changes, etc.).
type EntityFieldState struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`
	Hash string `yaml:"hash"` // SHA-256 of the serialized field definition
}

// ComponentState records a component's version and SHA at apply time.
type ComponentState struct {
	Version string `yaml:"version"`
	SHA     string `yaml:"sha,omitempty"`
}

// LoadState reads and parses a .stego/state.yaml file.
// Returns a zero State (no LastApplied) if the file does not exist.
func LoadState(path string) (*State, error) {
	data, err := parser.ReadDocument(path)
	if errors.Is(err, os.ErrNotExist) {
		return &State{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading state file: %w", err)
	}
	return decodeState(data, path)
}

func decodeState(data []byte, path string) (*State, error) {
	var s State
	if err := parser.DecodeStrict(data, path, &s); err != nil {
		return nil, fmt.Errorf("invalid state; restore or migrate the state file: %w", err)
	}
	if err := validateStatePaths(&s); err != nil {
		return nil, fmt.Errorf("invalid state in %s: %w", path, err)
	}
	return &s, nil
}

// SaveState writes a State to the given path, creating parent directories
// as needed.
func SaveState(path string, state *State) error {
	if err := validateStatePaths(state); err != nil {
		return err
	}
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

func validateStatePaths(state *State) error {
	if state == nil {
		return fmt.Errorf("state must not be nil")
	}
	if state.LastApplied == nil {
		return nil
	}
	for _, path := range sortedKeys(state.LastApplied.Files) {
		if err := gen.ValidatePath(path); err != nil {
			return err
		}
	}
	return nil
}

// HashBytes returns the hex-encoded SHA-256 hash of data.
func HashBytes(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h)
}
