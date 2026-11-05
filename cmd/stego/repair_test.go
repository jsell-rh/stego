package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jsell-rh/stego/internal/declaration"
	"gopkg.in/yaml.v3"
)

// The fixtures of Declaring's system tests (ST-DECL-TD01, ST-DECL-TD02). Each test runs the stego binary that
// stegoBinary builds once from the commit under test, in a new project folder, with no network.

var declaringRegistryFixture onceFixture
var declaringRegistryDir, declaringRegistryCommit string

// declaringRegistry is the registry of ST-DECL-TD01: a copy of registry/ at the commit under test, committed in its
// own Git checkout, so that a project configuration pins it at its full commit. Tests read it and never change it.
func declaringRegistry(t *testing.T) (string, string) {
	t.Helper()
	declaringRegistryFixture.get(t, func() (string, error) {
		source, err := filepath.Abs("../../registry")
		if err != nil {
			return "", err
		}
		declaringRegistryDir = filepath.Join(fixtureRoot, "declaring-registry")
		if output, err := exec.Command("cp", "-R", source, declaringRegistryDir).CombinedOutput(); err != nil {
			return "", fmt.Errorf("copy the registry: %v\n%s", err, output)
		}
		git(t, declaringRegistryDir, "init", "-q")
		git(t, declaringRegistryDir, "add", "-A")
		git(t, declaringRegistryDir, "commit", "-q", "-m", "fixture registry")
		declaringRegistryCommit = git(t, declaringRegistryDir, "rev-parse", "HEAD")
		return "", nil
	})
	return declaringRegistryDir, declaringRegistryCommit
}

// putFile writes a file of a test project, with the folders it is in.
func putFile(t *testing.T, folder, name, data string) {
	t.Helper()
	path := filepath.Join(folder, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
}

// declaringConfig is the project configuration of ST-DECL-TD01: the fixture registry as a local source pinned at
// its full commit.
func declaringConfig(t *testing.T) string {
	t.Helper()
	registry, commit := declaringRegistry(t)
	return fmt.Sprintf("registry:\n  - url: %s\n    ref: %s\n", registry, commit)
}

// declaringProject makes an empty project folder with the configuration of ST-DECL-TD01.
func declaringProject(t *testing.T) string {
	t.Helper()
	project := t.TempDir()
	putFile(t, project, ".stego/config.yaml", declaringConfig(t))
	return project
}

// td02 is the accepted rest-crud declaration of ST-DECL-TD02. The entity id is implicit in the declaration
// format, which has no uuid field type, so the declaration does not list it.
const td02 = `kind: service
name: todo-svc
archetype: rest-crud
language: go
base_path: /api/todo/v1
entities:
  - name: Todo
    fields:
      - {name: title, type: string, min_length: 1, max_length: 255}
      - {name: done, type: bool, default: false}
collections:
  todos:
    entity: Todo
    operations: [create, read, update, delete, list]
`

// threeFaults is ST-DECL-TD02 changed in three places: a second entity named Todo, a binding on todos at
// before_create with the operator merge, and the type strng on the field done.
const threeFaults = `kind: service
name: todo-svc
archetype: rest-crud
language: go
base_path: /api/todo/v1
entities:
  - name: Todo
    fields:
      - {name: title, type: string, min_length: 1, max_length: 255}
      - {name: done, type: strng, default: false}
  - name: Todo
    fields:
      - {name: note, type: string}
collections:
  todos:
    entity: Todo
    operations: [create, read, update, delete, list]
slots:
  - collection: todos
    slot: before_create
    merge: [policy]
`

// td02Project makes the project of ST-DECL-TD02 with the declaration given, and an empty fills folder.
func td02Project(t *testing.T, service string) string {
	t.Helper()
	project := declaringProject(t)
	putFile(t, project, "service.yaml", service)
	if err := os.MkdirAll(filepath.Join(project, "fills"), 0755); err != nil {
		t.Fatal(err)
	}
	return project
}

// stegoRun is one run of the stego binary: what it printed and its exit code.
type stegoRun struct {
	stdout, stderr string
	code           int
}

// stegoIn runs the stego binary in the folder, with a home folder of the test and with no registry override, so
// that the project configuration names the registry. Each extra environment entry is added.
func stegoIn(t *testing.T, folder string, environment []string, args ...string) stegoRun {
	t.Helper()
	binary := stegoBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = folder
	home := filepath.Join(fixtureRoot, "home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "STEGO_REGISTRY", "HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME":
			continue
		}
		command.Env = append(command.Env, entry)
	}
	command.Env = append(command.Env, "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"), "XDG_CACHE_HOME="+filepath.Join(home, ".cache"))
	command.Env = append(command.Env, environment...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	run := stegoRun{stdout: stdout.String(), stderr: stderr.String()}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		run.code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("stego %s: %v", strings.Join(args, " "), err)
	}
	return run
}

// validateJSON runs stego validate with its structured output and reads the one JSON document it prints.
func validateJSON(t *testing.T, project string) (declaration.FaultsDocument, stegoRun) {
	t.Helper()
	run := stegoIn(t, project, nil, "validate", "--json")
	var document declaration.FaultsDocument
	decoder := json.NewDecoder(strings.NewReader(run.stdout))
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("stego validate --json printed no JSON document: %v\nstdout:\n%s\nstderr:\n%s", err, run.stdout, run.stderr)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err == nil {
		t.Fatalf("stego validate --json printed more than one document:\n%s", run.stdout)
	}
	return document, run
}

// proseFaults gives the fault lines of the prose that stego validate prints for a person.
func proseFaults(output string) []string {
	var faults []string
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "  [") {
			faults = append(faults, strings.TrimSpace(line))
		}
	}
	return faults
}

// pathSegment is one step of a fault path: a key, with an index when the key holds a list.
var pathSegment = regexp.MustCompile(`^([^\[\]]+)(?:\[(\d+)\])?$`)

// resolvePath follows a fault path, such as entities[1].name, in the declaration, and gives the key node and the
// value node it leads to.
func resolvePath(t *testing.T, service []byte, path string) (*yaml.Node, *yaml.Node) {
	t.Helper()
	var document yaml.Node
	if err := yaml.Unmarshal(service, &document); err != nil {
		t.Fatal(err)
	}
	return resolvePathNode(t, &document, path)
}

// faultKey is what an agent compares between two checks: every structured field of a fault.
func faultKey(fault declaration.ValidationError) string {
	return fmt.Sprintf("%s|%s|%s|%s|%s", fault.Rule, fault.Path, fault.Found, strings.Join(fault.Allowed, ","), fault.Message)
}

func faultKeys(document declaration.FaultsDocument) []string {
	keys := make([]string, 0, len(document.Faults))
	for _, fault := range document.Faults {
		keys = append(keys, faultKey(fault))
	}
	return keys
}

func TestRepairARefusedDeclaration(t *testing.T) {
	t.Run("ST-DECL-UC04-R01-T01 Each fault is in the structured output, and no fault is only in prose", func(t *testing.T) {
		project := td02Project(t, threeFaults)
		document, run := validateJSON(t, project)
		if run.code == 0 || document.Accepted || len(document.Faults) != 3 {
			t.Fatalf("want a refusal with three faults, got exit %d:\n%s", run.code, run.stdout)
		}
		for _, fault := range document.Faults {
			if fault.Rule == "" || fault.Path == "" || fault.Found == "" || fault.Message == "" {
				t.Errorf("fault lacks its rule, path, value found or message: %+v", fault)
			}
		}
		allowed := 0
		for _, fault := range document.Faults {
			if len(fault.Allowed) > 0 {
				allowed++
			}
		}
		if allowed == 0 {
			t.Errorf("no fault names what is allowed: %+v", document.Faults)
		}
		prose := stegoIn(t, project, nil, "validate")
		lines := proseFaults(prose.stdout)
		if prose.code == 0 || len(lines) != len(document.Faults) {
			t.Fatalf("the prose names %d faults, the structured document %d:\n%s", len(lines), len(document.Faults), prose.stdout)
		}
		for _, line := range lines {
			held := false
			for _, fault := range document.Faults {
				held = held || strings.HasPrefix(line, "["+fault.Rule+"] "+fault.Path+": ")
			}
			if !held {
				t.Errorf("the prose names a fault that the structured document does not hold: %s", line)
			}
		}
	})

	t.Run("ST-DECL-UC04-R02-T01 Each fault names the rule it breaks and the path where it is", func(t *testing.T) {
		project := td02Project(t, threeFaults)
		document, _ := validateJSON(t, project)
		if len(document.Faults) != 3 {
			t.Fatalf("want three faults, got %+v", document.Faults)
		}
		rules := map[string]bool{}
		leads := map[string]bool{}
		for _, fault := range document.Faults {
			rules[fault.Rule] = true
			key, value := resolvePath(t, []byte(threeFaults), fault.Path)
			switch {
			case key != nil && key.Value == "merge":
				leads["operator"] = true
			case key != nil && key.Value == "name" && value.Value == "Todo" && strings.HasPrefix(fault.Path, "entities[1]"):
				leads["second entity"] = true
			case key != nil && key.Value == "type" && value.Value == "strng":
				leads["field type"] = true
			default:
				t.Errorf("the path %q of fault %+v leads to no value that breaks the rule", fault.Path, fault)
			}
		}
		if len(leads) != 3 {
			t.Errorf("the paths lead to %v, want the second entity, the operator and the field type", leads)
		}
		if len(rules) != 3 {
			t.Errorf("the faults name the rules %v, want a different rule for each", rules)
		}
	})

	t.Run("ST-DECL-UC04-R04-T01 The prose names what the structured faults name, in the same order", func(t *testing.T) {
		project := td02Project(t, threeFaults)
		document, _ := validateJSON(t, project)
		prose := stegoIn(t, project, nil, "validate")
		lines := proseFaults(prose.stdout)
		if len(lines) != len(document.Faults) {
			t.Fatalf("the prose names %d faults, the structured document %d:\n%s", len(lines), len(document.Faults), prose.stdout)
		}
		for i, fault := range document.Faults {
			line := lines[i]
			for _, part := range append([]string{"[" + fault.Rule + "]", fault.Path, fault.Found}, fault.Allowed...) {
				if !strings.Contains(line, part) {
					t.Errorf("prose fault %d does not name %q of its structured fault: %s", i, part, line)
				}
			}
		}
	})

	t.Run("ST-DECL-UC04-R05-T01 Two checks of one declaration against one registry give the same faults in the same order", func(t *testing.T) {
		project := td02Project(t, threeFaults)
		for _, name := range []string{"alpha", "beta", "gamma"} {
			putFile(t, project, filepath.Join("fills", name, "notes.txt"), name)
		}
		first, _ := validateJSON(t, project)
		// Make the fills folder again with its files in the other order, and move every modification time.
		if err := os.RemoveAll(filepath.Join(project, "fills")); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"gamma", "beta", "alpha"} {
			putFile(t, project, filepath.Join("fills", name, "notes.txt"), name)
		}
		later := time.Now().Add(-48 * time.Hour)
		if err := filepath.WalkDir(project, func(name string, _ os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return os.Chtimes(name, later, later)
		}); err != nil {
			t.Fatal(err)
		}
		second, _ := validateJSON(t, project)
		if len(first.Faults) != 3 || strings.Join(faultKeys(first), "\n") != strings.Join(faultKeys(second), "\n") {
			t.Fatalf("the two checks differ:\n%s\n---\n%s", strings.Join(faultKeys(first), "\n"), strings.Join(faultKeys(second), "\n"))
		}
	})

	t.Run("ST-DECL-UC04-R05-T02 Repairing one fault leaves the other faults as they were, in the same order", func(t *testing.T) {
		project := td02Project(t, threeFaults)
		if run := stegoIn(t, project, nil, "fill", "create", "policy", "--slot", "before_create", "--collection", "todos"); run.code != 0 {
			t.Fatalf("stego fill create: %s", run.stderr)
		}
		first, _ := validateJSON(t, project)
		putFile(t, project, "service.yaml", strings.Replace(threeFaults, "merge: [policy]", "gate: [policy]", 1))
		second, _ := validateJSON(t, project)
		var kept []string
		for _, fault := range first.Faults {
			if fault.Rule != "slot-operator" {
				kept = append(kept, faultKey(fault))
			}
		}
		if len(first.Faults) != 3 || len(kept) != 2 || strings.Join(kept, "\n") != strings.Join(faultKeys(second), "\n") {
			t.Fatalf("after the repair, want the other two faults as they were:\nfirst:\n%s\nsecond:\n%s", strings.Join(faultKeys(first), "\n"), strings.Join(faultKeys(second), "\n"))
		}
	})

	t.Run("ST-DECL-UC04-R06-T01 An unreadable declaration gives one fault with its line and position, and no semantic fault", func(t *testing.T) {
		lines := strings.Split(threeFaults, "\n")
		if lines[6] != "  - name: Todo" {
			t.Fatalf("line 7 is %q", lines[6])
		}
		lines[6] = "\t- name: Todo"
		project := td02Project(t, strings.Join(lines, "\n"))
		document, run := validateJSON(t, project)
		if run.code == 0 || len(document.Faults) != 1 {
			t.Fatalf("want one fault, got exit %d:\n%s", run.code, run.stdout)
		}
		fault := document.Faults[0]
		if fault.Line != 7 || fault.Position != 1 {
			t.Errorf("the fault names line %d position %d, want line 7 position 1: %+v", fault.Line, fault.Position, fault)
		}
		if fault.Rule == "unique-entity-name" || strings.Contains(fault.Message, "more than once") {
			t.Errorf("the fault is a semantic fault: %+v", fault)
		}
	})

	t.Run("ST-DECL-UC04-FL-T An agent repairs a refused declaration from the structured faults alone", func(t *testing.T) {
		project := td02Project(t, threeFaults)
		if run := stegoIn(t, project, nil, "fill", "create", "policy", "--slot", "before_create", "--collection", "todos"); run.code != 0 {
			t.Fatalf("stego fill create: %s", run.stderr)
		}
		previous := map[string]bool{}
		for attempt := 1; attempt <= 3; attempt++ {
			document, run := validateJSON(t, project)
			if document.Accepted {
				if run.code != 0 || attempt == 1 {
					t.Fatalf("accepted at run %d with exit %d", attempt, run.code)
				}
				return
			}
			if attempt == 3 {
				t.Fatalf("not accepted by the third run:\n%s", run.stdout)
			}
			for _, fault := range document.Faults {
				if previous[faultKey(fault)] {
					t.Logf("fault remains after its repair: %+v", fault)
				}
				previous[faultKey(fault)] = true
			}
			service, err := os.ReadFile(filepath.Join(project, "service.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			putFile(t, project, "service.yaml", string(repairFaults(t, service, document.Faults)))
		}
	})
}

// repairFaults is the scripted agent of ST-DECL-UC04-FL-T. It reads nothing but the structured faults: at the
// path of each fault it puts the first value the fault allows, or, when the fault allows no stated set, a new
// name made from the value found.
func repairFaults(t *testing.T, service []byte, faults []declaration.ValidationError) []byte {
	t.Helper()
	var document yaml.Node
	if err := yaml.Unmarshal(service, &document); err != nil {
		t.Fatal(err)
	}
	for _, fault := range faults {
		key, value := resolvePathNode(t, &document, fault.Path)
		replacement := fault.Found + "2"
		if len(fault.Allowed) > 0 {
			replacement = fault.Allowed[0]
		}
		if key != nil && key.Value == fault.Found {
			key.Value = replacement
		} else {
			value.Value = replacement
		}
	}
	repaired, err := yaml.Marshal(&document)
	if err != nil {
		t.Fatal(err)
	}
	return repaired
}

// resolvePathNode follows a fault path in a parsed declaration, as resolvePath does, so that the node can be
// changed.
func resolvePathNode(t *testing.T, document *yaml.Node, path string) (*yaml.Node, *yaml.Node) {
	t.Helper()
	node := document.Content[0]
	var key *yaml.Node
	for _, segment := range strings.Split(path, ".") {
		match := pathSegment.FindStringSubmatch(segment)
		if match == nil || node.Kind != yaml.MappingNode {
			t.Fatalf("path %q does not lead into the declaration at %q", path, segment)
		}
		found := false
		for i := 0; i < len(node.Content); i += 2 {
			if node.Content[i].Value == match[1] {
				key, node, found = node.Content[i], node.Content[i+1], true
				break
			}
		}
		if !found {
			t.Fatalf("path %q names %q, which the declaration does not hold", path, match[1])
		}
		if match[2] != "" {
			index, _ := strconv.Atoi(match[2])
			if node.Kind != yaml.SequenceNode || index >= len(node.Content) {
				t.Fatalf("path %q names item %d of %q, which the declaration does not hold", path, index, match[1])
			}
			key, node = nil, node.Content[index]
		}
	}
	return key, node
}
