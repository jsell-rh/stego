package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/fill"
	"github.com/jsell-rh/stego/internal/registry"
)

// The fixtures of Extending's system tests. They run the stego binary from
// outside, in a project folder, as the service author does.
//
// fillKeyFixture is ST-EXT-TD02: two SSH keys and two GPG keys made new for
// the test run, a SIGNERS file that lists listed-ssh and listed-gpg only, and
// one HOME folder for each key, whose git config names it as the signing key.
//
// fillProjectFixture is ST-EXT-TD01: a rest-crud project from stego init
// with the collections org-users and org-groups, SIGNERS at its root, and one
// fill at each qualification standing. It is made once; each test copies it.

type fillKeys struct {
	dir, gnupgHome                  string
	listedSSH, unlistedSSH          string
	listedGPG, keyringGPG           string
	signers                         string
	homes                           map[string]string
	goEnv                           []string
	registry                        string
	listedSSHPublic, unlistedPublic string
}

var fillKeyOnce onceFixture
var fillKeySet fillKeys

// testFillKeys makes the keys of ST-EXT-TD02 once per test run.
func testFillKeys(t *testing.T) *fillKeys {
	t.Helper()
	fillKeyOnce.get(t, func() (string, error) {
		k := &fillKeySet
		k.dir = filepath.Join(fixtureRoot, "fill-keys")
		k.gnupgHome = filepath.Join(k.dir, "gnupg")
		if err := os.MkdirAll(k.gnupgHome, 0700); err != nil {
			return "", err
		}
		run := func(name string, args ...string) (string, error) {
			cmd := exec.Command(name, args...)
			cmd.Env = append(os.Environ(), "GNUPGHOME="+k.gnupgHome)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return "", fmt.Errorf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
			}
			return string(out), nil
		}
		k.listedSSH, k.unlistedSSH = filepath.Join(k.dir, "listed-ssh"), filepath.Join(k.dir, "unlisted-ssh")
		for _, key := range []string{k.listedSSH, k.unlistedSSH} {
			if _, err := run("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", filepath.Base(key), "-f", key); err != nil {
				return "", err
			}
		}
		for _, name := range []string{"listed-gpg", "keyring-gpg"} {
			if _, err := run("gpg", "--batch", "--passphrase", "", "--quick-gen-key", name+" <"+name+"@example.com>", "ed25519", "sign", "never"); err != nil {
				return "", err
			}
		}
		defer run("gpgconf", "--kill", "gpg-agent")
		fingerprint := func(name string) (string, error) {
			out, err := run("gpg", "--batch", "--with-colons", "--list-keys", name+"@example.com")
			if err != nil {
				return "", err
			}
			for _, line := range strings.Split(out, "\n") {
				if strings.HasPrefix(line, "fpr:") {
					return strings.Split(line, ":")[9], nil
				}
			}
			return "", fmt.Errorf("no fingerprint for %s", name)
		}
		var err error
		if k.listedGPG, err = fingerprint("listed-gpg"); err != nil {
			return "", err
		}
		if k.keyringGPG, err = fingerprint("keyring-gpg"); err != nil {
			return "", err
		}
		public := func(key string) (string, error) {
			data, err := os.ReadFile(key + ".pub")
			if err != nil {
				return "", err
			}
			fields := strings.Fields(string(data))
			return fields[0] + " " + fields[1], nil
		}
		if k.listedSSHPublic, err = public(k.listedSSH); err != nil {
			return "", err
		}
		if k.unlistedPublic, err = public(k.unlistedSSH); err != nil {
			return "", err
		}
		armored, err := run("gpg", "--batch", "--armor", "--export", k.listedGPG)
		if err != nil {
			return "", err
		}
		k.signers = "# SSH keys, in the SSH allowed signers format\nlisted@example.com " + k.listedSSHPublic + "\n\n# GPG keys\nlisted-gpg@example.com gpg\n" + armored
		k.homes = map[string]string{}
		for name, config := range map[string][][2]string{
			"listed-ssh":   {{"gpg.format", "ssh"}, {"user.signingkey", k.listedSSH}},
			"unlisted-ssh": {{"gpg.format", "ssh"}, {"user.signingkey", k.unlistedSSH}},
			"listed-gpg":   {{"gpg.format", "openpgp"}, {"user.signingkey", k.listedGPG}},
			"no-key":       nil,
		} {
			home := filepath.Join(k.dir, "home-"+name)
			if err := os.MkdirAll(home, 0700); err != nil {
				return "", err
			}
			for _, pair := range config {
				cmd := exec.Command("git", "config", "--global", pair[0], pair[1])
				cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
				if out, err := cmd.CombinedOutput(); err != nil {
					return "", fmt.Errorf("git config: %v\n%s", err, out)
				}
			}
			k.homes[name] = home
		}
		out, err := exec.Command(pinnedGo(t), "env", "GOMODCACHE", "GOCACHE", "GOPATH").Output()
		if err != nil {
			return "", err
		}
		values := strings.Split(strings.TrimSpace(string(out)), "\n")
		k.goEnv = []string{"GOMODCACHE=" + values[0], "GOCACHE=" + values[1], "GOPATH=" + values[2]}
		// The fill fixture is a rest-crud project of the GORM store path until the store path is removed
		// (ST-SUP-RC20-OC39).
		k.registry = filepath.Join(fixtureRoot, "fill-store-registry")
		if err := shippedRegistryInto(k.registry); err != nil {
			return "", err
		}
		return "", nil
	})
	return &fillKeySet
}

// env gives the environment of a run of stego as the holder of one key of
// ST-EXT-TD02, or of no key: its own HOME, the test keyring, no ssh-agent,
// the built-in registry and the Go settings of the test run.
func (k *fillKeys) env(home string) []string {
	var env []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "HOME", "GNUPGHOME", "SSH_AUTH_SOCK", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "GOFLAGS", "GOWORK", "GOTOOLCHAIN", "GOMODCACHE", "GOCACHE", "GOPATH",
			"STEGO_REGISTRY", "STEGO_MODULE", "STEGO_GO_VERSION", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM":
			continue
		}
		env = append(env, entry)
	}
	env = append(env, k.goEnv...)
	return append(env, "HOME="+home, "GNUPGHOME="+k.gnupgHome, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"), "XDG_CACHE_HOME="+filepath.Join(home, ".cache"),
		"GOWORK=off", "GOTOOLCHAIN=local", "GOFLAGS=-mod=mod", "GIT_CONFIG_NOSYSTEM=1",
		"STEGO_REGISTRY="+k.registry, "STEGO_MODULE=example.com/fills", "STEGO_GO_VERSION=1.26.11")
}

// fillProject is one copy of the project of ST-EXT-TD01.
type fillProject struct {
	dir  string
	keys *fillKeys
	// registry is the active registry of the project, when it is not the
	// built-in one.
	registry string
	// declarationExtra is added to the declaration that bind writes.
	declarationExtra string
}

// stego runs the stego binary in the project as the holder of the key, and
// gives what it printed on standard output and standard error.
func (p *fillProject) stego(t *testing.T, key string, args ...string) (string, string, error) {
	t.Helper()
	command := exec.Command(stegoBinary(t), args...)
	command.Dir = p.dir
	home, ok := p.keys.homes[key]
	if !ok {
		t.Fatalf("no home for the key %q", key)
	}
	command.Env = p.keys.env(home)
	if p.registry != "" {
		command.Env = append(command.Env, "STEGO_REGISTRY="+p.registry)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if strings.HasPrefix(key, "listed-gpg") {
		stop := exec.Command("gpgconf", "--kill", "gpg-agent")
		stop.Env = append(os.Environ(), "GNUPGHOME="+p.keys.gnupgHome)
		_ = stop.Run()
	}
	return stdout.String(), stderr.String(), err
}

// mustStegoIn runs stego in the project and fails the test when it fails.
func (p *fillProject) must(t *testing.T, key string, args ...string) string {
	t.Helper()
	stdout, stderr, err := p.stego(t, key, args...)
	if err != nil {
		t.Fatalf("stego %s: %v\n%s\n%s", strings.Join(args, " "), err, stdout, stderr)
	}
	return stdout
}

// fillBaseService is the declaration of ST-EXT-TD01 with no slot binding.
var fillBaseService = onRuntimePath(`kind: service
name: fills
archetype: rest-crud
language: go
entities:
  - name: User
    fields:
      - {name: email, type: string}
  - name: Group
    fields:
      - {name: label, type: string}
collections:
  org-users:
    entity: User
    operations: [create, read, delete]
  org-groups:
    entity: Group
    operations: [create, read, delete]
`)

// slotBinding is one binding of a fill at a slot for a collection.
type slotBinding struct{ fill, slot, collection string }

// bind writes the declaration with the bindings, each in a gate.
func (p *fillProject) bind(t *testing.T, bindings ...slotBinding) {
	t.Helper()
	service := fillBaseService + p.declarationExtra
	if len(bindings) > 0 {
		service += "slots:\n"
		for _, b := range bindings {
			service += fmt.Sprintf("  - {slot: %s, collection: %s, gate: [%s]}\n", b.slot, b.collection, b.fill)
		}
	}
	p.write(t, "service.yaml", service)
}

func (p *fillProject) path(name string) string { return filepath.Join(p.dir, filepath.FromSlash(name)) }

func (p *fillProject) read(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(p.path(name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func (p *fillProject) write(t *testing.T, name, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p.path(name)), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.path(name), []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
	withRuntimeSchema(t, p.dir, name, data)
}

// replace changes the first match of old in the file, which must hold it.
func (p *fillProject) replace(t *testing.T, name, old, new string) {
	t.Helper()
	data := string(p.read(t, name))
	if !strings.Contains(data, old) {
		t.Fatalf("%s does not hold %q", name, old)
	}
	p.write(t, name, strings.Replace(data, old, new, 1))
}

// manifest reads a fill manifest of the project.
func (p *fillProject) manifest(t *testing.T, name string) *fill.Fill {
	t.Helper()
	f, err := fill.ParseFill(p.path("fills/" + name + "/fill.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// tree gives the digest of each file under the folders of the project.
func (p *fillProject) tree(t *testing.T, folders ...string) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, folder := range folders {
		root := p.path(folder)
		err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			data, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			relative, _ := filepath.Rel(p.dir, name)
			files[filepath.ToSlash(relative)] = hex.EncodeToString(sum[:])
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return files
}

// fillSource gives the files of a fill for a slot of rest-api. Its code
// imports only out/slots and the standard library, and its test passes.
func fillSource(name, slot, collection string, test bool) map[string]string {
	pkg := strings.ReplaceAll(name, "-", "_")
	kind := map[string]string{"before_create": "BeforeCreate", "before_delete": "BeforeDelete"}[slot]
	files := map[string]string{
		"fill.yaml": fmt.Sprintf("kind: fill\nname: %s\nimplements: rest-api.%s\ncollection: %s\n", name, slot, collection),
		"policy.go": fmt.Sprintf(`package %[1]s

import (
	"context"

	slots "example.com/fills/out/slots"
)

// Policy allows each request.
type Policy struct{}

// New makes the policy.
func New() *Policy { return &Policy{} }

var _ slots.%[2]sSlot = (*Policy)(nil)

// Evaluate allows the request.
func (p *Policy) Evaluate(ctx context.Context, req *slots.%[2]sRequest) (*slots.SlotResult, error) {
	return &slots.SlotResult{Ok: true}, nil
}
`, pkg, kind),
	}
	if test {
		files["policy_test.go"] = fmt.Sprintf(`package %[1]s

import (
	"context"
	"testing"

	slots "example.com/fills/out/slots"
)

func TestPolicyAllows(t *testing.T) {
	result, err := New().Evaluate(context.Background(), &slots.%[2]sRequest{})
	if err != nil || result == nil || !result.Ok {
		t.Fatalf("the policy denied: %%v", err)
	}
}
`, pkg, kind)
	}
	return files
}

func (p *fillProject) addFill(t *testing.T, name, slot, collection string, test bool) {
	t.Helper()
	for file, data := range fillSource(name, slot, collection, test) {
		p.write(t, "fills/"+name+"/"+file, data)
	}
}

var fillProjectOnce onceFixture
var fillProjectMaster string

// newFillProject gives a new copy of the project of ST-EXT-TD01.
func newFillProject(t *testing.T) *fillProject {
	t.Helper()
	keys := testFillKeys(t)
	fillProjectOnce.get(t, func() (string, error) {
		p := &fillProject{dir: filepath.Join(fixtureRoot, "fill-project", "fills"), keys: keys}
		if err := os.MkdirAll(p.dir, 0755); err != nil {
			return "", err
		}
		run := func(key string, args ...string) error {
			if stdout, stderr, err := p.stego(t, key, args...); err != nil {
				return fmt.Errorf("stego %s: %v\n%s\n%s", strings.Join(args, " "), err, stdout, stderr)
			}
			return nil
		}
		if err := run("no-key", "init", "--archetype", "rest-crud"); err != nil {
			return "", err
		}
		p.bind(t)
		p.write(t, "SIGNERS", keys.signers)
		for _, f := range []struct{ name, slot string }{
			{"qualified-fill", "before_create"}, {"stale-fill", "before_create"}, {"unsigned-fill", "before_create"},
			{"unlisted-fill", "before_create"}, {"other-slot-fill", "before_delete"},
		} {
			p.addFill(t, f.name, f.slot, "org-users", true)
		}
		if err := run("no-key", "apply"); err != nil {
			return "", err
		}
		if err := run("no-key", "test"); err != nil {
			return "", err
		}
		for fillName, key := range map[string]string{"qualified-fill": "listed-ssh", "stale-fill": "listed-ssh", "other-slot-fill": "listed-ssh", "unlisted-fill": "unlisted-ssh"} {
			if err := run(key, "fill", "sign", fillName); err != nil {
				return "", err
			}
		}
		p.replace(t, "fills/stale-fill/policy.go", "// Policy allows each request.", "// Policy allows each request, as before.")
		p.addFill(t, "untested-fill", "before_create", "org-users", true)
		p.addFill(t, "untested-fill-2", "before_create", "org-groups", true)
		p.addFill(t, "no-tests-fill", "before_create", "org-users", false)
		fillProjectMaster = p.dir
		return "", nil
	})
	return &fillProject{dir: copyFolder(t, fillProjectMaster), keys: keys}
}

// sshVerify verifies an SSH sign-off outside STEGO, with ssh-keygen -Y verify
// and an allowed signers file that holds only the public key given.
func sshVerify(t *testing.T, public string, signOff *fill.FillSignOff, digest string) error {
	t.Helper()
	dir := t.TempDir()
	allowed := filepath.Join(dir, "allowed")
	signature := filepath.Join(dir, "signature")
	if err := os.WriteFile(allowed, []byte("person "+public+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(signature, []byte(signOff.Signature.Armored), 0600); err != nil {
		t.Fatal(err)
	}
	verify := exec.Command("ssh-keygen", "-Y", "verify", "-f", allowed, "-I", "person", "-n", fill.SSHNamespace, "-s", signature)
	verify.Stdin = strings.NewReader(digest)
	if out, err := verify.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}

func evidenceDigest(t *testing.T, f *fill.Fill) string {
	t.Helper()
	digest, err := fill.EvidenceDigest(f)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

// statusOf gives the line of fill status for the fill.
func statusOf(t *testing.T, status, name string) string {
	t.Helper()
	for _, line := range strings.Split(status, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == name {
			return line
		}
	}
	t.Fatalf("fill status shows no line for %s:\n%s", name, status)
	return ""
}

// expectStanding fails the test unless fill status shows the fill with the
// standing.
func expectStanding(t *testing.T, status, name string, standing fill.Standing) string {
	t.Helper()
	line := statusOf(t, status, name)
	if fields := strings.Fields(line); len(fields) < 2 || fields[1] != string(standing) {
		t.Fatalf("fill status shows %s not as %s:\n%s", name, standing, status)
	}
	return line
}

func TestFillQualification(t *testing.T) {
	t.Run("ST-EXT-UC02-R01-T01 stego test writes evidence with both digests into the fill manifest of each fill whose tests pass", func(t *testing.T) {
		p := newFillProject(t)
		contract := contractDigest(t, p, "before_create")
		want := map[string]string{}
		for _, name := range []string{"untested-fill", "untested-fill-2"} {
			digest, err := fill.Digest(p.path("fills/" + name))
			if err != nil {
				t.Fatal(err)
			}
			want[name] = digest
		}
		p.must(t, "no-key", "test")
		for name, digest := range want {
			f := p.manifest(t, name)
			if f.Evidence == nil || f.Evidence.FillDigest != digest || f.Evidence.ContractDigest != contract {
				t.Fatalf("%s has no evidence with its fill digest %s and the contract digest %s: %+v", name, digest, contract, f.Evidence)
			}
		}
	})

	t.Run("ST-EXT-UC02-R01-T02 stego test writes no evidence for a fill whose test fails, and still writes it for the fill whose tests pass", func(t *testing.T) {
		p := newFillProject(t)
		p.replace(t, "fills/untested-fill/policy_test.go", "if err != nil", "if true")
		before := p.read(t, "fills/untested-fill/fill.yaml")
		stdout, stderr, err := p.stego(t, "no-key", "test")
		if err == nil || !strings.Contains(stdout+stderr, "untested-fill") {
			t.Fatalf("stego test passed or does not name untested-fill: %v\n%s\n%s", err, stdout, stderr)
		}
		if !bytes.Equal(before, p.read(t, "fills/untested-fill/fill.yaml")) {
			t.Fatal("stego test changed the manifest of the failing fill")
		}
		if p.manifest(t, "untested-fill-2").Evidence == nil {
			t.Fatal("stego test wrote no evidence for untested-fill-2")
		}
	})

	t.Run("ST-EXT-UC02-R01-T03 A failed run leaves the earlier evidence of a fill as it was, and the fill shows as stale", func(t *testing.T) {
		p := newFillProject(t)
		p.replace(t, "fills/unsigned-fill/policy_test.go", "if err != nil", "if true")
		before := p.read(t, "fills/unsigned-fill/fill.yaml")
		if _, _, err := p.stego(t, "no-key", "test"); err == nil {
			t.Fatal("stego test passed with a failing test")
		}
		if !bytes.Equal(before, p.read(t, "fills/unsigned-fill/fill.yaml")) {
			t.Fatal("stego test changed the manifest of the failing fill")
		}
		p.bind(t, slotBinding{"unsigned-fill", "before_create", "org-users"})
		expectStanding(t, p.must(t, "no-key", "fill", "status"), "unsigned-fill", fill.Stale)
	})

	t.Run("ST-EXT-UC02-R02-T01 stego fill sign signs the current evidence with the person's SSH key", func(t *testing.T) {
		p := newFillProject(t)
		p.must(t, "listed-ssh", "fill", "sign", "unsigned-fill")
		f := p.manifest(t, "unsigned-fill")
		digest := evidenceDigest(t, f)
		if f.SignOff == nil || f.SignOff.EvidenceDigest != digest {
			t.Fatalf("the sign-off does not name the evidence digest %s: %+v", digest, f.SignOff)
		}
		if err := sshVerify(t, p.keys.listedSSHPublic, f.SignOff, digest); err != nil {
			t.Fatalf("ssh-keygen -Y verify refuses the sign-off: %v", err)
		}
	})

	t.Run("ST-EXT-UC02-R02-T02 stego fill sign signs the current evidence with the person's GPG key", func(t *testing.T) {
		p := newFillProject(t)
		p.must(t, "listed-gpg", "fill", "sign", "unsigned-fill")
		f := p.manifest(t, "unsigned-fill")
		digest := evidenceDigest(t, f)
		if f.SignOff == nil || f.SignOff.EvidenceDigest != digest {
			t.Fatalf("the sign-off does not name the evidence digest %s: %+v", digest, f.SignOff)
		}
		signature := filepath.Join(t.TempDir(), "signature.asc")
		if err := os.WriteFile(signature, []byte(f.SignOff.Signature.Armored), 0600); err != nil {
			t.Fatal(err)
		}
		verify := exec.Command("gpg", "--batch", "--status-fd", "1", "--verify", signature, "-")
		verify.Env = append(os.Environ(), "GNUPGHOME="+p.keys.gnupgHome)
		verify.Stdin = strings.NewReader(digest)
		out, err := verify.Output()
		stop := exec.Command("gpgconf", "--kill", "gpg-agent")
		stop.Env = verify.Env
		_ = stop.Run()
		if err != nil || !strings.Contains(string(out), "VALIDSIG") || !strings.Contains(string(out), p.keys.listedGPG) {
			t.Fatalf("gpg --verify does not accept the sign-off as made by listed-gpg: %v\n%s", err, out)
		}
	})

	t.Run("ST-EXT-UC02-R02-T04 Plan accepts a fill signed by a key that SIGNERS lists", func(t *testing.T) {
		p := newFillProject(t)
		p.bind(t, slotBinding{"qualified-fill", "before_create", "org-users"})
		stdout := p.must(t, "no-key", "plan")
		if strings.Contains(stdout, "qualified-fill") {
			t.Fatalf("plan reports a fault for qualified-fill:\n%s", stdout)
		}
	})

	t.Run("ST-EXT-UC02-R04-T01 With no signing key set up, stego fill sign refuses and makes no key", func(t *testing.T) {
		p := newFillProject(t)
		gnupg := filepath.Join(t.TempDir(), "gnupg")
		if err := os.Mkdir(gnupg, 0700); err != nil {
			t.Fatal(err)
		}
		home := p.keys.homes["no-key"]
		listing := func() string {
			return fmt.Sprint(listFiles(t, p.dir), listFiles(t, home), listFiles(t, gnupg))
		}
		before := listing()
		manifest := p.read(t, "fills/unsigned-fill/fill.yaml")
		command := exec.Command(stegoBinary(t), "fill", "sign", "unsigned-fill")
		command.Dir = p.dir
		command.Env = append(p.keys.env(home), "GNUPGHOME="+gnupg)
		out, err := command.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "no signing key is set up") {
			t.Fatalf("fill sign did not refuse with no signing key: %v\n%s", err, out)
		}
		if !bytes.Equal(manifest, p.read(t, "fills/unsigned-fill/fill.yaml")) {
			t.Fatal("fill sign changed the manifest")
		}
		if after := listing(); after != before {
			t.Fatalf("fill sign added or changed files:\nbefore: %s\nafter:  %s", before, after)
		}
	})

	t.Run("ST-EXT-UC02-R04-T02 A sign-off by stego fill sign verifies with the person's public key alone, and the run stores no key", func(t *testing.T) {
		p := newFillProject(t)
		home := p.keys.homes["listed-ssh"]
		before := map[string]map[string]string{"project": listFiles(t, p.dir), "home": listFiles(t, home)}
		p.must(t, "listed-ssh", "fill", "sign", "unsigned-fill")
		after := map[string]map[string]string{"project": listFiles(t, p.dir), "home": listFiles(t, home)}
		var changed []string
		for place, files := range after {
			for name, digest := range files {
				if before[place][name] != digest {
					changed = append(changed, place+":"+name)
				}
			}
			for name := range before[place] {
				if _, ok := files[name]; !ok {
					changed = append(changed, place+":"+name)
				}
			}
		}
		sort.Strings(changed)
		if fmt.Sprint(changed) != "[project:fills/unsigned-fill/fill.yaml]" {
			t.Fatalf("the run added or changed other files than fill.yaml: %v", changed)
		}
		f := p.manifest(t, "unsigned-fill")
		if err := sshVerify(t, p.keys.listedSSHPublic, f.SignOff, evidenceDigest(t, f)); err != nil {
			t.Fatalf("the sign-off does not verify with the public key of listed-ssh alone: %v", err)
		}
	})
}

// listFiles gives the digest of each file under the folder, by its path in
// the folder.
func listFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(root, name)
		if entry.IsDir() {
			files[filepath.ToSlash(relative)+"/"] = "folder"
			return nil
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		files[filepath.ToSlash(relative)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// contractDigest gives the contract digest of a slot of rest-api in the
// built-in registry, as fill.ContractDigest computes it from the registry.
func contractDigest(t *testing.T, p *fillProject, slot string) string {
	t.Helper()
	reg, err := registry.LoadDirectories(p.keys.registry)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := fill.ContractDigest(reg, "components/rest-api/slots/"+slot+".proto")
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestFillEvidenceInvariants(t *testing.T) {
	t.Parallel()
	t.Run("ST-EXT-INV02-T Recorded evidence holds the fill digest and the contract digest of the passing run, and keeps them after a later change", func(t *testing.T) {
		p := newFillProject(t)
		fillDigest, err := fill.Digest(p.path("fills/untested-fill"))
		if err != nil {
			t.Fatal(err)
		}
		contract := contractDigest(t, p, "before_create")
		p.must(t, "no-key", "test")
		p.replace(t, "fills/untested-fill/policy.go", "// Policy allows each request.", "// Policy allows each request, changed.")
		f := p.manifest(t, "untested-fill")
		if f.Evidence == nil || f.Evidence.FillDigest != fillDigest || f.Evidence.ContractDigest != contract {
			t.Fatalf("the evidence does not hold the digests %s and %s of the run: %+v", fillDigest, contract, f.Evidence)
		}
		for _, digest := range []string{f.Evidence.FillDigest, f.Evidence.ContractDigest} {
			if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
				t.Fatalf("the digest %q is not 64 lower-case hexadecimal digits", digest)
			}
		}
		manifest := string(p.read(t, "fills/untested-fill/fill.yaml"))
		for field, line := range map[string]string{"fill_digest": "  fill_digest: " + fillDigest + "\n", "contract_digest": "  contract_digest: " + contract + "\n"} {
			if !strings.Contains(manifest, line) {
				t.Fatalf("the manifest does not hold %q:\n%s", line, manifest)
			}
			p.write(t, "fills/broken/fill.yaml", strings.Replace(manifest, line, "", 1))
			if _, err := fill.ParseFill(p.path("fills/broken/fill.yaml")); err == nil || !strings.Contains(err.Error(), field) {
				t.Fatalf("the reader accepts evidence with no %s, or does not name it: %v", field, err)
			}
		}
	})

	t.Run("ST-EXT-INV03-T A sign-off is good only with a signature over the digest of its evidence, and new evidence leaves the old sign-off signing nothing", func(t *testing.T) {
		p := newFillProject(t)
		p.must(t, "listed-ssh", "fill", "sign", "unsigned-fill")
		f := p.manifest(t, "unsigned-fill")
		digest := evidenceDigest(t, f)
		if f.SignOff == nil || f.SignOff.EvidenceDigest != digest {
			t.Fatalf("the sign-off does not hold the evidence digest %s", digest)
		}
		if err := sshVerify(t, p.keys.listedSSHPublic, f.SignOff, digest); err != nil {
			t.Fatalf("ssh-keygen -Y verify refuses the sign-off: %v", err)
		}
		signers, err := fill.ReadSigners(p.dir)
		if err != nil {
			t.Fatal(err)
		}
		current, err := fill.Digest(p.path("fills/unsigned-fill"))
		if err != nil {
			t.Fatal(err)
		}
		other := strings.Repeat("0", 64)
		sign := exec.Command("ssh-keygen", "-Y", "sign", "-n", fill.SSHNamespace, "-f", p.keys.listedSSH)
		sign.Stdin = strings.NewReader(other)
		otherSignature, err := sign.Output()
		if err != nil {
			t.Fatal(err)
		}
		copied := *f
		copiedSignOff := *f.SignOff
		copiedSignOff.Signature.Armored = string(otherSignature)
		copied.SignOff = &copiedSignOff
		if _, err := fill.VerifySignOff(copied.SignOff, digest, signers); err == nil || !strings.Contains(err.Error(), fill.ErrNotCovered.Error()) {
			t.Fatalf("the copy's signature over another digest is not refused as not covering its evidence: %v", err)
		}
		if fill.IsQualified(&copied, current, f.Evidence.ContractDigest, signers) {
			t.Fatal("the copy is qualified")
		}
		p.replace(t, "fills/unsigned-fill/policy.go", "// Policy allows each request.", "// Policy allows each request, changed.")
		p.must(t, "no-key", "test")
		renewed := p.manifest(t, "unsigned-fill")
		if renewed.SignOff == nil {
			t.Fatal("the new evidence removed the old sign-off")
		}
		newDigest := evidenceDigest(t, renewed)
		if _, err := fill.VerifySignOff(renewed.SignOff, newDigest, signers); err == nil || !strings.Contains(err.Error(), fill.ErrOtherEvidence.Error()) {
			t.Fatalf("the old sign-off is not refused as signing other evidence: %v", err)
		}
		if fill.IsQualified(renewed, renewed.Evidence.FillDigest, renewed.Evidence.ContractDigest, signers) {
			t.Fatal("the fill with new evidence and the old sign-off is qualified")
		}
	})

	t.Run("ST-EXT-INV04-T A fill with no test file gets no evidence, and STEGO names it", func(t *testing.T) {
		p := newFillProject(t)
		before := p.read(t, "fills/no-tests-fill/fill.yaml")
		stdout := p.must(t, "no-key", "test")
		if !strings.Contains(stdout, "fill no-tests-fill: no evidence recorded: it has no test file") {
			t.Fatalf("stego test does not name no-tests-fill with the reason that it has no test file:\n%s", stdout)
		}
		if !bytes.Equal(before, p.read(t, "fills/no-tests-fill/fill.yaml")) {
			t.Fatal("stego test changed the manifest of no-tests-fill")
		}
		if p.manifest(t, "untested-fill").Evidence == nil {
			t.Fatal("untested-fill got no evidence")
		}
		p.write(t, "fills/no-tests-fill/notes_test.txt", "These are not tests.\n")
		stdout = p.must(t, "no-key", "test")
		if !strings.Contains(stdout, "fill no-tests-fill: no evidence recorded: it has no test file") {
			t.Fatalf("stego test does not name no-tests-fill with the reason that it has no test file:\n%s", stdout)
		}
		if !bytes.Equal(before, p.read(t, "fills/no-tests-fill/fill.yaml")) {
			t.Fatal("stego test changed the manifest of no-tests-fill")
		}
	})
}
