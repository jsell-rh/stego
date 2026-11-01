package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// TestMain runs the test binary as the assure command when ASSURE_TEST_MAIN is 1, so the tests run assure
// record end to end as a process, with its own environment, working folder and exit code.
func TestMain(m *testing.M) {
	if os.Getenv("ASSURE_TEST_MAIN") == "1" {
		os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv, fixtureProvider))
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
	for name, content := range fixtureData {
		writeFile(t, filepath.Join(f.repository, name), content)
	}
	writeFile(t, f.goJSON, goJSON)
	writeFile(t, f.pythonJSON, pythonJSON)
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

// refusedWithNothingWritten checks that assure record exited non-zero and wrote no predicate.
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

	t.Run("ST-ASR-UC01-R04-T01 The subject is the full commit checked out, and a short ID, a branch or another commit is refused", func(t *testing.T) {
		f := newFixture(t, goFixture, pythonFixture)
		first := f.commit
		writeFile(t, filepath.Join(f.repository, "data", "domain.jsonl"), `{"id":"ST-FIX","title":"Fixture, second"}`+"\n")
		f.git(t, "-c", "user.name=fixture", "-c", "user.email=fixture@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-a", "-m", "second")
		head := strings.TrimSpace(f.git(t, "rev-parse", "HEAD"))
		short := strings.TrimSpace(f.git(t, "rev-parse", "--short", "HEAD"))
		branch := strings.TrimSpace(f.git(t, "branch", "--show-current"))
		if branch == "" {
			t.Fatal("the fixture repository is on no branch")
		}
		for name, commit := range map[string]string{"a short ID": short, "the branch " + branch: branch, "the commit before the one checked out": first} {
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
		name, digest := workflowSubject(t, head)
		if name != "git+https://github.com/jsell-rh/stego@"+head || digest != "sha256:"+hex.EncodeToString(sum[:]) {
			t.Fatalf("the attest job names the subject %s (%s), not the commit %s", name, digest, head)
		}
	})

	t.Run("ST-ASR-UC01-R04-T02 A tree with uncommitted changes is refused, also when git status hides them", func(t *testing.T) {
		userExcludes := filepath.Join(t.TempDir(), "excludes")
		userConfig := filepath.Join(t.TempDir(), "gitconfig")
		writeFile(t, userExcludes, "*.local\n")
		writeFile(t, userConfig, "[core]\n\texcludesFile = "+userExcludes+"\n")
		type change struct {
			name string
			make func(f fixture)
			env  []string
		}
		for _, c := range []change{
			{name: "a changed file", make: func(f fixture) {
				writeFile(t, filepath.Join(f.repository, "data", "domain.jsonl"), `{"id":"ST-FIX","title":"Changed"}`+"\n")
			}},
			{name: "a staged change", make: func(f fixture) {
				writeFile(t, filepath.Join(f.repository, "data", "domain.jsonl"), `{"id":"ST-FIX","title":"Staged"}`+"\n")
				f.git(t, "add", "data/domain.jsonl")
			}},
			{name: "an untracked file", make: func(f fixture) {
				writeFile(t, filepath.Join(f.repository, "internal", "fix", "new.go"), "package fix\n")
			}},
			{name: "a change hidden by skip-worktree", make: func(f fixture) {
				f.git(t, "update-index", "--skip-worktree", "data/domain.jsonl")
				writeFile(t, filepath.Join(f.repository, "data", "domain.jsonl"), `{"id":"ST-FIX","title":"Hidden"}`+"\n")
			}},
			{name: "an automated test record hidden by assume-unchanged", make: func(f fixture) {
				f.git(t, "update-index", "--assume-unchanged", "data/invariant_test.jsonl")
				writeFile(t, filepath.Join(f.repository, "data", "invariant_test.jsonl"), fixtureData["data/invariant_test.jsonl"]+
					`{"id":"ST-FIX-INV04-T","title":"Hidden","invariant_id":"ST-FIX-INV04","level":"unit","state":"automated","file":"internal/fix/fix_test.go"}`+"\n")
			}},
			{name: "an untracked file hidden by info/exclude", make: func(f fixture) {
				writeFile(t, filepath.Join(f.repository, ".git", "info", "exclude"), "*.local\n")
				writeFile(t, filepath.Join(f.repository, "internal", "fix", "extra.local"), "x\n")
			}},
			{name: "an untracked file hidden by the user's excludes file", make: func(f fixture) {
				writeFile(t, filepath.Join(f.repository, "internal", "fix", "extra.local"), "x\n")
			}, env: []string{"GIT_CONFIG_GLOBAL=" + userConfig}},
			{name: "another commit's tree shown by a replace ref", make: func(f fixture) {
				head := strings.TrimSpace(f.git(t, "rev-parse", "HEAD"))
				branch := strings.TrimSpace(f.git(t, "branch", "--show-current"))
				f.git(t, "checkout", "-q", "-b", "other")
				writeFile(t, filepath.Join(f.repository, "data", "invariant_test.jsonl"), fixtureData["data/invariant_test.jsonl"]+
					`{"id":"ST-FIX-INV04-T","title":"Replaced","invariant_id":"ST-FIX-INV04","level":"unit","state":"automated","file":"internal/fix/fix_test.go"}`+"\n")
				f.git(t, "-c", "user.name=fixture", "-c", "user.email=fixture@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-a", "-m", "other")
				other := strings.TrimSpace(f.git(t, "rev-parse", "HEAD"))
				f.git(t, "checkout", "-q", branch)
				f.git(t, "replace", head, other)
				f.git(t, "reset", "-q", "--hard")
			}},
			{name: "a same-size change hidden by minimal stat checks", make: func(f fixture) {
				f.git(t, "config", "core.checkStat", "minimal")
				f.git(t, "config", "core.trustctime", "false")
				file := filepath.Join(f.repository, "data", "domain.jsonl")
				old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
				if err := os.Chtimes(file, old, old); err != nil {
					t.Fatal(err)
				}
				f.git(t, "update-index", "--refresh")
				writeFile(t, file, strings.Replace(fixtureData["data/domain.jsonl"], "Fixture", "Fixturf", 1))
				if err := os.Chtimes(file, old, old); err != nil {
					t.Fatal(err)
				}
			}},
			{name: "a new file hidden by an untracked .gitignore that ignores itself", make: func(f fixture) {
				writeFile(t, filepath.Join(f.repository, "internal", "fix", ".gitignore"), "*\n")
				writeFile(t, filepath.Join(f.repository, "internal", "fix", "new.go"), "package fix\n")
			}},
		} {
			f := newFixture(t, goFixture, pythonFixture)
			c.make(f)
			if strings.Contains(c.name, "hidden") || strings.Contains(c.name, "replace") {
				if status := f.git(t, "-c", "core.excludesFile="+userExcludes, "status", "--porcelain"); status != "" {
					t.Fatalf("%s: git status shows %q, so the case hides nothing", c.name, status)
				}
			}
			result := f.record(t, f.commit, f.predicate, c.env...)
			if result.code == 0 {
				t.Fatalf("%s: assure record exited 0", c.name)
			}
			f.refusedWithNothingWritten(t, result, f.predicate, "uncommitted changes")
		}
	})

	t.Run("ST-ASR-UC01-R05-T01 Only the trusted workflow's attestation job signs the proof run, it runs no repository code, and CI commits no evidence file", func(t *testing.T) {
		workflow := readWorkflow(t)
		if len(workflow.Permissions) != 1 || workflow.Permissions["contents"] != "read" {
			t.Fatalf("the workflow's permissions are %v, want only contents: read", workflow.Permissions)
		}
		suites, attest := workflow.Jobs["suites"], workflow.Jobs["attest"]
		if len(suites.Steps) == 0 || len(attest.Steps) == 0 {
			t.Fatal("the workflow does not have both a suites job and an attest job")
		}
		if len(suites.Permissions) != 1 || suites.Permissions["contents"] != "read" {
			t.Fatalf("the suites job's permissions are %v, want only contents: read: it runs the repository's code", suites.Permissions)
		}
		// The workflow and the attest job are exactly what is allowed: each key of the workflow and of the job, and
		// each step with each of its keys and values, in order. The attest job needs the suites job, may only get an
		// identity token and write attestations, checks out and runs no code of the repository, and signs
		// proof-run.json with actions/attest at the proof run predicate type and the subject its subject step names.
		raw := readRawWorkflow(t)
		exactWorkflow(t, raw)
		exactJob(t, raw, "attest", attestJob())
		var record *workflowStep
		for _, job := range []workflowJob{suites, attest} {
			for i, step := range job.Steps {
				if strings.Contains(step.Run, "git commit") || strings.Contains(step.Run, "git push") {
					t.Fatalf("step %q commits to the repository", step.Name)
				}
				if action, pin, ok := strings.Cut(step.Uses, "@"); step.Uses != "" && (!ok || len(pin) != 40 || action == "") {
					t.Fatalf("step %q uses %s, which is not pinned to a commit", step.Name, step.Uses)
				}
				if strings.Contains(step.Run, "./cmd/assure record") {
					record = &job.Steps[i]
				}
			}
		}
		if record == nil || !strings.Contains(record.Run, `--predicate "$RUNNER_TEMP/`) || !strings.Contains(record.Run, `--commit "$COMMIT"`) {
			t.Fatal("the suites job does not record the proof run at the commit, under the runner's temporary folder")
		}
		if workflow.Env["COMMIT"] != "${{ github.event.pull_request.head.sha || github.sha }}" {
			t.Fatalf("the commit is %q, want the pull request's head or the commit pushed", workflow.Env["COMMIT"])
		}
		if suites.Steps[0].With["ref"] != "${{ env.COMMIT }}" {
			t.Fatalf("the suites job checks out %q, not the commit", suites.Steps[0].With["ref"])
		}

		f := newFixture(t, goFixture, pythonFixture)
		other := f.record(t, f.commit, f.predicate, "GITHUB_WORKFLOW_REF=jsell-rh/stego/.github/workflows/checks.yml@refs/heads/main")
		f.refusedWithNothingWritten(t, other, f.predicate, "trusted signer")
		inside := filepath.Join(f.repository, "proof-run.json")
		f.refusedWithNothingWritten(t, f.record(t, f.commit, inside), inside, "inside the repository")
		if status := f.git(t, "status", "--porcelain", "--untracked-files=all"); status != "" {
			t.Fatalf("assure record changed the repository: %s", status)
		}
		if _, _, err := runSubjectStep(t, "main"); err == nil {
			t.Fatal("the attest job names a branch as the subject")
		}

		commit := strings.Repeat("ab", 20)
		if err := runPredicateCheck(t, commit, commit); err != nil {
			t.Fatalf("the attest job refuses a predicate about the commit: %v", err)
		}
		if err := runPredicateCheck(t, commit, strings.Repeat("cd", 20)); err == nil {
			t.Fatal("the attest job accepts a predicate about another commit")
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

type workflowJob struct {
	Needs       stringList        `yaml:"needs"`
	Permissions map[string]string `yaml:"permissions"`
	Steps       []workflowStep    `yaml:"steps"`
}

type workflowFile struct {
	Env         map[string]string      `yaml:"env"`
	Permissions map[string]string      `yaml:"permissions"`
	Jobs        map[string]workflowJob `yaml:"jobs"`
}

// stringList is a YAML value that is one string or a list of strings, as a job's needs is.
type stringList []string

func (l *stringList) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		*l = []string{node.Value}
		return nil
	}
	var list []string
	if err := node.Decode(&list); err != nil {
		return err
	}
	*l = list
	return nil
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

// runSubjectStep runs the attest job's step that names the subject, with bash, as GitHub Actions would for the
// commit, and returns the subject name and digest it gives.
func runSubjectStep(t *testing.T, commit string) (string, string, error) {
	t.Helper()
	var script string
	for _, step := range readWorkflow(t).Jobs["attest"].Steps {
		if step.ID == "subject" {
			script = step.Run
		}
	}
	if script == "" {
		t.Fatal("the attest job has no step with the ID subject")
	}
	outputs := filepath.Join(t.TempDir(), "github-output")
	writeFile(t, outputs, "")
	command := exec.Command("bash", "-e", "-c", script)
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"COMMIT=" + commit,
		"GITHUB_SERVER_URL=https://github.com",
		"GITHUB_REPOSITORY=jsell-rh/stego",
		"GITHUB_OUTPUT=" + outputs,
	}
	if out, err := command.CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("%v: %s", err, out)
	}
	data, err := os.ReadFile(outputs)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		key, value, _ := strings.Cut(line, "=")
		values[key] = value
	}
	return values["name"], values["digest"], nil
}

// workflowSubject is the subject name and digest the attest job gives for the commit.
func workflowSubject(t *testing.T, commit string) (string, string) {
	t.Helper()
	name, digest, err := runSubjectStep(t, commit)
	if err != nil {
		t.Fatalf("the attest job's subject step fails for %s: %v", commit, err)
	}
	return name, digest
}

// runPredicateCheck runs, with bash, the attest job's step that checks that the downloaded predicate is about the
// commit, on a predicate whose commit is about.
func runPredicateCheck(t *testing.T, commit, about string) error {
	t.Helper()
	var script string
	for _, step := range readWorkflow(t).Jobs["attest"].Steps {
		if step.Name == "Check that the predicate is about the commit" {
			script = step.Run
		}
	}
	if script == "" {
		t.Fatal("the attest job has no step that checks the predicate's commit")
	}
	runnerTemp := t.TempDir()
	writeFile(t, filepath.Join(runnerTemp, "proof-run", "proof-run.json"), `{"commit": "`+about+`", "outcomes": []}`)
	command := exec.Command("bash", "-e", "-c", script)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "COMMIT=" + commit, "RUNNER_TEMP=" + runnerTemp}
	if out, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}
