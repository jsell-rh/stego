package authz

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/encoding/protowire"

	"stego.dev/runtime/internal/pgtest"
	"stego.dev/runtime/outbox"
)

// spiceDBImage is the pinned SpiceDB image of the local stand-in (ST-STD-LOCAL-R04): version 1.57.0 at its digest.
const spiceDBImage = "authzed/spicedb:v1.57.0@sha256:a170d372c9f6b921bb6849064df6ca3566dd5b174882c1a2c4300c2ae1d60efd"

// fixtureSchema is the schema of the tests: the resource type document with the relations owner and viewer and the
// permissions view and edit, a group whose members can be viewers, a banned relation that view excludes, and a
// caveated relation reader; and, for the tenant cases, the resource type order with a viewer relation and a folder
// parent, and the resource type report, whose viewers' delegates may view it too.
const fixtureSchema = `definition user {
	relation delegate: user
}

definition group {
	relation member: user
}

caveat has_region(region string) {
	region == "eu"
}

definition stego_floor {
	relation marker: stego_floor
}

definition document {
	relation owner: user
	relation viewer: user | group#member
	relation banned: user
	relation reader: user with has_region
	permission view = (owner + viewer) - banned
	permission edit = owner
	permission read = reader
}

definition folder {
	relation viewer: user
}

definition order {
	relation viewer: user | group#member
	relation parent: folder
	permission view = viewer + parent->viewer
}

definition report {
	relation viewer: user
	permission view = viewer + viewer->delegate
}
`

// spiceServer is one local SpiceDB container without privilege, which the first test that needs it starts and
// TestMain removes.
type spiceServer struct {
	once     sync.Once
	name     string
	endpoint string
	key      string
	err      error
	extra    []string
	network  string
}

// The tests use two servers. testing runs serve-testing, which keeps an in-memory datastore per preshared key, so
// each test gets its own datastore through a key of its own. quantized runs serve on one in-memory datastore with
// a revision quantization of quantum, so a check at least as fresh as an old token may read the state of the last
// quantization boundary, as a SpiceDB in production does: the revocation floor tests need that stale read to be
// able to fail.
var (
	testingServer   spiceServer
	quantizedServer spiceServer
	postgresServer  spiceServer
)

// quantum is the revision quantization interval of the quantized server.
const quantum = 4 * time.Second

func TestMain(m *testing.M) {
	code := pgtest.Main(m)
	for _, server := range []*spiceServer{&testingServer, &quantizedServer, &postgresServer} {
		for _, name := range append([]string{server.name}, server.extra...) {
			if name != "" {
				_ = exec.Command("docker", "rm", "-f", name).Run()
			}
		}
		if server.network != "" {
			_ = exec.Command("docker", "network", "rm", server.network).Run()
		}
	}
	os.Exit(code)
}

// needDocker fails the test when docker is not on the PATH, as pgtest does: a missing result is not a pass. Only
// STEGO_TEST_NO_CONTAINERS=1 makes it skip.
func needDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		if os.Getenv(pgtest.NoContainersVariable) == "1" {
			t.Skip("docker is not on the PATH, and STEGO_TEST_NO_CONTAINERS=1: this test needs a local SpiceDB container")
		}
		t.Fatal("docker is not on the PATH: this test needs a local SpiceDB container")
	}
}

func startContainer(args ...string) (string, string, error) {
	return startContainerOn("", args...)
}

// startContainerOn starts the SpiceDB image without privilege, on the docker network when one is given, and gives
// its name and its gRPC endpoint on 127.0.0.1.
func startContainerOn(network string, args ...string) (string, string, error) {
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return "", "", err
	}
	name := "stego-authz-spicedb-" + hex.EncodeToString(suffix)
	flags := []string{"run", "-d", "--rm", "--name", name, "--memory", "256m", "--cpus", "1",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "-p", "127.0.0.1::50051"}
	if network != "" {
		flags = append(flags, "--network", network)
	}
	run := exec.Command("docker", append(append(flags, spiceDBImage), args...)...)
	var stderr bytes.Buffer
	run.Stderr = &stderr
	if err := run.Run(); err != nil {
		return "", "", fmt.Errorf("start the SpiceDB container: %v: %s", err, stderr.String())
	}
	out, err := exec.Command("docker", "port", name, "50051/tcp").Output()
	if err != nil {
		return name, "", fmt.Errorf("read the port of the SpiceDB container: %v", err)
	}
	endpoint := strings.TrimSpace(strings.Split(string(out), "\n")[0])
	return name, endpoint, nil
}

// bearer sends the preshared key of the local server on each call.
type bearer string

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}

func (bearer) RequireTransportSecurity() bool { return false }

func randomKey(t *testing.T) string {
	t.Helper()
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(key)
}

// spiceEnv is the datastore of one test on the local SpiceDB server.
type spiceEnv struct {
	key      bearer
	conn     *grpc.ClientConn
	provider *SpiceDB
	schema   v1.SchemaServiceClient
	perms    v1.PermissionsServiceClient
}

// startSpiceDB gives the test its own datastore on the local serve-testing container, loaded with schema. It skips
// the test when docker is not on the PATH.
func startSpiceDB(t *testing.T, schema string) *spiceEnv {
	t.Helper()
	needDocker(t)
	testingServer.once.Do(func() {
		testingServer.name, testingServer.endpoint, testingServer.err = startContainer("serve-testing")
	})
	return connectSpiceDB(t, &testingServer, randomKey(t), schema)
}

// startQuantizedSpiceDB connects the test to the shared datastore of the quantized container, loaded with schema.
// Tests on it use object IDs of their own.
func startQuantizedSpiceDB(t *testing.T, schema string) *spiceEnv {
	t.Helper()
	needDocker(t)
	quantizedServer.once.Do(func() {
		quantizedServer.key = randomKey(t)
		quantizedServer.name, quantizedServer.endpoint, quantizedServer.err = startContainer("serve",
			"--grpc-preshared-key", quantizedServer.key, "--datastore-engine", "memory",
			"--datastore-revision-quantization-interval", quantum.String(), "--telemetry-endpoint", "")
	})
	return connectSpiceDB(t, &quantizedServer, quantizedServer.key, schema)
}

// startPostgresSpiceDB connects the test to SpiceDB on the PostgreSQL datastore, whose revisions are snapshots, with
// the revision quantization of quantum, so a check at an old token can read a stale state, as on the quantized server: a
// PostgreSQL container of pgtest's pinned image on a docker network of its own, the datastore migrated with the
// pinned SpiceDB image, and SpiceDB serving it, each without privilege; TestMain removes them. Tests on it use object
// IDs of their own.
func startPostgresSpiceDB(t *testing.T, schema string) *spiceEnv {
	t.Helper()
	needDocker(t)
	postgresServer.once.Do(func() {
		s := &postgresServer
		suffix := make([]byte, 6)
		if _, err := rand.Read(suffix); err != nil {
			s.err = err
			return
		}
		s.network = "stego-authz-net-" + hex.EncodeToString(suffix)
		if out, err := exec.Command("docker", "network", "create", s.network).CombinedOutput(); err != nil {
			s.err = fmt.Errorf("create the docker network: %v: %s", err, out)
			return
		}
		database := "stego-authz-spicedb-pg-" + hex.EncodeToString(suffix)
		s.extra = append(s.extra, database)
		if out, err := exec.Command("docker", "run", "-d", "--rm", "--name", database, "--network", s.network,
			"--memory", "512m", "--cpus", "1", "--tmpfs", "/var/lib/postgresql/data", "-e", "POSTGRES_PASSWORD=spicedb",
			pgtest.Image, "-c", "track_commit_timestamp=on").CombinedOutput(); err != nil {
			s.err = fmt.Errorf("start the PostgreSQL container of SpiceDB: %v: %s", err, out)
			return
		}
		uri := "postgres://postgres:spicedb@" + database + ":5432/postgres?sslmode=disable"
		deadline := time.Now().Add(2 * time.Minute)
		for {
			out, err := exec.Command("docker", "run", "--rm", "--network", s.network, "--memory", "256m", "--cap-drop", "ALL",
				"--security-opt", "no-new-privileges", spiceDBImage, "datastore", "migrate", "head",
				"--datastore-engine", "postgres", "--datastore-conn-uri", uri).CombinedOutput()
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				s.err = fmt.Errorf("migrate the SpiceDB datastore: %v: %s", err, out)
				return
			}
			time.Sleep(time.Second)
		}
		s.key = hex.EncodeToString(suffix) + "-key"
		s.name, s.endpoint, s.err = startContainerOn(s.network, "serve", "--grpc-preshared-key", s.key,
			"--datastore-engine", "postgres", "--datastore-conn-uri", uri, "--telemetry-endpoint", "",
			"--datastore-revision-quantization-interval", quantum.String())
	})
	return connectSpiceDB(t, &postgresServer, postgresServer.key, schema)
}

func connectSpiceDB(t *testing.T, server *spiceServer, key, schema string) *spiceEnv {
	t.Helper()
	if server.err != nil {
		t.Fatal(server.err)
	}
	conn, err := grpc.NewClient(server.endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(bearer(key)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	env := &spiceEnv{key: bearer(key), conn: conn, schema: v1.NewSchemaServiceClient(conn), perms: v1.NewPermissionsServiceClient(conn)}
	deadline := time.Now().Add(60 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err = env.schema.WriteSchema(ctx, &v1.WriteSchemaRequest{Schema: schema})
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("write the schema to the SpiceDB container: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	env.provider, err = NewSpiceDB(conn)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

// count reads the relationships of resourceType at the newest state.
func (e *spiceEnv) count(t *testing.T, resourceType string) int {
	t.Helper()
	stream, err := e.perms.ReadRelationships(context.Background(), &v1.ReadRelationshipsRequest{
		Consistency:        &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
		RelationshipFilter: &v1.RelationshipFilter{ResourceType: resourceType},
	})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for {
		_, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return n
		}
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
}

// zed encodes a ZedToken of SpiceDB with the revision text, as SpiceDB's DecodedZedToken v1 holds it.
func zed(revision string) string {
	inner := protowire.AppendTag(nil, 1, protowire.BytesType)
	inner = protowire.AppendString(inner, revision)
	outer := protowire.AppendTag(nil, 3, protowire.BytesType)
	outer = protowire.AppendBytes(outer, inner)
	return base64.StdEncoding.EncodeToString(outer)
}

// fakeSpiceDB is a fake SpiceDB gRPC server that records each request it gets. Like SpiceDB, it binds the
// consistency of a lookup into the cursors it gives and refuses a cursor sent with another consistency.
type fakeSpiceDB struct {
	v1.UnimplementedPermissionsServiceServer
	mu             sync.Mutex
	checks         []*v1.CheckPermissionRequest
	bulks          []*v1.CheckBulkPermissionsRequest
	writes         []*v1.WriteRelationshipsRequest
	lookups        []*v1.LookupResourcesRequest
	permissionship v1.CheckPermissionResponse_Permissionship
	missing        []string
	// fail, when set, is the error of every call.
	fail error
	// denied are the resource IDs that CheckBulkPermissions denies.
	denied  map[string]bool
	cursors map[string]string
}

func consistencyKey(c *v1.Consistency) string {
	if c.GetFullyConsistent() {
		return "fresh"
	}
	return "at_least:" + c.GetAtLeastAsFresh().GetToken()
}

func (f *fakeSpiceDB) CheckPermission(_ context.Context, request *v1.CheckPermissionRequest) (*v1.CheckPermissionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks = append(f.checks, request)
	if f.fail != nil {
		return nil, f.fail
	}
	response := &v1.CheckPermissionResponse{CheckedAt: &v1.ZedToken{Token: zed("7")}, Permissionship: f.permissionship}
	if f.permissionship == v1.CheckPermissionResponse_PERMISSIONSHIP_CONDITIONAL_PERMISSION {
		response.PartialCaveatInfo = &v1.PartialCaveatInfo{MissingRequiredContext: f.missing}
	}
	return response, nil
}

func (f *fakeSpiceDB) CheckBulkPermissions(_ context.Context, request *v1.CheckBulkPermissionsRequest) (*v1.CheckBulkPermissionsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bulks = append(f.bulks, request)
	if f.fail != nil {
		return nil, f.fail
	}
	response := &v1.CheckBulkPermissionsResponse{CheckedAt: &v1.ZedToken{Token: zed("10")}}
	for _, item := range request.GetItems() {
		permissionship := v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION
		if f.denied[item.GetResource().GetObjectId()] {
			permissionship = v1.CheckPermissionResponse_PERMISSIONSHIP_NO_PERMISSION
		}
		response.Pairs = append(response.Pairs, &v1.CheckBulkPermissionsPair{Request: item,
			Response: &v1.CheckBulkPermissionsPair_Item{Item: &v1.CheckBulkPermissionsResponseItem{Permissionship: permissionship}}})
	}
	return response, nil
}

func (f *fakeSpiceDB) WriteRelationships(_ context.Context, request *v1.WriteRelationshipsRequest) (*v1.WriteRelationshipsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, request)
	if f.fail != nil {
		return nil, f.fail
	}
	return &v1.WriteRelationshipsResponse{WrittenAt: &v1.ZedToken{Token: zed("8")}}, nil
}

// LookupResources answers four resources, d1 to d4, in pages of the request's limit, after the cursor it gets.
func (f *fakeSpiceDB) LookupResources(request *v1.LookupResourcesRequest, stream grpc.ServerStreamingServer[v1.LookupResourcesResponse]) error {
	f.mu.Lock()
	f.lookups = append(f.lookups, request)
	if f.cursors == nil {
		f.cursors = map[string]string{}
	}
	start := 0
	if cursor := request.GetOptionalCursor().GetToken(); cursor != "" {
		if bound, ok := f.cursors[cursor]; !ok || bound != consistencyKey(request.GetConsistency()) {
			f.mu.Unlock()
			return status.Error(codes.InvalidArgument, "the cursor was made for another consistency")
		}
		if _, err := fmt.Sscanf(cursor, "after-%d", &start); err != nil {
			f.mu.Unlock()
			return err
		}
	}
	key := consistencyKey(request.GetConsistency())
	f.mu.Unlock()
	for i := start; i < 4 && (request.GetOptionalLimit() == 0 || i-start < int(request.GetOptionalLimit())); i++ {
		cursor := fmt.Sprintf("after-%d", i+1)
		f.mu.Lock()
		f.cursors[cursor] = key
		f.mu.Unlock()
		if err := stream.Send(&v1.LookupResourcesResponse{
			LookedUpAt:        &v1.ZedToken{Token: zed("9")},
			ResourceObjectId:  fmt.Sprintf("d%d", i+1),
			Permissionship:    v1.LookupPermissionship_LOOKUP_PERMISSIONSHIP_HAS_PERMISSION,
			AfterResultCursor: &v1.Cursor{Token: cursor},
		}); err != nil {
			return err
		}
	}
	return nil
}

func startFake(t *testing.T) (*fakeSpiceDB, *SpiceDB) {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	fake := &fakeSpiceDB{permissionship: v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION}
	v1.RegisterPermissionsServiceServer(server, fake)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	provider, err := NewSpiceDB(conn)
	if err != nil {
		t.Fatal(err)
	}
	return fake, provider
}

func TestSpiceDBProvider(t *testing.T) {
	ctx := context.Background()
	viewer := Tuple{Resource: doc("1"), Relation: "viewer", Subject: user("bob")}

	t.Run("ST-SUP-RP06-LC05-T01 The SpiceDB provider evaluates over gRPC", func(t *testing.T) {
		db := startSpiceDB(t, fixtureSchema)
		written, err := db.provider.Touch(ctx, []Tuple{viewer}, nil)
		if err != nil {
			t.Fatal(err)
		}
		result, err := db.provider.Evaluate(ctx, EvaluationRequest{Subject: user("bob"), Action: Action{Name: "view"},
			Resource: doc("1"), Consistency: AtLeast(written)})
		if err != nil {
			t.Fatal(err)
		}
		if !result.Allowed || result.Token.IsZero() {
			t.Fatalf("result %+v, want an allow from the server with its token", result)
		}
		// The provider holds only the gRPC client of the connection it was given: it opens no transport of its own.
		if got := exportedFields(reflectType(db.provider)); len(got) != 0 {
			t.Errorf("the provider has the exported fields %v", got)
		}
	})

	t.Run("ST-SUP-RP06-LC05-T02 Fresh maps to fully_consistent and at-least maps to at_least_as_fresh", func(t *testing.T) {
		fake, provider := startFake(t)
		token, err := NewToken(zed("5"))
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range []Consistency{Fresh(), AtLeast(token)} {
			if _, err := provider.Evaluate(ctx, EvaluationRequest{Subject: user("bob"), Action: Action{Name: "view"},
				Resource: doc("1"), Consistency: c}); err != nil {
				t.Fatal(err)
			}
		}
		if len(fake.checks) != 2 {
			t.Fatalf("the server got %d checks", len(fake.checks))
		}
		if !fake.checks[0].GetConsistency().GetFullyConsistent() {
			t.Errorf("Fresh sent %v", fake.checks[0].GetConsistency())
		}
		if got := fake.checks[1].GetConsistency().GetAtLeastAsFresh().GetToken(); got != token.Value() {
			t.Errorf("AtLeast sent %v, want at_least_as_fresh with the ZedToken %s", fake.checks[1].GetConsistency(), token.Value())
		}
	})

	t.Run("ST-SUP-RP06-LC05-T03 The provider writes with TOUCH and DELETE only", func(t *testing.T) {
		fake, provider := startFake(t)
		if _, err := provider.Touch(ctx, []Tuple{viewer}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := provider.Delete(ctx, []Filter{FilterOf(viewer)}, nil); err != nil {
			t.Fatal(err)
		}
		if len(fake.writes) != 2 {
			t.Fatalf("the server got %d writes, want 2", len(fake.writes))
		}
		want := []v1.RelationshipUpdate_Operation{v1.RelationshipUpdate_OPERATION_TOUCH, v1.RelationshipUpdate_OPERATION_DELETE}
		for i, write := range fake.writes {
			for _, update := range write.GetUpdates() {
				if update.GetOperation() == v1.RelationshipUpdate_OPERATION_CREATE || update.GetOperation() != want[i] {
					t.Errorf("write %d sent %s, want %s", i, update.GetOperation(), want[i])
				}
			}
		}
	})

	t.Run("ST-SUP-RP06-LC05-T04 A search page token keeps the first page's consistency, and later pages are filtered at the floor", func(t *testing.T) {
		fake, provider := startFake(t)
		fake.denied = map[string]bool{"d4": true}
		floorFirst, err := NewToken(zed("1"))
		if err != nil {
			t.Fatal(err)
		}
		floorLater, err := NewToken(zed("6"))
		if err != nil {
			t.Fatal(err)
		}
		request := SearchRequest{Subject: user("bob"), Action: Action{Name: "view"}, ResourceType: "document",
			Consistency: AtLeast(floorFirst), Page: Page{Limit: 2}}
		first, err := provider.Search(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		if len(first.Resources) != 2 || first.Next.IsZero() || len(fake.bulks) != 0 {
			t.Fatalf("the first page is %+v with %d bulk calls", first, len(fake.bulks))
		}
		// The floor rises from t-1 to t-6 between the pages.
		request.Consistency = AtLeast(floorLater)
		request.Page.Token = first.Next
		second, err := provider.Search(ctx, request)
		if err != nil {
			t.Fatalf("the second page: %v", err)
		}
		lookup := fake.lookups[1]
		if lookup.GetOptionalCursor().GetToken() != "after-2" || lookup.GetConsistency().GetAtLeastAsFresh().GetToken() != floorFirst.Value() {
			t.Fatalf("the second lookup carries %v and %v, want after-2 and the first page's consistency",
				lookup.GetOptionalCursor(), lookup.GetConsistency())
		}
		if len(fake.bulks) != 1 || fake.bulks[0].GetConsistency().GetAtLeastAsFresh().GetToken() != floorLater.Value() {
			t.Fatalf("the bulk calls are %v, want one at least as fresh as t-6", fake.bulks)
		}
		if len(second.Resources) != 1 || second.Resources[0].ID != "d3" {
			t.Fatalf("the second page is %+v, want d3 only, which the bulk check allows", second.Resources)
		}
		// The fake refuses a cursor sent with another consistency, as SpiceDB does.
		request.Page.Token = NewPageToken(spiceDBName, "after-2", AtLeast(floorLater))
		if _, err := provider.Search(ctx, request); err == nil {
			t.Fatal("the fake took a cursor with another consistency")
		}
	})

	t.Run("ST-SUP-RP06-LC05-T05 The provider never sends at_exact_snapshot", func(t *testing.T) {
		source, err := os.ReadFile("spicedb.go")
		if err != nil {
			t.Fatal(err)
		}
		// The comments say that the option does not exist; the code must not name it.
		fset, files := packageSource(t)
		for _, file := range files {
			if fset.Position(file.Pos()).Filename != "spicedb.go" {
				continue
			}
			identifiers, literals := identifiersAndStrings(t, append(files[:0:0], file))
			for _, word := range append(identifiers, literals...) {
				if strings.Contains(strings.ToLower(word), "exactsnapshot") || strings.Contains(word, "at_exact_snapshot") {
					t.Errorf("spicedb.go names %s", word)
				}
			}
		}
		if !bytes.Contains(source, []byte("FullyConsistent")) {
			t.Fatal("the scan did not read the provider source")
		}
		fake, provider := startFake(t)
		for _, c := range []Consistency{Fresh(), AtLeast(tok(3)), {}} {
			_, _ = provider.Evaluate(ctx, EvaluationRequest{Subject: user("bob"), Action: Action{Name: "view"},
				Resource: doc("1"), Consistency: c})
		}
		for _, check := range fake.checks {
			if check.GetConsistency().GetAtExactSnapshot() != nil || check.GetConsistency().GetMinimizeLatency() {
				t.Errorf("a request holds %v", check.GetConsistency())
			}
		}
		if len(fake.checks) != 2 {
			t.Errorf("the server got %d checks, want 2: the zero consistency must not reach it", len(fake.checks))
		}
	})

	t.Run("ST-SUP-RP06-LC08-T02 A conditional permission is an error, not a deny", func(t *testing.T) {
		fake, provider := startFake(t)
		fake.permissionship = v1.CheckPermissionResponse_PERMISSIONSHIP_CONDITIONAL_PERMISSION
		fake.missing = []string{"region"}
		result, err := provider.Evaluate(ctx, EvaluationRequest{Subject: user("bob"), Action: Action{Name: "read"},
			Resource: doc("1"), Consistency: Fresh()})
		var contextErr *ContextError
		if !errors.As(err, &contextErr) || !strings.Contains(err.Error(), "region") {
			t.Fatalf("a conditional permission gave %+v, %v; want an error that names the missing context", result, err)
		}
		if result != (EvaluationResult{}) {
			t.Fatalf("a conditional permission gave the decision %+v", result)
		}
	})

	t.Run("ST-SUP-RP06-LC08-T03 A missing caveat field is an error, not a deny", func(t *testing.T) {
		db := startSpiceDB(t, fixtureSchema)
		response, err := db.perms.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{Resource: spiceObject("document", doc("1").ID), Relation: "reader",
				Subject:        spiceSubject(user("bob"), ""),
				OptionalCaveat: &v1.ContextualizedCaveat{CaveatName: "has_region"}},
		}}})
		if err != nil {
			t.Fatal(err)
		}
		written, err := tokenOf(response.GetWrittenAt())
		if err != nil {
			t.Fatal(err)
		}
		request := EvaluationRequest{Subject: user("bob"), Action: Action{Name: "read"}, Resource: doc("1"),
			Context: NewContext([]string{"region"}, map[string]any{}), Consistency: AtLeast(written)}
		_, err = db.provider.Evaluate(ctx, request)
		var contextErr *ContextError
		if !errors.As(err, &contextErr) || !strings.Contains(err.Error(), "region") {
			t.Fatalf("a missing caveat field gave %v, want an error that names region", err)
		}
		// With the field the caveat decides.
		request.Context = NewContext([]string{"region"}, map[string]any{"region": "eu"})
		result, err := db.provider.Evaluate(ctx, request)
		if err != nil || !result.Allowed {
			t.Fatalf("with the region the check gave %+v, %v", result, err)
		}
	})

	t.Run("ST-SUP-RP06-LC20-T01 A provider error leaks no subject, caveat value or provider message", func(t *testing.T) {
		fake, provider := startFake(t)
		fake.fail = status.Error(codes.InvalidArgument, "could not parse ip address string 'not-an-ip-198.51.100.77-secret'")
		leaks := func(t *testing.T, err error) {
			t.Helper()
			if err == nil {
				t.Fatal("no error")
			}
			if !IsRefused(err) || !errors.Is(err, ErrProvider) {
				t.Errorf("the error %q is not marked refused", err)
			}
			for chain := []error{err}; len(chain) > 0; {
				next := chain[0]
				chain = chain[1:]
				for _, bad := range []string{"198.51.100.77", "secret", "user:bob", "not-an-ip"} {
					if strings.Contains(next.Error(), bad) {
						t.Errorf("the error %q holds %s", next.Error(), bad)
					}
				}
				switch e := next.(type) {
				case interface{ Unwrap() error }:
					if inner := e.Unwrap(); inner != nil {
						chain = append(chain, inner)
					}
				case interface{ Unwrap() []error }:
					chain = append(chain, e.Unwrap()...)
				}
			}
		}
		_, err := provider.Evaluate(ctx, EvaluationRequest{Subject: user("bob"), Action: Action{Name: "view"},
			Resource: doc("1"), Context: Context{"ip": "not-an-ip-198.51.100.77-secret"}, Consistency: Fresh()})
		leaks(t, err)
		relay, err := NewRelay(RelayConfig{Name: "authz-relay", Relationships: provider, DB: fakeDB(t), Schema: "public"})
		if err != nil {
			t.Fatal(err)
		}
		delivery := outbox.Delivery{Message: outbox.Message{ID: uuid.New(), Destination: DestinationName, ResourceKey: "document:1",
			Payload: json.RawMessage(`{"v":2,"op":"touch","revocation":"` + uuid.NewString() +
				`","tuple":{"resource_type":"document","resource_id":"` + doc("1").ID + `","relation":"viewer","subject_type":"user","subject_id":"bob"}}`)},
			Tenant: mustTenant(t, "t1")}
		err = relay.Deliver(ctx, floorTx(t, &floorSeq{}), delivery)
		leaks(t, err)
		if !strings.Contains(err.Error(), doc("1").String()+"#viewer") {
			t.Errorf("the park record %q does not name %s#viewer", err, doc("1"))
		}
		// A context error stays readable by kind, for a provider call and for a floor read.
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		fake.fail = nil
		_, err = provider.Evaluate(canceled, EvaluationRequest{Subject: user("bob"), Action: Action{Name: "view"},
			Resource: doc("1"), Consistency: Fresh()})
		if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrProvider) {
			t.Errorf("a canceled call gave %v, want context.Canceled and a provider error", err)
		}
		expired, stop := context.WithDeadline(ctx, time.Now().Add(-time.Second))
		defer stop()
		_, err = provider.Evaluate(expired, EvaluationRequest{Subject: user("bob"), Action: Action{Name: "view"},
			Resource: doc("1"), Consistency: Fresh()})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("a call past its deadline gave %v, want context.DeadlineExceeded", err)
		}
		floor, err := NewTableFloor(fakeDB(t), "public")
		if err != nil {
			t.Fatal(err)
		}
		port, err := NewPort(provider, floor)
		if err != nil {
			t.Fatal(err)
		}
		_, err = port.Evaluate(canceled, EvaluationRequest{Subject: user("bob"), Action: Action{Name: "view"},
			Resource: doc("1"), Consistency: Fresh()})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("a canceled floor read gave %v, want context.Canceled", err)
		}
		// A validation error names no ID either.
		_, err = provider.Touch(ctx, []Tuple{{Resource: doc("1"), Relation: "viewer", Subject: Subject{Type: "user", ID: "bob secret"}}}, nil)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Errorf("a bad subject ID gave %v", err)
		}
	})
}
