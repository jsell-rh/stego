package appbuild

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jsell-rh/stego/internal/compiler"
)

// ResolveDependencies resolves the current service with Go's module tools.
// Apply must first bring generated code up to date. The module files change
// only after resolution and validation succeed, through Generating's apply
// journal (compiler.CommitResolvedModules), so recovery can complete them.
func ResolveDependencies(ctx context.Context, input compiler.ReconcilerInput) error {
	return compiler.CommitResolvedModules(ctx, input, resolveModules)
}

// resolveModules runs the module tools that resolve the staged go.mod and go.sum.
func resolveModules(ctx context.Context, directory, modPath string) error {
	for _, args := range [][]string{{"mod", "tidy"}, {"mod", "verify"}, {"list", "-mod=readonly", "-deps", "-test", "./..."}, {"mod", "tidy", "-diff"}} {
		if err := runDependencyCommand(ctx, directory, modPath, args...); err != nil {
			return err
		}
	}
	return nil
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
	// Use this module. Do not inherit workspace or build flags. An empty
	// GOFLAGS value falls back to saved Go settings. A space parses as no
	// flags and still permits saved private proxy and module settings.
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS= ")
	cmd.WaitDelay = time.Second
	var output boundedCommandOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go %s failed: %w\n%s", strings.Join(args[:2], " "), err, output.String())
	}
	return nil
}
