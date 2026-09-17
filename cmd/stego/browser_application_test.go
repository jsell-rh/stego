package main

import (
	"bytes"
	"crypto/sha256"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPrivateBrowserApplicationCommands(t *testing.T) {
	registry, err := filepath.Abs("../../registry")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	t.Setenv("STEGO_REGISTRY", registry)
	t.Setenv("STEGO_MODULE", "example.com/private-browser")
	t.Setenv("STEGO_GO_VERSION", "1.26.8")
	if err := os.Mkdir("ui", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("ui/index.html", []byte(`<html><head></head><body>Private application</body></html>`), 0600); err != nil {
		t.Fatal(err)
	}
	declaration := `kind: service
name: private-browser
archetype: browser-service
language: go
overrides:
  health-check:
    database: true
  browser-backend:
    api_prefix: /api/v1
    routes: [/]
    assets: [{source: ui/index.html, path: /index.html}]
  kubernetes-service:
    local_applications:
      - port: 8000
        image: registry.example.test/application@sha256:` + strings.Repeat("a", 64) + `
        listen_env: LISTEN_ADDRESS
        port_env: PORT
        env_secret: application-env
        files_secret: application-files
        health_path: /api/v1/readyz
`
	if err := os.WriteFile("service.yaml", []byte(declaration), 0600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []func([]string) error{runValidate, runApply, runApply, runDrift} {
		if err := command(nil); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(".stego/state.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := runApply(nil); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(".stego/state.yaml")
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("repeated application changed compiler state", err)
	}
	for _, name := range []string{"out/browser/socket.go", "out/health/application.go", "out/deploy/render/manifest.json.tmpl", "out/browsertelemetry/index.js", "out/browsertelemetry/index.d.ts", "out/browsertelemetry/package.json"} {
		if _, err := os.Stat(name); err != nil {
			t.Fatal(err)
		}
	}
	for file, identity := range map[string]string{
		"out/browsertelemetry/index.js": `const serviceName="private-browser";`,
		"out/browser/backend.go":        `\"TelemetryService\":\"private-browser\"`,
	} {
		content, err := os.ReadFile(file)
		if err != nil || !bytes.Contains(content, []byte(identity)) {
			t.Fatal("common browser archetype lost its shared telemetry identity", file, err)
		}
	}
	snapshot := func() map[string][32]byte {
		t.Helper()
		files := map[string][32]byte{}
		if err := filepath.WalkDir("out", func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			data, err := os.ReadFile(path)
			if err == nil {
				files[path] = sha256.Sum256(data)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return files
	}
	outputs := snapshot()
	conflict := strings.Replace(declaration, "overrides:\n", "overrides:\n  browser-telemetry:\n    service_name: first-browser\n", 1)
	conflict = strings.Replace(conflict, "  browser-backend:\n", "  browser-backend:\n    telemetry_service_name: second-browser\n", 1)
	if err := os.WriteFile("service.yaml", []byte(conflict), 0600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []func([]string) error{runValidate, runApply} {
		if err := command(nil); err == nil {
			t.Fatal("conflicting browser identity was accepted")
		}
		state, err := os.ReadFile(".stego/state.yaml")
		if err != nil || !bytes.Equal(state, before) || !reflect.DeepEqual(outputs, snapshot()) {
			t.Fatal("conflicting browser identity changed generated output or state", err)
		}
	}
	for _, change := range []struct{ old, new string }{{"port: 8000", "port: 8443"}, {"files_secret: application-files", "files_secret: private-browser-files"}, {"database: true", "database: false"}} {
		bad := strings.Replace(declaration, change.old, change.new, 1)
		if err := os.WriteFile("service.yaml", []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if err := runApply(nil); err == nil {
			t.Fatal("unsafe application declaration was applied")
		}
		after, err := os.ReadFile(".stego/state.yaml")
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("failed declaration changed state", err)
		}
	}
	// A valid declaration change must update every consumer of the shared port.
	changed := strings.Replace(declaration, "port: 8000", "port: 8001", 1)
	changed = strings.Replace(changed, "health_path: /api/v1/readyz", "health_path: /api/v1/health", 1)
	if err := os.WriteFile("service.yaml", []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runApply(nil); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct{ file, value string }{
		{"out/browser/client/client.go", "127.0.0.1:8001"},
		{"out/health/application.go", "/api/v1/health"},
		{"out/deploy/render/manifest.json.tmpl", `"value": "8001"`},
	} {
		content, err := os.ReadFile(check.file)
		if err != nil || !strings.Contains(string(content), check.value) {
			t.Fatalf("shared declaration did not reach %s: %v", check.file, err)
		}
	}
	if err := runDrift(nil); err != nil {
		t.Fatal(err)
	}

}
