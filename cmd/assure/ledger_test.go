package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/assure"
)

// ledgerSpec is the spec of the ledger's fixture repository: two automated test records and a manual one.
var ledgerSpec = map[string]string{
	"lathe.config.json": `{"schema": 2, "data": "data", "tests": ["**/*_test.go"]}` + "\n",
	"data/invariant_test.jsonl": `{"id":"FX-LEND-INV01-T","title":"One loan","invariant_id":"FX-LEND-INV01","level":"unit","state":"automated","file":"internal/lend/rules_test.go"}
{"id":"FX-LEND-INV02-T","title":"Due after start","invariant_id":"FX-LEND-INV02","level":"unit","state":"automated","file":"internal/lend/rules_test.go"}
{"id":"FX-LEND-INV03-T","title":"Borrower known","invariant_id":"FX-LEND-INV03","level":"unit","state":"manual","manual_reason":"A person checks it."}
`,
	".github/workflows/proof-run.yml": "name: Proof run\n",
}

// ledgerHistory is a fixture repository with five commits on one line, oldest first, each with the ledger's
// spec, and a runner folder outside it.
type ledgerHistory struct {
	gateRepository
	commits []string
}

func newLedgerHistory(t *testing.T) ledgerHistory {
	t.Helper()
	folder := t.TempDir()
	h := ledgerHistory{gateRepository: gateRepository{root: filepath.Join(folder, "repository"), runnerTemp: filepath.Join(folder, "runner-temp")}}
	for _, dir := range []string{h.root, h.runnerTemp} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h.git(t, "init", "-q")
	parent := ""
	for i := 1; i <= 5; i++ {
		files := map[string]string{"README.md": strings.Repeat("Fixture.\n", i)}
		if i == 1 {
			files = with(ledgerSpec, files)
		}
		parent = h.commit(t, parent, files)
		h.commits = append(h.commits, parent)
	}
	h.base = h.commits[0]
	return h
}

// at is the commit n of the history, from 1 for the oldest.
func (h ledgerHistory) at(n int) string {
	return h.commits[n-1]
}

// forged is a proof run statement whose signature does not verify.
func forged(statement assure.SignedStatement) assure.SignedStatement {
	statement.Signer.Subject = "forged"
	return statement
}

// ledger runs assure ledger in the repository, with the attestations in the file, the flags, and any change to
// the environment given as NAME=value. It logs each commit the store is asked for in the file fetches.
func (g gateRepository) ledger(t *testing.T, attestations, fetches string, flags []string, env ...string) assureResult {
	t.Helper()
	command := exec.Command(os.Args[0], append([]string{"ledger"}, flags...)...)
	command.Dir = g.root
	command.Env = append([]string{
		"ASSURE_TEST_MAIN=1",
		"ASSURE_TEST_ATTESTATIONS=" + attestations,
		"ASSURE_TEST_FETCHES=" + fetches,
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + g.runnerTemp,
		"TMPDIR=" + g.runnerTemp,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GITHUB_SERVER_URL=https://github.com",
		"GITHUB_REPOSITORY=jsell-rh/stego",
	}, env...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("assure ledger did not run: %v", err)
	}
	return assureResult{code: command.ProcessState.ExitCode(), stdout: stdout.String(), stderr: stderr.String()}
}

// fetched is the list of commits the store was asked for, in order, from the file.
func fetched(t *testing.T, file string) []string {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

// signedAtMain is how the ledger names a proof run signed by the proof run workflow at main.
const signedAtMain = "signed by " + proofRunWorkflow + "@" + mainRef + " (issuer " + assure.GitHubIssuer + ")"

// historyStatements are the attestations of the history: at commit 5 a proof run with FX-LEND-INV02-T failed, at
// commit 4 none, at commit 3 one whose signature does not verify, at commit 2 one with both automated test
// records passed, and at commit 1 one with FX-LEND-INV02-T passed.
func historyStatements(h ledgerHistory) []assure.SignedStatement {
	return []assure.SignedStatement{
		proofRun(h.at(5), mainRef, 5, "FX-LEND-INV01-T passed", "FX-LEND-INV02-T failed"),
		forged(proofRun(h.at(3), mainRef, 3, "FX-LEND-INV01-T passed", "FX-LEND-INV02-T passed")),
		proofRun(h.at(2), mainRef, 2, "FX-LEND-INV01-T passed", "FX-LEND-INV02-T passed"),
		proofRun(h.at(1), mainRef, 1, "FX-LEND-INV02-T passed"),
	}
}

// historyLedger is what assure ledger prints for commit 5 of the history with historyStatements.
func historyLedger(h ledgerHistory) string {
	return "proof ledger at " + h.at(5) + ": 3 test records, 4 commits walked of at most 200\n" +
		"FX-LEND-INV01-T (automated): passed, last passed at " + h.at(5) + "\n" +
		"FX-LEND-INV02-T (automated): failed, last passed at " + h.at(2) + "\n" +
		"FX-LEND-INV03-T (manual): none, last passed not within 4 commits\n" +
		"proof run at " + h.at(5) + ": release grade, " + signedAtMain + ", rekor:5\n" +
		"refused at " + h.at(3) + ": an attestation fails its signature check: the signature does not verify\n" +
		"not passed (2): FX-LEND-INV02-T, FX-LEND-INV03-T\n"
}

func TestLedger(t *testing.T) {
	t.Run("ST-ASR-UC04-R01-T01 The ledger comes from verified attestations and the committed spec, and no file of the working tree changes it", func(t *testing.T) {
		h := newLedgerHistory(t)
		attestations := h.keep(t, "history", historyStatements(h)...)
		clean := h.ledger(t, attestations, "", []string{"--commit", h.at(5)})
		if clean.code != 1 || clean.stdout != historyLedger(h) {
			t.Fatalf("assure ledger exited %d with:\n%s%s\nwant:\n%s", clean.code, clean.stdout, clean.stderr, historyLedger(h))
		}

		// The working tree is at another commit, its spec says something else, and it holds evidence files that
		// say every test passed: the ledger is the same, and assure ledger writes nothing into the repository.
		h.git(t, "checkout", "-q", "--detach", h.at(1))
		writeFile(t, filepath.Join(h.root, "data", "invariant_test.jsonl"), `{"id":"FX-LEND-INV09-T","title":"Other","invariant_id":"FX-LEND-INV09","level":"unit","state":"automated","file":"x_test.go"}`+"\n")
		writeFile(t, filepath.Join(h.root, assure.ProofRunFile), `{"outcomes":[{"testRecord":"FX-LEND-INV02-T","result":"passed"}]}`+"\n")
		writeFile(t, filepath.Join(h.root, "evidence", "ledger.txt"), "FX-LEND-INV02-T (automated): passed\n")
		before := h.git(t, "status", "--porcelain", "--untracked-files=all")
		dirty := h.ledger(t, attestations, "", []string{"--commit", h.at(5)})
		if dirty.code != clean.code || dirty.stdout != clean.stdout {
			t.Fatalf("files of the working tree change the ledger: exit %d with:\n%s%s", dirty.code, dirty.stdout, dirty.stderr)
		}
		if after := h.git(t, "status", "--porcelain", "--untracked-files=all"); after != before {
			t.Fatalf("assure ledger changed the repository:\n%s\nwas:\n%s", after, before)
		}

		// With no attestation anywhere there is no result, whatever the files say.
		none := h.ledger(t, h.keep(t, "none"), "", []string{"--commit", h.at(5)})
		want := "proof ledger at " + h.at(5) + ": 3 test records, 5 commits walked of at most 200\n" +
			"FX-LEND-INV01-T (automated): none, last passed not within 5 commits\n" +
			"FX-LEND-INV02-T (automated): none, last passed not within 5 commits\n" +
			"FX-LEND-INV03-T (manual): none, last passed not within 5 commits\n" +
			"proof run at " + h.at(5) + ": none, as the provider keeps no proof run attestation for it\n" +
			"not passed (3): FX-LEND-INV01-T, FX-LEND-INV02-T, FX-LEND-INV03-T\n"
		if none.code != 1 || none.stdout != want {
			t.Fatalf("with no attestation, assure ledger exited %d with:\n%s%s\nwant:\n%s", none.code, none.stdout, none.stderr, want)
		}

		// A commit whose tree has no spec gives no ledger.
		bare := h.commit(t, "", map[string]string{"data/invariant_test.jsonl": "", "lathe.config.json": "", ".github/workflows/proof-run.yml": "", assure.ProofRunFile: "", "evidence/ledger.txt": ""})
		if result := h.ledger(t, attestations, "", []string{"--commit", bare}); result.code != 2 || result.stdout != "" || !strings.Contains(result.stderr, "no ledger") {
			t.Fatalf("a commit with no spec: assure ledger exited %d with:\n%s%s", result.code, result.stdout, result.stderr)
		}
		for name, flags := range map[string][]string{
			"a short commit":  {"--commit", h.at(5)[:12]},
			"no commit":       {},
			"a depth of zero": {"--commit", h.at(5), "--depth", "0"},
		} {
			if result := h.ledger(t, attestations, "", flags); result.code != 2 || result.stdout != "" {
				t.Fatalf("%s: assure ledger exited %d with:\n%s%s", name, result.code, result.stdout, result.stderr)
			}
		}
	})

	t.Run("ST-ASR-UC04-R02-T01 Each test record shows its state, its result at the commit and the newest commit a verified proof run passed it at, within the depth", func(t *testing.T) {
		h := newLedgerHistory(t)
		attestations := h.keep(t, "history", historyStatements(h)...)
		fetches := filepath.Join(h.runnerTemp, "fetches")
		result := h.ledger(t, attestations, fetches, []string{"--commit", h.at(5)})
		if result.code != 1 || result.stdout != historyLedger(h) {
			t.Fatalf("assure ledger exited %d with:\n%s%s\nwant:\n%s", result.code, result.stdout, result.stderr, historyLedger(h))
		}
		// The walk asks for each commit once, newest first, passes over commit 4, which has no attestation, and
		// stops at commit 2, where every automated test record has passed.
		if got, want := strings.Join(fetched(t, fetches), " "), strings.Join([]string{h.at(5), h.at(4), h.at(3), h.at(2)}, " "); got != want {
			t.Fatalf("the store was asked for\n%s\nwant\n%s", got, want)
		}
		again := h.ledger(t, attestations, "", []string{"--commit", h.at(5)})
		if again.code != result.code || again.stdout != result.stdout {
			t.Fatalf("the same inputs gave another ledger:\n%s", again.stdout)
		}

		os.Remove(fetches)
		shallow := h.ledger(t, attestations, fetches, []string{"--commit", h.at(5), "--depth", "2"})
		want := "proof ledger at " + h.at(5) + ": 3 test records, 2 commits walked of at most 2\n" +
			"FX-LEND-INV01-T (automated): passed, last passed at " + h.at(5) + "\n" +
			"FX-LEND-INV02-T (automated): failed, last passed not within 2 commits\n" +
			"FX-LEND-INV03-T (manual): none, last passed not within 2 commits\n" +
			"proof run at " + h.at(5) + ": release grade, " + signedAtMain + ", rekor:5\n" +
			"not passed (2): FX-LEND-INV02-T, FX-LEND-INV03-T\n"
		if shallow.code != 1 || shallow.stdout != want || len(fetched(t, fetches)) != 2 {
			t.Fatalf("with --depth 2, assure ledger exited %d, asked for %v, with:\n%s%s\nwant:\n%s", shallow.code, fetched(t, fetches), shallow.stdout, shallow.stderr, want)
		}

		// Every automated test record passed at the commit: it exits 0, and the manual one is listed as not
		// passed, as it is in another state.
		passed := h.keep(t, "passed", proofRun(h.at(4), mainRef, 4, "FX-LEND-INV01-T passed", "FX-LEND-INV02-T passed", "FX-LEND-INV03-T passed"))
		os.Remove(fetches)
		result = h.ledger(t, passed, fetches, []string{"--commit", h.at(4)})
		want = "proof ledger at " + h.at(4) + ": 3 test records, 1 commit walked of at most 200\n" +
			"FX-LEND-INV01-T (automated): passed, last passed at " + h.at(4) + "\n" +
			"FX-LEND-INV02-T (automated): passed, last passed at " + h.at(4) + "\n" +
			"FX-LEND-INV03-T (manual): passed, last passed at " + h.at(4) + "\n" +
			"proof run at " + h.at(4) + ": release grade, " + signedAtMain + ", rekor:4\n" +
			"not passed (1): FX-LEND-INV03-T\n"
		if result.code != 0 || result.stdout != want || len(fetched(t, fetches)) != 1 {
			t.Fatalf("with every automated test passed, assure ledger exited %d, asked for %v, with:\n%s%s\nwant:\n%s", result.code, fetched(t, fetches), result.stdout, result.stderr, want)
		}

		// A test record that never passed makes the walk go to the root commit.
		failing := h.keep(t, "failing", proofRun(h.at(5), mainRef, 5, "FX-LEND-INV01-T passed", "FX-LEND-INV02-T failed"))
		os.Remove(fetches)
		result = h.ledger(t, failing, fetches, []string{"--commit", h.at(5)})
		if result.code != 1 || !strings.Contains(result.stdout, ": 3 test records, 5 commits walked of at most 200\n") ||
			!strings.Contains(result.stdout, "FX-LEND-INV02-T (automated): failed, last passed not within 5 commits\n") || len(fetched(t, fetches)) != 5 {
			t.Fatalf("with a test that never passed, assure ledger exited %d, asked for %v, with:\n%s%s", result.code, fetched(t, fetches), result.stdout, result.stderr)
		}
	})

	t.Run("ST-ASR-UC04-R03-T01 An attestation that fails verification gives no result at its commit, and the ledger says why", func(t *testing.T) {
		h := newLedgerHistory(t)
		statements := historyStatements(h)
		statements[0] = forged(statements[0])
		result := h.ledger(t, h.keep(t, "forged", statements...), "", []string{"--commit", h.at(5)})
		want := "proof ledger at " + h.at(5) + ": 3 test records, 4 commits walked of at most 200\n" +
			"FX-LEND-INV01-T (automated): none, last passed at " + h.at(2) + "\n" +
			"FX-LEND-INV02-T (automated): none, last passed at " + h.at(2) + "\n" +
			"FX-LEND-INV03-T (manual): none, last passed not within 4 commits\n" +
			"proof run at " + h.at(5) + ": none verified, as each attestation there is refused\n" +
			"refused at " + h.at(5) + ": an attestation fails its signature check: the signature does not verify\n" +
			"refused at " + h.at(3) + ": an attestation fails its signature check: the signature does not verify\n" +
			"not passed (3): FX-LEND-INV01-T, FX-LEND-INV02-T, FX-LEND-INV03-T\n"
		if result.code != 1 || result.stdout != want {
			t.Fatalf("with a forged attestation at the commit, assure ledger exited %d with:\n%s%s\nwant:\n%s", result.code, result.stdout, result.stderr, want)
		}

		// A proof run signed at a pull request's ref counts at the commit only for that change, with its base
		// named, and only while the workflows folder is the base's.
		change := h.commit(t, h.at(5), map[string]string{"README.md": "Changed.\n"})
		workflow := h.commit(t, h.at(5), map[string]string{".github/workflows/proof-run.yml": "name: Proof run, changed\n"})
		own := h.keep(t, "own",
			proofRun(change, pullRef, 6, "FX-LEND-INV01-T passed", "FX-LEND-INV02-T passed"),
			proofRun(workflow, pullRef, 7, "FX-LEND-INV01-T passed", "FX-LEND-INV02-T passed"))
		refused := h.ledger(t, own, "", []string{"--commit", change, "--depth", "1"}, "GITHUB_REF="+pullRef)
		if refused.code != 1 || !strings.Contains(refused.stdout, "FX-LEND-INV01-T (automated): none, last passed not within 1 commit\n") ||
			!strings.Contains(refused.stdout, "refused at "+change+": the attestation at rekor:6 is refused: it is signed by "+proofRunWorkflow+" at "+pullRef+
				", not at "+mainRef+", and only release-grade evidence counts here\n") {
			t.Fatalf("a change-grade proof run with no base: assure ledger exited %d with:\n%s%s", refused.code, refused.stdout, refused.stderr)
		}
		accepted := h.ledger(t, own, "", []string{"--commit", change, "--depth", "1", "--base", h.at(5)}, "GITHUB_REF="+pullRef)
		if accepted.code != 0 || !strings.Contains(accepted.stdout, "proof run at "+change+": change grade, signed by "+proofRunWorkflow+"@"+pullRef) {
			t.Fatalf("a change-grade proof run with its base: assure ledger exited %d with:\n%s%s", accepted.code, accepted.stdout, accepted.stderr)
		}
		altered := h.ledger(t, own, "", []string{"--commit", workflow, "--depth", "1", "--base", h.at(5)}, "GITHUB_REF="+pullRef)
		if altered.code != 1 || !strings.Contains(altered.stdout, "proof run at "+workflow+": none verified, as each attestation there is refused\n") ||
			!strings.Contains(altered.stdout, "the change alters the workflows folder: .github/workflows/proof-run.yml\n") {
			t.Fatalf("a change-grade proof run of a change that alters a workflow: assure ledger exited %d with:\n%s%s", altered.code, altered.stdout, altered.stderr)
		}
	})
}
