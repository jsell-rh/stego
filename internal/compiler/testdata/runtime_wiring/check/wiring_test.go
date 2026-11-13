// Package check runs the generated service of the runtime wiring test as an operator runs it: the migration job and
// then the service binary, built from the generated main.go, against a local PostgreSQL server
// (STEGO_TEST_POSTGRES_DSN) with verified TLS and a local SpiceDB container (STEGO_TEST_SPICEDB), and sends it one
// collection call, one command and one query over HTTP. The service has the event-publisher mixin: its worker sends
// the change events of the collections to a local Kafka broker of kafkatest (ST-SUP-RC20-OC40, ST-SUP-RC18-OC31).
package check

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
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
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/jsell-rh/stego/registry/runtime/authn"
	"github.com/jsell-rh/stego/registry/runtime/authn/authntest"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/kafkatest"
	"github.com/jsell-rh/stego/registry/runtime/outbox/kafkaoutbox"
	"github.com/jsell-rh/stego/registry/runtime/tenant"

	"example.com/shop/out/internal/stegoruntime"
	"example.com/shop/out/internal/storage"
	"example.com/shop/out/internal/tenancy"
)

const (
	issuer   = "https://issuer.wiring.test/realms/r1"
	audience = "wiring-shop"
	password = "wiring-check"
)

// changeTopic is the declared topic of the change events of the service, so the events of one entity go to one
// partition of one topic in their order.
const changeTopic = "wiring-shop.changes"

// TestMain removes the Kafka container of kafkatest when the tests end.
func TestMain(m *testing.M) { os.Exit(kafkatest.Wrap(m.Run)) }

// migratorRole is the login of the migration job: the owner role, as the outbox migration of the runtime library
// needs (ST-SUP-RP01-LC19), and never the runtime role.
var migratorRole = stegoruntime.OwnerRole

// service is the running service: its database, its SpiceDB and its address, and the results of the runs of the
// binary before the checks.
type service struct {
	t      *testing.T
	ctx    context.Context
	admin  *sql.DB
	dbName string
	spice  *grpc.ClientConn
	key    *authntest.Key
	base   string
	log    *bytes.Buffer

	weakMigration, weakStart, foreignMigration, legacyMigration, secondMigration, nextRelease, noKafka runResult
	broker                                                                                             *kafkatest.Broker
	foreignDB, legacyDB                                                                                *sql.DB
	schemaBeforeNext                                                                                   string
}

// runResult is the outcome of one run of the binary.
type runResult struct {
	err    error
	output string
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// execAll runs each statement on db.
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

// serverTLS writes a server certificate for localhost, signed by a new CA, into the data folder of the server with
// COPY TO PROGRAM, and turns TLS on with ALTER SYSTEM and a reload, as the runtime's tenant tests do. It turns TLS off
// again when the test ends, and gives the PEM file of the CA.
func serverTLS(t *testing.T, root *sql.DB) string {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "wiring test CA"},
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
		execAll(t, root, "COPY (SELECT line FROM (VALUES "+strings.Join(quoted, ", ")+") AS v(line)) TO PROGRAM 'rm -f "+
			dataDir+"/"+name+" && umask 077 && cat > "+dataDir+"/"+name+"'")
	}
	write("stego_wiring_server.crt", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})))
	write("stego_wiring_server.key", string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})))
	execAll(t, root, "ALTER SYSTEM SET ssl_cert_file = 'stego_wiring_server.crt'", "ALTER SYSTEM SET ssl_key_file = 'stego_wiring_server.key'",
		"ALTER SYSTEM SET ssl = on", "SELECT pg_reload_conf()")
	t.Cleanup(func() {
		for _, statement := range []string{"ALTER SYSTEM RESET ssl", "ALTER SYSTEM RESET ssl_cert_file", "ALTER SYSTEM RESET ssl_key_file", "SELECT pg_reload_conf()"} {
			_, _ = root.Exec(statement)
		}
	})
	time.Sleep(300 * time.Millisecond)
	return writeFile(t, "ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
}

// newDatabase makes a database of its own on the server, and gives its admin pool and its URL as the admin.
func newDatabase(t *testing.T, root *sql.DB, server *url.URL) (*sql.DB, *url.URL) {
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	name := "wiring_" + hex.EncodeToString(suffix)
	execAll(t, root, "CREATE DATABASE "+name)
	t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)") })
	u := *server
	u.Path = "/" + name
	admin, err := sql.Open("pgx", u.String())
	must(t, err)
	t.Cleanup(func() { admin.Close() })
	return admin, &u
}

// prepare runs what the database administrator runs before the first migration job: the role migration of the
// tenancy component, which makes the owner, runtime, worker and relay roles and the purge and outbox relay roles of
// the service, with the owner's SET membership in the last two (ST-SUP-RC27-OC04), and a login of each role, which a
// deployment gives. No other role or grant is made by hand.
func prepare(t *testing.T, admin *sql.DB) {
	execAll(t, admin, tenancy.Roles)
	for _, role := range []string{stegoruntime.RuntimeRole, stegoruntime.WorkerRole, stegoruntime.RelayRole, migratorRole} {
		execAll(t, admin, "ALTER ROLE "+quote(role)+" LOGIN PASSWORD '"+password+"'")
	}
}

// grantSchema prepares another database of the server as prepare does: the role migration runs on each database,
// since the grants on the database and its schema public are the database's, and it gives each role its login again.
func grantSchema(t *testing.T, admin *sql.DB) { prepare(t, admin) }

// withTLS gives the URL of role on u, on the host localhost, with sslmode mode; verify-full also names the CA file.
func withTLS(u *url.URL, role, mode, caFile string) string {
	v := *u
	v.User = url.UserPassword(role, password)
	if _, port, err := net.SplitHostPort(v.Host); err == nil {
		v.Host = net.JoinHostPort("localhost", port)
	}
	q := v.Query()
	q.Set("sslmode", mode)
	q.Del("sslrootcert")
	if mode == "verify-full" {
		q.Set("sslrootcert", caFile)
	}
	v.RawQuery = q.Encode()
	return v.String()
}

// freePort gives a free loopback TCP port.
func freePort(t *testing.T) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer listener.Close()
	return fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
}

// writeFile writes data to a new file of the test and gives its path.
func writeFile(t *testing.T, name string, data []byte) string {
	file := filepath.Join(t.TempDir(), name)
	must(t, os.WriteFile(file, data, 0o600))
	return file
}

// noTestMode turns the loopback test mode of the adapter off, which the CI checks turn on for their fixtures: every
// run of the binary needs verified TLS to the database.
const noTestMode = "STEGO_DATABASE_ALLOW_INSECURE_LOOPBACK=0"

// run runs binary with args and the environment env, and waits at most a minute.
func run(binary string, env []string, args ...string) runResult {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = append(append(os.Environ(), env...), noTestMode)
	output, err := cmd.CombinedOutput()
	return runResult{err: err, output: string(output)}
}

// start prepares the database and runs the binary: the migration job with TLS that does not verify the server, on
// a database that holds a foreign table, and twice on its own database; the service with TLS that does not verify
// the server, and then the service with verified TLS, until it answers. It then runs the migration job of the next
// release, whose declaration adds an entity.
func start(t *testing.T) *service {
	dsn, endpoint := os.Getenv("STEGO_TEST_POSTGRES_DSN"), os.Getenv("STEGO_TEST_SPICEDB")
	binary, next := os.Getenv("STEGO_WIRING_BINARY"), os.Getenv("STEGO_WIRING_NEXT_BINARY")
	if dsn == "" || endpoint == "" || binary == "" || next == "" {
		t.Fatal("the check needs STEGO_TEST_POSTGRES_DSN, STEGO_TEST_SPICEDB, STEGO_WIRING_BINARY and STEGO_WIRING_NEXT_BINARY: a missing result is not a pass")
	}
	server, err := url.Parse(dsn)
	must(t, err)
	root, err := sql.Open("pgx", dsn)
	must(t, err)
	t.Cleanup(func() { root.Close() })
	s := &service{t: t, ctx: context.Background(), log: &bytes.Buffer{}}
	// The roles belong to the server: they are dropped after the databases, whose cleanups run first.
	t.Cleanup(func() {
		for _, role := range []string{stegoruntime.RuntimeRole, stegoruntime.WorkerRole, stegoruntime.RelayRole, stegoruntime.OwnerRole} {
			if _, err := root.Exec("DROP ROLE IF EXISTS " + quote(role)); err != nil {
				t.Errorf("drop the role %s: %v", role, err)
			}
		}
	})
	var u *url.URL
	s.admin, u = newDatabase(t, root, server)
	s.dbName = strings.TrimPrefix(u.Path, "/")
	prepare(t, s.admin)
	ca := serverTLS(t, root)

	spiceKey := os.Getenv("STEGO_TEST_SPICEDB_KEY")
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithPerRPCCredentials(bearer(spiceKey)))
	must(t, err)
	t.Cleanup(func() { conn.Close() })
	s.spice = conn
	spice := []string{"STEGO_SPICEDB_ENDPOINT=" + endpoint, "STEGO_SPICEDB_KEY_FILE=" + writeFile(t, "spicedb.key", []byte(spiceKey)),
		"STEGO_SPICEDB_INSECURE_LOOPBACK=1"}
	migration := func(mode string, target *url.URL) []string {
		return append(append([]string{}, spice...), "STEGO_MIGRATION_DATABASE_URL="+withTLS(target, migratorRole, mode, ca))
	}

	// A migration job with TLS that does not verify the server writes nothing.
	s.weakMigration = run(binary, migration("prefer", u), "migrate")
	// A migration job on a database that holds a table of the declaration and no ledger writes nothing.
	var foreignURL *url.URL
	s.foreignDB, foreignURL = newDatabase(t, root, server)
	grantSchema(t, s.foreignDB)
	execAll(t, s.foreignDB, "CREATE TABLE orders (id uuid PRIMARY KEY, sku text)")
	s.foreignMigration = run(binary, migration("verify-full", foreignURL), "migrate")
	// A migration job on a database whose ledger holds the digest of the name alone for 001_initial, as a release
	// on the GORM store wrote it, writes nothing: the ledger never takes the GORM tables over as the SQL baseline.
	var legacyURL *url.URL
	s.legacyDB, legacyURL = newDatabase(t, root, server)
	grantSchema(t, s.legacyDB)
	execAll(t, s.legacyDB, "CREATE TABLE orders (id uuid PRIMARY KEY, sku text)", "CREATE SCHEMA stego_schema",
		"CREATE TABLE stego_schema.migrations (version text PRIMARY KEY, digest text NOT NULL)",
		"INSERT INTO stego_schema.migrations VALUES ('001_initial', '"+storage.Digest("001_initial")+"')",
		// The release on the GORM store made them as the owner role, as the migration job of the release does.
		"ALTER SCHEMA stego_schema OWNER TO "+quote(stegoruntime.OwnerRole), "ALTER TABLE stego_schema.migrations OWNER TO "+quote(stegoruntime.OwnerRole),
		"ALTER TABLE orders OWNER TO "+quote(stegoruntime.OwnerRole))
	s.legacyMigration = run(binary, migration("verify-full", legacyURL), "migrate")

	if r := run(binary, migration("verify-full", u), "migrate"); r.err != nil {
		t.Fatalf("the migration job failed: %v\n%s", r.err, r.output)
	}
	s.secondMigration = run(binary, migration("verify-full", u), "migrate")

	s.key = authntest.NewKey(t, "k1")
	cursor := make([]byte, 32)
	_, _ = rand.Read(cursor)
	keys := writeFile(t, "keys.json", authntest.Document(s.key.JWK()))
	cursorFile := writeFile(t, "cursor.key", cursor)
	port := freePort(t)
	s.base = "http://127.0.0.1:" + port
	s.broker = kafkatest.Start(t)
	s.broker.CreateTopic(t, changeTopic, 3)
	kafka := []string{kafkaoutbox.BrokersSetting + "=" + s.broker.Plain.Addr(), kafkaoutbox.InsecureLoopbackSetting + "=1",
		kafkaoutbox.TopicSetting + "=" + changeTopic}
	serviceEnv := func(mode string) []string {
		return append(append(append([]string{}, spice...), kafka...),
			"PORT="+port, "DATABASE_URL="+withTLS(u, stegoruntime.RuntimeRole, mode, ca), "STEGO_AUTHN_KEYS_FILE="+keys,
			"STEGO_WORKER_DATABASE_URL="+withTLS(u, stegoruntime.WorkerRole, mode, ca),
			"STEGO_RELAY_DATABASE_URL="+withTLS(u, stegoruntime.RelayRole, mode, ca),
			"STEGO_QUERY_CURSOR_KEY_FILE="+cursorFile, "STEGO_SERVICE_VERSION=1.0.0")
	}
	// A service with TLS that does not verify the server does not start.
	s.weakStart = run(binary, serviceEnv("disable"))
	// The worker mode of a service with no Kafka setting does not start: it names the setting (ST-SUP-RC18-OC31).
	var unset []string
	for _, v := range serviceEnv("verify-full") {
		if !strings.HasPrefix(v, "STEGO_KAFKA_") {
			unset = append(unset, v)
		}
	}
	s.noKafka = run(binary, append(unset, kafkaoutbox.BrokersSetting+"=", kafkaoutbox.TopicSetting+"=", kafkaoutbox.InsecureLoopbackSetting+"="), "--mode=worker")

	process := exec.Command(binary)
	process.Env = append(append(os.Environ(), serviceEnv("verify-full")...), noTestMode)
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
		status := 0
		if err == nil {
			response.Body.Close()
			if status = response.StatusCode; status == http.StatusOK {
				break
			}
		}
		select {
		case <-exited:
			t.Fatalf("the service stopped before it answered /livez:\n%s", s.log)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the service does not answer /livez with 200: %d %v\n%s", status, err, s.log)
		}
	}
	schema, err := v1.NewSchemaServiceClient(s.spice).ReadSchema(s.ctx, &v1.ReadSchemaRequest{})
	must(t, err)
	s.schemaBeforeNext = schema.SchemaText
	// The next release adds an entity, whose table is not in the baseline of the ledger.
	s.nextRelease = run(next, migration("verify-full", u), "migrate")
	return s
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

// as mints a token for subject in tenantID, for the declared audience or the audience aud.
func (s *service) as(subject, tenantID string, aud ...string) caller {
	t := s.t
	claims := authntest.Claims(issuer, audience, subject, time.Now())
	if len(aud) > 0 {
		claims["aud"] = aud[0]
	}
	claims["tenant"] = tenantID
	claims["exp"] = time.Now().Add(time.Hour).Unix()
	token := authntest.Mint(t, authntest.Header("k1"), claims, s.key)
	keys, err := authn.ParseKeys(authntest.Document(s.key.JWK()))
	must(t, err)
	verifier, err := authn.New(authn.Settings{Issuer: issuer, Audience: claims["aud"].(string), TenantClaim: "tenant", ProblemCode: "CHECK", Keys: keys})
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

func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestWiring(t *testing.T) {
	s := start(t)
	owner := s.as("u-"+uuid.NewString()[:8], "t-"+uuid.NewString()[:8])
	var order string

	t.Run("ST-SUP-RC01-OC21-T03 The generated migration job runs as the owner role and the service as the runtime role", func(t *testing.T) {
		s.t = t
		if n := count(t, s.admin, `SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tableowner <> $1`, stegoruntime.OwnerRole); n != 0 {
			t.Errorf("%d tables of public are not owned by the owner role %s", n, stegoruntime.OwnerRole)
		}
		if n := count(t, s.admin, `SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tablename IN ('orders', 'notes', 'idempotency_keys')`); n != 3 {
			t.Errorf("the migration job made %d of the tables orders, notes and idempotency_keys", n)
		}
		if n := count(t, s.admin, `SELECT count(*) FROM pg_class c JOIN pg_roles r ON r.oid = c.relowner WHERE r.rolname = $1`, stegoruntime.RuntimeRole); n != 0 {
			t.Errorf("the runtime role owns %d relations", n)
		}
		if n := count(t, s.admin, `SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND usename = $2`, s.dbName, stegoruntime.RuntimeRole); n == 0 {
			t.Error("the service holds no connection as the runtime role")
		}
		// The worker mode connects as the worker role and the relay as the relay role (ST-SUP-RP14-LC04); the
		// request path holds no other role.
		if n := count(t, s.admin, `SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND backend_type = 'client backend' AND usename NOT IN ($2, $3, $4, current_user)`, s.dbName, stegoruntime.RuntimeRole, stegoruntime.WorkerRole, stegoruntime.RelayRole); n != 0 {
			t.Errorf("the service holds %d connections as another role than the runtime, worker and relay roles", n)
		}
		if n := count(t, s.admin, `SELECT count(*) FROM pg_stat_activity WHERE usename = $1`, migratorRole); n != 0 {
			t.Errorf("%d connections use the login of the migration job after it ended", n)
		}
	})

	t.Run("ST-SUP-RC01-OC41-T04 The runtime pool is opened through tenant.Open with the adapter's verified TLS configuration", func(t *testing.T) {
		s.t = t
		if n := count(t, s.admin, `SELECT count(*) FROM pg_stat_activity a JOIN pg_stat_ssl l ON l.pid = a.pid WHERE a.datname = $1 AND a.usename IN ($2, $3, $4) AND NOT l.ssl`, s.dbName, stegoruntime.RuntimeRole, stegoruntime.WorkerRole, stegoruntime.RelayRole); n != 0 {
			t.Errorf("%d connections of the service do not use TLS", n)
		}
		if s.weakStart.err == nil {
			t.Errorf("the service started with sslmode disable:\n%s", s.weakStart.output)
		}
		if s.weakMigration.err == nil || !strings.Contains(s.weakMigration.output, `"stage":"migrate.settings"`) {
			t.Errorf("the migration job with sslmode prefer gave %v, want a refusal of its settings:\n%s", s.weakMigration.err, s.weakMigration.output)
		}
	})

	t.Run("ST-SUP-RC01-OC41-T07 The generated migration job runs again on its own database and refuses a database with tables it did not make", func(t *testing.T) {
		s.t = t
		if s.secondMigration.err != nil {
			t.Errorf("the second run of the migration job failed: %v\n%s", s.secondMigration.err, s.secondMigration.output)
		}
		if s.foreignMigration.err == nil || !strings.Contains(s.foreignMigration.output, `"stage":"migrate.ledger_check"`) {
			t.Errorf("the migration job on a database with a table of the declaration and no ledger gave %v, want the refusal of the ledger check:\n%s", s.foreignMigration.err, s.foreignMigration.output)
		}
		if n := count(t, s.foreignDB, `SELECT count(*) FROM pg_namespace WHERE nspname IN ('stego_schema', 'stego_outbox')`); n != 0 {
			t.Errorf("the refused migration job made %d schemas", n)
		}
		if s.legacyMigration.err == nil || !strings.Contains(s.legacyMigration.output, `"stage":"migrate.ledger_check"`) {
			t.Errorf("the migration job on a database whose ledger holds the name digest of a GORM release gave %v, want the refusal of the ledger check:\n%s", s.legacyMigration.err, s.legacyMigration.output)
		}
		if n := count(t, s.legacyDB, `SELECT count(*) FROM stego_schema.migrations WHERE digest = $1`, storage.Digest("001_initial")); n != 1 {
			t.Error("the refused migration job changed the ledger row of the GORM release")
		}
		if n := count(t, s.legacyDB, `SELECT count(*) FROM pg_namespace WHERE nspname = 'stego_outbox'`); n != 0 {
			t.Error("the refused migration job made the outbox schema")
		}
		if s.nextRelease.err == nil || !strings.Contains(s.nextRelease.output, `"stage":"migrate.ledger_check"`) {
			t.Errorf("the migration job of a release that adds an entity with no schema step gave %v, want the refusal of the ledger check:\n%s", s.nextRelease.err, s.nextRelease.output)
		}
		if n := count(t, s.admin, `SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tablename = 'tags'`); n != 0 {
			t.Error("the refused migration job of the next release made the table tags")
		}
		schema, err := v1.NewSchemaServiceClient(s.spice).ReadSchema(s.ctx, &v1.ReadSchemaRequest{})
		must(t, err)
		if schema.SchemaText != s.schemaBeforeNext {
			t.Error("the refused migration job of the next release wrote the authorization schema")
		}
	})

	t.Run("ST-SUP-RC29-OC23-T02 The generated migration job writes the schema to SpiceDB before the service starts", func(t *testing.T) {
		s.t = t
		for _, want := range []string{"definition order", "definition note", "definition stego_floor"} {
			if !strings.Contains(s.schemaBeforeNext, want) {
				t.Errorf("SpiceDB holds no %q after the migration job:\n%s", want, s.schemaBeforeNext)
			}
		}
	})

	t.Run("ST-SUP-RC20-OC37-T07 A collection create and read reach the generated handlers through the generated main.go", func(t *testing.T) {
		s.t = t
		if got := s.send(nil, http.MethodGet, "/orders", "", ""); got.status != http.StatusUnauthorized {
			t.Errorf("a list with no token answered %d %s, want 401", got.status, got.body)
		}
		stranger := s.as("u-"+uuid.NewString()[:8], owner.tenant.ID(), "another-service")
		if got := s.send(&stranger, http.MethodGet, "/orders", "", ""); got.status != http.StatusUnauthorized {
			t.Errorf("a list with a token for another audience than the declared one answered %d %s, want 401", got.status, got.body)
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
		if n := count(t, s.admin, `SELECT count(*) FROM orders WHERE id = $1 AND tenant_id = $2`, order, owner.tenant.ID()); n != 1 {
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
		if n := count(t, s.admin, `SELECT count(*) FROM notes WHERE order_id = $1 AND text = 'hello' AND tenant_id = $2`, order, owner.tenant.ID()); n != 1 {
			t.Errorf("the notes table holds %d notes of the command, want 1", n)
		}
		other := s.as("u-"+uuid.NewString()[:8], owner.tenant.ID())
		if got := s.send(&other, http.MethodPost, "/orders/"+order+"/notes", "k-"+uuid.NewString(), `{"text":"denied"}`); got.status != http.StatusNotFound {
			t.Errorf("the command of a caller who may not edit the order answered %d %s, want 404", got.status, got.body)
		}
	})

	t.Run("ST-SUP-RP14-LC04-T02 The generated service runs its workers through the service runtime", func(t *testing.T) {
		s.t = t
		if order == "" {
			t.Fatal("no order: the collection check did not pass")
		}
		eventually := func(what string, ok func() bool) {
			t.Helper()
			for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
				if ok() {
					return
				}
			}
			t.Fatalf("after 60 s: %s\n%s", what, s.log)
		}
		// The after slot of the create is an effect of its command: the outbox worker of the worker mode runs it, as
		// the worker role, and the message is done (ST-SUP-RC20-OC34, ST-SUP-RC18-OC25).
		eventually("the after slot of the create is delivered", func() bool {
			return count(t, s.admin, `SELECT count(*) FROM stego_outbox.messages WHERE tenant_id = $1 AND destination = 'rest-api.after-slots' AND state = 'done'`, owner.tenant.ID()) == 1
		})
		if n := count(t, s.admin, `SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND usename = $2`, s.dbName, stegoruntime.WorkerRole); n == 0 {
			t.Error("the service holds no connection as the worker role")
		}
		// The command add-note writes a Note that refers to the order: the relay, as the relay role, writes the
		// relation note#order of the reference to SpiceDB (ST-SUP-RP10-LC05, ST-SUP-RP06-LC06), so the owner of the
		// order may view the note.
		created := s.send(&owner, http.MethodPost, "/orders/"+order+"/notes", "k-"+uuid.NewString(), `{"text":"relayed"}`)
		if created.status != http.StatusCreated {
			t.Fatalf("the command answered %d %s, want 201", created.status, created.body)
		}
		var note string
		must(t, s.admin.QueryRow(`SELECT id::text FROM notes WHERE text = 'relayed' AND tenant_id = $1`, owner.tenant.ID()).Scan(&note))
		noteID, err := authz.ResourceID(owner.tenant, note)
		must(t, err)
		eventually("the relay writes the relation of the note to its order", func() bool {
			answer, err := v1.NewPermissionsServiceClient(s.spice).CheckPermission(s.ctx, &v1.CheckPermissionRequest{
				Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
				Resource:    &v1.ObjectReference{ObjectType: "note", ObjectId: noteID}, Permission: "view",
				Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: owner.subject}}})
			return err == nil && answer.GetPermissionship() == v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION
		})
		// The anchor job of the controller mode anchors the chain of the tenant under the controller lease, at the
		// declared anchor period of 2 s: the boundary is stored and the anchor is in the outbox for the audit sink.
		execAll(t, s.admin, "INSERT INTO stego_tenants (tenant_id) VALUES ('"+owner.tenant.ID()+"') ON CONFLICT DO NOTHING")
		eventually("the anchor job anchors the chain of the tenant", func() bool {
			return count(t, s.admin, `SELECT count(*) FROM audit_anchor_boundaries WHERE tenant_id = $1`, owner.tenant.ID()) == 1 &&
				count(t, s.admin, `SELECT (SELECT count(*) FROM stego_outbox.messages WHERE tenant_id = $1 AND destination = 'audit-sink')
 - (SELECT count(*) FROM audit_event WHERE tenant_id = $1)`, owner.tenant.ID()) >= 1
		})
		if n := count(t, s.admin, `SELECT count(*) FROM leases WHERE name = 'stego.controller' AND expires_at > now()`); n != 1 {
			t.Errorf("%d holders of the controller lease, want one", n)
		}
		// The readiness of the modes answers every mode ready.
		eventually("every mode is ready", func() bool {
			response, err := http.Get(s.base + "/readyz/modes")
			if err != nil {
				return false
			}
			defer response.Body.Close()
			return response.StatusCode == http.StatusOK
		})
	})

	t.Run("ST-SUP-RP14-LC04-T03 The generated worker delivers each audit entry and anchor to the default table", func(t *testing.T) {
		s.t = t
		if order == "" {
			t.Fatal("no order: the collection check did not pass")
		}
		// No STEGO_AUDIT_SINK_URL is set, so the worker mode binds the audit-sink port to the table of its own in the
		// service database: every audit-sink message of the tenant is done, and the table holds one row for each,
		// keyed by the event ID or the anchor ID, written as the worker role.
		var last string
		for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			pending := count(t, s.admin, `SELECT count(*) FROM stego_outbox.messages WHERE tenant_id = $1 AND destination = 'audit-sink' AND state <> 'done'`, owner.tenant.ID())
			done := count(t, s.admin, `SELECT count(*) FROM stego_outbox.messages WHERE tenant_id = $1 AND destination = 'audit-sink' AND state = 'done'`, owner.tenant.ID())
			events := count(t, s.admin, `SELECT count(*) FROM stego_audit_sink.entries e JOIN audit_event a ON a.id::text = e.entry_id WHERE e.tenant_id = $1 AND e.kind = 'event'`, owner.tenant.ID())
			anchors := count(t, s.admin, `SELECT count(*) FROM stego_audit_sink.entries WHERE tenant_id = $1 AND kind = 'anchor'`, owner.tenant.ID())
			audited := count(t, s.admin, `SELECT count(*) FROM audit_event WHERE tenant_id = $1`, owner.tenant.ID())
			last = fmt.Sprintf("%d pending, %d done, %d events of %d and %d anchors in the table", pending, done, events, audited, anchors)
			if pending == 0 && done > 0 && audited > 0 && events == audited && anchors >= 1 && done == events+anchors {
				return
			}
		}
		t.Fatalf("after 60 s the audit-sink messages did not all land in the default table: %s\n%s", last, s.log)
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

	t.Run("ST-SUP-RC18-OC31-T02 The generated worker sends to Kafka with the settings of the process, and does not start without them", func(t *testing.T) {
		s.t = t
		// The run with no Kafka setting has the environment of the service of the check, which runs, without the
		// STEGO_KAFKA_ settings alone: its worker fails to start, and the failure record names the stage and the task,
		// with no error text (ST-SUP-RP14-LC05).
		if s.noKafka.err == nil || !strings.Contains(s.noKafka.output, `"stage":"service.run"`) || !strings.Contains(s.noKafka.output, `"stego-runtime[0]"`) {
			t.Fatalf("the worker mode with no Kafka setting gave %v, want a failed start of the runtime task:\n%s", s.noKafka.err, s.noKafka.output)
		}
		// The service of the check runs with the settings: its worker registered the destination kafka, and the
		// change event of the first create is done.
		if order == "" {
			t.Fatal("no order: the collection check did not pass")
		}
		s.eventDone(t, order, "wiring-shop.order.created")
	})

	t.Run("ST-SUP-RC20-OC40-T02 A change event of a generated write arrives once with its key and headers, waits out a broker outage in the outbox, and keeps the order of its entity", func(t *testing.T) {
		s.t = t
		created := s.send(&owner, http.MethodPost, "/orders", "k-"+uuid.NewString(), `{"sku":"k1","qty":1}`)
		if created.status != http.StatusCreated {
			t.Fatalf("the create answered %d %s, want 201", created.status, created.body)
		}
		id := created.member(t, "id")
		s.own(owner, id)
		s.canEdit(t, owner, id)
		first := s.eventDone(t, id, "wiring-shop.order.created")
		records := s.records(t, id, 1)
		if len(records) != 1 {
			t.Fatalf("the topic holds %d records of the order, want the one event", len(records))
		}
		r := records[0]
		if want := kafkaoutbox.Key(owner.tenant.ID(), first.resourceKey); string(r.Key) != want {
			t.Errorf("the record key is %q, want %q: the tenant, the separator and the resource key of the message", r.Key, want)
		}
		var payload map[string]string
		if err := json.Unmarshal(r.Value, &payload); err != nil || payload["entity"] != "Order" || payload["action"] != "created" || payload["id"] != id || len(payload) != 3 {
			t.Errorf("the record value is %s (%v), want the entity Order, the action created and the ID %s only", r.Value, err, id)
		}
		for name, value := range map[string]string{kafkaoutbox.HeaderEventID: first.id, kafkaoutbox.HeaderEventType: "wiring-shop.order.created",
			kafkaoutbox.HeaderEventAction:    "created",
			kafkaoutbox.HeaderIdempotencyKey: first.outgoing, kafkaoutbox.HeaderTenant: owner.tenant.ID(), kafkaoutbox.HeaderContentType: "application/json"} {
			if got, once := kafkatest.Header(r, name); !once || got != value {
				t.Errorf("the header %s is %q (once: %v), want %q", name, got, once, value)
			}
		}

		// The broker is down: the patch commits, and its event stays in the outbox with no record.
		s.broker.Plain.Down()
		patched := s.send(&owner, http.MethodPatch, "/orders/"+id, "k-"+uuid.NewString(), `{"qty":5}`)
		if patched.status != http.StatusOK {
			s.broker.Plain.Up()
			t.Fatalf("the patch during the outage answered %d %s, want 200", patched.status, patched.body)
		}
		deleted := s.send(&owner, http.MethodDelete, "/orders/"+id, "", "")
		if deleted.status != http.StatusNoContent {
			s.broker.Plain.Up()
			t.Fatalf("the delete during the outage answered %d %s, want 204", deleted.status, deleted.body)
		}
		s.eventually(t, "the event of the patch is tried during the outage", func() bool {
			return count(t, s.admin, `SELECT count(*) FROM stego_outbox.messages WHERE destination = 'kafka' AND kind = 'wiring-shop.order.updated'
 AND resource_key = $1 AND state <> 'done' AND attempts > 0`, first.resourceKey) == 1
		})
		pending := count(t, s.admin, `SELECT count(*) FROM stego_outbox.messages WHERE destination = 'kafka' AND resource_key = $1 AND state <> 'done'`, first.resourceKey)
		s.broker.Plain.Up()
		if pending != 2 {
			t.Errorf("%d events of the order wait in the outbox during the outage, want the patch and the delete", pending)
		}
		s.eventDone(t, id, "wiring-shop.order.updated")
		s.eventDone(t, id, "wiring-shop.order.deleted")

		// Each event arrives once, all on one partition, in the order of the writes.
		records = s.records(t, id, 3)
		var actions []string
		for _, r := range records {
			var payload map[string]string
			must(t, json.Unmarshal(r.Value, &payload))
			actions = append(actions, payload["action"])
			if r.Partition != records[0].Partition {
				t.Errorf("the events of one order are on the partitions %d and %d", records[0].Partition, r.Partition)
			}
		}
		if got := strings.Join(actions, ","); got != "created,updated,deleted" {
			t.Errorf("the events of the order arrived as %s, want created,updated,deleted, each once", got)
		}
	})

	t.Run("ST-SUP-RC20-OC40-T03 A replay of a write with its idempotency key stages no second event", func(t *testing.T) {
		s.t = t
		key := "k-" + uuid.NewString()
		first := s.send(&owner, http.MethodPost, "/orders", key, `{"sku":"replayed","qty":1}`)
		again := s.send(&owner, http.MethodPost, "/orders", key, `{"sku":"replayed","qty":1}`)
		if first.status != http.StatusCreated || again.status != http.StatusCreated || first.member(t, "id") != again.member(t, "id") {
			t.Fatalf("the create and its replay answered %d %s and %d %s, want 201 twice with one ID", first.status, first.body, again.status, again.body)
		}
		id := first.member(t, "id")
		s.eventDone(t, id, "wiring-shop.order.created")
		if n := count(t, s.admin, `SELECT count(*) FROM stego_outbox.messages WHERE destination = 'kafka' AND resource_key LIKE '%:' || $1`, id); n != 1 {
			t.Errorf("the outbox holds %d events of the order, want the one of the create", n)
		}
		if records := s.records(t, id, 1); len(records) != 1 {
			t.Errorf("the topic holds %d records of the order, want one", len(records))
		}
	})

	t.Run("ST-SUP-RC20-OC40-T04 A write that rolls back after it staged its event leaves no outbox message and no record", func(t *testing.T) {
		s.t = t
		// A deferred trigger refuses the commit of an order with the sku rolled-back: the fill wrote the row and
		// staged the event, and the transaction rolls back at its end. The schema manifest check runs only at start,
		// so the trigger, which the test drops again, does not stop the running service.
		execAll(t, s.admin, `CREATE FUNCTION wiring_refuse_at_commit() RETURNS trigger LANGUAGE plpgsql AS $f$
BEGIN RAISE EXCEPTION 'the check refuses this commit'; END $f$`,
			`CREATE CONSTRAINT TRIGGER wiring_refuse_at_commit AFTER INSERT ON orders DEFERRABLE INITIALLY DEFERRED
 FOR EACH ROW WHEN (NEW.sku = 'rolled-back') EXECUTE FUNCTION wiring_refuse_at_commit()`)
		defer execAll(t, s.admin, "DROP TRIGGER wiring_refuse_at_commit ON orders", "DROP FUNCTION wiring_refuse_at_commit()")
		messages := s.kafkaMessages(t)
		before := len(s.broker.Records(t, changeTopic, 0, 10*time.Second, time.Second))
		got := s.send(&owner, http.MethodPost, "/orders", "k-"+uuid.NewString(), `{"sku":"rolled-back","qty":1}`)
		if got.status < http.StatusInternalServerError {
			t.Fatalf("the create whose commit is refused answered %d %s, want a server error", got.status, got.body)
		}
		if n := count(t, s.admin, `SELECT count(*) FROM orders WHERE sku = 'rolled-back'`); n != 0 {
			t.Errorf("the refused create left %d rows", n)
		}
		if n := s.kafkaMessages(t); n != messages {
			t.Errorf("the outbox holds %d events of the destination kafka after the refused create, want the %d of before", n, messages)
		}
		if after := len(s.broker.Records(t, changeTopic, 0, 10*time.Second, 3*time.Second)); after != before {
			t.Errorf("the topic holds %d records after the refused create, want the %d of before", after, before)
		}
	})
}

// kafkaMessages counts the outbox messages of the destination kafka.
func (s *service) kafkaMessages(t *testing.T) int {
	return count(t, s.admin, `SELECT count(*) FROM stego_outbox.messages WHERE destination = 'kafka'`)
}

// canEdit waits until SpiceDB answers, at its quantized revision, that the caller may edit the order id, as the
// service asks it on a patch or a delete.
func (s *service) canEdit(t *testing.T, c caller, id string) {
	t.Helper()
	resourceID, err := authz.ResourceID(c.tenant, id)
	must(t, err)
	s.eventually(t, "SpiceDB answers that the owner may edit the order", func() bool {
		answer, err := v1.NewPermissionsServiceClient(s.spice).CheckPermission(s.ctx, &v1.CheckPermissionRequest{
			Consistency: &v1.Consistency{Requirement: &v1.Consistency_MinimizeLatency{MinimizeLatency: true}},
			Resource:    &v1.ObjectReference{ObjectType: "order", ObjectId: resourceID}, Permission: "edit",
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: c.subject}}})
		return err == nil && answer.GetPermissionship() == v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION
	})
}

// message is one outbox message of the destination kafka.
type message struct{ id, resourceKey, outgoing string }

// eventually waits at most 60 s for ok.
func (s *service) eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("after 60 s: %s\n%s", what, s.log)
}

// eventDone waits until the one change event of the kind for the order id is done, and gives its message.
func (s *service) eventDone(t *testing.T, id, kind string) message {
	t.Helper()
	s.eventually(t, "the event "+kind+" of the order "+id+" is done", func() bool {
		return count(t, s.admin, `SELECT count(*) FROM stego_outbox.messages WHERE destination = 'kafka' AND kind = $1
 AND resource_key LIKE '%:' || $2 AND state = 'done'`, kind, id) == 1
	})
	var m message
	must(t, s.admin.QueryRow(`SELECT id::text, resource_key, outgoing_key FROM stego_outbox.messages WHERE destination = 'kafka'
 AND kind = $1 AND resource_key LIKE '%:' || $2`, kind, id).Scan(&m.id, &m.resourceKey, &m.outgoing))
	return m
}

// records reads the records of the change topic whose payload names the order id: at least least of them, and then
// what more arrives in the next three seconds.
func (s *service) records(t *testing.T, id string, least int) []*kgo.Record {
	t.Helper()
	var mine []*kgo.Record
	for _, r := range s.broker.Records(t, changeTopic, 0, 10*time.Second, 3*time.Second) {
		if bytes.Contains(r.Value, []byte(`"id":"`+id+`"`)) {
			mine = append(mine, r)
		}
	}
	if len(mine) < least {
		t.Fatalf("the topic %s holds %d records of the order %s, want at least %d", changeTopic, len(mine), id, least)
	}
	return mine
}
