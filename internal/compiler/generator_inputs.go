package compiler

import (
	"fmt"
	"os"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
)

func captureGeneratorInputs(project, out string, names []string, snapshots map[string]fileSnapshot) (map[string][]byte, error) {
	if len(names) > 128 {
		return nil, fmt.Errorf("at most 128 generator inputs are allowed")
	}
	root, err := os.OpenRoot(project)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	inputs := make(map[string][]byte, len(names))
	total := 0
	for _, name := range names {
		if gen.ValidatePath(name) != nil || name == "" || name == out || strings.HasPrefix(name, out+"/") || name == ".stego" || strings.HasPrefix(name, ".stego/") {
			return nil, fmt.Errorf("invalid generator input %q", name)
		}
		if _, exists := inputs[name]; exists {
			return nil, fmt.Errorf("duplicate generator input %q", name)
		}
		data, snapshot, err := readSnapshot(root, name, 1<<20, true)
		if err != nil {
			return nil, err
		}
		if !snapshot.Exists {
			return nil, fmt.Errorf("generator input %q does not exist", name)
		}
		total += len(data)
		if total > 8<<20 {
			return nil, fmt.Errorf("generator inputs exceed 8 MiB")
		}
		if before, exists := snapshots[name]; exists && before != snapshot {
			return nil, fmt.Errorf("generator input %q changed during compilation", name)
		}
		inputs[name], snapshots[name] = data, snapshot
	}
	return inputs, nil
}
