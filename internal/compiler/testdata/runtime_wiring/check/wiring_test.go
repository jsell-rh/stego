// Package check runs the generated service of the runtime wiring test as an operator runs it: the migration job and
// then the service binary, built from the generated main.go, against a local PostgreSQL server
// (STEGO_TEST_POSTGRES_DSN) and a local SpiceDB container (STEGO_TEST_SPICEDB), and sends it one collection call,
// one command and one query over HTTP.
package check

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
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
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"stego.dev/runtime/authn"
	"stego.dev/runtime/authn/authntest"
	"stego.dev/runtime/authz"
	"stego.dev/runtime/tenant"

	"example.com/shop/out/internal/stegoruntime"
	"example.com/shop/out/internal/tenancy"
)

const (
	issuer   = "https://issuer.wiring.test/realms/r1"
	audience = "wiring-shop"
	password = "wiring-check"
)

// service is the running service: its database, its SpiceDB and its address.
type service struct {
	t       *testing.T
	ctx     context.Context
	admin   *sql.DB
	dbName  string
	spice   *grpc.ClientConn
	key     *authntest.Key
	base    string
	process *exec.Cmd
	log     *bytes.Buffer
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// exec runs each statement on db.
func execAll(t *testing.T, db *sql.DB, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%.100s: %v", statement, err)
		}
	}
}

// quote quotes one SQL name.
func quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// prepare makes a database of its own and the roles that the database administrator makes before the first
// migration job: the runtime and owner roles of the tenancy component, the worker and relay roles, the owner's
// membership in the relay role of the outbox runtime, CREATE on public for that relay role, and a login of the
// runtime role.
func prepare(t *testing.T, dsn string) (*sql.DB, string, string) {
	root, err := sql.Open("pgx", dsn)
	must(t, err)
	t.Cleanup(func() { root.Close() })
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	name := "wiring_" + hex.EncodeToString(suffix)
	execAll(t, root, "CREATE DATABASE "+name)
	roles := []string{stegoruntime.RuntimeRole, stegoruntime.OwnerRole, stegoruntime.WorkerRole, stegoruntime.RelayRole}
	t.Cleanup(func() {
		_, _ = root.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
		for _, role := range roles {
			_, _ = root.Exec("DROP ROLE IF EXISTS " + quote(role))
		}
	})
	u, err := url.Parse(dsn)
	must(t, err)
	u.Path = "/" + name
	admin, err := sql.Open("pgx", u.String())
	must(t, err)
	t.Cleanup(func() { admin.Close() })
	execAll(t, admin, tenancy.Roles,
		"CREATE ROLE "+quote(stegoruntime.WorkerRole)+" NOLOGIN NOINHERIT",
		"CREATE ROLE "+quote(stegoruntime.RelayRole)+" NOLOGIN NOINHERIT",
		"DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'stego_outbox_relay') THEN CREATE ROLE stego_outbox_relay NOLOGIN; END IF; END $$",
		"GRANT stego_outbox_relay TO "+quote(stegoruntime.OwnerRole)+" WITH SET TRUE, INHERIT TRUE",
		// The authorization migration gives its state hook function to stego_outbox_relay, which then needs CREATE on
		// the schema of the protected tables (ST-SUP-Q92).
		"GRANT CREATE ON SCHEMA public TO stego_outbox_relay",
		"ALTER ROLE "+quote(stegoruntime.RuntimeRole)+" LOGIN PASSWORD '"+password+"'",
	)
	migration := u.String()
	u.User = url.UserPassword(stegoruntime.RuntimeRole, password)
	return admin, migration, u.String()
}

// freePort gives a free loopback TCP port.
func freePort(t *testing.T) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer listener.Close()
	return fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
}

// write writes data to a new file of the test and gives its path.
func write(t *testing.T, name string, data []byte) string {
	file := filepath.Join(t.TempDir(), name)
	must(t, os.WriteFile(file, data, 0o600))
	return file
}

// start runs the migration job and starts the service binary, and waits until it answers.
func start(t *testing.T) *service {
	dsn, endpoint, binary := os.Getenv("STEGO_TEST_POSTGRES_DSN"), os.Getenv("STEGO_TEST_SPICEDB"), os.Getenv("STEGO_WIRING_BINARY")
	if dsn == "" || endpoint == "" || binary == "" {
		t.Fatal("the check needs STEGO_TEST_POSTGRES_DSN, STEGO_TEST_SPICEDB and STEGO_WIRING_BINARY: a missing result is not a pass")
	}
	spiceKey := os.Getenv("STEGO_TEST_SPICEDB_KEY")
	s := &service{t: t, ctx: context.Background(), log: &bytes.Buffer{}}
	var migration, runtimeDSN string
	s.admin, migration, runtimeDSN = prepare(t, dsn)
	u, err := url.Parse(migration)
	must(t, err)
	s.dbName = strings.TrimPrefix(u.Path, "/")
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithPerRPCCredentials(bearer(spiceKey)))
	must(t, err)
	t.Cleanup(func() { conn.Close() })
	s.spice = conn

	s.key = authntest.NewKey(t, "k1")
	cursor := make([]byte, 32)
	_, _ = rand.Read(cursor)
	settings := []string{
		"STEGO_SPICEDB_ENDPOINT=" + endpoint, "STEGO_SPICEDB_KEY_FILE=" + write(t, "spicedb.key", []byte(spiceKey)),
		"STEGO_SPICEDB_INSECURE_LOOPBACK=1",
	}
	job := exec.Command(binary, "migrate")
	job.Env = append(os.Environ(), append(settings, "STEGO_MIGRATION_DATABASE_URL="+migration)...)
	if output, err := job.CombinedOutput(); err != nil {
		t.Fatalf("the migration job failed: %v\n%s", err, output)
	}
	port := freePort(t)
	s.base = "http://127.0.0.1:" + port
	s.process = exec.Command(binary)
	s.process.Env = append(os.Environ(), append(settings,
		"PORT="+port, "DATABASE_URL="+runtimeDSN, "STEGO_DATABASE_ALLOW_INSECURE_LOOPBACK=1",
		"STEGO_AUTHN_ISSUER="+issuer, "STEGO_AUTHN_AUDIENCE="+audience,
		"STEGO_AUTHN_KEYS_FILE="+write(t, "keys.json", authntest.Document(s.key.JWK())),
		"STEGO_QUERY_CURSOR_KEY_FILE="+write(t, "cursor.key", cursor), "STEGO_SERVICE_VERSION=1.0.0")...)
	s.process.Stdout, s.process.Stderr = s.log, s.log
	must(t, s.process.Start())
	t.Cleanup(func() {
		_ = s.process.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = s.process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = s.process.Process.Kill()
		}
		if t.Failed() {
			t.Logf("service log:\n%s", s.log)
		}
	})
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		response, err := http.Get(s.base + "/livez")
		status := 0
		if err == nil {
			response.Body.Close()
			if status = response.StatusCode; status == http.StatusOK {
				return s
			}
		}
		if time.Now().After(deadline) || s.process.ProcessState != nil {
			t.Fatalf("the service does not answer /livez with 200: %d %v\n%s", status, err, s.log)
		}
	}
}

// bearer sends the preshared key of the local SpiceDB server on each call.
type bearer string

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}
func (bearer) RequireTransportSecurity() bool { return false }

// caller is one caller of a tenant, with a token.
type caller struct {
	tenant  tenant.Tenant
	subject string
	token   string
}

// as mints a token for subject in tenantID.
func (s *service) as(subject, tenantID string) caller {
	t := s.t
	claims := authntest.Claims(issuer, audience, subject, time.Now())
	claims["tenant"] = tenantID
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

// own writes the owner relation of the caller on an order of the caller's tenant in SpiceDB.
func (s *service) own(c caller, id string) {
	resourceID, err := authz.ResourceID(c.tenant, id)
	must(s.t, err)
	_, err = v1.NewPermissionsServiceClient(s.spice).WriteRelationships(s.ctx, &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{
		Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
		Relationship: &v1.Relationship{Resource: &v1.ObjectReference{ObjectType: "order", ObjectId: resourceID}, Relation: "owner",
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: c.subject}}}}}})
	must(s.t, err)
}

// reply is one answer of the service.
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

// send sends one request as the caller, with an Idempotency-Key when key is not empty.
func (s *service) send(c *caller, method, path, key, body string) reply {
	t := s.t
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, s.base+"/api/shop/v1"+path, reader)
	must(t, err)
	if c != nil {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", `"`+key+`"`)
	}
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
	if err := s.admin.QueryRowContext(s.ctx, query, args...).Scan(&n); err != nil {
		s.t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestWiring(t *testing.T) {
	s := start(t)
	owner := s.as("u-"+uuid.NewString()[:8], "t-"+uuid.NewString()[:8])
	var order string

	t.Run("ST-SUP-RC01-OC21-T03 The generated migration job runs as the owner role and the service as the runtime role", func(t *testing.T) {
		s.t = t
		if n := s.count(`SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tableowner <> $1`, stegoruntime.OwnerRole); n != 0 {
			t.Errorf("%d tables of public are not owned by the owner role %s", n, stegoruntime.OwnerRole)
		}
		if n := s.count(`SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tablename IN ('orders', 'notes', 'idempotency_keys')`); n != 3 {
			t.Errorf("the migration job made %d of the tables orders, notes and idempotency_keys", n)
		}
		if n := s.count(`SELECT count(*) FROM pg_class c JOIN pg_roles r ON r.oid = c.relowner WHERE r.rolname = $1`, stegoruntime.RuntimeRole); n != 0 {
			t.Errorf("the runtime role owns %d relations", n)
		}
		if n := s.count(`SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND usename = $2`, s.dbName, stegoruntime.RuntimeRole); n == 0 {
			t.Error("the service holds no connection as the runtime role")
		}
		if n := s.count(`SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND backend_type = 'client backend' AND usename NOT IN ($2, current_user)`, s.dbName, stegoruntime.RuntimeRole); n != 0 {
			t.Errorf("the service holds %d connections as another role than the runtime role", n)
		}
	})

	t.Run("ST-SUP-RC29-OC23-T02 The generated migration job writes the schema to SpiceDB before the service starts", func(t *testing.T) {
		s.t = t
		schema, err := v1.NewSchemaServiceClient(s.spice).ReadSchema(s.ctx, &v1.ReadSchemaRequest{})
		must(t, err)
		for _, want := range []string{"definition order", "definition note", "definition stego_floor"} {
			if !strings.Contains(schema.SchemaText, want) {
				t.Errorf("SpiceDB holds no %q after the migration job:\n%s", want, schema.SchemaText)
			}
		}
	})

	t.Run("ST-SUP-RC20-OC37-T07 A collection create and read reach the generated handlers through the generated main.go", func(t *testing.T) {
		s.t = t
		if got := s.send(nil, http.MethodGet, "/orders", "", ""); got.status != http.StatusUnauthorized {
			t.Errorf("a list with no token answered %d %s, want 401", got.status, got.body)
		}
		created := s.send(&owner, http.MethodPost, "/orders", "k-"+uuid.NewString(), `{"sku":"a1","qty":2}`)
		if created.status != http.StatusCreated {
			t.Fatalf("the create answered %d %s, want 201", created.status, created.body)
		}
		order = created.member(t, "id")
		s.own(owner, order)
		read := s.send(&owner, http.MethodGet, "/orders/"+order, "", "")
		if read.status != http.StatusOK || read.member(t, "sku") != "a1" {
			t.Fatalf("the read answered %d %s, want 200 with sku a1", read.status, read.body)
		}
		if n := s.count(`SELECT count(*) FROM orders WHERE id = $1 AND tenant_id = $2`, order, owner.tenant.ID()); n != 1 {
			t.Errorf("the orders table holds %d rows of the order in the caller's tenant, want 1", n)
		}
	})

	t.Run("ST-SUP-RC32-OC02-T03 A declared command reaches its fill through the generated main.go", func(t *testing.T) {
		s.t = t
		if order == "" {
			t.Fatal("no order: the collection check did not pass")
		}
		got := s.send(&owner, http.MethodPost, "/orders/"+order+"/notes", "k-"+uuid.NewString(), `{"text":"hello"}`)
		if got.status != http.StatusCreated {
			t.Fatalf("the command answered %d %s, want 201", got.status, got.body)
		}
		if n := s.count(`SELECT count(*) FROM notes WHERE order_id = $1 AND text = 'hello' AND tenant_id = $2`, order, owner.tenant.ID()); n != 1 {
			t.Errorf("the notes table holds %d notes of the command, want 1", n)
		}
		other := s.as("u-"+uuid.NewString()[:8], owner.tenant.ID())
		if got := s.send(&other, http.MethodPost, "/orders/"+order+"/notes", "k-"+uuid.NewString(), `{"text":"denied"}`); got.status != http.StatusForbidden {
			t.Errorf("the command of a caller who may not edit the order answered %d %s, want 403", got.status, got.body)
		}
	})

	t.Run("ST-SUP-RC33-OC02-T04 A declared query reaches its fill through the generated main.go", func(t *testing.T) {
		s.t = t
		if order == "" {
			t.Fatal("no order: the collection check did not pass")
		}
		got := s.send(&owner, http.MethodGet, "/orders/"+order+"/summary", "", "")
		if got.status != http.StatusOK || !bytes.Contains(got.body, []byte(`"a1"`)) {
			t.Fatalf("the query answered %d %s, want 200 with the sku a1", got.status, got.body)
		}
		other := s.as("u-"+uuid.NewString()[:8], owner.tenant.ID())
		if got := s.send(&other, http.MethodGet, "/orders/"+order+"/summary", "", ""); got.status != http.StatusNotFound {
			t.Errorf("the query of a caller who may not view the order answered %d %s, want 404", got.status, got.body)
		}
	})
}
