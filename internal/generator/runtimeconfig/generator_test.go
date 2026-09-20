package runtimeconfig

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

//go:embed testdata/configuration_test.go
var runtimeTests []byte

func fixture() map[string]any {
	var result map[string]any
	_ = json.Unmarshal([]byte(`{"groups":[{"name":"Worker","fields":[
 {"name":"Address","env":"WIDGET_ADDRESS","type":"string","min_length":1,"max_length":256},
 {"name":"WatchLimit","env":"WIDGET_WATCH_LIMIT","type":"integer","min":"0","max":"1000","default":"0"},
 {"name":"Resync","env":"WIDGET_RESYNC","type":"duration","min":"1ms","max":"1h","default":"30s"},
 {"name":"Enabled","env":"WIDGET_ENABLED","type":"boolean","default":"false"},
 {"name":"Optional","env":"WIDGET_OPTIONAL","type":"string","min_length":0,"max_length":32,"default":""}
 ]},{"name":"Storage","fields":[{"name":"Address","env":"WIDGET_ADDRESS","type":"string","min_length":1,"max_length":256}]}]}`), &result)
	return result
}

func TestConfigurationDeclaration(t *testing.T) {
	for name, change := range map[string]func(map[string]any, map[string]any){
		"unknown root":        func(c, f map[string]any) { c["passthrough"] = true },
		"alternate root case": func(c, f map[string]any) { c["Groups"] = c["groups"]; delete(c, "groups") },
		"alternate group case": func(c, f map[string]any) {
			g := c["groups"].([]any)[0].(map[string]any)
			g["Name"] = g["name"]
			delete(g, "name")
		},
		"alternate field case": func(c, f map[string]any) { f["Name"] = f["name"]; delete(f, "name") },
		"duplicate key case":   func(c, f map[string]any) { f["Name"] = "Different" },
		"invalid UTF8 default": func(c, f map[string]any) { f["default"] = string([]byte{0xff}) },
		"nested default":       func(c, f map[string]any) { f["default"] = map[string]any{"private-sentinel": "value"} },
		"unknown field":        func(c, f map[string]any) { f["unknown"] = "private-sentinel" },
		"invalid name":         func(c, f map[string]any) { f["name"] = "private-sentinel" },
		"method collision":     func(c, f map[string]any) { f["name"] = "Format" },
		"invalid env":          func(c, f map[string]any) { f["env"] = "private-sentinel" },
		"invalid type":         func(c, f map[string]any) { f["type"] = "private-sentinel" },
		"invalid default":      func(c, f map[string]any) { f["default"] = "private-sentinel\n" },
		"null default":         func(c, f map[string]any) { f["default"] = nil },
		"number default":       func(c, f map[string]any) { f["default"] = 3 },
		"missing bounds":       func(c, f map[string]any) { delete(f, "max_length") },
		"reversed bounds":      func(c, f map[string]any) { f["min_length"] = 300 },
		"excessive bound":      func(c, f map[string]any) { f["max_length"] = 4097 },
		"wrong bound":          func(c, f map[string]any) { f["min"] = "1" },
		"duplicate field":      func(c, f map[string]any) { f["name"] = "Enabled" },
		"duplicate env":        func(c, f map[string]any) { f["env"] = "WIDGET_ENABLED" },
		"empty groups":         func(c, f map[string]any) { c["groups"] = []any{} },
		"duplicate group": func(c, f map[string]any) {
			groups := c["groups"].([]any)
			groups[1].(map[string]any)["name"] = "Worker"
		},
		"loader collision": func(c, f map[string]any) {
			groups := c["groups"].([]any)
			groups[1].(map[string]any)["name"] = "LoadWorker"
		},
		"reserved group":      func(c, f map[string]any) { c["groups"].([]any)[0].(map[string]any)["name"] = "Error" },
		"empty fields":        func(c, f map[string]any) { c["groups"].([]any)[0].(map[string]any)["fields"] = []any{} },
		"unknown group field": func(c, f map[string]any) { c["groups"].([]any)[0].(map[string]any)["extra"] = true },
	} {
		t.Run(name, func(t *testing.T) {
			config := fixture()
			field := config["groups"].([]any)[0].(map[string]any)["fields"].([]any)[0].(map[string]any)
			change(config, field)
			ctx := gen.Context{OutputNamespace: "settings", ComponentConfig: config}
			if err := new(Generator).ValidateContext(ctx); err == nil || strings.Contains(err.Error(), "private-sentinel") {
				t.Fatal("preflight accepted invalid input or exposed its value")
			}
			files, wiring, err := new(Generator).Generate(ctx)
			if err == nil || files != nil || wiring != nil || strings.Contains(err.Error(), "private-sentinel") {
				t.Fatal("invalid declaration produced output or exposed its value")
			}
		})
	}
}

func TestConfigurationNumericContracts(t *testing.T) {
	for _, f := range []field{
		{Type: "integer", Min: pointer("-9223372036854775808"), Max: pointer("9223372036854775807"), Default: pointer("0")},
		{Type: "duration", Min: pointer("-1h"), Max: pointer("1h"), Default: pointer("1.5ms")},
		{Type: "boolean", Default: pointer("true")},
	} {
		if !validField(f) {
			t.Fatal("valid typed declaration rejected")
		}
	}
	for _, f := range []field{
		{Type: "integer", Min: pointer("0"), Max: pointer("9223372036854775808")},
		{Type: "integer", Min: pointer("01"), Max: pointer("100")},
		{Type: "integer", Min: pointer("0"), Max: pointer("10"), Default: pointer("11")},
		{Type: "duration", Min: pointer("1s"), Max: pointer("0s")},
		{Type: "duration", Min: pointer("0s"), Max: pointer("1h"), Default: pointer("0" + strings.Repeat("0", 64) + "s")},
		{Type: "boolean", Default: pointer("1")},
	} {
		if validField(f) {
			t.Fatal("invalid typed declaration accepted")
		}
	}
}

func pointer(value string) *string { return &value }

func TestGeneratedConfiguration(t *testing.T) {
	for _, namespace := range []string{"settings", "libraries/configuration"} {
		t.Run(namespace, func(t *testing.T) {
			ctx := gen.Context{OutputNamespace: namespace, ComponentConfig: fixture()}
			files, wiring, err := new(Generator).Generate(ctx)
			if err != nil {
				t.Fatal(err)
			}
			repeated, _, err := new(Generator).Generate(ctx)
			if err != nil || len(files) != 1 || len(repeated) != 1 || !bytes.Equal(files[0].Bytes(), repeated[0].Bytes()) || len(wiring.GoModRequires) != 0 {
				t.Fatal("generation is not stable and dependency-free")
			}
			project := t.TempDir()
			packageName := filepath.Base(namespace)
			contents := map[string][]byte{"go.mod": []byte("module example.com/widget\ngo 1.26.8\n"), files[0].Path: files[0].Bytes(), namespace + "/configuration_test.go": bytes.Replace(runtimeTests, []byte("package settings"), []byte("package "+packageName), 1)}
			for name, content := range contents {
				file := filepath.Join(project, name)
				if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, content, 0600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("go", "test", "-json", "-race", "-count=1", "-mod=readonly", "-timeout=30s", "./...")
			cmd.Dir = project
			cmd.Env = append(os.Environ(), "GOWORK=off", "GOMAXPROCS=2")
			output, err := cmd.CombinedOutput()
			t.Log(string(output))
			if err != nil {
				t.Fatal("generated configuration test failed", err)
			}
		})
	}
}
