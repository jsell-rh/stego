package compiler

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jsell-rh/stego/internal/gen"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
)

type dependencyRunner func(context.Context, string, string, ...string) error

// ResolveDependencies resolves the current service with Go's module tools.
// Apply must first bring generated code up to date. The module files change
// only after resolution and validation succeed. Recovery uses the apply journal.
func ResolveDependencies(ctx context.Context, input ReconcilerInput) error {
	return resolveDependencies(ctx, input, runDependencyCommand, nil)
}

func resolveDependencies(ctx context.Context, input ReconcilerInput, run dependencyRunner, fault applyFault) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	plan, err := Reconcile(input)
	if err != nil {
		return err
	}
	if plan.HasChanges() {
		return fmt.Errorf("generated code is not current; run 'stego apply' before 'stego deps'")
	}
	project, err := os.OpenRoot(plan.projectDir)
	if err != nil {
		return err
	}
	defer project.Close()
	lock, err := lockProject(project)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := pendingTransaction(project); err != nil {
		return err
	}
	if err := verifySnapshots(project, plan.snapshots); err != nil {
		return err
	}
	if err := plan.verifyRegistry(); err != nil {
		return err
	}
	moduleData, _, err := readSnapshot(project, "go.mod", maxTrackedFileBytes, true)
	if err != nil {
		return err
	}
	module, err := modfile.Parse("go.mod", moduleData, nil)
	if err != nil {
		return err
	}
	roots := []string{plan.projectDir}
	for _, replace := range module.Replace {
		if replace.New.Version == "" {
			root := replace.New.Path
			if !filepath.IsAbs(root) {
				root = filepath.Join(plan.projectDir, root)
			}
			roots = append(roots, filepath.Clean(root))
		}
	}
	before, err := dependencyInputs(ctx, roots)
	if err != nil {
		return err
	}
	// Keep temporary module files outside the application's package tree.
	stage, err := os.MkdirTemp("", "stego-deps-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	stageRoot, err := os.OpenRoot(stage)
	if err != nil {
		return err
	}
	defer stageRoot.Close()
	for _, name := range []string{"go.mod", "go.sum"} {
		data, snapshot, err := readSnapshot(project, name, maxTrackedFileBytes, true)
		if err != nil {
			return err
		}
		if snapshot.Exists {
			if err := writeRootFileMode(stageRoot, name, data, 0600); err != nil {
				return err
			}
		}
	}
	modPath := filepath.Join(stage, "go.mod")
	for _, args := range [][]string{{"mod", "tidy"}, {"mod", "verify"}, {"list", "-mod=readonly", "-deps", "-test", "./..."}, {"mod", "tidy", "-diff"}} {
		if err := run(ctx, plan.projectDir, modPath, args...); err != nil {
			return err
		}
	}
	resolved := make(map[string][]byte)
	for _, name := range []string{"go.mod", "go.sum"} {
		data, snapshot, err := readSnapshot(stageRoot, name, maxTrackedFileBytes, true)
		if err != nil {
			return err
		}
		if name == "go.mod" && !snapshot.Exists {
			return fmt.Errorf("dependency resolution removed go.mod")
		}
		resolved[name] = data
	}
	if err := validateResolvedModule(module, resolved["go.mod"], plan.requirements); err != nil {
		return err
	}
	after, err := dependencyInputs(ctx, roots)
	if err != nil {
		return err
	}
	if !maps.Equal(before, after) {
		return fmt.Errorf("Go source or module inputs changed during dependency resolution; run 'stego deps' again")
	}
	if err := verifySnapshots(project, plan.snapshots); err != nil {
		return err
	}
	if err := plan.verifyRegistry(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Use the same transaction format and conflict checks as apply. Module
	// files stay outside generated-file ownership in the saved state.
	for _, name := range []string{"go.mod", "go.sum"} {
		data := resolved[name]
		found := false
		for i := range plan.GeneratedFiles {
			if plan.GeneratedFiles[i].Path == name {
				plan.GeneratedFiles[i].Content = data
				found = true
			}
		}
		if !found {
			plan.GeneratedFiles = append(plan.GeneratedFiles, gen.File{Path: name, Content: data})
		}
		action := ActionUpdate
		if !plan.snapshots[name].Exists {
			action = ActionGenerate
		} else if plan.snapshots[name].Hash == HashBytes(data) {
			action = ActionUnchanged
		}
		found = false
		for i := range plan.Files {
			if plan.Files[i].Path == name {
				plan.Files[i].Action = action
				found = true
			}
		}
		if !found {
			plan.Files = append(plan.Files, PlannedFile{Path: name, Action: action})
		}
	}
	relative, err := outputRelative(plan.projectDir, plan.outDir)
	if err != nil {
		return err
	}
	return commitTransaction(project, plan.projectDir, plan, relative, fault)
}

func validateResolvedModule(before *modfile.File, data []byte, required map[string]string) error {
	after, err := modfile.Parse("resolved go.mod", data, nil)
	if err != nil {
		return err
	}
	if before.Module == nil || after.Module == nil || before.Module.Mod.Path != after.Module.Mod.Path {
		return fmt.Errorf("dependency resolution changed the module name")
	}
	versions := make(map[string]string)
	for _, req := range after.Require {
		versions[req.Mod.Path] = req.Mod.Version
	}
	for _, name := range sortedKeys(required) {
		if semver.Compare(versions[name], required[name]) < 0 {
			return fmt.Errorf("resolved module does not satisfy component requirement %s %s", name, required[name])
		}
	}
	return nil
}

// dependencyInputs records files that can change package imports. It also
// records local replacement modules. Include hidden and testdata directories:
// an explicit import can use packages outside Go's normal package scan.
func dependencyInputs(ctx context.Context, roots []string) (map[string]fileSnapshot, error) {
	inputs := make(map[string]fileSnapshot)
	var totalBytes int64
	for _, directory := range roots {
		root, err := os.OpenRoot(directory)
		if err != nil {
			return nil, err
		}
		err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			base := entry.Name()
			if entry.IsDir() {
				if name != "." && base == ".git" {
					return fs.SkipDir
				}
				return nil
			}
			if base != "go.mod" && base != "go.sum" && !strings.HasSuffix(base, ".go") {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			totalBytes += info.Size()
			if totalBytes > 256<<20 {
				return fmt.Errorf("dependency input size exceeds 256 MiB")
			}
			_, snapshot, err := readSnapshot(root, name, maxTrackedFileBytes, false)
			if err != nil {
				return err
			}
			inputs[filepath.Join(directory, name)] = snapshot
			if len(inputs) > maxTransactionFiles {
				return fmt.Errorf("dependency input file count exceeds %d", maxTransactionFiles)
			}
			return nil
		})
		root.Close()
		if err != nil {
			return nil, err
		}
	}
	return inputs, nil
}

type boundedCommandOutput struct{ buffer bytes.Buffer }

func (b *boundedCommandOutput) String() string { return b.buffer.String() }

func (b *boundedCommandOutput) Write(data []byte) (int, error) {
	const limit = 64 << 10
	length := len(data)
	if remaining := limit - b.buffer.Len(); remaining > 0 {
		b.buffer.Write(data[:min(remaining, length)])
	}
	return length, nil
}

func runDependencyCommand(ctx context.Context, directory, modPath string, args ...string) error {
	args = append(append([]string{}, args[:2]...), append([]string{"-modfile=" + modPath}, args[2:]...)...)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = directory
	// Use this module. Do not inherit workspace or alternate-module flags.
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	cmd.WaitDelay = time.Second
	var output boundedCommandOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go %s failed: %w\n%s", strings.Join(args[:2], " "), err, output.String())
	}
	return nil
}
