// Package check runs the example user-management on the runtime data path as an operator runs it: the migration
// job and then the service binary, built from the generated main.go, against a local PostgreSQL server
// (STEGO_TEST_POSTGRES_DSN) and a local SpiceDB container (STEGO_TEST_SPICEDB), and calls its collections over HTTP.
package check

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	_ "github.com/jackc/pgx/v5/stdlib"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/jsell-rh/stego/registry/runtime/authn"
	"github.com/jsell-rh/stego/registry/runtime/authn/authntest"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/tenant"

	"github.com/example/service/out/internal/stegoruntime"
	"github.com/example/service/out/internal/tenancy"
)

const (
	issuer   = "https://issuer.example.test/realms/users"
	audience = "user-management"
	password = "example-check"
	base     = "/api/user-mgmt/v1"
)

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func execAll(t *testing.T, db *sql.DB, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%.100s: %v", statement, err)
		}
	}
}

func quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func writeFile(t *testing.T, name string, data []byte) string {
	file := filepath.Join(t.TempDir(), name)
	must(t, os.WriteFile(file, data, 0o600))
	return file
}

// service is the running example: its database, its SpiceDB and its address.
type service struct {
	t     *testing.T
	ctx   context.Context
	admin *sql.DB
	spice *grpc.ClientConn
	key   *authntest.Key
	base  string
	log   *bytes.Buffer
	// cursor is the cursor key of the service, and processEnv the environment of the service binary on a port.
	cursor     []byte
	processEnv func(port string) []string
}

// start makes a database of its own with the roles of the tenancy component, runs the migration job, and starts the
// service until it answers. The database is on the loopback address, which the loopback test mode of the adapter
// takes with no TLS.
func start(t *testing.T) *service {
	s := migrated(t)
	s.serve()
	return s
}

// migrated makes a database of its own with the roles of the tenancy component and runs the migration job on it.
func migrated(t *testing.T) *service {
	dsn, endpoint, binary := os.Getenv("STEGO_TEST_POSTGRES_DSN"), os.Getenv("STEGO_TEST_SPICEDB"), os.Getenv("STEGO_EXAMPLE_BINARY")
	if dsn == "" || endpoint == "" || binary == "" {
		t.Fatal("the check needs STEGO_TEST_POSTGRES_DSN, STEGO_TEST_SPICEDB and STEGO_EXAMPLE_BINARY: a missing result is not a pass")
	}
	server, err := url.Parse(dsn)
	must(t, err)
	root, err := sql.Open("pgx", dsn)
	must(t, err)
	t.Cleanup(func() { root.Close() })
	t.Cleanup(func() {
		for _, role := range []string{stegoruntime.RuntimeRole, stegoruntime.WorkerRole, stegoruntime.RelayRole, stegoruntime.OwnerRole} {
			_, _ = root.Exec("DROP ROLE IF EXISTS " + quote(role))
		}
	})
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	name := "example_" + hex.EncodeToString(suffix)
	execAll(t, root, "CREATE DATABASE "+name)
	t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)") })
	u := *server
	u.Path = "/" + name
	s := &service{t: t, ctx: context.Background(), log: &bytes.Buffer{}}
	s.admin, err = sql.Open("pgx", u.String())
	must(t, err)
	t.Cleanup(func() { s.admin.Close() })
	execAll(t, s.admin, tenancy.Roles)
	for _, role := range []string{stegoruntime.RuntimeRole, stegoruntime.WorkerRole, stegoruntime.RelayRole, stegoruntime.OwnerRole} {
		execAll(t, s.admin, "ALTER ROLE "+quote(role)+" LOGIN PASSWORD '"+password+"'")
	}
	as := func(role string) string {
		v := u
		v.User = url.UserPassword(role, password)
		q := v.Query()
		q.Set("sslmode", "disable")
		v.RawQuery = q.Encode()
		return v.String()
	}
	spiceKey := os.Getenv("STEGO_TEST_SPICEDB_KEY")
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithPerRPCCredentials(bearer(spiceKey)))
	must(t, err)
	t.Cleanup(func() { conn.Close() })
	s.spice = conn
	env := []string{"STEGO_DATABASE_ALLOW_INSECURE_LOOPBACK=1", "STEGO_SPICEDB_ENDPOINT=" + endpoint,
		"STEGO_SPICEDB_KEY_FILE=" + writeFile(t, "spicedb.key", []byte(spiceKey)), "STEGO_SPICEDB_INSECURE_LOOPBACK=1"}
	ctx, cancel := context.WithTimeout(s.ctx, time.Minute)
	defer cancel()
	migrate := exec.CommandContext(ctx, binary, "migrate")
	migrate.Env = append(append(os.Environ(), env...), "STEGO_MIGRATION_DATABASE_URL="+as(stegoruntime.OwnerRole))
	if output, err := migrate.CombinedOutput(); err != nil {
		t.Fatalf("the migration job failed: %v\n%s", err, output)
	}
	s.processEnv = func(port string) []string {
		return append(append(os.Environ(), env...), "PORT="+port, "DATABASE_URL="+as(stegoruntime.RuntimeRole),
			"STEGO_WORKER_DATABASE_URL="+as(stegoruntime.WorkerRole), "STEGO_RELAY_DATABASE_URL="+as(stegoruntime.RelayRole),
			"STEGO_AUTHN_KEYS_FILE="+writeFile(t, "keys.json", authntest.Document(s.key.JWK())),
			"STEGO_AUTHN_ISSUER="+issuer, "STEGO_AUTHN_AUDIENCE="+audience,
			"STEGO_QUERY_CURSOR_KEY_FILE="+writeFile(t, "cursor.key", s.cursor), "STEGO_SERVICE_VERSION=1.0.0")
	}
	s.key = authntest.NewKey(t, "k1")
	s.cursor = make([]byte, 32)
	_, _ = rand.Read(s.cursor)
	return s
}

// freePort gives a free port of the loopback address.
func freePort(t *testing.T) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer listener.Close()
	return fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
}

// serve starts the service binary on the migrated database until it answers /livez.
func (s *service) serve() {
	t := s.t
	port := freePort(t)
	s.base = "http://127.0.0.1:" + port
	process := exec.Command(os.Getenv("STEGO_EXAMPLE_BINARY"))
	process.Env = s.processEnv(port)
	process.Stdout, process.Stderr = s.log, s.log
	must(t, process.Start())
	exited := make(chan struct{})
	go func() { _ = process.Wait(); close(exited) }()
	t.Cleanup(func() {
		_ = process.Process.Signal(os.Interrupt)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			_ = process.Process.Kill()
		}
		if t.Failed() {
			t.Logf("service log:\n%s", s.log)
		}
	})
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		response, err := http.Get(s.base + "/livez")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-exited:
			t.Fatalf("the service stopped before it answered /livez:\n%s", s.log)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the service does not answer /livez: %v\n%s", err, s.log)
		}
	}
}

// bearer sends the preshared key of the local SpiceDB server on each call.
type bearer string

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}
func (bearer) RequireTransportSecurity() bool { return false }

// caller is one caller of a tenant, with a token that holds its roles.
type caller struct {
	tenant  tenant.Tenant
	subject string
	token   string
}

func (s *service) as(subject, tenantID string, roles ...string) caller {
	t := s.t
	claims := authntest.Claims(issuer, audience, subject, time.Now())
	claims["tenant"] = tenantID
	if len(roles) > 0 {
		claims["roles"] = roles
	}
	claims["exp"] = time.Now().Add(time.Hour).Unix()
	token := authntest.Mint(t, authntest.Header("k1"), claims, s.key)
	keys, err := authn.ParseKeys(authntest.Document(s.key.JWK()))
	must(t, err)
	verifier, err := authn.New(authn.Settings{Issuer: issuer, Audience: audience, TenantClaim: "tenant", ProblemCode: "CHECK", Keys: keys})
	must(t, err)
	defer verifier.Stop()
	id, err := verifier.Verify(s.ctx, token)
	must(t, err)
	c := caller{token: token}
	c.tenant, err = tenant.New(tenant.Claim(id))
	must(t, err)
	principal, err := authz.Person("user", id)
	must(t, err)
	c.subject = principal.Subject().ID
	return c
}

// relate writes the relation of a resource of the caller's tenant in SpiceDB to the caller.
func (s *service) relate(c caller, resourceType, id, relation string) {
	resourceID, err := authz.ResourceID(c.tenant, id)
	must(s.t, err)
	_, err = v1.NewPermissionsServiceClient(s.spice).WriteRelationships(s.ctx, &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{
		Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
		Relationship: &v1.Relationship{Resource: &v1.ObjectReference{ObjectType: resourceType, ObjectId: resourceID}, Relation: relation,
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: c.subject}}}}}})
	must(s.t, err)
}

type reply struct {
	status int
	body   []byte
}

func (r reply) member(t *testing.T, name string) string {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(r.body, &document); err != nil {
		t.Fatalf("the answer %s is not a JSON object: %v", r.body, err)
	}
	value, _ := document[name].(string)
	return value
}

func (r reply) items(t *testing.T) []map[string]any {
	t.Helper()
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(r.body, &page); err != nil {
		t.Fatalf("the answer %s is not a page: %v", r.body, err)
	}
	return page.Items
}

// send sends one request as the caller, with the token in the header that the example declares for jwt-auth.
func (s *service) send(c caller, method, path, body string) reply {
	t := s.t
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, s.base+base+path, reader)
	must(t, err)
	request.Header.Set("X-Internal-Token", "Bearer "+c.token)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	must(t, err)
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	must(t, err)
	return reply{status: response.StatusCode, body: data}
}

func (s *service) count(query string, args ...any) int {
	s.t.Helper()
	var n int
	if err := s.admin.QueryRow(query, args...).Scan(&n); err != nil {
		s.t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestExample(t *testing.T) {
	s := start(t)
	t.Run("ST-SUP-RC20-OC39-T02 The example user-management runs on the runtime data path with its scoped collections, upsert, implicit fields and fills", func(t *testing.T) {
		s.t = t
		owner := s.as("u-owner", "t-acme", "member")
		// The chain of organizations.before_create refuses a reserved name, and creates the organization.
		if r := s.send(owner, http.MethodPost, "/organizations", `{"name":"system org"}`); r.status != http.StatusBadRequest {
			t.Errorf("a reserved organization name answered %d %s, want 400 from org-name-validator", r.status, r.body)
		}
		created := s.send(owner, http.MethodPost, "/organizations", `{"name":"acme"}`)
		if created.status != http.StatusCreated {
			t.Fatalf("the create of an organization answered %d %s, want 201", created.status, created.body)
		}
		org := created.member(t, "id")
		s.relate(owner, "organization", org, "owner")
		// A duplicate unique name is a conflict.
		if r := s.send(owner, http.MethodPost, "/organizations", `{"name":"acme"}`); r.status != http.StatusConflict {
			t.Errorf("a duplicate organization name answered %d %s, want 409", r.status, r.body)
		}
		// The gates of org-users.before_create: a caller with no role is refused; a member may not create an admin.
		noRole := s.as("u-norole", "t-acme")
		s.relate(noRole, "organization", org, "owner")
		if r := s.send(noRole, http.MethodPost, "/organizations/"+org+"/users", `{"email":"a@acme.test","display_name":"A","role":"member"}`); r.status != http.StatusForbidden {
			t.Errorf("a create with no role answered %d %s, want 403 from rbac-policy", r.status, r.body)
		}
		if r := s.send(owner, http.MethodPost, "/organizations/"+org+"/users", `{"email":"b@acme.test","display_name":"B","role":"admin"}`); r.status != http.StatusForbidden {
			t.Errorf("a member that creates an admin answered %d %s, want 403 from admin-creation-policy", r.status, r.body)
		}
		user := s.send(owner, http.MethodPost, "/organizations/"+org+"/users", `{"email":"c@acme.test","display_name":"C","role":"viewer","metadata":{"team":"core"}}`)
		if user.status != http.StatusCreated {
			t.Fatalf("the create of a user answered %d %s, want 201", user.status, user.body)
		}
		userID := user.member(t, "id")
		if n := s.count(`SELECT count(*) FROM users WHERE id = $1::uuid AND org_id = $2::uuid AND tenant_id = 't-acme' AND metadata = '{"team":"core"}'::jsonb`, userID, org); n != 1 {
			t.Errorf("%d user rows of the organization and the tenant with the jsonb metadata, want 1", n)
		}
		s.relate(owner, "member", userID, "owner")
		if r := s.send(owner, http.MethodPatch, "/organizations/"+org+"/users/"+userID, `{"display_name":"Cee"}`); r.status != http.StatusOK || r.member(t, "display_name") != "Cee" {
			t.Errorf("the patch of the user answered %d %s, want 200 with the new name", r.status, r.body)
		}
		if items := s.send(owner, http.MethodGet, "/users?search="+url.QueryEscape("display_name = 'Cee'"), "").items(t); len(items) != 1 {
			t.Errorf("the search of all users gave %d users, want 1", len(items))
		}
		// The upsert of org-settings creates the row of its key and then updates it.
		first := s.send(owner, http.MethodPut, "/organizations/"+org+"/orgsettings", `{"key":"theme","value":{"dark":true},"generation":1}`)
		// The relay writes the relation of the new setting to its organization after the commit; the setting is
		// seen, and found by its key, once SpiceDB holds it.
		for deadline := time.Now().Add(30 * time.Second); len(s.send(owner, http.MethodGet, "/organizations/"+org+"/orgsettings", "").items(t)) == 0; time.Sleep(200 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("the relay did not write the relation of the setting to its organization:\n%s", s.log)
			}
		}
		second := s.send(owner, http.MethodPut, "/organizations/"+org+"/orgsettings", `{"key":"theme","value":{"dark":false},"generation":2}`)
		if first.status != http.StatusCreated || second.status != http.StatusOK || first.member(t, "id") != second.member(t, "id") {
			t.Errorf("the upserts answered %d %s and %d %s, want 201 and 200 for one row", first.status, first.body, second.status, second.body)
		}
		// The activity entries of a user carry the implicit source type, under the path prefix of their collection.
		event := s.send(owner, http.MethodPost, "/user-activity", `{"source_type":"Organization","source_id":"`+userID+`","action":"login"}`)
		if event.status != http.StatusCreated || event.member(t, "source_type") != "User" {
			t.Errorf("the create of a user activity entry answered %d %s, want 201 with the source type User", event.status, event.body)
		}
		s.relate(owner, "activity_entry", event.member(t, "id"), "owner")
		if items := s.send(owner, http.MethodGet, "/org-activity", "").items(t); len(items) != 0 {
			t.Errorf("the list of organization activity holds %d entries, want none", len(items))
		}
		// Another tenant sees none of it.
		stranger := s.as("u-owner", "t-other", "admin")
		if r := s.send(stranger, http.MethodGet, "/organizations/"+org, ""); r.status != http.StatusNotFound {
			t.Errorf("the read from another tenant answered %d %s, want 404", r.status, r.body)
		}
		// Each write left its audit row in the tenant.
		if n := s.count(`SELECT count(*) FROM audit_event WHERE tenant_id = 't-acme' AND outcome = 'ok' AND action IN ('create', 'patch', 'upsert')`); n < 5 {
			t.Errorf("%d ok audit rows of the tenant, want at least 5", n)
		}
	})
}

// refusedStart starts the service binary on the database and waits until it stops: it gives the log of the
// process, and fails the test when the process answers /livez or does not stop with a failure.
func (s *service) refusedStart() string {
	t := s.t
	port := freePort(t)
	ctx, cancel := context.WithTimeout(s.ctx, time.Minute)
	defer cancel()
	process := exec.CommandContext(ctx, os.Getenv("STEGO_EXAMPLE_BINARY"))
	process.Env = s.processEnv(port)
	var log bytes.Buffer
	process.Stdout, process.Stderr = &log, &log
	err := process.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || ctx.Err() != nil {
		t.Fatalf("the service did not stop with a failure: %v\n%s", err, log.String())
	}
	if response, err := http.Get("http://127.0.0.1:" + port + "/livez"); err == nil {
		response.Body.Close()
		t.Fatalf("a process answered /livez on the port of the refused service:\n%s", log.String())
	}
	return log.String()
}

// rows gives the number of rows of each table that the service writes, by table.
func (s *service) rows() map[string]int {
	counts := map[string]int{}
	for _, table := range []string{"public.organizations", "public.users", "public.org_settings", "public.activity_entries",
		"public.audit_event", "public.audit_chain_heads", "public.idempotency_keys", "public.stego_tenants", "stego_outbox.messages"} {
		counts[table] = s.count("SELECT count(*) FROM " + table)
	}
	return counts
}

func TestSchemaManifest(t *testing.T) {
	t.Run("ST-SUP-RC01-OC12-T08 Startup fails closed when the database does not satisfy the schema manifest", func(t *testing.T) {
		// Each change makes the database that the migration job made differ from the schema manifest of the code
		// once, as a change by hand after a release does; the service then restarts on it.
		changes := []struct{ name, statement, fault string }{
			{"a dropped unique index", `DROP INDEX public.organizations_name_key`,
				"table public.organizations: index organizations_name_key is missing"},
			{"an upsert key index on other columns", `DROP INDEX public.org_settings_org_id_key_key;
CREATE UNIQUE INDEX org_settings_org_id_key_key ON public.org_settings (tenant_id, key)`,
				"table public.org_settings: index org_settings_org_id_key_key does not have the columns (tenant_id, org_id, key) of the manifest"},
			{"an upsert key index with a predicate", `DROP INDEX public.org_settings_org_id_key_key;
CREATE UNIQUE INDEX org_settings_org_id_key_key ON public.org_settings (tenant_id, org_id, key) WHERE generation > 0`,
				"table public.org_settings: index org_settings_org_id_key_key does not have the predicate of the manifest"},
			{"a dropped trigger of the audit record", `DROP TRIGGER audit_event_reject_row ON public.audit_event`,
				"table public.audit_event: trigger audit_event_reject_row is missing"},
			{"disabled row-level security", `ALTER TABLE public.users DISABLE ROW LEVEL SECURITY`,
				"table public.users: row-level security is not enabled and forced"},
			{"a changed column type", `ALTER TABLE public.users ALTER COLUMN email TYPE text`,
				"table public.users: column email has the type text, and the manifest needs character varying(255)"},
			{"a dropped check constraint", `ALTER TABLE public.users DROP CONSTRAINT users_role_check`,
				"table public.users: constraint users_role_check is missing"},
		}
		for _, change := range changes {
			t.Run(change.name, func(t *testing.T) {
				s := migrated(t)
				execAll(t, s.admin, change.statement)
				before := s.rows()
				log := s.refusedStart()
				if !strings.Contains(log, "the database does not satisfy the schema manifest") || !strings.Contains(log, change.fault) {
					t.Fatalf("the refused start does not name %q:\n%s", change.fault, log)
				}
				if strings.Contains(log, password) || strings.Contains(log, "postgres://") {
					t.Fatalf("the log of the refused start holds a credential:\n%s", log)
				}
				if after := s.rows(); fmt.Sprint(after) != fmt.Sprint(before) {
					t.Fatalf("the refused start wrote rows: %v, then %v", before, after)
				}
			})
		}
		t.Run("the database of the migration job starts", func(t *testing.T) {
			s := migrated(t)
			s.serve()
		})
	})
}
