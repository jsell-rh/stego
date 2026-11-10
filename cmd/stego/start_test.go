package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"

	"github.com/jsell-rh/stego/internal/registry"
)

// treeOf reads every file and folder under a folder, as projectFiles does, with the apply lock and with the
// target of each symbolic link.
func treeOf(t *testing.T, folder string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(folder, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(folder, name)
		switch {
		case entry.Type()&os.ModeSymlink != 0:
			target, err := os.Readlink(name)
			tree[relative+"@"] = target
			return err
		case entry.IsDir():
			tree[relative+"/"] = ""
			return nil
		}
		data, err := os.ReadFile(name)
		tree[relative] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// declaredArchetypes lists the archetypes that the fixture registry's own folders hold.
func declaredArchetypes(t *testing.T) []string {
	t.Helper()
	registryDir, _ := declaringRegistry(t)
	entries, err := os.ReadDir(filepath.Join(registryDir, "archetypes"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names
}

func readConfig(t *testing.T, project string) registry.RegistryConfig {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(project, ".stego", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var config registry.RegistryConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	return config
}

func TestStartAServiceDeclaration(t *testing.T) {
	t.Parallel()
	t.Run("ST-DECL-UC01-R02-T01 A scaffold refuses to run when a declaration is present, and changes no existing file", func(t *testing.T) {
		forms := map[string]func(t *testing.T, project, outside string){
			"a file": func(t *testing.T, project, _ string) { putFile(t, project, "service.yaml", "kind: service\n") },
			"a folder": func(t *testing.T, project, _ string) {
				if err := os.Mkdir(filepath.Join(project, "service.yaml"), 0755); err != nil {
					t.Fatal(err)
				}
			},
			"a symbolic link to a folder outside the project": func(t *testing.T, project, outside string) {
				if err := os.Symlink(outside, filepath.Join(project, "service.yaml")); err != nil {
					t.Fatal(err)
				}
			},
			"a dangling symbolic link": func(t *testing.T, project, outside string) {
				if err := os.Symlink(filepath.Join(outside, "missing"), filepath.Join(project, "service.yaml")); err != nil {
					t.Fatal(err)
				}
			},
		}
		for name, make := range forms {
			project, outside := t.TempDir(), t.TempDir()
			putFile(t, project, ".stego/config.yaml", declaringConfig(t))
			make(t, project, outside)
			before, beforeOutside := treeOf(t, project), treeOf(t, outside)
			run := stegoIn(t, project, nil, "init", "--archetype", "rest-crud")
			if run.code == 0 || !strings.Contains(run.stderr, "service.yaml") {
				t.Errorf("%s: want a refusal that names service.yaml, got exit %d: %s", name, run.code, run.stderr)
			}
			if !reflect.DeepEqual(before, treeOf(t, project)) || !reflect.DeepEqual(beforeOutside, treeOf(t, outside)) {
				t.Errorf("%s: the refused scaffold made, changed or removed a file", name)
			}
		}
		project := declaringProject(t)
		putFile(t, project, "README.md", "# todo\n")
		putFile(t, project, "fills/policy/fill.go", "package policy\n")
		before := treeOf(t, project)
		if run := stegoIn(t, project, nil, "init", "--archetype", "rest-crud"); run.code != 0 {
			t.Fatalf("stego init beside existing files: %s", run.stderr)
		}
		after := treeOf(t, project)
		if _, ok := after["service.yaml"]; !ok {
			t.Error("stego init wrote no declaration")
		}
		for name, data := range before {
			if after[name] != data {
				t.Errorf("stego init changed or removed %s", name)
			}
		}
	})

	t.Run("ST-DECL-UC01-R03-T01 An unknown archetype is refused, with the archetypes the registry holds", func(t *testing.T) {
		project := declaringProject(t)
		before := treeOf(t, project)
		run := stegoIn(t, project, nil, "init", "--archetype", "no-such-archetype")
		if run.code == 0 {
			t.Fatal("stego init accepted an unknown archetype")
		}
		for _, name := range append([]string{"no-such-archetype"}, declaredArchetypes(t)...) {
			if !strings.Contains(run.stderr, name) {
				t.Errorf("the refusal does not name %s: %s", name, run.stderr)
			}
		}
		if !reflect.DeepEqual(before, treeOf(t, project)) {
			t.Error("the refused scaffold wrote a file or folder")
		}
	})

	t.Run("ST-DECL-UC01-R04-T02 The rest-crud scaffold holds no example content", func(t *testing.T) {
		project := declaringProject(t)
		if run := stegoIn(t, project, nil, "init", "--archetype", "rest-crud"); run.code != 0 {
			t.Fatalf("stego init: %s", run.stderr)
		}
		data, err := os.ReadFile(filepath.Join(project, "service.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		var service map[string]any
		if err := yaml.Unmarshal(data, &service); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"entities", "collections", "slots"} {
			if value, ok := service[key]; ok && reflect.ValueOf(value).Len() != 0 {
				t.Errorf("the scaffold holds %s: %v", key, value)
			}
		}
		var paths []string
		for path := range treeOf(t, project) {
			paths = append(paths, path)
		}
		want := []string{"./", ".stego/", ".stego/apply.lock", ".stego/config.yaml", "fills/", "service.yaml"}
		if strings.Join(sortStrings(paths), " ") != strings.Join(want, " ") {
			t.Errorf("the project holds %v, want %v", sortStrings(paths), want)
		}
	})

	t.Run("ST-DECL-UC01-R06-T02 Init refuses while another process holds the apply lock, and writes nothing", func(t *testing.T) {
		project := declaringProject(t)
		putFile(t, project, ".stego/apply.lock", "")
		before := treeOf(t, project)
		// The helper is a process apart from stego that holds the lock, as a running apply does.
		helper := exec.Command("flock", "--exclusive", "--no-fork", filepath.Join(project, ".stego", "apply.lock"), "sleep", "60")
		if err := helper.Start(); err != nil {
			t.Fatal(err)
		}
		waitForLock(t, filepath.Join(project, ".stego", "apply.lock"))
		run := stegoIn(t, project, nil, "init", "--archetype", "rest-crud")
		_ = helper.Process.Kill()
		_ = helper.Wait()
		if run.code == 0 || !strings.Contains(run.stderr, "cannot lock project; another STEGO process may be active") {
			t.Fatalf("want the held-lock error of generation, got exit %d: %s", run.code, run.stderr)
		}
		if !reflect.DeepEqual(before, treeOf(t, project)) {
			t.Fatal("stego init wrote while the lock was held")
		}
		if run := stegoIn(t, project, nil, "init", "--archetype", "rest-crud"); run.code != 0 {
			t.Fatalf("stego init after the lock is released: %s", run.stderr)
		}
	})

	t.Run("ST-DECL-UC01-R06-T03 Init keeps an existing configuration byte for byte", func(t *testing.T) {
		registryDir, commit := declaringRegistry(t)
		config := "# The team's registry.\n\nregistry:   \n  - ref: " + commit + "   \n    url: " + registryDir + "\n\n# end\n"
		project := t.TempDir()
		putFile(t, project, ".stego/config.yaml", config)
		if run := stegoIn(t, project, nil, "init", "--archetype", "rest-crud"); run.code != 0 {
			t.Fatalf("stego init: %s", run.stderr)
		}
		data, err := os.ReadFile(filepath.Join(project, ".stego", "config.yaml"))
		if err != nil || string(data) != config {
			t.Fatalf("the configuration changed: %q, %v", data, err)
		}
	})

	t.Run("ST-DECL-UC01-R06-T04 A symbolic link at each path init would write is refused, naming the path", func(t *testing.T) {
		for _, path := range []string{"service.yaml", ".stego/config.yaml", ".stego", ".stego/apply.lock", "fills"} {
			project, outside := t.TempDir(), t.TempDir()
			if path != ".stego" {
				putFile(t, project, ".stego/config.yaml", declaringConfig(t))
			}
			if err := os.RemoveAll(filepath.Join(project, path)); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(project, path)); err != nil {
				t.Fatal(err)
			}
			before, beforeOutside := treeOf(t, project), treeOf(t, outside)
			run := stegoIn(t, project, nil, "init", "--archetype", "rest-crud")
			if run.code == 0 || !strings.Contains(run.stderr, path) {
				t.Errorf("%s: want a refusal that names the path, got exit %d: %s", path, run.code, run.stderr)
			}
			if !reflect.DeepEqual(before, treeOf(t, project)) || !reflect.DeepEqual(beforeOutside, treeOf(t, outside)) {
				t.Errorf("%s: the refused scaffold changed a file", path)
			}
		}
	})

	t.Run("ST-DECL-UC01-R06-T05 With no configuration, init writes one that names the default local registry folder", func(t *testing.T) {
		registryDir, _ := declaringRegistry(t)
		home := t.TempDir()
		configHome := filepath.Join(home, ".config")
		defaultFolder := filepath.Join(configHome, "stego", "registry")
		if err := os.MkdirAll(filepath.Dir(defaultFolder), 0755); err != nil {
			t.Fatal(err)
		}
		if output, err := exec.Command("cp", "-R", registryDir, defaultFolder).CombinedOutput(); err != nil {
			t.Fatalf("copy the registry: %v\n%s", err, output)
		}
		environment := []string{"HOME=" + home, "XDG_CONFIG_HOME=" + configHome}
		project := t.TempDir()
		if run := stegoIn(t, project, environment, "init", "--archetype", "rest-crud"); run.code != 0 {
			t.Fatalf("stego init: %s", run.stderr)
		}
		config := readConfig(t, project)
		if len(config.Registry) != 1 || config.Registry[0].URL != defaultFolder || config.Registry[0].Ref != "local" {
			t.Fatalf("the configuration names %+v, want the local source %s", config.Registry, defaultFolder)
		}
		if run := stegoIn(t, project, environment, "validate"); run.code != 0 {
			t.Fatalf("stego validate does not resolve the registry: %s%s", run.stdout, run.stderr)
		}
	})

	t.Run("ST-DECL-UC01-R06-T06 With no configuration and STEGO_REGISTRY set, init writes a configuration that names the override", func(t *testing.T) {
		registryDir, _ := declaringRegistry(t)
		project := t.TempDir()
		if run := stegoIn(t, project, []string{"STEGO_REGISTRY=" + registryDir}, "init", "--archetype", "rest-crud"); run.code != 0 {
			t.Fatalf("stego init: %s", run.stderr)
		}
		config := readConfig(t, project)
		if len(config.Registry) != 1 || config.Registry[0].URL != registryDir || config.Registry[0].Ref != "local" {
			t.Fatalf("the configuration names %+v, want the local source %s", config.Registry, registryDir)
		}
	})
}

// waitForLock waits until another process holds the lock on the file.
func waitForLock(t *testing.T, name string) {
	t.Helper()
	for range 500 {
		file, err := os.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		}
		file.Close()
		if err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the helper process never held the lock")
}

func sortStrings(values []string) []string {
	sorted := append([]string(nil), values...)
	sort.Strings(sorted)
	return sorted
}
