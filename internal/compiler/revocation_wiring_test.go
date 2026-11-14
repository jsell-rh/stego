package compiler

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The service tests of the revocation wiring (ST-SUP-RC29-OC16 to OC19, ST-SUP-RC20-OC41): a service generated with
// stego apply from revocationService, built with the pinned Go, on a local PostgreSQL server. The check of
// testdata/revocation_wiring/check runs in the module of the generated service: it starts a fake SpiceDB server and a
// fake token key set in its own process, runs the migration job and the service binary, and drives the service over
// HTTP. On the runtime data path a create is public with the owner from the caller, and a field subject does not
// change after the create (ST-SUP-RC29-OC01, ST-SUP-Q118), so viewers are added and removed by the declared commands
// share-document and unshare-document, whose fills write the relation document#viewer through the typed relation
// helpers of the command contract (ST-SUP-RC32-OC13; orchestrator, 2026-11-13).

// revocationFixture is the folder of the check of the revocation wiring tests.
const revocationFixture = "testdata/revocation_wiring"

// revocationSchema is the schema of the service: documents with an owner and viewers, and the service resource of
// the tenancy provisioning.
const revocationSchema = `definition user {}

definition document {
	relation owner: user
	relation viewer: user
	permission view = owner + viewer
	permission edit = owner
}

definition service {
	relation operator: user
	permission provision = operator
	permission retire = operator
}
`

// revocationService is the declaration of the service: the entity Document with resource type document, a public
// create whose owner is the caller, and the collection documents; the commands share-document and unshare-document,
// which write the relation document#viewer; idempotency on create; and the bounds of a wait for a revocation: a cap
// of 3 s, at most 3 waits, and at most 2 of one tenant.
const revocationService = `kind: service
name: docs-svc
archetype: rest-crud
language: go
base_path: /api/docs/v1
error_type_base: https://errors.example.com/docs/
tenancy:
  kind: pooled
  key: tenant_id
  claim: tenant
  provisioning: {resource_type: service, provision: provision, retire: retire}
experimental_acceptances:
  - {component: spicedb-authz, accepted_until: 2027-04-30, reason: the revocation tests take the authorization component}
  - {component: tenancy, accepted_until: 2027-04-30, reason: authorization needs pooled tenancy}
  - {component: audit-log, accepted_until: 2027-04-30, reason: the runtime data path audits each write}
  - {component: idempotency, accepted_until: 2027-04-30, reason: the runtime data path keys each create}
authorization:
  provider: spicedb
  schema_path: authz/schema.zed
  subject_from_identity: user
audit:
  level: change
idempotency:
  required_on: [create]
overrides:
  jwt-auth:
    issuer: https://issuer.docs.test/realms/r1
    audience: docs-svc
  postgres-adapter:
    schema_generation: tables-v1
    writer_lease: "off"
  command:
    proto_files:
      - {path: proto/docs.proto, import_path: docs/v1/docs.proto}
  spicedb-authz:
    wait_cap: 3s
    waiter_limit: 3
    tenant_waiter_limit: 2
entities:
  - name: Document
    fields:
      - {name: title, type: string, min_length: 1, max_length: 255}
    resource_type: document
    public_reasons: {create: a new document has no resource to decide on before it exists}
    permissions: {create: public, read: view, update: edit, delete: edit, list: view}
    relationships:
      - {relation: owner, subject_source: caller, subject_type: user}
collections:
  documents:
    entity: Document
    operations: [create, read, update, delete, list]
commands:
  - name: share-document
    writes: [Document]
    resource_type: document
    permission: edit
    resource_member: documentId
    route: POST /documents/{documentId}/viewers
    fill: share-document
    relationships_written: [document#viewer]
  - name: unshare-document
    writes: [Document]
    resource_type: document
    permission: edit
    resource_member: documentId
    route: POST /documents/{documentId}/unshare
    fill: unshare-document
    relationships_written: [document#viewer]
`

// TestRevocationWiring generates the service of revocationService with stego apply, builds it with the pinned Go,
// reads its emitted table and router, and runs the check of testdata/revocation_wiring/check in its module once,
// against the local PostgreSQL server of STEGO_TEST_POSTGRES_DSN. A missing server fails the test: a missing result
// is not a pass.
func TestRevocationWiring(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a generated service")
	}
	// The check turns TLS on at the server and off again, as the checks of TestRuntimeWiring and TestRuntimeGRPC do,
	// so it does not run in parallel with them.
	if os.Getenv("STEGO_TEST_POSTGRES_DSN") == "" {
		t.Fatal("STEGO_TEST_POSTGRES_DSN is not set: this test needs a local PostgreSQL server")
	}
	p := newRegistryProject(t, stegoBinary(t), runtimeArchetype)
	if err := os.CopyFS(p.dir, os.DirFS(filepath.Join(revocationFixture, "project"))); err != nil {
		t.Fatal(err)
	}
	p.write("authz/schema.zed", revocationSchema)
	p.write("service.yaml", revocationService)
	p.stego("apply")

	t.Run("ST-SUP-RC29-OC18-T02 The status route comes from the operation table", func(t *testing.T) {
		table := p.read("out/internal/authz/authz.go")
		entry := regexp.MustCompile(`(?m)^\t\{[^\n]*"GET /authz/revocations/\{id\}"[^\n]*\},$`)
		entries := entry.FindAllString(table, -1)
		if len(entries) != 1 || !strings.Contains(entries[0], "Writer: true") || strings.Contains(entries[0], "Public") ||
			strings.Count(table, "/authz/revocations/") != 1 {
			t.Fatalf("the operation table holds %q, want one entry of GET /authz/revocations/{id} with the decision Writer, not public, and the route nowhere else", entries)
		}
		router := p.read("out/internal/stegoruntime/runtime.go")
		main := p.read("out/main.go")
		// The runtime wiring registers the handler from the table entry alone; main.go only forwards the pattern of
		// that entry to Routes, as it forwards the route of each declared command.
		registered := regexp.MustCompile(`(?s)for _, o := range svcauthz\.Operations \{\s*if o\.Writer \{\s*rt\.routes\.Handle\(o\.Route, command\.ServeRevocationStatus\(`)
		if !registered.MatchString(router) || strings.Contains(router, "/authz/revocations") || strings.Count(router, "ServeRevocationStatus(") != 1 ||
			strings.Count(main, "/authz/revocations/") != 1 || !strings.Contains(main, `mux.Handle("GET /authz/revocations/{id}", service.Routes())`) {
			t.Fatal("the router does not register the revocation status route from its table entry alone")
		}
	})

	check, err := os.ReadFile(filepath.Join(revocationFixture, "check", "revocation_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	p.write("out/revocationcheck/revocation_test.go", string(check))
	p.useLocalRuntime()
	p.goCommand("mod", "tidy")
	binary := filepath.Join(p.dir, "bin", "service")
	p.goCommand("build", "-o", binary, "./out")
	cmd := exec.Command(wiringPinnedGo(), "test", "-count=1", "-timeout=300s", "-v", "-run", "^TestRevocationWiring$", "./out/revocationcheck")
	cmd.Dir = p.dir
	cmd.Env = wiringGoEnv("STEGO_REVOCATION_BINARY=" + binary)
	output, runErr := cmd.CombinedOutput()
	// passed fails the subtest when the check of the same name did not run and pass in the generated module.
	passed := func(t *testing.T) {
		name := strings.TrimPrefix(t.Name(), "TestRevocationWiring/")
		if !strings.Contains(string(output), "--- PASS: TestRevocationWiring/"+name+" ") {
			t.Fatalf("the check %s of the generated service did not run and pass (go test: %v):\n%s", strings.SplitN(name, "_", 2)[0], runErr, output)
		}
	}
	t.Run("ST-SUP-RC29-OC16-T01 A revoking unshare answers pending, and its replay is byte for byte", passed)
	t.Run("ST-SUP-RC29-OC16-T02 An operation that writes no tuple sends no revocation header", passed)
	t.Run("ST-SUP-RC29-OC17-T01 The status route answers pending, then complete with no token", passed)
	t.Run("ST-SUP-RC29-OC17-T03 The status route reports a parked revocation", passed)
	t.Run("ST-SUP-RC29-OC17-T04 The status route reports a failed revocation", passed)
	t.Run("ST-SUP-RC29-OC18-T01 Only the writer of a revocation reads its status", passed)
	t.Run("ST-SUP-RC29-OC19-T01 Prefer wait returns at the delivery, and the cap cuts a longer wait", passed)
	t.Run("ST-SUP-RC29-OC19-T02 A waiting request holds no database connection", passed)
	t.Run("ST-SUP-RC29-OC19-T03 A request over the waiter limit is answered at once", passed)
	t.Run("ST-SUP-RC20-OC41-T01 A list that names the caller's write waits for it and shows it", passed)
	t.Run("ST-SUP-RC20-OC42-T01 A read that names the caller's write waits for it and shows it", passed)
	t.Run("ST-SUP-RC20-OC42-T02 A wait does not open a denied row, and reads and the status route share the waiter limit", passed)
	t.Run("ST-SUP-RC32-OC13-T01 A fill shares and unshares a document through the typed relation helpers", passed)
}
