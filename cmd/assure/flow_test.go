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
		wantOutputs := "subject-name=git+https://github.com/jsell-rh/stego@" + f.commit + "\n" +
			"subject-digest=sha256:" + hex.EncodeToString(sum[:]) + "\n" +
			"predicate-type=https://github.com/jsell-rh/stego/assure/proof-run/v1\n" +
			"predicate-path=" + f.predicate + "\n"
		if outputs, _ := os.ReadFile(f.outputs); string(outputs) != wantOutputs {
			t.Fatalf("step outputs for the attestation action:\n%s\nwant:\n%s", outputs, wantOutputs)
		}

		again := filepath.Join(f.runnerTemp, "proof-run-again.json")
		if result := f.record(t, f.commit, again); result.code != 0 {
			t.Fatalf("assure record exited %d the second time:\n%s", result.code, result.stderr)
		}
		if second, _ := os.ReadFile(again); !bytes.Equal(first, second) {
			t.Fatalf("the same run gave another predicate:\n%s", second)
		}
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
