package oidcauth

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strings"
	"testing"
	"time"
)

// The end-to-end proof of oidc-auth and of rh-sso-auth as its configuration: the stego binary of this commit
// generates the service of internal/compiler/testdata/runtime_wiring/project on the runtime data path with each auth
// provider, the test reads the generated files, builds the service with the pinned Go, and runs the check of
// testdata/oidc_test.go in its module, which starts the service against a local PostgreSQL server, a local SpiceDB
// container and a local OpenID Connect provider (ST-STD-LOCAL).

// shopModule is the module of the generated service, which the fills of the fixture import.
const shopModule = "example.com/shop"

// fixture is the project of the runtime wiring test, which the generated service reuses.
var fixture = filepath.Join("..", "..", "compiler", "testdata", "runtime_wiring", "project")

// pinnedGo is the Go command that builds the stego binary and the service: STEGO_TEST_PINNED_GO, else bin/go of
// the SDK that runs the test.
func pinnedGo() string {
	if name := os.Getenv("STEGO_TEST_PINNED_GO"); name != "" {
		return name
	}
	return filepath.Join(goruntime.GOROOT(), "bin", "go")
}

// goEnv is the environment of a go command and of the stego binary: no workspace, the pinned Go first on the path,
// and no toolchain switch.
func goEnv(extra ...string) []string {
	return append(append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local",
		"PATH="+filepath.Dir(pinnedGo())+string(os.PathListSeparator)+os.Getenv("PATH")), extra...)
}

// command runs a command in dir with env and fails the test when it fails.
func command(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env = dir, env
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", filepath.Base(name), strings.Join(args, " "), err, output)
	}
	return string(output)
}

// runtimeComponents is the components list of the rest-crud archetype on the runtime data path.
const runtimeComponents = "components:\n  - rest-api\n  - postgres-adapter\n  - tenancy\n  - domain-probe\n  - command\n  - query\n  - outbox\n  - otel-tracing\n  - health-check\n"

// project is one generated service: its folder, the stego binary and the address of its provider.
type project struct {
	t       *testing.T
	dir     string
	stego   string
	address string
}

// newProject copies the registry of this repository, with the components of the runtime data path in rest-crud and
// each alpha component of the test staged as ga; then the fixture project, whose declaration signs in with the
// provider and names the issuer at the address of a free loopback port.
func newProject(t *testing.T, stego, provider string) *project {
	t.Helper()
	p := &project{t: t, dir: t.TempDir(), stego: stego, address: "127.0.0.1:" + freePort(t)}
	registry := filepath.Join(p.dir, "registry")
	if err := os.CopyFS(registry, os.DirFS(filepath.Join("..", "..", "..", "registry"))); err != nil {
		t.Fatal(err)
	}
	archetype := filepath.Join(registry, "archetypes", "rest-crud", "archetype.yaml")
	p.write(archetype, regexp.MustCompile(`(?s)components:\n(  - [a-z-]+\n)+`).ReplaceAllString(p.read(archetype), runtimeComponents))
	sunset := regexp.MustCompile(`sunset: "?[0-9-]+"?\n`)
	for _, name := range []string{"tenancy", "command", "query", "domain-probe", "audit-log", "idempotency", "spicedb-authz"} {
		file := filepath.Join(registry, "components", name, "component.yaml")
		p.write(file, sunset.ReplaceAllString(strings.Replace(p.read(file), "stage: alpha\n", "stage: ga\n", 1), ""))
	}
	if err := os.CopyFS(p.dir, os.DirFS(fixture)); err != nil {
		t.Fatal(err)
	}
	service := filepath.Join(p.dir, "service.yaml")
	text := p.read(service)
	auth := "  auth-provider: oidc-auth\n  oidc-auth:\n    issuer: https://" + p.address + "/realms/shop\n    audience: orders-api\n    algorithms: [RS256, ES256]\n"
	name := "oidc-shop"
	if provider == "rh-sso-auth" {
		auth = "  auth-provider: rh-sso-auth\n  rh-sso-auth:\n    issuer: https://" + p.address + "/auth/realms/redhat-external\n    audience: orders-api\n"
		name = "sso-shop"
	}
	for old, replacement := range map[string]string{
		"  jwt-auth:\n    issuer: https://issuer.wiring.test/realms/r1\n    audience: wiring-shop\n": auth,
		"name: wiring-shop\n": "name: " + name + "\n",
		"  claim: tenant\n":   "  claim: org_id\n",
	} {
		if !strings.Contains(text, old) {
			t.Fatalf("the fixture declaration holds no %q", old)
		}
		text = strings.Replace(text, old, replacement, 1)
	}
	p.write(service, text)
	p.write(filepath.Join(p.dir, "go.mod"), "module "+shopModule+"\n\ngo 1.26.11\n")
	// The project is a repository whose branch main holds it with no record, so plan and apply have a base, the
	// default branch, at which no release has run (ST-GEN-UC12-R05).
	command(t, p.dir, os.Environ(), "git", "init", "-q", "-b", "main")
	command(t, p.dir, os.Environ(), "git", "add", "-A")
	command(t, p.dir, os.Environ(), "git", "-c", "user.name=stego-test", "-c", "user.email=stego-test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "project")
	return p
}

func (p *project) write(name, text string) {
	p.t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(text), 0o644); err != nil {
		p.t.Fatal(err)
	}
}

func (p *project) read(name string) string {
	p.t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		p.t.Fatal(err)
	}
	return string(data)
}

// run runs the stego binary in the project with its registry copy, and gives its output and its error.
func (p *project) run(args ...string) (string, error) {
	cmd := exec.Command(p.stego, args...)
	cmd.Dir = p.dir
	cmd.Env = goEnv("STEGO_REGISTRY="+filepath.Join(p.dir, "registry"), "STEGO_MODULE="+shopModule)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// generated gives the text of every generated Go file of the service, by its path under out.
func (p *project) generated() map[string]string {
	p.t.Helper()
	files := map[string]string{}
	out := filepath.Join(p.dir, "out")
	err := filepath.WalkDir(out, func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(name, ".go") {
			return err
		}
		rel, _ := filepath.Rel(out, name)
		files[filepath.ToSlash(rel)] = p.read(name)
		return nil
	})
	if err != nil {
		p.t.Fatal(err)
	}
	return files
}

// build takes the runtime library from this repository's registry/runtime folder, as the work file of
// scripts/runtime-workspace.sh does, builds the service and copies the check into its module.
func (p *project) build() string {
	t := p.t
	t.Helper()
	runtimeDir, err := filepath.Abs(filepath.Join("..", "..", "..", "registry", "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	mod := filepath.Join(p.dir, "go.mod")
	p.write(mod, p.read(mod)+"\nreplace github.com/jsell-rh/stego/registry/runtime => "+runtimeDir+"\n")
	check, err := os.ReadFile(filepath.Join("testdata", "oidc_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	p.write(filepath.Join(p.dir, "out", "oidccheck", "oidc_test.go"), string(check))
	env := goEnv("GOFLAGS=-mod=mod")
	command(t, p.dir, env, pinnedGo(), "mod", "tidy")
	binary := filepath.Join(p.dir, "bin", "service")
	command(t, p.dir, env, pinnedGo(), "build", "-o", binary, "./out")
	return binary
}

func freePort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
}

// spiceDBImage is the pinned SpiceDB image of the local stand-in (ST-STD-LOCAL-R04), as the runtime's tests pin it.
const spiceDBImage = "authzed/spicedb:v1.57.0@sha256:a170d372c9f6b921bb6849064df6ca3566dd5b174882c1a2c4300c2ae1d60efd"

// startSpiceDB starts a local SpiceDB container without privilege, with an in-memory datastore and limits, and gives
// its endpoint and preshared key. It removes the container when the test ends.
func startSpiceDB(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("docker is not on the PATH: this test needs a local SpiceDB container")
	}
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	name, key := "stego-oidc-spicedb-"+hex.EncodeToString(suffix), "oidc-"+hex.EncodeToString(suffix)
	run := exec.Command("docker", "run", "-d", "--rm", "--name", name, "--memory", "256m", "--cpus", "1",
		"--security-opt", "no-new-privileges", "--cap-drop", "ALL", "--stop-timeout", "1",
		"-p", "127.0.0.1::50051", spiceDBImage, "serve", "--grpc-preshared-key", key, "--datastore-engine", "memory")
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

// checkRuns runs the check of the service's module for the provider, and fails each check ID that did not run and
// pass.
func (p *project) checkRuns(binary, provider, issuerPath, endpoint, key string, ids ...string) {
	t := p.t
	cmd := exec.Command(pinnedGo(), "test", "-count=1", "-timeout=300s", "-v", "-run", "^TestOIDC$", "./out/oidccheck")
	cmd.Dir = p.dir
	cmd.Env = goEnv("GOFLAGS=-mod=mod", "STEGO_TEST_SPICEDB="+endpoint, "STEGO_TEST_SPICEDB_KEY="+key, "STEGO_OIDC_BINARY="+binary,
		"STEGO_OIDC_ADDRESS="+p.address, "STEGO_OIDC_ISSUER_PATH="+issuerPath, "STEGO_OIDC_FORM="+provider)
	output, runErr := cmd.CombinedOutput()
	for _, id := range ids {
		t.Run(id, func(t *testing.T) {
			if !strings.Contains(string(output), "--- PASS: TestOIDC/"+strings.ReplaceAll(id, " ", "_")+" ") {
				t.Fatalf("the check %s of the generated service did not run and pass (go test: %v):\n%s", strings.SplitN(id, " ", 2)[0], runErr, output)
			}
		})
	}
}

// TestOIDCService generates, builds and runs a service with rh-sso-auth and one with oidc-auth, which the registry
// ships at ga (ST-SUP-Q99), one after the other: both use the one PostgreSQL server, whose roles belong to the
// server. A missing server fails the test: a missing result is not a pass.
func TestOIDCService(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs generated services")
	}
	if os.Getenv("STEGO_TEST_POSTGRES_DSN") == "" {
		t.Fatal("STEGO_TEST_POSTGRES_DSN is not set: this test needs a local PostgreSQL server")
	}
	stego := filepath.Join(t.TempDir(), "stego")
	command(t, filepath.Join("..", "..", ".."), goEnv(), pinnedGo(), "build", "-o", stego, "./cmd/stego")
	endpoint, key := startSpiceDB(t)

	sso := newProject(t, stego, "rh-sso-auth")
	t.Run("ST-SUP-RC21-OC15-T01 rh-sso-auth on the runtime data path is a configuration of oidc-auth", func(t *testing.T) {
		sso.t = t
		// The Red Hat SSO claims hold the tenant in org_id, so another tenancy claim is refused.
		declaration := filepath.Join(sso.dir, "service.yaml")
		text := sso.read(declaration)
		sso.write(declaration, strings.Replace(text, "  claim: org_id\n", "  claim: tenant\n", 1))
		if output, err := sso.run("plan"); err == nil || !strings.Contains(output, "org_id") {
			t.Errorf("the plan of rh-sso-auth with the tenancy claim tenant gave %v, want a refusal that names org_id:\n%s", err, output)
		}
		sso.write(declaration, text)
		if output, err := sso.run("apply"); err != nil {
			t.Fatalf("stego apply of the service with rh-sso-auth: %v\n%s", err, output)
		}
		files := sso.generated()
		for name := range files {
			if strings.HasPrefix(name, "internal/auth/") {
				t.Errorf("rh-sso-auth emitted the file %s, a verifier of its own", name)
			}
		}
		wiring := files["internal/stegoruntime/runtime.go"]
		for _, want := range []string{`declaredIssuer    = "https://` + sso.address + `/auth/realms/redhat-external"`, `Discovery: true`,
			`Algorithms: []string{"RS256"}`, `declaredSubjectClaim = "sub"`, `tenantClaim = "org_id"`} {
			if !strings.Contains(wiring, want) {
				t.Errorf("the runtime wiring does not hold the Red Hat SSO setting %s", want)
			}
		}
	})
	sso.t = t
	binary := sso.build()
	sso.checkRuns(binary, "rh-sso-auth", "/auth/realms/redhat-external", endpoint, key,
		"ST-SUP-RC21-OC15-T01 A service with rh-sso-auth on the runtime data path gets the oidc-auth verifier with the Red Hat SSO settings")

	oidc := newProject(t, stego, Component)
	if output, err := oidc.run("apply"); err != nil {
		t.Fatalf("stego apply of the service with oidc-auth: %v\n%s", err, output)
	}
	t.Run("ST-SUP-RC37-OC04-T01 The service verifies through the runtime verifier only", func(t *testing.T) {
		oidc.t = t
		files := oidc.generated()
		for name, text := range files {
			if strings.HasPrefix(name, "internal/auth/") {
				t.Errorf("oidc-auth emitted the file %s", name)
			}
			for _, own := range []string{`"crypto/rsa"`, `"crypto/ecdsa"`, "NewKeySet", "ParseKeys", "jwks", ".well-known", "golang-jwt"} {
				if strings.Contains(text, own) {
					t.Errorf("the generated file %s holds %s: a verifier, key set or key source of the service's own", name, own)
				}
			}
		}
		wiring := files["internal/stegoruntime/runtime.go"]
		for _, want := range []string{`declaredIssuer    = "https://` + oidc.address + `/realms/shop"`, `declaredAudience  = "orders-api"`,
			`Discovery: true`, `Algorithms: []string{"RS256", "ES256"}`, `declaredSubjectClaim = "sub"`} {
			if !strings.Contains(wiring, want) {
				t.Errorf("the runtime wiring does not make the runtime verifier from the setting %s", want)
			}
		}
		if main := files["main.go"]; !strings.Contains(main, "stegoruntime.StartStage(err)") || strings.Contains(main, "/out/internal/auth\"") {
			t.Error("main.go does not name the stage of a refused start, or wires a package of its own for the sign-in")
		}
	})
	oidc.t = t
	binary = oidc.build()
	oidc.checkRuns(binary, Component, "/realms/shop", endpoint, key,
		"ST-SUP-RC37-OC01-T01 A provider is found through its discovery document, and a mismatched document is refused",
		"ST-SUP-RC37-OC02-T01 A token for another audience or with another algorithm is refused",
		"ST-SUP-RC37-OC03-T01 The subject and the tenant come only from the declared claims")
}
