// Package pgtest gives the PostgreSQL tests of one test binary of the runtime module their local server
// (ST-STD-LOCAL-R04). It is test support: only test files import it.
//
// A package's TestMain calls Main. A test calls DSN or Database. The server is one of these:
//   - the server that STEGO_TEST_POSTGRES_DSN names, a superuser URL, when the variable is set;
//   - else one container of Image per test binary, which the first test that needs it starts and which Main
//     removes when the tests end. The container runs with podman when podman is on PATH and can start it, else
//     with docker, without privilege, with 512 MiB of memory and one CPU, its data on a tmpfs, published on
//     127.0.0.1 at a free port, and with a time limit (the binary's -test.timeout and one minute, else one
//     hour), so that it stops also when the test binary is killed.
//
// When neither podman nor docker is on PATH, each test that needs the server fails: a missing result is not a
// pass. Only when STEGO_TEST_NO_CONTAINERS=1 is set does such a test skip, and Main then says on standard error
// how many tests skipped.
//
// The package imports no database driver (ST-STD-RUNTIME-R06): Database opens the server through the driver
// name "pgx", which the test files of the package under test register when they import pgx's stdlib package.
package pgtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Image is the pinned PostgreSQL server image: postgres:17.7, by the digest of its multi-platform index.
const Image = "docker.io/library/postgres:17.7@sha256:2006493727bd5277eece187319af1ef82b4cf82cf4fc1ed00da0775b646ac2a4"

const (
	// DSNVariable names an existing server, a superuser URL; when it is set, no container starts.
	DSNVariable = "STEGO_TEST_POSTGRES_DSN"
	// NoContainersVariable set to 1 makes a test that needs a container skip, instead of fail, when no
	// container can start.
	NoContainersVariable = "STEGO_TEST_NO_CONTAINERS"
	// startLimit is the time the container has to pull its image and become ready.
	startLimit = 3 * time.Minute
	// defaultLife is the time limit of the container when the test binary has no -test.timeout.
	defaultLife = time.Hour
)

var (
	once    sync.Once
	dsn     string
	skipWhy string
	failWhy string
	skipped atomic.Int64

	mu        sync.Mutex
	tool      string
	container string
)

// Main runs the tests of m, removes the container that a test started, if any, and gives the exit code for
// os.Exit. It also removes the container when the process gets SIGINT. It does not watch SIGTERM, which tests of
// a shutdown send to their own process; the time limit of the container covers a process that is killed.
//
//	func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }
func Main(m *testing.M) int {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	done := make(chan struct{})
	go func() {
		select {
		case s := <-signals:
			remove()
			fmt.Fprintf(os.Stderr, "pgtest: %v: the PostgreSQL container is removed\n", s)
			os.Exit(2)
		case <-done:
		}
	}()
	code := m.Run()
	close(done)
	signal.Stop(signals)
	remove()
	if n := skipped.Load(); n > 0 {
		fmt.Fprintf(os.Stderr, "pgtest: %d tests skipped: %s\n", n, skipWhy)
	}
	return code
}

// DSN gives the superuser URL of the server, and starts the container the first time a test of the binary
// asks. It fails the test when the server cannot start, and skips it only as the package comment says.
func DSN(t testing.TB) string {
	t.Helper()
	once.Do(start)
	switch {
	case dsn != "":
		return dsn
	case skipWhy != "":
		skipped.Add(1)
		t.Skip("pgtest: " + skipWhy)
	default:
		t.Fatal("pgtest: " + failWhy)
	}
	return ""
}

// Database makes a new, empty database on the server for the test, named prefix and a random suffix, drops it
// when the test ends, and gives its URL as the superuser.
func Database(t testing.TB, prefix string) string {
	t.Helper()
	server := DSN(t)
	admin, err := sql.Open("pgx", server)
	if err != nil {
		t.Fatalf("pgtest: open the server with the driver pgx; does a test file import github.com/jackc/pgx/v5/stdlib? %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	name := prefix + "_" + suffix()
	ctx := context.Background()
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("pgtest: create the database %s: %v", name, err)
	}
	// The drop gets 30 seconds, and a drop that fails is logged: the cleanup alone never fails the test.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("pgtest: cleanup failed: drop the database %s: %v", name, err)
		}
	})
	u, err := url.Parse(server)
	if err != nil {
		t.Fatalf("pgtest: %s must be a postgres:// URL: %v", DSNVariable, err)
	}
	u.Path = "/" + name
	return u.String()
}

// start finds the server: the variable, else a new container. It sets dsn, or skipWhy, or failWhy.
func start() {
	if v := os.Getenv(DSNVariable); v != "" {
		if u, err := url.Parse(v); err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
			failWhy = DSNVariable + " must be a postgres:// URL"
			return
		}
		dsn = v
		return
	}
	var found []string
	for _, name := range []string{"podman", "docker"} {
		if p, err := exec.LookPath(name); err == nil {
			found = append(found, p)
		}
	}
	if len(found) == 0 {
		why := "neither podman nor docker is on PATH, so the PostgreSQL container of the test cannot start; install one, or set " +
			DSNVariable + " to a local server"
		if os.Getenv(NoContainersVariable) == "1" {
			skipWhy = why + " (" + NoContainersVariable + "=1)"
			return
		}
		failWhy = why + ". Set " + NoContainersVariable + "=1 to skip these tests instead"
		return
	}
	// podman comes first. When it is there but cannot start the container, as a rootless podman without the
	// cgroup controllers for the limits cannot, docker is tried next.
	var errs []string
	for _, path := range found {
		got, err := run(path)
		if err == nil {
			dsn = got
			return
		}
		remove()
		errs = append(errs, err.Error())
	}
	failWhy = "start the PostgreSQL container: " + strings.Join(errs, "; then: ")
}

// run starts the container with the tool, waits until the server accepts TCP connections inside it, and gives
// the superuser URL on the published port.
func run(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), startLimit)
	defer cancel()
	password := suffix() + suffix()
	life := defaultLife
	if f := flag.Lookup("test.timeout"); f != nil {
		if d, err := time.ParseDuration(f.Value.String()); err == nil && d > 0 {
			life = d + time.Minute
		}
	}
	name := fmt.Sprintf("stego-pgtest-%d-%s", os.Getpid(), suffix())
	args := []string{"run", "--detach", "--rm", "--name", name,
		"--memory", "512m", "--cpus", "1",
		"--security-opt", "no-new-privileges",
		"--tmpfs", "/var/lib/postgresql/data:rw,size=384m",
		"--publish", "127.0.0.1::5432",
		"--env", "POSTGRES_PASSWORD=" + password,
		"--entrypoint", "timeout",
		Image,
		fmt.Sprintf("%ds", int(life.Seconds())), "docker-entrypoint.sh", "postgres",
		"-c", "fsync=off", "-c", "synchronous_commit=off", "-c", "full_page_writes=off"}
	mu.Lock()
	tool = path
	mu.Unlock()
	out, err := command(ctx, path, args...)
	if err != nil {
		return "", err
	}
	mu.Lock()
	container = strings.TrimSpace(out)
	mu.Unlock()
	out, err = command(ctx, path, "port", name, "5432/tcp")
	if err != nil {
		return "", err
	}
	host, port, err := net.SplitHostPort(strings.TrimSpace(strings.SplitN(out, "\n", 2)[0]))
	if err != nil {
		return "", fmt.Errorf("read the published port from %q: %w", out, err)
	}
	// The image's entrypoint first runs a server for initdb that listens only on its socket; a TCP connection
	// inside the container reaches only the final server.
	for {
		if _, err := command(ctx, path, "exec", name, "pg_isready", "--host", "127.0.0.1", "--username", "postgres", "--timeout", "2"); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			logs, _ := command(context.Background(), path, "logs", "--tail", "40", name)
			return "", fmt.Errorf("the server is not ready after %v; its last log lines:\n%s", startLimit, logs)
		case <-time.After(200 * time.Millisecond):
		}
	}
	u := url.URL{Scheme: "postgres", User: url.UserPassword("postgres", password), Host: net.JoinHostPort(host, port),
		Path: "/postgres", RawQuery: "sslmode=disable"}
	return u.String(), nil
}

// remove removes the container, if one started. It is safe to call more than once.
func remove() {
	mu.Lock()
	path, id := tool, container
	container = ""
	mu.Unlock()
	if id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := command(ctx, path, "rm", "--force", id); err != nil {
		fmt.Fprintf(os.Stderr, "pgtest: remove the PostgreSQL container %s: %v\n", id, err)
	}
}

// command runs the tool and gives its standard output, or an error with its standard error.
func command(ctx context.Context, path string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) || stderr.Len() > 0 {
			return "", fmt.Errorf("%s %s: %v: %s", path, args[0], err, strings.TrimSpace(stderr.String()))
		}
		return "", fmt.Errorf("%s %s: %w", path, args[0], err)
	}
	return stdout.String(), nil
}

// suffix is 12 random hexadecimal digits.
func suffix() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
