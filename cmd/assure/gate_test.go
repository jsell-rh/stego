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
// a constraint of the product that is designed only. It has two workflow files, and a go.mod, so its code faults
// can be read: the rule FX-LEND-INV02-RL names an identifier that its file does not declare (ST-STD-CODE-R02).
var gateSpec = map[string]string{
	"lathe.config.json":    `{"schema": 2, "data": "data", "tests": ["**/*_test.go"]}` + "\n",
	"go.mod":               "module example.com/fixture\n\ngo 1.26\n",
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

// reasons is the list of reasons the decision prints, one per line, or none when it prints none. After a passed
// release decision, assure gate release prints where it wrote the decision predicate.
func (r assureResult) reasons(t *testing.T) []string {
	t.Helper()
	_, list, ok := strings.Cut(r.stdout, "\nreasons (")
	if !ok {
		if !strings.HasSuffix(r.stdout, "\nreasons: none\n") && !strings.Contains(r.stdout, "\nreasons: none\ndecision predicate ") {
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
	ID               string            `yaml:"id"`
	If               string            `yaml:"if"`
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

// fullyProved changes the gate's fixture spec so that every obligation is designed, built and proved and the tree
// has no code-standard fault: a rule and an automated test for each invariant, a test for the constraint, the value
// type's file, and rules that each declare the identifier their record names.
var fullyProved = map[string]string{
	"data/rule.jsonl": gateSpec["data/rule.jsonl"] +
		`{"id":"FX-LEND-INV03-RL","title":"lend.borrowerKnown","invariant_id":"FX-LEND-INV03","file":"internal/lend/rules.go"}` + "\n",
	"data/invariant_test.jsonl":  invariantTest01 + invariantTest02 + invariantTest03,
	"data/constraint_test.jsonl": `{"id":"FX-VO01-C01-T","title":"In the future","constraint_id":"FX-VO01-C01","level":"unit","state":"automated","file":"internal/lend/rules_test.go","accepts":"Tomorrow.","refuses":"Yesterday."}` + "\n",
	"internal/lend/rules.go": "package lend\n\nfunc oneLoanPerTool(open int) bool { return open < 2 }\n\n" +
		"func dueAfterStart(start, due int) bool { return due > start }\n\n" +
		"func borrowerKnown(known map[string]bool, borrower string) bool { return known[borrower] }\n",
	"internal/lend/due.go": "package lend\n\n// DueDate is the day a loan ends.\ntype DueDate struct{ Day string }\n",
	"internal/lend/rules_test.go": "package lend\n\nimport \"testing\"\n\nfunc TestRules(t *testing.T) {\n" +
		"\tt.Run(\"FX-LEND-INV01-T one loan\", func(t *testing.T) {})\n\tt.Run(\"FX-LEND-INV02-T due after start\", func(t *testing.T) {})\n" +
		"\tt.Run(\"FX-LEND-INV03-T borrower known\", func(t *testing.T) {})\n\tt.Run(\"FX-VO01-C01-T in the future\", func(t *testing.T) {})\n}\n",
}

const invariantTest03 = `{"id":"FX-LEND-INV03-T","title":"Borrower known","invariant_id":"FX-LEND-INV03","level":"unit","state":"automated","file":"internal/lend/rules_test.go","when":"Lends to a stranger.","then":"Refused."}` + "\n"

// allPassed is the outcome of each automated test record of the fully proved spec: passed.
var allPassed = []string{"FX-LEND-INV01-T passed", "FX-LEND-INV02-T passed", "FX-LEND-INV03-T passed", "FX-VO01-C01-T passed"}

// with is the files given with each change written over them, as a commit of g.commit takes them.
func with(files map[string]string, changes map[string]string) map[string]string {
	merged := make(map[string]string, len(files)+len(changes))
	for name, content := range files {
		merged[name] = content
	}
	for name, content := range changes {
		merged[name] = content
	}
	return merged
}

// releasePredicate is where the gate-release job has assure gate release write the decision predicate.
func (g gateRepository) releasePredicate(t *testing.T) string {
	t.Helper()
	folder := filepath.Join(g.runnerTemp, "decision")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(folder, "decision.json")
}

// release runs assure gate release in the repository as the gate-release job of a push to main would, with the
// attestations in the file and the predicate to write, and with any change to the environment given as
// NAME=value.
func (g gateRepository) release(t *testing.T, commit, attestations, predicate string, env ...string) assureResult {
	t.Helper()
	command := exec.Command(os.Args[0], "gate", "release", "--commit", commit, "--predicate", predicate, "--lathe", g.lathe)
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
		"GITHUB_RUN_ID=36000000001",
		"GITHUB_RUN_ATTEMPT=1",
		"GITHUB_SERVER_URL=https://github.com",
		"GITHUB_REPOSITORY=jsell-rh/stego",
		"GITHUB_REF=" + mainRef,
		"GITHUB_WORKFLOW_REF=" + trustedWorkflowRef,
		"RUNNER_TEMP=" + g.runnerTemp,
	}, env...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("assure gate release did not run: %v", err)
	}
	return assureResult{code: command.ProcessState.ExitCode(), stdout: stdout.String(), stderr: stderr.String()}
}

// refusedRelease checks that the release gate refused the release for exactly the reasons, and wrote no predicate.
func refusedRelease(t *testing.T, name string, result assureResult, predicate string, reasons ...string) {
	t.Helper()
	decided(t, name, result, 1, reasons...)
	if !strings.HasPrefix(result.stdout, "release gate on ") || !strings.Contains(result.stdout, ": refused\n") {
		t.Fatalf("%s: the decision does not say that the release is refused:\n%s", name, result.stdout)
	}
	noPredicate(t, name, predicate)
}

// noPredicate checks that no decision predicate was written.
func noPredicate(t *testing.T, name, predicate string) {
	t.Helper()
	if _, err := os.Stat(predicate); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s: a decision predicate is written at %s (%v)", name, predicate, err)
	}
}

// releaseDecision is the decision predicate as the attestation job reads it.
type releaseDecision struct {
	Commit     string   `json:"commit"`
	Kind       string   `json:"kind"`
	Passed     bool     `json:"passed"`
	Reasons    []string `json:"reasons"`
	CodeFaults []string `json:"codeFaults"`
}

// readReleaseDecision reads the decision predicate, and refuses a field it does not know.
func readReleaseDecision(t *testing.T, file string) releaseDecision {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		releaseDecision
		Coverage map[string]int `json:"coverage"`
		ProofRun map[string]any `json:"proofRun"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("the decision predicate does not read: %v:\n%s", err, data)
	}
	return document.releaseDecision
}

func TestGateRelease(t *testing.T) {
	t.Run("ST-ASR-UC03-R01-T01 A release is refused while any obligation is not designed, built and proved, and each such obligation is named", func(t *testing.T) {
		g := newGateRepository(t)
		predicate := g.releasePredicate(t)
		// FX-LEND-INV02 loses its test record, and FX-LEND-INV03 its rule and its test record.
		short := g.commit(t, g.base, with(fullyProved, map[string]string{
			"data/rule.jsonl":           gateSpec["data/rule.jsonl"],
			"data/invariant_test.jsonl": invariantTest01,
		}))
		result := g.release(t, short, g.keep(t, "short", proofRun(short, mainRef, 41, "FX-LEND-INV01-T passed", "FX-VO01-C01-T passed")), predicate)
		refusedRelease(t, "a commit with obligations not built or not proved", result, predicate,
			"FX-LEND-INV02 is not proved", "FX-LEND-INV03 is not built", "FX-LEND-INV03 is not proved")
		if !strings.Contains(result.stdout, "coverage at "+short+": 4 obligations: 4 designed (100.0%), 3 built (75.0%), 2 proved (50.0%)\n") {
			t.Fatalf("the decision does not give the coverage at the commit:\n%s", result.stdout)
		}

		full := g.commit(t, g.base, fullyProved)
		decided(t, "a fully proved commit", g.release(t, full, g.keep(t, "full", proofRun(full, mainRef, 42, allPassed...)), predicate), 0)
	})

	t.Run("ST-ASR-UC03-R02-T01 A release commit with no verified proof run, or with an automated test record not passed there, is refused", func(t *testing.T) {
		g := newGateRepository(t)
		predicate := g.releasePredicate(t)
		full := g.commit(t, g.base, fullyProved)
		none := noVerifiedProofRunAt(full)
		refusedRelease(t, "no attestation", g.release(t, full, g.keep(t, "none"), predicate), predicate,
			none+": the provider keeps no proof run attestation for it")

		forged := proofRun(full, mainRef, 43, allPassed...)
		forged.Signer.Subject = "forged"
		refusedRelease(t, "a forged attestation", g.release(t, full, g.keep(t, "forged", forged), predicate), predicate,
			none+": an attestation fails its signature check: the signature does not verify")

		failing := proofRun(full, mainRef, 44, "FX-LEND-INV01-T passed", "FX-LEND-INV02-T failed", "FX-VO01-C01-T passed")
		refusedRelease(t, "a failed test and a test with no outcome", g.release(t, full, g.keep(t, "failing", failing), predicate), predicate,
			"FX-LEND-INV02-T failed", "FX-LEND-INV03-T has no outcome in the proof run")
		skipped := proofRun(full, mainRef, 45, "FX-LEND-INV01-T passed", "FX-LEND-INV02-T passed", "FX-LEND-INV03-T passed", "FX-VO01-C01-T skipped")
		refusedRelease(t, "a skipped test", g.release(t, full, g.keep(t, "skipped", skipped), predicate), predicate, "FX-VO01-C01-T skipped")

		decided(t, "every automated test passed", g.release(t, full, g.keep(t, "passed", proofRun(full, mainRef, 46, allPassed...)), predicate), 0)
	})

	t.Run("ST-ASR-UC03-R03-T01 The release gate job runs on a push to main, and only the attest-release job, which runs no repository code, signs a passed decision with the commit as subject", func(t *testing.T) {
		workflow := readWorkflow(t)
		gates, ok := readGateWorkflow(t).Jobs["gate-release"]
		if !ok {
			t.Fatal("the proof run workflow has no gate-release job")
		}
		if len(gates.Needs) != 1 || gates.Needs[0] != "attest" {
			t.Fatalf("the gate-release job needs %v, want the attest job", gates.Needs)
		}
		if gates.If != "${{ !cancelled() && github.event_name == 'push' && github.ref == 'refs/heads/main' }}" {
			t.Fatalf("the gate-release job runs when %q, want on each push to main, also when the suites or the attest job fail", gates.If)
		}
		if len(gates.Permissions) != 1 || gates.Permissions["contents"] != "read" {
			t.Fatalf("the gate-release job's permissions are %v, want only contents: read", gates.Permissions)
		}
		var checkout, run, upload *gateStep
		for i, step := range gates.Steps {
			if action, pin, ok := strings.Cut(step.Uses, "@"); step.Uses != "" && (!ok || len(pin) != 40 || action == "") {
				t.Fatalf("step %q uses %s, which is not pinned to a commit", step.Name, step.Uses)
			}
			if strings.HasPrefix(step.Uses, "actions/checkout@") && step.With["path"] == "stego" {
				checkout = &gates.Steps[i]
			}
			if strings.Contains(step.Run, "./cmd/assure gate release") {
				run = &gates.Steps[i]
			}
			if strings.HasPrefix(step.Uses, "actions/upload-artifact@") {
				upload = &gates.Steps[i]
			}
			if strings.Contains(step.Run, "npm ci") && step.WorkingDirectory != "lathe/lathe" {
				t.Fatalf("step %q installs packages that the commit names: %s", step.Name, step.Run)
			}
		}
		if checkout == nil || checkout.With["ref"] != "${{ env.COMMIT }}" || checkout.With["fetch-depth"] != "0" {
			t.Fatalf("the gate-release job does not check out the commit with its full history: %+v", checkout)
		}
		if run == nil || run.WorkingDirectory != "stego" ||
			!strings.Contains(run.Run, `gate release --commit "$COMMIT" --predicate "$RUNNER_TEMP/decision/decision.json"`) ||
			!strings.Contains(run.Run, `--lathe "$GITHUB_WORKSPACE/lathe/lathe/dist/bin/lathe.js"`) {
			t.Fatalf("the gate-release job does not gate the commit with the Lathe the workflow builds: %+v", run)
		}
		// The upload has no condition of its own, so it runs only when the gate passed.
		if upload == nil || upload.If != "" || upload.With["name"] != "release-decision" ||
			upload.With["path"] != "${{ runner.temp }}/decision/decision.json" || upload.With["if-no-files-found"] != "error" {
			t.Fatalf("the gate-release job does not upload the predicate only when the release passed: %+v", upload)
		}

		signers := 0
		for name, job := range workflow.Jobs {
			if job.Permissions["id-token"] != "" || job.Permissions["attestations"] != "" {
				signers++
				if name != "attest" && name != "attest-release" {
					t.Fatalf("the job %s may get an identity token or write attestations", name)
				}
			}
		}
		attest := workflow.Jobs["attest-release"]
		if signers != 2 || len(attest.Permissions) != 2 || attest.Permissions["id-token"] != "write" || attest.Permissions["attestations"] != "write" {
			t.Fatalf("the attest-release job's permissions are %v, want only id-token and attestations", attest.Permissions)
		}
		if len(attest.Needs) != 1 || attest.Needs[0] != "gate-release" {
			t.Fatalf("the attest-release job needs %v, want the gate-release job", attest.Needs)
		}
		var download, signs *workflowStep
		for i, step := range attest.Steps {
			if strings.HasPrefix(step.Uses, "actions/checkout@") || strings.HasPrefix(step.Uses, "actions/setup-") {
				t.Fatalf("the attest-release job checks out or sets up code: %s", step.Uses)
			}
			for _, word := range []string{"go ", "python", "npm", "node ", "make", "./"} {
				if strings.Contains(step.Run, word) {
					t.Fatalf("the attest-release job's step %q runs %q", step.Name, word)
				}
			}
			if action, pin, ok := strings.Cut(step.Uses, "@"); step.Uses != "" && (!ok || len(pin) != 40 || action == "") {
				t.Fatalf("step %q uses %s, which is not pinned to a commit", step.Name, step.Uses)
			}
			if strings.HasPrefix(step.Uses, "actions/download-artifact@") {
				download = &attest.Steps[i]
			}
			if strings.HasPrefix(step.Uses, "actions/attest@") {
				signs = &attest.Steps[i]
			}
		}
		if download == nil || download.With["name"] != "release-decision" || download.With["path"] != "${{ runner.temp }}/decision" {
			t.Fatalf("the attest-release job does not download the decision predicate: %+v", download)
		}
		if signs == nil {
			t.Fatal("the attest-release job does not sign the decision with actions/attest")
		}
		for input, value := range map[string]string{
			"subject-name":   "${{ steps.subject.outputs.name }}",
			"subject-digest": "${{ steps.subject.outputs.digest }}",
			"predicate-type": assure.ReleaseDecisionPredicateType,
			"predicate-path": "${{ runner.temp }}/decision/decision.json",
		} {
			if signs.With[input] != value {
				t.Fatalf("the attestation action's %s is %q, want %q", input, signs.With[input], value)
			}
		}

		commit := strings.Repeat("ab", 20)
		name, digest, err := runJobSubjectStep(t, "attest-release", commit)
		sum := sha256.Sum256([]byte(commit))
		if err != nil || name != "git+https://github.com/jsell-rh/stego@"+commit || digest != "sha256:"+hex.EncodeToString(sum[:]) {
			t.Fatalf("the attest-release job names the subject %s (%s), %v, not the commit %s", name, digest, err, commit)
		}
		if _, _, err := runJobSubjectStep(t, "attest-release", "main"); err == nil {
			t.Fatal("the attest-release job names a branch as the subject")
		}
		passed := `{"commit": "` + commit + `", "kind": "release", "passed": true, "reasons": []}`
		if err := runDecisionCheck(t, commit, passed); err != nil {
			t.Fatalf("the attest-release job refuses a passed release decision about the commit: %v", err)
		}
		for name, document := range map[string]string{
			"about another commit": strings.Replace(passed, commit, strings.Repeat("cd", 20), 1),
			"refused":              strings.Replace(passed, `"passed": true`, `"passed": false`, 1),
			"of the change gate":   strings.Replace(passed, `"kind": "release"`, `"kind": "change"`, 1),
			"with no kind":         `{"commit": "` + commit + `", "passed": true}`,
		} {
			if err := runDecisionCheck(t, commit, document); err == nil {
				t.Fatalf("the attest-release job accepts a decision %s", name)
			}
		}
	})

	t.Run("ST-ASR-UC03-R04-T01 A release commit whose tree has a code-standard fault is refused, and each fault is named with its file and rule", func(t *testing.T) {
		g := newGateRepository(t)
		predicate := g.releasePredicate(t)
		faulty := g.commit(t, g.base, with(fullyProved, map[string]string{
			"data/rule.jsonl":        strings.Replace(fullyProved["data/rule.jsonl"], "lend.borrowerKnown", "lend.borrowerIsKnown", 1),
			"internal/lend/extra.go": "package lend\n\nconst extra = 1\n",
		}))
		result := g.release(t, faulty, g.keep(t, "faulty", proofRun(faulty, mainRef, 47, allPassed...)), predicate)
		refusedRelease(t, "a tree with two code-standard faults", result, predicate,
			"internal/lend/rules.go: FX-LEND-INV03-RL names lend.borrowerIsKnown, which the file does not declare (ST-STD-CODE-R02, FX-LEND-INV03-RL)",
			"internal/lend/extra.go: no technical record names the file, directly or through a folder (ST-STD-CODE-R05, internal/lend/extra.go)")
		if !strings.Contains(result.stdout, "coverage at "+faulty+": 4 obligations: 4 designed (100.0%), 4 built (100.0%), 4 proved (100.0%)\n") ||
			!strings.Contains(result.stdout, "\ncode faults (2):\n"+
				"internal/lend/rules.go: FX-LEND-INV03-RL names lend.borrowerIsKnown, which the file does not declare (ST-STD-CODE-R02)\n"+
				"internal/lend/extra.go: no technical record names the file, directly or through a folder (ST-STD-CODE-R05)\nreasons (2):\n") {
			t.Fatalf("the decision does not list the faults of a fully proved commit:\n%s", result.stdout)
		}
	})

	t.Run("ST-ASR-UC03-R05-T01 A release takes only a proof run signed at main, and one signed at any other ref is refused with the ref named", func(t *testing.T) {
		g := newGateRepository(t)
		predicate := g.releasePredicate(t)
		full := g.commit(t, g.base, fullyProved)
		refusal := func(logIndex, reference string) string {
			return noVerifiedProofRunAt(full) + ": the attestation at rekor:" + logIndex + " is refused: it is signed by " + proofRunWorkflow +
				" at " + reference + ", not at " + mainRef + ", and only release-grade evidence counts here"
		}
		own := proofRun(full, pullRef, 48, allPassed...)
		refusedRelease(t, "a proof run signed at a pull request's ref", g.release(t, full, g.keep(t, "own", own), predicate), predicate,
			refusal("48", pullRef))
		branch := proofRun(full, "refs/heads/feature", 49, allPassed...)
		refusedRelease(t, "a proof run signed at another branch", g.release(t, full, g.keep(t, "branch", branch), predicate), predicate,
			refusal("49", "refs/heads/feature"))

		result := g.release(t, full, g.keep(t, "both", own, proofRun(full, mainRef, 50, allPassed...)), predicate)
		decided(t, "a proof run signed at main beside one signed at a pull request's ref", result, 0)
		if !strings.Contains(result.stdout, "proof run: "+assure.ProofRunPredicateType+" about "+full+", signed by "+proofRunWorkflow+"@"+mainRef+" (issuer "+assure.GitHubIssuer+"), rekor:50\n") {
			t.Fatalf("the decision does not rest on the proof run signed at main:\n%s", result.stdout)
		}
	})

	t.Run("ST-ASR-INV09-T A passed release decision is handed to the attestation action about its commit, for the trusted signer at main, and nothing else is", func(t *testing.T) {
		g := newGateRepository(t)
		full := g.commit(t, g.base, fullyProved)
		passed := g.keep(t, "passed", proofRun(full, mainRef, 51, allPassed...))
		predicate := g.releasePredicate(t)
		result := g.release(t, full, passed, predicate)
		decided(t, "a fully proved commit", result, 0)
		if !strings.Contains(result.stdout, "attestation: "+assure.ReleaseDecisionPredicateType+" about "+full+", for "+proofRunWorkflow+"@"+mainRef+" (issuer "+assure.GitHubIssuer+") to sign\n") {
			t.Fatalf("the passed decision does not name the attestation the trusted signer is to make:\n%s", result.stdout)
		}
		if decision := readReleaseDecision(t, predicate); decision.Commit != full || decision.Kind != "release" || !decision.Passed || len(decision.Reasons) != 0 {
			t.Fatalf("the predicate is not a passed release decision about %s: %+v", full, decision)
		}

		for name, env := range map[string]string{
			"in a job of the trusted workflow at a pull request's ref": "GITHUB_WORKFLOW_REF=jsell-rh/stego/.github/workflows/proof-run.yml@" + pullRef,
			"in a job of another workflow":                             "GITHUB_WORKFLOW_REF=jsell-rh/stego/.github/workflows/checks.yml@" + mainRef,
		} {
			other := filepath.Join(g.runnerTemp, "decision", "other.json")
			result := g.release(t, full, passed, other, env)
			if result.code != 2 || result.stdout != "" || !strings.Contains(result.stderr, "is not the provider's trusted signer "+proofRunWorkflow+" at "+mainRef) {
				t.Fatalf("%s: assure gate release exits %d:\n%s%s", name, result.code, result.stdout, result.stderr)
			}
			noPredicate(t, name, other)
		}

		refusedPredicate := filepath.Join(g.runnerTemp, "decision", "refused.json")
		failed := g.keep(t, "failed", proofRun(full, mainRef, 52, "FX-LEND-INV01-T failed", "FX-LEND-INV02-T passed", "FX-LEND-INV03-T passed", "FX-VO01-C01-T passed"))
		refused := g.release(t, full, failed, refusedPredicate)
		refusedRelease(t, "a refused release", refused, refusedPredicate, "FX-LEND-INV01-T failed")
		if strings.Contains(refused.stdout, "attestation:") {
			t.Fatalf("the refused decision names an attestation:\n%s", refused.stdout)
		}

		inside := filepath.Join(g.root, "decision.json")
		if result := g.release(t, full, passed, inside); result.code != 2 || !strings.Contains(result.stderr, "inside the repository") {
			t.Fatalf("a predicate inside the repository: assure gate release exits %d:\n%s%s", result.code, result.stdout, result.stderr)
		}
		noPredicate(t, "a predicate inside the repository", inside)
		if status := g.git(t, "status", "--porcelain", "--untracked-files=all"); status != "" {
			t.Fatalf("assure gate release wrote into the repository: %s", status)
		}

		var signs *workflowStep
		steps := readWorkflow(t).Jobs["attest-release"].Steps
		for i, step := range steps {
			if strings.HasPrefix(step.Uses, "actions/attest@") {
				signs = &steps[i]
			}
		}
		if signs == nil || signs.With["predicate-type"] != assure.ReleaseDecisionPredicateType || signs.With["subject-digest"] != "${{ steps.subject.outputs.digest }}" {
			t.Fatalf("the attest-release job does not sign the decision with its commit as subject: %+v", signs)
		}
		name, _, err := runJobSubjectStep(t, "attest-release", full)
		if err != nil || name != "git+https://github.com/jsell-rh/stego@"+full {
			t.Fatalf("the attest-release job names the subject %s (%v), not the release commit", name, err)
		}
	})
}

// runJobSubjectStep runs, with bash, the step with the ID subject of the job, as GitHub Actions would for the
// commit, and returns the subject name and digest it gives.
func runJobSubjectStep(t *testing.T, job, commit string) (string, string, error) {
	t.Helper()
	var script string
	for _, step := range readWorkflow(t).Jobs[job].Steps {
		if step.ID == "subject" {
			script = step.Run
		}
	}
	if script == "" {
		t.Fatalf("the %s job has no step with the ID subject", job)
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

// runDecisionCheck runs, with bash, the attest-release job's step that checks the downloaded predicate, on the
// decision document given.
func runDecisionCheck(t *testing.T, commit, document string) error {
	t.Helper()
	var script string
	for _, step := range readWorkflow(t).Jobs["attest-release"].Steps {
		if step.Name == "Check that the predicate is a passed release decision about the commit" {
			script = step.Run
		}
	}
	if script == "" {
		t.Fatal("the attest-release job has no step that checks the decision predicate")
	}
	runnerTemp := t.TempDir()
	writeFile(t, filepath.Join(runnerTemp, "decision", "decision.json"), document)
	command := exec.Command("bash", "-e", "-c", script)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "COMMIT=" + commit, "RUNNER_TEMP=" + runnerTemp}
	if out, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}

func TestGateChangeCodeStandard(t *testing.T) {
	t.Run("ST-ASR-UC02-R05-T01 A change is refused for a code-standard fault the base head does not have, and a fault the base has is reported, not refused", func(t *testing.T) {
		g := newGateRepository(t)
		base := g.commit(t, g.base, map[string]string{"internal/lend/old.go": "package lend\n\nconst old = 1\n"})
		outcomes := []string{"FX-LEND-INV01-T passed", "FX-LEND-INV02-T passed"}
		const (
			rules = "internal/lend/rules.go: FX-LEND-INV02-RL names lend.dueAfterStart, which the file does not declare (ST-STD-CODE-R02)"
			old   = "internal/lend/old.go: no technical record names the file, directly or through a folder (ST-STD-CODE-R05)"
			added = "internal/lend/new.go: no technical record names the file, directly or through a folder (ST-STD-CODE-R05)"
		)

		change := g.commit(t, base, map[string]string{"internal/lend/new.go": "package lend\n\nconst added = 2\n"})
		result := g.gate(t, change, base, g.keep(t, "added", proofRun(change, pullRef, 61, outcomes...)))
		decided(t, "a change that adds a file no record names", result, 1,
			"internal/lend/new.go: no technical record names the file, directly or through a folder (ST-STD-CODE-R05, internal/lend/new.go)")
		if !strings.Contains(result.stdout, "\ncode faults (3):\n"+rules+", present at base\n"+added+", not at base\n"+old+", present at base\nreasons (1):\n") {
			t.Fatalf("the decision does not list each fault and whether the base head has it:\n%s", result.stdout)
		}

		kept := g.commit(t, base, map[string]string{"README.md": "Fixture.\n"})
		result = g.gate(t, kept, base, g.keep(t, "kept", proofRun(kept, pullRef, 62, outcomes...)))
		decided(t, "a change that keeps the base head's faults", result, 0)
		if !strings.Contains(result.stdout, "\ncode faults (2):\n"+rules+", present at base\n"+old+", present at base\nreasons: none\n") {
			t.Fatalf("the decision does not report the faults the base head has:\n%s", result.stdout)
		}

		// The rule names another identifier that its file does not declare: the fault's reason changes, its rule and
		// key do not, so it is not new.
		renamed := g.commit(t, base, map[string]string{"data/rule.jsonl": strings.Replace(gateSpec["data/rule.jsonl"], "lend.dueAfterStart", "lend.dueBeforeEnd", 1)})
		result = g.gate(t, renamed, base, g.keep(t, "renamed", proofRun(renamed, pullRef, 63, outcomes...)))
		decided(t, "a change under which a fault of the base head changes its reason", result, 0)
		if !strings.Contains(result.stdout, "internal/lend/rules.go: FX-LEND-INV02-RL names lend.dueBeforeEnd, which the file does not declare (ST-STD-CODE-R02), present at base\n") {
			t.Fatalf("the decision does not report the fault with its new reason as present at base:\n%s", result.stdout)
		}

		fixed := g.commit(t, base, map[string]string{"internal/lend/old.go": ""})
		result = g.gate(t, fixed, base, g.keep(t, "fixed", proofRun(fixed, pullRef, 64, outcomes...)))
		decided(t, "a change that fixes a fault", result, 0)
		if !strings.Contains(result.stdout, "\ncode faults (1):\n"+rules+", present at base\nreasons: none\n") {
			t.Fatalf("the decision lists a fault the change fixed:\n%s", result.stdout)
		}
	})
}
