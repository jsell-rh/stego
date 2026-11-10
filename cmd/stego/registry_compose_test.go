package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The Supplying fixtures ST-SUP-TD01 and ST-SUP-TD02. Each test writes the
// registry files into a new folder, commits them with Git, and pins the
// commit in the project's .stego/config.yaml by a file:// address, so that
// stego clones the pinned commit as it clones a remote registry.

// commonRegistryFiles are the files of the common registry of ST-SUP-TD01.
func commonRegistryFiles() map[string]string {
	component := func(name, namespace, provides, requires, slots string) string {
		return "kind: component\nname: " + name + "\nversion: 1.0.0\nstage: ga\noutput_namespace: " + namespace +
			"\n\nconfig:\n  enabled:\n    type: bool\n\nrequires: " + requires + "\n\nprovides: " + provides + "\n\nslots: " + slots + "\n"
	}
	return map[string]string{
		"common/types.proto": "syntax = \"proto3\";\n\npackage stego.common;\n",
		"ports/store-port/port.yaml": "kind: port\nname: store-port\nundertakes: Stores and reads one entity by its identity.\n" +
			"fields:\n  - name: id\n    meaning: The identity of the entity.\n  - name: version\n    meaning: A count of the changes to the entity that starts at 1.\nconformance: conformance/store-port\n",
		"ports/auth-port/port.yaml": "kind: port\nname: auth-port\nundertakes: Names the caller of a request.\n" +
			"fields:\n  - name: subject\n    meaning: The identity of the caller.\nconformance: conformance/auth-port\n",
		"components/store-a/component.yaml": component("store-a", "internal/store", "[store-port]", "[]", "[]"),
		"components/auth-a/component.yaml":  component("auth-a", "internal/auth", "[auth-port]", "[]", "[]"),
		"components/api-a/component.yaml":   component("api-a", "internal/api", "[]", "[store-port, auth-port]", "\n  - name: before_create\n    proto: stego.components.api_a.slots.BeforeCreate\n"),
		"components/api-a/slots/before_create.proto": "syntax = \"proto3\";\n\npackage stego.components.api_a.slots;\n\n" +
			"message BeforeCreateRequest {\n  string entity = 1;\n  string actor = 2;\n}\n\nmessage BeforeCreateResponse {}\n\n" +
			"service BeforeCreate {\n  rpc Check(BeforeCreateRequest) returns (BeforeCreateResponse);\n}\n",
		"components/tenancy/component.yaml":   component("tenancy", "internal/tenancy", "[]", "[]", "[]"),
		"components/audit-log/component.yaml": component("audit-log", "internal/audit", "[]", "[]", "[]"),
		"components/extra-a/component.yaml":   component("extra-a", "internal/extra", "[]", "[]", "[]"),
		"archetypes/svc-arch/archetype.yaml": "kind: archetype\nruntime: {module: github.com/jsell-rh/stego/registry/runtime, version: v0.1.0, sum: 'h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='}\nname: svc-arch\nlanguage: go\nversion: 1.0.0\n" +
			"components:\n  - api-a\n  - store-a\n  - auth-a\n  - tenancy\n  - audit-log\n" +
			"conventions:\n  layout: flat\n  logging: structured-json\n  cors: disabled\n" +
			"compatible_mixins:\n  - extra-mixin\nbindings:\n  store-port: store-a\n  auth-port: auth-a\n",
		"mixins/extra-mixin/mixin.yaml": "kind: mixin\nname: extra-mixin\nversion: 1.0.0\n\nadds_components:\n  - extra-a\n\n" +
			"adds_slots:\n  - name: after_extra\n    proto: stego.mixins.extra_mixin.slots.AfterExtra\n\noverrides: none\n",
		"mixins/extra-mixin/slots/after_extra.proto": "syntax = \"proto3\";\n\npackage stego.mixins.extra_mixin.slots;\n\n" +
			"message AfterExtraRequest {}\n\nmessage AfterExtraResponse {}\n\n" +
			"service AfterExtra {\n  rpc Run(AfterExtraRequest) returns (AfterExtraResponse);\n}\n",
	}
}

// localRegistryFiles are the files of the local registry of ST-SUP-TD02.
func localRegistryFiles() map[string]string {
	return map[string]string{
		".gitignore": "*.log\n",
		"components/local-a/component.yaml": "kind: component\nname: local-a\nversion: 0.1.0\nstage: beta\nsunset: " + daysFromToday(60) + "\noutput_namespace: internal/local\n\n" +
			"config:\n  enabled:\n    type: bool\n\nrequires: []\n\nprovides: [local-port]\n\nslots: []\n",
	}
}

// daysFromToday gives the date a number of days after today, as a sunset
// date is written; a negative number gives a date before today.
func daysFromToday(days int) string {
	return time.Now().AddDate(0, 0, days).Format(time.DateOnly)
}

// registryRepository writes files into a new Git repository and commits them.
// It gives the repository and its commit.
func registryRepository(t *testing.T, name string, files map[string]string) (string, string) {
	t.Helper()
	repository := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(repository, 0755); err != nil {
		t.Fatal(err)
	}
	git(t, repository, "init", "-q")
	return repository, commitRegistry(t, repository, files)
}

// commitRegistry writes files into a repository, removes each file whose
// content is empty, commits, and gives the new commit.
func commitRegistry(t *testing.T, repository string, files map[string]string) string {
	t.Helper()
	for name, data := range files {
		path := filepath.Join(repository, filepath.FromSlash(name))
		if data == "" {
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repository, "add", "-A")
	git(t, repository, "commit", "-q", "--allow-empty", "-m", "registry")
	return git(t, repository, "rev-parse", "HEAD")
}

// registrySource is one entry of the registry list of .stego/config.yaml.
type registrySource struct{ url, ref, vendor string }

// pinned names a repository as a pinned Git source.
func pinned(repository, commit string) registrySource {
	return registrySource{url: "file://" + repository, ref: commit}
}

// registryProject writes a project folder whose .stego/config.yaml names the
// sources in order, with extra lines after the list.
func registryProject(t *testing.T, project string, extra string, sources ...registrySource) string {
	t.Helper()
	if project == "" {
		project = filepath.Join(t.TempDir(), "project")
	}
	var config strings.Builder
	config.WriteString("registry:\n")
	for _, source := range sources {
		fmt.Fprintf(&config, "  - url: %q\n    ref: %q\n", source.url, source.ref)
		if source.vendor != "" {
			fmt.Fprintf(&config, "    vendor: %q\n", source.vendor)
		}
	}
	config.WriteString(extra)
	if err := os.MkdirAll(filepath.Join(project, ".stego"), 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(project, ".stego"), "config.yaml", config.String(), 0644)
	return project
}

// supplyingFixture is ST-SUP-TD02: the common registry of ST-SUP-TD01 and a
// local registry, each in its own repository, pinned in that order.
type supplyingFixture struct {
	common, commonSHA string
	local, localSHA   string
	project           string
	cache             string
}

func newSupplyingFixture(t *testing.T) *supplyingFixture {
	t.Helper()
	f := &supplyingFixture{cache: filepath.Join(t.TempDir(), "cache")}
	f.common, f.commonSHA = registryRepository(t, "common-registry", commonRegistryFiles())
	f.local, f.localSHA = registryRepository(t, "local-registry", localRegistryFiles())
	f.project = registryProject(t, "", "", pinned(f.common, f.commonSHA), pinned(f.local, f.localSHA))
	return f
}

// composeRun is one run of the stego binary in a folder.
type composeRun struct {
	stdout, stderr string
	err            error
	trace          string
}

// runStegoIn runs the stego binary in a folder with the fixture's own cache,
// STEGO_REGISTRY unset unless env sets it, and Git tracing into a file.
func (f *supplyingFixture) runStegoIn(t *testing.T, folder string, env []string, args ...string) composeRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	trace := filepath.Join(t.TempDir(), "git-trace")
	command := exec.CommandContext(ctx, stegoBinary(t), args...)
	command.Dir = folder
	command.Env = append(os.Environ(), "XDG_CACHE_HOME="+f.cache, "STEGO_REGISTRY=", "GIT_TRACE="+trace,
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	command.Env = append(command.Env, env...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	data, _ := os.ReadFile(trace)
	return composeRun{stdout: stdout.String(), stderr: stderr.String(), err: err, trace: string(data)}
}

// compose runs stego registry compose in the fixture's project.
func (f *supplyingFixture) compose(t *testing.T, env ...string) composeRun {
	t.Helper()
	return f.runStegoIn(t, f.project, env, "registry", "compose")
}

// refused fails the test unless the run exited non-zero and its message names
// every one of the words.
func (r composeRun) refused(t *testing.T, words ...string) {
	t.Helper()
	if r.err == nil {
		t.Fatalf("the run was accepted, want a refusal\nstdout:\n%s", r.stdout)
	}
	for _, word := range words {
		if !strings.Contains(r.stderr, word) {
			t.Fatalf("the refusal does not name %q:\n%s", word, r.stderr)
		}
	}
}

// accepted fails the test unless the run exited 0, and gives its output.
func (r composeRun) accepted(t *testing.T) string {
	t.Helper()
	if r.err != nil {
		t.Fatalf("the run failed: %v\n%s", r.err, r.stderr)
	}
	return r.stdout
}

// fetched reports whether the Git trace shows a clone or a fetch.
func (r composeRun) fetched() bool {
	return strings.Contains(r.trace, "clone") || strings.Contains(r.trace, "fetch")
}

func TestRegistryComposeSources(t *testing.T) {
	t.Parallel()
	t.Run("ST-SUP-UC02-R01-T01 A local component with the name of a common component is refused, naming the part and both registries", func(t *testing.T) {
		for _, changed := range []bool{false, true} {
			f := newSupplyingFixture(t)
			data := commonRegistryFiles()["components/store-a/component.yaml"]
			if changed {
				data = strings.Replace(data, "1.0.0", "1.0.1", 1)
			}
			localSHA := commitRegistry(t, f.local, map[string]string{"components/store-a/component.yaml": data})
			registryProject(t, f.project, "", pinned(f.common, f.commonSHA), pinned(f.local, localSHA))
			f.compose(t).refused(t, "store-a", "file://"+f.common, f.commonSHA, "file://"+f.local, localSHA)
		}
	})

	t.Run("ST-SUP-UC02-R01-T02 A local archetype, mixin or port with the name of a common one is refused, naming the part and both registries", func(t *testing.T) {
		for _, part := range []struct{ kind, name, file string }{
			{"archetype", "svc-arch", "archetypes/svc-arch/archetype.yaml"},
			{"mixin", "extra-mixin", "mixins/extra-mixin/mixin.yaml"},
			{"port", "store-port", "ports/store-port/port.yaml"},
		} {
			f := newSupplyingFixture(t)
			files := map[string]string{part.file: commonRegistryFiles()[part.file]}
			if part.kind == "mixin" {
				files["mixins/extra-mixin/slots/after_extra.proto"] = commonRegistryFiles()["mixins/extra-mixin/slots/after_extra.proto"]
			}
			localSHA := commitRegistry(t, f.local, files)
			registryProject(t, f.project, "", pinned(f.common, f.commonSHA), pinned(f.local, localSHA))
			f.compose(t).refused(t, part.kind+` "`+part.name+`"`, "file://"+f.common, f.commonSHA, "file://"+f.local, localSHA)
		}
	})

	t.Run("ST-SUP-UC02-R02-T02 Compose prints each source with its reference and the parts it adds", func(t *testing.T) {
		f := newSupplyingFixture(t)
		output := f.compose(t).accepted(t)
		lines := strings.Split(output, "\n")
		common, local := -1, -1
		for i, line := range lines {
			if line == "common registry file://"+f.common+" at "+f.commonSHA {
				common = i
			}
			if line == "local registry file://"+f.local+" at "+f.localSHA {
				local = i
			}
		}
		if common < 0 || local <= common {
			t.Fatalf("compose does not name both sources with their commits in order:\n%s", output)
		}
		commonParts := strings.Join(lines[common+1:local], "\n")
		for _, part := range []string{"component store-a", "component api-a", "archetype svc-arch", "mixin extra-mixin", "port store-port"} {
			if !strings.Contains(commonParts, part) {
				t.Fatalf("the common registry does not list %s:\n%s", part, output)
			}
		}
		if localParts := strings.Join(lines[local+1:], "\n"); strings.TrimSpace(localParts) != "component local-a" {
			t.Fatalf("the local registry lists %q, want local-a only:\n%s", localParts, output)
		}
	})
}

func TestRegistryComposeLimits(t *testing.T) {
	t.Parallel()
	t.Run("ST-SUP-UC02-R03-T01 A composition of nine sources is refused, and one of eight is accepted", func(t *testing.T) {
		f := newSupplyingFixture(t)
		sources := []registrySource{pinned(f.common, f.commonSHA), pinned(f.local, f.localSHA)}
		for i := 3; i <= 9; i++ {
			name := fmt.Sprintf("local-%d", i)
			repository, commit := registryRepository(t, name+"-registry", map[string]string{
				"components/" + name + "/component.yaml": "kind: component\nname: " + name + "\nversion: 0.1.0\nstage: ga\noutput_namespace: internal/" + name + "\nrequires: []\nprovides: []\nslots: []\n",
			})
			sources = append(sources, pinned(repository, commit))
		}
		registryProject(t, f.project, "", sources[:8]...)
		f.compose(t).accepted(t)
		registryProject(t, f.project, "", sources...)
		f.compose(t).refused(t, "9 registry sources", "the limit is 8")
	})

	t.Run("ST-SUP-UC02-R03-T02 Two sources that supply one artifact name are refused, even as identical copies", func(t *testing.T) {
		for _, changed := range []bool{false, true} {
			f := newSupplyingFixture(t)
			files := localRegistryFiles()
			if changed {
				files["components/local-a/component.yaml"] = strings.Replace(files["components/local-a/component.yaml"], "0.1.0", "0.2.0", 1)
			}
			second, secondSHA := registryRepository(t, "second-local-registry", files)
			registryProject(t, f.project, "", pinned(f.common, f.commonSHA), pinned(f.local, f.localSHA), pinned(second, secondSHA))
			f.compose(t).refused(t, `component "local-a"`, "file://"+f.local, "file://"+second)
		}
	})

	t.Run("ST-SUP-UC02-R03-T03 Two sources that supply one input path are refused", func(t *testing.T) {
		f := newSupplyingFixture(t)
		localSHA := commitRegistry(t, f.local, map[string]string{"common/types.proto": commonRegistryFiles()["common/types.proto"]})
		registryProject(t, f.project, "", pinned(f.common, f.commonSHA), pinned(f.local, localSHA))
		f.compose(t).refused(t, "common/types.proto", "file://"+f.common, "file://"+f.local)
	})

	t.Run("ST-SUP-UC02-R03-T04 A symbolic link in a registry or a vendor path is refused", func(t *testing.T) {
		f := newSupplyingFixture(t)
		if err := os.Symlink("local-a", filepath.Join(f.local, "components", "link")); err != nil {
			t.Fatal(err)
		}
		linkSHA := commitRegistry(t, f.local, nil)
		registryProject(t, f.project, "", pinned(f.common, f.commonSHA), pinned(f.local, linkSHA))
		f.compose(t).refused(t, filepath.Join("components", "link"))

		g := newSupplyingFixture(t)
		vendor := filepath.Join(g.project, "vendor")
		git(t, g.project, "clone", "-q", g.local, filepath.Join(vendor, "local-copy"))
		if err := os.Symlink("local-copy", filepath.Join(vendor, "local")); err != nil {
			t.Fatal(err)
		}
		local := pinned(g.local, g.localSHA)
		local.vendor = "vendor/local"
		registryProject(t, g.project, "", pinned(g.common, g.commonSHA), local)
		g.compose(t).refused(t, filepath.Join(g.project, "vendor", "local")+":")
	})

	// plainSources points the project at the working folders of both
	// repositories, so files made at run time and not committed are read.
	plainSources := func(t *testing.T, f *supplyingFixture) {
		registryProject(t, f.project, "", registrySource{url: f.common, ref: f.commonSHA}, registrySource{url: f.local, ref: f.localSHA})
	}
	// inputs gives the count and the bytes of the registry inputs of a
	// folder: its YAML and protobuf files outside .git.
	inputs := func(t *testing.T, folder string) (int, int64) {
		count, size := 0, int64(0)
		err := filepath.WalkDir(folder, func(name string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() && entry.Name() == ".git" {
				return filepath.SkipDir
			}
			if ext := filepath.Ext(name); !entry.IsDir() && (ext == ".yaml" || ext == ".proto") {
				info, err := entry.Info()
				if err != nil {
					return err
				}
				count++
				size += info.Size()
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return count, size
	}

	t.Run("ST-SUP-UC02-R03-T05 Combined inputs over 64 MiB are refused, and inputs at the limit are accepted", func(t *testing.T) {
		f := newSupplyingFixture(t)
		plainSources(t, f)
		_, commonBytes := inputs(t, f.common)
		_, localBytes := inputs(t, f.local)
		const limit = int64(64 << 20)
		missing := limit + 1 - commonBytes - localBytes
		var last string
		for i, folder := 0, f.common; missing > 0; i++ {
			if i == 8 {
				folder = f.local
			}
			size := min(missing, int64(4<<20))
			last = filepath.Join(folder, "data", fmt.Sprintf("part-%02d.yaml", i))
			if err := os.MkdirAll(filepath.Dir(last), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(last, bytes.Repeat([]byte("#"), int(size)), 0644); err != nil {
				t.Fatal(err)
			}
			missing -= size
		}
		_, commonBytes = inputs(t, f.common)
		_, localBytes = inputs(t, f.local)
		if commonBytes >= limit || localBytes >= limit || commonBytes+localBytes != limit+1 {
			t.Fatalf("the sources hold %d and %d bytes, want each below the limit and %d together", commonBytes, localBytes, limit+1)
		}
		f.compose(t).refused(t, "64 MiB")
		info, err := os.Stat(last)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(last, info.Size()-1); err != nil {
			t.Fatal(err)
		}
		f.compose(t).accepted(t)
	})

	t.Run("ST-SUP-UC02-R03-T06 Combined inputs over 4,096 files are refused, and 4,096 files are accepted", func(t *testing.T) {
		f := newSupplyingFixture(t)
		plainSources(t, f)
		commonFiles, _ := inputs(t, f.common)
		localFiles, _ := inputs(t, f.local)
		var last string
		for i := 0; commonFiles+localFiles+i < 4097; i++ {
			folder := f.common
			if i%2 == 1 {
				folder = f.local
			}
			last = filepath.Join(folder, "data", fmt.Sprintf("part-%04d.yaml", i))
			if err := os.MkdirAll(filepath.Dir(last), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(last, []byte("# data\n"), 0644); err != nil {
				t.Fatal(err)
			}
		}
		commonFiles, _ = inputs(t, f.common)
		localFiles, _ = inputs(t, f.local)
		if commonFiles >= 4096 || localFiles >= 4096 || commonFiles+localFiles != 4097 {
			t.Fatalf("the sources hold %d and %d files, want each below the limit and 4097 together", commonFiles, localFiles)
		}
		f.compose(t).refused(t, "4,096 files")
		if err := os.Remove(last); err != nil {
			t.Fatal(err)
		}
		f.compose(t).accepted(t)
	})

	t.Run("ST-SUP-UC02-R03-T07 Every source is captured before any source is validated", func(t *testing.T) {
		f := newSupplyingFixture(t)
		commonSHA := commitRegistry(t, f.common, map[string]string{"components/store-a/component.yaml": "kind: component\nname: [store-a\n"})
		if err := os.Symlink("local-a", filepath.Join(f.local, "components", "link")); err != nil {
			t.Fatal(err)
		}
		linkSHA := commitRegistry(t, f.local, nil)
		registryProject(t, f.project, "", pinned(f.common, commonSHA), pinned(f.local, linkSHA))
		first := f.compose(t)
		first.refused(t, filepath.Join("components", "link"))
		if strings.Contains(first.stderr, "store-a") {
			t.Fatalf("the refusal names the invalid file of the first source before the link of the second:\n%s", first.stderr)
		}
		unlinkSHA := commitRegistry(t, f.local, map[string]string{"components/link": ""})
		registryProject(t, f.project, "", pinned(f.common, commonSHA), pinned(f.local, unlinkSHA))
		f.compose(t).refused(t, "components/store-a/component.yaml")
	})
}

func TestRegistryComposePins(t *testing.T) {
	t.Parallel()
	t.Run("ST-SUP-UC02-R04-T01 A source pinned by a short, an upper-case or a branch reference is refused", func(t *testing.T) {
		f := newSupplyingFixture(t)
		for _, ref := range []string{f.localSHA[:12], strings.ToUpper(f.localSHA), "main"} {
			registryProject(t, f.project, "", pinned(f.common, f.commonSHA), registrySource{url: "file://" + f.local, ref: ref})
			run := f.compose(t)
			run.refused(t, "file://"+f.local, ref, "full lowercase Git commit SHA")
			if run.fetched() {
				t.Fatalf("a fetch was made for the ref %s:\n%s", ref, run.trace)
			}
		}
	})

	t.Run("ST-SUP-UC02-R04-T02 A cached copy is used only at the pinned commit", func(t *testing.T) {
		f := newSupplyingFixture(t)
		other := commitRegistry(t, f.local, map[string]string{"components/other/component.yaml": "kind: component\nname: other\nversion: 0.1.0\nstage: ga\nrequires: []\nprovides: []\nslots: []\n"})
		first := f.compose(t)
		digest := strings.SplitN(first.accepted(t), "\n", 2)[0]
		var cached string
		_ = filepath.WalkDir(f.cache, func(name string, entry os.DirEntry, err error) error {
			if err == nil && entry.IsDir() && entry.Name() == f.localSHA {
				cached = name
				return filepath.SkipAll
			}
			return nil
		})
		if cached == "" {
			t.Fatal("the local registry was not cached")
		}
		git(t, cached, "checkout", "-q", "--detach", other)
		for _, repository := range []string{f.common, f.local} {
			if err := os.Rename(repository, repository+"-offline"); err != nil {
				t.Fatal(err)
			}
		}
		second := f.compose(t)
		if second.err == nil {
			if got := strings.SplitN(second.stdout, "\n", 2)[0]; got != digest {
				t.Fatalf("the second run read another tree: %q, want %q", got, digest)
			}
			return
		}
		second.refused(t, "file://"+f.local, f.localSHA)
	})

	// vendored clones the local registry into the project at a commit and
	// pins it there by its vendor path.
	vendored := func(t *testing.T, f *supplyingFixture, commit string) string {
		copy := filepath.Join(f.project, "vendor", "local")
		git(t, f.project, "clone", "-q", f.local, copy)
		git(t, copy, "checkout", "-q", "--detach", commit)
		local := pinned(f.local, f.localSHA)
		local.vendor = "vendor/local"
		registryProject(t, f.project, "", pinned(f.common, f.commonSHA), local)
		return copy
	}

	t.Run("ST-SUP-UC02-R04-T03 A vendored copy at another commit is refused without a fetch", func(t *testing.T) {
		f := newSupplyingFixture(t)
		f.compose(t).accepted(t)
		other := commitRegistry(t, f.local, map[string]string{"components/other/component.yaml": "kind: component\nname: other\nversion: 0.1.0\nstage: ga\nrequires: []\nprovides: []\nslots: []\n"})
		vendored(t, f, other)
		run := f.compose(t)
		run.refused(t, "file://"+f.local, f.localSHA, other)
		if run.fetched() {
			t.Fatalf("a fetch was made:\n%s", run.trace)
		}
	})

	t.Run("ST-SUP-UC02-R04-T04 A vendored copy without version-control metadata is refused", func(t *testing.T) {
		f := newSupplyingFixture(t)
		copy := vendored(t, f, f.localSHA)
		if err := os.RemoveAll(filepath.Join(copy, ".git")); err != nil {
			t.Fatal(err)
		}
		f.compose(t).refused(t, "vendor/local", "lacks version-control metadata")
	})

	t.Run("ST-SUP-UC02-R04-T05 A vendored copy with a modified, an ignored or an untracked file is refused", func(t *testing.T) {
		for _, change := range []struct{ file, data string }{
			{"components/local-a/component.yaml", "kind: component\nname: local-a\nversion: 9.9.9\nstage: ga\nrequires: []\nprovides: []\nslots: []\n"},
			{"build.log", "ignored\n"},
			{"components/local-a/notes.yaml", "untracked: true\n"},
		} {
			f := newSupplyingFixture(t)
			copy := vendored(t, f, f.localSHA)
			writeFile(t, copy, change.file, change.data, 0644)
			f.compose(t).refused(t, "vendor/local", change.file)
		}
	})

	t.Run("ST-SUP-UC02-R04-T06 With a vendor path set, STEGO reads only the vendored copy and does not fetch", func(t *testing.T) {
		f := newSupplyingFixture(t)
		f.compose(t).accepted(t)
		vendored(t, f, f.localSHA)
		local := registrySource{url: "https://unreachable.invalid/local-registry.git", ref: f.localSHA, vendor: "vendor/local"}
		registryProject(t, f.project, "", pinned(f.common, f.commonSHA), local)
		run := f.compose(t)
		if output := run.accepted(t); !strings.Contains(output, "component local-a") {
			t.Fatalf("the vendored local-a is not in the active set:\n%s", output)
		}
		if run.fetched() {
			t.Fatalf("a fetch or a clone was made:\n%s", run.trace)
		}
	})

	t.Run("ST-SUP-UC02-R05-T03 A nonempty per-component pin is refused before any source is resolved", func(t *testing.T) {
		f := newSupplyingFixture(t)
		local := registrySource{url: "https://unreachable.invalid/local-registry.git", ref: f.localSHA}
		registryProject(t, f.project, "pins:\n  store-a: 1.0.0\n", pinned(f.common, f.commonSHA), local)
		run := f.compose(t)
		run.refused(t, "pins", "store-a")
		if strings.TrimSpace(run.trace) != "" {
			t.Fatalf("a Git command ran:\n%s", run.trace)
		}
	})

	t.Run("ST-SUP-UC02-R05-T04 An environment override of the registry sources is warned of, and no warning is given without it", func(t *testing.T) {
		f := newSupplyingFixture(t)
		override := filepath.Join(t.TempDir(), "override-registry")
		git(t, filepath.Dir(override), "clone", "-q", f.common, override)
		run := f.compose(t, "STEGO_REGISTRY="+override)
		run.accepted(t)
		if !strings.Contains(run.stderr, override) || !strings.Contains(run.stderr, "configured registry sources are not used") {
			t.Fatalf("no warning names the override and the unused sources:\n%s", run.stderr)
		}
		without := f.compose(t)
		without.accepted(t)
		if strings.Contains(without.stderr, "STEGO_REGISTRY") || strings.Contains(without.stderr, "WARNING") {
			t.Fatalf("a warning was given without the override:\n%s", without.stderr)
		}
	})
}

func TestRegistryComposeStages(t *testing.T) {
	t.Run("ST-SUP-UC04-R01-T01 An alpha or beta component with no sunset date is refused", func(t *testing.T) {
		for _, stage := range []string{"beta", "alpha"} {
			f := newSupplyingFixture(t)
			commonSHA := commitRegistry(t, f.common, map[string]string{
				"components/exp-a/component.yaml": "kind: component\nname: exp-a\nversion: 0.1.0\nstage: " + stage + "\noutput_namespace: internal/exp\nrequires: []\nprovides: [exp-port]\nslots: []\n",
			})
			registryProject(t, f.project, "", pinned(f.common, commonSHA), pinned(f.local, f.localSHA))
			f.compose(t).refused(t, "exp-a", "sunset date is missing")
		}
	})

	t.Run("ST-SUP-UC05-R03-T01 A ga component that states a sunset date is refused", func(t *testing.T) {
		f := newSupplyingFixture(t)
		sunset := daysFromToday(30)
		data := strings.Replace(commonRegistryFiles()["components/store-a/component.yaml"], "stage: ga\n", "stage: ga\nsunset: "+sunset+"\n", 1)
		commonSHA := commitRegistry(t, f.common, map[string]string{"components/store-a/component.yaml": data})
		registryProject(t, f.project, "", pinned(f.common, commonSHA), pinned(f.local, f.localSHA))
		f.compose(t).refused(t, "store-a", "sunset date "+sunset)
	})
}
