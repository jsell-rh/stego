package compiler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/mod/modfile"
)

func TestDependencyResolutionIgnoresSavedSourceOverlay(t *testing.T) {
	input := snapshotTestInput(t)
	applyInitialSnapshot(t, input)
	write := func(name, data string) {
		t.Helper()
		if err := os.WriteFile(name, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	dependency := t.TempDir()
	write(filepath.Join(dependency, "go.mod"), "module example.com/overlay\ngo 1.26.8\n")
	write(filepath.Join(dependency, "overlay.go"), "package overlay\n")
	modulePath := filepath.Join(input.ProjectDir, "go.mod")
	raw, err := os.ReadFile(modulePath)
	if err != nil {
		t.Fatal(err)
	}
	module, err := modfile.Parse(modulePath, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := module.AddReplace("example.com/overlay", "", dependency, ""); err != nil {
		t.Fatal(err)
	}
	raw, err = module.Format()
	if err != nil {
		t.Fatal(err)
	}
	write(modulePath, string(raw))
	domain := filepath.Join(input.ProjectDir, "domain.go")
	const source = "package domain\n"
	write(domain, source)
	outside := t.TempDir()
	virtual := filepath.Join(outside, "virtual.go")
	write(virtual, "package domain\nimport _ \"example.com/overlay\"\n")
	overlay := filepath.Join(outside, "overlay.json")
	encoded, err := json.Marshal(map[string]any{"Replace": map[string]string{domain: virtual}})
	if err != nil {
		t.Fatal(err)
	}
	write(overlay, string(encoded))
	config := filepath.Join(outside, "go.env")
	// The saved source overlay must not reach any dependency command. The
	// module and source snapshots contain the real file, not this overlay.
	write(config, "GOFLAGS=-overlay="+overlay+"\nGOPROXY=off\n")
	t.Setenv("GOENV", config)
	t.Setenv("GOPROXY", "")
	t.Setenv("GOTOOLCHAIN", "local")
	t.Setenv("GOFLAGS", "-stego-invalid-ambient-flag")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := ResolveDependencies(ctx, input); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(modulePath)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := modfile.Parse(modulePath, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, requirement := range resolved.Require {
		if requirement.Mod.Path == "example.com/overlay" {
			t.Fatal("saved GOFLAGS imported source outside the dependency snapshot")
		}
	}
	actual, err := os.ReadFile(domain)
	if err != nil || string(actual) != source {
		t.Fatal("dependency resolution changed application source", err)
	}
}

func TestDependencyCommandsPreserveSavedProxyConfiguration(t *testing.T) {
	var requests atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusGone)
	}))
	defer proxy.Close()
	directory := t.TempDir()
	for name, data := range map[string]string{
		"go.mod":    "module example.com/proxycheck\ngo 1.26.8\nrequire example.invalid/stego-proxy-check v1.0.0\n",
		"domain.go": "package proxycheck\nimport _ \"example.invalid/stego-proxy-check\"\n",
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	config := filepath.Join(t.TempDir(), "go.env")
	if err := os.WriteFile(config, []byte("GOPROXY="+proxy.URL+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOENV", config)
	t.Setenv("GOPROXY", "")
	t.Setenv("GONOPROXY", "none")
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := runDependencyCommand(ctx, directory, filepath.Join(directory, "go.mod"), "mod", "tidy")
	if err == nil || requests.Load() == 0 || ctx.Err() != nil {
		t.Fatal("dependency command did not use the saved proxy configuration", requests.Load(), err)
	}
}
