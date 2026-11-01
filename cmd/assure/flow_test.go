package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordFlow(t *testing.T) {
	t.Run("ST-ASR-UC01-FL01-T assure record writes the same predicate for the same run, for the attestation action to sign with the commit as subject", func(t *testing.T) {
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
	t.Run("ST-ASR-UC02-FL01-T assure gate change measures both commits, verifies the proof run at the change, and prints the decision with every cause", func(t *testing.T) {
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
	})
}
