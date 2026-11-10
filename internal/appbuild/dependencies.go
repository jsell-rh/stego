package appbuild

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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

// resolveModules runs the module tools that resolve the staged go.mod and go.sum. A runtime proxy folder, which
// Generating stages from the common registry's runtime folder at the archetype's pin, goes before the module
// proxies that the Go configuration names, so the runtime pin resolves with no fetch of its module path's host.
func resolveModules(ctx context.Context, directory, modPath, runtimeProxy string) error {
	var env []string
	if runtimeProxy != "" {
		configured, err := dependencyOutput(ctx, directory, nil, "env", "GOPROXY")
		if err != nil {
			return err
		}
		proxy := (&url.URL{Scheme: "file", Path: filepath.ToSlash(runtimeProxy)}).String()
		if configured = strings.TrimSpace(configured); configured != "" {
			proxy += "," + configured
		}
		env = append(env, "GOPROXY="+proxy)
	}
	for _, args := range [][]string{{"mod", "tidy"}, {"mod", "verify"}, {"list", "-mod=readonly", "-deps", "-test", "./..."}, {"mod", "tidy", "-diff"}} {
		if err := runDependencyCommand(ctx, directory, modPath, env, args...); err != nil {
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

func runDependencyCommand(ctx context.Context, directory, modPath string, env []string, args ...string) error {
	args = append(append([]string{}, args[:2]...), append([]string{"-modfile=" + modPath}, args[2:]...)...)
	_, err := dependencyOutput(ctx, directory, env, args...)
	return err
}

// dependencyOutput runs one go command in the project folder and gives its output.
func dependencyOutput(ctx context.Context, directory string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = directory
	// Use this module. Do not inherit workspace or build flags. An empty
	// GOFLAGS value falls back to saved Go settings. A space parses as no
	// flags and still permits saved private proxy and module settings.
	cmd.Env = append(append(os.Environ(), "GOWORK=off", "GOFLAGS= "), env...)
	cmd.WaitDelay = time.Second
	var output boundedCommandOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go %s failed: %w\n%s", strings.Join(args[:2], " "), err, output.String())
	}
	return output.String(), nil
}
