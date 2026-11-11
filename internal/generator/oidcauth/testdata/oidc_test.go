// Package check runs a generated service of the runtime data path that signs in with oidc-auth or with rh-sso-auth,
// as an operator runs it: the migration job and then the service binary in the api mode, against a local
// PostgreSQL server (STEGO_TEST_POSTGRES_DSN), a local SpiceDB container (STEGO_TEST_SPICEDB) and a local OpenID
// Connect provider: the key server of authntest, which serves the discovery document and the key document of the
// issuer over TLS on 127.0.0.1 with its own certificate authority (ST-STD-LOCAL). It sends tokens of that provider
// over HTTP.
package check

import (
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
	_ "github.com/jackc/pgx/v5/stdlib"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/jsell-rh/stego/registry/runtime/authn"
	"github.com/jsell-rh/stego/registry/runtime/authn/authntest"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/tenant"

	"example.com/shop/out/internal/stegoruntime"
	"example.com/shop/out/internal/tenancy"
)

const (
	audience = "orders-api"
	password = "oidc-check"
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

func freePort(t *testing.T) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer listener.Close()
	return fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
}

// bearer sends the preshared key of the local SpiceDB server on each call.
type bearer string

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}
func (bearer) RequireTransportSecurity() bool { return false }

// service is the running service, its provider and the keys of the provider.
type service struct {
	t        *testing.T
	provider *authntest.Server
	issuer   string
	rsa, ec  *authntest.Key
	es       *authntest.ECKey
	spice    *grpc.ClientConn
	base     string
	env      []string
	binary   string
	started  time.Time
	log      *strings.Builder
}

// start makes a database of its own with the roles of the tenancy component, runs the migration job, and starts the
// service in the api mode with the CA file of the provider, until it answers /livez. The declaration states the
// issuer, on the address of STEGO_OIDC_ADDRESS, where the provider listens.
func start(t *testing.T) *service {
	dsn, endpoint, key := os.Getenv("STEGO_TEST_POSTGRES_DSN"), os.Getenv("STEGO_TEST_SPICEDB"), os.Getenv("STEGO_TEST_SPICEDB_KEY")
	binary, address, issuerPath := os.Getenv("STEGO_OIDC_BINARY"), os.Getenv("STEGO_OIDC_ADDRESS"), os.Getenv("STEGO_OIDC_ISSUER_PATH")
	if dsn == "" || endpoint == "" || binary == "" || address == "" || issuerPath == "" {
		t.Fatal("the check needs STEGO_TEST_POSTGRES_DSN, STEGO_TEST_SPICEDB, STEGO_OIDC_BINARY, STEGO_OIDC_ADDRESS and STEGO_OIDC_ISSUER_PATH: a missing result is not a pass")
	}
	s := &service{t: t, provider: authntest.NewServerAt(t, address), binary: binary, log: &strings.Builder{}}
	s.issuer = s.provider.Issuer(issuerPath)
	s.rsa, s.es = authntest.NewKey(t, "r1"), authntest.NewECKey(t, "e1")
	s.provider.Publish(s.rsa, s.es)

	root, err := sql.Open("pgx", dsn)
	must(t, err)
	t.Cleanup(func() { root.Close() })
	t.Cleanup(func() {
		for _, role := range []string{stegoruntime.RuntimeRole, stegoruntime.WorkerRole, stegoruntime.RelayRole, stegoruntime.OwnerRole} {
			if _, err := root.Exec("DROP ROLE IF EXISTS " + quote(role)); err != nil {
				t.Errorf("drop the role %s: %v", role, err)
			}
		}
	})
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	name := "oidc_" + hex.EncodeToString(suffix)
	execAll(t, root, "CREATE DATABASE "+name)
	t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)") })
	server, err := url.Parse(dsn)
	must(t, err)
	server.Path = "/" + name
	admin, err := sql.Open("pgx", server.String())
	must(t, err)
	t.Cleanup(func() { admin.Close() })
	execAll(t, admin, tenancy.Roles)
	for _, role := range []string{stegoruntime.RuntimeRole, stegoruntime.WorkerRole, stegoruntime.RelayRole, stegoruntime.OwnerRole} {
		execAll(t, admin, "ALTER ROLE "+quote(role)+" LOGIN PASSWORD '"+password+"'")
	}
	as := func(role string) string {
		u := *server
		u.User = url.UserPassword(role, password)
		q := u.Query()
		q.Set("sslmode", "disable")
		u.RawQuery = q.Encode()
		return u.String()
	}
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithPerRPCCredentials(bearer(key)))
	must(t, err)
	t.Cleanup(func() { conn.Close() })
	s.spice = conn
	// The database is a loopback server with no TLS, which the adapter takes only in its loopback test mode.
	common := []string{"STEGO_SPICEDB_ENDPOINT=" + endpoint, "STEGO_SPICEDB_KEY_FILE=" + writeFile(t, "spicedb.key", []byte(key)),
		"STEGO_SPICEDB_INSECURE_LOOPBACK=1", "STEGO_DATABASE_ALLOW_INSECURE_LOOPBACK=1"}
	if r := run(binary, append(common, "STEGO_MIGRATION_DATABASE_URL="+as(stegoruntime.OwnerRole)), "migrate"); r.err != nil {
		t.Fatalf("the migration job failed: %v\n%s", r.err, r.output)
	}
	cursor := make([]byte, 32)
	_, _ = rand.Read(cursor)
	s.env = append(common, "DATABASE_URL="+as(stegoruntime.RuntimeRole), "STEGO_MODE=api",
		"STEGO_AUTHN_CA_FILE="+s.provider.CAFile(t), "STEGO_QUERY_CURSOR_KEY_FILE="+writeFile(t, "cursor.key", cursor),
		"STEGO_SERVICE_VERSION=1.0.0")
	port := freePort(t)
	s.base = "http://127.0.0.1:" + port
	process := exec.Command(binary)
	process.Env = append(append(os.Environ(), s.env...), "PORT="+port)
	process.Stdout, process.Stderr = s.log, s.log
	s.started = time.Now()
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
		if response, err := http.Get(s.base + "/livez"); err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return s
			}
		}
		select {
		case <-exited:
			t.Fatalf("the service stopped before it answered /livez:\n%s", s.log)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the service does not answer /livez:\n%s", s.log)
		}
	}
}

// runResult is the outcome of one run of the binary.
type runResult struct {
	err    error
	output string
}

func run(binary string, env []string, args ...string) runResult {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = append(os.Environ(), env...)
	output, err := cmd.CombinedOutput()
	return runResult{err: err, output: string(output)}
}

// claims are valid claims of the provider for the subject in the organization org, in the claim org_id.
func (s *service) claims(subject, org string) map[string]any {
	c := authntest.Claims(s.issuer, audience, subject, time.Now())
	c["exp"] = time.Now().Add(time.Hour).Unix()
	c["org_id"] = org
	c["preferred_username"] = "user-" + subject
	c["email"] = subject + "@example.com"
	return c
}

// token mints a token with the algorithm and the key ID, signed with the key.
func (s *service) token(alg, kid string, claims map[string]any, key any) string {
	return authntest.Mint(s.t, authntest.HeaderOf(alg, kid), claims, key)
}

// reply is one answer of the service.
type reply struct {
	status int
	body   []byte
}

// send sends one request with the bearer token, when it is not empty.
func (s *service) send(token, method, path, body string) reply {
	t := s.t
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, s.base+"/api/shop/v1"+path, reader)
	must(t, err)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
		key := make([]byte, 16)
		_, _ = rand.Read(key)
		request.Header.Set("Idempotency-Key", `"`+hex.EncodeToString(key)+`"`)
	}
	response, err := http.DefaultClient.Do(request)
	must(t, err)
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	must(t, err)
	return reply{status: response.StatusCode, body: data}
}

// accepted reports that the service took the token: a read of an order that does not exist reaches the handler and
// is answered 404, where a refused token is answered 401.
func (s *service) accepted(token string) bool {
	s.t.Helper()
	r := s.send(token, http.MethodGet, "/orders/00000000-0000-4000-8000-000000000000", "")
	if r.status != http.StatusNotFound && r.status != http.StatusUnauthorized {
		s.t.Fatalf("the read gave %d %s, want 404 or 401", r.status, r.body)
	}
	return r.status == http.StatusNotFound
}

// identity creates an order with the token, makes the subject of the token its owner in SpiceDB in the tenant of
// org, and reads it with the token and with the tokens of another tenant and another subject: only the first sees
// the order, so the verified identity holds the subject and the tenant of the token's claims.
func (s *service) identity(org, own, otherTenant, otherSubject string) {
	t := s.t
	t.Helper()
	created := s.send(own, http.MethodPost, "/orders", `{"sku":"a-1","qty":1}`)
	if created.status != http.StatusCreated {
		t.Fatalf("the create gave %d %s, want 201", created.status, created.body)
	}
	var order map[string]any
	must(t, json.Unmarshal(created.body, &order))
	id, _ := order["id"].(string)
	if id == "" {
		t.Fatalf("the create gave no id: %s", created.body)
	}
	tn, subject := s.principal(own)
	if tn.ID() != org {
		t.Fatalf("the token's tenant is %s, want %s", tn.ID(), org)
	}
	resourceID, err := authz.ResourceID(tn, id)
	must(t, err)
	_, err = v1.NewPermissionsServiceClient(s.spice).WriteRelationships(context.Background(), &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{
		Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
		Relationship: &v1.Relationship{Resource: &v1.ObjectReference{ObjectType: "order", ObjectId: resourceID}, Relation: "owner",
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: subject}}}}}})
	must(t, err)
	var got reply
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if got = s.send(own, http.MethodGet, "/orders/"+id, ""); got.status == http.StatusOK {
			break
		}
	}
	if got.status != http.StatusOK {
		t.Fatalf("the owner's read gave %d %s, want 200", got.status, got.body)
	}
	for name, token := range map[string]string{"another tenant": otherTenant, "another subject": otherSubject} {
		if r := s.send(token, http.MethodGet, "/orders/"+id, ""); r.status != http.StatusNotFound {
			t.Errorf("the read with the token of %s gave %d %s, want 404", name, r.status, r.body)
		}
	}
}

// principal is the tenant and the SpiceDB subject of the verified identity of the token, as the tenant and the
// authorization runtimes map it: the identity that a verifier of the same issuer and the provider's keys gives.
func (s *service) principal(token string) (tenant.Tenant, string) {
	t := s.t
	t.Helper()
	keys, err := authn.ParseKeys(authntest.Document(s.rsa.JWK(), s.es.JWK()))
	must(t, err)
	verifier, err := authn.New(authn.Settings{Issuer: s.issuer, Audience: audience, TenantClaim: "org_id", ProblemCode: "CHECK",
		Algorithms: []string{authn.AlgorithmRS256, authn.AlgorithmES256}, Keys: keys})
	must(t, err)
	defer verifier.Stop()
	id, err := verifier.Verify(context.Background(), token)
	must(t, err)
	tn, err := tenant.New(tenant.Claim(id))
	must(t, err)
	principal, err := authz.Person("user", id)
	must(t, err)
	return tn, principal.Subject().ID
}

// startFails runs the binary with the provider's discovery document set to the document, and gives the output of
// the failed start.
func (s *service) startFails(document []byte) runResult {
	s.provider.SetDiscovery(document)
	defer s.provider.SetDiscovery(nil)
	r := run(s.binary, append(append([]string{}, s.env...), "PORT="+freePort(s.t)))
	return r
}

// refusals checks what every configuration of oidc-auth refuses: a token of another issuer, an expired token, a
// token of an unknown key, a token for another audience, PS256 and alg none; that a rotated key of the provider is
// taken after the cooldown of the key set with no second discovery fetch; and that a start with a discovery document
// of another issuer or with an http key set URL is refused at the stage service.start.authn.discovery, with no
// address in the failure record.
func (s *service) refusals(good map[string]any) {
	t := s.t
	t.Helper()
	other := s.claims("u1", "org-1")
	other["iss"] = "https://127.0.0.1:1/realms/shop"
	expired := s.claims("u1", "org-1")
	expired["iat"], expired["exp"] = time.Now().Add(-2*time.Hour).Unix(), time.Now().Add(-time.Hour).Unix()
	foreign := s.claims("u1", "org-1")
	foreign["aud"] = "billing-api"
	unknown := authntest.NewKey(t, "k9")
	for name, token := range map[string]string{
		"a token of another issuer":    s.token("RS256", "r1", other, s.rsa),
		"an expired token":             s.token("RS256", "r1", expired, s.rsa),
		"a token of an unknown key":    s.token("RS256", "k9", good, unknown),
		"a token for another audience": s.token("RS256", "r1", foreign, s.rsa),
		"a PS256 token":                s.token("PS256", "r1", good, s.rsa),
		"an alg none token":            s.token("none", "r1", good, nil),
	} {
		if s.accepted(token) {
			t.Errorf("%s was accepted", name)
		}
	}
	// Key rotation: the provider publishes the new key r2; after the cooldown of the key set, a token of r2 loads the
	// key document again, and not the discovery document.
	rotated := authntest.NewKey(t, "r2")
	s.provider.Publish(s.rsa, s.es, rotated)
	time.Sleep(time.Until(s.started.Add(31 * time.Second)))
	if !s.accepted(s.token("RS256", "r2", good, rotated)) || s.provider.Discoveries() != 1 {
		t.Errorf("the token of the rotated key was refused, or the discovery document was read again (%d fetches)", s.provider.Discoveries())
	}
	for name, document := range map[string][]byte{
		"another issuer":      authntest.Discovery("https://127.0.0.1:1/realms/shop", s.provider.URL()),
		"an http key set URL": authntest.Discovery(s.issuer, strings.Replace(s.provider.URL(), "https:", "http:", 1)),
	} {
		r := s.startFails(document)
		if r.err == nil || !strings.Contains(r.output, `"stage":"service.start.authn.discovery"`) || strings.Contains(r.output, s.provider.Addr()) {
			t.Errorf("the start with a discovery document of %s gave %v, want a refusal at the stage service.start.authn.discovery with no address:\n%s", name, r.err, r.output)
		}
	}
}

func TestOIDC(t *testing.T) {
	s := start(t)
	form := os.Getenv("STEGO_OIDC_FORM")
	t.Logf("the provider %s serves the form %s", s.issuer, form)
	good := s.claims("u1", "org-1")
	switch form {
	case "oidc-auth":
		t.Run("ST-SUP-RC37-OC01-T01 A provider is found through its discovery document, and a mismatched document is refused", func(t *testing.T) {
			s.t = t
			if !s.accepted(s.token("RS256", "r1", good, s.rsa)) || s.provider.Discoveries() != 1 {
				t.Fatalf("the token was not accepted after one discovery fetch (%d fetches)", s.provider.Discoveries())
			}
			s.refusals(good)
		})
		t.Run("ST-SUP-RC37-OC02-T01 A token for another audience or with another algorithm is refused", func(t *testing.T) {
			s.t = t
			if !s.accepted(s.token("RS256", "r1", good, s.rsa)) || !s.accepted(s.token("ES256", "e1", good, s.es)) {
				t.Fatal("an RS256 or ES256 token for orders-api was refused")
			}
			foreign := s.claims("u1", "org-1")
			foreign["aud"] = "billing-api"
			if s.accepted(s.token("RS256", "r1", foreign, s.rsa)) || s.accepted(s.token("PS256", "r1", good, s.rsa)) ||
				s.accepted(s.token("none", "r1", good, nil)) {
				t.Error("a token for another audience, with PS256 or with alg none was accepted")
			}
		})
		t.Run("ST-SUP-RC37-OC03-T01 The subject and the tenant come only from the declared claims", func(t *testing.T) {
			s.t = t
			missing := s.claims("u1", "org-1")
			delete(missing, "org_id")
			elsewhere := s.claims("u1", "org-1")
			delete(elsewhere, "org_id")
			elsewhere["tenant"] = "org-1"
			for name, token := range map[string]string{"no org_id": s.token("RS256", "r1", missing, s.rsa),
				"the tenant in another claim": s.token("RS256", "r1", elsewhere, s.rsa)} {
				if r := s.send(token, http.MethodGet, "/orders/00000000-0000-4000-8000-000000000000", ""); r.status != http.StatusUnauthorized {
					t.Errorf("the token with %s gave %d, want 401", name, r.status)
				}
			}
			s.identity("org-1", s.token("ES256", "e1", good, s.es), s.token("RS256", "r1", s.claims("u1", "org-2"), s.rsa),
				s.token("RS256", "r1", s.claims("u2", "org-1"), s.rsa))
		})
	case "rh-sso-auth":
		t.Run("ST-SUP-RC21-OC15-T01 A service with rh-sso-auth on the runtime data path gets the oidc-auth verifier with the Red Hat SSO settings", func(t *testing.T) {
			s.t = t
			if !strings.HasSuffix(s.issuer, "/auth/realms/redhat-external") {
				t.Fatalf("the issuer %s is not of the Red Hat SSO form", s.issuer)
			}
			if !s.accepted(s.token("RS256", "r1", good, s.rsa)) || s.provider.Discoveries() != 1 {
				t.Fatalf("the Red Hat SSO token was not accepted after one discovery fetch (%d fetches)", s.provider.Discoveries())
			}
			missing := s.claims("u1", "org-1")
			delete(missing, "org_id")
			for name, token := range map[string]string{"ES256, which Red Hat SSO does not sign with": s.token("ES256", "e1", good, s.es),
				"no org_id": s.token("RS256", "r1", missing, s.rsa)} {
				if s.accepted(token) {
					t.Errorf("the token with %s was accepted", name)
				}
			}
			s.identity("org-1", s.token("RS256", "r1", good, s.rsa), s.token("RS256", "r1", s.claims("u1", "org-2"), s.rsa),
				s.token("RS256", "r1", s.claims("u2", "org-1"), s.rsa))
			s.refusals(good)
		})
	default:
		t.Fatalf("STEGO_OIDC_FORM is %q, want oidc-auth or rh-sso-auth", form)
	}
}
