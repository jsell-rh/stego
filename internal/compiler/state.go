package compiler

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jsell-rh/stego/internal/buildidentity"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/parser"
	"github.com/jsell-rh/stego/internal/registry"
	"gopkg.in/yaml.v3"
)

// State represents the contents of .stego/state.yaml, tracking the last
// successful apply for change detection.
type State struct {
	// FormatVersion is zero for legacy state, and 1 or StateFormatVersion for
	// state that names its version.
	FormatVersion int `yaml:"format_version,omitempty"`

	// LastApplied records the state of the last successful apply.
	LastApplied *AppliedState `yaml:"last_applied,omitempty"`
}

// StateFormatVersion identifies the state format written by this compiler.
// Version 2 adds service_name, and makes the sha of each component its
// component digest (ComponentDigest) where version 1 held the registry
// reference. Version 3 adds archetype: the archetype's name, version and
// runtime library pin (ST-GEN-EN01). Version 4 added data_path, the data path
// of the run, runtime or store (ST-SUP-RC01-OC41), and version 5 removes it
// again: the runtime data path is the only data path (ST-SUP-RC20-OC39). This
// compiler reads versions 0 to 5, and the next apply writes version 5.
const StateFormatVersion = 5

// formatWithDataPath is the one format version whose last_applied held the
// data path. The compiler drops that key when it reads such a record, and its
// next apply writes the record without it.
const formatWithDataPath = 4

// oldestNamedStateFormat is the first state format that names its version.
const oldestNamedStateFormat = 1

// UnmarshalYAML requires an integer when a format version is present.
// DecodeStrict checks field names before this method runs.
func (state *State) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("state must be a mapping")
	}
	for i := 0; i < len(node.Content); i += 2 {
		if node.Content[i].Value == "format_version" && (node.Content[i+1].Kind != yaml.ScalarNode || node.Content[i+1].Tag != "!!int") {
			return fmt.Errorf("state format version must be an integer")
		}
	}
	type plainState State
	return node.Decode((*plainState)(state))
}

// AppliedState captures the details of a single apply operation.
type AppliedState struct {
	// Inputs records captured project files and supplied generation options.
	Inputs *InputManifest `yaml:"inputs,omitempty"`

	// CompilerBuild records selected metadata of the compiler executable.
	// Old state can omit this record.
	CompilerBuild *buildidentity.Record `yaml:"compiler_build,omitempty"`

	// ServiceName is the name that the declaration gives the service.
	// State before format version 2 omits it.
	ServiceName string `yaml:"service_name,omitempty"`

	// ServiceHash is the SHA-256 hash of the service.yaml content at apply time.
	ServiceHash string `yaml:"service_hash"`

	// RegistrySHA is the registry ref from .stego/config.yaml.
	RegistrySHA string `yaml:"registry_sha,omitempty"`

	// RegistryContentSHA256 identifies captured registry YAML and protobuf files.
	// An absent hash in old state means that registry content was not recorded.
	RegistryContentSHA256 string `yaml:"registry_content_sha256,omitempty"`

	// Components records the version and the component digest of each
	// component used.
	Components map[string]ComponentState `yaml:"components,omitempty"`

	// Entities records the entity names and their field snapshots at apply time,
	// enabling plan to show entity field changes including type and constraint
	// modifications.
	Entities map[string][]EntityFieldState `yaml:"entities,omitempty"`

	// Files records the SHA-256 hash of each generated file.
	Files map[string]string `yaml:"files,omitempty"`

	// Archetype records the archetype of the run with its runtime library pin.
	// State before format version 3 omits it.
	Archetype *ArchetypeState `yaml:"archetype,omitempty"`
}

// legacyRecord is a record as the strict decoder reads it before the compiler decodes it as State: the record with
// the key data_path that format version 4 held in last_applied. The strict decoder refuses a second document, an
// alias, a deep nesting and an unknown field, so the key is removed only from a record that holds nothing else
// that State does not read (ST-SUP-RC20-OC39).
type legacyRecord struct {
	FormatVersion int               `yaml:"format_version,omitempty"`
	LastApplied   *legacyAppliedRun `yaml:"last_applied,omitempty"`
}

// legacyAppliedRun is last_applied with the key data_path of format version 4.
type legacyAppliedRun struct {
	AppliedState   `yaml:",inline"`
	LegacyDataPath string `yaml:"data_path,omitempty"`
}

// withoutDataPath removes the key data_path from the last_applied mapping of a record of format version 4, the one
// version that held it, after the strict decoder accepted the whole record with it. The key told the data path of
// the run, runtime or store; every run is on the runtime data path now, and the migration job of the service, not
// the record, refuses a database that holds tables its ledger does not record (ST-SUP-RC20-OC39). A record of
// another version with the key, or with another value of it, is refused.
func withoutDataPath(data []byte, path string) ([]byte, error) {
	var legacy legacyRecord
	if err := parser.DecodeStrict(data, path, &legacy); err != nil {
		// The strict decoder of State reports what is wrong with the record.
		return data, nil
	}
	if legacy.LastApplied == nil || legacy.LastApplied.LegacyDataPath == "" {
		return data, nil
	}
	if legacy.FormatVersion != formatWithDataPath {
		return nil, fmt.Errorf("the record of format version %d has data_path, which only format version %d held", legacy.FormatVersion, formatWithDataPath)
	}
	if value := legacy.LastApplied.LegacyDataPath; value != "runtime" && value != "store" {
		return nil, fmt.Errorf("the record has the data path %q, which is not runtime or store", value)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil || document.Kind != yaml.DocumentNode || len(document.Content) != 1 || !dropDataPath(document.Content[0]) {
		return nil, fmt.Errorf("the record of format version %d cannot be read without its data path", formatWithDataPath)
	}
	return yaml.Marshal(&document)
}

// dropDataPath removes the key data_path from the last_applied mapping of the record's mapping, and tells whether
// it removed it.
func dropDataPath(node *yaml.Node) bool {
	if node.Kind != yaml.MappingNode {
		return false
	}
	var applied *yaml.Node
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == "last_applied" {
			applied = node.Content[i+1]
		}
	}
	if applied == nil || applied.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(applied.Content); i += 2 {
		if applied.Content[i].Value == "data_path" {
			applied.Content = append(applied.Content[:i:i], applied.Content[i+2:]...)
			return true
		}
	}
	return false
}

// ArchetypeState records the archetype of one run: its name, its version and
// its runtime library pin, so a later run refuses a pin that moved within one
// archetype version (ST-SUP-INV10), and whether generated code imports the
// runtime library, so drift checks go.mod against the pin (ST-GEN-INV13).
type ArchetypeState struct {
	Name           string               `yaml:"name"`
	Version        string               `yaml:"version"`
	Runtime        *registry.RuntimePin `yaml:"runtime,omitempty"`
	ImportsRuntime bool                 `yaml:"imports_runtime,omitempty"`
}

// EntityFieldState captures a field's name, type, and a hash of its full
// definition for change detection (type changes, constraint changes, etc.).
type EntityFieldState struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`
	Hash string `yaml:"hash"` // SHA-256 of the serialized field definition
}

// ComponentState records a component's version and its component digest.
// In state before format version 2, SHA holds the registry reference.
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
	data, err := withoutDataPath(data, path)
	if err != nil {
		return nil, fmt.Errorf("invalid state; restore or migrate the state file: %w", err)
	}
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
	data, err := encodeState(state)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing state file: %w", err)
	}
	return nil
}

// Validate before any write. Upgrade a copy so reads and caller state stay unchanged.
func encodeState(state *State) ([]byte, error) {
	if err := validateStatePaths(state); err != nil {
		return nil, err
	}
	current := *state
	current.FormatVersion = StateFormatVersion
	data, err := yaml.Marshal(&current)
	if err != nil {
		return nil, fmt.Errorf("marshaling state: %w", err)
	}
	if len(data) > parser.MaxDocumentBytes {
		return nil, fmt.Errorf("state exceeds the %d-byte document limit", parser.MaxDocumentBytes)
	}
	return data, nil
}

func validateStatePaths(state *State) error {
	if state == nil {
		return fmt.Errorf("state must not be nil")
	}
	if state.FormatVersion != 0 && (state.FormatVersion < oldestNamedStateFormat || state.FormatVersion > StateFormatVersion) {
		return fmt.Errorf("unsupported state format version %d; this compiler reads legacy version 0 and versions %d to %d", state.FormatVersion, oldestNamedStateFormat, StateFormatVersion)
	}
	if state.LastApplied == nil {
		return nil
	}
	if manifest := state.LastApplied.Inputs; manifest != nil {
		if err := manifest.validate(); err != nil {
			return err
		}
		if manifest.Files["service.yaml"].SHA256 != state.LastApplied.ServiceHash {
			return fmt.Errorf("input manifest does not match the applied service: the record names declaration digest %q and its input manifest %q", state.LastApplied.ServiceHash, manifest.Files["service.yaml"].SHA256)
		}
		if manifest.Options.RegistryRef != state.LastApplied.RegistrySHA {
			return fmt.Errorf("input manifest does not match the registry reference: the record names registry reference %q and its input manifest %q", state.LastApplied.RegistrySHA, manifest.Options.RegistryRef)
		}
	}
	if record := state.LastApplied.CompilerBuild; record != nil {
		if err := record.Validate(); err != nil {
			return err
		}
	}
	if digest := state.LastApplied.RegistryContentSHA256; digest != "" {
		if len(digest) != sha256.Size*2 {
			return fmt.Errorf("registry content hash must be a lowercase SHA-256 digest")
		}
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != sha256.Size || digest != strings.ToLower(digest) {
			return fmt.Errorf("registry content hash must be a lowercase SHA-256 digest")
		}
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

// archetype gives the recorded archetype, or nil for no record or state from an older release.
func (s *AppliedState) archetype() *ArchetypeState {
	if s == nil {
		return nil
	}
	return s.Archetype
}

// stateDiffers says whether two states give different records.
func stateDiffers(old, new *State) (bool, error) {
	oldData, err := yaml.Marshal(old)
	if err != nil {
		return false, err
	}
	newData, err := yaml.Marshal(new)
	if err != nil {
		return false, err
	}
	return string(oldData) != string(newData), nil
}
