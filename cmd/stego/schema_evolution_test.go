package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// evolutionFixture is the folder of the TD04 harness: the seed of the database and the check that runs the service
// binaries of the releases on it.
const evolutionFixture = "testdata/schema_evolution"

// evolutionSpiceDBImage is the pinned SpiceDB image of the harness.
const evolutionSpiceDBImage = "authzed/spicedb:v1.57.0@sha256:a170d372c9f6b921bb6849064df6ca3566dd5b174882c1a2c4300c2ae1d60efd"

// evolution is ST-GEN-TD04: a git repository with the commits of examples/user-management at each release, the
// service binary of each release, and the folder of the last release, where the check runs.
type evolution struct {
	repo, bin string
	env       []string
	plans     map[string]string
}

// The declaration of v2 of ST-GEN-TD04, by the edits of the declaration of v1: the add of User.nickname and the
// rename of User.email to contact_email, each at expand. The other kinds of ST-GEN-TD04 v2 wait for their builds
// (interim scope of the harness, accepted by the orchestrator, 2026-11-13).
var evolutionV2 = [][2]string{
	{"      - { name: email, type: string, unique: true, max_length: 255 }\n",
		"      - { name: contact_email, type: string, unique: true, max_length: 255 }\n      - { name: nickname, type: string, optional: true, max_length: 50 }\n"},
	{"\ncollections:\n", "\nschema_changes:\n  - {id: user-nickname-add, kind: add, entity: User, field: nickname}\n" +
		"  - {id: user-email-rename, kind: rename, entity: User, field: contact_email, from: email}\n" +
		"schema_history:\n  - {change: user-nickname-add, phase: expand}\n  - {change: user-email-rename, phase: expand}\n\ncollections:\n"},
}

// newEvolution makes the repository of ST-GEN-TD04 with the commit v1, the example as it is, and the commit v2,
// which stego plan --base v1 and stego apply make from the declaration of v2, and builds the service binary of each
// with the pinned Go against the runtime module of this repository.
func newEvolution(t *testing.T) *evolution {
	t.Helper()
	stego := stegoBinary(t)
	top, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatal(err)
	}
	root := strings.TrimSpace(string(top))
	parent := t.TempDir()
	e := &evolution{repo: filepath.Join(parent, "user-management"), bin: filepath.Join(parent, "bin"), plans: map[string]string{}}
	home := filepath.Join(parent, ".home")
	// The module cache stays the one of the test run, so no build downloads into the temporary home.
	cache, err := exec.Command(pinnedGo(t), "env", "GOMODCACHE").Output()
	if err != nil {
		t.Fatal(err)
	}
	e.env = append(os.Environ(), "HOME="+home, "GOMODCACHE="+strings.TrimSpace(string(cache)), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GOWORK=off", "GOTOOLCHAIN=local",
		"STEGO_REGISTRY="+filepath.Join(root, "registry"), "PATH="+filepath.Dir(pinnedGo(t))+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GIT_AUTHOR_NAME=td04", "GIT_AUTHOR_EMAIL=td04@example.test", "GIT_COMMITTER_NAME=td04", "GIT_COMMITTER_EMAIL=td04@example.test")
	if output, err := exec.Command("cp", "-a", filepath.Join(root, "examples", "user-management"), e.repo).CombinedOutput(); err != nil {
		t.Fatalf("copy the example: %v\n%s", err, output)
	}
	e.git(t, "init", "-q")
	e.git(t, "add", "-A")
	e.git(t, "commit", "-q", "-m", "v1")
	e.git(t, "tag", "v1")
	e.build(t, "v1", root)
	declaration, err := os.ReadFile(filepath.Join(e.repo, "service.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	v2 := string(declaration)
	for _, edit := range evolutionV2 {
		if !strings.Contains(v2, edit[0]) {
			t.Fatalf("the declaration of the example has no %q to edit for v2", edit[0])
		}
		v2 = strings.Replace(v2, edit[0], edit[1], 1)
	}
	if err := os.WriteFile(filepath.Join(e.repo, "service.yaml"), []byte(v2), 0o644); err != nil {
		t.Fatal(err)
	}
	e.plans["v2 plan"], e.plans["v2 plan error"] = e.stego(t, stego, "plan", "--base", "v1")
	e.plans["v2 apply"], e.plans["v2 apply error"] = e.stego(t, stego, "apply", "--base", "v1")
	if e.plans["v2 apply error"] != "" {
		t.Fatalf("stego apply of v2: %s\n%s", e.plans["v2 apply error"], e.plans["v2 apply"])
	}
	e.git(t, "add", "-A")
	e.git(t, "commit", "-q", "-m", "v2")
	e.git(t, "tag", "v2")
	e.build(t, "v2", root)
	// A head that renames email with no schema change: plan refuses it and writes nothing.
	head := strings.Replace(string(declaration), evolutionV2[0][0], strings.Replace(evolutionV2[0][0], "email", "contact_email", 1), 1)
	if err := os.WriteFile(filepath.Join(e.repo, "service.yaml"), []byte(head), 0o644); err != nil {
		t.Fatal(err)
	}
	e.plans["undeclared plan"], e.plans["undeclared plan error"] = e.stego(t, stego, "plan", "--base", "v1")
	e.plans["undeclared status"] = e.git(t, "status", "--porcelain", "--untracked-files=all")
	e.git(t, "checkout", "-q", "--", "service.yaml")
	return e
}

// git runs git in the repository and gives its output.
func (e *evolution) git(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = e.repo, e.env
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

// stego runs the stego binary in the repository and gives its output and its error, or "".
func (e *evolution) stego(t *testing.T, stego string, args ...string) (string, string) {
	t.Helper()
	cmd := exec.Command(stego, args...)
	cmd.Dir, cmd.Env = e.repo, e.env
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), err.Error()
	}
	return string(output), ""
}

// build builds the service binary of the release at the tag into bin/<tag>, from a copy of the commit, against the
// runtime module of this repository, as each commit of ST-GEN-TD04 uses the registry at the commit under test.
func (e *evolution) build(t *testing.T, tag, root string) {
	t.Helper()
	dir := filepath.Join(filepath.Dir(e.repo), "build-"+tag)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	archive := exec.Command("sh", "-c", `git archive "$1" | tar -x -C "$2"`, "sh", tag, dir)
	archive.Dir, archive.Env = e.repo, e.env
	if output, err := archive.CombinedOutput(); err != nil {
		t.Fatalf("check out %s: %v\n%s", tag, err, output)
	}
	for _, args := range [][]string{
		{"mod", "edit", "-replace", "github.com/jsell-rh/stego/registry/runtime=" + filepath.Join(root, "registry", "runtime")},
		{"mod", "tidy"},
		{"build", "-o", filepath.Join(e.bin, tag), "./out"},
	} {
		cmd := exec.Command(pinnedGo(t), args...)
		cmd.Dir, cmd.Env = dir, e.env
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go %s for %s: %v\n%s", strings.Join(args, " "), tag, err, output)
		}
	}
}

// startEvolutionSpiceDB starts a local SpiceDB container, unprivileged, with the memory datastore, and gives its
// address and preshared key. The container is removed when the test ends.
func startEvolutionSpiceDB(t *testing.T) (string, string) {
	t.Helper()
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	name, key := "stego-td04-spicedb-"+hex.EncodeToString(suffix), "td04-"+hex.EncodeToString(suffix)
	run := exec.Command("docker", "run", "-d", "--rm", "--name", name, "--memory", "512m", "--cpus", "1",
		"--security-opt", "no-new-privileges", "--cap-drop", "ALL", "--stop-timeout", "1",
		"-p", "127.0.0.1::50051", evolutionSpiceDBImage, "serve", "--grpc-preshared-key", key, "--datastore-engine", "memory")
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("docker run SpiceDB: %v\n%s", err, output)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		out, err := exec.Command("docker", "port", name, "50051/tcp").Output()
		if err == nil && len(out) > 0 {
			return strings.TrimSpace(strings.Split(string(out), "\n")[0]), key
		}
	}
	t.Fatal("the SpiceDB container publishes no port")
	return "", ""
}

// userSchema gives the schema of User in the OpenAPI document of the release at the tag.
func (e *evolution) userSchema(t *testing.T, tag string) map[string]any {
	t.Helper()
	var document struct {
		Components struct {
			Schemas map[string]map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal([]byte(e.git(t, "show", tag+":out/internal/api/openapi.json")), &document); err != nil {
		t.Fatal(err)
	}
	schema, ok := document.Components.Schemas["User"]
	if !ok {
		t.Fatalf("the OpenAPI document of %s has no schema User", tag)
	}
	return schema
}

// TestSchemaEvolution runs ST-GEN-TD04: it makes the repository with the releases v1 and v2, builds their service
// binaries, and runs the check of testdata/schema_evolution/check in the module of v2 once, against the local
// PostgreSQL server of STEGO_TEST_POSTGRES_DSN and a local SpiceDB container. A missing server fails the test: a
// missing result is not a pass.
func TestSchemaEvolution(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the releases of the example user-management")
	}
	dsn := os.Getenv("STEGO_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Fatal("STEGO_TEST_POSTGRES_DSN is not set: this test needs a local PostgreSQL server")
	}
	e := newEvolution(t)
	check, err := os.ReadFile(filepath.Join(evolutionFixture, "check", "evolution_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	seed, err := filepath.Abs(filepath.Join(evolutionFixture, "seed.sql"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(filepath.Dir(e.repo), "build-v2")
	if err := os.MkdirAll(filepath.Join(dir, "out", "evolutioncheck"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "out", "evolutioncheck", "evolution_test.go"), check, 0o644); err != nil {
		t.Fatal(err)
	}
	endpoint, key := startEvolutionSpiceDB(t)
	// STEGO_TD04_CHECK_RUN narrows the check to the tests it names, for a run by hand; CI runs every check.
	args := []string{"test", "-count=1", "-timeout=1500s", "-v"}
	if only := os.Getenv("STEGO_TD04_CHECK_RUN"); only != "" {
		args = append(args, "-run", only)
	}
	run := exec.Command(pinnedGo(t), append(args, "./out/evolutioncheck")...)
	run.Dir = dir
	run.Env = append(e.env, "STEGO_TEST_SPICEDB="+endpoint, "STEGO_TEST_SPICEDB_KEY="+key, "STEGO_TD04_SEED="+seed,
		"STEGO_TD04_V1="+filepath.Join(e.bin, "v1"), "STEGO_TD04_V2="+filepath.Join(e.bin, "v2"))
	var output bytes.Buffer
	run.Stdout, run.Stderr = &output, &output
	runErr := run.Run()
	passed := func(id string) bool {
		for _, line := range strings.Split(output.String(), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "--- PASS: ") && strings.Contains(line, "/"+id+"_") {
				return true
			}
		}
		return false
	}
	result := func(t *testing.T, id string) {
		t.Helper()
		if !passed(id) {
			t.Fatalf("the check %s did not run and pass (go test: %v):\n%s", id, runErr, output.String())
		}
	}

	t.Run("ST-GEN-UC12-R02-T01 After the expand migration the old column is as it was and the new one is nullable", func(t *testing.T) {
		result(t, "ST-GEN-UC12-R02-T01")
	})
	t.Run("ST-GEN-UC12-R04-T01 After the rename the API still names the field email", func(t *testing.T) {
		result(t, "ST-GEN-UC12-R04-T01")
		// The User schema of v2 is the one of v1 with the added field nickname, and holds email and no
		// contact_email.
		before, after := e.userSchema(t, "v1"), e.userSchema(t, "v2")
		properties := func(schema map[string]any) map[string]any {
			p, _ := schema["properties"].(map[string]any)
			return p
		}
		got := properties(after)
		if _, ok := got["contact_email"]; ok || got["email"] == nil || got["nickname"] == nil {
			t.Fatalf("the User schema of v2 has the properties %v, want email and nickname and no contact_email", got)
		}
		trimmed := map[string]any{}
		for k, v := range got {
			if k != "nickname" {
				trimmed[k] = v
			}
		}
		if !reflect.DeepEqual(trimmed, properties(before)) || !reflect.DeepEqual(after["required"], before["required"]) {
			t.Fatalf("the User schema of v2 is not the one of v1 with nickname:\n v1 %v\n v2 %v", before, after)
		}
	})
	t.Run("ST-GEN-UC12-FL-T The email rename starts while v1 and v2 both run on a table with rows", func(t *testing.T) {
		plan := e.plans["v2 plan"]
		if e.plans["v2 plan error"] != "" || !strings.Contains(plan, "user-email-rename: rename User.contact_email at expand; migration internal/storage/migrations/step_0002_user-email-rename_expand.sql; trigger users_contact_email_sync; backfill in the migration job under the lease stego.backfill.user-email-rename") {
			t.Fatalf("stego plan --base v1 of v2 (%s) does not list the rename with its kind, phase, migration, trigger and backfill:\n%s", e.plans["v2 plan error"], plan)
		}
		if refused := e.plans["undeclared plan"]; e.plans["undeclared plan error"] == "" || !strings.Contains(refused, "rename") || e.plans["undeclared status"] != " M service.yaml\n" {
			t.Fatalf("the plan of a head that renames email with no schema change exits with %q, leaves %q, and says:\n%s\nwant a refusal that names a schema change of kind rename, and nothing written",
				e.plans["undeclared plan error"], e.plans["undeclared status"], refused)
		}
		result(t, "ST-GEN-UC12-FL-T")
	})
	t.Run("ST-GEN-UC13-R01-T01 Of two migration jobs only the lease holder moves rows", func(t *testing.T) { result(t, "ST-GEN-UC13-R01-T01") })
	t.Run("ST-GEN-UC13-R02-T01 The batch of a job whose lease expired fails as fenced", func(t *testing.T) { result(t, "ST-GEN-UC13-R02-T01") })
	t.Run("ST-GEN-UC13-R03-T01 A batch that meets a locked row times out and leaves the checkpoint where it was", func(t *testing.T) {
		result(t, "ST-GEN-UC13-R03-T01")
	})
	t.Run("ST-GEN-UC13-R04-T01 A backfill that is killed between batches resumes from its checkpoint", func(t *testing.T) {
		result(t, "ST-GEN-UC13-R04-T01")
	})
	t.Run("ST-GEN-UC13-R05-T01 The change is complete with its time when no row is left", func(t *testing.T) { result(t, "ST-GEN-UC13-R05-T01") })
	t.Run("ST-GEN-UC13-R06-T01 Progress is counts with no row value, and a stall is a probe signal", func(t *testing.T) {
		result(t, "ST-GEN-UC13-R06-T01")
	})
	t.Run("ST-GEN-UC13-FL-T The email rename backfills 10,000 rows under the lease while v1 and v2 write", func(t *testing.T) {
		result(t, "ST-GEN-UC13-FL-T")
	})
}
