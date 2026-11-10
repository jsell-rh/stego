package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/fill"
)

// expectRefused runs plan, then apply, and expects each to exit non-zero with
// a fault that holds each text, and apply to leave the out folder and the
// record as they were.
func expectRefused(t *testing.T, p *fillProject, texts ...string) {
	t.Helper()
	before := p.tree(t, "out", ".stego")
	for _, command := range []string{"plan", "apply"} {
		stdout, stderr, err := p.stego(t, "no-key", command)
		if err == nil {
			t.Fatalf("stego %s accepted the binding:\n%s", command, stdout)
		}
		for _, text := range texts {
			if !strings.Contains(stdout+stderr, text) {
				t.Fatalf("stego %s does not name %q:\n%s\n%s", command, text, stdout, stderr)
			}
		}
	}
	after := p.tree(t, "out", ".stego")
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatal("apply changed the out folder or the record")
	}
}

func TestFillGate(t *testing.T) {
	t.Parallel()
	t.Run("ST-EXT-UC02-R02-T03 Plan and apply refuse a fill signed by a key that SIGNERS does not list, and name the fill", func(t *testing.T) {
		p := newFillProject(t)
		p.bind(t, slotBinding{"unlisted-fill", "before_create", "org-users"})
		expectRefused(t, p, "unlisted-fill", "SIGNERS does not list its key")
	})

	t.Run("ST-EXT-UC02-R03-T01 A sign-off copied into another fill with the same code does not qualify that fill", func(t *testing.T) {
		p := newFillProject(t)
		for _, file := range []string{"fill.yaml", "policy.go", "policy_test.go"} {
			p.write(t, "fills/twin-fill/"+file, string(p.read(t, "fills/qualified-fill/"+file)))
		}
		p.replace(t, "fills/twin-fill/fill.yaml", "name: qualified-fill", "name: twin-fill")
		p.must(t, "no-key", "test")
		if err := fill.WriteSignOff(p.path("fills/twin-fill/fill.yaml"), *p.manifest(t, "qualified-fill").SignOff); err != nil {
			t.Fatal(err)
		}
		p.bind(t, slotBinding{"twin-fill", "before_create", "org-users"})
		line := statusOf(t, p.must(t, "no-key", "fill", "status"), "twin-fill")
		if strings.Contains(line, string(fill.Qualified)+" ") || !strings.Contains(line, "its sign-off does not sign its evidence") {
			t.Fatalf("fill status does not show twin-fill as not qualified because its sign-off does not sign its evidence:\n%s", line)
		}
		if stdout, stderr, err := p.stego(t, "no-key", "plan"); err == nil || !strings.Contains(stdout+stderr, "twin-fill") {
			t.Fatalf("plan accepts twin-fill or does not name it: %v\n%s\n%s", err, stdout, stderr)
		}
	})

	t.Run("ST-EXT-UC02-R03-T02 A sign-off moved onto new evidence of the same fill does not qualify it", func(t *testing.T) {
		p := newFillProject(t)
		p.bind(t, slotBinding{"qualified-fill", "before_create", "org-users"})
		signOff := *p.manifest(t, "qualified-fill").SignOff
		p.replace(t, "fills/qualified-fill/policy.go", "// Evaluate allows the request.", "// Evaluate allows the request, as it did.")
		p.must(t, "no-key", "test")
		if err := fill.WriteSignOff(p.path("fills/qualified-fill/fill.yaml"), signOff); err != nil {
			t.Fatal(err)
		}
		line := statusOf(t, p.must(t, "no-key", "fill", "status"), "qualified-fill")
		if strings.Contains(line, string(fill.Qualified)+" ") || !strings.Contains(line, "its sign-off does not sign its evidence") {
			t.Fatalf("fill status does not show qualified-fill as not qualified because its sign-off does not sign its evidence:\n%s", line)
		}
		if stdout, stderr, err := p.stego(t, "no-key", "plan"); err == nil || !strings.Contains(stdout+stderr, "qualified-fill") {
			t.Fatalf("plan accepts qualified-fill or does not name it: %v\n%s\n%s", err, stdout, stderr)
		}
	})

}

func TestQualifyFillFlow(t *testing.T) {
	t.Parallel()
	t.Run("ST-EXT-UC02-FL-T The service author records evidence and signs it with a listed key, and the fill becomes qualified", func(t *testing.T) {
		p := newFillProject(t)
		p.bind(t, slotBinding{"untested-fill", "before_create", "org-users"})
		p.must(t, "listed-ssh", "test")
		if p.manifest(t, "untested-fill").Evidence == nil {
			t.Fatal("stego test wrote no evidence for untested-fill")
		}
		p.must(t, "listed-ssh", "fill", "sign", "untested-fill")
		f := p.manifest(t, "untested-fill")
		if err := sshVerify(t, p.keys.listedSSHPublic, f.SignOff, evidenceDigest(t, f)); err != nil {
			t.Fatalf("ssh-keygen -Y verify refuses the sign-off: %v", err)
		}
		expectStanding(t, p.must(t, "listed-ssh", "fill", "status"), "untested-fill", fill.Qualified)
		p.must(t, "listed-ssh", "plan")

		// The failed path, from the project of ST-EXT-TD01.
		p = newFillProject(t)
		p.replace(t, "fills/untested-fill-2/policy_test.go", "if err != nil", "if true")
		before := p.read(t, "fills/untested-fill-2/fill.yaml")
		if _, _, err := p.stego(t, "listed-ssh", "test"); err == nil {
			t.Fatal("stego test passed with a failing test")
		}
		if !bytes.Equal(before, p.read(t, "fills/untested-fill-2/fill.yaml")) {
			t.Fatal("stego test wrote evidence for the failing untested-fill-2")
		}
		if stdout, stderr, err := p.stego(t, "listed-ssh", "fill", "sign", "untested-fill-2"); err == nil || !strings.Contains(stderr, "has no evidence") {
			t.Fatalf("fill sign does not refuse a fill with no evidence: %v\n%s\n%s", err, stdout, stderr)
		}

		// The unlisted path, from the project of ST-EXT-TD01.
		p = newFillProject(t)
		p.must(t, "listed-ssh", "test")
		p.must(t, "unlisted-ssh", "fill", "sign", "untested-fill-2")
		p.bind(t, slotBinding{"untested-fill-2", "before_create", "org-groups"})
		stdout, stderr, err := p.stego(t, "listed-ssh", "plan")
		if err == nil || !strings.Contains(stdout+stderr, "untested-fill-2") || !strings.Contains(stdout+stderr, "SIGNERS does not list its key") {
			t.Fatalf("plan does not refuse untested-fill-2 for its unlisted key: %v\n%s\n%s", err, stdout, stderr)
		}
	})
}

// The fixture modules that a fill's imports resolve to, through replace
// directives, so that a fill compiles with the network off and only the
// import rule can refuse it.
var fixtureModules = map[string]map[string]string{
	"github.com/lib/pq":           {"pq.go": "package pq\n\n// Name is the name of the driver.\nconst Name = \"postgres\"\n"},
	"github.com/google/uuidx":     {"uuidx.go": "package uuidx\n\n// Name is the name of the module.\nconst Name = \"uuidx\"\n"},
	"github.com/google/uuid":      {"uuid.go": "package uuid\n\n// NewString gives a fixed identifier.\nfunc NewString() string { return \"00000000-0000-0000-0000-000000000000\" }\n", "sub/sub.go": "package sub\n\n// Name is the name of the package.\nconst Name = \"sub\"\n"},
	"github.com/stretchr/testify": {"assert/assert.go": "package assert\n\nimport \"testing\"\n\n// True fails the test when the value is false.\nfunc True(t testing.TB, value bool) bool {\n\tif !value {\n\t\tt.Error(\"not true\")\n\t}\n\treturn value\n}\n"},
}

// withFillModules makes the declaration list the fill module
// github.com/google/uuid, and resolves the fixture modules through replace
// directives in go.mod, which then holds github.com/stretchr/testify.
func (p *fillProject) withFillModules(t *testing.T) {
	t.Helper()
	p.declarationExtra = "fill_modules:\n  - {module: github.com/google/uuid, reason: fills create request IDs}\n"
	p.bind(t)
	goMod := string(p.read(t, "go.mod"))
	var require, replace string
	for module, files := range fixtureModules {
		folder := "fixture-modules/" + strings.ReplaceAll(module, "/", "_")
		p.write(t, folder+"/go.mod", "module "+module+"\n\ngo 1.21\n")
		for name, data := range files {
			p.write(t, folder+"/"+name, data)
		}
		if !strings.Contains(goMod, "\t"+module+" ") {
			require += "\t" + module + " v1.0.0\n"
		}
		replace += "replace " + module + " => ./" + folder + "\n"
	}
	p.write(t, "go.mod", goMod+"\nrequire (\n"+require+")\n\n"+replace)
}

// addImport adds an import to a Go file of a fill and a use of it.
func (p *fillProject) addImport(t *testing.T, name, path, use string) {
	t.Helper()
	p.replace(t, name, "import (\n\t\"context\"\n", "import (\n\t\"context\"\n\t\""+path+"\"\n")
	data := string(p.read(t, name))
	p.write(t, name, data+"\nvar _ = "+use+"\n")
}

// copyFill copies untested-fill under a new name.
func (p *fillProject) copyFill(t *testing.T, name string) {
	t.Helper()
	for _, file := range []string{"fill.yaml", "policy.go", "policy_test.go"} {
		p.write(t, "fills/"+name+"/"+file, string(p.read(t, "fills/untested-fill/"+file)))
	}
	p.replace(t, "fills/"+name+"/fill.yaml", "name: untested-fill", "name: "+name)
}

// expectNoEvidence fails the test unless the run's output names the fill
// and each text, and the fill's manifest is as it was.
func expectNoEvidence(t *testing.T, p *fillProject, stdout, name string, before []byte, texts ...string) {
	t.Helper()
	var line string
	for _, l := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(l, "fill "+name+": ") {
			line = l
		}
	}
	for _, text := range texts {
		if !strings.Contains(line, text) {
			t.Fatalf("stego test does not name %s with %q:\n%s", name, text, stdout)
		}
	}
	if !bytes.Equal(before, p.read(t, "fills/"+name+"/fill.yaml")) {
		t.Fatalf("stego test changed the manifest of %s", name)
	}
}

func TestFillBinding(t *testing.T) {
	t.Run("ST-EXT-UC01-R02-T01 Plan and apply refuse a qualified fill bound at a slot it does not implement, naming the fill, the slot and the collection", func(t *testing.T) {
		p := newFillProject(t)
		p.bind(t, slotBinding{"other-slot-fill", "before_create", "org-users"})
		expectRefused(t, p, "other-slot-fill", "rest-api.before_create", "rest-api.before_delete", "org-users")
		p.bind(t, slotBinding{"other-slot-fill", "before_delete", "org-users"})
		stdout := p.must(t, "no-key", "plan")
		if strings.Contains(stdout, "other-slot-fill") {
			t.Fatalf("plan reports a fault for other-slot-fill at before_delete:\n%s", stdout)
		}
	})

	t.Run("ST-EXT-UC01-R02-T02 Plan and apply refuse a qualified fill bound for a collection it does not serve, naming the fill, the slot and the collection", func(t *testing.T) {
		p := newFillProject(t)
		p.bind(t, slotBinding{"qualified-fill", "before_create", "org-groups"})
		expectRefused(t, p, "qualified-fill", "rest-api.before_create", "org-groups", "org-users")
	})

	t.Run("ST-EXT-UC01-R02-T03 Each fault of a bound fill names the path of its binding in the declaration, with the collection and the slot", func(t *testing.T) {
		p := newFillProject(t)
		p.write(t, "service.yaml", fillBaseService+p.declarationExtra+"slots:\n  - {slot: before_create, collection: org-users, gate: [qualified-fill, other-slot-fill, no-tests-fill]}\n")
		stdout, stderr, err := p.stego(t, "no-key", "validate", "--json")
		if err == nil {
			t.Fatalf("validate accepted the bindings:\n%s", stdout)
		}
		var document struct {
			Faults []struct{ Rule, Path, Found, Message string }
		}
		if err := json.Unmarshal([]byte(stdout), &document); err != nil {
			t.Fatalf("validate --json printed no document: %v\n%s\n%s", err, stdout, stderr)
		}
		want := map[string]string{"slots[0].gate[1]": "other-slot-fill", "slots[0].gate[2]": "no-tests-fill"}
		rules := map[string]string{"slots[0].gate[1]": "fill-fits-binding", "slots[0].gate[2]": "fill-qualified"}
		if len(document.Faults) != len(want) {
			t.Fatalf("got %d faults, want %d: %+v", len(document.Faults), len(want), document.Faults)
		}
		for _, fault := range document.Faults {
			if want[fault.Path] != fault.Found || rules[fault.Path] != fault.Rule {
				t.Errorf("fault %+v is not one of %v with rules %v", fault, want, rules)
			}
			for _, part := range []string{fault.Found, fault.Path, "org-users.before_create"} {
				if !strings.Contains(fault.Message, part) {
					t.Errorf("fault %q does not name %q", fault.Message, part)
				}
			}
		}
	})

	t.Run("ST-EXT-UC01-R04-T01 Plan and apply refuse a bound fill with no test file, and name it", func(t *testing.T) {
		p := newFillProject(t)
		p.bind(t, slotBinding{"no-tests-fill", "before_create", "org-users"})
		expectRefused(t, p, "no-tests-fill", "it has no test file")
	})
}

func TestFillImports(t *testing.T) {
	t.Parallel()
	t.Run("ST-EXT-UC01-R03-T01 stego test records no evidence for a fill that imports a module the declaration does not list, and names the import", func(t *testing.T) {
		p := newFillProject(t)
		p.withFillModules(t)
		p.addImport(t, "fills/untested-fill/policy.go", "github.com/lib/pq", "pq.Name")
		before := p.read(t, "fills/untested-fill/fill.yaml")
		stdout := p.must(t, "no-key", "test")
		expectNoEvidence(t, p, stdout, "untested-fill", before, "github.com/lib/pq")
		if p.manifest(t, "untested-fill-2").Evidence == nil {
			t.Fatal("untested-fill-2 got no evidence in the same run")
		}
	})

	t.Run("ST-EXT-UC01-R03-T02 stego test records no evidence for a fill that imports a module whose path only starts with a listed module's path", func(t *testing.T) {
		p := newFillProject(t)
		p.withFillModules(t)
		p.addImport(t, "fills/untested-fill/policy.go", "github.com/google/uuidx", "uuidx.Name")
		before := p.read(t, "fills/untested-fill/fill.yaml")
		stdout := p.must(t, "no-key", "test")
		expectNoEvidence(t, p, stdout, "untested-fill", before, "github.com/google/uuidx")
	})

	t.Run("ST-EXT-UC01-R03-T03 stego test records no evidence for a fill that imports a generated package other than out/slots, and names the import", func(t *testing.T) {
		p := newFillProject(t)
		p.withFillModules(t)
		p.replace(t, "fills/untested-fill/policy.go", "import (\n\t\"context\"\n", "import (\n\t\"context\"\n\t_ \"example.com/fills/out/internal/storage\"\n")
		before := p.read(t, "fills/untested-fill/fill.yaml")
		stdout := p.must(t, "no-key", "test")
		expectNoEvidence(t, p, stdout, "untested-fill", before, "example.com/fills/out/internal/storage")
	})

	t.Run("ST-EXT-UC01-R03-T04 stego test records evidence for a fill whose test file imports a module that go.mod holds and the declaration does not list", func(t *testing.T) {
		p := newFillProject(t)
		p.withFillModules(t)
		p.replace(t, "fills/untested-fill/policy_test.go", "import (\n\t\"context\"\n", "import (\n\t\"context\"\n\t\"github.com/stretchr/testify/assert\"\n")
		p.replace(t, "fills/untested-fill/policy_test.go", "\tif err != nil", "\tassert.True(t, err == nil)\n\tif err != nil")
		stdout := p.must(t, "no-key", "test")
		if p.manifest(t, "untested-fill").Evidence == nil {
			t.Fatalf("untested-fill got no evidence:\n%s", stdout)
		}
		if strings.Contains(stdout, "imports github.com/stretchr/testify") {
			t.Fatalf("stego test names testify as an import that is not allowed:\n%s", stdout)
		}
	})

	t.Run("ST-EXT-UC01-R03-T05 stego test records no evidence for a fill whose built file imports a test module, even when the file name holds the word test", func(t *testing.T) {
		p := newFillProject(t)
		p.withFillModules(t)
		p.write(t, "fills/untested-fill/testutil.go", "package untested_fill\n\nimport \"github.com/stretchr/testify/assert\"\n\nvar _ = assert.True\n")
		before := p.read(t, "fills/untested-fill/fill.yaml")
		stdout := p.must(t, "no-key", "test")
		expectNoEvidence(t, p, stdout, "untested-fill", before, "testutil.go", "github.com/stretchr/testify/assert")
	})

	t.Run("ST-EXT-INV05-T A fill whose built files import anything but out/slots, the standard library or a listed fill module gets no evidence, naming the import, and its test files may import any module of go.mod", func(t *testing.T) {
		p := newFillProject(t)
		p.withFillModules(t)
		changes := []func(name string){
			func(name string) { p.addImport(t, "fills/"+name+"/policy.go", "fmt", "fmt.Sprint") },
			func(name string) {
				p.addImport(t, "fills/"+name+"/policy.go", "github.com/google/uuid", "uuid.NewString")
			},
			func(name string) {
				p.addImport(t, "fills/"+name+"/policy.go", "github.com/google/uuid/sub", "sub.Name")
			},
			func(name string) {
				p.replace(t, "fills/"+name+"/policy_test.go", "import (\n\t\"context\"\n", "import (\n\t\"context\"\n\t\"github.com/stretchr/testify/assert\"\n")
				p.replace(t, "fills/"+name+"/policy_test.go", "\tif err != nil", "\tassert.True(t, err == nil)\n\tif err != nil")
			},
			func(name string) {
				p.write(t, "fills/"+name+"/policy.go", string(p.read(t, "fills/"+name+"/policy.go"))+"\nvar _ = &slots.BeforeDeleteRequest{}\n")
			},
			func(name string) { p.addImport(t, "fills/"+name+"/policy.go", "github.com/google/uuidx", "uuidx.Name") },
			func(name string) { p.addImport(t, "fills/"+name+"/policy.go", "github.com/lib/pq", "pq.Name") },
			func(name string) {
				p.replace(t, "fills/"+name+"/policy.go", "import (\n\t\"context\"\n", "import (\n\t\"context\"\n\t_ \"example.com/fills/out/internal/storage\"\n")
			},
			func(name string) {
				p.write(t, "fills/"+name+"/testutil.go", "package untested_fill\n\nimport \"github.com/stretchr/testify/assert\"\n\nvar _ = assert.True\n")
			},
		}
		refused := map[int][2]string{
			6: {"policy.go", "github.com/google/uuidx"},
			7: {"policy.go", "github.com/lib/pq"},
			8: {"policy.go", "example.com/fills/out/internal/storage"},
			9: {"testutil.go", "github.com/stretchr/testify/assert"},
		}
		before := map[string][]byte{}
		for i, change := range changes {
			name := fmt.Sprintf("import-copy-%d", i+1)
			p.copyFill(t, name)
			change(name)
			before[name] = p.read(t, "fills/"+name+"/fill.yaml")
		}
		stdout := p.must(t, "no-key", "test")
		for i := range changes {
			name := fmt.Sprintf("import-copy-%d", i+1)
			if want, ok := refused[i+1]; ok {
				expectNoEvidence(t, p, stdout, name, before[name], name, "the file "+want[0]+" imports "+want[1])
				continue
			}
			if p.manifest(t, name).Evidence == nil {
				t.Errorf("%s got no evidence:\n%s", name, stdout)
			}
		}
	})
}

func TestAddServiceRuleFlow(t *testing.T) {
	t.Parallel()
	t.Run("ST-EXT-UC01-FL-T An agent scaffolds a fill, writes its rule and its test, binds it, and stego test records its evidence", func(t *testing.T) {
		p := newFillProject(t)
		p.must(t, "no-key", "fill", "create", "admin-policy", "--slot", "before_create", "--collection", "org-users")
		for _, file := range []string{"fill.yaml", "fill.go"} {
			if _, err := os.Stat(p.path("fills/admin-policy/" + file)); err != nil {
				t.Fatalf("fill create did not write %s: %v", file, err)
			}
		}
		vet := exec.Command(pinnedGo(t), "vet", "./fills/admin-policy/...")
		vet.Dir = p.dir
		vet.Env = p.keys.env(p.keys.homes["no-key"])
		if out, err := vet.CombinedOutput(); err != nil {
			t.Fatalf("go vet of the scaffold: %v\n%s", err, out)
		}
		p.write(t, "fills/admin-policy/fill.go", `package admin_policy

import (
	"context"

	slots "example.com/fills/out/slots"
)

// Fill denies a caller who is not an admin the creation of an admin.
type Fill struct{}

// New creates the fill.
func New() *Fill { return &Fill{} }

var _ slots.BeforeCreateSlot = (*Fill)(nil)

// Evaluate denies the creation of an admin by a caller who is not an admin.
func (f *Fill) Evaluate(ctx context.Context, req *slots.BeforeCreateRequest) (*slots.SlotResult, error) {
	if req != nil && req.Input != nil && req.Input.Fields["role"] == "admin" && (req.Caller == nil || req.Caller.Role != "admin") {
		return &slots.SlotResult{Ok: false, ErrorMessage: "only an admin may create an admin", StatusCode: 403}, nil
	}
	return &slots.SlotResult{Ok: true}, nil
}
`)
		test := `package admin_policy

import (
	"context"
	"testing"

	slots "example.com/fills/out/slots"
)

func TestOnlyAnAdminCreatesAnAdmin(t *testing.T) {
	admin := &slots.CreateRequest{Fields: map[string]string{"role": "admin"}}
	denied, err := New().Evaluate(context.Background(), &slots.BeforeCreateRequest{Input: admin, Caller: &slots.Identity{Role: "user"}})
	if err != nil || denied.Ok {
		t.Fatalf("a user created an admin: %+v, %v", denied, err)
	}
	allowed, err := New().Evaluate(context.Background(), &slots.BeforeCreateRequest{Input: admin, Caller: &slots.Identity{Role: "admin"}})
	if err != nil || !allowed.Ok {
		t.Fatalf("an admin did not create an admin: %+v, %v", allowed, err)
	}
}
`
		p.write(t, "fills/admin-policy/fill_test.go", test)
		p.bind(t, slotBinding{"admin-policy", "before_create", "org-users"})
		fillDigest, err := fill.Digest(p.path("fills/admin-policy"))
		if err != nil {
			t.Fatal(err)
		}
		p.must(t, "no-key", "test")
		evidence := p.manifest(t, "admin-policy").Evidence
		if evidence == nil || evidence.FillDigest != fillDigest || evidence.ContractDigest != contractDigest(t, p, "before_create") {
			t.Fatalf("stego test did not write evidence with the digests of admin-policy and before_create.proto: %+v", evidence)
		}

		// The misfit path.
		p.bind(t, slotBinding{"admin-policy", "before_create", "org-groups"})
		stdout, stderr, err := p.stego(t, "no-key", "plan")
		fault := `fill "admin-policy" serves the collection org-users, not the bound collection org-groups, at the slot rest-api.before_create`
		if err == nil || !strings.Contains(stdout+stderr, fault) || !strings.Contains(stdout+stderr, "does not fit its binding") {
			t.Fatalf("plan does not refuse the misfit of admin-policy: %v\n%s\n%s", err, stdout, stderr)
		}

		// The refused path.
		p.bind(t, slotBinding{"admin-policy", "before_create", "org-users"})
		if err := os.Remove(p.path("fills/admin-policy/fill_test.go")); err != nil {
			t.Fatal(err)
		}
		before := p.read(t, "fills/admin-policy/fill.yaml")
		stdout = p.must(t, "no-key", "test")
		expectNoEvidence(t, p, stdout, "admin-policy", before, "no test file")
		p.write(t, "fills/admin-policy/fill_test.go", test)
		p.withFillModules(t)
		p.bind(t, slotBinding{"admin-policy", "before_create", "org-users"})
		p.addImport(t, "fills/admin-policy/fill.go", "github.com/lib/pq", "pq.Name")
		before = p.read(t, "fills/admin-policy/fill.yaml")
		stdout = p.must(t, "no-key", "test")
		expectNoEvidence(t, p, stdout, "admin-policy", before, "github.com/lib/pq")
	})
}

func TestRequalifyGate(t *testing.T) {
	t.Parallel()
	t.Run("ST-EXT-UC04-R01-T04 Plan and apply refuse a stale fill and name it", func(t *testing.T) {
		p := newFillProject(t)
		p.bind(t, slotBinding{"stale-fill", "before_create", "org-users"})
		expectRefused(t, p, `fill "stale-fill" is stale`)
	})

	t.Run("ST-EXT-UC04-R01-T05 A change to a file that the slot contract imports makes a qualified fill stale", func(t *testing.T) {
		p := newFillProject(t)
		p.bind(t, slotBinding{"qualified-fill", "before_create", "org-users"})
		changeRegistryComment(t, p, "common/types.proto")
		expectStanding(t, p.must(t, "no-key", "fill", "status"), "qualified-fill", fill.Stale)
		stdout, stderr, err := p.stego(t, "no-key", "plan")
		if err == nil || !strings.Contains(stdout+stderr, `fill "qualified-fill" is stale`) {
			t.Fatalf("plan does not refuse qualified-fill as stale: %v\n%s\n%s", err, stdout, stderr)
		}
	})

	t.Run("ST-EXT-UC04-R03-T01 New evidence alone does not requalify a stale fill, because its old sign-off does not carry over", func(t *testing.T) {
		p := newFillProject(t)
		p.bind(t, slotBinding{"stale-fill", "before_create", "org-users"})
		newDigest, err := fill.Digest(p.path("fills/stale-fill"))
		if err != nil {
			t.Fatal(err)
		}
		p.must(t, "no-key", "test")
		if evidence := p.manifest(t, "stale-fill").Evidence; evidence == nil || evidence.FillDigest != newDigest {
			t.Fatalf("stego test did not write new evidence with the new fill digest %s: %+v", newDigest, evidence)
		}
		expectStanding(t, p.must(t, "no-key", "fill", "status"), "stale-fill", fill.Unsigned)
		if stdout, stderr, err := p.stego(t, "no-key", "plan"); err == nil || !strings.Contains(stdout+stderr, "stale-fill") {
			t.Fatalf("plan accepts stale-fill or does not name it: %v\n%s\n%s", err, stdout, stderr)
		}
	})
}

func TestRequalifyFillFlow(t *testing.T) {
	t.Run("ST-EXT-UC04-FL-T The service author finds a stale fill, records new evidence, signs it, and plan accepts the fill again", func(t *testing.T) {
		p := newFillProject(t)
		p.bind(t, slotBinding{"stale-fill", "before_create", "org-users"})
		line := expectStanding(t, p.must(t, "listed-ssh", "fill", "status"), "stale-fill", fill.Stale)
		if !strings.Contains(line, "policy.go") {
			t.Fatalf("fill status does not name policy.go as the file that changed:\n%s", line)
		}
		p.must(t, "listed-ssh", "test")
		f := p.manifest(t, "stale-fill")
		p.must(t, "listed-ssh", "fill", "sign", "stale-fill")
		signed := p.manifest(t, "stale-fill")
		if signed.SignOff == nil || signed.SignOff.EvidenceDigest != evidenceDigest(t, f) {
			t.Fatal("fill sign did not sign the new evidence")
		}
		p.must(t, "listed-ssh", "plan")

		// The stopped path, from the project of ST-EXT-TD01.
		p = newFillProject(t)
		p.bind(t, slotBinding{"stale-fill", "before_create", "org-users"})
		expectRefused(t, p, "stale-fill")
	})
}
