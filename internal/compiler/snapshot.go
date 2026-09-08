package compiler

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/jsell-rh/stego/internal/parser"
)

const maxTrackedFileBytes = 64 << 20

type fileSnapshot struct {
	Exists bool
	Hash   string
	Mode   fs.FileMode
}

func readSnapshot(root *os.Root, name string, limit int64, keepData bool) ([]byte, fileSnapshot, error) {
	if err := checkFileTarget(root, name); err != nil {
		return nil, fileSnapshot{}, err
	}
	file, err := root.Open(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fileSnapshot{}, nil
	}
	if err != nil {
		return nil, fileSnapshot{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fileSnapshot{}, err
	}
	if !info.Mode().IsRegular() {
		return nil, fileSnapshot{}, fmt.Errorf("%s must be a regular file", name)
	}
	var data bytes.Buffer
	hash := sha256.New()
	var writer io.Writer = hash
	if keepData {
		writer = io.MultiWriter(hash, &data)
	}
	count, err := io.Copy(writer, io.LimitReader(file, limit+1))
	if err != nil {
		return nil, fileSnapshot{}, err
	}
	if count > limit {
		return nil, fileSnapshot{}, fmt.Errorf("%s exceeds the %d-byte tracking limit", name, limit)
	}
	return data.Bytes(), fileSnapshot{Exists: true, Hash: fmt.Sprintf("%x", hash.Sum(nil)), Mode: info.Mode()}, nil
}

func captureProjectInputs(projectDir string, serviceData []byte) (*State, map[string]fileSnapshot, error) {
	root, err := os.OpenRoot(projectDir)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	snapshots := make(map[string]fileSnapshot)
	state := &State{}
	for _, name := range []string{"service.yaml", "go.mod", ".stego/state.yaml", ".stego/config.yaml"} {
		data, snapshot, err := readSnapshot(root, name, parser.MaxDocumentBytes, name == ".stego/state.yaml")
		if err != nil {
			return nil, nil, err
		}
		snapshots[name] = snapshot
		if name == "service.yaml" && (!snapshot.Exists || snapshot.Hash != HashBytes(serviceData)) {
			return nil, nil, fmt.Errorf("service.yaml changed during compilation; run plan again")
		}
		if name == ".stego/state.yaml" && snapshot.Exists {
			state, err = decodeState(data, name)
			if err != nil {
				return nil, nil, err
			}
		}
	}
	return state, snapshots, nil
}

func verifySnapshots(root *os.Root, snapshots map[string]fileSnapshot) error {
	for _, name := range sortedKeys(snapshots) {
		_, actual, err := readSnapshot(root, name, maxTrackedFileBytes, false)
		if err != nil {
			return err
		}
		if actual != snapshots[name] {
			return fmt.Errorf("%s changed after planning; run plan again", name)
		}
	}
	return nil
}

func bindPlanInputs(plan *Plan, projectDir string, inputs map[string]fileSnapshot) error {
	root, err := os.OpenRoot(projectDir)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := verifySnapshots(root, inputs); err != nil {
		return err
	}
	for name, snapshot := range inputs {
		if prior, exists := plan.snapshots[name]; exists && prior != snapshot {
			return fmt.Errorf("%s changed during compilation; run plan again", name)
		}
		plan.snapshots[name] = snapshot
	}
	return nil
}

func verifyPlanLocation(plan *Plan, projectDir, outDir string) error {
	project, err := filepath.Abs(projectDir)
	if err != nil {
		return err
	}
	output, err := filepath.Abs(outDir)
	if err != nil {
		return err
	}
	if plan.snapshots == nil || plan.projectDir != project || plan.outDir != output {
		return fmt.Errorf("plan has no snapshot for this project and output directory; run plan again")
	}
	return nil
}
