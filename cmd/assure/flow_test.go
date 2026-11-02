package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/assure"
)

func TestRecordFlow(t *testing.T) {
	t.Run("ST-ASR-UC01-FL-T assure record writes the same predicate for the same run, for the attestation action to sign with the commit as subject", func(t *testing.T) {
		f := newFixture(t, goFixture, pythonFixture)
		result := f.record(t, f.commit, f.predicate)
		if result.code != 0 {
			t.Fatalf("assure record exited %d:\n%s", result.code, result.stderr)
		}
		if !strings.Contains(result.stdout, "3 outcomes, 2 passed, 1 failed, 0 skipped") {
			t.Fatalf("assure record does not report the outcomes:\n%s", result.stdout)
		}
		want := `{
  "commit": "` + f.commit + `",
  "provider": "github-actions",
  "runReference": "github:35950507614/1",
  "startedAt": "2026-10-29T14:02:11.5Z",
  "finishedAt": "2026-10-29T14:03:05.25Z",
  "specDigest": "` + specDigest(t, f.repository) + `",
  "outcomes": [
    {
      "testRecord": "ST-FIX-INV01-T",
      "result": "passed",
      "file": "internal/fix/fix_test.go",
      "duration": 0.42
    },
    {
      "testRecord": "ST-FIX-INV02-T",
      "result": "failed",
      "file": "internal/fix/fix_test.go",
      "detail": "did not run"
    },
    {
      "testRecord": "ST-FIX-UC01-R01-T01",
      "result": "passed",
      "file": "scripts/test_fix.py"
    }
  ]
}
`
		first, err := os.ReadFile(f.predicate)
		if err != nil {
			t.Fatal(err)
		}
		if string(first) != want {
			t.Fatalf("predicate:\n%s\nwant:\n%s", first, want)
		}
		sum := sha256.Sum256([]byte(f.commit))
		name, digest := workflowSubject(t, f.commit)
		if name != "git+https://github.com/jsell-rh/stego@"+f.commit || digest != "sha256:"+hex.EncodeToString(sum[:]) {
			t.Fatalf("the attest job names the subject %s (%s), not the commit %s", name, digest, f.commit)
		}

		again := filepath.Join(f.runnerTemp, "proof-run-again.json")
		if result := f.record(t, f.commit, again); result.code != 0 {
			t.Fatalf("assure record exited %d the second time:\n%s", result.code, result.stderr)
		}
		if second, _ := os.ReadFile(again); !bytes.Equal(first, second) {
			t.Fatalf("the same run gave another predicate:\n%s", second)
		}

		unknown := newFixture(t, goFixture+`{"Time":"2026-10-29T14:02:15Z","Action":"run","Package":"example.com/fix","Test":"TestOther/ST-FIX-INV09-T_names_no_record"}
{"Time":"2026-10-29T14:02:15Z","Action":"pass","Package":"example.com/fix","Test":"TestOther/ST-FIX-INV09-T_names_no_record","Elapsed":0}
`, pythonFixture)
		refused := unknown.record(t, unknown.commit, unknown.predicate)
		unknown.refusedWithNothingWritten(t, refused, unknown.predicate, "ST-FIX-INV09-T")
	})
}

// specDigest is the digest of the fixture's data files, as sha256sum data/*.jsonl | sha256sum gives it.
func specDigest(t *testing.T, repository string) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(repository, "data", "*.jsonl"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no data files: %v", err)
	}
	var lines strings.Builder
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		lines.WriteString(hex.EncodeToString(sum[:]) + "  data/" + filepath.Base(file) + "\n")
	}
	sum := sha256.Sum256([]byte(lines.String()))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestGateFlow(t *testing.T) {
	t.Run("ST-ASR-UC02-FL-T assure gate change measures both commits, verifies the proof run at the change, compares the code faults of both, and prints the decision with every cause", func(t *testing.T) {
		g := newGateRepository(t)
		change := g.commit(t, g.base, map[string]string{"data/invariant.jsonl": gateSpec["data/invariant.jsonl"] + newInvariant})
		proof := g.keep(t, "proof", proofRun(change, pullRef, 21, "FX-LEND-INV01-T passed", "FX-LEND-INV02-T passed"))
		summary := filepath.Join(g.runnerTemp, "step-summary.md")
		result := g.gate(t, change, g.base, proof, "GITHUB_STEP_SUMMARY="+summary)
		want := "change gate on " + change + ": passed\n" +
			"base head: " + g.base + "\n" +
			"coverage at " + change + ": 5 obligations: 5 designed (100.0%), 2 built (40.0%), 2 proved (40.0%)\n" +
			"coverage at " + g.base + ": 4 obligations: 4 designed (100.0%), 2 built (50.0%), 2 proved (50.0%)\n" +
			"proof run: https://github.com/jsell-rh/stego/assure/proof-run/v1 about " + change +
			", signed by https://github.com/jsell-rh/stego/.github/workflows/proof-run.yml@refs/pull/7/merge (issuer https://token.actions.githubusercontent.com), rekor:21\n" +
			"code faults (1):\n" +
			"internal/lend/rules.go (ST-STD-CODE-R02, present at base): FX-LEND-INV02-RL names lend.dueAfterStart, which the file does not declare\n" +
			"reasons: none\n"
		if result.code != 0 || result.stdout != want {
			t.Fatalf("assure gate change exited %d with:\n%s%s\nwant:\n%s", result.code, result.stdout, result.stderr, want)
		}
		if data, err := os.ReadFile(summary); err != nil || string(data) != "```text\n"+want+"```\n" {
			t.Fatalf("the job's step summary does not hold the decision (%v):\n%s", err, data)
		}

		fell := g.commit(t, g.base, map[string]string{"internal/lend/rules.go": ""})
		failed := g.keep(t, "failed", proofRun(fell, pullRef, 22, "FX-LEND-INV01-T passed", "FX-LEND-INV02-T failed"))
		result = g.gate(t, fell, g.base, failed)
		if result.code == 0 || !strings.HasSuffix(result.stdout, "reasons (3):\nFX-LEND-INV01 fell from built\nFX-LEND-INV02 fell from built\nFX-LEND-INV02-T failed\n") {
			t.Fatalf("a change under which a share fell and a proof failed exits %d with:\n%s%s", result.code, result.stdout, result.stderr)
		}

		workflow := g.commit(t, g.base, map[string]string{".github/workflows/checks.yml": "name: Checks, by the change\n"})
		own := g.keep(t, "own", proofRun(workflow, pullRef, 23, "FX-LEND-INV01-T passed", "FX-LEND-INV02-T passed"))
		result = g.gate(t, workflow, g.base, own)
		if result.code == 0 || !strings.HasSuffix(result.stdout, "\n.github/workflows/checks.yml differs from the base head\n") ||
			!strings.Contains(result.stdout, "proof run: none verified\n") {
			t.Fatalf("a change that alters a workflow file with a proof run signed at its own ref exits %d with:\n%s%s", result.code, result.stdout, result.stderr)
		}

		fault := g.commit(t, g.base, map[string]string{"internal/lend/new.go": "package lend\n\nconst added = 2\n"})
		result = g.gate(t, fault, g.base, g.keep(t, "fault", proofRun(fault, pullRef, 24, "FX-LEND-INV01-T passed", "FX-LEND-INV02-T passed")))
		if result.code == 0 || !strings.HasSuffix(result.stdout, "reasons (1):\n"+
			"internal/lend/new.go (ST-STD-CODE-R05, internal/lend/new.go): no technical record names the file, directly or through a folder\n") ||
			!strings.Contains(result.stdout, " (ST-STD-CODE-R02, present at base): ") {
			t.Fatalf("a change that adds a code-standard fault exits %d with:\n%s%s", result.code, result.stdout, result.stderr)
		}
	})
}

func TestReleaseFlow(t *testing.T) {
	t.Run("ST-ASR-UC03-FL-T assure gate release passes a fully proved commit and writes the decision predicate, and refuses every other commit with each cause named", func(t *testing.T) {
		g := newGateRepository(t)
		full := g.commit(t, g.base, fullyProved)
		proof := g.keep(t, "proof", proofRun(full, mainRef, 31, allPassed...))
		summary := filepath.Join(g.runnerTemp, "step-summary.md")
		predicate := g.releasePredicate(t)
		result := g.release(t, full, proof, predicate, "GITHUB_STEP_SUMMARY="+summary)
		decision := "release gate on " + full + ": passed\n" +
			"coverage at " + full + ": 4 obligations: 4 designed (100.0%), 4 built (100.0%), 4 proved (100.0%)\n" +
			"proof run: https://github.com/jsell-rh/stego/assure/proof-run/v1 about " + full +
			", signed by https://github.com/jsell-rh/stego/.github/workflows/proof-run.yml@refs/heads/main (issuer https://token.actions.githubusercontent.com), rekor:31\n" +
			"attestation: https://github.com/jsell-rh/stego/assure/release-decision/v1 about " + full +
			", for https://github.com/jsell-rh/stego/.github/workflows/proof-run.yml@refs/heads/main (issuer https://token.actions.githubusercontent.com) to sign\n" +
			"code faults: none\n" +
			"reasons: none\n"
		want := `{
  "commit": "` + full + `",
  "kind": "release",
  "coverage": {
    "total": 4,
    "designed": 4,
    "built": 4,
    "proved": 4
  },
  "passed": true,
  "reasons": [],
  "codeFaults": [],
  "proofRun": {
    "predicateType": "https://github.com/jsell-rh/stego/assure/proof-run/v1",
    "subject": "` + full + `",
    "issuer": "https://token.actions.githubusercontent.com",
    "signer": "https://github.com/jsell-rh/stego/.github/workflows/proof-run.yml@refs/heads/main",
    "logEntry": "rekor:31"
  }
}
`
		sum := sha256.Sum256([]byte(want))
		predicateLine := "decision predicate " + predicate + " (sha256:" + hex.EncodeToString(sum[:]) + "), type " +
			"https://github.com/jsell-rh/stego/assure/release-decision/v1, for https://github.com/jsell-rh/stego/.github/workflows/proof-run.yml@refs/heads/main to sign\n"
		if result.code != 0 || result.stdout != decision+predicateLine {
			t.Fatalf("assure gate release exited %d with:\n%s%s\nwant:\n%s%s", result.code, result.stdout, result.stderr, decision, predicateLine)
		}
		first, err := os.ReadFile(predicate)
		if err != nil || string(first) != want {
			t.Fatalf("the decision predicate (%v):\n%s\nwant:\n%s", err, first, want)
		}
		if data, err := os.ReadFile(summary); err != nil || string(data) != "```text\n"+decision+"```\n" {
			t.Fatalf("the job's step summary does not hold the decision (%v):\n%s", err, data)
		}
		name, digest, err := runJobSubjectStep(t, "attest-release", full)
		if err != nil || name != "git+https://github.com/jsell-rh/stego@"+full || digest != assure.SubjectDigest(full) {
			t.Fatalf("the attest-release job names the subject %s (%s), %v, not the release commit", name, digest, err)
		}
		again := filepath.Join(g.runnerTemp, "decision", "again.json")
		if result := g.release(t, full, proof, again); result.code != 0 {
			t.Fatalf("assure gate release exited %d the second time:\n%s", result.code, result.stderr)
		}
		if second, _ := os.ReadFile(again); !bytes.Equal(first, second) {
			t.Fatalf("the same commit and proof run gave another predicate:\n%s", second)
		}

		unproved := g.commit(t, g.base, with(fullyProved, map[string]string{"data/invariant_test.jsonl": invariantTest01 + invariantTest02}))
		refusedRelease(t, "a commit with an obligation not proved",
			g.release(t, unproved, g.keep(t, "unproved", proofRun(unproved, mainRef, 32, allPassed...)), predicate+".unproved"),
			predicate+".unproved", "FX-LEND-INV03 is not proved")

		failed := g.keep(t, "failed", proofRun(full, mainRef, 33, "FX-LEND-INV01-T passed", "FX-LEND-INV02-T failed", "FX-LEND-INV03-T passed", "FX-VO01-C01-T passed"))
		refusedRelease(t, "a proof run with a failed test", g.release(t, full, failed, predicate+".failed"), predicate+".failed", "FX-LEND-INV02-T failed")

		change := g.keep(t, "change", proofRun(full, pullRef, 34, allPassed...))
		refusedRelease(t, "a change-grade proof run", g.release(t, full, change, predicate+".change"), predicate+".change",
			noVerifiedProofRunAt(full)+": the attestation at rekor:34 is refused: it is signed by "+proofRunWorkflow+" at "+pullRef+
				", not at "+mainRef+", and only release-grade evidence counts here")

		fault := g.commit(t, g.base, with(fullyProved, map[string]string{"internal/lend/extra.go": "package lend\n\nconst extra = 1\n"}))
		refusedRelease(t, "a tree with a code-standard fault",
			g.release(t, fault, g.keep(t, "fault", proofRun(fault, mainRef, 35, allPassed...)), predicate+".fault"), predicate+".fault",
			"internal/lend/extra.go (ST-STD-CODE-R05, internal/lend/extra.go): no technical record names the file, directly or through a folder")
	})
}

func TestLedgerFlow(t *testing.T) {
	t.Run("ST-ASR-UC04-FL-T assure ledger fetches and verifies the proof runs of a commit and its history, prints each test record with its result and where it last passed, and shows a refused attestation as none with why", func(t *testing.T) {
		// The main path: commit 5 has a proof run with one test failed, commit 4 has none, commit 3 has one whose
		// signature does not verify, and commit 2 has one where every automated test passed.
		h := newLedgerHistory(t)
		fetches := filepath.Join(h.runnerTemp, "fetches")
		result := h.ledger(t, h.keep(t, "history", historyStatements(h)...), fetches, []string{"--commit", h.at(5)})
		if result.code != 1 || result.stdout != historyLedger(h) || result.stderr != "" {
			t.Fatalf("assure ledger exited %d with:\n%s%s\nwant:\n%s", result.code, result.stdout, result.stderr, historyLedger(h))
		}
		if got := fetched(t, fetches); strings.Join(got, " ") != strings.Join([]string{h.at(5), h.at(4), h.at(3), h.at(2)}, " ") {
			t.Fatalf("the store was asked for %v", got)
		}

		// The refused path: the attestation at the commit fails verification, so the commit has no result, and
		// the ledger says why.
		statements := historyStatements(h)
		statements[0] = forged(statements[0])
		result = h.ledger(t, h.keep(t, "forged", statements...), "", []string{"--commit", h.at(5)})
		if result.code != 1 || !strings.Contains(result.stdout, "FX-LEND-INV01-T (automated): none, last passed at "+h.at(2)+"\n") ||
			!strings.Contains(result.stdout, "proof run at "+h.at(5)+": none verified at release grade\n") ||
			!strings.Contains(result.stdout, "refused at "+h.at(5)+": an attestation fails its signature check: the signature does not verify\n") {
			t.Fatalf("with the attestation at the commit forged, assure ledger exited %d with:\n%s%s", result.code, result.stdout, result.stderr)
		}
	})
}
