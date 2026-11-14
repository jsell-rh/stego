// Package check runs the service generated from ST-DECL-TD03 as ST-DECL-TD05 sets it up: the migration job and the
// service binary against a database of its own on a local PostgreSQL server (STEGO_TEST_POSTGRES_DSN), a fake
// SpiceDB server that answers decisions from the relationships it holds and records each call it gets, and a fake
// token key set. It drives the service over HTTP and proves ST-DECL-UC06-R04-T02 to T04.
package check

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	_ "github.com/jackc/pgx/v5/stdlib"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/jsell-rh/stego/registry/runtime/authn"
	"github.com/jsell-rh/stego/registry/runtime/authn/authntest"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/tenant"

	"example.com/shop/out/internal/stegoruntime"
	"example.com/shop/out/internal/tenancy"
)

const (
	issuer   = "https://issuer.todo.test/realms/r1"
	audience = "todo-svc"
	basePath = "/api/todo/v1"
)

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// quote quotes one SQL name.
func quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// writeFile writes data to a new file of the test and gives its path.
func writeFile(t *testing.T, name string, data []byte) string {
	file := filepath.Join(t.TempDir(), name)
	must(t, os.WriteFile(file, data, 0o600))
	return file
}

// freePort gives a free loopback TCP port.
func freePort(t *testing.T) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer listener.Close()
	return fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
}

// serverTLS writes a server certificate for localhost, signed by a new CA, into the data folder of the server with
// COPY TO PROGRAM, and turns TLS on with ALTER SYSTEM and a reload, as the runtime wiring check does: the service
// runtime connects to its database only with verified TLS. It turns TLS off again when the test ends, and gives the
// PEM file of the CA.
func serverTLS(t *testing.T, root *sql.DB) string {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "relationship test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	must(t, err)
	ca, err := x509.ParseCertificate(caDER)
	must(t, err)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	serverDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: big.NewInt(2),
		Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}, ca, &key.PublicKey, caKey)
	must(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	must(t, err)
	var dataDir string
	must(t, root.QueryRow("SHOW data_directory").Scan(&dataDir))
	write := func(name, text string) {
		lines := strings.Split(strings.TrimSpace(text), "\n")
		quoted := make([]string, len(lines))
		for i, l := range lines {
			quoted[i] = "('" + l + "')"
		}
		// The server reads a key only when no group or other may read it, so the file is written with umask 077.
		_, err := root.Exec("COPY (SELECT line FROM (VALUES " + strings.Join(quoted, ", ") + ") AS v(line)) TO PROGRAM 'rm -f " +
			dataDir + "/" + name + " && umask 077 && cat > " + dataDir + "/" + name + "'")
		must(t, err)
	}
	write("stego_relationship_server.crt", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})))
	write("stego_relationship_server.key", string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})))
	for _, statement := range []string{"ALTER SYSTEM SET ssl_cert_file = 'stego_relationship_server.crt'",
		"ALTER SYSTEM SET ssl_key_file = 'stego_relationship_server.key'", "ALTER SYSTEM SET ssl = on", "SELECT pg_reload_conf()"} {
		_, err := root.Exec(statement)
		must(t, err)
	}
	t.Cleanup(func() {
		for _, statement := range []string{"ALTER SYSTEM RESET ssl", "ALTER SYSTEM RESET ssl_cert_file", "ALTER SYSTEM RESET ssl_key_file", "SELECT pg_reload_conf()"} {
			if _, err := root.Exec(statement); err != nil {
				t.Logf("%s: %v", statement, err)
			}
		}
	})
	time.Sleep(300 * time.Millisecond)
	return writeFile(t, "ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
}

// eventually waits at most 20 seconds for done, and fails the test with what when it does not come.
func eventually(t *testing.T, what string, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); !done(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen in 20 seconds", what)
		}
	}
}

// relationship is one relationship that the fake holds or gets, in the text form resource#relation@subject.
type relationship struct {
	resourceType, resourceID, relation, subjectType, subjectID string
}

func (r relationship) String() string {
	return r.resourceType + ":" + r.resourceID + "#" + r.relation + "@" + r.subjectType + ":" + r.subjectID
}

func relationshipOf(r *v1.Relationship) relationship {
	return relationship{resourceType: r.GetResource().GetObjectType(), resourceID: r.GetResource().GetObjectId(),
		relation: r.GetRelation(), subjectType: r.GetSubject().GetObject().GetObjectType(), subjectID: r.GetSubject().GetObject().GetObjectId()}
}

// call is one write call that the fake got: its touches, its deletes and the filter of a filtered delete, the token
// it gave back, and whether the row of each document that the call names was committed when the call came.
type call struct {
	touches, deletes []relationship
	filter           *v1.RelationshipFilter
	token            string
	committed        map[string]bool
}

// fakeSpiceDB is the fake authorization provider of ST-DECL-TD05: a SpiceDB gRPC server that holds the relationships
// written to it, answers a decision from them (owner gives edit and view, viewer gives view), gives a new token for
// each write, and records each write call. It refuses a call without the preshared key.
type fakeSpiceDB struct {
	v1.UnimplementedPermissionsServiceServer
	v1.UnimplementedSchemaServiceServer
	key string
	// rows tells whether the row of the Todo with the key given is committed and visible to a new transaction.
	rows func(key string) bool

	mu     sync.Mutex
	held   map[relationship]bool
	calls  []call
	schema string
	tokens int
}

func (f *fakeSpiceDB) token() string {
	f.tokens++
	return fmt.Sprintf("fake-token-%d", f.tokens)
}

// committed tells, for each document that the relationships name, whether its row is committed now.
func (f *fakeSpiceDB) committed(relationships []relationship, filter *v1.RelationshipFilter) map[string]bool {
	seen := map[string]bool{}
	ids := []string{}
	for _, r := range relationships {
		if r.resourceType == "document" {
			ids = append(ids, r.resourceID)
		}
	}
	if filter.GetResourceType() == "document" {
		ids = append(ids, filter.GetOptionalResourceId())
	}
	for _, id := range ids {
		_, key, ok := strings.Cut(id, "/")
		if !ok {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(key)
		if err != nil {
			continue
		}
		seen[id] = f.rows(string(raw))
	}
	return seen
}

func (f *fakeSpiceDB) authorize(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	if values := md.Get("authorization"); len(values) != 1 || values[0] != "Bearer "+f.key {
		return status.Error(codes.Unauthenticated, "the call has no valid preshared key")
	}
	return nil
}

func (f *fakeSpiceDB) allowed(resource *v1.ObjectReference, permission string, subject *v1.SubjectReference) bool {
	relations := map[string][]string{"edit": {"owner"}, "view": {"owner", "viewer"}}[permission]
	for _, relation := range relations {
		if f.held[relationship{resourceType: resource.GetObjectType(), resourceID: resource.GetObjectId(), relation: relation,
			subjectType: subject.GetObject().GetObjectType(), subjectID: subject.GetObject().GetObjectId()}] {
			return true
		}
	}
	return false
}

func permissionship(allowed bool) v1.CheckPermissionResponse_Permissionship {
	if allowed {
		return v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION
	}
	return v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION
}

func (f *fakeSpiceDB) CheckPermission(ctx context.Context, request *v1.CheckPermissionRequest) (*v1.CheckPermissionResponse, error) {
	if err := f.authorize(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return &v1.CheckPermissionResponse{CheckedAt: &v1.ZedToken{Token: f.token()},
		Permissionship: permissionship(f.allowed(request.GetResource(), request.GetPermission(), request.GetSubject()))}, nil
}

func (f *fakeSpiceDB) CheckBulkPermissions(ctx context.Context, request *v1.CheckBulkPermissionsRequest) (*v1.CheckBulkPermissionsResponse, error) {
	if err := f.authorize(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	response := &v1.CheckBulkPermissionsResponse{CheckedAt: &v1.ZedToken{Token: f.token()}}
	for _, item := range request.GetItems() {
		response.Pairs = append(response.Pairs, &v1.CheckBulkPermissionsPair{Request: item,
			Response: &v1.CheckBulkPermissionsPair_Item{Item: &v1.CheckBulkPermissionsResponseItem{
				Permissionship: permissionship(f.allowed(item.GetResource(), item.GetPermission(), item.GetSubject()))}}})
	}
	return response, nil
}

func (f *fakeSpiceDB) WriteRelationships(ctx context.Context, request *v1.WriteRelationshipsRequest) (*v1.WriteRelationshipsResponse, error) {
	if err := f.authorize(ctx); err != nil {
		return nil, err
	}
	var c call
	for _, update := range request.GetUpdates() {
		r := relationshipOf(update.GetRelationship())
		switch update.GetOperation() {
		case v1.RelationshipUpdate_OPERATION_TOUCH:
			c.touches = append(c.touches, r)
		case v1.RelationshipUpdate_OPERATION_DELETE:
			c.deletes = append(c.deletes, r)
		default:
			return nil, status.Error(codes.InvalidArgument, "the fake takes TOUCH and DELETE only")
		}
	}
	c.committed = f.committed(append(append([]relationship{}, c.touches...), c.deletes...), nil)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range c.touches {
		f.held[r] = true
	}
	for _, r := range c.deletes {
		delete(f.held, r)
	}
	c.token = f.token()
	f.calls = append(f.calls, c)
	return &v1.WriteRelationshipsResponse{WrittenAt: &v1.ZedToken{Token: c.token}}, nil
}

func (f *fakeSpiceDB) DeleteRelationships(ctx context.Context, request *v1.DeleteRelationshipsRequest) (*v1.DeleteRelationshipsResponse, error) {
	if err := f.authorize(ctx); err != nil {
		return nil, err
	}
	filter := request.GetRelationshipFilter()
	if filter.GetResourceType() == "" || filter.GetOptionalResourceId() == "" || filter.GetOptionalRelation() == "" {
		return nil, status.Error(codes.InvalidArgument, "the fake takes a filter of one resource and one relation only")
	}
	c := call{filter: filter, committed: f.committed(nil, filter)}
	f.mu.Lock()
	defer f.mu.Unlock()
	for r := range f.held {
		if r.resourceType == filter.GetResourceType() && r.resourceID == filter.GetOptionalResourceId() && r.relation == filter.GetOptionalRelation() {
			delete(f.held, r)
			c.deletes = append(c.deletes, r)
		}
	}
	c.token = f.token()
	f.calls = append(f.calls, c)
	return &v1.DeleteRelationshipsResponse{DeletedAt: &v1.ZedToken{Token: c.token}}, nil
}

func (f *fakeSpiceDB) WriteSchema(ctx context.Context, request *v1.WriteSchemaRequest) (*v1.WriteSchemaResponse, error) {
	if err := f.authorize(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.schema = request.GetSchema()
	return &v1.WriteSchemaResponse{WrittenAt: &v1.ZedToken{Token: f.token()}}, nil
}

func (f *fakeSpiceDB) ReadSchema(ctx context.Context, _ *v1.ReadSchemaRequest) (*v1.ReadSchemaResponse, error) {
	if err := f.authorize(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.schema == "" {
		return nil, status.Error(codes.NotFound, "no schema is written")
	}
	return &v1.ReadSchemaResponse{SchemaText: f.schema, ReadAt: &v1.ZedToken{Token: f.token()}}, nil
}

// callsOf gives a copy of the write calls that name the document resourceID.
func (f *fakeSpiceDB) callsOf(resourceID string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var calls []call
	for _, c := range f.calls {
		names := c.filter.GetOptionalResourceId() == resourceID
		for _, r := range append(append([]relationship{}, c.touches...), c.deletes...) {
			names = names || (r.resourceType == "document" && r.resourceID == resourceID)
		}
		if names {
			calls = append(calls, c)
		}
	}
	return calls
}

// holds tells whether the fake holds the relationship.
func (f *fakeSpiceDB) holds(r relationship) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held[r]
}

// heldOf gives the relationships that the fake holds on the document resourceID.
func (f *fakeSpiceDB) heldOf(resourceID string) []relationship {
	f.mu.Lock()
	defer f.mu.Unlock()
	var held []relationship
	for r := range f.held {
		if r.resourceType == "document" && r.resourceID == resourceID {
			held = append(held, r)
		}
	}
	return held
}

// service is the running service of ST-DECL-TD05: its database, the fake provider and its address.
type service struct {
	admin *sql.DB
	fake  *fakeSpiceDB
	key   *authntest.Key
	base  string
}

// start makes a database of its own on the server, runs the role migration of the tenancy component as the database
// administrator does, starts the fake provider, runs the migration job and starts the service until it answers.
func start(t *testing.T) *service {
	dsn, binary := os.Getenv("STEGO_TEST_POSTGRES_DSN"), os.Getenv("STEGO_RELATIONSHIP_BINARY")
	if dsn == "" || binary == "" {
		t.Fatal("the check needs STEGO_TEST_POSTGRES_DSN and STEGO_RELATIONSHIP_BINARY: a missing result is not a pass")
	}
	server, err := url.Parse(dsn)
	must(t, err)
	root, err := sql.Open("pgx", dsn)
	must(t, err)
	t.Cleanup(func() { root.Close() })
	// The roles belong to the server: they are dropped after the database, whose cleanup runs first. A cleanup
	// that fails is logged and does not fail the test.
	t.Cleanup(func() {
		for _, role := range []string{stegoruntime.RuntimeRole, stegoruntime.WorkerRole, stegoruntime.RelayRole, stegoruntime.OwnerRole,
			stegoruntime.PurgeRole, stegoruntime.OutboxRelayRole} {
			if _, err := root.Exec("DROP ROLE IF EXISTS " + quote(role)); err != nil {
				t.Logf("drop the role %s: %v", role, err)
			}
		}
	})
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	name := "relationship_" + hex.EncodeToString(suffix)
	_, err = root.Exec("CREATE DATABASE " + name)
	must(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := root.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("drop the database %s: %v", name, err)
		}
	})
	u := *server
	u.Path = "/" + name
	if _, port, err := net.SplitHostPort(u.Host); err == nil {
		u.Host = net.JoinHostPort("127.0.0.1", port)
	}
	s := &service{}
	s.admin, err = sql.Open("pgx", u.String())
	must(t, err)
	t.Cleanup(func() { s.admin.Close() })
	_, err = s.admin.Exec(tenancy.Roles)
	must(t, err)
	// The roles belong to the whole server, so their login password is new for each run, from crypto/rand: a role
	// that a killed run leaves behind cannot be logged in to with a password that the source holds.
	secret := make([]byte, 16)
	_, err = rand.Read(secret)
	must(t, err)
	password := hex.EncodeToString(secret)
	for _, role := range []string{stegoruntime.RuntimeRole, stegoruntime.WorkerRole, stegoruntime.RelayRole, stegoruntime.OwnerRole} {
		_, err := s.admin.Exec("ALTER ROLE " + quote(role) + " LOGIN PASSWORD '" + password + "'")
		must(t, err)
	}
	ca := serverTLS(t, root)
	as := func(role string) string {
		v := u
		v.User = url.UserPassword(role, password)
		if _, port, err := net.SplitHostPort(v.Host); err == nil {
			v.Host = net.JoinHostPort("localhost", port)
		}
		q := v.Query()
		q.Set("sslmode", "verify-full")
		q.Set("sslrootcert", ca)
		v.RawQuery = q.Encode()
		return v.String()
	}

	spiceKey := hex.EncodeToString(suffix)
	s.fake = &fakeSpiceDB{key: spiceKey, held: map[relationship]bool{}, rows: func(key string) bool {
		var n int
		if err := s.admin.QueryRow(`SELECT count(*) FROM public.todos WHERE id::text = $1`, key).Scan(&n); err != nil {
			return false
		}
		return n == 1
	}}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	grpcServer := grpc.NewServer()
	v1.RegisterPermissionsServiceServer(grpcServer, s.fake)
	v1.RegisterSchemaServiceServer(grpcServer, s.fake)
	// The service asks the health service of SpiceDB before it starts, as it asks the real server.
	healthpb.RegisterHealthServer(grpcServer, health.NewServer())
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	// The fake provider listens on a loopback address without TLS, which the loopback setting admits; the database
	// connections all verify TLS, so the loopback test mode of the adapter is off.
	common := []string{"STEGO_SPICEDB_ENDPOINT=" + listener.Addr().String(), "STEGO_SPICEDB_KEY_FILE=" + writeFile(t, "spicedb.key", []byte(spiceKey)),
		"STEGO_SPICEDB_INSECURE_LOOPBACK=1", "STEGO_DATABASE_ALLOW_INSECURE_LOOPBACK=0"}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	migrate := exec.CommandContext(ctx, binary, "migrate")
	migrate.Env = append(append(os.Environ(), common...), "STEGO_MIGRATION_DATABASE_URL="+as(stegoruntime.OwnerRole))
	if output, err := migrate.CombinedOutput(); err != nil {
		t.Fatalf("the migration job failed: %v\n%s", err, output)
	}

	s.key = authntest.NewKey(t, "k1")
	cursor := make([]byte, 32)
	_, _ = rand.Read(cursor)
	port := freePort(t)
	s.base = "http://127.0.0.1:" + port
	log := &strings.Builder{}
	var logMu sync.Mutex
	writer := writerFunc(func(p []byte) (int, error) { logMu.Lock(); defer logMu.Unlock(); return log.Write(p) })
	process := exec.Command(binary)
	process.Env = append(append(os.Environ(), common...), "PORT="+port, "DATABASE_URL="+as(stegoruntime.RuntimeRole),
		"STEGO_WORKER_DATABASE_URL="+as(stegoruntime.WorkerRole), "STEGO_RELAY_DATABASE_URL="+as(stegoruntime.RelayRole),
		"STEGO_AUTHN_KEYS_FILE="+writeFile(t, "keys.json", authntest.Document(s.key.JWK())),
		"STEGO_QUERY_CURSOR_KEY_FILE="+writeFile(t, "cursor.key", cursor), "STEGO_SERVICE_VERSION=1.0.0")
	process.Stdout, process.Stderr = writer, writer
	must(t, process.Start())
	exited := make(chan struct{})
	go func() { _ = process.Wait(); close(exited) }()
	t.Cleanup(func() {
		_ = process.Process.Signal(os.Interrupt)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			_ = process.Process.Kill()
			<-exited
		}
		if t.Failed() {
			logMu.Lock()
			t.Logf("service log:\n%s", log.String())
			logMu.Unlock()
		}
	})
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		response, err := http.Get(s.base + "/livez")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case <-exited:
			t.Fatal("the service stopped before it answered /livez")
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the service does not answer /livez with 200 in 30 seconds")
		}
	}
	return s
}

type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }

// caller is one user of a tenant, with a token, and the subject ID that the service maps the identity to.
type caller struct {
	tenant  tenant.Tenant
	subject string
	token   string
}

// as mints a token for subject in the tenant tenantID.
func (s *service) as(t *testing.T, subject, tenantID string) caller {
	claims := authntest.Claims(issuer, audience, subject, time.Now())
	claims["tenant"] = tenantID
	claims["exp"] = time.Now().Add(time.Hour).Unix()
	token := authntest.Mint(t, authntest.Header("k1"), claims, s.key)
	keys, err := authn.ParseKeys(authntest.Document(s.key.JWK()))
	must(t, err)
	verifier, err := authn.New(authn.Settings{Issuer: issuer, Audience: audience, TenantClaim: "tenant", ProblemCode: "CHECK", Keys: keys})
	must(t, err)
	defer verifier.Stop()
	id, err := verifier.Verify(context.Background(), token)
	must(t, err)
	c := caller{token: token}
	c.tenant, err = tenant.New(tenant.Claim(id))
	must(t, err)
	principal, err := authz.Person("user", id)
	must(t, err)
	c.subject = principal.Subject().ID
	return c
}

// send sends one request as the caller and gives its status and body.
func (s *service) send(t *testing.T, c caller, method, path, body string) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, s.base+basePath+path, reader)
	must(t, err)
	request.Header.Set("Authorization", "Bearer "+c.token)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	must(t, err)
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	must(t, err)
	return response.StatusCode, data
}

// create creates a Todo as the caller with the owner_id given and gives its ID and its tenant-qualified resource ID.
func (s *service) create(t *testing.T, c caller, ownerID string) (string, string) {
	t.Helper()
	code, body := s.send(t, c, http.MethodPost, "/todos", fmt.Sprintf(`{"title":"todo of %s","owner_id":%q}`, ownerID, ownerID))
	if code != http.StatusCreated {
		t.Fatalf("the create answered %d, want 201: %s", code, body)
	}
	var created struct {
		ID string `json:"id"`
	}
	must(t, json.Unmarshal(body, &created))
	if created.ID == "" {
		t.Fatalf("the create answered no id: %s", body)
	}
	resourceID, err := authz.ResourceID(c.tenant, created.ID)
	must(t, err)
	return created.ID, resourceID
}

// staged is one message of the outbox destination authz, as the command runtime staged it.
type staged struct {
	operation string
	tuple     relationship
	state     string
}

// snapshot reads, in one transaction with one snapshot, whether the Todo id is stored and the messages of the
// outbox destination authz that name its document resourceID.
func (s *service) snapshot(t *testing.T, id, resourceID string) (bool, []staged) {
	t.Helper()
	tx, err := s.admin.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	must(t, err)
	defer func() { _ = tx.Rollback() }()
	var stored int
	must(t, tx.QueryRow(`SELECT count(*) FROM public.todos WHERE id = $1::uuid`, id).Scan(&stored))
	rows, err := tx.Query(`SELECT payload, state FROM stego_outbox.messages WHERE destination = 'authz' ORDER BY sequence`)
	must(t, err)
	defer rows.Close()
	var messages []staged
	for rows.Next() {
		var payload []byte
		var state string
		must(t, rows.Scan(&payload, &state))
		var message struct {
			Operation string `json:"op"`
			Tuple     struct {
				ResourceType string `json:"resource_type"`
				ResourceID   string `json:"resource_id"`
				Relation     string `json:"relation"`
				SubjectType  string `json:"subject_type"`
				SubjectID    string `json:"subject_id"`
			} `json:"tuple"`
		}
		must(t, json.Unmarshal(payload, &message))
		if message.Tuple.ResourceType != "document" || message.Tuple.ResourceID != resourceID {
			continue
		}
		messages = append(messages, staged{operation: message.Operation, state: state, tuple: relationship{
			resourceType: message.Tuple.ResourceType, resourceID: message.Tuple.ResourceID, relation: message.Tuple.Relation,
			subjectType: message.Tuple.SubjectType, subjectID: message.Tuple.SubjectID}})
	}
	must(t, rows.Err())
	return stored == 1, messages
}

// done tells whether every message of the outbox destination authz that names the document is delivered.
func (s *service) done(t *testing.T, id, resourceID string, want int) bool {
	_, messages := s.snapshot(t, id, resourceID)
	if len(messages) != want {
		return false
	}
	for _, m := range messages {
		if m.state != "done" {
			return false
		}
	}
	return true
}

// token reads the authz_token of the stored Todo.
func (s *service) token(t *testing.T, id string) string {
	t.Helper()
	var token string
	must(t, s.admin.QueryRow(`SELECT authz_token FROM public.todos WHERE id = $1::uuid`, id).Scan(&token))
	return token
}

func TestRelationshipOutbox(t *testing.T) {
	s := start(t)
	u1 := s.as(t, "u1", "org-a")
	u2 := s.as(t, "u2", "org-b")

	t.Run("ST-DECL-UC06-R04-T02 A create writes the relationship through the outbox after commit, and the stored instance holds the consistency token", func(t *testing.T) {
		id, resourceID := s.create(t, u1, "u1")
		owner := relationship{resourceType: "document", resourceID: resourceID, relation: "owner", subjectType: "user", subjectID: u1.subject}
		// The create committed the Todo and its one outbox message in one transaction: one snapshot sees both.
		stored, messages := s.snapshot(t, id, resourceID)
		if !stored {
			t.Fatal("the created Todo is not stored")
		}
		if len(messages) != 1 || messages[0].operation != "touch" || messages[0].tuple != owner {
			t.Fatalf("the outbox holds %+v for the Todo, want one touch of %s", messages, owner)
		}
		// The relay delivers it: the fake gets the write once, from the relay, after the commit.
		eventually(t, "the relay delivery of the owner relationship", func() bool { return s.done(t, id, resourceID, 1) })
		calls := s.fake.callsOf(resourceID)
		if len(calls) != 1 || len(calls[0].touches) == 0 || calls[0].touches[0] != owner || len(calls[0].deletes) != 0 {
			t.Fatalf("the fake got %+v for the Todo, want one write that touches %s", calls, owner)
		}
		if !calls[0].committed[resourceID] {
			t.Error("the fake got the write before the Todo was committed")
		}
		if !s.fake.holds(owner) {
			t.Errorf("the fake does not hold %s", owner)
		}
		// The stored Todo holds the token that the provider gave back for the write.
		eventually(t, "the token on the stored Todo", func() bool { return s.token(t, id) == calls[0].token })
	})

	t.Run("ST-DECL-UC06-R04-T03 A delete removes the relationship through the outbox", func(t *testing.T) {
		id, resourceID := s.create(t, u1, "u1")
		owner := relationship{resourceType: "document", resourceID: resourceID, relation: "owner", subjectType: "user", subjectID: u1.subject}
		eventually(t, "the relay write of the owner relationship", func() bool { return s.fake.holds(owner) })
		before := len(s.fake.callsOf(resourceID))
		if code, body := s.send(t, u1, http.MethodDelete, "/todos/"+id, ""); code != http.StatusNoContent {
			t.Fatalf("the delete answered %d, want 204: %s", code, body)
		}
		// The delete committed the removal of the row and the delete of the owner relation in one transaction.
		stored, messages := s.snapshot(t, id, resourceID)
		if stored {
			t.Fatal("the deleted Todo is still stored")
		}
		var deletes int
		for _, m := range messages {
			if m.operation == "delete_relation" && m.tuple.relation == "owner" && m.tuple.subjectID == "" {
				deletes++
			}
		}
		if len(messages) == 0 || messages[0].operation != "touch" || deletes != 1 {
			t.Fatalf("the outbox holds %+v for the Todo, want the touch of the create and one delete of the relation owner", messages)
		}
		eventually(t, "the relay delivery of the delete", func() bool { return s.done(t, id, resourceID, len(messages)) })
		var deleted bool
		for _, c := range s.fake.callsOf(resourceID)[before:] {
			if c.filter.GetOptionalRelation() == "owner" {
				deleted = true
				if visible, read := c.committed[resourceID]; !read || visible {
					t.Error("the fake got the delete before the delete of the Todo was committed")
				}
			}
		}
		if !deleted {
			t.Fatal("the fake got no delete of the relation owner of the Todo from the relay")
		}
		if s.fake.holds(owner) {
			t.Errorf("the fake still holds %s after the delete", owner)
		}
	})

	t.Run("ST-DECL-UC06-R04-T04 A relationship with the subject source caller takes the subject from the caller", func(t *testing.T) {
		id, resourceID := s.create(t, u1, u2.subject)
		var stored string
		must(t, s.admin.QueryRow(`SELECT owner_id FROM public.todos WHERE id = $1::uuid`, id).Scan(&stored))
		if stored != u2.subject {
			t.Fatalf("the stored owner_id is %q, want %q", stored, u2.subject)
		}
		_, messages := s.snapshot(t, id, resourceID)
		if len(messages) != 1 || messages[0].tuple.subjectID != u1.subject {
			t.Fatalf("the outbox holds %+v for the Todo, want one touch with the subject of the caller u1", messages)
		}
		eventually(t, "the relay delivery of the owner relationship", func() bool { return s.done(t, id, resourceID, 1) })
		want := relationship{resourceType: "document", resourceID: resourceID, relation: "owner", subjectType: "user", subjectID: u1.subject}
		held := s.fake.heldOf(resourceID)
		if len(held) != 1 || held[0] != want {
			t.Fatalf("the fake holds %v on the Todo, want only %s, the caller", held, want)
		}
	})
}
