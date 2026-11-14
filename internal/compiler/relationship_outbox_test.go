package compiler

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The system tests of ST-DECL-UC06-R04 on a running service: the service generated from the declaration of
// ST-DECL-TD03 with stego apply, built with the pinned Go, on a local PostgreSQL server with the fakes of
// ST-DECL-TD05. The check of testdata/relationship_outbox/check runs in the module of the generated service: it
// starts a fake SpiceDB server and a fake token key set in its own process, runs the migration job and the service
// binary, and drives the service over HTTP.

// relationshipFixture is the folder of the check of the relationship tests.
const relationshipFixture = "testdata/relationship_outbox"

// td03Folder is the folder of the declaration and the schema of ST-DECL-TD03.
const td03Folder = "../declaration/testdata/td03"

// relationshipOverrides are the settings that the runtime data path needs and that ST-DECL-TD03 does not state:
// the audit of each write, the issuer and the audience of the token verifier, and the schema generation of the
// tables, with no writer lease.
const relationshipOverrides = `audit:
  level: change
overrides:
  jwt-auth:
    issuer: https://issuer.todo.test/realms/r1
    audience: todo-svc
  postgres-adapter:
    schema_generation: tables-v1
    writer_lease: "off"
entities:
`

// td03OnRuntimePath is the declaration of ST-DECL-TD03 in the form that the runtime data path, the only data path
// (ST-SUP-RC20-OC39), generates: a create of the top-level Todo is public with a reason (ST-SUP-RC29-OC01), and a
// public create may not write a field-sourced relationship, since the client would choose the subject (ST-SUP-Q118),
// so the owner relationship takes its subject from the caller (ST-DECL-UC06-R04-T04).
func td03OnRuntimePath(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(td03Folder, "service.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	service := string(data)
	for _, edit := range [][2]string{
		{"permissions: {create: edit,", "public_reasons: {create: a new todo has no resource to decide on before it exists}\n    permissions: {create: public,"},
		{"{relation: owner, subject_source: field, subject_field: owner_id, subject_type: user}", "{relation: owner, subject_source: caller, subject_type: user}"},
		{"  - {component: tenancy,", "  - {component: audit-log, accepted_until: 2027-04-30, reason: the runtime data path audits each write}\n  - {component: tenancy,"},
		{"entities:\n", relationshipOverrides},
	} {
		if !strings.Contains(service, edit[0]) {
			t.Fatalf("the declaration of ST-DECL-TD03 does not hold %q", edit[0])
		}
		service = strings.Replace(service, edit[0], edit[1], 1)
	}
	return service
}

// TestRelationshipOutbox generates the service of ST-DECL-TD03 with stego apply, builds it with the pinned Go, and
// runs the check of testdata/relationship_outbox/check in its module once, against the local PostgreSQL server of
// STEGO_TEST_POSTGRES_DSN. A missing server fails the test: a missing result is not a pass.
func TestRelationshipOutbox(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a generated service")
	}
	// The check turns TLS on at the server and off again, as the checks of TestRuntimeWiring and TestRuntimeGRPC do,
	// so it does not run in parallel with them: a test that is not parallel ends before the parallel tests start.
	if os.Getenv("STEGO_TEST_POSTGRES_DSN") == "" {
		t.Fatal("STEGO_TEST_POSTGRES_DSN is not set: this test needs a local PostgreSQL server")
	}
	p := newRegistryProject(t, stegoBinary(t), runtimeArchetype)
	schema, err := os.ReadFile(filepath.Join(td03Folder, "authz", "schema.zed"))
	if err != nil {
		t.Fatal(err)
	}
	p.write("authz/schema.zed", string(schema))
	p.write("service.yaml", td03OnRuntimePath(t))
	if err := os.MkdirAll(filepath.Join(p.dir, "fills"), 0o755); err != nil {
		t.Fatal(err)
	}
	p.stego("apply")
	check, err := os.ReadFile(filepath.Join(relationshipFixture, "check", "relationship_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	p.write("out/relationshipcheck/relationship_test.go", string(check))
	p.useLocalRuntime()
	p.goCommand("mod", "tidy")
	binary := filepath.Join(p.dir, "bin", "service")
	p.goCommand("build", "-o", binary, "./out")
	cmd := exec.Command(wiringPinnedGo(), "test", "-count=1", "-timeout=300s", "-v", "-run", "^TestRelationshipOutbox$", "./out/relationshipcheck")
	cmd.Dir = p.dir
	cmd.Env = wiringGoEnv("STEGO_RELATIONSHIP_BINARY=" + binary)
	output, runErr := cmd.CombinedOutput()
	// passed fails the subtest when the check of the same name did not run and pass in the generated module.
	passed := func(t *testing.T) {
		name := strings.TrimPrefix(t.Name(), "TestRelationshipOutbox/")
		if !strings.Contains(string(output), "--- PASS: TestRelationshipOutbox/"+name+" ") {
			t.Fatalf("the check %s of the generated service did not run and pass (go test: %v):\n%s", strings.SplitN(name, "_", 2)[0], runErr, output)
		}
	}
	t.Run("ST-DECL-UC06-R04-T02 A create writes the relationship through the outbox after commit, and the stored instance holds the consistency token", passed)
	t.Run("ST-DECL-UC06-R04-T03 A delete removes the relationship through the outbox", passed)
	t.Run("ST-DECL-UC06-R04-T04 A relationship with the subject source caller takes the subject from the caller", passed)
}
