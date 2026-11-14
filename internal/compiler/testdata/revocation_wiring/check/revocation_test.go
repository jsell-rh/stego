// Package check runs the service of the revocation wiring tests (ST-SUP-RC29-OC16 to OC19, ST-SUP-RC20-OC41): the
// migration job, the service binary in the api mode and, when a test lets the relay run, in the worker mode, against a
// database of its own on a local PostgreSQL server (STEGO_TEST_POSTGRES_DSN), a fake SpiceDB server in this process
// that answers decisions from the relationships it holds and records each call, and a fake token key set. It drives
// the service over HTTP.
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
	issuer   = "https://issuer.docs.test/realms/r1"
	audience = "docs-svc"
	basePath = "/api/docs/v1"
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
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "revocation test CA"},
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
	write("stego_revocation_server.crt", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})))
	write("stego_revocation_server.key", string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})))
	for _, statement := range []string{"ALTER SYSTEM SET ssl_cert_file = 'stego_revocation_server.crt'",
		"ALTER SYSTEM SET ssl_key_file = 'stego_revocation_server.key'", "ALTER SYSTEM SET ssl = on", "SELECT pg_reload_conf()"} {
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
	// rows tells whether the row of the Document with the key given is committed and visible to a new transaction.
	rows func(key string) bool

	mu     sync.Mutex
	held   map[relationship]bool
	calls  []call
	schema string
	tokens int
	// decisions counts the decision calls, CheckPermission and CheckBulkPermissions.
	decisions int
	// refuseDeletes makes the fake refuse each delete with INVALID_ARGUMENT, which the relay parks.
	refuseDeletes bool
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
	f.decisions++
	return &v1.CheckPermissionResponse{CheckedAt: &v1.ZedToken{Token: f.token()},
		Permissionship: permissionship(f.allowed(request.GetResource(), request.GetPermission(), request.GetSubject()))}, nil
}

func (f *fakeSpiceDB) CheckBulkPermissions(ctx context.Context, request *v1.CheckBulkPermissionsRequest) (*v1.CheckBulkPermissionsResponse, error) {
	if err := f.authorize(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decisions++
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
	if f.refuseDeletes && len(c.deletes) > 0 {
		return nil, status.Error(codes.InvalidArgument, "the fake refuses this delete")
	}
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
	if f.refuseDeletes {
		return nil, status.Error(codes.InvalidArgument, "the fake refuses this delete")
	}
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
	// binary and env are the service binary and the settings of its processes, for a worker that a test starts.
	binary string
	env    []string
}

// start makes a database of its own on the server, runs the role migration of the tenancy component as the database
// administrator does, starts the fake provider, runs the migration job and starts the service until it answers.
func start(t *testing.T) *service {
	dsn, binary := os.Getenv("STEGO_TEST_POSTGRES_DSN"), os.Getenv("STEGO_REVOCATION_BINARY")
	if dsn == "" || binary == "" {
		t.Fatal("the check needs STEGO_TEST_POSTGRES_DSN and STEGO_REVOCATION_BINARY: a missing result is not a pass")
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
	name := "revocation_" + hex.EncodeToString(suffix)
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
		if err := s.admin.QueryRow(`SELECT count(*) FROM public.documents WHERE id::text = $1`, key).Scan(&n); err != nil {
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
	s.binary = binary
	s.env = append(append(os.Environ(), common...), "DATABASE_URL="+as(stegoruntime.RuntimeRole),
		"STEGO_WORKER_DATABASE_URL="+as(stegoruntime.WorkerRole), "STEGO_RELAY_DATABASE_URL="+as(stegoruntime.RelayRole),
		"STEGO_AUTHN_KEYS_FILE="+writeFile(t, "keys.json", authntest.Document(s.key.JWK())),
		"STEGO_QUERY_CURSOR_KEY_FILE="+writeFile(t, "cursor.key", cursor), "STEGO_SERVICE_VERSION=1.0.0")
	// The api process runs no relay: a test starts a worker process when it lets the relay deliver.
	port, exited := s.run(t, "api")
	s.base = "http://127.0.0.1:" + port
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

// run starts the service binary in the mode given on a free port until the test ends or the stop that it gives is
// called, and gives the port and a channel that closes when the process exits. It logs the output of the process
// when the test fails.
func (s *service) run(t *testing.T, mode string) (string, chan struct{}) {
	port, _, exited := s.process(t, mode)
	return port, exited
}

// process is run, with the stop of the process.
func (s *service) process(t *testing.T, mode string) (string, func(), chan struct{}) {
	port := freePort(t)
	log := &strings.Builder{}
	var logMu sync.Mutex
	writer := writerFunc(func(p []byte) (int, error) { logMu.Lock(); defer logMu.Unlock(); return log.Write(p) })
	process := exec.Command(s.binary)
	process.Env = append(append([]string{}, s.env...), "PORT="+port, "STEGO_MODE="+mode)
	process.Stdout, process.Stderr = writer, writer
	must(t, process.Start())
	exited := make(chan struct{})
	go func() { _ = process.Wait(); close(exited) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = process.Process.Signal(os.Interrupt)
			select {
			case <-exited:
			case <-time.After(10 * time.Second):
				_ = process.Process.Kill()
				<-exited
			}
		})
	}
	t.Cleanup(func() {
		stop()
		if t.Failed() {
			logMu.Lock()
			t.Logf("%s process log:\n%s", mode, log.String())
			logMu.Unlock()
		}
	})
	return port, stop, exited
}

// startRelay starts a worker process, which runs the relay, and gives its stop.
func (s *service) startRelay(t *testing.T) func() {
	_, stop, _ := s.process(t, "worker")
	return stop
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

// reply is one answer of the service: its status, headers and body, and how long it took.
type reply struct {
	code   int
	header http.Header
	body   []byte
	took   time.Duration
}

// state is the state member of the JSON body of a status answer.
func (r reply) state(t *testing.T) string {
	t.Helper()
	var body map[string]any
	must(t, json.Unmarshal(r.body, &body))
	if _, ok := body["token"]; ok {
		t.Fatalf("the status body holds a token: %s", r.body)
	}
	state, _ := body["state"].(string)
	return state
}

// do sends one request to the path of the service, as c or with no credential when c is nil, with the headers given,
// and gives the answer.
func (s *service) do(t *testing.T, c *caller, method, path, body string, headers ...string) reply {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, s.base+path, reader)
	must(t, err)
	if c != nil {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Add(headers[i], headers[i+1])
	}
	start := time.Now()
	response, err := http.DefaultClient.Do(request)
	must(t, err)
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	must(t, err)
	return reply{code: response.StatusCode, header: response.Header, body: data, took: time.Since(start)}
}

// keys maps the name of an idempotency key of a test to the key, a random UUID.
var (
	keysMu sync.Mutex
	keys   = map[string]string{}
)

// keyOf gives the Idempotency-Key header of the name given, the same each time.
func keyOf(name string) string {
	keysMu.Lock()
	defer keysMu.Unlock()
	if key, ok := keys[name]; ok {
		return key
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	h := hex.EncodeToString(b)
	// The header is an RFC 9651 string, so the key is quoted.
	keys[name] = `"` + h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:] + `"`
	return keys[name]
}

// create creates a Document as c with the idempotency key of the name given, and gives its ID and the answer.
func (s *service) create(t *testing.T, c caller, key string) (string, reply) {
	t.Helper()
	r := s.do(t, &c, http.MethodPost, basePath+"/documents", `{"title":"document `+key+`"}`, "Idempotency-Key", keyOf(key))
	if r.code != http.StatusCreated {
		t.Fatalf("the create answered %d, want 201: %s", r.code, r.body)
	}
	var created struct {
		ID string `json:"id"`
	}
	must(t, json.Unmarshal(r.body, &created))
	return created.ID, r
}

// viewer runs the command share-document, or unshare-document when remove is set, as c on the Document id for the
// user, with the idempotency key of the name given, or a new key when the name is empty, and gives the answer.
func (s *service) viewer(t *testing.T, c caller, id string, user caller, remove bool, key string) reply {
	t.Helper()
	path := basePath + "/documents/" + id + "/viewers"
	if remove {
		path = basePath + "/documents/" + id + "/unshare"
	}
	if key == "" {
		key = "new-" + keyOf(path+user.subject+time.Now().String())
	}
	r := s.do(t, &c, http.MethodPost, path, `{"user_id":"`+user.subject+`"}`, "Idempotency-Key", keyOf(key))
	if r.code != http.StatusOK && r.code != http.StatusCreated {
		t.Fatalf("the command on %s answered %d, want 200 or 201: %s", path, r.code, r.body)
	}
	return r
}

// location is the Authz-Revocation-Location of a write that enqueued a tuple; it fails the test when the write does
// not answer Authz-Revocation: pending with a location under /authz/revocations/.
func location(t *testing.T, r reply) string {
	t.Helper()
	path := r.header.Get(authz.RevocationLocationHeader)
	if r.header.Get(authz.RevocationHeader) != "pending" || !strings.HasPrefix(path, authz.RevocationPath) {
		t.Fatalf("the write answered %v, want Authz-Revocation: pending and a location under %s", r.header, authz.RevocationPath)
	}
	return path
}

// complete reads the status path as c with Prefer: wait=5 until it is complete.
func (s *service) complete(t *testing.T, c caller, path string) {
	t.Helper()
	eventually(t, "the revocation "+path+" complete", func() bool {
		r := s.do(t, &c, http.MethodGet, path, "", "Prefer", "wait=5")
		return r.code == http.StatusOK && r.state(t) == "complete"
	})
}

// inTransaction counts the connections of the runtime role to the database of the service that are in a
// transaction and wait for the client.
func (s *service) inTransaction(t *testing.T) int {
	var n int
	must(t, s.admin.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE usename = $1 AND datname = current_database()
		AND state IN ('idle in transaction', 'idle in transaction (aborted)')`, stegoruntime.RuntimeRole).Scan(&n))
	return n
}

// ids gives the IDs of the items of a list answer.
func ids(t *testing.T, r reply) map[string]bool {
	t.Helper()
	var page struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	must(t, json.Unmarshal(r.body, &page))
	out := map[string]bool{}
	for _, item := range page.Items {
		out[item.ID] = true
	}
	return out
}

// waits starts one status read of path for each caller with Prefer: wait=5 and gives the channel of their answers.
func (s *service) waits(t *testing.T, path map[string]string, callers ...caller) chan reply {
	answers := make(chan reply, len(callers))
	for _, c := range callers {
		go func() {
			answers <- s.do(t, &c, http.MethodGet, path[c.tenant.ID()+"/"+c.subject], "", "Prefer", "wait=5")
		}()
	}
	return answers
}

func TestRevocationWiring(t *testing.T) {
	s := start(t)
	alice, bob := s.as(t, "alice", "t1"), s.as(t, "bob", "t1")
	aliceT2, carol := s.as(t, "alice", "t2"), s.as(t, "carol", "t2")

	t.Run("ST-SUP-RC29-OC16-T01 A revoking unshare answers pending, and its replay is byte for byte", func(t *testing.T) {
		stop := s.startRelay(t)
		id, created := s.create(t, alice, "k-oc16-1")
		s.complete(t, alice, location(t, created))
		s.complete(t, alice, location(t, s.viewer(t, alice, id, bob, false, "")))
		stop()
		first := s.viewer(t, alice, id, bob, true, "k-oc16-1u")
		path := location(t, first)
		stop = s.startRelay(t)
		s.complete(t, alice, path)
		stop()
		replay := s.viewer(t, alice, id, bob, true, "k-oc16-1u")
		if replay.code != first.code || string(replay.body) != string(first.body) || replay.header.Get("Idempotent-Replayed") != "true" ||
			replay.header.Get(authz.RevocationHeader) != "pending" || replay.header.Get(authz.RevocationLocationHeader) != path {
			t.Fatalf("the replay of the unshare of %s answered %d %v %s, want the stored answer, still pending at %s", id, replay.code, replay.header, replay.body, path)
		}
	})

	t.Run("ST-SUP-RC29-OC16-T02 An operation that writes no tuple sends no revocation header", func(t *testing.T) {
		id, created := s.create(t, alice, "k-oc16-2")
		stop := s.startRelay(t)
		s.complete(t, alice, location(t, created))
		stop()
		for _, r := range []reply{s.do(t, &alice, http.MethodPut, basePath+"/documents/"+id, `{"title":"renamed"}`),
			s.do(t, &alice, http.MethodGet, basePath+"/documents/"+id, "")} {
			if r.code != http.StatusOK || r.header.Get(authz.RevocationHeader) != "" || r.header.Get(authz.RevocationLocationHeader) != "" {
				t.Fatalf("an update and a read of %s answered %d %v %s, want 200 with no revocation header", id, r.code, r.header, r.body)
			}
		}
	})

	t.Run("ST-SUP-RC29-OC17-T01 The status route answers pending, then complete with no token", func(t *testing.T) {
		stop := s.startRelay(t)
		id, created := s.create(t, alice, "k-oc17-1")
		s.complete(t, alice, location(t, created))
		s.complete(t, alice, location(t, s.viewer(t, alice, id, bob, false, "")))
		stop()
		// The unshare revokes bob's viewer relation.
		path := location(t, s.viewer(t, alice, id, bob, true, ""))
		if r := s.do(t, &alice, http.MethodGet, path, ""); r.code != http.StatusOK || r.state(t) != "pending" {
			t.Fatalf("the status before the delivery is %d %s, want 200 pending", r.code, r.body)
		}
		stop = s.startRelay(t)
		defer stop()
		s.complete(t, alice, path)
	})

	// The writes from here on wait in the outbox until a test starts the relay again.
	paths := map[string]string{}
	_, created := s.create(t, alice, "k-pending-1")
	paths["t1/"+alice.subject] = location(t, created)
	_, created = s.create(t, carol, "k-pending-2")
	paths["t2/"+carol.subject] = location(t, created)
	_, created = s.create(t, aliceT2, "k-pending-3")
	paths["t2/"+aliceT2.subject] = location(t, created)

	t.Run("ST-SUP-RC29-OC18-T01 Only the writer of a revocation reads its status", func(t *testing.T) {
		path := paths["t1/"+alice.subject]
		s.fake.mu.Lock()
		before := s.fake.decisions
		s.fake.mu.Unlock()
		for _, check := range []struct {
			who  *caller
			want int
		}{{&alice, http.StatusOK}, {&bob, http.StatusNotFound}, {&aliceT2, http.StatusNotFound}, {nil, http.StatusUnauthorized}} {
			if r := s.do(t, check.who, http.MethodGet, path, ""); r.code != check.want {
				t.Fatalf("a read of the status of alice in t1 answered %d, want %d: %s", r.code, check.want, r.body)
			}
		}
		s.fake.mu.Lock()
		after := s.fake.decisions
		s.fake.mu.Unlock()
		if after != before {
			t.Fatalf("the status reads made %d decision calls, want none", after-before)
		}
	})

	t.Run("ST-SUP-RC29-OC19-T03 A request over the waiter limit is answered at once", func(t *testing.T) {
		// The waiter limit is 3 and the limit of a tenant 2, and no relay runs.
		held := s.waits(t, paths, alice, alice)
		time.Sleep(300 * time.Millisecond)
		over := s.do(t, &alice, http.MethodGet, paths["t1/"+alice.subject], "", "Prefer", "wait=5")
		if over.took > time.Second || over.header.Get("Preference-Applied") != "" || over.state(t) != "pending" {
			t.Fatalf("a third wait of tenant t1 answered %v %s after %s, want pending at once with no Preference-Applied", over.header, over.body, over.took)
		}
		held2 := s.waits(t, paths, carol)
		time.Sleep(300 * time.Millisecond)
		over = s.do(t, &aliceT2, http.MethodGet, paths["t2/"+aliceT2.subject], "", "Prefer", "wait=5")
		if over.took > time.Second || over.header.Get("Preference-Applied") != "" || over.state(t) != "pending" {
			t.Fatalf("a fourth wait answered %v %s after %s, want pending at once with no Preference-Applied", over.header, over.body, over.took)
		}
		for range 2 {
			if r := <-held; r.state(t) != "pending" || r.header.Get("Preference-Applied") != "wait=3" || r.took < 2500*time.Millisecond {
				t.Fatalf("a held wait answered %v %s after %s, want pending after the cap of 3 s", r.header, r.body, r.took)
			}
		}
		<-held2
	})

	t.Run("ST-SUP-RC29-OC19-T02 A waiting request holds no database connection", func(t *testing.T) {
		answers := s.waits(t, paths, alice, alice, carol)
		time.Sleep(300 * time.Millisecond)
		held := 0
		read := make(chan reply, 1)
		go func() { read <- s.do(t, &alice, http.MethodGet, basePath+"/documents?pageSize=1", "") }()
		for range 40 {
			if s.inTransaction(t) > 0 {
				held++
			}
			time.Sleep(50 * time.Millisecond)
		}
		if r := <-read; r.code != http.StatusOK || r.took > time.Second {
			t.Fatalf("a list during the waits answered %d after %s, want 200 at once", r.code, r.took)
		}
		for range 3 {
			<-answers
		}
		// A wait that held its transaction would show at each sample; a short read of a poll shows at few.
		if held > 10 {
			t.Fatalf("the runtime role had a connection idle in a transaction at %d of 40 samples during three waits, want at most 10", held)
		}
	})

	t.Run("ST-SUP-RC29-OC19-T01 Prefer wait returns at the delivery, and the cap cuts a longer wait", func(t *testing.T) {
		stopped := make(chan func(), 1)
		go func() { time.Sleep(time.Second); stopped <- s.startRelay(t) }()
		r := s.do(t, &alice, http.MethodGet, paths["t1/"+alice.subject], "", "Prefer", "wait=2")
		stop := <-stopped
		if r.state(t) != "complete" || r.header.Get("Preference-Applied") != "wait=2" || r.took < 900*time.Millisecond || r.took > 2*time.Second {
			t.Fatalf("a wait of 2 s with the relay from 1 s answered %v %s after %s, want complete before 2 s with Preference-Applied: wait=2", r.header, r.body, r.took)
		}
		stop()
		_, created := s.create(t, alice, "k-oc19-1")
		r = s.do(t, &alice, http.MethodGet, location(t, created), "", "Prefer", "wait=30")
		if r.state(t) != "pending" || r.header.Get("Preference-Applied") != "wait=3" || r.took < 2500*time.Millisecond || r.took > 6*time.Second {
			t.Fatalf("a wait of 30 s with no relay answered %v %s after %s, want pending after the cap of 3 s with Preference-Applied: wait=3", r.header, r.body, r.took)
		}
	})

	t.Run("ST-SUP-RC20-OC41-T01 A list that names the caller's write waits for it and shows it", func(t *testing.T) {
		first, _ := s.create(t, bob, "k-oc41-1")
		second, created := s.create(t, bob, "k-oc41-2")
		path := location(t, created)
		// A list with no Authz-Revocation-After waits for nothing, even with a pending write and Prefer: wait, and
		// sends no Authz-Revocation header.
		plain := s.do(t, &bob, http.MethodGet, basePath+"/documents", "", "Prefer", "wait=5")
		if listed := ids(t, plain); listed[first] || listed[second] {
			t.Fatalf("a list before the delivery shows %v, want neither create", listed)
		}
		if plain.code != http.StatusOK || plain.took > time.Second || len(plain.header.Values(authz.RevocationHeader)) != 0 {
			t.Fatalf("a list with no %s answered %d %v after %s, want 200 at once with no %s", authz.RevocationAfterHeader, plain.code, plain.header, plain.took, authz.RevocationHeader)
		}
		stopped := make(chan func(), 1)
		go func() { time.Sleep(300 * time.Millisecond); stopped <- s.startRelay(t) }()
		r := s.do(t, &bob, http.MethodGet, basePath+"/documents", "", authz.RevocationAfterHeader, path, "Prefer", "wait=5")
		defer (<-stopped)()
		if listed := ids(t, r); r.code != http.StatusOK || r.header.Get(authz.RevocationHeader) != "complete" || !listed[first] || !listed[second] {
			t.Fatalf("a list with %s answered %d %v %s, want complete and both creates", authz.RevocationAfterHeader, r.code, r.header, r.body)
		}
		// A revocation that is not the caller's is unknown, and a value that is no identity is a bad request.
		if r := s.do(t, &bob, http.MethodGet, basePath+"/documents", "", authz.RevocationAfterHeader, paths["t1/"+alice.subject]); r.code != http.StatusOK || r.header.Get(authz.RevocationHeader) != "unknown" {
			t.Fatalf("a list with alice's revocation answered %d %v, want 200 unknown", r.code, r.header)
		}
		if r := s.do(t, &bob, http.MethodGet, basePath+"/documents", "", authz.RevocationAfterHeader, "not-an-id"); r.code != http.StatusBadRequest {
			t.Fatalf("a list with a bad %s answered %d, want 400", authz.RevocationAfterHeader, r.code)
		}
	})

	t.Run("ST-SUP-RC32-OC13-T01 A fill shares and unshares a document through the typed relation helpers", func(t *testing.T) {
		stop := s.startRelay(t)
		defer stop()
		id, created := s.create(t, alice, "k-oc13-1")
		s.complete(t, alice, location(t, created))
		read := func() int { return s.do(t, &bob, http.MethodGet, basePath+"/documents/"+id, "").code }
		if code := read(); code != http.StatusNotFound {
			t.Fatalf("bob read alice's document before the share: %d, want 404", code)
		}
		s.complete(t, alice, location(t, s.viewer(t, alice, id, bob, false, "")))
		want := relationship{resourceType: "document", relation: "viewer", subjectType: "user", subjectID: bob.subject}
		want.resourceID, _ = authz.ResourceID(alice.tenant, id)
		if !s.fake.holds(want) {
			t.Fatalf("the fake does not hold %s after the share", want)
		}
		if code := read(); code != http.StatusOK {
			t.Fatalf("bob read the shared document: %d, want 200", code)
		}
		s.complete(t, alice, location(t, s.viewer(t, alice, id, bob, true, "")))
		if s.fake.holds(want) {
			t.Fatalf("the fake still holds %s after the unshare", want)
		}
		if code := read(); code != http.StatusNotFound {
			t.Fatalf("bob read the document after the unshare: %d, want 404", code)
		}
		// Bob, a viewer only, may not share: the command decides edit on the row first.
		r := s.do(t, &bob, http.MethodPost, basePath+"/documents/"+id+"/viewers", `{"user_id":"`+bob.subject+`"}`, "Idempotency-Key", keyOf("k-oc13-bob"))
		if r.code != http.StatusNotFound {
			t.Fatalf("bob shared alice's document: %d, want 404", r.code)
		}
	})

	// parked deletes a delivered Document as alice while the fake refuses each delete, and gives the status path of
	// the delete's revocation once it is parked.
	parked := func(t *testing.T, key string) string {
		t.Helper()
		stop := s.startRelay(t)
		defer stop()
		id, created := s.create(t, alice, key)
		s.complete(t, alice, location(t, created))
		s.fake.mu.Lock()
		s.fake.refuseDeletes = true
		s.fake.mu.Unlock()
		defer func() { s.fake.mu.Lock(); s.fake.refuseDeletes = false; s.fake.mu.Unlock() }()
		deleted := s.do(t, &alice, http.MethodDelete, basePath+"/documents/"+id, "")
		if deleted.code != http.StatusNoContent {
			t.Fatalf("the delete answered %d, want 204: %s", deleted.code, deleted.body)
		}
		path := location(t, deleted)
		eventually(t, "the parked delete", func() bool {
			r := s.do(t, &alice, http.MethodGet, path, "", "Prefer", "wait=2")
			return r.code == http.StatusOK && r.state(t) == "parked"
		})
		return path
	}

	t.Run("ST-SUP-RC29-OC17-T03 The status route reports a parked revocation", func(t *testing.T) {
		parked(t, "k-oc17-3")
	})

	t.Run("ST-SUP-RC29-OC17-T04 The status route reports a failed revocation", func(t *testing.T) {
		path := parked(t, "k-oc17-4")
		revocation := strings.TrimPrefix(path, authz.RevocationPath)
		// The parked message is resolved as discarded as the outbox worker's Resolve does: the message moves to
		// discarded and the relay's state function counts it, in one transaction of its tenant.
		tx, err := s.admin.Begin()
		must(t, err)
		defer func() { _ = tx.Rollback() }()
		var message, tenantID string
		must(t, tx.QueryRow(`SELECT message_id::text, tenant_id FROM public.authz_revocation_tuples WHERE revocation_id = $1::uuid AND state = 'parked'`, revocation).Scan(&message, &tenantID))
		_, err = tx.Exec(`SELECT pg_catalog.set_config('app.tenant', $1, true)`, tenantID)
		must(t, err)
		_, err = tx.Exec(`UPDATE stego_outbox.messages SET state = 'discarded', parked_from = NULL, resolution = 'discarded', resolved_by = 'check', resolved_at = now() WHERE id = $1::uuid`, message)
		must(t, err)
		var counted bool
		must(t, tx.QueryRow(`SELECT public.authz_revocation_state($1::uuid, $2::uuid, 'discarded')`, message, revocation).Scan(&counted))
		must(t, tx.Commit())
		if !counted {
			t.Fatal("the state function did not count the discard")
		}
		if r := s.do(t, &alice, http.MethodGet, path, ""); r.code != http.StatusOK || r.state(t) != "failed" {
			t.Fatalf("the status after the discard is %d %s, want 200 failed", r.code, r.body)
		}
	})
}
