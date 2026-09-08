package compiler

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/parser"
	"gopkg.in/yaml.v3"
)

const transactionPath = ".stego/transaction.yaml"
const maxTransactionBytes = 128 << 20
const maxTransactionFiles = 10000

var ErrRecoveryRequired = errors.New("incomplete apply; run 'stego recover'")

type transaction struct {
	Version    int                     `yaml:"version"`
	OutputDir  string                  `yaml:"output_dir"`
	Expected   map[string]fileSnapshot `yaml:"expected"`
	Operations []transactionOperation  `yaml:"operations"`
}

type transactionOperation struct {
	Path   string       `yaml:"path"`
	Before fileSnapshot `yaml:"before"`
	After  fileSnapshot `yaml:"after"`
	Data   string       `yaml:"data,omitempty"`
}

type applyFault func(string) error

func defaultOutputMode() os.FileMode {
	if runtime.GOOS == "windows" {
		return 0666
	}
	return 0644
}

func pendingTransaction(root *os.Root) error {
	if err := checkFileTarget(root, transactionPath); err != nil {
		return err
	}
	_, err := root.Lstat(transactionPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return ErrRecoveryRequired
}

func checkPendingProject(projectDir string) error {
	root, err := os.OpenRoot(projectDir)
	if err != nil {
		return err
	}
	defer root.Close()
	return pendingTransaction(root)
}

func validSnapshot(snapshot fileSnapshot) bool {
	if !snapshot.Exists {
		return snapshot == (fileSnapshot{})
	}
	if len(snapshot.Hash) != 64 || snapshot.Mode & ^os.ModePerm != 0 {
		return false
	}
	for _, ch := range snapshot.Hash {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}

func validateTransactionPath(outputDir, name string) error {
	if err := gen.ValidatePath(name); err != nil {
		return err
	}
	if isProjectRootFile(name) || name == ".stego/state.yaml" {
		return nil
	}
	if !strings.HasPrefix(name, outputDir+"/") {
		return fmt.Errorf("transaction target %q is outside generated output", name)
	}
	return gen.ValidatePath(strings.TrimPrefix(name, outputDir+"/"))
}

func prepareTransaction(plan *Plan, relative string) (*transaction, error) {
	if plan.NewState.LastApplied == nil {
		return nil, fmt.Errorf("plan has no applied-state record")
	}
	tx := &transaction{Version: 1, OutputDir: filepath.ToSlash(relative), Expected: make(map[string]fileSnapshot)}
	for name, snapshot := range plan.snapshots {
		name = filepath.ToSlash(name)
		// Source inputs are checked before the transaction starts. Recovery
		// completes saved output and preserves later source edits.
		if !isProjectRootFile(name) && name != ".stego/state.yaml" && !strings.HasPrefix(name, tx.OutputDir+"/") {
			continue
		}
		tx.Expected[name] = snapshot
	}
	files := make(map[string][]byte)
	total := 0
	for _, file := range plan.GeneratedFiles {
		if _, exists := files[file.Path]; exists {
			return nil, fmt.Errorf("duplicate generated file %s", file.Path)
		}
		data := file.Bytes()
		if !isProjectRootFile(file.Path) && plan.NewState.LastApplied.Files[file.Path] != HashBytes(data) {
			return nil, fmt.Errorf("plan content does not match state for %s", file.Path)
		}
		files[file.Path] = data
	}
	for _, file := range plan.Files {
		name := filepath.ToSlash(projectFilePath(relative, file.Path))
		before, exists := tx.Expected[name]
		if !exists {
			return nil, fmt.Errorf("plan has no snapshot for %s", name)
		}
		if file.Action == ActionUnchanged {
			continue
		}
		op := transactionOperation{Path: name, Before: before}
		if file.Action == ActionDelete {
			if _, generated := files[file.Path]; generated {
				return nil, fmt.Errorf("plan deletes generated file %s", name)
			}
			if !before.Exists {
				continue
			}
		} else if file.Action == ActionGenerate || file.Action == ActionUpdate {
			data, exists := files[file.Path]
			if !exists {
				return nil, fmt.Errorf("plan has no content for %s", name)
			}
			total += len(data)
			if total > maxTrackedFileBytes {
				return nil, fmt.Errorf("apply content exceeds the %d-byte transaction limit", maxTrackedFileBytes)
			}
			op.After = fileSnapshot{Exists: true, Hash: HashBytes(data), Mode: defaultOutputMode()}
			if before.Exists {
				op.After.Mode = before.Mode.Perm()
			}
			op.Data = base64.StdEncoding.EncodeToString(data)
		} else {
			return nil, fmt.Errorf("unknown plan action %q", file.Action)
		}
		tx.Operations = append(tx.Operations, op)
	}
	stateData, err := yaml.Marshal(plan.NewState)
	if err != nil {
		return nil, err
	}
	before := tx.Expected[".stego/state.yaml"]
	after := fileSnapshot{Exists: true, Hash: HashBytes(stateData), Mode: defaultOutputMode()}
	if before.Exists {
		after.Mode = before.Mode.Perm()
	}
	if len(tx.Operations) == 0 && before == after {
		return nil, nil
	}
	if total+len(stateData) > maxTrackedFileBytes {
		return nil, fmt.Errorf("state exceeds the transaction content limit")
	}
	tx.Operations = append(tx.Operations, transactionOperation{
		Path: ".stego/state.yaml", Before: before, After: after, Data: base64.StdEncoding.EncodeToString(stateData),
	})
	return tx, nil
}

// validateTransaction checks the entire record before any output changes.
func validateTransaction(tx *transaction, projectDir string) (map[string][]byte, error) {
	if tx.Version != 1 {
		return nil, fmt.Errorf("unsupported transaction version %d", tx.Version)
	}
	if gen.ValidatePath(tx.OutputDir) != nil {
		return nil, fmt.Errorf("invalid transaction output directory")
	}
	if _, err := outputRelative(projectDir, filepath.Join(projectDir, filepath.FromSlash(tx.OutputDir))); err != nil {
		return nil, err
	}
	if len(tx.Operations) == 0 || len(tx.Operations) > maxTransactionFiles || len(tx.Expected) > maxTransactionFiles {
		return nil, fmt.Errorf("invalid transaction file count")
	}
	for name, snapshot := range tx.Expected {
		if err := validateTransactionPath(tx.OutputDir, name); err != nil {
			return nil, err
		}
		if !validSnapshot(snapshot) {
			return nil, fmt.Errorf("invalid expected snapshot for %s", name)
		}
	}
	if err := validateFileLayout(sortedKeys(tx.Expected)); err != nil {
		return nil, err
	}
	contents := make(map[string][]byte)
	final := make(map[string]fileSnapshot)
	for name, snapshot := range tx.Expected {
		final[name] = snapshot
	}
	seen := make(map[string]bool)
	total := 0
	for index, op := range tx.Operations {
		if err := validateTransactionPath(tx.OutputDir, op.Path); err != nil {
			return nil, err
		}
		if seen[op.Path] {
			return nil, fmt.Errorf("duplicate transaction target %s", op.Path)
		}
		seen[op.Path] = true
		if expected, exists := tx.Expected[op.Path]; !exists || expected != op.Before {
			return nil, fmt.Errorf("inconsistent transaction snapshot for %s", op.Path)
		}
		if !validSnapshot(op.Before) || !validSnapshot(op.After) {
			return nil, fmt.Errorf("invalid transaction snapshot for %s", op.Path)
		}
		if op.Path == ".stego/state.yaml" && (index != len(tx.Operations)-1 || !op.After.Exists) {
			return nil, fmt.Errorf("transaction must save state last")
		}
		if isProjectRootFile(op.Path) && !op.After.Exists {
			return nil, fmt.Errorf("transaction cannot delete the application module")
		}
		if op.After.Exists {
			if len(op.Data) > base64.StdEncoding.EncodedLen(maxTrackedFileBytes) {
				return nil, fmt.Errorf("transaction content exceeds limit")
			}
			data, err := base64.StdEncoding.Strict().DecodeString(op.Data)
			if err != nil || HashBytes(data) != op.After.Hash {
				return nil, fmt.Errorf("transaction content hash mismatch for %s", op.Path)
			}
			total += len(data)
			if total > maxTrackedFileBytes {
				return nil, fmt.Errorf("transaction content exceeds limit")
			}
			contents[op.Path] = data
		} else if op.Data != "" {
			return nil, fmt.Errorf("deleted transaction target has content")
		}
		final[op.Path] = op.After
	}
	stateData, exists := contents[".stego/state.yaml"]
	if !exists {
		return nil, fmt.Errorf("transaction has no state record")
	}
	state, err := decodeState(stateData, "transaction state")
	if err != nil {
		return nil, err
	}
	if state.LastApplied == nil {
		return nil, fmt.Errorf("transaction has no applied-state record")
	}
	for name, hash := range state.LastApplied.Files {
		if isProjectRootFile(name) {
			return nil, fmt.Errorf("transaction cannot claim ownership of the project module")
		}
		path := tx.OutputDir + "/" + name
		if !final[path].Exists || final[path].Hash != hash {
			return nil, fmt.Errorf("transaction state does not match output %s", path)
		}
	}
	for path, snapshot := range final {
		if strings.HasPrefix(path, tx.OutputDir+"/") && snapshot.Exists {
			name := strings.TrimPrefix(path, tx.OutputDir+"/")
			if state.LastApplied.Files[name] != snapshot.Hash {
				return nil, fmt.Errorf("transaction output is not tracked: %s", path)
			}
		}
	}
	return contents, nil
}

func syncParents(root *os.Root, name string) error {
	for directory := filepath.Dir(name); ; directory = filepath.Dir(directory) {
		if err := syncDirectory(root, directory); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if directory == "." {
			return nil
		}
	}
}

func runApplyFault(fault applyFault, point string) error {
	if fault == nil {
		return nil
	}
	return fault(point)
}

func executeTransaction(project *os.Root, projectDir string, tx *transaction, fault applyFault) error {
	contents, err := validateTransaction(tx, projectDir)
	if err != nil {
		return err
	}
	current := make(map[string]fileSnapshot)
	after := make(map[string]fileSnapshot)
	for _, op := range tx.Operations {
		after[op.Path] = op.After
	}
	for _, name := range sortedKeys(tx.Expected) {
		_, actual, err := readSnapshot(project, name, maxTrackedFileBytes, false)
		if err != nil {
			return err
		}
		if actual != tx.Expected[name] {
			if desired, exists := after[name]; !exists || actual != desired {
				return fmt.Errorf("recovery conflict at %s; preserve the file and inspect the transaction", name)
			}
		}
		current[name] = actual
	}
	if err := project.MkdirAll(tx.OutputDir, 0755); err != nil {
		return err
	}
	output, err := project.OpenRoot(tx.OutputDir)
	if err != nil {
		return err
	}
	defer output.Close()
	for index, op := range tx.Operations {
		root, name := project, op.Path
		if strings.HasPrefix(name, tx.OutputDir+"/") {
			root, name = output, strings.TrimPrefix(name, tx.OutputDir+"/")
		}
		if current[op.Path] != op.After {
			if op.After.Exists {
				if err := writeRootFileMode(root, name, contents[op.Path], op.After.Mode); err != nil {
					return err
				}
			} else if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if err := syncParents(root, name); err != nil {
			return err
		}
		if err := runApplyFault(fault, fmt.Sprintf("after_operation_%d", index)); err != nil {
			return err
		}
	}
	if err := syncParents(project, tx.OutputDir+"/entry"); err != nil {
		return err
	}
	if err := runApplyFault(fault, "before_complete"); err != nil {
		return err
	}
	final := make(map[string]fileSnapshot)
	for name, snapshot := range tx.Expected {
		final[name] = snapshot
	}
	for _, op := range tx.Operations {
		final[op.Path] = op.After
	}
	if err := verifySnapshots(project, final); err != nil {
		return err
	}
	if err := project.Remove(transactionPath); err != nil {
		return err
	}
	return syncParents(project, transactionPath)
}

func commitTransaction(project *os.Root, projectDir string, plan *Plan, relative string, fault applyFault) error {
	tx, err := prepareTransaction(plan, relative)
	if err != nil || tx == nil {
		return err
	}
	if _, err := validateTransaction(tx, projectDir); err != nil {
		return err
	}
	data, err := yaml.Marshal(tx)
	if err != nil {
		return err
	}
	if len(data) > maxTransactionBytes {
		return fmt.Errorf("transaction record exceeds size limit")
	}
	if err := runApplyFault(fault, "before_prepare"); err != nil {
		return err
	}
	if err := writeRootFileMode(project, transactionPath, data, 0600); err != nil {
		return err
	}
	if err := syncParents(project, transactionPath); err != nil {
		return errors.Join(ErrRecoveryRequired, err)
	}
	if err := runApplyFault(fault, "prepared"); err != nil {
		return errors.Join(ErrRecoveryRequired, err)
	}
	if err := executeTransaction(project, projectDir, tx, fault); err != nil {
		return errors.Join(ErrRecoveryRequired, err)
	}
	return nil
}

// Recover completes a saved apply. A conflicting file stops recovery before any
// write. Source declaration changes are preserved; the next plan uses them.
func Recover(projectDir string) error {
	project, err := os.OpenRoot(projectDir)
	if err != nil {
		return err
	}
	defer project.Close()
	if err := pendingTransaction(project); !errors.Is(err, ErrRecoveryRequired) {
		return err
	}
	lock, err := lockProject(project)
	if err != nil {
		return err
	}
	defer lock.Close()
	data, snapshot, err := readSnapshot(project, transactionPath, maxTransactionBytes, true)
	if err != nil {
		return err
	}
	if !snapshot.Exists {
		return nil
	}
	var tx transaction
	if err := parser.DecodeStrictWithLimit(data, transactionPath, &tx, maxTransactionBytes); err != nil {
		return err
	}
	if err := executeTransaction(project, projectDir, &tx, nil); err != nil {
		return errors.Join(ErrRecoveryRequired, err)
	}
	return nil
}
