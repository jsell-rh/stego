package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/ports"
	"github.com/jsell-rh/stego/internal/registry"
	"gopkg.in/yaml.v3"
)

// smallService is the test data set ST-GEN-TD03: a small applied service and
// a registry in Git with two commits. R1 is the registry of the commit under
// test; R2 changes one line of component A (jwt-auth, the component that
// authenticates each request): the default of the header it reads the token
// from, a fix that changes one generated file in A's namespace. The service folder, made by stego init and stego apply with
// its registry pinned at R1, is its own Git repository with one commit. The
// parent folder holds the service folder, the registry repository and a
// sentinel file, so a test can show that a run writes nothing outside the
// service. Each test makes its own copy in a temporary folder; the stego
// binary is built once per run, and no command reaches the network.
type smallService struct {
	parent, service, registry string
	r1, r2                    string
	env                       []string
}

// smallServiceDeclaration is the declaration of ST-GEN-TD03: the rest-crud
// archetype on the runtime data path, the only data path (ST-SUP-RC20-OC39),
// with one entity Note that has a title of type string, and no fill.
const smallServiceDeclaration = `kind: service
name: notes
archetype: rest-crud
language: go
tenancy: {kind: pooled, key: tenant_id, claim: tenant, provisioning: {resource_type: service, provision: provision, retire: retire}}
authorization: {provider: spicedb, schema_path: authz/schema.zed, subject_from_identity: user}
audit: {level: change}
overrides:
  postgres-adapter: {schema_generation: tables-v1}
entities:
  - name: Note
    resource_type: note
    permissions: {create: public, read: view, list: view}
    public_reasons: {create: a new note has no resource to decide on before it exists}
    fields: [{name: title, type: string}]
collections:
  notes: {entity: Note, operations: [create, read, list]}
`

// secondEntityDeclaration adds a second entity, Tag, with its collection.
var secondEntityDeclaration = strings.Replace(smallServiceDeclaration, "collections:\n", "  - name: Tag\n    resource_type: tag\n    permissions: {create: public, read: view, list: view}\n    public_reasons: {create: a new tag has no resource to decide on before it exists}\n    fields: [{name: label, type: string}]\ncollections:\n  tags: {entity: Tag, operations: [create, read]}\n", 1)

// componentA is the component that R2 changes. On the runtime data path the
// runtime verifier takes the place of jwt-auth and reads its settings, so the
// fix of its default header changes one generated file, componentAFile, the
// runtime wiring.
const componentA = "jwt-auth"
const componentAFile = "internal/stegoruntime/runtime.go"

func newSmallService(t *testing.T) *smallService {
	t.Helper()
	stegoBinary(t)
	parent := t.TempDir()
	s := &smallService{parent: parent, service: filepath.Join(parent, "notes"), registry: filepath.Join(parent, "registry")}
	home := filepath.Join(parent, ".home")
	for _, folder := range []string{home, s.service} {
		if err := os.MkdirAll(folder, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s.env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + home, "XDG_CACHE_HOME=" + filepath.Join(home, "cache"),
		"TMPDIR=" + os.TempDir(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GOTOOLCHAIN=local", "GOPROXY=off", "GOWORK=off",
	}
	if output, err := exec.Command("cp", "-a", shippedRegistry(t), s.registry).CombinedOutput(); err != nil {
		t.Fatalf("copy the registry: %v\n%s", err, output)
	}
	git(t, s.registry, "init", "-q")
	git(t, s.registry, "add", "-A")
	git(t, s.registry, "commit", "-q", "-m", "R1")
	s.r1 = git(t, s.registry, "rev-parse", "HEAD")
	definition := filepath.Join(s.registry, "components", componentA, "component.yaml")
	data, err := os.ReadFile(definition)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "    default: Authorization\n") {
		t.Fatalf("component A states no default header to fix:\n%s", data)
	}
	writeFile(t, filepath.Dir(definition), "component.yaml", strings.Replace(string(data), "    default: Authorization\n", "    default: X-Notes-Token\n", 1), 0o644)
	git(t, s.registry, "commit", "-q", "-am", "R2")
	s.r2 = git(t, s.registry, "rev-parse", "HEAD")
	git(t, s.registry, "checkout", "-q", "--detach", s.r1)

	writeFile(t, parent, "sentinel.txt", "outside the service\n", 0o644)
	s.pin(t, s.r1)
	s.must(t, "init", "--archetype", "rest-crud")
	s.write(t, "service.yaml", smallServiceDeclaration)
	s.must(t, "apply")
	git(t, s.service, "init", "-q")
	git(t, s.service, "add", "-A")
	git(t, s.service, "commit", "-q", "-m", "notes")
	return s
}

// pin writes the service's registry configuration with the registry
// repository at the commit.
func (s *smallService) pin(t *testing.T, commit string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(s.service, ".stego"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.write(t, ".stego/config.yaml", "registry:\n  - url: file://"+s.registry+"\n    ref: "+commit+"\n")
}

// write writes a file of the service with mode 0644, whatever mode a Git
// checkout gave it.
func (s *smallService) write(t *testing.T, name, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(s.service, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	path := writeFile(t, filepath.Dir(filepath.Join(s.service, name)), filepath.Base(name), data, 0o644)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	withRuntimeSchema(t, s.service, name, data)
}

func (s *smallService) read(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.service, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// stego runs the stego binary in the service folder with the fixture's
// environment.
func (s *smallService) stego(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, stegoBinary(t), args...)
	command.Dir, command.Env = s.service, s.env
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}

func (s *smallService) must(t *testing.T, args ...string) string {
	t.Helper()
	stdout, stderr, err := s.stego(t, args...)
	if err != nil {
		t.Fatalf("stego %s: %v\n%s%s", strings.Join(args, " "), err, stdout, stderr)
	}
	return stdout
}

// record reads .stego/state.yaml with a YAML parser and no STEGO code.
func (s *smallService) record(t *testing.T) map[string]any {
	t.Helper()
	var record map[string]any
	if err := yaml.Unmarshal([]byte(s.read(t, ".stego/state.yaml")), &record); err != nil {
		t.Fatal(err)
	}
	return record
}

// tree gives the SHA-256 of every file under the folder, by its slash path,
// without the Git metadata of the folder itself.
func tree(t *testing.T, folder string) map[string]string {
	t.Helper()
	files := map[string]string{}
	if err := filepath.WalkDir(folder, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(folder, name)
		if entry.IsDir() {
			if relative == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		data, err := os.ReadFile(name)
		sum := sha256.Sum256(data)
		files[filepath.ToSlash(relative)] = hex.EncodeToString(sum[:])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return files
}

// registryAt checks the registry repository out at the commit into a new
// folder, and gives the folder.
func (s *smallService) registryAt(t *testing.T, commit string) string {
	t.Helper()
	folder := filepath.Join(t.TempDir(), "registry")
	git(t, s.registry, "worktree", "add", "-q", "--detach", folder, commit)
	return folder
}

func TestServiceRecordNamesItsInputs(t *testing.T) {
	t.Parallel()
	t.Run("ST-GEN-UC01-R01-T01 The record names the service, the compiler, the registry and each component used with its version and digest", func(t *testing.T) {
		s := newSmallService(t)
		applied, _ := s.record(t)["last_applied"].(map[string]any)
		if applied == nil {
			t.Fatal("the record has no last_applied")
		}
		if applied["service_name"] != "notes" {
			t.Errorf("the record names the service %v, not notes as service.yaml does", applied["service_name"])
		}

		var identity struct {
			Build map[string]any `json:"build"`
		}
		if err := json.Unmarshal([]byte(s.must(t, "version", "--json")), &identity); err != nil {
			t.Fatal(err)
		}
		build, _ := applied["compiler_build"].(map[string]any)
		if len(identity.Build) == 0 || len(build) != len(identity.Build) {
			t.Errorf("the record's compiler build %v is not the build identity %v that stego version reports", build, identity.Build)
		}
		for field, value := range identity.Build {
			if build[field] != value {
				t.Errorf("the record's compiler build has %s %v, and stego version reports %v", field, build[field], value)
			}
		}

		checkout := s.registryAt(t, s.r1)
		reg, err := registry.Load(checkout)
		if err != nil {
			t.Fatal(err)
		}
		if applied["registry_content_sha256"] != reg.ContentHash() {
			t.Errorf("the record names the registry content %v, not %s of the registry at R1", applied["registry_content_sha256"], reg.ContentHash())
		}

		archetype := reg.Archetype("rest-crud")
		baseline := map[string]*registry.Component{}
		// The pooled tenancy, the SpiceDB authorization and the audit of the declaration add their components.
		for _, name := range append(slices.Clone(archetype.Components), archetype.DefaultAuth, "tenancy", "spicedb-authz", "audit-log") {
			baseline[name] = reg.Component(name)
		}
		resolution, err := ports.Resolve(ports.ResolveInput{Components: baseline, ArchetypeBindings: archetype.Bindings, ComponentLoader: reg.Component})
		if err != nil {
			t.Fatal(err)
		}
		files, err := registry.CaptureInputFiles(checkout)
		if err != nil {
			t.Fatal(err)
		}
		pins, _ := applied["components"].(map[string]any)
		for name, component := range resolution.ActiveComponents {
			digest, err := compiler.ComponentDigest(files, name)
			if err != nil {
				t.Fatal(err)
			}
			pin, _ := pins[name].(map[string]any)
			if pin == nil {
				t.Errorf("the record does not list component %s of the active set", name)
				continue
			}
			if pin["version"] != component.Version || pin["sha"] != digest || len(digest) != 64 {
				t.Errorf("the record pins %s at version %v and digest %v, not %s and %s", name, pin["version"], pin["sha"], component.Version, digest)
			}
		}
		for name := range pins {
			if resolution.ActiveComponents[name] == nil {
				t.Errorf("the record lists component %s, which is not in the active set", name)
			}
		}
		if len(resolution.ActiveComponents) < 5 {
			t.Fatalf("the active set of rest-crud holds only %d components", len(resolution.ActiveComponents))
		}
	})
}

func TestServiceRecordIsCommittedWithItsCode(t *testing.T) {
	t.Parallel()
	t.Run("ST-GEN-UC01-R05-T01 The record is written inside the service's repository, and one commit holds it with the code it describes", func(t *testing.T) {
		s := newSmallService(t)
		parentBefore, registryBefore := tree(t, s.parent), tree(t, s.registry)
		recordBefore := s.read(t, ".stego/state.yaml")
		s.write(t, "service.yaml", secondEntityDeclaration)
		s.must(t, "apply")
		if s.read(t, ".stego/state.yaml") == recordBefore {
			t.Fatal("apply of a second entity left the record as it was")
		}
		parentAfter := tree(t, s.parent)
		for name, hash := range parentBefore {
			if inside := strings.HasPrefix(name, "notes/"); !inside && parentAfter[name] != hash {
				t.Errorf("apply changed %s outside the service folder", name)
			}
		}
		for name := range parentAfter {
			if _, existed := parentBefore[name]; !existed && !strings.HasPrefix(name, "notes/") {
				t.Errorf("apply wrote %s outside the service folder", name)
			}
		}
		if !maps.Equal(registryBefore, tree(t, s.registry)) {
			t.Error("apply changed the registry copy")
		}

		git(t, s.service, "add", "-A")
		git(t, s.service, "commit", "-q", "-m", "a second entity")
		if status := git(t, s.service, "status", "--porcelain", "--untracked-files=all"); status != "" {
			t.Fatalf("one commit leaves the service folder unclean:\n%s", status)
		}
		committed := map[string]bool{}
		for _, name := range strings.Split(git(t, s.service, "ls-tree", "-r", "--name-only", "HEAD"), "\n") {
			committed[name] = true
		}
		if !committed[".stego/state.yaml"] {
			t.Fatal("the commit does not hold .stego/state.yaml")
		}
		applied, _ := s.record(t)["last_applied"].(map[string]any)
		files, _ := applied["files"].(map[string]any)
		if len(files) == 0 {
			t.Fatal("the record lists no generated file")
		}
		for name, hash := range files {
			path := "out/" + name
			if name == "go.mod" || name == "go.sum" {
				path = name
			}
			if !committed[path] {
				t.Errorf("the commit does not hold %s, which the record lists", path)
				continue
			}
			show := exec.Command("git", "show", "HEAD:"+path)
			show.Dir, show.Env = s.service, s.env
			data, err := show.Output()
			if err != nil {
				t.Fatal(err)
			}
			if sum := sha256.Sum256(data); hash != hex.EncodeToString(sum[:]) {
				t.Errorf("the commit holds %s at another content than the record's hash %v", path, hash)
			}
		}
	})
}
