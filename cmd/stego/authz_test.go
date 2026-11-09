package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/declaration"
)

// The system tests of ST-DECL-UC06 Declare authorization. Each runs the stego binary built from the commit under
// test in a project folder of ST-DECL-TD03: ST-DECL-TD02 with authorization bound to the fixture schema
// authz/schema.zed, which internal/declaration/testdata/td03 holds.

// td03Fixture is the folder that holds the declaration and the schema of ST-DECL-TD03.
const td03Fixture = "../../internal/declaration/testdata/td03"

// td03Service reads the declaration of ST-DECL-TD03.
func td03Service(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(td03Fixture, "service.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// td03Schema reads the fixture schema of ST-DECL-TD03.
func td03Schema(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(td03Fixture, "authz", "schema.zed"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// td03Project makes the project of ST-DECL-TD03 with the declaration given and the fixture schema.
func td03Project(t *testing.T, service string) string {
	t.Helper()
	project := td02Project(t, service)
	putFile(t, project, "authz/schema.zed", td03Schema(t))
	return project
}

// edit replaces the first match of old in text, which must hold it.
func edit(t *testing.T, text, old, new string) string {
	t.Helper()
	if !strings.Contains(text, old) {
		t.Fatalf("the declaration does not hold %q", old)
	}
	return strings.Replace(text, old, new, 1)
}

var authzFillsFixture onceFixture
var authzFillsDir string

// authzFills gives a project of ST-DECL-TD03 that holds two qualified fills, each with a test, its evidence and a
// sign-off by a key that its SIGNERS file lists: policy at before_create of todos and relate at after_create of
// todos. They are made in the TD03 project itself, so the slot contracts they were qualified against are those of
// TD03.
func authzFills(t *testing.T) string {
	t.Helper()
	keys := testFillKeys(t)
	authzFillsFixture.get(t, func() (string, error) {
		p := &fillProject{dir: filepath.Join(fixtureRoot, "authz-fills", "fills"), keys: keys}
		if err := os.MkdirAll(p.dir, 0755); err != nil {
			return "", err
		}
		run := func(key string, args ...string) error {
			if stdout, stderr, err := p.stego(t, key, args...); err != nil {
				return fmt.Errorf("stego %s: %v\n%s\n%s", strings.Join(args, " "), err, stdout, stderr)
			}
			return nil
		}
		if err := run("no-key", "init", "--archetype", "rest-crud"); err != nil {
			return "", err
		}
		// Generation refuses a declaration that binds authorization while the registry holds no spicedb-authz
		// (ST-DECL-UC06-R07), so the fills are generated against TD03 with its authorization left out. The slot
		// contracts are the same: no slot contract carries the authorization of the declaration yet.
		p.write(t, "service.yaml", withoutAuthorization(td03Service(t)))
		p.write(t, "authz/schema.zed", td03Schema(t))
		p.write(t, "SIGNERS", keys.signers)
		p.addFill(t, "policy", "before_create", "todos", true)
		for file, data := range afterCreateFill("relate") {
			p.write(t, "fills/relate/"+file, data)
		}
		for _, step := range []struct{ key, args string }{{"no-key", "apply"}, {"no-key", "test"}, {"listed-ssh", "fill sign policy"}, {"listed-ssh", "fill sign relate"}} {
			if err := run(step.key, strings.Fields(step.args)...); err != nil {
				return "", err
			}
		}
		authzFillsDir = p.dir
		return "", nil
	})
	return authzFillsDir
}

// withoutAuthorization is a declaration of ST-DECL-TD03 with its authorization setting and the authorization parts
// of Todo left out.
func withoutAuthorization(service string) string {
	var kept []string
	skip := false
	for _, line := range strings.Split(service, "\n") {
		switch {
		case line == "authorization:":
			skip = true
			continue
		case skip && strings.HasPrefix(line, "  "):
			continue
		}
		skip = false
		if strings.HasPrefix(line, "    resource_type:") || strings.HasPrefix(line, "    permissions:") || strings.HasPrefix(line, "    relationships:") || strings.HasPrefix(line, "      - {relation:") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// afterCreateFill gives the files of a fill at after_create of todos that passes each request, with its test.
func afterCreateFill(name string) map[string]string {
	return map[string]string{
		"fill.yaml": fmt.Sprintf("kind: fill\nname: %s\nimplements: rest-api.after_create\ncollection: todos\n", name),
		"relate.go": fmt.Sprintf(`package %s

import (
	"context"

	slots "example.com/fills/out/slots"
)

// Relate passes each created row.
type Relate struct{}

// New makes the fill.
func New() *Relate { return &Relate{} }

var _ slots.AfterCreateSlot = (*Relate)(nil)

// Evaluate passes the request.
func (r *Relate) Evaluate(ctx context.Context, req *slots.AfterCreateRequest) (*slots.SlotResult, error) {
	return &slots.SlotResult{Ok: true}, nil
}
`, name),
		"relate_test.go": fmt.Sprintf(`package %s

import (
	"context"
	"testing"

	slots "example.com/fills/out/slots"
)

func TestRelatePasses(t *testing.T) {
	result, err := New().Evaluate(context.Background(), &slots.AfterCreateRequest{})
	if err != nil || result == nil || !result.Ok {
		t.Fatalf("the fill refused: %%v", err)
	}
}
`, name),
	}
}

// td03WithFills makes a project of ST-DECL-TD03 with the declaration given and the qualified fills of authzFills.
func td03WithFills(t *testing.T, service string) string {
	t.Helper()
	project := td03Project(t, service)
	source := authzFills(t)
	for _, name := range []string{"policy", "relate"} {
		if output, err := exec.Command("cp", "-a", filepath.Join(source, "fills", name), filepath.Join(project, "fills", name)).CombinedOutput(); err != nil {
			t.Fatalf("copy the fill %s: %v\n%s", name, err, output)
		}
	}
	data, err := os.ReadFile(filepath.Join(source, "SIGNERS"))
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, project, "SIGNERS", string(data))
	return project
}

// faultsOf gives the faults of the rule.
func faultsOf(document declaration.FaultsDocument, rule string) []declaration.ValidationError {
	var faults []declaration.ValidationError
	for _, fault := range document.Faults {
		if fault.Rule == rule {
			faults = append(faults, fault)
		}
	}
	return faults
}

// expectRefusal runs stego validate on the project and expects a non-zero exit with exactly one fault of the
// rule, which names each text in its value found, its path or its message. It gives that fault.
func expectRefusal(t *testing.T, project, rule string, texts ...string) declaration.ValidationError {
	t.Helper()
	document, run := validateJSON(t, project)
	if run.code == 0 || document.Accepted {
		t.Fatalf("stego validate accepted the declaration:\n%s", run.stdout)
	}
	faults := faultsOf(document, rule)
	if len(faults) != 1 {
		t.Fatalf("want one fault of %s, got:\n%s", rule, run.stdout)
	}
	whole := faults[0].Found + " " + faults[0].Path + " " + faults[0].Message
	for _, text := range texts {
		if !strings.Contains(whole, text) {
			t.Errorf("the fault does not name %q: %+v", text, faults[0])
		}
	}
	return faults[0]
}

// expectAcceptance runs stego validate on the project and expects exit 0 and no fault.
func expectAcceptance(t *testing.T, project string) {
	t.Helper()
	document, run := validateJSON(t, project)
	if run.code != 0 || !document.Accepted || len(document.Faults) != 0 {
		t.Fatalf("stego validate refused the declaration, exit %d:\n%s\n%s", run.code, run.stdout, run.stderr)
	}
}

// systemBinding is a binding on todos at before_create with the operator gate and the one fill policy, which runs
// as system.
const systemBinding = "slots:\n  - collection: todos\n    slot: before_create\n    gate: [policy]\n    runs_as: system\n"

func TestDeclareAuthorization(t *testing.T) {
	td03 := td03Service(t)

	t.Run("ST-DECL-UC06-R01-T01 A declaration that names the provider, the schema path and the subject field is accepted, and its schema is compiled", func(t *testing.T) {
		project := td03Project(t, td03)
		expectAcceptance(t, project)
		putFile(t, project, "authz/schema.zed", edit(t, td03Schema(t), "\trelation viewer: user\n}", "\trelation viewer user\n}"))
		fault := expectRefusal(t, project, "schema-compile", "authz/schema.zed")
		if fault.Found != "authz/schema.zed:4" || !strings.Contains(fault.Message, "line 4") {
			t.Errorf("the schema fault does not name line 4: %+v", fault)
		}
	})

	missingSetting := func(t *testing.T, line, key, name string) {
		{
			fault := expectRefusal(t, td03Project(t, edit(t, td03, line, "")), "authorization-setting", "authorization."+key, "setting "+name+" is missing")
			if fault.Path != "authorization."+key {
				t.Errorf("the fault is not at the missing setting: %+v", fault)
			}
		}
	}
	t.Run("ST-DECL-UC06-R01-T02 Authorization bound with no provider is refused, naming the missing setting", func(t *testing.T) { missingSetting(t, "  provider: spicedb\n", "provider", "provider") })
	t.Run("ST-DECL-UC06-R01-T03 Authorization bound with no schema path is refused, naming the missing setting", func(t *testing.T) {
		missingSetting(t, "  schema_path: authz/schema.zed\n", "schema_path", "schema path")
	})
	t.Run("ST-DECL-UC06-R01-T04 Authorization bound with no subject from identity is refused, naming the missing setting", func(t *testing.T) {
		missingSetting(t, "  subject_from_identity: user\n", "subject_from_identity", "subject from identity")
	})

	t.Run("ST-DECL-UC06-R01-T05 A schema path outside the service's own source is refused, naming the path", func(t *testing.T) {
		project := td03Project(t, td03)
		shared := filepath.Join(filepath.Dir(project), "shared", "schema.zed")
		putFile(t, filepath.Dir(project), "shared/schema.zed", td03Schema(t))
		for _, outside := range []string{"../shared/schema.zed", shared} {
			putFile(t, project, "service.yaml", edit(t, td03, "schema_path: authz/schema.zed", "schema_path: "+outside))
			fault := expectRefusal(t, project, "schema-path", outside)
			if fault.Path != "authorization.schema_path" || fault.Found != outside {
				t.Errorf("the fault does not name the schema path %s: %+v", outside, fault)
			}
		}
	})

	lackingName := func(t *testing.T, old, new, name, kind string) {
		{
			service := edit(t, td03, old, new)
			fault := expectRefusal(t, td03Project(t, service), "schema-names", name, "the "+kind+" ")
			if fault.Found != name {
				t.Errorf("the fault does not name %s as found: %+v", name, fault)
			}
			if _, value := resolvePath(t, []byte(service), fault.Path); value == nil || value.Value != name {
				t.Errorf("the path %s does not lead to %s in the declaration", fault.Path, name)
			}
		}
	}
	t.Run("ST-DECL-UC06-R02-T01 A resource type the schema lacks is refused, naming the name and where it is used", func(t *testing.T) {
		lackingName(t, "resource_type: document", "resource_type: archive", "archive", "resource type")
	})
	t.Run("ST-DECL-UC06-R02-T02 A permission the schema lacks is refused, naming the name and where it is used", func(t *testing.T) { lackingName(t, "delete: edit", "delete: destroy", "destroy", "permission") })
	t.Run("ST-DECL-UC06-R02-T03 A relation the schema lacks is refused, naming the name and where it is used", func(t *testing.T) {
		lackingName(t, "{relation: owner,", "{relation: approver,", "approver", "relation")
	})
	t.Run("ST-DECL-UC06-R02-T04 A caveat parameter the schema lacks is refused, naming the name and where it is used", func(t *testing.T) {
		lackingName(t, "    relationships:\n", "    context_fields: [request.region]\n    relationships:\n", "request.region", "caveat parameter")
	})

	t.Run("ST-DECL-UC06-R03-T03 A binding that runs as system with no system subject type is refused, naming the binding", func(t *testing.T) {
		fault := expectRefusal(t, td03WithFills(t, td03+systemBinding), "no-bypass", "todos", "before_create", "runs as system", "names no system subject type")
		if fault.Path != "slots[0].runs_as" {
			t.Errorf("the fault is not at the binding: %+v", fault)
		}
	})

	t.Run("ST-DECL-UC06-R03-T04 A system subject type that the schema does not define, or that no relation grants, is refused, naming the type", func(t *testing.T) {
		for subject, reason := range map[string]string{"robot": "does not define", "folder": "no relation of the schema"} {
			service := edit(t, td03, "  subject_from_identity: user\n", "  subject_from_identity: user\n  system_subject_type: "+subject+"\n") + systemBinding
			fault := expectRefusal(t, td03WithFills(t, service), "no-bypass", subject, reason, "todos", "before_create")
			if fault.Found != subject {
				t.Errorf("the fault does not name %s: %+v", subject, fault)
			}
		}
	})

	t.Run("ST-DECL-UC06-R03-T05 A binding that runs as system with a system subject type that the schema grants relations to is accepted", func(t *testing.T) {
		service := edit(t, td03, "  subject_from_identity: user\n", "  subject_from_identity: user\n  system_subject_type: service_account\n") + systemBinding
		expectAcceptance(t, td03WithFills(t, service))
	})

	t.Run("ST-DECL-UC06-R04-T01 A relationship with no relation, subject source or subject type is refused", func(t *testing.T) {
		for _, c := range []struct{ old, key string }{
			{"relation: owner, ", "relation"},
			{"subject_source: field, ", "subject_source"},
			{", subject_type: user", "subject_type"},
		} {
			fault := expectRefusal(t, td03Project(t, edit(t, td03, c.old, "")), "relationship", "entities[0].relationships[0]."+c.key)
			if !strings.Contains(fault.Message, "the relationship") || !strings.Contains(fault.Message, "has no "+strings.ReplaceAll(c.key, "_", " ")) {
				t.Errorf("the fault does not name the relationship and the part left out: %+v", fault)
			}
		}
	})

	t.Run("ST-DECL-UC06-R07-T01 An entity a collection reads with no resource type is refused, naming the entity", func(t *testing.T) {
		service := edit(t, edit(t, td03, "    resource_type: document\n", ""), "    relationships:\n      - {relation: owner, subject_source: field, subject_field: owner_id, subject_type: user}\n", "")
		expectRefusal(t, td03Project(t, service), "authorization-coverage", `"Todo"`, "no resource type")
	})

	t.Run("ST-DECL-UC06-R07-T02 Each operation with no permission is refused as its own fault", func(t *testing.T) {
		document, run := validateJSON(t, td03Project(t, edit(t, td03, "update: edit, delete: edit, ", "")))
		faults := faultsOf(document, "authorization-coverage")
		if run.code == 0 || len(faults) != 2 {
			t.Fatalf("want a refusal with two coverage faults, got exit %d:\n%s", run.code, run.stdout)
		}
		for i, operation := range []string{"update", "delete"} {
			if faults[i].Found != "todos."+operation || !strings.Contains(faults[i].Message, `"todos"`) || !strings.Contains(faults[i].Message, `"`+operation+`"`) {
				t.Errorf("fault %d does not name todos and %s: %+v", i, operation, faults[i])
			}
		}
	})

	t.Run("ST-DECL-UC06-R07-T03 A public operation is accepted only with a reason", func(t *testing.T) {
		public := edit(t, td03, "list: view}", "list: public}")
		expectRefusal(t, td03Project(t, public), "authorization-coverage", "todos.list", "public", "no reason")
		reasoned := edit(t, public, "    relationships:\n", "    public_reasons: {list: the catalog is public by design}\n    relationships:\n")
		expectAcceptance(t, td03Project(t, reasoned))
	})

	t.Run("ST-DECL-UC06-R01-T06 A schema that defines the floor marker stego_floor is refused, naming it", func(t *testing.T) {
		project := td03Project(t, td03)
		putFile(t, project, "authz/schema.zed", td03Schema(t)+"\ndefinition stego_floor {\n\trelation marker: stego_floor\n}\n")
		fault := expectRefusal(t, project, "schema-names", "stego_floor", "adds to the schema it writes")
		if fault.Found != "stego_floor" || fault.Path != "authorization.schema_path" {
			t.Errorf("the fault does not name stego_floor at the schema path: %+v", fault)
		}
	})

	t.Run("ST-DECL-UC06-R03-T06 A system subject type other than service_account is refused, also when the schema defines it and grants it relations", func(t *testing.T) {
		service := edit(t, td03, "  subject_from_identity: user\n", "  subject_from_identity: user\n  system_subject_type: user\n") + systemBinding
		fault := expectRefusal(t, td03WithFills(t, service), "no-bypass", "user", "is not service_account")
		if fault.Path != "authorization.system_subject_type" || fault.Found != "user" || len(fault.Allowed) != 1 || fault.Allowed[0] != "service_account" {
			t.Errorf("the fault does not name user and allow only service_account: %+v", fault)
		}
	})

	t.Run("ST-DECL-UC06-R07-T05 An entity whose tenant is system with a resource type or permissions is refused, saying why", func(t *testing.T) {
		system := edit(t, td03, "  - name: Todo\n", "  - name: Todo\n    tenant: system\n")
		document, run := validateJSON(t, td03Project(t, system))
		faults := faultsOf(document, "system-entity-protected")
		if run.code == 0 || len(faults) != 2 {
			t.Fatalf("want a refusal with two system-entity-protected faults, got exit %d:\n%s", run.code, run.stdout)
		}
		for i, key := range []string{"resource_type", "permissions"} {
			if faults[i].Path != "entities[0]."+key || !strings.Contains(faults[i].Message, `"Todo"`) || !strings.Contains(faults[i].Message, "tenant-qualified") {
				t.Errorf("fault %d does not name Todo, its %s and why: %+v", i, key, faults[i])
			}
		}
	})

	t.Run("ST-DECL-UC06-R07-T06 Generation refuses a declaration that binds authorization while the registry holds no spicedb-authz, naming the component", func(t *testing.T) {
		project := td03Project(t, td03)
		expectAcceptance(t, project)
		for _, command := range []string{"plan", "apply"} {
			run := stegoIn(t, project, nil, command)
			output := run.stdout + run.stderr
			if run.code == 0 || !strings.Contains(output, "generation refused") || !strings.Contains(output, "spicedb-authz") {
				t.Errorf("stego %s did not refuse the declaration for the missing component spicedb-authz, exit %d:\n%s", command, run.code, output)
			}
		}
		if _, err := os.Stat(filepath.Join(project, "out")); !os.IsNotExist(err) {
			t.Errorf("the refused apply wrote the output folder: %v", err)
		}
	})

	t.Run("ST-DECL-UC06-FL-T An author binds authorization to a schema in the service source, and checking compiles it and accepts the declaration", func(t *testing.T) {
		service := td03 + "slots:\n  - collection: todos\n    slot: after_create\n    chain: [relate]\n    relationships_written: [document#viewer]\n"
		expectAcceptance(t, td03WithFills(t, service))
	})
}
