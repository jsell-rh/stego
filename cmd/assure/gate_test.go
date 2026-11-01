package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/assure"
	"gopkg.in/yaml.v3"
)

// fixtureProvider stands for GitHub's attestation store and Sigstore's signature check at the adapter's edge, in
// the assure process the tests start. ASSURE_TEST_ATTESTATIONS names a JSON file of signed statements, which
// stand for the bundles the store keeps; the store gives those whose subject digest is SubjectDigest of the
// commit asked for, as GitHub looks them up. A statement whose signer subject is "forged" fails the signature
// check. ASSURE_TEST_FETCHES names a file that gets a line for each commit the store is asked for.
func fixtureProvider(getenv func(string) string) (assure.AttestationStore, assure.BundleVerifier, func(), error) {
	return fixtureStore{statements: getenv("ASSURE_TEST_ATTESTATIONS"), fetches: getenv("ASSURE_TEST_FETCHES")}, fixtureVerifier{}, func() {}, nil
}

type fixtureStore struct {
	statements string
	fetches    string
}

func (s fixtureStore) HandOver(assure.ProofRun) (string, error) {
	return "", errors.New("the fixture store takes no proof run")
}

func (s fixtureStore) Fetch(commit string) ([][]byte, error) {
	if s.fetches != "" {
		log, err := os.OpenFile(s.fetches, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, err
		}
		fmt.Fprintln(log, commit)
		if err := log.Close(); err != nil {
			return nil, err
		}
	}
	if s.statements == "" {
		return nil, nil
	}
	data, err := os.ReadFile(s.statements)
	if err != nil {
		return nil, err
	}
	var statements []json.RawMessage
	if err := json.Unmarshal(data, &statements); err != nil {
		return nil, err
	}
	var bundles [][]byte
	for _, raw := range statements {
		var statement assure.SignedStatement
		if err := json.Unmarshal(raw, &statement); err != nil {
			return nil, err
		}
		if len(statement.Subjects) > 0 && statement.Subjects[0].Digest == assure.SubjectDigest(commit) {
			bundles = append(bundles, raw)
		}
	}
	return bundles, nil
}

type fixtureVerifier struct{}

func (fixtureVerifier) VerifyBundle(data []byte, subjectDigest string) (assure.SignedStatement, error) {
	var statement assure.SignedStatement
	if err := json.Unmarshal(data, &statement); err != nil {
		return assure.SignedStatement{}, err
	}
	if statement.Signer.Subject == "forged" {
		return assure.SignedStatement{}, errors.New("the signature does not verify")
	}
	for _, subject := range statement.Subjects {
		if subject.Digest != subjectDigest {
			return assure.SignedStatement{}, errors.New("the statement does not name the subject digest")
		}
	}
	return statement, nil
}

const (
	proofRunWorkflow = "https://github.com/jsell-rh/stego/.github/workflows/proof-run.yml"
	pullRef          = "refs/pull/7/merge"
	mainRef          = "refs/heads/main"
)

// gateSpec is the spec of the gate's fixture repository: the domain FX-LEND with three invariants, of which
// FX-LEND-INV01 and FX-LEND-INV02 are built and proved by automated tests and FX-LEND-INV03 is only designed, and
// a constraint of the product that is designed only. It has two workflow files.
var gateSpec = map[string]string{
	"lathe.config.json":    `{"schema": 2, "data": "data", "tests": ["**/*_test.go"]}` + "\n",
	"data/product.jsonl":   `{"id":"FX","title":"Fixture","description":"What Fixture is, who it is for, and what it must do."}` + "\n",
	"data/domain.jsonl":    `{"id":"FX-LEND","title":"Lending","product_id":"FX","subdomain_type":"core","description":"Lending tools."}` + "\n",
	"data/aggregate.jsonl": `{"id":"FX-LEND-A1","title":"Loan","domain_id":"FX-LEND","description":"A loan.","identity":"Loan ID"}` + "\n",
	"data/invariant.jsonl": `{"id":"FX-LEND-INV01","title":"One loan per tool","aggregate_id":"FX-LEND-A1","statement":"A tool MUST have at most one open loan."}
{"id":"FX-LEND-INV02","title":"Due after start","aggregate_id":"FX-LEND-A1","statement":"A loan MUST end after it starts."}
{"id":"FX-LEND-INV03","title":"Borrower known","aggregate_id":"FX-LEND-A1","statement":"A loan MUST name a known borrower."}
`,
	"data/rule.jsonl": `{"id":"FX-LEND-INV01-RL","title":"lend.oneLoanPerTool","invariant_id":"FX-LEND-INV01","file":"internal/lend/rules.go"}
{"id":"FX-LEND-INV02-RL","title":"lend.dueAfterStart","invariant_id":"FX-LEND-INV02","file":"internal/lend/rules.go"}
`,
	"data/invariant_test.jsonl":     invariantTest01 + invariantTest02,
	"data/entity.jsonl":             `{"id":"FX-LEND-EN01","title":"Loan","aggregate_id":"FX-LEND-A1","role":"root","description":"A loan of a tool."}` + "\n",
	"data/entity_field.jsonl":       `{"entity_id":"FX-LEND-EN01","position":1,"name":"Due","meaning":"When it ends.","type_value_object_id":"FX-VO01","required":true}` + "\n",
	"data/value_object.jsonl":       `{"id":"FX-VO01","title":"Due date","description":"The day a loan ends."}` + "\n",
	"data/value_object_field.jsonl": `{"value_object_id":"FX-VO01","position":1,"name":"Day","meaning":"The day.","type":"date","required":true}` + "\n",
	"data/constraint.jsonl":         `{"id":"FX-VO01-C01","title":"In the future","value_object_id":"FX-VO01","statement":"A due date MUST be after today."}` + "\n",
	"data/value_type.jsonl":         `{"id":"FX-VO01-VT","title":"lend.DueDate","value_object_id":"FX-VO01","file":"internal/lend/due.go"}` + "\n",
	"internal/lend/rules.go":        "package lend\n\nfunc oneLoanPerTool(open int) bool { return open < 2 }\n",
	"internal/lend/rules_test.go": "package lend\n\nimport \"testing\"\n\nfunc TestRules(t *testing.T) {\n" +
		"\tt.Run(\"FX-LEND-INV01-T one loan\", func(t *testing.T) {})\n\tt.Run(\"FX-LEND-INV02-T due after start\", func(t *testing.T) {})\n}\n",
	".github/workflows/proof-run.yml": "name: Proof run\n",
	".github/workflows/checks.yml":    "name: Checks\n",
}

const (
	invariantTest01 = `{"id":"FX-LEND-INV01-T","title":"One loan","invariant_id":"FX-LEND-INV01","level":"unit","state":"automated","file":"internal/lend/rules_test.go","when":"Lends twice.","then":"Refused."}` + "\n"
	invariantTest02 = `{"id":"FX-LEND-INV02-T","title":"Due after start","invariant_id":"FX-LEND-INV02","level":"unit","state":"automated","file":"internal/lend/rules_test.go","when":"Ends before it starts.","then":"Refused."}` + "\n"
	// newInvariant adds FX-LEND-INV04, with no rule and no test.
	newInvariant = `{"id":"FX-LEND-INV04","title":"Tool returned","aggregate_id":"FX-LEND-A1","statement":"A loan MUST end with the tool returned."}` + "\n"
)

// latheCLI is the Lathe CLI the tests run: STEGO_LATHE, or node_modules/.bin/lathe of the repository the tests
// run in. Without it the test is skipped, unless STEGO_REQUIRE_LATHE is 1, as it is in the proof run.
func latheCLI(t *testing.T) string {
	t.Helper()
	lathe := os.Getenv("STEGO_LATHE")
	if lathe == "" {
		lathe = filepath.Join("..", "..", "node_modules", ".bin", "lathe")
	}
	if _, err := os.Stat(lathe); err != nil {
		if os.Getenv("STEGO_REQUIRE_LATHE") == "1" {
			t.Fatalf("the Lathe CLI %s is required: %v", lathe, err)
		}
		t.Skipf("no Lathe CLI at %s", lathe)
	}
	absolute, err := filepath.Abs(lathe)
	if err != nil {
		t.Fatal(err)
	}
	return absolute
}

// gateRepository is a git repository with the gate's fixture spec committed as the base head, and a runner
// folder outside it.
type gateRepository struct {
	root       string
	base       string
	runnerTemp string
	lathe      string
}

func newGateRepository(t *testing.T) gateRepository {
	t.Helper()
	folder := t.TempDir()
	g := gateRepository{root: filepath.Join(folder, "repository"), runnerTemp: filepath.Join(folder, "runner-temp"), lathe: latheCLI(t)}
	for _, dir := range []string{g.root, g.runnerTemp} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	g.git(t, "init", "-q")
	g.base = g.commit(t, "", gateSpec)
	return g
}

func (g gateRepository) git(t *testing.T, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", g.root}, args...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

// commit makes a commit on top of the commit parent (none for the first), with the files written and each file
// whose content is empty removed, and returns it.
func (g gateRepository) commit(t *testing.T, parent string, files map[string]string) string {
	t.Helper()
	if parent != "" {
		g.git(t, "checkout", "-q", "--detach", parent)
	}
	for name, content := range files {
		file := filepath.Join(g.root, filepath.FromSlash(name))
		if content == "" {
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
			continue
		}
		writeFile(t, file, content)
	}
	g.git(t, "add", "-A")
	g.git(t, "-c", "user.name=fixture", "-c", "user.email=fixture@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "change")
	return strings.TrimSpace(g.git(t, "rev-parse", "HEAD"))
}

// proofRun is a signed proof run statement about the commit, signed by the proof run workflow at the reference,
// with its outcomes given as "ID result".
func proofRun(commit, reference string, logIndex int64, outcomes ...string) assure.SignedStatement {
	var entries []string
	for _, outcome := range outcomes {
		id, result, _ := strings.Cut(outcome, " ")
		entries = append(entries, `{"testRecord":"`+id+`","result":"`+result+`","file":"internal/lend/rules_test.go"}`)
	}
	predicate := `{"commit":"` + commit + `","provider":"github-actions","runReference":"github:1/1","startedAt":"2026-10-31T10:00:00Z",` +
		`"finishedAt":"2026-10-31T10:05:00Z","specDigest":"sha256:` + strings.Repeat("a", 64) + `","outcomes":[` + strings.Join(entries, ",") + `]}`
	return assure.SignedStatement{
		Subjects:         []assure.StatementSubject{{Name: "git+https://github.com/jsell-rh/stego@" + commit, Digest: assure.SubjectDigest(commit)}},
		PredicateType:    assure.ProofRunPredicateType,
		Predicate:        json.RawMessage(predicate),
		Signer:           assure.Signer{Issuer: assure.GitHubIssuer, Subject: proofRunWorkflow + "@" + reference},
		SourceRepository: "https://github.com/jsell-rh/stego",
		SourceRef:        reference,
		LogIndex:         logIndex,
	}
}

// keep writes the statements as the attestations the store keeps, and returns the file.
func (g gateRepository) keep(t *testing.T, name string, statements ...assure.SignedStatement) string {
	t.Helper()
	if statements == nil {
		statements = []assure.SignedStatement{}
	}
	data, err := json.Marshal(statements)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(g.runnerTemp, name+".json")
	writeFile(t, file, string(data))
	return file
}

// gate runs assure gate change in the repository as the gate job of a pull request would, with the attestations
// in the file, and with any change to the environment given as NAME=value.
func (g gateRepository) gate(t *testing.T, commit, base, attestations string, env ...string) assureResult {
	t.Helper()
	command := exec.Command(os.Args[0], "gate", "change", "--commit", commit, "--base", base, "--lathe", g.lathe)
	command.Dir = g.root
	command.Env = append([]string{
		"ASSURE_TEST_MAIN=1",
		"ASSURE_TEST_ATTESTATIONS=" + attestations,
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + g.runnerTemp,
		"TMPDIR=" + g.runnerTemp,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GITHUB_ACTIONS=true",
		"GITHUB_SERVER_URL=https://github.com",
		"GITHUB_REPOSITORY=jsell-rh/stego",
		"GITHUB_REF=" + pullRef,
		"RUNNER_TEMP=" + g.runnerTemp,
	}, env...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("assure gate change did not run: %v", err)
	}
	return assureResult{code: command.ProcessState.ExitCode(), stdout: stdout.String(), stderr: stderr.String()}
}

// reasons is the list of reasons the decision prints, one per line, or none when it prints none.
func (r assureResult) reasons(t *testing.T) []string {
	t.Helper()
	_, list, ok := strings.Cut(r.stdout, "\nreasons (")
	if !ok {
		if !strings.HasSuffix(r.stdout, "\nreasons: none\n") {
			t.Fatalf("the decision lists no reasons:\n%s%s", r.stdout, r.stderr)
		}
		return nil
	}
	_, list, _ = strings.Cut(list, "):\n")
	return strings.Split(strings.TrimSuffix(list, "\n"), "\n")
}

// decided checks that the gate exited with the code and printed the reasons.
func decided(t *testing.T, name string, result assureResult, code int, reasons ...string) {
	t.Helper()
	if result.code != code {
		t.Fatalf("%s: assure gate change exited %d, want %d:\n%s%s", name, result.code, code, result.stdout, result.stderr)
	}
	got := result.reasons(t)
	if strings.Join(got, "\n") != strings.Join(reasons, "\n") {
		t.Fatalf("%s: the reasons are\n%s\nwant\n%s\n(decision:\n%s)", name, strings.Join(got, "\n"), strings.Join(reasons, "\n"), result.stdout)
	}
}

func TestGateChange(t *testing.T) {
	t.Run("ST-ASR-UC02-R01-T01 A change under which an obligation falls is refused, and one that only adds unbuilt, unproved obligations passes", func(t *testing.T) {
		g := newGateRepository(t)
		grows := g.commit(t, g.base, map[string]string{"data/invariant.jsonl": gateSpec["data/invariant.jsonl"] + newInvariant})
		passed := g.keep(t, "grows", proofRun(grows, pullRef, 1, "FX-LEND-INV01-T passed", "FX-LEND-INV02-T passed"))
		result := g.gate(t, grows, g.base, passed)
		decided(t, "a change that adds an obligation with no rule and no test", result, 0)
		if !strings.Contains(result.stdout, "coverage at "+grows+": 5 obligations: 5 designed (100.0%), 2 built (40.0%), 2 proved (40.0%)\n") {
			t.Fatalf("the decision does not give the change's coverage:\n%s", result.stdout)
		}

		falls := g.commit(t, g.base, map[string]string{
			"data/invariant.jsonl":      gateSpec["data/invariant.jsonl"] + newInvariant,
			"data/invariant_test.jsonl": invariantTest01,
			"internal/lend/rules.go":    "",
		})
		passed = g.keep(t, "falls", proofRun(falls, pullRef, 2, "FX-LEND-INV01-T passed"))
		decided(t, "a change that removes the rules and a test record", g.gate(t, falls, g.base, passed), 1,
			"FX-LEND-INV01 fell from built", "FX-LEND-INV02 fell from built", "FX-LEND-INV02 fell from proved")
	})

	t.Run("ST-ASR-UC02-R01-T02 A change is not gated when Lathe finds an error in the spec at the base head or at the change", func(t *testing.T) {
		g := newGateRepository(t)
		outcomes := []string{"FX-LEND-INV01-T passed", "FX-LEND-INV02-T passed"}
		// A dangling test record: Lathe finds an error and gives no coverage.
		dangling := `{"id":"FX-LEND-INV03-T","title":"Borrower","invariant_id":"FX-LEND-INV03","level":"unit","state":"automated","file":"internal/lend/missing_test.go","when":"Lends to a stranger.","then":"Refused."}` + "\n"
		brokenBase := g.commit(t, g.base, map[string]string{"data/invariant_test.jsonl": invariantTest01 + invariantTest02 + dangling})
		// The change drops every rule, test and file: against a base read as empty it would pass.
		dropAll := g.commit(t, brokenBase, map[string]string{
			"data/invariant_test.jsonl": "",
			"data/rule.jsonl":           "",
			"internal/lend/rules.go":    "",
		})
		brokenChange := g.commit(t, g.base, map[string]string{"data/invariant_test.jsonl": invariantTest01 + invariantTest02 + dangling})
		for name, check := range map[string]struct{ commit, base string }{
			"an error at the base head": {dropAll, brokenBase},
			"an error at the change":    {brokenChange, g.base},
		} {
			result := g.gate(t, check.commit, check.base, g.keep(t, "proof", proofRun(check.commit, pullRef, 16, outcomes...)))
			if result.code != 2 || result.stdout != "" {
				t.Fatalf("%s: assure gate change exits %d and prints a decision:\n%s%s", name, result.code, result.stdout, result.stderr)
			}
			if !strings.Contains(result.stderr, "no decision") || !strings.Contains(result.stderr, "[test-file-missing] FX-LEND-INV03-T: ") {
				t.Fatalf("%s: assure gate change does not name the spec's error:\n%s", name, result.stderr)
			}
		}
	})

	t.Run("ST-ASR-UC02-R02-T01 A change with no verified proof run at its commit, or with an automated test record not passed there, is refused", func(t *testing.T) {
		g := newGateRepository(t)
		change := g.commit(t, g.base, map[string]string{"README.md": "Fixture.\n"})
		none := noVerifiedProofRunAt(change)
		decided(t, "no attestation", g.gate(t, change, g.base, g.keep(t, "none")), 1, none+": the provider keeps no proof run attestation for it")

		forged := proofRun(change, pullRef, 3, "FX-LEND-INV01-T passed", "FX-LEND-INV02-T passed")
		forged.Signer.Subject = "forged"
		other := proofRun(strings.Repeat("ab", 20), pullRef, 4, "FX-LEND-INV01-T passed", "FX-LEND-INV02-T passed")
		other.Subjects[0].Digest = assure.SubjectDigest(change)
		decided(t, "a forged attestation and one about another commit", g.gate(t, change, g.base, g.keep(t, "forged", forged, other)), 1,
			none+": an attestation fails its signature check: the signature does not verify; the attestation at rekor:4 is refused: the statement is about "+
				other.Subjects[0].Name+" ("+assure.SubjectDigest(change)+"), not about the commit "+change)

		another := proofRun(change, "refs/pull/8/merge", 5, "FX-LEND-INV01-T passed", "FX-LEND-INV02-T passed")
		result := g.gate(t, change, g.base, g.keep(t, "another", another))
		if result.code != 1 || len(result.reasons(t)) != 1 || !strings.Contains(result.reasons(t)[0], "signed by "+proofRunWorkflow+"@refs/pull/8/merge") {
			t.Fatalf("a proof run signed at another pull request's ref is not refused for it:\n%s%s", result.stdout, result.stderr)
		}

		failing := proofRun(change, pullRef, 6, "FX-LEND-INV01-T failed")
		decided(t, "a failed test and a test with no outcome", g.gate(t, change, g.base, g.keep(t, "failing", failing)), 1,
			"FX-LEND-INV01-T failed", "FX-LEND-INV02-T has no outcome in the proof run")
		skipped := proofRun(change, pullRef, 7, "FX-LEND-INV01-T passed", "FX-LEND-INV02-T skipped")
		decided(t, "a skipped test", g.gate(t, change, g.base, g.keep(t, "skipped", skipped)), 1, "FX-LEND-INV02-T skipped")

		passed := proofRun(change, pullRef, 8, "FX-LEND-INV01-T passed", "FX-LEND-INV02-T passed")
		decided(t, "every automated test passed", g.gate(t, change, g.base, g.keep(t, "passed", passed)), 0)
	})

	t.Run("ST-ASR-UC02-R03-T01 A refusal names each obligation that fell with its share and each test record that did not pass, and exits non-zero", func(t *testing.T) {
		g := newGateRepository(t)
		change := g.commit(t, g.base, map[string]string{
			"data/invariant.jsonl":      strings.Replace(gateSpec["data/invariant.jsonl"], `{"id":"FX-LEND-INV03"`, `{"id":"FX-LEND-INV09"`, 1),
			"data/invariant_test.jsonl": invariantTest02,
			"internal/lend/rules.go":    "",
		})
		statements := g.keep(t, "proof", proofRun(change, pullRef, 9, "FX-LEND-INV02-T failed"))
		summary := filepath.Join(g.runnerTemp, "step-summary.md")
		result := g.gate(t, change, g.base, statements, "GITHUB_STEP_SUMMARY="+summary)
		decided(t, "two obligations fell from shares, one went, and a test failed", result, 1,
			"FX-LEND-INV01 fell from built", "FX-LEND-INV01 fell from proved", "FX-LEND-INV02 fell from built",
			"FX-LEND-INV03 is absent at the change (was designed at the base)", "FX-LEND-INV02-T failed")
		if !strings.HasPrefix(result.stdout, "change gate on "+change+": refused\n") {
			t.Fatalf("the decision does not say that the change is refused:\n%s", result.stdout)
		}
		data, err := os.ReadFile(summary)
		if err != nil || string(data) != "```text\n"+result.stdout+"```\n" {
			t.Fatalf("the job's step summary does not hold the decision (%v):\n%s", err, data)
		}
		if status := g.git(t, "status", "--porcelain", "--untracked-files=all"); status != "" {
			t.Fatalf("assure gate change wrote into the repository: %s", status)
		}
	})

	t.Run("ST-ASR-UC02-R04-T01 The same commit, base and proof run give the same decision and reasons in the same order", func(t *testing.T) {
		g := newGateRepository(t)
		change := g.commit(t, g.base, map[string]string{
			"data/invariant_test.jsonl":    invariantTest02,
			"internal/lend/rules.go":       "",
			".github/workflows/checks.yml": "name: Checks, changed\n",
		})
		forged := proofRun(change, mainRef, 11, "FX-LEND-INV02-T passed")
		forged.Signer.Subject = "forged"
		statements := []assure.SignedStatement{proofRun(change, pullRef, 10, "FX-LEND-INV02-T failed"), forged}
		first := g.gate(t, change, g.base, g.keep(t, "first", statements...))
		if first.code != 1 || len(first.reasons(t)) != 5 {
			t.Fatalf("the change is not refused for five reasons:\n%s%s", first.stdout, first.stderr)
		}
		second := g.gate(t, change, g.base, g.keep(t, "first", statements...))
		// The store gives the attestations in the other order, and the working tree has changes of its own.
		writeFile(t, filepath.Join(g.root, "data", "invariant_test.jsonl"), invariantTest01+invariantTest02)
		writeFile(t, filepath.Join(g.root, "internal", "lend", "rules.go"), gateSpec["internal/lend/rules.go"])
		third := g.gate(t, change, g.base, g.keep(t, "reversed", statements[1], statements[0]))
		for _, again := range []assureResult{second, third} {
			if again.code != first.code || again.stdout != first.stdout {
				t.Fatalf("the same inputs give another decision:\n%s\nwant:\n%s", again.stdout, first.stdout)
			}
		}
	})

	t.Run("ST-ASR-UC02-R06-T01 A proof run signed at the change's own ref counts only while the workflows folder is the base's, and one signed at main counts for any tree", func(t *testing.T) {
		g := newGateRepository(t)
		change := g.commit(t, g.base, map[string]string{
			".github/workflows/checks.yml":    "name: Checks\non: push\n",
			".github/workflows/extra.yml":     "name: Extra\n",
			".github/workflows/proof-run.yml": "",
		})
		outcomes := []string{"FX-LEND-INV01-T passed", "FX-LEND-INV02-T passed"}
		own := g.keep(t, "own", proofRun(change, pullRef, 12, outcomes...))
		decided(t, "a change-grade proof run with the workflows folder changed", g.gate(t, change, g.base, own), 1,
			noVerifiedProofRunAt(change)+": the attestation at rekor:12 is refused: it is signed at the change's own reference "+pullRef+
				", and the change alters the workflows folder: .github/workflows/checks.yml, .github/workflows/extra.yml, .github/workflows/proof-run.yml",
			".github/workflows/checks.yml differs from the base head",
			".github/workflows/extra.yml differs from the base head",
			".github/workflows/proof-run.yml differs from the base head")
		main := g.keep(t, "main", proofRun(change, mainRef, 13, outcomes...))
		decided(t, "a release-grade proof run with the workflows folder changed", g.gate(t, change, g.base, main), 0)

		same := g.commit(t, g.base, map[string]string{"README.md": "Fixture.\n"})
		decided(t, "a change-grade proof run with the workflows folder as the base's", g.gate(t, same, g.base, g.keep(t, "same", proofRun(same, pullRef, 14, outcomes...))), 0)
	})

	t.Run("ST-ASR-PO01-T The gate job runs after the proof run is attested, and its next run for the commit reads that attestation from the store", func(t *testing.T) {
		workflow := readGateWorkflow(t)
		gate, ok := workflow.Jobs["gate"]
		if !ok {
			t.Fatal("the proof run workflow has no gate job")
		}
		if len(gate.Needs) != 1 || gate.Needs[0] != "attest" {
			t.Fatalf("the gate job needs %v, want the attest job", gate.Needs)
		}
		if gate.If != "always() && github.event_name == 'pull_request'" {
			t.Fatalf("the gate job runs when %q, want on every pull request, also when the suites or the attest job fail", gate.If)
		}
		if len(gate.Permissions) != 1 || gate.Permissions["contents"] != "read" {
			t.Fatalf("the gate job's permissions are %v, want only contents: read", gate.Permissions)
		}
		var checkout, run *gateStep
		for i, step := range gate.Steps {
			if action, pin, ok := strings.Cut(step.Uses, "@"); step.Uses != "" && (!ok || len(pin) != 40 || action == "") {
				t.Fatalf("step %q uses %s, which is not pinned to a commit", step.Name, step.Uses)
			}
			if strings.HasPrefix(step.Uses, "actions/checkout@") && step.With["path"] == "stego" {
				checkout = &gate.Steps[i]
			}
			if strings.Contains(step.Run, "./cmd/assure gate change") {
				run = &gate.Steps[i]
			}
			if strings.Contains(step.Run, "npm ci") && step.WorkingDirectory != "lathe/lathe" {
				t.Fatalf("step %q installs packages that the change names: %s", step.Name, step.Run)
			}
		}
		if checkout == nil || checkout.With["ref"] != "${{ env.COMMIT }}" || checkout.With["fetch-depth"] != "0" {
			t.Fatalf("the gate job does not check out the change's commit with its full history: %+v", checkout)
		}
		if run == nil || !strings.Contains(run.Run, `gate change --commit "$COMMIT" --base "$BASE"`) ||
			run.Env["BASE"] != "${{ github.event.pull_request.base.sha }}" || run.WorkingDirectory != "stego" ||
			!strings.Contains(run.Run, `--lathe "$GITHUB_WORKSPACE/lathe/lathe/dist/bin/lathe.js"`) {
			t.Fatalf("the gate job does not gate the change's commit against the pull request's base: %+v", run)
		}

		g := newGateRepository(t)
		change := g.commit(t, g.base, map[string]string{"README.md": "Fixture.\n"})
		fetches := filepath.Join(g.runnerTemp, "fetches")
		before := g.gate(t, change, g.base, g.keep(t, "store"), "ASSURE_TEST_FETCHES="+fetches)
		decided(t, "before the proof run is attested", before, 1, noVerifiedProofRunAt(change)+": the provider keeps no proof run attestation for it")
		// The attest job signs the proof run, and the store keeps it.
		g.keep(t, "store", proofRun(change, pullRef, 15, "FX-LEND-INV01-T passed", "FX-LEND-INV02-T passed"))
		after := g.gate(t, change, g.base, filepath.Join(g.runnerTemp, "store.json"), "ASSURE_TEST_FETCHES="+fetches)
		decided(t, "after the proof run is attested", after, 0)
		if !strings.Contains(after.stdout, "proof run: "+assure.ProofRunPredicateType+" about "+change+", signed by "+proofRunWorkflow+"@"+pullRef+" (issuer "+assure.GitHubIssuer+"), rekor:15\n") {
			t.Fatalf("the decision does not rest on the attested proof run:\n%s", after.stdout)
		}
		asked, err := os.ReadFile(fetches)
		if err != nil || string(asked) != change+"\n"+change+"\n" {
			t.Fatalf("the store was asked for %q (%v), want the change's commit at each run", asked, err)
		}
	})
}

// noVerifiedProofRunAt is the start of the reason that the commit has no verified proof run.
func noVerifiedProofRunAt(commit string) string {
	return "no verified proof run at " + commit
}

type gateStep struct {
	Name             string            `yaml:"name"`
	Uses             string            `yaml:"uses"`
	Run              string            `yaml:"run"`
	With             map[string]string `yaml:"with"`
	Env              map[string]string `yaml:"env"`
	WorkingDirectory string            `yaml:"working-directory"`
}

type gateJob struct {
	If          string            `yaml:"if"`
	Needs       stringList        `yaml:"needs"`
	Permissions map[string]string `yaml:"permissions"`
	Steps       []gateStep        `yaml:"steps"`
}

// readGateWorkflow reads the jobs of the proof run workflow from the repository this test runs in.
func readGateWorkflow(t *testing.T) gateWorkflow {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "proof-run.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow gateWorkflow
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	return workflow
}

type gateWorkflow struct {
	Jobs map[string]gateJob `yaml:"jobs"`
}
