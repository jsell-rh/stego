package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/jsell-rh/stego/internal/appbuild"
)

// The shared fixture of the tests that run the stego command from outside,
// as an operator or a CI job does. It builds the stego binary once per test
// run, from a clean Git snapshot of the working tree, since an image record
// accepts only a packer built from clean source. It makes one real
// application build once, with that binary, from a module cache that the
// binary downloads, and one image of it, so that appbuild.Verify accepts
// the executable. The build needs the Go SDK that STEGO pins: the fixture
// uses STEGO_TEST_PINNED_GO, else bin/go of the SDK that runs the tests, and
// a test skips only when STEGO refuses that SDK as not the pinned one. Each
// test copies what it changes. TestMain removes the fixture folder.

var fixtureRoot string

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "stego-fixture-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fixtureRoot = root
	code := m.Run()
	// A build keeps read-only module sources; make them writable to remove them.
	_ = filepath.WalkDir(root, func(name string, entry os.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			_ = os.Chmod(name, 0700)
		}
		return nil
	})
	_ = os.RemoveAll(root)
	os.Exit(code)
}

// onceFixture makes one part of the fixture once. A failure fails every test
// that needs the part; a refused SDK skips them.
type onceFixture struct {
	once    sync.Once
	failure string
	skip    string
}

func (f *onceFixture) get(t *testing.T, make func() (skip string, err error)) {
	t.Helper()
	f.once.Do(func() {
		skip, err := make()
		f.skip = skip
		if err != nil {
			f.failure = err.Error()
		}
	})
	if f.skip != "" {
		t.Skip(f.skip)
	}
	if f.failure != "" {
		t.Fatal(f.failure)
	}
}

var binaryFixture onceFixture
var stegoPath string

// stegoBinary builds cmd/stego once, from a clean Git snapshot of the
// tracked and new files of the working tree, and gives its path.
func stegoBinary(t *testing.T) string {
	t.Helper()
	binaryFixture.get(t, func() (string, error) {
		top, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
		if err != nil {
			return "", fmt.Errorf("find the source tree: %v", err)
		}
		snapshot := filepath.Join(fixtureRoot, "stego-source")
		if err := os.Mkdir(snapshot, 0700); err != nil {
			return "", err
		}
		copyFiles := exec.Command("sh", "-c", "git ls-files -co --exclude-standard -z | tar --null --ignore-failed-read -T - -cf - | tar -xf - -C \"$1\"", "sh", snapshot)
		copyFiles.Dir = strings.TrimSpace(string(top))
		if output, err := copyFiles.CombinedOutput(); err != nil {
			return "", fmt.Errorf("snapshot the source: %v\n%s", err, output)
		}
		git(t, snapshot, "init", "-q")
		git(t, snapshot, "add", "-A")
		git(t, snapshot, "commit", "-q", "-m", "snapshot")
		stegoPath = filepath.Join(fixtureRoot, "bin", "stego")
		build := exec.Command(pinnedGo(t), "build", "-mod=readonly", "-trimpath", "-o", stegoPath, "./cmd/stego")
		build.Dir = snapshot
		build.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=readonly", "GOTOOLCHAIN=local")
		if output, err := build.CombinedOutput(); err != nil {
			return "", fmt.Errorf("build stego: %v\n%s", err, output)
		}
		return "", nil
	})
	return stegoPath
}

// runStego runs the stego binary with the arguments and gives what it
// printed on standard output and on standard error.
func runStego(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, stegoBinary(t), args...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}

// mustStego runs the stego binary and fails the test when it fails.
func mustStego(t *testing.T, args ...string) string {
	t.Helper()
	stdout, stderr, err := runStego(t, args...)
	if err != nil {
		t.Fatalf("stego %s: %v\n%s", strings.Join(args, " "), err, stderr)
	}
	return stdout
}

// applicationFixture is the result of one real application build.
type applicationFixture struct {
	result, recordSHA256, revision string
	// event is the Application built event that stego build printed.
	event string
}

var applicationOnce onceFixture
var application applicationFixture

// builtApplication makes one real application build with the stego binary,
// from a module cache that the binary downloads, and gives its result.
func builtApplication(t *testing.T) applicationFixture {
	t.Helper()
	binary := stegoBinary(t)
	applicationOnce.get(t, func() (string, error) {
		root := filepath.Join(fixtureRoot, "application")
		if err := os.Mkdir(root, 0700); err != nil {
			return "", err
		}
		source, revision := buildSourceIn(t, root)
		goTool := pinnedGo(t)
		cache := filepath.Join(root, "cache")
		for _, args := range [][]string{
			{"build", "download", "--source", source, "--revision", revision, "--target", "out/internal/api/tiny", "--go", goTool, "--output", cache},
			buildArgs(source, revision, goTool, filepath.Join(root, "work"), filepath.Join(root, "result"), cache),
		} {
			if args[0] != "build" {
				args = append([]string{"build"}, args...)
			}
			command := exec.Command(binary, args...)
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			if err := command.Run(); err != nil {
				if strings.Contains(stderr.String(), "is not the pinned official release") {
					return "the Go SDK of this run is not the pinned one; set STEGO_TEST_PINNED_GO: " + stderr.String(), nil
				}
				return "", fmt.Errorf("stego %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
			}
			application.event = stdout.String()
		}
		application.result = filepath.Join(root, "result")
		application.recordSHA256 = fileSHA256(t, filepath.Join(application.result, "build.json"))
		application.revision = revision
		return "", nil
	})
	return application
}

// imageCA gives the explicit CA bundle of the image tests and its digest.
func imageCA(t *testing.T) (string, string) {
	t.Helper()
	name, err := filepath.Abs(filepath.Join("..", "..", "internal", "appbuild", "testdata", "image-ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	return name, fileSHA256(t, name)
}

// imageFixture is an image folder that stego image build wrote.
type imageFixture struct {
	folder, recordSHA256, manifest string
	// event is the Image built event that stego image build printed.
	event string
}

var imageOnce onceFixture
var builtImageFixture imageFixture

// builtImage builds one image of the real application build with the stego
// binary, and gives its folder. A test that changes the folder copies it.
func builtImage(t *testing.T) imageFixture {
	t.Helper()
	app := builtApplication(t)
	imageOnce.get(t, func() (string, error) {
		ca, caSHA256 := imageCA(t)
		folder := filepath.Join(fixtureRoot, "image")
		stdout, stderr, err := runStego(t, "image", "build", "--build-record", filepath.Join(app.result, "build.json"), "--build-record-sha256", app.recordSHA256, "--application", filepath.Join(app.result, "application"), "--trust-store", ca, "--trust-store-sha256", caSHA256, "--entrypoint", "service", "--output", folder)
		if err != nil {
			return "", fmt.Errorf("stego image build: %v\n%s", err, stderr)
		}
		builtImageFixture = imageFixture{folder: folder, recordSHA256: fileSHA256(t, filepath.Join(folder, "image.json")), event: stdout}
		var record appbuild.ImageRecord
		data, err := os.ReadFile(filepath.Join(folder, "image.json"))
		if err == nil {
			err = json.Unmarshal(data, &record)
		}
		builtImageFixture.manifest = record.Manifest.SHA256
		return "", err
	})
	return builtImageFixture
}

// copyFolder copies a fixture folder into a new folder of the test.
func copyFolder(t *testing.T, folder string) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), filepath.Base(folder))
	if output, err := exec.Command("cp", "-a", folder, target).CombinedOutput(); err != nil {
		t.Fatalf("copy %s: %v\n%s", folder, err, output)
	}
	return target
}

// writeFile writes a file of the test and gives its path.
func writeFile(t *testing.T, folder, name, data string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(folder, name)
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// eventFile writes an event as stego prints it and gives its path.
func eventFile(t *testing.T, event any) string {
	t.Helper()
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	return writeFile(t, t.TempDir(), "event.json", string(data)+"\n", 0600)
}

// lastEvent reads the event that stego printed as its last line.
func lastEvent(t *testing.T, stdout string, event any) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	decoder := json.NewDecoder(strings.NewReader(lines[len(lines)-1]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(event); err != nil {
		t.Fatalf("stego printed no event as its last line: %v\n%s", err, stdout)
	}
}

const trustedIssuer = "https://token.actions.githubusercontent.com"
const trustedSubject = "https://github.com/example/service/.github/workflows/build.yml@refs/heads/main"
const otherSubject = "https://github.com/example/other/.github/workflows/build.yml@refs/heads/main"

// fakeGH writes a fake GitHub CLI that prints, for gh attestation verify,
// the verification result that the bundle file holds, and gives its path.
// The bundles that signatureBundles writes are such results, so the
// signature checks of STEGO run on them as on the real CLI's output. A real
// keyless signature needs a CI identity, so that proof stays in CI.
func fakeGH(t *testing.T) string {
	t.Helper()
	return writeFile(t, t.TempDir(), "gh", "#!/bin/sh\n[ \"$1\" = attestation ] && [ \"$2\" = verify ] || exit 2\nwhile [ $# -gt 0 ]; do\n  if [ \"$1\" = --bundle ]; then exec cat \"$2\"; fi\n  shift\ndone\nexit 2\n", 0700)
}

// signatureBundles writes, for the image folder, the two SBOM attestation
// bundles about its image digest, signed by the subject, and gives their
// paths.
func signatureBundles(t *testing.T, image, subject string) (string, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(image, "image.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record appbuild.ImageRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	folder := t.TempDir()
	var paths []string
	for _, format := range []appbuild.SBOMFormat{appbuild.SPDX, appbuild.CycloneDX} {
		document, err := os.ReadFile(filepath.Join(image, format.FileName()))
		if err != nil {
			t.Fatal(err)
		}
		result := []map[string]any{{
			"attestation": map[string]any{"bundle": map[string]any{"verificationMaterial": map[string]any{"tlogEntries": []any{map[string]any{"logIndex": 42}}}}},
			"verificationResult": map[string]any{
				"statement": map[string]any{
					"_type":         "https://in-toto.io/Statement/v1",
					"subject":       []any{map[string]any{"digest": map[string]string{"sha256": record.Manifest.SHA256}}},
					"predicateType": format.PredicateType(),
					"predicate":     json.RawMessage(document),
				},
				"signature":          map[string]any{"certificate": map[string]any{"issuer": trustedIssuer, "subjectAlternativeName": subject}},
				"verifiedTimestamps": []any{map[string]any{"type": "Tlog", "timestamp": "2026-11-04T10:00:00Z"}},
			},
		}}
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, writeFile(t, folder, string(format)+".bundle.json", string(encoded), 0600))
	}
	return paths[0], paths[1]
}

// signatureArgs gives the signature flags of the trusted signer for the
// bundles.
func signatureArgs(gh, spdx, cyclonedx string) []string {
	return []string{"--spdx-bundle", spdx, "--cyclonedx-bundle", cyclonedx, "--signer-issuer", trustedIssuer, "--signer-subject", trustedSubject, "--gh", gh, "--source-repository", "example/service"}
}

// signedImage copies the fixture image, signs the copy with stego image
// sign, and gives the folder, the Image signed event that sign printed, and
// the signature flags it used.
func signedImage(t *testing.T) (string, string, []string) {
	t.Helper()
	fixture := builtImage(t)
	image := copyFolder(t, fixture.folder)
	spdx, cyclonedx := signatureBundles(t, image, trustedSubject)
	signature := signatureArgs(fakeGH(t), spdx, cyclonedx)
	stdout := mustStego(t, append([]string{"image", "sign", "--image", image, "--record-sha256", fixture.recordSHA256}, signature...)...)
	return image, stdout, signature
}

// registryServer is an in-process TLS registry with basic auth that counts
// the requests it gets.
type registryServer struct {
	server   *httptest.Server
	requests atomic.Int64
	// ca and credentials are files of the test, with the CA digest.
	ca, caSHA256, credentials string
}

func newRegistry(t *testing.T) *registryServer {
	t.Helper()
	fixture := &registryServer{}
	handler := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Registry Test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	raw, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	fixture.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.requests.Add(1)
		user, password, ok := r.BasicAuth()
		if !ok || user != "test" || password != "fixture-credential" {
			w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	fixture.server.Config.ErrorLog = log.New(io.Discard, "", 0)
	fixture.server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{raw}, PrivateKey: key}}}
	fixture.server.StartTLS()
	t.Cleanup(fixture.server.Close)
	folder := t.TempDir()
	fixture.ca = writeFile(t, folder, "ca.pem", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw})), 0600)
	fixture.caSHA256 = fileSHA256(t, fixture.ca)
	fixture.credentials = writeFile(t, folder, "credentials.json", "{\"username\":\"test\",\"password\":\"fixture-credential\"}\n", 0600)
	return fixture
}

// repository gives an explicit place in the registry, with no tag.
func (r *registryServer) repository(name string) string {
	return strings.TrimPrefix(r.server.URL, "https://") + "/test/" + name
}

// accessArgs gives the registry flags of a repository.
func (r *registryServer) accessArgs(name string) []string {
	return []string{"--repository", r.repository(name), "--registry-ca", r.ca, "--registry-ca-sha256", r.caSHA256, "--credentials", r.credentials}
}

func TestPolicyBuildImageFromBuiltApplication(t *testing.T) {
	t.Run("ST-BLD-PO01-T The image build hears Application built, reads the build record at its digest, and builds from the recorded executable only", func(t *testing.T) {
		app := builtApplication(t)
		var event appbuild.ApplicationBuilt
		lastEvent(t, app.event, &event)
		if event.RecordDigest != app.recordSHA256 || event.SourceRevision != app.revision {
			t.Fatalf("stego build printed %+v; expected the record digest %s and the revision %s", event, app.recordSHA256, app.revision)
		}
		ca, caSHA256 := imageCA(t)
		build := func(event appbuild.ApplicationBuilt, extra ...string) (string, string, string, error) {
			output := filepath.Join(t.TempDir(), "image")
			args := append([]string{"image", "build", "--after", eventFile(t, event), "--build-result", app.result, "--trust-store", ca, "--trust-store-sha256", caSHA256, "--entrypoint", "service", "--output", output}, extra...)
			stdout, stderr, err := runStego(t, args...)
			return output, stdout, stderr, err
		}
		output, stdout, stderr, err := build(event)
		if err != nil {
			t.Fatalf("stego image build --after: %v\n%s", err, stderr)
		}
		var record appbuild.ImageRecord
		data, err := os.ReadFile(filepath.Join(output, "image.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &record); err != nil {
			t.Fatal(err)
		}
		if record.BuildRecordSHA256 != event.RecordDigest || record.Application.SHA256 != event.ExecutableDigest {
			t.Fatalf("the image is of build record %s and executable %s; expected those of the event %+v", record.BuildRecordSHA256, record.Application.SHA256, event)
		}
		var built appbuild.ImageBuilt
		lastEvent(t, stdout, &built)
		if built.ImageDigest != record.Manifest.SHA256 || built.BuildRecordDigest != event.RecordDigest {
			t.Fatalf("stego image build printed %+v; expected the image sha256:%s of build record %s", built, record.Manifest.SHA256, event.RecordDigest)
		}
		for _, item := range []struct {
			name   string
			event  appbuild.ApplicationBuilt
			extra  []string
			reason string
		}{
			{"another executable", appbuild.ApplicationBuilt{SourceRevision: event.SourceRevision, RecordDigest: event.RecordDigest, ExecutableDigest: strings.Repeat("0", 64)}, nil, "sha256:" + strings.Repeat("0", 64)},
			{"another build record", appbuild.ApplicationBuilt{SourceRevision: event.SourceRevision, RecordDigest: strings.Repeat("1", 64), ExecutableDigest: event.ExecutableDigest}, nil, "expected digest " + strings.Repeat("1", 64)},
			{"another revision", appbuild.ApplicationBuilt{SourceRevision: strings.Repeat("2", 40), RecordDigest: event.RecordDigest, ExecutableDigest: event.ExecutableDigest}, nil, "not of revision " + strings.Repeat("2", 40)},
			{"an executable beside the event", event, []string{"--application", filepath.Join(app.result, "build.json")}, "from the event only"},
		} {
			output, _, stderr, err := build(item.event, item.extra...)
			if err == nil || !strings.Contains(stderr, item.reason) {
				t.Errorf("stego image build --after with %s: %v, %q; expected a refusal that names %q", item.name, err, stderr, item.reason)
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Errorf("stego image build --after with %s left the image folder", item.name)
			}
		}
	})
}

func TestPolicySignBuiltImage(t *testing.T) {
	t.Run("ST-BLD-PO02-T Sign the image hears Image built, reads the image record at the built digest, and signs that digest with the trusted signer", func(t *testing.T) {
		fixture := builtImage(t)
		var event appbuild.ImageBuilt
		lastEvent(t, fixture.event, &event)
		if event.ImageDigest != fixture.manifest {
			t.Fatalf("stego image build printed %+v; expected the image sha256:%s", event, fixture.manifest)
		}
		gh := fakeGH(t)
		sign := func(event appbuild.ImageBuilt, subject string) (string, string, string, error) {
			image := copyFolder(t, fixture.folder)
			spdx, cyclonedx := signatureBundles(t, image, subject)
			stdout, stderr, err := runStego(t, append([]string{"image", "sign", "--after", eventFile(t, event), "--image", image}, signatureArgs(gh, spdx, cyclonedx)...)...)
			return image, stdout, stderr, err
		}
		image, stdout, stderr, err := sign(event, trustedSubject)
		if err != nil {
			t.Fatalf("stego image sign --after: %v\n%s", err, stderr)
		}
		var signature appbuild.ImageSignature
		data, err := os.ReadFile(filepath.Join(image, "signature.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &signature); err != nil {
			t.Fatal(err)
		}
		if signature.ImageSHA256 != event.ImageDigest || signature.ImageRecordSHA256 != fixture.recordSHA256 || signature.Signer.Subject != trustedSubject || len(signature.Attestations) != 2 {
			t.Fatalf("the signature record is %+v; expected the trusted signer's signature of image sha256:%s with both SBOMs", signature, event.ImageDigest)
		}
		var signed appbuild.ImageSigned
		lastEvent(t, stdout, &signed)
		if signed.ImageDigest != event.ImageDigest || !strings.Contains(signed.Signer, trustedSubject) {
			t.Fatalf("stego image sign printed %+v; expected the image sha256:%s and the trusted signer", signed, event.ImageDigest)
		}
		for _, item := range []struct {
			name    string
			event   appbuild.ImageBuilt
			subject string
			reason  string
		}{
			{"another image", appbuild.ImageBuilt{ImageDigest: strings.Repeat("3", 64), BuildRecordDigest: event.BuildRecordDigest}, trustedSubject, "not sha256:" + strings.Repeat("3", 64)},
			{"another build record", appbuild.ImageBuilt{ImageDigest: event.ImageDigest, BuildRecordDigest: strings.Repeat("4", 64)}, trustedSubject, "not sha256:" + strings.Repeat("4", 64)},
			{"another signer", event, otherSubject, "not by the trusted signer"},
		} {
			image, _, stderr, err := sign(item.event, item.subject)
			if err == nil || !strings.Contains(stderr, item.reason) {
				t.Errorf("stego image sign --after with %s: %v, %q; expected a refusal that names %q", item.name, err, stderr, item.reason)
			}
			if _, err := os.Stat(filepath.Join(image, "signature.json")); !os.IsNotExist(err) {
				t.Errorf("stego image sign --after with %s wrote signature.json", item.name)
			}
		}
	})
}
