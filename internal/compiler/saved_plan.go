package compiler

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/jsell-rh/stego/internal/buildidentity"
	"github.com/jsell-rh/stego/internal/parser"
	"gopkg.in/yaml.v3"
)

// savedPlanPath is the saved plan in STEGO's working state (ST-GEN-EN06-TB).
// It is local working state, not committed.
const savedPlanPath = ".stego/plan.yaml"

// savedPlanVersion is the format of the saved plan.
const savedPlanVersion = 1

// SavedPlan is the saved plan of ST-GEN-EN06: what one plan would change, and
// the digests it was made from. Apply carries out a saved plan only when a new
// plan from the inputs on disk gives the same saved plan, so it writes exactly
// the files the saved plan names, with the content it planned, or nothing.
type SavedPlan struct {
	Version int `yaml:"version"`
	// BaseRecordSHA256 is the SHA-256 of the record the plan was made
	// against; a plan for a service with no record leaves it out.
	BaseRecordSHA256      string               `yaml:"base_record_sha256,omitempty"`
	ServiceHash           string               `yaml:"service_hash"`
	RegistryContentSHA256 string               `yaml:"registry_content_sha256"`
	CompilerBuild         buildidentity.Record `yaml:"compiler_build"`
	OutputDir             string               `yaml:"output_dir"`
	// RecordSHA256 is the SHA-256 of the record the apply would save, and is
	// empty when the record does not change.
	RecordSHA256 string `yaml:"record_sha256,omitempty"`
	// Files lists each file the plan creates, updates or deletes, in path
	// order, with the SHA-256 of its new content.
	Files []SavedPlanFile `yaml:"files"`
}

// SavedPlanFile is one planned change of a generated file.
type SavedPlanFile struct {
	Path   string `yaml:"path"`
	Action string `yaml:"action"`
	SHA256 string `yaml:"sha256,omitempty"`
}

// newSavedPlan gives the saved form of a plan.
func newSavedPlan(plan *Plan) (*SavedPlan, error) {
	if plan == nil || plan.NewState == nil || plan.NewState.LastApplied == nil || plan.NewState.LastApplied.CompilerBuild == nil {
		return nil, fmt.Errorf("a plan without its new record cannot be saved")
	}
	applied := plan.NewState.LastApplied
	saved := &SavedPlan{
		Version: savedPlanVersion, ServiceHash: applied.ServiceHash, RegistryContentSHA256: applied.RegistryContentSHA256,
		CompilerBuild: *applied.CompilerBuild, Files: []SavedPlanFile{},
	}
	if applied.Inputs != nil {
		saved.OutputDir = applied.Inputs.Options.OutputDir
	}
	if base := plan.snapshots[".stego/state.yaml"]; base.Exists {
		saved.BaseRecordSHA256 = base.Hash
	}
	if plan.StateChanged {
		record, err := encodeState(plan.NewState)
		if err != nil {
			return nil, err
		}
		saved.RecordSHA256 = HashBytes(record)
	}
	contents := make(map[string]string, len(plan.GeneratedFiles))
	for _, file := range plan.GeneratedFiles {
		contents[file.Path] = HashBytes(file.Bytes())
	}
	for _, file := range plan.Files {
		switch file.Action {
		case ActionUnchanged:
		case ActionDelete:
			saved.Files = append(saved.Files, SavedPlanFile{Path: file.Path, Action: string(file.Action)})
		default:
			saved.Files = append(saved.Files, SavedPlanFile{Path: file.Path, Action: string(file.Action), SHA256: contents[file.Path]})
		}
	}
	return saved, nil
}

// SavePlan is the handler of ST-GEN-CMD03, Make a plan: it saves the plan that
// Reconcile made in .stego/plan.yaml. It writes no generated file and does not
// change the record. It refuses while an apply is pending.
func SavePlan(plan *Plan, projectDir string) error {
	saved, err := newSavedPlan(plan)
	if err != nil {
		return err
	}
	data, err := yaml.Marshal(saved)
	if err != nil {
		return err
	}
	if len(data) > parser.MaxDocumentBytes {
		return fmt.Errorf("the saved plan exceeds the %d-byte document limit", parser.MaxDocumentBytes)
	}
	root, err := os.OpenRoot(projectDir)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := pendingTransaction(root); err != nil {
		return err
	}
	return writeRootFileMode(root, savedPlanPath, data, 0o600)
}

// LoadPlan is the handler of ST-GEN-QRY02, Get the saved plan: it reads
// .stego/plan.yaml through the strict decoder, and writes nothing.
func LoadPlan(projectDir string) (*SavedPlan, error) {
	root, err := os.OpenRoot(projectDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	data, snapshot, err := readSnapshot(root, savedPlanPath, parser.MaxDocumentBytes, true)
	if err != nil {
		return nil, err
	}
	if !snapshot.Exists {
		return nil, fmt.Errorf("no saved plan in %s; run stego plan first", savedPlanPath)
	}
	var saved SavedPlan
	if err := parser.DecodeStrict(data, savedPlanPath, &saved); err != nil {
		return nil, fmt.Errorf("the saved plan cannot be read: %w", err)
	}
	if saved.Version != savedPlanVersion {
		return nil, fmt.Errorf("the saved plan has format version %d; this compiler reads version %d", saved.Version, savedPlanVersion)
	}
	return &saved, nil
}

// UseSavedPlan makes apply carry out the saved plan: apply refuses, and writes
// nothing, unless the plan gives the same saved plan.
func (plan *Plan) UseSavedPlan(saved *SavedPlan) {
	plan.saved = saved
}

// StillApplies tells whether a plan made now from the inputs on disk gives the
// saved plan, and when it does not, what differs.
func (saved *SavedPlan) StillApplies(plan *Plan) error {
	current, err := newSavedPlan(plan)
	if err != nil {
		return err
	}
	var differences []string
	if saved.BaseRecordSHA256 != current.BaseRecordSHA256 {
		differences = append(differences, "the record")
	}
	if saved.ServiceHash != current.ServiceHash {
		differences = append(differences, "the declaration")
	}
	if saved.RegistryContentSHA256 != current.RegistryContentSHA256 {
		differences = append(differences, "the registry content")
	}
	if saved.CompilerBuild != current.CompilerBuild {
		differences = append(differences, "the compiler")
	}
	if saved.OutputDir != current.OutputDir || saved.RecordSHA256 != current.RecordSHA256 || !reflect.DeepEqual(saved.Files, current.Files) {
		differences = append(differences, "the planned files")
	}
	if len(differences) > 0 {
		return fmt.Errorf("the saved plan is stale: %s changed after planning; run stego plan again", strings.Join(differences, ", "))
	}
	return nil
}

// removeSavedPlan removes the saved plan after an apply, so a saved plan
// applies only once.
func removeSavedPlan(root *os.Root) error {
	if err := root.Remove(savedPlanPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// FormatSavedPlan prints each file the saved plan creates, updates or
// deletes, and whether the record changes.
func FormatSavedPlan(saved *SavedPlan) string {
	var out strings.Builder
	for _, file := range saved.Files {
		fmt.Fprintf(&out, "  %s: %s\n", file.Action, file.Path)
	}
	if saved.RecordSHA256 != "" {
		out.WriteString("The record changes.\n")
	} else {
		out.WriteString("The record does not change.\n")
	}
	return out.String()
}
