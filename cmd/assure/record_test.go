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
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestMain runs the test binary as the assure command when ASSURE_TEST_MAIN is 1, so the tests run assure
// record end to end as a process, with its own environment, working folder and exit code.
func TestMain(m *testing.M) {
	if os.Getenv("ASSURE_TEST_MAIN") == "1" {
		os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv))
	}
	os.Exit(m.Run())
}

const trustedWorkflowRef = "jsell-rh/stego/.github/workflows/proof-run.yml@refs/heads/main"

// fixtureData is the spec of the fixture repository: test records of two kinds, three of them automated, and a
// table that holds no tests.
var fixtureData = map[string]string{
	"lathe.config.json": `{"schema": 2, "data": "data", "tests": ["**/*_test.go", "scripts/test_*.py"]}` + "\n",
	"data/domain.jsonl": `{"id":"ST-FIX","title":"Fixture"}` + "\n",
	"data/invariant_test.jsonl": `{"id":"ST-FIX-INV01-T","title":"One","invariant_id":"ST-FIX-INV01","level":"unit","state":"automated","file":"internal/fix/fix_test.go"}
{"id":"ST-FIX-INV02-T","title":"Two","invariant_id":"ST-FIX-INV02","level":"unit","state":"automated","file":"internal/fix/fix_test.go"}
{"id":"ST-FIX-INV03-T","title":"Three","invariant_id":"ST-FIX-INV03","level":"unit","state":"not automated yet","file":"internal/fix/fix_test.go"}
`,
	"data/use_case_test.jsonl": `{"id":"ST-FIX-UC01-R01-T01","title":"Four","uc_requirement_id":"ST-FIX-UC01-R01","level":"system","state":"automated","file":"scripts/test_fix.py"}
`,
}

// goFixture is go test -json output of the fixture's Go suite: ST-FIX-INV01-T passes in a subtest, a test with
// no ID fails, and ST-FIX-INV02-T does not run.
const goFixture = `{"Time":"2026-10-29T14:02:11.5Z","Action":"start","Package":"example.com/fix"}
{"Time":"2026-10-29T14:02:12Z","Action":"run","Package":"example.com/fix","Test":"TestFix"}
{"Time":"2026-10-29T14:02:12Z","Action":"run","Package":"example.com/fix","Test":"TestFix/ST-FIX-INV01-T_one_holds"}
{"Time":"2026-10-29T14:02:12Z","Action":"pass","Package":"example.com/fix","Test":"TestFix/ST-FIX-INV01-T_one_holds","Elapsed":0.42}
{"Time":"2026-10-29T14:02:13Z","Action":"run","Package":"example.com/fix","Test":"TestFix/a_case_with_no_ID"}
{"Time":"2026-10-29T14:02:13Z","Action":"output","Package":"example.com/fix","Test":"TestFix/a_case_with_no_ID","Output":"    fix_test.go:40: broken\n"}
{"Time":"2026-10-29T14:02:13Z","Action":"fail","Package":"example.com/fix","Test":"TestFix/a_case_with_no_ID","Elapsed":0}
{"Time":"2026-10-29T14:02:14Z","Action":"fail","Package":"example.com/fix","Test":"TestFix","Elapsed":1.5}
{"Time":"2026-10-29T14:02:14Z","Action":"fail","Package":"example.com/fix","Elapsed":2.5}
`

// pythonFixture is the document of the fixture's Python suite: ST-FIX-UC01-R01-T01 passes in a subtest.
const pythonFixture = `{"format": 1, "started": "2026-10-29T14:03:00Z", "finished": "2026-10-29T14:03:05.25Z", "tests": [
 {"test": "test_fix.Fix.test_cases [ST-FIX-UC01-R01-T01 four holds]", "name": "[ST-FIX-UC01-R01-T01 four holds]", "result": "passed", "elapsed": null, "detail": ""},
 {"test": "test_fix.Fix.test_cases", "name": "test_cases", "result": "passed", "elapsed": 0.3, "detail": ""}
]}`

// fixture is a git repository with the fixture spec committed, and a runner folder outside it with the suites'
// output and the job's step output file.
type fixture struct {
	repository string
	commit     string
	runnerTemp string
	goJSON     string
	pythonJSON string
	predicate  string
	outputs    string
}

func newFixture(t *testing.T, goJSON, pythonJSON string) fixture {
	t.Helper()
	base := t.TempDir()
	f := fixture{
		repository: filepath.Join(base, "repository"),
		runnerTemp: filepath.Join(base, "runner-temp"),
	}
	f.goJSON = filepath.Join(f.runnerTemp, "go-test.json")
	f.pythonJSON = filepath.Join(f.runnerTemp, "python-tests.json")
	f.predicate = filepath.Join(f.runnerTemp, "proof-run.json")
	f.outputs = filepath.Join(f.runnerTemp, "github-output")
	for name, content := range fixtureData {
		writeFile(t, filepath.Join(f.repository, name), content)
	}
	writeFile(t, f.goJSON, goJSON)
	writeFile(t, f.pythonJSON, pythonJSON)
	writeFile(t, f.outputs, "")
	f.git(t, "init", "-q")
	f.git(t, "add", "-A")
	f.git(t, "-c", "user.name=fixture", "-c", "user.email=fixture@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "fixture")
	f.commit = strings.TrimSpace(f.git(t, "rev-parse", "HEAD"))
	return f
}

func writeFile(t *testing.T, file, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f fixture) git(t *testing.T, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", f.repository}, args...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

// assureResult is what one run of the assure command gave.
type assureResult struct {
	code   int
	stdout string
	stderr string
}

// record runs assure record in the fixture repository as the proof run workflow's job would, with the
// environment GitHub Actions sets, and with any change to that environment given as NAME=value.
func (f fixture) record(t *testing.T, commit, predicate string, env ...string) assureResult {
	t.Helper()
	command := exec.Command(os.Args[0], "record", "--commit", commit, "--go-json", f.goJSON, "--python-json", f.pythonJSON, "--predicate", predicate)
	command.Dir = f.repository
	command.Env = append([]string{
		"ASSURE_TEST_MAIN=1",
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + f.runnerTemp,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GITHUB_ACTIONS=true",
		"GITHUB_RUN_ID=35950507614",
		"GITHUB_RUN_ATTEMPT=1",
		"GITHUB_SERVER_URL=https://github.com",
		"GITHUB_REPOSITORY=jsell-rh/stego",
		"GITHUB_WORKFLOW_REF=" + trustedWorkflowRef,
		"GITHUB_OUTPUT=" + f.outputs,
	}, env...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("assure record did not run: %v", err)
	}
	return assureResult{code: command.ProcessState.ExitCode(), stdout: stdout.String(), stderr: stderr.String()}
}

// predicateDocument is the proof run predicate as assure record writes it.
type predicateDocument struct {
	Commit       string `json:"commit"`
	Provider     string `json:"provider"`
	RunReference string `json:"runReference"`
	StartedAt    string `json:"startedAt"`
	FinishedAt   string `json:"finishedAt"`
	SpecDigest   string `json:"specDigest"`
	Outcomes     []struct {
		TestRecord string   `json:"testRecord"`
		Result     string   `json:"result"`
		File       string   `json:"file"`
		Duration   *float64 `json:"duration"`
		Detail     string   `json:"detail"`
	} `json:"outcomes"`
}

func readPredicate(t *testing.T, file string) predicateDocument {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("no predicate: %v", err)
	}
	var document predicateDocument
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("the predicate is not a proof run: %v\n%s", err, data)
	}
	return document
}

// results lists each outcome as "ID result", in the predicate's order.
func (d predicateDocument) results() []string {
	var list []string
	for _, outcome := range d.Outcomes {
		list = append(list, outcome.TestRecord+" "+outcome.Result)
	}
	return list
}

// refusedWithNothingWritten checks that assure record exited non-zero and wrote no predicate and no step output.
func (f fixture) refusedWithNothingWritten(t *testing.T, result assureResult, predicate, says string) {
	t.Helper()
	if result.code == 0 {
		t.Fatalf("assure record exited 0:\n%s", result.stdout)
	}
	if !strings.Contains(result.stderr, says) {
		t.Fatalf("assure record does not say %q:\n%s", says, result.stderr)
	}
	if _, err := os.Stat(predicate); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused run wrote the predicate %s (%v)", predicate, err)
	}
	if data, _ := os.ReadFile(f.outputs); len(data) != 0 {
		t.Fatalf("a refused run wrote step outputs:\n%s", data)
	}
}

func TestRecord(t *testing.T) {
	t.Run("ST-ASR-UC01-R01-T01 Every automated test record gets an outcome, and one that did not run is failed with did not run", func(t *testing.T) {
		f := newFixture(t, goFixture, pythonFixture)
		result := f.record(t, f.commit, f.predicate)
		if result.code != 0 {
			t.Fatalf("assure record exited %d:\n%s", result.code, result.stderr)
		}
		document := readPredicate(t, f.predicate)
		want := []string{"ST-FIX-INV01-T passed", "ST-FIX-INV02-T failed", "ST-FIX-UC01-R01-T01 passed"}
		if got := document.results(); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("outcomes:\n%s\nwant one per automated test record:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		if didNotRun := document.Outcomes[1]; didNotRun.Detail != "did not run" || didNotRun.Duration != nil || didNotRun.File != "internal/fix/fix_test.go" {
			t.Fatalf("the test that did not run is %+v, want failed with the detail did not run", didNotRun)
		}
	})

	t.Run("ST-ASR-UC01-R02-T01 Outcomes come from Go and Python subtests by the ID in their names, and a test with no ID is ignored", func(t *testing.T) {
		python, err := exec.LookPath("python3")
		if err != nil {
			t.Fatal("python3 is needed to run the Python runner")
		}
		suite := t.TempDir()
		writeFile(t, filepath.Join(suite, "test_fix.py"), `import unittest


class Fix(unittest.TestCase):
    def test_cases(self):
        with self.subTest("ST-FIX-UC01-R01-T01 four holds"):
            self.assertEqual(4, 2 + 2)
        with self.subTest("a case with no ID"):
            self.assertEqual(5, 2 + 2)
`)
		goJSON := strings.Replace(goFixture, `"Test":"TestFix/a_case_with_no_ID"}`, `"Test":"TestFix/a_case_with_no_ID"}`+"\n"+
			`{"Time":"2026-10-29T14:02:13Z","Action":"run","Package":"example.com/fix","Test":"TestFix/ST-FIX-INV02-T_two_fails"}`+"\n"+
			`{"Time":"2026-10-29T14:02:13Z","Action":"fail","Package":"example.com/fix","Test":"TestFix/ST-FIX-INV02-T_two_fails","Elapsed":0.1}`, 1)
		f := newFixture(t, goJSON, "")
		runner, err := filepath.Abs(filepath.Join("..", "..", "scripts", "run-python-tests.py"))
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command(python, "-B", runner, "--start", suite, "--output", f.pythonJSON)
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("the Python runner failed: %v\n%s", err, out)
		}
		result := f.record(t, f.commit, f.predicate)
		if result.code != 0 {
			t.Fatalf("assure record exited %d:\n%s", result.code, result.stderr)
		}
		document := readPredicate(t, f.predicate)
		want := []string{"ST-FIX-INV01-T passed", "ST-FIX-INV02-T failed", "ST-FIX-UC01-R01-T01 passed"}
		if got := document.results(); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("outcomes:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		if document.Outcomes[1].Detail == "did not run" {
			t.Fatal("the Go subtest ST-FIX-INV02-T ran and failed, but it is recorded as not run")
		}
		if document.Outcomes[2].File != "scripts/test_fix.py" {
			t.Fatalf("the Python outcome's file is %q", document.Outcomes[2].File)
		}
	})

	t.Run("ST-ASR-UC01-R03-T01 An outcome for an ID that is not a test record refuses the run, and no predicate is written", func(t *testing.T) {
		goJSON := goFixture + `{"Time":"2026-10-29T14:02:15Z","Action":"run","Package":"example.com/fix","Test":"TestOther/ST-FIX-INV09-T_names_no_record"}
{"Time":"2026-10-29T14:02:15Z","Action":"pass","Package":"example.com/fix","Test":"TestOther/ST-FIX-INV09-T_names_no_record","Elapsed":0}
`
		f := newFixture(t, goJSON, pythonFixture)
		result := f.record(t, f.commit, f.predicate)
		f.refusedWithNothingWritten(t, result, f.predicate, "ST-FIX-INV09-T")
	})

	t.Run("ST-ASR-UC01-R04-T01 The subject is the full commit checked out, and a short or other commit is refused", func(t *testing.T) {
		f := newFixture(t, goFixture, pythonFixture)
		first := f.commit
		writeFile(t, filepath.Join(f.repository, "data", "domain.jsonl"), `{"id":"ST-FIX","title":"Fixture, second"}`+"\n")
		f.git(t, "-c", "user.name=fixture", "-c", "user.email=fixture@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-a", "-m", "second")
		head := strings.TrimSpace(f.git(t, "rev-parse", "HEAD"))
		short := strings.TrimSpace(f.git(t, "rev-parse", "--short", "HEAD"))
		for name, commit := range map[string]string{"a short ID": short, "a branch": "HEAD", "the commit before the one checked out": first} {
			result := f.record(t, commit, f.predicate)
			if result.code == 0 {
				t.Fatalf("%s (%s): assure record exited 0", name, commit)
			}
			f.refusedWithNothingWritten(t, result, f.predicate, "commit")
		}
		result := f.record(t, head, f.predicate)
		if result.code != 0 {
			t.Fatalf("assure record exited %d:\n%s", result.code, result.stderr)
		}
		if got := readPredicate(t, f.predicate).Commit; got != head {
			t.Fatalf("the predicate is about %q, want the commit checked out %s", got, head)
		}
		sum := sha256.Sum256([]byte(head))
		outputs, _ := os.ReadFile(f.outputs)
		for _, line := range []string{"subject-name=git+https://github.com/jsell-rh/stego@" + head, "subject-digest=sha256:" + hex.EncodeToString(sum[:])} {
			if !strings.Contains(string(outputs), line+"\n") {
				t.Fatalf("the attestation's subject is not the commit: no %q in\n%s", line, outputs)
			}
		}
	})

	t.Run("ST-ASR-UC01-R04-T02 A tree with uncommitted changes is refused", func(t *testing.T) {
		for name, change := range map[string]func(f fixture){
			"a changed file": func(f fixture) {
				writeFile(t, filepath.Join(f.repository, "data", "domain.jsonl"), `{"id":"ST-FIX","title":"Changed"}`+"\n")
			},
			"an added file": func(f fixture) { f.git(t, "rm", "-q", "--cached", "data/domain.jsonl") },
			"an untracked file": func(f fixture) {
				writeFile(t, filepath.Join(f.repository, "internal", "fix", "new.go"), "package fix\n")
			},
		} {
			f := newFixture(t, goFixture, pythonFixture)
			change(f)
			result := f.record(t, f.commit, f.predicate)
			if result.code == 0 {
				t.Fatalf("%s: assure record exited 0", name)
			}
			f.refusedWithNothingWritten(t, result, f.predicate, "uncommitted changes")
		}
	})

	t.Run("ST-ASR-UC01-R05-T01 Only the trusted workflow's attestation action signs the proof run, and CI commits no evidence file", func(t *testing.T) {
		workflow := readWorkflow(t)
		if workflow.Permissions["contents"] != "read" {
			t.Fatalf("the workflow's contents permission is %q, want read", workflow.Permissions["contents"])
		}
		job, ok := workflow.Jobs["proof-run"]
		if !ok {
			t.Fatal("the workflow has no proof-run job")
		}
		for permission, access := range job.Permissions {
			if access == "write" && permission != "id-token" && permission != "attestations" {
				t.Fatalf("the job may write %s; it may write only its identity token and attestations", permission)
			}
		}
		if job.Permissions["contents"] != "read" || job.Permissions["id-token"] != "write" || job.Permissions["attestations"] != "write" {
			t.Fatalf("the job's permissions are %v", job.Permissions)
		}
		var record, attest *workflowStep
		for i, step := range job.Steps {
			if strings.Contains(step.Run, "git commit") || strings.Contains(step.Run, "git push") {
				t.Fatalf("step %q commits to the repository", step.Name)
			}
			if strings.Contains(step.Run, "./cmd/assure record") {
				record = &job.Steps[i]
			}
			if strings.HasPrefix(step.Uses, "actions/attest@") {
				attest = &job.Steps[i]
			}
		}
		if record == nil || attest == nil {
			t.Fatal("the job does not both record the proof run and attest it")
		}
		if !strings.Contains(record.Run, `--predicate "$RUNNER_TEMP/`) {
			t.Fatalf("the predicate is not written under the runner's temporary folder: %s", record.Run)
		}
		if pin := strings.TrimPrefix(attest.Uses, "actions/attest@"); len(pin) != 40 {
			t.Fatalf("the attestation action is not pinned to a commit: %s", attest.Uses)
		}
		for _, input := range []string{"subject-name", "subject-digest", "predicate-type", "predicate-path"} {
			if want := "${{ steps." + record.ID + ".outputs." + input + " }}"; attest.With[input] != want {
				t.Fatalf("the attestation action's %s is %q, want %q", input, attest.With[input], want)
			}
		}

		f := newFixture(t, goFixture, pythonFixture)
		other := f.record(t, f.commit, f.predicate, "GITHUB_WORKFLOW_REF=jsell-rh/stego/.github/workflows/checks.yml@refs/heads/main")
		f.refusedWithNothingWritten(t, other, f.predicate, "trusted signer")
		inside := filepath.Join(f.repository, "proof-run.json")
		f.refusedWithNothingWritten(t, f.record(t, f.commit, inside), inside, "inside the repository")
		if status := f.git(t, "status", "--porcelain", "--untracked-files=all"); status != "" {
			t.Fatalf("assure record changed the repository: %s", status)
		}
	})
}

type workflowStep struct {
	ID   string            `yaml:"id"`
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	With map[string]string `yaml:"with"`
}

type workflowFile struct {
	Permissions map[string]string `yaml:"permissions"`
	Jobs        map[string]struct {
		Permissions map[string]string `yaml:"permissions"`
		Steps       []workflowStep    `yaml:"steps"`
	} `yaml:"jobs"`
}

// readWorkflow reads the proof run workflow from the repository this test runs in.
func readWorkflow(t *testing.T) workflowFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "proof-run.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow workflowFile
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	return workflow
}
