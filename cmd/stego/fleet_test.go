package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// fleetFixture is the test data set ST-DIS-TD02: six service checkouts and a registry with two advisories. Each
// recorded checkout is a copy of the small service that stego apply made (ST-GEN-TD03), with the pins of the record
// set to the fleet's components, so each record has the format the compiler writes and passes stego drift. Each
// checkout is a Git repository with one commit. Each digest is 64 lower-case hexadecimal digits, and each lifecycle
// date is relative to the day T the test runs.
type fleetFixture struct {
	*smallService
	checkouts map[string]string
	order     []string
	// advisories maps the test data set's names ADV-2 and ADV-3 to the ids the registry gave them.
	advisories map[string]string
}

var (
	fleetD1 = strings.Repeat("d1", 32)
	fleetD2 = strings.Repeat("d2", 32)
	fleetE1 = strings.Repeat("e1", 32)
	fleetF1 = strings.Repeat("f1", 32)
)

type fleetPin struct{ component, version, digest string }

func newFleet(t *testing.T) *fleetFixture {
	t.Helper()
	s := newSmallService(t)
	f := &fleetFixture{smallService: s, checkouts: map[string]string{}, advisories: map[string]string{}}
	pins := map[string][]fleetPin{
		"alpha":   {{"component-a", "1.2.0", fleetD1}, {"component-c", "2.0.0", fleetF1}},
		"bravo":   {{"component-a", "1.2.0", fleetD1}, {"component-b", "0.9.0", fleetE1}},
		"charlie": {{"component-a", "1.3.0", fleetD2}, {"component-b", "0.9.0", fleetE1}},
		"delta":   {{"component-c", "2.0.0", fleetF1}},
	}
	for _, name := range []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"} {
		folder := filepath.Join(s.parent, name)
		f.checkouts[name], f.order = folder, append(f.order, folder)
		switch name {
		case "echo":
			// Generated output under out and no record.
			if err := os.MkdirAll(folder, 0o755); err != nil {
				t.Fatal(err)
			}
			if output, err := exec.Command("cp", "-a", filepath.Join(s.service, "out"), filepath.Join(folder, "out")).CombinedOutput(); err != nil {
				t.Fatalf("copy the output: %v\n%s", err, output)
			}
		default:
			if output, err := exec.Command("cp", "-a", s.service, folder).CombinedOutput(); err != nil {
				t.Fatalf("copy the service: %v\n%s", err, output)
			}
			if err := os.RemoveAll(filepath.Join(folder, ".git")); err != nil {
				t.Fatal(err)
			}
			f.rewriteRecord(t, folder, name, pins[name])
		}
		git(t, folder, "init", "-q")
		git(t, folder, "add", "-A")
		git(t, folder, "commit", "-q", "-m", name)
		if pins[name] != nil {
			if stdout, stderr, err := f.stegoIn(t, folder, "drift"); err != nil {
				t.Fatalf("the record of %s does not pass stego drift: %v\n%s%s", name, err, stdout, stderr)
			}
		}
	}
	// The registry's advisories: ADV-2 supersedes D1 by D2, failing after T+90; ADV-3 withdraws E1, failing after
	// T+60. stego lifecycle supersede needs the component stage that the registry does not state yet, so the
	// fixture writes ADV-2 as the command writes an advisory.
	f.advisories["ADV-2"] = "STEGO-2026-9002"
	if err := os.MkdirAll(filepath.Join(s.registry, "lifecycle"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(s.registry, "lifecycle"), "STEGO-2026-9002.json", fmt.Sprintf(`{
  "schema_version": "1.6.0",
  "id": "STEGO-2026-9002",
  "modified": "2026-11-01T09:00:00Z",
  "published": "2026-11-01T09:00:00Z",
  "summary": "component-a 1.3.0 supersedes 1.2.0",
  "affected": [{"package": {"ecosystem": "STEGO", "name": "component-a"},
    "ranges": [{"type": "ECOSYSTEM", "events": [{"introduced": "%s"}, {"fixed": "%s"}]}]}],
  "database_specific": {"stego": {"kind": "supersession", "fails_after": "%s",
    "needs_declaration_change": false, "needs_fill_change": false}}
}
`, fleetD1, fleetD2, lifecycleDay(90)), 0o644)
	out := mustStego(t, "lifecycle", "withdraw", "--registry", s.registry, "--component", "component-b", "--digest", fleetE1,
		"--fails-after", lifecycleDay(60), "--reason", "leaks a token in a log")
	f.advisories["ADV-3"] = strings.TrimSuffix(strings.Fields(out)[2], ":")
	git(t, s.registry, "add", "-A")
	git(t, s.registry, "commit", "-q", "-m", "advisories")
	return f
}

// rewriteRecord sets the service name and the pins of a copied record, or its format version for foxtrot.
func (f *fleetFixture) rewriteRecord(t *testing.T, folder, name string, pins []fleetPin) {
	t.Helper()
	path := filepath.Join(folder, ".stego", "state.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := yaml.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if name == "foxtrot" {
		record["format_version"] = 99
	} else {
		applied := record["last_applied"].(map[string]any)
		applied["service_name"] = name
		components := map[string]any{}
		for _, pin := range pins {
			components[pin.component] = map[string]any{"version": pin.version, "sha": pin.digest}
		}
		applied["components"] = components
	}
	data, err = yaml.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *fleetFixture) stegoIn(t *testing.T, folder string, args ...string) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, stegoBinary(t), args...)
	command.Dir, command.Env = folder, f.env
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}

func (f *fleetFixture) paths(names ...string) []string {
	var result []string
	for _, name := range names {
		result = append(result, f.checkouts[name])
	}
	return result
}

// offline runs the stego binary so that a network call cannot pass unseen. Where it can, it runs the binary in a
// network namespace with no interface but loopback, as an unprivileged user: bwrap --unshare-net where the kernel
// does not let a user call unshare -n itself, else unshare -rn. Where neither runs (a CI runner whose kernel
// refuses user namespaces and that has no bwrap), strace alone watches the run, and checkStartsOnlyStegoAndGit
// fails on any network socket it opens. strace also records each process the run starts.
func offline(t *testing.T, dir string, env []string, args ...string) (string, []string) {
	t.Helper()
	binary := stegoBinary(t)
	var sandbox []string
	switch {
	case exec.Command("bwrap", "--unshare-net", "--dev-bind", "/", "/", "true").Run() == nil:
		sandbox = []string{"bwrap", "--unshare-net", "--dev-bind", "/", "/"}
	case exec.Command("unshare", "-rn", "true").Run() == nil:
		sandbox = []string{"unshare", "-rn"}
	}
	_, straceErr := exec.LookPath("strace")
	if sandbox == nil && straceErr != nil {
		t.Fatal("the test needs a network namespace or strace: neither bwrap --unshare-net, unshare -rn nor strace runs here")
	}
	if sandbox == nil {
		t.Log("no network namespace here: strace alone shows that the run opens no network socket")
	}
	trace := ""
	command := append(slices.Clone(sandbox), binary)
	if straceErr == nil {
		trace = filepath.Join(t.TempDir(), "trace")
		command = append(slices.Clone(sandbox), "strace", "-f", "-qq", "-e", "trace=execve,socket,connect", "-o", trace, binary)
	}
	run := exec.Command(command[0], append(command[1:], args...)...)
	run.Dir, run.Env = dir, env
	var stdout, stderr bytes.Buffer
	run.Stdout, run.Stderr = &stdout, &stderr
	if err := run.Run(); err != nil {
		t.Fatalf("stego %s offline: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	var events []string
	if trace != "" {
		data, err := os.ReadFile(trace)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, "execve(") || strings.Contains(line, "socket(") || strings.Contains(line, "connect(") {
				events = append(events, line)
			}
		}
	}
	return stdout.String(), events
}

// checkStartsOnlyStegoAndGit fails when the traced run started a process other than stego or git, or opened a
// network socket.
func checkStartsOnlyStegoAndGit(t *testing.T, binary string, events []string) {
	t.Helper()
	for _, event := range events {
		switch {
		case strings.Contains(event, "execve("):
			if !strings.Contains(event, `execve("`+binary+`"`) && !strings.Contains(event, "/git\"") {
				t.Errorf("the run started another process: %s", event)
			}
		case strings.Contains(event, "AF_INET"), strings.Contains(event, "AF_NETLINK"):
			t.Errorf("the run opened a network socket: %s", event)
		}
	}
}

// fleetLines is the answer of stego fleet by checkout path: the line of each one.
func fleetLines(t *testing.T, output string) map[string][]string {
	t.Helper()
	lines := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			t.Fatalf("an answer line has no checkout: %q", line)
		}
		path := fields[1]
		if fields[0] != "unknown" {
			path = fields[2]
		}
		lines[strings.TrimSuffix(path, ":")] = append(lines[strings.TrimSuffix(path, ":")], line)
	}
	return lines
}

func TestFindEveryServiceBuiltFromADigest(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	t.Run("ST-DIS-UC02-R01-T01 The fleet query answers from the records alone with no network and no service", func(t *testing.T) {
		args := append([]string{"fleet", "--digest", fleetD1}, f.order...)
		online := mustStego(t, args...)
		answer, events := offline(t, f.parent, f.env, args...)
		if answer != online {
			t.Fatalf("the answer with no network differs:\n%s\nfrom the answer with the network:\n%s", answer, online)
		}
		lines := fleetLines(t, answer)
		for _, name := range []string{"alpha", "bravo"} {
			if len(lines[f.checkouts[name]]) != 1 || !strings.HasPrefix(lines[f.checkouts[name]][0], "pins "+name+" ") {
				t.Errorf("%s is not named as pinning D1: %v", name, lines[f.checkouts[name]])
			}
		}
		checkStartsOnlyStegoAndGit(t, stegoBinary(t), events)
	})
	t.Run("ST-DIS-UC02-R01-T02 The fleet query reads a record by its format version, and a format it does not know is unknown", func(t *testing.T) {
		lines := fleetLines(t, mustStego(t, append([]string{"fleet", "--digest", fleetD1}, f.order...)...))
		foxtrot := lines[f.checkouts["foxtrot"]]
		if len(foxtrot) != 1 || !strings.HasPrefix(foxtrot[0], "unknown "+f.checkouts["foxtrot"]+" format-version 99:") {
			t.Fatalf("foxtrot is not unknown with its path and format version 99: %v", foxtrot)
		}
	})
	t.Run("ST-DIS-UC02-R02-T01 A checkout with generated output and no record is reported as unknown, never left out", func(t *testing.T) {
		lines := fleetLines(t, mustStego(t, append([]string{"fleet", "--digest", fleetD1}, f.order...)...))
		if len(lines) != 6 {
			t.Fatalf("the answer has lines for %d checkouts, not six: %v", len(lines), lines)
		}
		want := map[string]string{"alpha": "pins ", "bravo": "pins ", "charlie": "does-not-pin ", "delta": "does-not-pin ",
			"echo": "unknown ", "foxtrot": "unknown "}
		for name, prefix := range want {
			got := lines[f.checkouts[name]]
			if len(got) != 1 || !strings.HasPrefix(got[0], prefix) {
				t.Errorf("%s: want one line that starts %q, got %v", name, prefix, got)
			}
		}
		if !strings.Contains(lines[f.checkouts["echo"]][0], "generated output") {
			t.Errorf("echo is not reported as generated output with no record: %v", lines[f.checkouts["echo"]])
		}
	})
	t.Run("ST-DIS-UC02-R03-T01 Each affected service is named with the version and the full digest it holds", func(t *testing.T) {
		lines := fleetLines(t, mustStego(t, append([]string{"fleet", "--digest", fleetD1}, f.order...)...))
		for _, name := range []string{"alpha", "bravo"} {
			want := "pins " + name + " " + f.checkouts[name] + " component component-a version 1.2.0 digest " + fleetD1
			if got := lines[f.checkouts[name]]; len(got) != 1 || got[0] != want {
				t.Errorf("%s: got %v, want %q", name, got, want)
			}
		}
		if got := lines[f.checkouts["charlie"]]; len(got) != 1 || strings.HasPrefix(got[0], "pins") {
			t.Errorf("charlie, which holds D2, is named as affected: %v", got)
		}
	})
	t.Run("ST-DIS-UC07-R01-T01 Before a withdrawal, STEGO names every recorded service that pins the digest", func(t *testing.T) {
		lines := fleetLines(t, mustStego(t, append([]string{"fleet", "--digest", fleetE1}, f.order...)...))
		for _, name := range []string{"bravo", "charlie"} {
			want := "pins " + name + " " + f.checkouts[name] + " component component-b version 0.9.0 digest " + fleetE1
			if got := lines[f.checkouts[name]]; len(got) != 1 || got[0] != want {
				t.Errorf("%s: got %v, want %q", name, got, want)
			}
		}
		for _, name := range []string{"echo", "foxtrot"} {
			if got := lines[f.checkouts[name]]; len(got) != 1 || !strings.HasPrefix(got[0], "unknown ") {
				t.Errorf("%s is not named as unknown: %v", name, got)
			}
		}
		for _, name := range []string{"alpha", "delta"} {
			if got := lines[f.checkouts[name]]; len(got) != 1 || strings.HasPrefix(got[0], "pins") {
				t.Errorf("%s is named as pinning E1: %v", name, got)
			}
		}
	})
	vex := func(t *testing.T, names ...string) map[[2]string]map[string]any {
		t.Helper()
		before := tree(t, f.parent)
		output, events := offline(t, f.parent, f.env, append([]string{"fleet", "vex", "--registry", f.registry}, f.paths(names...)...)...)
		checkStartsOnlyStegoAndGit(t, stegoBinary(t), events)
		if after := tree(t, f.parent); !mapsEqual(before, after) {
			t.Fatal("stego fleet vex wrote a file")
		}
		var document struct {
			Context    string           `json:"@context"`
			ID         string           `json:"@id"`
			Author     string           `json:"author"`
			Timestamp  string           `json:"timestamp"`
			Version    int              `json:"version"`
			Statements []map[string]any `json:"statements"`
		}
		decoder := json.NewDecoder(strings.NewReader(output))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&document); err != nil {
			t.Fatalf("the output is not an OpenVEX document: %v\n%s", err, output)
		}
		if document.Context != "https://openvex.dev/ns/v0.2.0" || document.ID == "" || document.Author == "" || document.Version != 1 {
			t.Fatalf("the document lacks the OpenVEX fields: %+v", document)
		}
		if _, err := time.Parse(time.RFC3339, document.Timestamp); err != nil {
			t.Fatalf("the timestamp is not RFC 3339: %v", err)
		}
		statements := map[[2]string]map[string]any{}
		for _, statement := range document.Statements {
			name := statement["vulnerability"].(map[string]any)["name"].(string)
			products := statement["products"].([]any)
			if len(products) != 1 {
				t.Fatalf("a statement names %d products, not one", len(products))
			}
			product := products[0].(map[string]any)["@id"].(string)
			status := statement["status"].(string)
			switch status {
			case "affected":
				if statement["action_statement"] == nil {
					t.Errorf("an affected statement has no action statement: %v", statement)
				}
			case "not_affected":
				if statement["justification"] == nil && statement["impact_statement"] == nil {
					t.Errorf("a not_affected statement has no justification: %v", statement)
				}
			case "fixed", "under_investigation":
			default:
				t.Errorf("the status %q is not an OpenVEX status", status)
			}
			statements[[2]string{name, product}] = statement
		}
		if len(statements) != len(document.Statements) {
			t.Fatalf("two statements name the same advisory and product")
		}
		return statements
	}
	recorded := map[string]map[string]string{
		"ADV-2": {"alpha": "affected", "bravo": "affected", "charlie": "fixed", "delta": "not_affected"},
		"ADV-3": {"alpha": "not_affected", "bravo": "affected", "charlie": "affected", "delta": "not_affected"},
	}
	t.Run("ST-DIS-UC02-R04-T01 The sweep emits one OpenVEX statement for each recorded service and advisory, with the status the records give", func(t *testing.T) {
		statements := vex(t, "alpha", "bravo", "charlie", "delta")
		if len(statements) != 8 {
			t.Fatalf("the document holds %d statements, not eight", len(statements))
		}
		for advisory, services := range recorded {
			for name, status := range services {
				statement := statements[[2]string{f.advisories[advisory], "pkg:generic/" + name}]
				if statement == nil || statement["status"] != status {
					t.Errorf("%s for %s: got %v, want %s", advisory, name, statement, status)
				}
			}
		}
	})
	t.Run("ST-DIS-UC02-R04-T02 The sweep emits a statement with the status under_investigation for each advisory and each checkout with no readable record", func(t *testing.T) {
		statements := vex(t, "alpha", "bravo", "charlie", "delta", "echo", "foxtrot")
		if len(statements) != 12 {
			t.Fatalf("the document holds %d statements, not twelve", len(statements))
		}
		for advisory, services := range recorded {
			for name, status := range services {
				if statement := statements[[2]string{f.advisories[advisory], "pkg:generic/" + name}]; statement == nil || statement["status"] != status {
					t.Errorf("%s for %s: got %v, want %s", advisory, name, statement, status)
				}
			}
			for _, name := range []string{"echo", "foxtrot"} {
				statement := statements[[2]string{f.advisories[advisory], "file://" + f.checkouts[name]}]
				if statement == nil || statement["status"] != "under_investigation" {
					t.Errorf("%s for %s: got %v, want under_investigation", advisory, name, statement)
				}
			}
		}
	})
	t.Run("ST-DIS-UC02-FL-T The platform engineer names every service built from a digest and gets the fleet's VEX statements", func(t *testing.T) {
		before := tree(t, f.parent)
		answer, events := offline(t, f.parent, f.env, append([]string{"fleet", "--digest", fleetD1}, f.order...)...)
		checkStartsOnlyStegoAndGit(t, stegoBinary(t), events)
		lines := fleetLines(t, answer)
		for _, name := range []string{"alpha", "bravo"} {
			want := "pins " + name + " " + f.checkouts[name] + " component component-a version 1.2.0 digest " + fleetD1
			if got := lines[f.checkouts[name]]; len(got) != 1 || got[0] != want {
				t.Errorf("%s: got %v, want %q", name, got, want)
			}
		}
		for _, name := range []string{"echo", "foxtrot"} {
			if got := lines[f.checkouts[name]]; len(got) != 1 || !strings.HasPrefix(got[0], "unknown ") {
				t.Errorf("%s is not named as unknown: %v", name, got)
			}
		}
		if after := tree(t, f.parent); !mapsEqual(before, after) {
			t.Fatal("stego fleet wrote a file")
		}
		statements := vex(t, "alpha", "bravo", "charlie", "delta")
		if len(statements) != 8 {
			t.Fatalf("the document holds %d statements, not eight", len(statements))
		}
		for advisory, services := range recorded {
			for name, status := range services {
				if statement := statements[[2]string{f.advisories[advisory], "pkg:generic/" + name}]; statement == nil || statement["status"] != status {
					t.Errorf("%s for %s: got %v, want %s", advisory, name, statement, status)
				}
			}
		}
	})
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}
