// Package evolutioncheck runs the releases of ST-GEN-TD04 on one database, as an operator runs them in a rolling
// deploy: the migration job of v1, the rows of the data set, then the migration job of v2, which runs the expand
// steps and the backfill under its lease, while the api replicas of v1 and v2 serve. It runs the service binaries of
// STEGO_TD04_V1 and STEGO_TD04_V2 against the local PostgreSQL server of STEGO_TEST_POSTGRES_DSN and the local
// SpiceDB container of STEGO_TEST_SPICEDB, and calls the collections over HTTP.
package evolutioncheck

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
	"sync"
	"syscall"
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
	password = "td04-check"
	base     = "/api/user-mgmt/v1"
	change   = "user-email-rename"
	leaseOf  = "stego.backfill." + change
	// ttl is the default lease TTL of the backfill (lease.DefaultTTL), and renew its renew interval.
	ttl   = 15 * time.Second
	renew = 5 * time.Second
)

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func writeFile(t *testing.T, name string, data []byte) string {
	file := filepath.Join(t.TempDir(), name)
	must(t, os.WriteFile(file, data, 0o600))
	return file
}

// buffer is the output of a process, which the test reads while the process writes it.
type buffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// database is one database of ST-GEN-TD04 after the migration job of v1 and the load of its rows.
type database struct {
	t     *testing.T
	admin *sql.DB
	spice *grpc.ClientConn
	key   *authntest.Key
	env   []string
	as    func(role string) string
}

var roles = []string{stegoruntime.RuntimeRole, stegoruntime.WorkerRole, stegoruntime.RelayRole, stegoruntime.OwnerRole}

// prepared makes a database of its own with the roles of the tenancy component, runs the migration job of v1 on
// it as the owner role, and loads the rows of seed.sql (ST-GEN-TD04 prepare).
func prepared(t *testing.T) *database {
	dsn, endpoint := os.Getenv("STEGO_TEST_POSTGRES_DSN"), os.Getenv("STEGO_TEST_SPICEDB")
	if dsn == "" || endpoint == "" || os.Getenv("STEGO_TD04_V1") == "" || os.Getenv("STEGO_TD04_V2") == "" || os.Getenv("STEGO_TD04_SEED") == "" {
		t.Fatal("the check needs STEGO_TEST_POSTGRES_DSN, STEGO_TEST_SPICEDB, STEGO_TD04_V1, STEGO_TD04_V2 and STEGO_TD04_SEED: a missing result is not a pass")
	}
	server, err := url.Parse(dsn)
	must(t, err)
	root, err := sql.Open("pgx", dsn)
	must(t, err)
	t.Cleanup(func() { root.Close() })
	t.Cleanup(func() {
		for _, role := range roles {
			_, _ = root.Exec("DROP OWNED BY " + quote(role) + " CASCADE")
			_, _ = root.Exec("DROP ROLE IF EXISTS " + quote(role))
		}
	})
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	name := "td04_" + hex.EncodeToString(suffix)
	_, err = root.Exec("CREATE DATABASE " + name)
	must(t, err)
	t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)") })
	u := *server
	u.Path = "/" + name
	d := &database{t: t}
	d.admin, err = sql.Open("pgx", u.String())
	must(t, err)
	t.Cleanup(func() { d.admin.Close() })
	_, err = d.admin.Exec(tenancy.Roles)
	must(t, err)
	for _, role := range roles {
		_, err = d.admin.Exec("ALTER ROLE " + quote(role) + " LOGIN PASSWORD '" + password + "'")
		must(t, err)
	}
	d.as = func(role string) string {
		v := u
		v.User = url.UserPassword(role, password)
		q := v.Query()
		q.Set("sslmode", "disable")
		v.RawQuery = q.Encode()
		return v.String()
	}
	spiceKey := os.Getenv("STEGO_TEST_SPICEDB_KEY")
	d.spice, err = grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithPerRPCCredentials(bearer(spiceKey)))
	must(t, err)
	t.Cleanup(func() { d.spice.Close() })
	d.env = []string{"STEGO_DATABASE_ALLOW_INSECURE_LOOPBACK=1", "STEGO_SPICEDB_ENDPOINT=" + endpoint,
		"STEGO_SPICEDB_KEY_FILE=" + writeFile(t, "spicedb.key", []byte(spiceKey)), "STEGO_SPICEDB_INSECURE_LOOPBACK=1"}
	d.key = authntest.NewKey(t, "k1")
	if j := d.migrate("v1"); j.wait(time.Minute) != nil {
		t.Fatalf("the migration job of v1 failed:\n%s", j.out)
	}
	seed, err := os.ReadFile(os.Getenv("STEGO_TD04_SEED"))
	must(t, err)
	_, err = d.admin.Exec(string(seed))
	must(t, err)
	return d
}

// binary gives the service binary of the release.
func binary(release string) string { return os.Getenv("STEGO_TD04_" + strings.ToUpper(release)) }

// job is one run of a migration job.
type job struct {
	cmd    *exec.Cmd
	out    *buffer
	done   chan struct{}
	err    error
	holder string
}

// migrate starts the migration job of the release as the owner role, with the probe console in JSON.
func (d *database) migrate(release string) *job {
	t := d.t
	j := &job{out: &buffer{}, done: make(chan struct{})}
	j.cmd = exec.Command(binary(release), "migrate")
	j.cmd.Env = append(append(os.Environ(), d.env...), "STEGO_MIGRATION_DATABASE_URL="+d.as(stegoruntime.OwnerRole), "STEGO_PROBE_CONSOLE=json")
	j.cmd.Stdout, j.cmd.Stderr = j.out, j.out
	must(t, j.cmd.Start())
	go func() { j.err = j.cmd.Wait(); close(j.done) }()
	t.Cleanup(func() {
		if t.Failed() {
			holder, token := d.leaseRow()
			out := j.out.String()
			if len(out) > 4000 {
				out = out[len(out)-4000:]
			}
			var activity []string
			if rows, err := d.admin.Query(`SELECT usename || ' ' || state || ' ' || COALESCE(wait_event_type, '') || ':' || COALESCE(wait_event, '') || ' ' || pg_catalog.pg_blocking_pids(pid)::text || ' ' || (now() - query_start)::text || ' ' || pg_catalog.left(query, 120)
FROM pg_catalog.pg_stat_activity WHERE datname = pg_catalog.current_database() AND pid <> pg_catalog.pg_backend_pid()`); err == nil {
				for rows.Next() {
					var a string
					_ = rows.Scan(&a)
					activity = append(activity, a)
				}
				rows.Close()
			}
			t.Logf("migration job of %s (running %v): checkpoint %+v, lease %s %d, sessions:\n%s\noutput:\n%s", release, j.running(), d.checkpoint(), holder, token, strings.Join(activity, "\n"), out)
		}
		_ = j.cmd.Process.Signal(syscall.SIGCONT)
		_ = j.cmd.Process.Kill()
		<-j.done
	})
	return j
}

// wait waits for the job to end within the time and gives its error.
func (j *job) wait(within time.Duration) error {
	select {
	case <-j.done:
		return j.err
	case <-time.After(within):
		return fmt.Errorf("the migration job did not end within %v", within)
	}
}

func (j *job) running() bool {
	select {
	case <-j.done:
		return false
	default:
		return true
	}
}

// api is one api replica of a release.
type api struct {
	base string
	log  *buffer
}

// freePort gives a free port of the loopback address.
func freePort(t *testing.T) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer listener.Close()
	return fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
}

// serve starts an api replica of the release on the database until it answers /livez.
func (d *database) serve(release string) *api {
	t := d.t
	port := freePort(t)
	cursor := make([]byte, 32)
	_, _ = rand.Read(cursor)
	a := &api{base: "http://127.0.0.1:" + port, log: &buffer{}}
	process := exec.Command(binary(release))
	process.Env = append(append(os.Environ(), d.env...), "STEGO_MODE=api", "PORT="+port, "DATABASE_URL="+d.as(stegoruntime.RuntimeRole),
		"STEGO_WORKER_DATABASE_URL="+d.as(stegoruntime.WorkerRole), "STEGO_RELAY_DATABASE_URL="+d.as(stegoruntime.RelayRole),
		"STEGO_AUTHN_KEYS_FILE="+writeFile(t, "keys.json", authntest.Document(d.key.JWK())),
		"STEGO_AUTHN_ISSUER="+issuer, "STEGO_AUTHN_AUDIENCE="+audience,
		"STEGO_QUERY_CURSOR_KEY_FILE="+writeFile(t, "cursor.key", cursor), "STEGO_SERVICE_VERSION="+release)
	process.Stdout, process.Stderr = a.log, a.log
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
			t.Logf("%s api log:\n%s", release, a.log)
		}
	})
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if a.live() {
			return a
		}
		select {
		case <-exited:
			t.Fatalf("the %s api replica stopped before it answered /livez:\n%s", release, a.log)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the %s api replica does not answer /livez:\n%s", release, a.log)
		}
	}
}

func (a *api) live() bool {
	response, err := http.Get(a.base + "/livez")
	if err != nil {
		return false
	}
	response.Body.Close()
	return response.StatusCode == http.StatusOK
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

func (d *database) caller(subject, tenantID string, roles ...string) caller {
	t := d.t
	claims := authntest.Claims(issuer, audience, subject, time.Now())
	claims["tenant"] = tenantID
	if len(roles) > 0 {
		claims["roles"] = roles
	}
	claims["exp"] = time.Now().Add(time.Hour).Unix()
	token := authntest.Mint(t, authntest.Header("k1"), claims, d.key)
	keys, err := authn.ParseKeys(authntest.Document(d.key.JWK()))
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

// relate writes the relation of a resource of the caller's tenant in SpiceDB to the caller.
func (d *database) relate(c caller, resourceType, id, relation string) {
	resourceID, err := authz.ResourceID(c.tenant, id)
	must(d.t, err)
	_, err = v1.NewPermissionsServiceClient(d.spice).WriteRelationships(context.Background(), &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{
		Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
		Relationship: &v1.Relationship{Resource: &v1.ObjectReference{ObjectType: resourceType, ObjectId: resourceID}, Relation: relation,
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: c.subject}}}}}})
	must(d.t, err)
}

type reply struct {
	status int
	body   []byte
}

func (r reply) object(t *testing.T) map[string]any {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(r.body, &document); err != nil {
		t.Fatalf("the answer %s is not a JSON object: %v", r.body, err)
	}
	return document
}

// send sends one request as the caller to the api replica, with the token in the header that the example declares.
func (d *database) send(a *api, c caller, method, path, body string) reply {
	t := d.t
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, a.base+base+path, reader)
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

func (d *database) count(query string, args ...any) int {
	d.t.Helper()
	var n int
	if err := d.admin.QueryRow(query, args...).Scan(&n); err != nil {
		d.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// checkpoint is the row of the change in stego_schema.backfills.
type checkpoint struct {
	moved, token int64
	completed    sql.NullTime
	present      bool
}

// checkpoint reads the row of the change; before the step migration of v2 makes it, the row is zero.
func (d *database) checkpoint() checkpoint {
	d.t.Helper()
	var c checkpoint
	err := d.admin.QueryRow(`SELECT rows_moved, token, completed_at FROM stego_schema.backfills WHERE change_id = $1`, change).Scan(&c.moved, &c.token, &c.completed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) && !strings.Contains(err.Error(), "SQLSTATE 42P01") && !strings.Contains(err.Error(), "SQLSTATE 3F000") {
		d.t.Fatal(err)
	}
	c.present = err == nil
	return c
}

// held starts the migration job of v2 with its backfill held at a known batch: a gate holds the lease of the backfill
// until the steps of the job end, since a row lock would block their DDL; then a row lock on the first user of the
// tenant holds the batch of that tenant, and the gate opens. It gives the job and the release of the row lock.
func (d *database) held(tenantID string) (*job, func()) {
	jobs, release := d.heldJobs(tenantID, 1)
	return jobs[0], release
}

// heldJobs is held with n migration jobs of v2, which all end their steps before the row lock, since the steps of a
// job that starts later would wait behind the lock and hold the batches of the others in the lock queue.
func (d *database) heldJobs(tenantID string, n int) ([]*job, func()) {
	t := d.t
	_, err := d.admin.Exec(`INSERT INTO public.leases (name, holder, expires_at) VALUES ($1, 'td04-gate', now() + interval '1 hour')`, leaseOf)
	must(t, err)
	// Each job starts when the one before it waits for the lease: the job waits for the lease once its steps end,
	// and its lease session last ran the acquire statement. A runtime step of the job takes an ACCESS EXCLUSIVE lock
	// on the entity tables, so the row lock waits until every job waits for the lease.
	var jobs []*job
	for i := range n {
		jobs = append(jobs, d.migrate("v2"))
		until(t, time.Minute, "the migration job of v2 waits for the lease of the backfill", func() bool {
			return d.count(`SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE datname = pg_catalog.current_database()
  AND query LIKE 'UPDATE public.leases SET holder=%' AND state = 'idle'`) > i
		})
	}
	release := d.lockRow(tenantID)
	t.Cleanup(func() {
		if t.Failed() {
			var locks []string
			rows, err := d.admin.Query(`SELECT l.pid::text || ' ' || l.locktype || ' ' || COALESCE(c.relname, '') || ' ' || l.mode || ' ' || l.granted::text
FROM pg_catalog.pg_locks l LEFT JOIN pg_catalog.pg_class c ON c.oid = l.relation WHERE NOT l.granted OR c.relname = 'users'`)
			if err == nil {
				for rows.Next() {
					var a string
					_ = rows.Scan(&a)
					locks = append(locks, a)
				}
				rows.Close()
			}
			var activity []string
			if rows, err := d.admin.Query(`SELECT pid::text || ' ' || usename || ' ' || state || ' ' || COALESCE(wait_event_type, '') || ':' || COALESCE(wait_event, '') || ' ' || pg_catalog.pg_blocking_pids(pid)::text || ' ' || pg_catalog.left(query, 80)
FROM pg_catalog.pg_stat_activity WHERE datname = pg_catalog.current_database()`); err == nil {
				for rows.Next() {
					var a string
					_ = rows.Scan(&a)
					activity = append(activity, a)
				}
				rows.Close()
			}
			t.Logf("while the row lock holds: locks\n%s\nsessions\n%s", strings.Join(locks, "\n"), strings.Join(activity, "\n"))
		}
	})
	_, err = d.admin.Exec(`UPDATE public.leases SET expires_at = now() - interval '1 second' WHERE name = $1 AND holder = 'td04-gate'`, leaseOf)
	must(t, err)
	return jobs, release
}

// holding gives the job of the two that moves rows, and the other.
func holding(t *testing.T, jobs []*job) (*job, *job) {
	t.Helper()
	var holder, other *job
	until(t, 30*time.Second, "one job reports moved rows", func() bool {
		for i, j := range jobs {
			if len(probeLines(j.out.String(), "schema.backfill.rows")) > 0 {
				holder, other = j, jobs[1-i]
				return true
			}
		}
		return false
	})
	return holder, other
}

// leaseRow is the lease of the backfill: its holder and its token.
func (d *database) leaseRow() (string, int64) {
	var holder string
	var token int64
	if err := d.admin.QueryRow(`SELECT holder, token FROM public.leases WHERE name = $1`, leaseOf).Scan(&holder, &token); err != nil {
		return "", 0
	}
	return holder, token
}

// column is one column of the users table as the catalog holds it.
type column struct {
	typ     string
	notNull bool
}

// catalog reads the columns, the unique indexes and the triggers of the users table.
func (d *database) catalog() (map[string]column, map[string]string, []string) {
	t := d.t
	columns := map[string]column{}
	rows, err := d.admin.Query(`SELECT a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod), a.attnotnull FROM pg_catalog.pg_attribute a
WHERE a.attrelid = 'public.users'::regclass AND a.attnum > 0 AND NOT a.attisdropped`)
	must(t, err)
	for rows.Next() {
		var n string
		var c column
		must(t, rows.Scan(&n, &c.typ, &c.notNull))
		columns[n] = c
	}
	rows.Close()
	unique := map[string]string{}
	rows, err = d.admin.Query(`SELECT c.relname, pg_catalog.pg_get_indexdef(i.indexrelid) FROM pg_catalog.pg_index i JOIN pg_catalog.pg_class c ON c.oid = i.indexrelid
WHERE i.indrelid = 'public.users'::regclass AND i.indisunique AND i.indisvalid AND NOT i.indisprimary`)
	must(t, err)
	for rows.Next() {
		var n, definition string
		must(t, rows.Scan(&n, &definition))
		unique[n] = definition
	}
	rows.Close()
	var triggers []string
	rows, err = d.admin.Query(`SELECT tgname FROM pg_catalog.pg_trigger WHERE tgrelid = 'public.users'::regclass AND NOT tgisinternal ORDER BY 1`)
	must(t, err)
	for rows.Next() {
		var n string
		must(t, rows.Scan(&n))
		triggers = append(triggers, n)
	}
	rows.Close()
	return columns, unique, triggers
}

// lockRow holds a row lock on the first user of the tenant, in key order, until the returned function releases it.
func (d *database) lockRow(tenantID string) func() {
	t := d.t
	tx, err := d.admin.Begin()
	must(t, err)
	if _, err := tx.Exec(`SELECT id FROM public.users WHERE tenant_id = $1 ORDER BY id LIMIT 1 FOR UPDATE`, tenantID); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(func() { _ = tx.Rollback() }) }
	t.Cleanup(release)
	return release
}

// until polls the condition every 100 ms until it holds, or fails the test after the time.
func until(t *testing.T, within time.Duration, what string, condition func() bool) {
	t.Helper()
	for deadline := time.Now().Add(within); !condition(); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %v", what, within)
		}
	}
}

// probeLines gives the JSON console lines of the output that name the observation.
func probeLines(output, name string) []map[string]any {
	var lines []map[string]any
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, `"`+name+`"`) {
			continue
		}
		var document map[string]any
		if json.Unmarshal([]byte(line), &document) == nil {
			lines = append(lines, document)
		}
	}
	return lines
}

func TestEvolution(t *testing.T) {
	d := prepared(t)
	owner := d.caller("u-owner", "t-00", "admin")
	org := "00000000-0000-0000-0000-000000000000"
	d.relate(owner, "organization", org, "owner")
	columnsBefore, uniqueBefore, _ := d.catalog()
	v1api := d.serve("v1")
	job := d.migrate("v2")
	jobErr := job.wait(5 * time.Minute)
	columnsAfter, uniqueAfter, triggers := d.catalog()
	v1Live := v1api.live()

	t.Run("ST-GEN-UC12-R02-T01 After the expand migration the old column is as it was and the new one is nullable", func(t *testing.T) {
		d.t = t
		if jobErr != nil {
			t.Fatalf("the migration job of v2 failed: %v\n%s", jobErr, job.out)
		}
		for name, c := range columnsBefore {
			if columnsAfter[name] != c {
				t.Errorf("the column %s was %v before the expand migration and is %v after it", name, c, columnsAfter[name])
			}
		}
		for name, definition := range uniqueBefore {
			if uniqueAfter[name] != definition {
				t.Errorf("the unique index %s was %q before the expand migration and is %q after it", name, definition, uniqueAfter[name])
			}
		}
		if uniqueBefore["users_email_key"] == "" {
			t.Errorf("v1 has no unique index users_email_key: %v", uniqueBefore)
		}
		if c, ok := columnsAfter["contact_email"]; !ok || c.notNull || c.typ != columnsBefore["email"].typ {
			t.Errorf("the new column contact_email is %v (there %v), want nullable and of the type of email", c, ok)
		}
		if !strings.Contains(uniqueAfter["users_contact_email_key"], `(tenant_id, contact_email)`) {
			t.Errorf("contact_email has no valid unique index users_contact_email_key: %v", uniqueAfter)
		}
		if strings.Join(triggers, ",") != "users_contact_email_sync" {
			t.Errorf("the users table has the triggers %v, want the one new trigger users_contact_email_sync", triggers)
		}
		if len(columnsAfter) != len(columnsBefore)+2 {
			t.Errorf("the table has the columns %v after the step, want those of v1 with contact_email and nickname", columnsAfter)
		}
	})

	t.Run("ST-GEN-UC13-R05-T01 The change is complete with its time when no row is left", func(t *testing.T) {
		d.t = t
		c := d.checkpoint()
		if !c.completed.Valid || c.moved != 10000 {
			t.Fatalf("the checkpoint of %s has %d rows moved, completed %v; want 10000 and a time", change, c.moved, c.completed)
		}
		if n := d.count(`SELECT count(*) FROM public.users WHERE contact_email IS NULL OR contact_email <> email`); n != 0 {
			t.Fatalf("%d users have no contact_email equal to email", n)
		}
		// No batch after completion: the job ended, and the checkpoint does not move.
		time.Sleep(2 * time.Second)
		if again := d.checkpoint(); again != c {
			t.Fatalf("the checkpoint moved after completion: %v, then %v", c, again)
		}
	})

	v2api := d.serve("v2")
	t.Run("ST-GEN-UC12-R04-T01 After the rename the API still names the field email", func(t *testing.T) {
		d.t = t
		created := d.send(v2api, owner, http.MethodPost, "/organizations/"+org+"/users", `{"email":"r04@acme.test","display_name":"R","role":"viewer"}`)
		if created.status != http.StatusCreated {
			t.Fatalf("a create through v2 with the field email answered %d %s, want 201", created.status, created.body)
		}
		id, _ := created.object(t)["id"].(string)
		d.relate(owner, "member", id, "owner")
		read := d.send(v2api, owner, http.MethodGet, "/organizations/"+org+"/users/"+id, "").object(t)
		if read["email"] != "r04@acme.test" || read["contact_email"] != nil {
			t.Fatalf("the read through v2 is %v, want email and no contact_email", read)
		}
	})

	t.Run("ST-GEN-UC12-FL-T The email rename starts while v1 and v2 both run on a table with rows", func(t *testing.T) {
		d.t = t
		if jobErr != nil || !v1Live {
			t.Fatalf("the migration job of v2 ended with %v and v1 is live %v, want 0 and live:\n%s", jobErr, v1Live, job.out)
		}
		create := func(a *api, email string) string {
			r := d.send(a, owner, http.MethodPost, "/organizations/"+org+"/users", `{"email":"`+email+`","display_name":"F","role":"viewer"}`)
			if r.status != http.StatusCreated {
				t.Fatalf("a create answered %d %s, want 201", r.status, r.body)
			}
			id, _ := r.object(t)["id"].(string)
			d.relate(owner, "member", id, "owner")
			return id
		}
		update := func(a *api, id, email string) {
			r := d.send(a, owner, http.MethodPut, "/organizations/"+org+"/users/"+id, `{"email":"`+email+`","display_name":"F","role":"viewer"}`)
			if r.status != http.StatusOK {
				t.Fatalf("an update answered %d %s, want 200", r.status, r.body)
			}
		}
		read := func(a *api, id string) any {
			return d.send(a, owner, http.MethodGet, "/organizations/"+org+"/users/"+id, "").object(t)["email"]
		}
		equal := func(id, want string) {
			if n := d.count(`SELECT count(*) FROM public.users WHERE id = $1::uuid AND email = $2 AND contact_email = $2`, id, want); n != 1 {
				t.Errorf("the row %s does not hold %s in both email and contact_email", id, want)
			}
		}
		a, b := create(v1api, "a@flow.test"), create(v2api, "b@flow.test")
		equal(a, "a@flow.test")
		equal(b, "b@flow.test")
		update(v2api, a, "a2@flow.test")
		update(v1api, b, "b2@flow.test")
		equal(a, "a2@flow.test")
		equal(b, "b2@flow.test")
		for _, c := range []struct {
			replica *api
			id      string
			want    string
		}{{v1api, a, "a2@flow.test"}, {v2api, a, "a2@flow.test"}, {v1api, b, "b2@flow.test"}, {v2api, b, "b2@flow.test"}} {
			if got := read(c.replica, c.id); got != c.want {
				t.Errorf("a read of %s gives the email %v, want %s", c.id, got, c.want)
			}
		}
	})
}

// The tests of the backfill run the migration job of v2 on a database of their own, each from the migration job of
// v1 and the rows of ST-GEN-TD04, with the default lease TTL of 15 s and batch size of 1,000. A row lock on the first
// user of a tenant holds a job at a known batch: the batches of the tenants before it move, and the batch of that
// tenant waits at the lock timeout and runs again.

func TestBackfillLease(t *testing.T) {
	t.Run("ST-GEN-UC13-R01-T01 Of two migration jobs only the lease holder moves rows", func(t *testing.T) {
		d := prepared(t)
		jobs, release := d.heldJobs("t-03", 2)
		until(t, time.Minute, "the holder moves the rows of t-00 to t-02", func() bool { return d.checkpoint().moved == 3000 })
		first, second := holding(t, jobs)
		holder, _ := d.leaseRow()
		// Both jobs run while the lock holds: the other waits for the lease, and the checkpoint stays.
		for end := time.Now().Add(2 * ttl); time.Now().Before(end); time.Sleep(250 * time.Millisecond) {
			if h, _ := d.leaseRow(); h != holder {
				t.Fatalf("the lease went from %s to %s while the holder ran", holder, h)
			}
			if c := d.checkpoint(); c.moved != 3000 {
				t.Fatalf("the checkpoint moved to %d while the batch of t-03 waits for the lock", c.moved)
			}
		}
		release()
		if err := first.wait(time.Minute); err != nil {
			t.Fatalf("the holder: %v\n%s", err, first.out)
		}
		if err := second.wait(2 * ttl); err != nil {
			t.Fatalf("the other job: %v\n%s", err, second.out)
		}
		c := d.checkpoint()
		if !c.completed.Valid || c.moved != 10000 {
			t.Fatalf("the checkpoint has %d rows moved, complete %v; want each of the 10000 rows once", c.moved, c.completed.Valid)
		}
		if lines := probeLines(second.out.String(), "schema.backfill.rows"); len(lines) != 0 {
			t.Fatalf("the other job, which did not hold the lease while rows were left, reports moved rows: %v", lines)
		}
	})

	t.Run("ST-GEN-UC13-R02-T01 The batch of a job whose lease expired fails as fenced", func(t *testing.T) {
		d := prepared(t)
		jobs, release := d.heldJobs("t-03", 2)
		until(t, time.Minute, "the holder moves the rows of t-00 to t-02", func() bool { return d.checkpoint().moved == 3000 })
		first, second := holding(t, jobs)
		firstHolder, _ := d.leaseRow()
		must(t, first.cmd.Process.Signal(syscall.SIGSTOP))
		until(t, 3*ttl+renew, "the other job takes the lease", func() bool { h, _ := d.leaseRow(); return h != "" && h != firstHolder })
		_, takenAt := d.leaseRow()
		release()
		if err := second.wait(time.Minute); err != nil {
			t.Fatalf("the job that took the lease: %v\n%s", err, second.out)
		}
		after := d.checkpoint()
		must(t, first.cmd.Process.Signal(syscall.SIGCONT))
		if err := first.wait(2 * time.Minute); err != nil {
			t.Fatalf("the first job ends with %v after it lost the lease, want it to find the change complete:\n%s", err, first.out)
		}
		final := d.checkpoint()
		if final != after || final.moved != 10000 || !final.completed.Valid || final.token < takenAt {
			t.Fatalf("the checkpoint was %v when the second job ended and is %v after the first one went on; want 10000 rows, complete, with a token of the second job (%d or later), and no change by the first", after, final, takenAt)
		}
		if n := d.count(`SELECT count(*) FROM public.users WHERE contact_email IS DISTINCT FROM email`); n != 0 {
			t.Fatalf("%d users have no contact_email equal to email", n)
		}
	})

	t.Run("ST-GEN-UC13-R03-T01 A batch that meets a locked row times out and leaves the checkpoint where it was", func(t *testing.T) {
		d := prepared(t)
		j, release := d.held("t-01")
		until(t, time.Minute, "the first batch moves the rows of t-00", func() bool { return d.checkpoint().moved == 1000 })
		// Two lock timeouts of 2 s: the batch of t-01 fails at each, and the checkpoint stays.
		for end := time.Now().Add(4 * time.Second); time.Now().Before(end); time.Sleep(250 * time.Millisecond) {
			if c := d.checkpoint(); c.moved != 1000 || c.completed.Valid {
				t.Fatalf("the checkpoint moved to %v while the batch waits for the locked row", c)
			}
		}
		release()
		if err := j.wait(time.Minute); err != nil {
			t.Fatalf("the job: %v\n%s", err, j.out)
		}
		if c := d.checkpoint(); c.moved != 10000 || !c.completed.Valid {
			t.Fatalf("after the lock the checkpoint is %v, want 10000 rows and complete", c)
		}
		if n := d.count(`SELECT count(*) FROM public.users WHERE contact_email IS DISTINCT FROM email`); n != 0 {
			t.Fatalf("%d users, the locked one among them, have no contact_email equal to email", n)
		}
	})

	t.Run("ST-GEN-UC13-R04-T01 A backfill that is killed between batches resumes from its checkpoint", func(t *testing.T) {
		d := prepared(t)
		first, release := d.held("t-03")
		until(t, time.Minute, "the job moves 3,000 rows", func() bool { return d.checkpoint().moved == 3000 })
		must(t, first.cmd.Process.Kill())
		_ = first.wait(10 * time.Second)
		release()
		before := d.count(`SELECT count(*) FROM public.users WHERE contact_email IS NOT NULL`)
		second := d.migrate("v2")
		if err := second.wait(2*ttl + time.Minute); err != nil {
			t.Fatalf("the second job: %v\n%s", err, second.out)
		}
		c := d.checkpoint()
		var moved float64
		for _, line := range probeLines(second.out.String(), "schema.backfill.rows") {
			if v, ok := line["value"].(float64); ok {
				moved += v
			}
		}
		if c.moved != 10000 || !c.completed.Valid || before != 3000 || moved != 7000 {
			t.Fatalf("the checkpoint has %d rows moved, complete %v; %d rows were moved before the second job, and it reports %v; want 10000, complete, 3000 and 7000", c.moved, c.completed.Valid, before, moved)
		}
	})
}

func TestBackfillProgress(t *testing.T) {
	t.Run("ST-GEN-UC13-R06-T01 Progress is counts with no row value, and a stall is a probe signal", func(t *testing.T) {
		d := prepared(t)
		j, release := d.held("t-05")
		until(t, time.Minute, "the job moves 5,000 rows", func() bool { return d.checkpoint().moved == 5000 })
		stalledFrom := time.Now()
		until(t, 3*ttl+renew+5*time.Second, "schema.backfill.stalled", func() bool { return len(probeLines(j.out.String(), "schema.backfill.stalled")) > 0 })
		if waited := time.Since(stalledFrom); waited < 3*ttl-renew {
			t.Fatalf("the stall came %v after the last batch, before three lease TTLs", waited)
		}
		// The job keeps running through a stall of four lease TTLs.
		for end := stalledFrom.Add(4 * ttl); time.Now().Before(end); time.Sleep(time.Second) {
			if !j.running() {
				t.Fatalf("the job ended during the stall: %v\n%s", j.err, j.out)
			}
		}
		release()
		if err := j.wait(time.Minute); err != nil {
			t.Fatalf("the job: %v\n%s", err, j.out)
		}
		output := j.out.String()
		if strings.Contains(output, "@seed.test") {
			t.Fatal("the output of the job holds an email value")
		}
		lines := probeLines(output, "schema.backfill.rows")
		if len(lines) == 0 {
			t.Fatalf("the job reports no schema.backfill.rows:\n%s", output)
		}
		var moved float64
		for _, line := range lines {
			if !strings.Contains(fmt.Sprint(line), change) {
				t.Errorf("the line %v does not name the change", line)
			}
			v, _ := line["value"].(float64)
			moved += v
		}
		if moved != 10000 || len(probeLines(output, "schema.backfill.stalled")) != 1 || len(probeLines(output, "schema.backfill.completed")) != 1 {
			t.Fatalf("the job reports %v moved rows, %d stalls and %d completions; want 10000, 1 and 1:\n%s", moved,
				len(probeLines(output, "schema.backfill.stalled")), len(probeLines(output, "schema.backfill.completed")), output)
		}
	})

	t.Run("ST-GEN-UC13-FL-T The email rename backfills 10,000 rows under the lease while v1 and v2 write", func(t *testing.T) {
		d := prepared(t)
		owner := d.caller("u-owner", "t-09", "admin")
		org := "00000000-0000-0000-0000-000000000009"
		d.relate(owner, "organization", org, "owner")
		v1api := d.serve("v1")
		jobs, release := d.heldJobs("t-05", 2)
		first, second := jobs[0], jobs[1]
		until(t, time.Minute, "a job moves 5,000 rows", func() bool { return d.checkpoint().moved == 5000 })
		holder, _ := d.leaseRow()
		// v1 writes while the jobs run: the sync trigger copies each write, and the backfill leaves it.
		for i := range 3 {
			r := d.send(v1api, owner, http.MethodPost, "/organizations/"+org+"/users", fmt.Sprintf(`{"email":"w%d@flow.test","display_name":"W","role":"viewer"}`, i))
			if r.status != http.StatusCreated {
				t.Fatalf("a write of v1 during the backfill answered %d %s", r.status, r.body)
			}
		}
		// The fenced path: the holder stops for longer than its lease, the other job takes the lease and resumes.
		if h, _ := d.leaseRow(); h != holder {
			t.Fatal("the lease changed hands before the stop")
		}
		stopped, _ := holding(t, jobs)
		must(t, stopped.cmd.Process.Signal(syscall.SIGSTOP))
		until(t, 3*ttl+renew, "the other job takes the lease", func() bool { h, _ := d.leaseRow(); return h != "" && h != holder })
		release()
		other := first
		if stopped == first {
			other = second
		}
		if err := other.wait(time.Minute); err != nil {
			t.Fatalf("the job that took the lease: %v\n%s", err, other.out)
		}
		must(t, stopped.cmd.Process.Signal(syscall.SIGCONT))
		if err := stopped.wait(2 * time.Minute); err != nil {
			t.Fatalf("the job that lost the lease: %v\n%s", err, stopped.out)
		}
		c := d.checkpoint()
		if c.moved != 10000 || !c.completed.Valid {
			t.Fatalf("the checkpoint is %v, want each of the 10000 seeded rows moved once and complete", c)
		}
		if n := d.count(`SELECT count(*) FROM public.users WHERE contact_email IS DISTINCT FROM email`); n != 0 {
			t.Fatalf("%d users have no contact_email equal to email", n)
		}
		var moved float64
		completed := 0
		for _, j := range []*job{first, second} {
			out := j.out.String()
			if strings.Contains(out, "@seed.test") || strings.Contains(out, "@flow.test") {
				t.Fatal("the output of a job holds an email value")
			}
			for _, line := range probeLines(out, "schema.backfill.rows") {
				v, _ := line["value"].(float64)
				moved += v
			}
			completed += len(probeLines(out, "schema.backfill.completed"))
		}
		if moved != 10000 || completed != 1 {
			t.Fatalf("the jobs report %v moved rows and %d completions, want 10000 and 1", moved, completed)
		}
	})
}
