package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/buildidentity"
	"gopkg.in/yaml.v3"
)

type failedVersionWriter struct{}

func (failedVersionWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestVersion(t *testing.T) {
	var out bytes.Buffer
	t.Run("ST-DIS-UC09-R03-T01 The version command reports the current build identity", func(t *testing.T) {
		if err := runVersion(nil, &out); err != nil || out.String() != "stego "+version+"\n" {
			t.Fatal(out.String(), err)
		}
		out.Reset()
		if err := runVersion([]string{"--json"}, &out); err != nil {
			t.Fatal(err)
		}
		var report struct {
			Version string               `json:"version"`
			Build   buildidentity.Record `json:"build"`
		}
		if err := json.Unmarshal(out.Bytes(), &report); err != nil || report.Build != buildidentity.Current() || report.Version != version {
			t.Fatal(report, err)
		}
	})
	for _, args := range [][]string{{"--json", "extra"}, {"invalid"}} {
		out.Reset()
		if err := runVersion(args, &out); err == nil || out.Len() != 0 {
			t.Fatal("invalid arguments accepted")
		}
	}
	if err := runVersion([]string{"--json"}, failedVersionWriter{}); err == nil {
		t.Fatal("write failure ignored")
	}
}

func TestApplyRecordsTheIdentityTheCompilerReports(t *testing.T) {
	t.Run("ST-DIS-UC09-R03-T02 Apply writes the current build identity into the record", func(t *testing.T) {
		// The binary is built from the source tree before the test moves into the project.
		stegoBinary(t)
		generatingProject(t, generatingDeclaration)
		var report struct {
			Build map[string]any `json:"build"`
		}
		if err := json.Unmarshal([]byte(mustStego(t, "version", "--json")), &report); err != nil || len(report.Build) == 0 {
			t.Fatalf("stego version --json gives no build identity: %v", err)
		}
		recorded := func() any {
			applied, _ := plainRecord(t)["last_applied"].(map[string]any)
			return applied["compiler_build"]
		}
		mustStego(t, "apply")
		if got := recorded(); !reflect.DeepEqual(got, report.Build) {
			t.Fatalf("the record holds the compiler build %v, and stego version reports %v", got, report.Build)
		}
		// A legacy record has no compiler build. The next plan shows the change, apply adds the identity, and
		// a repeated plan changes nothing.
		record := plainRecord(t)
		delete(record["last_applied"].(map[string]any), "compiler_build")
		data, err := yaml.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		writeProjectFile(t, ".stego/state.yaml", string(data))
		if plan := mustStego(t, "plan"); strings.Contains(plan, "No changes.") {
			t.Fatalf("the plan of a legacy record hides the missing compiler build:\n%s", plan)
		}
		mustStego(t, "apply")
		if got := recorded(); !reflect.DeepEqual(got, report.Build) {
			t.Fatalf("the legacy record gained the compiler build %v, and stego version reports %v", got, report.Build)
		}
		if plan := mustStego(t, "plan"); !strings.Contains(plan, "No changes.") {
			t.Fatalf("a repeated plan changes something:\n%s", plan)
		}
	})
}

// fakeSignerGH is the local stand-in for the CI provider's client and its trusted signer: it answers gh
// attestation verify for the files whose digests the test trusts, under the compiler's signer policy, and fails
// for any other file or policy. It logs each call. It never reaches GitHub.
const fakeSignerGH = `#!/usr/bin/env python3
import hashlib, json, sys
from pathlib import Path
here = Path(__file__).resolve().parent
state = json.loads((here / "state.json").read_text())
args = sys.argv[1:]
with (here / "calls.jsonl").open("a") as log:
    log.write(json.dumps(args) + "\n")
def value(name):
    return args[args.index(name) + 1] if name in args else None
if args[:2] != ["attestation", "verify"]:
    sys.exit("the fake gh answers only attestation verify")
digest = lambda path: hashlib.sha256(Path(path).read_bytes()).hexdigest()
policy = {"--source-digest": state["revision"], "--signer-digest": state["revision"],
          "--source-ref": "refs/heads/main", "--predicate-type": "https://slsa.dev/provenance/v1",
          "--cert-identity": "https://github.com/jsell-rh/stego/.github/workflows/compiler-artifact.yml@refs/heads/main"}
if any(value(k) != v for k, v in policy.items()) or value("--bundle") is None or digest(value("--bundle")) != state["bundle"]:
    sys.exit("the signer policy or the bundle does not verify")
if digest(args[2]) not in state["trusted"]:
    sys.exit("no attestation verifies")
print(json.dumps([{"verificationResult": {"signature": {"certificate": {"sourceRepositoryDigest": state["revision"]}},
                   "statement": {"predicateType": "https://slsa.dev/provenance/v1"}}}]))
`

func TestInstallAQualifiedCompiler(t *testing.T) {
	t.Run("ST-DIS-UC09-FL-T An operator installs a release by its full commit, and the compiler reports the identity it records", func(t *testing.T) {
		binary := stegoBinary(t)
		python, err := exec.LookPath("python3")
		if err != nil {
			t.Skip("the installer runs with python3")
		}
		_, source, _, _ := runtime.Caller(0)
		installer := filepath.Join(filepath.Dir(source), "../../scripts/install-compiler.py")
		revision := strings.TrimSpace(git(t, filepath.Join(fixtureRoot, "stego-source"), "rev-parse", "HEAD"))
		var identity struct {
			Build map[string]any `json:"build"`
		}
		if err := json.Unmarshal([]byte(mustStego(t, "version", "--json")), &identity); err != nil {
			t.Fatal(err)
		}
		// The release assets of the commit, as the signed artifact run gives them.
		root := t.TempDir()
		release := filepath.Join(root, "release")
		if err := os.Mkdir(release, 0o700); err != nil {
			t.Fatal(err)
		}
		executable, err := os.ReadFile(binary)
		if err != nil {
			t.Fatal(err)
		}
		digest := func(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
		record, err := json.Marshal(map[string]any{"format": 1, "source_revision": revision,
			"artifact": map[string]any{"name": "stego-linux-amd64", "sha256": digest(executable), "size": len(executable)},
			"version":  map[string]any{"build": identity.Build}})
		if err != nil {
			t.Fatal(err)
		}
		bundle := []byte(`{"mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json"}` + "\n")
		sums := digest(executable) + "  stego-linux-amd64\n" + digest(record) + "  build.json\n"
		for name, data := range map[string][]byte{"stego-linux-amd64": executable, "build.json": record,
			"SHA256SUMS": []byte(sums), "provenance.jsonl": bundle} {
			if err := os.WriteFile(filepath.Join(release, name), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		bin := filepath.Join(root, "bin")
		if err := os.Mkdir(bin, 0o700); err != nil {
			t.Fatal(err)
		}
		gh := filepath.Join(bin, "gh")
		if err := os.WriteFile(gh, []byte(strings.Replace(fakeSignerGH, "/usr/bin/env python3", python, 1)), 0o700); err != nil {
			t.Fatal(err)
		}
		state, _ := json.Marshal(map[string]any{"revision": revision, "bundle": digest(bundle),
			"trusted": []string{digest(executable), digest(record)}})
		if err := os.WriteFile(filepath.Join(bin, "state.json"), state, 0o600); err != nil {
			t.Fatal(err)
		}
		install := func(output string, extra []string, environment ...string) (string, error) {
			args := append([]string{"-B", installer, "--revision", revision, "--output", output, "--gh", gh}, extra...)
			command := exec.Command(python, args...)
			command.Env = append([]string{"PATH=/usr/bin:/bin", "HOME=" + root}, environment...)
			output2, err := command.CombinedOutput()
			return string(output2), err
		}
		// The operator installs the release by its full commit on Linux amd64.
		installed := filepath.Join(root, "installed")
		if log, err := install(installed, []string{"--package", release}); err != nil {
			t.Fatalf("the installer fails: %v\n%s", err, log)
		}
		saved, err := os.ReadFile(filepath.Join(installed, "build.json"))
		if err != nil {
			t.Fatal(err)
		}
		var build struct {
			SourceRevision string `json:"source_revision"`
			Version        struct {
				Build map[string]any `json:"build"`
			} `json:"version"`
		}
		if err := json.Unmarshal(saved, &build); err != nil {
			t.Fatal(err)
		}
		if build.SourceRevision != revision || build.Version.Build["revision"] != revision ||
			build.Version.Build["goos"] != "linux" || build.Version.Build["goarch"] != "amd64" {
			t.Fatalf("the installed build record does not name the commit and Linux amd64: %s", saved)
		}
		// The installed compiler reports the identity of its build record, and records it in a service.
		compiler := filepath.Join(installed, "stego-linux-amd64")
		reported, err := exec.Command(compiler, "version", "--json").Output()
		if err != nil {
			t.Fatal(err)
		}
		var report struct {
			Build map[string]any `json:"build"`
		}
		if err := json.Unmarshal(reported, &report); err != nil || !reflect.DeepEqual(report.Build, build.Version.Build) {
			t.Fatalf("the installed compiler reports %v, and its build record holds %v (%v)", report.Build, build.Version.Build, err)
		}
		generatingProject(t, "kind: service\nname: small\narchetype: rest-crud\nlanguage: go\nentities:\n  - name: Note\n    fields: [{name: title, type: string}]\ncollections:\n  notes: {entity: Note, operations: [create, read]}\n")
		if output, err := exec.Command(compiler, "apply").CombinedOutput(); err != nil {
			t.Fatalf("the installed compiler cannot apply: %v\n%s", err, output)
		}
		applied, _ := plainRecord(t)["last_applied"].(map[string]any)
		if !reflect.DeepEqual(applied["compiler_build"], build.Version.Build) {
			t.Fatalf("the record holds %v, and the build record %v", applied["compiler_build"], build.Version.Build)
		}
		// On a machine that reports aarch64 the installer refuses before any download.
		machine := filepath.Join(root, "aarch64")
		if err := os.Mkdir(machine, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(machine, "sitecustomize.py"), []byte("import platform\nplatform.machine = lambda: 'aarch64'\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(bin, "calls.jsonl")); err != nil {
			t.Fatal(err)
		}
		refused := filepath.Join(root, "refused")
		log, err := install(refused, []string{"--release"}, "PYTHONPATH="+machine)
		if err == nil || !strings.Contains(log, "This compiler package requires Linux amd64") {
			t.Fatalf("the installer does not refuse aarch64: %v\n%s", err, log)
		}
		if _, err := os.Stat(filepath.Join(bin, "calls.jsonl")); !os.IsNotExist(err) {
			t.Fatal("the installer called the CI provider's client on aarch64")
		}
		if _, err := os.Stat(refused); !os.IsNotExist(err) {
			t.Fatal("the refused installation left a result folder")
		}
	})
}
