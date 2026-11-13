package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// These tests run the stego commands in process against the built-in
// registry, as a service author does, and read only what the commands leave
// on disk and print.

const generatingDeclaration = `kind: service
name: generating
archetype: rest-crud
language: go
tenancy: {kind: pooled, key: tenant_id, claim: tenant}
authorization: {provider: spicedb, schema_path: authz/schema.zed, subject_from_identity: user}
audit: {level: change}
overrides:
  postgres-adapter: {schema_generation: tables-v1}
entities:
  - name: Todo
    resource_type: todo
    permissions: {create: public, read: view, list: view}
    public_reasons: {create: a new todo has no resource to decide on before it exists}
    fields: [{name: title, type: string}]
  - name: Note
    resource_type: note
    permissions: {create: public, read: view, list: view}
    public_reasons: {create: a new note has no resource to decide on before it exists}
    fields: [{name: body, type: string}]
collections:
  todos: {entity: Todo, operations: [create, read, list]}
  notes: {entity: Note, operations: [create, read]}
`


// shippedRegistry gives the folder of the shipped registry, whose archetypes are on the runtime data path, the only
// data path (ST-SUP-RC20-OC39).
func shippedRegistry(t testing.TB) string {
	t.Helper()
	_, source, _, _ := runtime.Caller(0)
	dir, err := filepath.Abs(filepath.Join(filepath.Dir(source), "../../registry"))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// shippedRegistryCopy gives a copy of the shipped registry that a test may change.
func shippedRegistryCopy(t testing.TB) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "registry")
	if err := shippedRegistryInto(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

// shippedRegistryInto copies the shipped registry to dir, for a fixture that changes it or outlives one test.
func shippedRegistryInto(dir string) error {
	_, source, _, _ := runtime.Caller(0)
	return os.CopyFS(dir, os.DirFS(filepath.Join(filepath.Dir(source), "../../registry")))
}

// generatingProject makes an empty project with the declaration in a new
// working directory.
func generatingProject(t *testing.T, declaration string) {
	t.Helper()
	t.Chdir(t.TempDir())
	t.Setenv("STEGO_REGISTRY", shippedRegistry(t))
	t.Setenv("STEGO_MODULE", "example.com/generating")
	t.Setenv("STEGO_GO_VERSION", "1.26.11")
	writeProjectFile(t, "service.yaml", declaration)
}

func writeProjectFile(t testing.TB, name, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	withRuntimeSchema(t, ".", name, data)
}

// projectTree returns every file in the working directory with its content.
func projectTree(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	if err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		files[filepath.ToSlash(path)] = string(data)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return files
}

// capture runs a command and returns what it printed.
func capture(t *testing.T, command func([]string) error, args ...string) (string, error) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = writer
	printed := make(chan []byte)
	go func() {
		data, _ := io.ReadAll(reader)
		printed <- data
	}()
	runErr := command(args)
	os.Stdout = stdout
	writer.Close()
	return string(<-printed), runErr
}

func mustRun(t *testing.T, command func([]string) error, args ...string) string {
	t.Helper()
	printed, err := capture(t, command, args...)
	if err != nil {
		t.Fatalf("%v\n%s", err, printed)
	}
	return printed
}

// plainRecord reads the record with a YAML parser only, as a reader without
// STEGO does.
func plainRecord(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(".stego/state.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := yaml.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func recordedFiles(t *testing.T) map[string]any {
	t.Helper()
	applied, _ := plainRecord(t)["last_applied"].(map[string]any)
	files, _ := applied["files"].(map[string]any)
	if len(files) == 0 {
		t.Fatal("the record lists no generated files")
	}
	return files
}

func TestRecordReadsWithoutStego(t *testing.T) {
	t.Run("ST-GEN-UC01-R03-T01 A YAML parser alone reads every file and hash from the record", func(t *testing.T) {
		generatingProject(t, generatingDeclaration)
		mustRun(t, runApply)
		for path, hash := range recordedFiles(t) {
			data, err := os.ReadFile(filepath.Join("out", path))
			if err != nil {
				t.Fatalf("the record names %s, which apply did not write: %v", path, err)
			}
			sum := sha256.Sum256(data)
			if hash != hex.EncodeToString(sum[:]) {
				t.Fatalf("the plain record hash of %s is %v, not the SHA-256 of its content", path, hash)
			}
		}
	})
}

func TestNoOutputWithoutRecord(t *testing.T) {
	t.Run("ST-GEN-UC01-R02-T01 Every file apply leaves in the output folder is in the record", func(t *testing.T) {
		generatingProject(t, generatingDeclaration)
		mustRun(t, runApply)
		files := recordedFiles(t)
		for path := range projectTree(t) {
			name, ok := strings.CutPrefix(path, "out/")
			if ok && files[name] == nil {
				t.Fatalf("%s is generated output with no entry in the record", path)
			}
		}
	})
}

func TestRecordFormatIsVersioned(t *testing.T) {
	t.Run("ST-GEN-UC01-R04-T01 The record carries format version 5 and exactly the fields documented for it", func(t *testing.T) {
		generatingProject(t, generatingDeclaration)
		mustRun(t, runApply)
		record := plainRecord(t)
		if record["format_version"] != 5 {
			t.Fatalf("format version is %v, not 5", record["format_version"])
		}
		// A change to these fields is a change to the documented format, and
		// must come with a new format version. True marks a field that apply
		// always writes; registry_sha is written only for a registry in git.
		// Version 2 adds service_name, and makes the sha of each component
		// its component digest, where version 1 held the registry reference.
		// Version 3 adds archetype, with the archetype's runtime library pin.
		// Version 4 added data_path, the data path of the run, and version 5
		// removes it: the runtime data path is the only data path.
		documented := map[string]bool{
			"format_version": true, "last_applied": true,
			"last_applied.compiler_build": true, "last_applied.components": true, "last_applied.entities": true,
			"last_applied.files": true, "last_applied.inputs": true, "last_applied.registry_content_sha256": true,
			"last_applied.service_hash": true, "last_applied.service_name": true, "last_applied.registry_sha": false,
			"last_applied.archetype": true,
		}
		found := map[string]bool{}
		for name, value := range record {
			found[name] = true
			if name == "last_applied" {
				for inner := range value.(map[string]any) {
					found[name+"."+inner] = true
				}
			}
		}
		for name := range found {
			if _, ok := documented[name]; !ok {
				t.Errorf("the record has %s, which format version 5 does not document", name)
			}
		}
		for name, always := range documented {
			if always && !found[name] {
				t.Errorf("the record lacks %s, which format version 5 documents", name)
			}
		}

		// A record of format version 4 held the data path; the next apply reads it and writes version 5 without it.
		data, err := os.ReadFile(".stego/state.yaml")
		if err != nil {
			t.Fatal(err)
		}
		legacy := strings.Replace(string(data), "format_version: 5\n", "format_version: 4\n", 1)
		legacy = strings.Replace(legacy, "last_applied:\n", "last_applied:\n    data_path: runtime\n", 1)
		writeProjectFile(t, ".stego/state.yaml", legacy)
		mustRun(t, runApply)
		if upgraded := plainRecord(t); upgraded["format_version"] != 5 || upgraded["last_applied"].(map[string]any)["data_path"] != nil {
			t.Fatalf("apply on a record of format version 4 did not write version 5 without the data path: %v", upgraded["format_version"])
		}
	})
}

func TestDriftNamesEditedFile(t *testing.T) {
	t.Run("ST-GEN-UC02-R01-T02 The drift command fails and names a generated file that no longer matches its hash", func(t *testing.T) {
		generatingProject(t, generatingDeclaration)
		mustRun(t, runApply)
		mustRun(t, runDrift)
		var edited string
		for path := range recordedFiles(t) {
			edited = path
			break
		}
		writeProjectFile(t, filepath.Join("out", edited), "// edited by hand\n")
		printed, err := capture(t, runDrift)
		if err == nil || !strings.Contains(printed, edited) {
			t.Fatalf("drift did not report %s: %v\n%s", edited, err, printed)
		}
	})
}

func TestSameInputsGiveSameCode(t *testing.T) {
	t.Run("ST-GEN-UC04-R01-T01 Two projects from one declaration get identical files, and a repeated apply changes nothing", func(t *testing.T) {
		trees := []map[string]string{}
		for range 2 {
			generatingProject(t, generatingDeclaration)
			mustRun(t, runApply)
			first := projectTree(t)
			if printed := mustRun(t, runApply); !strings.Contains(printed, "No changes") {
				t.Fatalf("a second apply from the same inputs made changes:\n%s", printed)
			}
			second := projectTree(t)
			if !reflect.DeepEqual(first, second) {
				for path := range second {
					if first[path] != second[path] {
						t.Errorf("a second apply changed %s", path)
					}
				}
				t.Fatal("a second apply changed the project")
			}
			if printed := mustRun(t, runApply); !strings.Contains(printed, "No changes") {
				t.Fatalf("a third apply from the same inputs made changes:\n%s", printed)
			}
			if !reflect.DeepEqual(second, projectTree(t)) {
				t.Fatal("a third apply changed the project")
			}
			trees = append(trees, first)
		}
		if !reflect.DeepEqual(trees[0], trees[1]) {
			for path, data := range trees[0] {
				if trees[1][path] != data {
					t.Errorf("%s differs between the two projects", path)
				}
			}
			t.Fatal("the same declaration gave different files")
		}
	})
}

func TestApplyRemovesWhatItNoLongerGenerates(t *testing.T) {
	t.Run("ST-GEN-UC04-R02-T01 Apply deletes the files of a removed collection", func(t *testing.T) {
		generatingProject(t, generatingDeclaration)
		mustRun(t, runApply)
		before := recordedFiles(t)
		writeProjectFile(t, "service.yaml", strings.Replace(generatingDeclaration, "  notes: {entity: Note, operations: [create, read]}\n", "", 1))
		mustRun(t, runApply)
		after := recordedFiles(t)
		removed := 0
		for path := range before {
			if after[path] != nil {
				continue
			}
			removed++
			if _, err := os.Stat(filepath.Join("out", path)); !os.IsNotExist(err) {
				t.Fatalf("%s is no longer generated but is still in the output: %v", path, err)
			}
		}
		if removed == 0 {
			t.Fatal("removing a collection removed no generated file")
		}

		// A file that the new plan no longer generates, and that was edited
		// by hand, stops the apply, which names it and keeps it.
		generatingProject(t, generatingDeclaration)
		mustRun(t, runApply)
		var edited string
		for path := range recordedFiles(t) {
			if strings.Contains(path, "notes") {
				edited = path
			}
		}
		if edited == "" {
			t.Fatal("no generated file belongs to the collection notes")
		}
		writeProjectFile(t, filepath.Join("out", edited), "// edited by hand\n")
		writeProjectFile(t, "service.yaml", strings.Replace(generatingDeclaration, "  notes: {entity: Note, operations: [create, read]}\n", "", 1))
		project := projectTree(t)
		if printed, err := capture(t, runApply); err == nil || !strings.Contains(err.Error(), edited) {
			t.Fatalf("apply deleted or did not name the hand-edited file %s: %v\n%s", edited, err, printed)
		}
		if !reflect.DeepEqual(project, projectTree(t)) {
			t.Fatal("a refused apply changed the project")
		}
	})
}

func TestFailedApplyChangesNothing(t *testing.T) {
	t.Run("ST-GEN-UC04-R03-T01 An apply that fails validation names the failure and changes no project file", func(t *testing.T) {
		generatingProject(t, generatingDeclaration)
		mustRun(t, runApply)
		writeProjectFile(t, "service.yaml", strings.Replace(generatingDeclaration, "{name: title, type: string}", "{name: title, type: string, min_length: 10, max_length: 1}", 1))
		project := projectTree(t)
		printed, err := capture(t, runApply)
		if err == nil || !strings.Contains(err.Error(), "min_length") {
			t.Fatalf("apply did not name the failure: %v\n%s", err, printed)
		}
		if !reflect.DeepEqual(project, projectTree(t)) {
			t.Fatal("a failed apply changed the project")
		}
	})
}

func TestPlanWritesNothing(t *testing.T) {
	t.Run("ST-GEN-UC05-R01-T01 Plan leaves the output folder and the record unchanged", func(t *testing.T) {
		generatingProject(t, generatingDeclaration)
		mustRun(t, runPlan)
		tree := projectTree(t)
		if paths := slices.Sorted(maps.Keys(tree)); !slices.Equal(paths, []string{".stego/plan.yaml", "authz/schema.zed", "service.yaml"}) || tree["service.yaml"] != generatingDeclaration {
			t.Fatalf("plan on a new project wrote other files than the saved plan: %v", paths)
		}
		mustRun(t, runApply)
		writeProjectFile(t, "service.yaml", strings.Replace(generatingDeclaration, "{name: body, type: string}", "{name: body, type: string}, {name: author, type: string}", 1))
		project := projectTree(t)
		if printed := mustRun(t, runPlan); !strings.Contains(printed, "update:") {
			t.Fatalf("the plan shows no change for a changed declaration:\n%s", printed)
		}
		after := projectTree(t)
		if after[".stego/plan.yaml"] == "" {
			t.Fatal("plan saved no plan under .stego")
		}
		delete(after, ".stego/plan.yaml")
		if !reflect.DeepEqual(project, after) {
			t.Fatal("plan changed a project file other than the saved plan")
		}
	})
}

func TestPlanNamesEveryChange(t *testing.T) {
	t.Run("ST-GEN-UC05-R02-T01 The plan names each file that apply then creates, updates or deletes, and the record change", func(t *testing.T) {
		generatingProject(t, generatingDeclaration)
		mustRun(t, runApply)
		mustRun(t, runApply)
		// Remove one collection and add a field to another, so the next run
		// creates nothing new but updates and deletes files.
		changed := strings.Replace(generatingDeclaration, "  notes: {entity: Note, operations: [create, read]}\n", "", 1)
		changed = strings.Replace(changed, "{name: title, type: string}", "{name: title, type: string}, {name: done, type: bool}", 1)
		writeProjectFile(t, "service.yaml", changed)
		before := projectTree(t)
		plan := mustRun(t, runPlan)
		mustRun(t, runApply)
		after := projectTree(t)

		named := map[string]string{}
		for _, line := range strings.Split(plan, "\n") {
			action, path, ok := strings.Cut(strings.TrimSpace(line), ":")
			if ok && (action == "generate" || action == "update" || action == "delete") {
				named[strings.TrimSpace(path)] = action
			}
		}
		actual := map[string]string{}
		for path, data := range after {
			if old, ok := before[path]; !ok {
				actual[path] = "generate"
			} else if old != data {
				actual[path] = "update"
			}
		}
		for path := range before {
			if _, ok := after[path]; !ok {
				actual[path] = "delete"
			}
		}
		if record, ok := actual[".stego/state.yaml"]; ok {
			delete(actual, ".stego/state.yaml")
			actual["compiler state"] = record
		}
		for path, action := range actual {
			name := strings.TrimPrefix(path, "out/")
			if named[name] != action && named[path] != action {
				t.Errorf("apply did %s %s, which the plan did not name", action, path)
			}
		}
		if len(named) != len(actual) {
			t.Errorf("the plan named %d changes, and apply made %d:\n%s", len(named), len(actual), plan)
		}
		if !bytes.Contains([]byte(plan), []byte("delete:")) || !bytes.Contains([]byte(plan), []byte("update:")) {
			t.Fatalf("the plan shows no update and no delete:\n%s", plan)
		}
	})
}


// The declarations of these tests prove behaviour of the compiler that does not depend on the data path. Since the
// runtime data path is the only data path (ST-SUP-RC20-OC39), a rest-crud declaration with entities needs pooled
// tenancy, which authorization asks to be stated, the audit log, stated so that it selects its component, SpiceDB
// authorization with a schema,
// schema_generation, and a resource type and a permission of each operation on each entity. onRuntimePath adds what a declaration does not state, and
// runtimeSchema gives the schema that it names.

// runtimeHeader is what onRuntimePath adds after the language of the declaration, each line only when the
// declaration has no such key.
const runtimeHeader = `tenancy: {kind: pooled, key: tenant_id, claim: tenant, provisioning: {resource_type: service, provision: provision, retire: retire}}
authorization: {provider: spicedb, schema_path: authz/schema.zed, subject_from_identity: user}
audit: {level: change}
`

// runtimeSchemaMark is the first line of a schema that withRuntimeSchema writes.
const runtimeSchemaMark = "// The schema of the test declaration.\n"

// runtimeSchemaPath is the schema file that runtimeHeader names, from the project folder.
const runtimeSchemaPath = "authz/schema.zed"

// entityLine is the first line of an entity in the entities list.
var entityLine = regexp.MustCompile(`(?m)^  - name: ([A-Za-z][A-Za-z0-9]*)\n`)

// entitiesBlock gives where the entities list of a declaration is: from the line entities: to the next top-level
// key, or -1 when it has none.
func entitiesBlock(declaration string) (int, int) {
	start := strings.Index(declaration, "\nentities:\n")
	if start < 0 {
		return -1, -1
	}
	start++
	end := len(declaration)
	if next := regexp.MustCompile(`(?m)^[a-z_]+:`).FindStringIndex(declaration[start+len("entities:\n"):]); next != nil {
		end = start + len("entities:\n") + next[0]
	}
	return start, end
}

// resourceType is the resource type that onRuntimePath gives an entity. The type user is the subject of the
// schema, so an entity User gets the type member.
func resourceType(entity string) string {
	name := strings.ToLower(regexp.MustCompile(`([a-z0-9])([A-Z])`).ReplaceAllString(entity, "${1}_${2}"))
	if name == "user" {
		return "member"
	}
	return name
}

// onRuntimePath gives the declaration with the settings of the runtime data path that it does not state:
// tenancy, authorization and audit as runtimeHeader gives them, the setting schema_generation of postgres-adapter,
// and for each entity with no resource type its resource type and the permission of each operation (create public
// with a reason, read and list on view, every other write on edit).
func onRuntimePath(declaration string) string {
	if start, end := entitiesBlock(declaration); start >= 0 {
		block := declaration[start:end]
		locations := entityLine.FindAllStringSubmatchIndex(block, -1)
		// A name that the declaration gives twice gets a type of its own each time, as the schema needs.
		names := make([]string, len(locations))
		count := map[string]int{}
		for i, at := range locations {
			names[i] = resourceType(block[at[2]:at[3]])
			if count[names[i]]++; count[names[i]] > 1 {
				names[i] = fmt.Sprintf("%s_%d", names[i], count[names[i]])
			}
		}
		for i := len(locations) - 1; i >= 0; i-- {
			at := locations[i]
			next := len(block)
			if i+1 < len(locations) {
				next = locations[i+1][0]
			}
			if strings.Contains(block[at[1]:next], "resource_type:") {
				continue
			}
			name := names[i]
			block = block[:at[1]] + fmt.Sprintf("    resource_type: %s\n    permissions: {create: public, read: view, list: view, update: edit, patch: edit, delete: edit, upsert: edit}\n    public_reasons: {create: a new %s has no resource to decide on before it exists}\n", name, name) + block[at[1]:]
		}
		declaration = declaration[:start] + block + declaration[end:]
	}
	header := ""
	for _, line := range strings.SplitAfter(runtimeHeader, "\n") {
		key, _, _ := strings.Cut(line, ":")
		if line != "" && !regexp.MustCompile(`(?m)^`+key+`:`).MatchString(declaration) {
			header += line
		}
	}
	switch {
	case strings.Contains(declaration, "\n  postgres-adapter:"):
	case strings.Contains(declaration, "\noverrides:\n"):
		declaration = strings.Replace(declaration, "\noverrides:\n", "\noverrides:\n  postgres-adapter: {schema_generation: tables-v1}\n", 1)
	default:
		// The overrides go before the lists, so a test that appends to the last list of the declaration still does.
		header += "overrides:\n  postgres-adapter: {schema_generation: tables-v1}\n"
	}
	return strings.Replace(declaration, "language: go\n", "language: go\n"+header, 1)
}

// runtimeSchema gives the SpiceDB schema of the resource types that the declaration names, each with the
// permissions view and edit.
func runtimeSchema(declaration string) string {
	// The service-level resource of the tenant provisioning of runtimeHeader (ST-DECL-INV78).
	schema := "definition user {}\n\ndefinition service {\n\trelation operator: user\n\tpermission provision = operator\n\tpermission retire = operator\n}\n"
	seen := map[string]bool{"user": true, "service": true}
	for _, match := range regexp.MustCompile(`resource_type: *([a-z][a-z0-9_]*)`).FindAllStringSubmatch(declaration, -1) {
		if seen[match[1]] {
			continue
		}
		seen[match[1]] = true
		schema += fmt.Sprintf("\ndefinition %s {\n\trelation owner: user\n\tpermission view = owner\n\tpermission edit = owner\n}\n", match[1])
	}
	return schema
}

// withRuntimeSchema writes the schema of runtimeSchema beside a declaration that names runtimeSchemaPath, when the
// project has no schema there or one that it wrote, so a test that writes its own schema keeps it.
func withRuntimeSchema(t testing.TB, project, name, declaration string) {
	t.Helper()
	if filepath.Clean(name) != "service.yaml" || !strings.Contains(declaration, "schema_path: "+runtimeSchemaPath) {
		return
	}
	path := filepath.Join(project, runtimeSchemaPath)
	if data, err := os.ReadFile(path); err == nil && !strings.HasPrefix(string(data), runtimeSchemaMark) {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(runtimeSchemaMark+runtimeSchema(declaration)), 0o644); err != nil {
		t.Fatal(err)
	}
	// The mode is part of the record's inputs, so it must not depend on a file that was there before.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
}
