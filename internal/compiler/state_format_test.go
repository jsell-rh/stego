package compiler

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/jsell-rh/stego/internal/parser"
	"gopkg.in/yaml.v3"
)

func TestStateFormatRejectsUnsupportedVersionsBeforeWrites(t *testing.T) {
	for _, value := range []string{"-1", "2", "999999999999999999999999", "null", "1.0", "true", "\"1\"", "[1]"} {
		t.Run(value, func(t *testing.T) {
			if _, err := decodeState([]byte("format_version: "+value+"\n"), "state.yaml"); err == nil {
				t.Fatal("invalid state version was accepted")
			}
		})
	}
	for _, version := range []int{-1, 2} {
		path := filepath.Join(t.TempDir(), "absent", "state.yaml")
		if err := SaveState(path, &State{FormatVersion: version}); err == nil {
			t.Fatal("unsupported state was saved")
		}
		if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
			t.Fatal("rejected state created a directory", err)
		}
	}
}

func TestStateFormatLegacyReadAndSave(t *testing.T) {
	for _, prefix := range []string{"", "format_version: 0\n", "format_version: 1\n"} {
		t.Run(fmt.Sprintf("prefix-%q", prefix), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.yaml")
			original := []byte(prefix + "last_applied:\n  service_hash: original\n  files: {main.go: original}\n")
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			state, err := LoadState(path)
			if err != nil {
				t.Fatal(err)
			}
			before := state.FormatVersion
			unchanged, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(unchanged, original) {
				t.Fatal("read changed state", err)
			}
			if err := SaveState(path, state); err != nil {
				t.Fatal(err)
			}
			if state.FormatVersion != before {
				t.Fatal("save changed caller state")
			}
			current, err := LoadState(path)
			if err != nil {
				t.Fatal(err)
			}
			if current.FormatVersion != StateFormatVersion || current.LastApplied.ServiceHash != "original" || current.LastApplied.Files["main.go"] != "original" {
				t.Fatal("save did not preserve legacy records")
			}
		})
	}
}

func TestStateFormatUpgradeUsesRecoveryJournal(t *testing.T) {
	input := snapshotTestInput(t)
	applyInitialSnapshot(t, input)
	applyInitialSnapshot(t, input)
	path := filepath.Join(input.ProjectDir, ".stego/state.yaml")
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	legacy := bytes.TrimPrefix(current, []byte("format_version: 1\n"))
	if bytes.Equal(legacy, current) {
		t.Fatal("new state has no format version")
	}
	if err := os.WriteFile(path, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := Reconcile(input)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.StateChanged || !plan.HasChanges() {
		t.Fatal("legacy upgrade is absent from the plan")
	}
	for _, file := range plan.Files {
		if file.Action != ActionUnchanged {
			t.Fatal("format upgrade changes generated files", file.Path)
		}
	}
	prepareInterruptedApply(t, input, "prepared")
	unchanged, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(unchanged, legacy) {
		t.Fatal("journal preparation changed legacy state", err)
	}
	if err := Recover(input.ProjectDir); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(restored, current) {
		t.Fatal("recovery did not save the current state", err)
	}
	plan, err = Reconcile(input)
	if err != nil || plan.HasChanges() {
		t.Fatal("repeated plan has changes after upgrade", err)
	}
}

func TestStateFormatRecoveryPreservesLegacyAndRejectsFutureRecords(t *testing.T) {
	for _, version := range []int{0, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			input := transactionInput(t)
			prepareInterruptedApply(t, input, "prepared")
			path := filepath.Join(input.ProjectDir, transactionPath)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var tx transaction
			if err := parser.DecodeStrictWithLimit(original, path, &tx, maxTransactionBytes); err != nil {
				t.Fatal(err)
			}
			op := &tx.Operations[len(tx.Operations)-1]
			data, err := base64.StdEncoding.DecodeString(op.Data)
			if err != nil {
				t.Fatal(err)
			}
			data = bytes.Replace(data, []byte("format_version: 1\n"), nil, 1)
			if version != 0 {
				data = append([]byte(fmt.Sprintf("format_version: %d\n", version)), data...)
			}
			op.Data = base64.StdEncoding.EncodeToString(data)
			op.After.Hash = HashBytes(data)
			modified, err := yaml.Marshal(&tx)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, modified, 0600); err != nil {
				t.Fatal(err)
			}
			outputPath := filepath.Join(input.ProjectDir, "out/internal/api/a.go")
			before, err := os.ReadFile(outputPath)
			if err != nil {
				t.Fatal(err)
			}
			err = Recover(input.ProjectDir)
			if version == 0 {
				if err != nil {
					t.Fatal(err)
				}
				actual, err := os.ReadFile(filepath.Join(input.ProjectDir, ".stego/state.yaml"))
				if err != nil || !bytes.Equal(actual, data) {
					t.Fatal("recovery changed legacy journal bytes", err)
				}
				plan, err := Reconcile(input)
				if err != nil || !plan.StateChanged {
					t.Fatal("legacy journal recovery did not retain the later upgrade", err)
				}
			} else {
				if err == nil {
					t.Fatal("future state was recovered")
				}
				actual, err := os.ReadFile(outputPath)
				if err != nil || !bytes.Equal(actual, before) {
					t.Fatal("future state changed output", err)
				}
				retained, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(retained, modified) {
					t.Fatal("rejected journal was not retained", err)
				}
			}
		})
	}
}
