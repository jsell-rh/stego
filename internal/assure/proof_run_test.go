package assure

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	fixtureCommit = "0123456789abcdef0123456789abcdef01234567"
	otherCommit   = "fedcba9876543210fedcba9876543210fedcba98"
	trustedRef    = "https://github.com/jsell-rh/stego/.github/workflows/proof-run.yml"
)

// keptStore is an attestation store that keeps each proof run handed to it, for the tests to read.
type keptStore struct {
	runs []ProofRun
}

func (s *keptStore) HandOver(run ProofRun) (string, error) {
	s.runs = append(s.runs, run)
	return "sha256:" + strings.Repeat("0", 64), nil
}

// fixtureSpec is a spec with three automated test records and one that is not automated yet.
func fixtureSpec() Spec {
	return Spec{Digest: "sha256:" + strings.Repeat("a", 64), Tests: map[string]TestRecord{
		"ST-FIX-INV01-T":      {ID: "ST-FIX-INV01-T", File: "internal/fix/fix_test.go", Automated: true},
		"ST-FIX-SP01-T":       {ID: "ST-FIX-SP01-T", File: "internal/fix/fix_test.go", Automated: true},
		"ST-FIX-UC01-R01-T01": {ID: "ST-FIX-UC01-R01-T01", File: "scripts/test_fix.py", Automated: true},
		"ST-FIX-SP02-T":       {ID: "ST-FIX-SP02-T", File: "internal/fix/fix_test.go"},
	}}
}

// goRun is go test -json output in which each named test passed, or got the result given after a colon.
func goRun(tests ...string) string {
	var out strings.Builder
	out.WriteString(`{"Time":"2026-10-29T14:02:11Z","Action":"start","Package":"example.com/fix"}` + "\n")
	for _, test := range tests {
		name, action, _ := strings.Cut(test, ":")
		if action == "" {
			action = "pass"
		}
		out.WriteString(`{"Time":"2026-10-29T14:02:12Z","Action":"run","Package":"example.com/fix","Test":"TestFix/` + name + `"}` + "\n")
		out.WriteString(`{"Time":"2026-10-29T14:02:13Z","Action":"` + action + `","Package":"example.com/fix","Test":"TestFix/` + name + `","Elapsed":0.5}` + "\n")
	}
	return out.String()
}

// pythonRun is a Python document in which each named subtest got its result, given after a colon.
func pythonRun(tests ...string) string {
	var entries []string
	for _, test := range tests {
		name, result, _ := strings.Cut(test, ":")
		entries = append(entries, `{"test": "test_fix.Fix.test_cases [`+name+`]", "name": "[`+name+`]", "result": "`+result+`", "elapsed": null, "detail": ""}`)
	}
	return `{"format": 1, "started": "2026-10-29T14:03:00Z", "finished": "2026-10-29T14:03:01Z", "tests": [` + strings.Join(entries, ",") + `]}`
}

func fixtureRequest(goJSON, pythonJSON string) RecordRequest {
	return RecordRequest{
		Commit:       fixtureCommit,
		Tree:         Checkout{Head: fixtureCommit},
		Spec:         fixtureSpec(),
		GoTestJSON:   strings.NewReader(goJSON),
		PythonJSON:   strings.NewReader(pythonJSON),
		Provider:     "github-actions",
		RunReference: "github:35950507614/1",
		Signer:       Signer{Issuer: GitHubIssuer, Subject: trustedRef + "@refs/heads/main"},
		Trusted:      TrustedSigner{Issuer: GitHubIssuer, Workflow: trustedRef},
	}
}

// refusedBy checks that err is a refusal of the rule, and that the store got nothing.
func refusedBy(t *testing.T, err error, rule string, store *keptStore) *Refusal {
	t.Helper()
	var refusal *Refusal
	if !errors.As(err, &refusal) || refusal.Rule != rule {
		t.Fatalf("got %v, want a refusal by %s", err, rule)
	}
	if len(store.runs) != 0 {
		t.Fatalf("the store got %d proof runs from a refused run", len(store.runs))
	}
	return refusal
}

func TestProofRunInvariants(t *testing.T) {
	everyTest := goRun("ST-FIX-INV01-T_holds", "ST-FIX-SP01-T_holds")
	pythonPass := pythonRun("ST-FIX-UC01-R01-T01 holds:passed")

	t.Run("ST-ASR-INV01-T A proof run is only at one full commit whose tree has no uncommitted change", func(t *testing.T) {
		store := &keptStore{}
		run, _, err := RecordProofRun(fixtureRequest(everyTest, pythonPass), store)
		if err != nil {
			t.Fatalf("a clean tree at the full commit is refused: %v", err)
		}
		if run.Commit != fixtureCommit || len(store.runs) != 1 || store.runs[0].Commit != fixtureCommit {
			t.Fatalf("the proof run is at %q, want %s", run.Commit, fixtureCommit)
		}
		for name, change := range map[string]func(*RecordRequest){
			"a short commit":                func(r *RecordRequest) { r.Commit, r.Tree.Head = fixtureCommit[:12], fixtureCommit[:12] },
			"a branch":                      func(r *RecordRequest) { r.Commit, r.Tree.Head = "main", "main" },
			"an upper-case commit":          func(r *RecordRequest) { r.Commit = strings.ToUpper(fixtureCommit); r.Tree.Head = r.Commit },
			"a tree at another commit":      func(r *RecordRequest) { r.Tree.Head = otherCommit },
			"a changed file in the tree":    func(r *RecordRequest) { r.Tree.Changes = []string{" M internal/fix/fix.go"} },
			"an untracked file in the tree": func(r *RecordRequest) { r.Tree.Changes = []string{"?? internal/fix/new.go"} },
		} {
			request := fixtureRequest(everyTest, pythonPass)
			change(&request)
			store := &keptStore{}
			_, _, err := RecordProofRun(request, store)
			if err == nil {
				t.Fatalf("%s is accepted", name)
			}
			refusedBy(t, err, "atOneCleanCommit", store)
		}

		t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
		t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
		clean := gitRepository(t)
		tree, err := ReadCheckout(clean)
		if err != nil {
			t.Fatal(err)
		}
		if head := strings.TrimSpace(gitIn(t, clean, "rev-parse", "HEAD")); tree.Head != head || len(tree.Changes) != 0 {
			t.Fatalf("a clean tree at %s reads as %+v", head, tree)
		}
		globalExcludes := filepath.Join(t.TempDir(), "excludes")
		globalConfig := filepath.Join(t.TempDir(), "gitconfig")
		writeTestFile(t, globalExcludes, "*.local\n")
		writeTestFile(t, globalConfig, "[core]\n\texcludesFile = "+globalExcludes+"\n")
		for name, hide := range map[string]func(root string){
			"a change hidden by skip-worktree": func(root string) {
				gitIn(t, root, "update-index", "--skip-worktree", "internal/fix/fix.go")
				writeTestFile(t, filepath.Join(root, "internal", "fix", "fix.go"), "package fix\n\nvar changed = true\n")
			},
			"a spec change hidden by assume-unchanged": func(root string) {
				gitIn(t, root, "update-index", "--assume-unchanged", "data/invariant_test.jsonl")
				writeTestFile(t, filepath.Join(root, "data", "invariant_test.jsonl"), `{"id":"ST-FIX-INV01-T","state":"automated","file":"internal/fix/fix_test.go"}`+"\n"+`{"id":"ST-FIX-INV09-T","state":"automated","file":"internal/fix/fix_test.go"}`+"\n")
			},
			"an untracked file hidden by info/exclude": func(root string) {
				writeTestFile(t, filepath.Join(root, ".git", "info", "exclude"), "# patterns\n*.local\n")
				writeTestFile(t, filepath.Join(root, "internal", "fix", "extra.local"), "x\n")
			},
			"an untracked file hidden by the user's excludes file": func(root string) {
				t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
				writeTestFile(t, filepath.Join(root, "internal", "fix", "extra.local"), "x\n")
			},
			"another commit's tree shown by a replace ref": func(root string) {
				head := strings.TrimSpace(gitIn(t, root, "rev-parse", "HEAD"))
				branch := strings.TrimSpace(gitIn(t, root, "branch", "--show-current"))
				gitIn(t, root, "checkout", "-q", "-b", "other")
				writeTestFile(t, filepath.Join(root, "data", "invariant_test.jsonl"), `{"id":"ST-FIX-INV01-T","state":"automated","file":"internal/fix/fix_test.go"}`+"\n"+`{"id":"ST-FIX-INV09-T","state":"automated","file":"internal/fix/fix_test.go"}`+"\n")
				gitIn(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-a", "-m", "other")
				other := strings.TrimSpace(gitIn(t, root, "rev-parse", "HEAD"))
				gitIn(t, root, "checkout", "-q", branch)
				gitIn(t, root, "replace", head, other)
				gitIn(t, root, "reset", "-q", "--hard")
			},
			"a same-size change hidden by minimal stat checks": func(root string) {
				gitIn(t, root, "config", "core.checkStat", "minimal")
				gitIn(t, root, "config", "core.trustctime", "false")
				file := filepath.Join(root, "internal", "fix", "fix.go")
				old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
				if err := os.Chtimes(file, old, old); err != nil {
					t.Fatal(err)
				}
				gitIn(t, root, "update-index", "--refresh")
				writeTestFile(t, file, "package fiz\n")
				if err := os.Chtimes(file, old, old); err != nil {
					t.Fatal(err)
				}
			},
			"a new file hidden by an untracked .gitignore that ignores itself": func(root string) {
				writeTestFile(t, filepath.Join(root, "internal", "fix", ".gitignore"), "*\n")
				writeTestFile(t, filepath.Join(root, "internal", "fix", "new.go"), "package fix\n")
			},
		} {
			t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
			root := gitRepository(t)
			hide(root)
			if status := gitIn(t, root, "-c", "core.excludesFile="+globalExcludes, "status", "--porcelain"); status != "" {
				t.Fatalf("%s: git status shows %q, so the case hides nothing", name, status)
			}
			tree, err := ReadCheckout(root)
			if err != nil {
				t.Fatal(err)
			}
			request := fixtureRequest(everyTest, pythonPass)
			request.Commit, request.Tree = tree.Head, tree
			store := &keptStore{}
			_, _, err = RecordProofRun(request, store)
			if err == nil {
				t.Fatalf("%s is accepted", name)
			}
			refusedBy(t, err, "atOneCleanCommit", store)
			if name == "a spec change hidden by assume-unchanged" || name == "another commit's tree shown by a replace ref" {
				spec, err := ReadCommittedSpec(root, tree.Head)
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := spec.Tests["ST-FIX-INV09-T"]; ok || len(spec.Tests) != 1 {
					t.Fatalf("the spec is read from the working tree, not from the commit: %v", spec.Tests)
				}
			}
		}
	})

	t.Run("ST-ASR-INV02-T Every automated test record has an outcome, and a test that did not run is failed with did not run", func(t *testing.T) {
		store := &keptStore{}
		run, _, err := RecordProofRun(fixtureRequest(goRun("ST-FIX-SP01-T_holds:skip"), pythonRun()), store)
		if err != nil {
			t.Fatal(err)
		}
		want := []Outcome{
			{TestRecord: "ST-FIX-INV01-T", Result: Failed, File: "internal/fix/fix_test.go", Detail: DidNotRun},
			{TestRecord: "ST-FIX-SP01-T", Result: Skipped, File: "internal/fix/fix_test.go", Duration: 0.5, Timed: true},
			{TestRecord: "ST-FIX-UC01-R01-T01", Result: Failed, File: "scripts/test_fix.py", Detail: DidNotRun},
		}
		sameOutcomes(t, run.Outcomes, want)
	})

	t.Run("ST-ASR-INV03-T A proof run with an outcome for an unknown ID is refused and names each such ID", func(t *testing.T) {
		store := &keptStore{}
		goJSON := goRun("ST-FIX-INV01-T_holds", "ST-FIX-SP01-T_holds", "ST-FIX-SP09-T_is_not_a_record", "ST-FIX-INV09-T_is_not_one_either:fail")
		_, _, err := RecordProofRun(fixtureRequest(goJSON, pythonPass), store)
		refusal := refusedBy(t, err, "outcomesNameKnownRecords", store)
		if !strings.Contains(refusal.Reason, "ST-FIX-INV09-T, ST-FIX-SP09-T") {
			t.Fatalf("the refusal does not name each unknown ID in order: %s", refusal.Reason)
		}

		store = &keptStore{}
		run, _, err := RecordProofRun(fixtureRequest(goRun("ST-FIX-INV01-T_holds", "ST-FIX-SP01-T_holds", "ST-FIX-SP02-T_holds"), pythonPass), store)
		if err != nil {
			t.Fatalf("an outcome for a test record that is not automated yet is refused: %v", err)
		}
		if len(run.Outcomes) != 4 {
			t.Fatalf("got %d outcomes, want 4 with the one not automated yet", len(run.Outcomes))
		}
	})

	t.Run("ST-ASR-INV04-T A proof run is attested only by the provider's trusted signer with its commit as subject", func(t *testing.T) {
		store := &keptStore{}
		run, attested, err := RecordProofRun(fixtureRequest(everyTest, pythonPass), store)
		if err != nil {
			t.Fatal(err)
		}
		want := Attestation{PredicateType: ProofRunPredicateType, Subject: fixtureCommit, Signer: Signer{Issuer: GitHubIssuer, Subject: trustedRef + "@refs/heads/main"}}
		if run.Attestation != want || attested.Attestation != want {
			t.Fatalf("attestation = %+v, event = %+v, want %+v", run.Attestation, attested.Attestation, want)
		}
		trusted := TrustedSigner{Issuer: GitHubIssuer, Workflow: trustedRef}
		if err := attestedByTrustedSigner(run, want, trusted); err != nil {
			t.Fatalf("the trusted workflow at main is refused: %v", err)
		}
		for name, attestation := range map[string]Attestation{
			"another commit as subject":             {PredicateType: ProofRunPredicateType, Subject: otherCommit, Signer: want.Signer},
			"another predicate type":                {PredicateType: "https://slsa.dev/provenance/v1", Subject: fixtureCommit, Signer: want.Signer},
			"another workflow":                      {PredicateType: ProofRunPredicateType, Subject: fixtureCommit, Signer: Signer{Issuer: GitHubIssuer, Subject: "https://github.com/jsell-rh/stego/.github/workflows/checks.yml@refs/heads/main"}},
			"a workflow whose name starts the same": {PredicateType: ProofRunPredicateType, Subject: fixtureCommit, Signer: Signer{Issuer: GitHubIssuer, Subject: trustedRef + ".bak@refs/heads/main"}},
			"the workflow in a fork":                {PredicateType: ProofRunPredicateType, Subject: fixtureCommit, Signer: Signer{Issuer: GitHubIssuer, Subject: "https://github.com/someone/stego/.github/workflows/proof-run.yml@refs/heads/main"}},
			"another issuer":                        {PredicateType: ProofRunPredicateType, Subject: fixtureCommit, Signer: Signer{Issuer: "https://issuer.example", Subject: want.Signer.Subject}},
		} {
			err := attestedByTrustedSigner(run, attestation, trusted)
			var refusal *Refusal
			if !errors.As(err, &refusal) || refusal.Rule != "attestedByTrustedSigner" {
				t.Fatalf("%s: got %v, want a refusal by attestedByTrustedSigner", name, err)
			}
		}

		request := fixtureRequest(everyTest, pythonPass)
		request.Signer.Subject = "https://github.com/jsell-rh/stego/.github/workflows/checks.yml@refs/heads/main"
		store = &keptStore{}
		_, _, err = RecordProofRun(request, store)
		refusedBy(t, err, "attestedByTrustedSigner", store)
	})

	t.Run("ST-ASR-INV06-T A test record gets one outcome: failed if any run failed, else skipped if any was skipped, else passed", func(t *testing.T) {
		goJSON := goRun("ST-FIX-INV01-T_first", "ST-FIX-INV01-T_second:fail", "ST-FIX-SP01-T_first", "ST-FIX-SP01-T_second:skip", "ST-FIX-SP02-T_first:skip", "ST-FIX-SP02-T_second:fail")
		pythonJSON := pythonRun("ST-FIX-UC01-R01-T01 first:passed", "ST-FIX-UC01-R01-T01 second:passed", "ST-FIX-INV01-T third:passed", "ST-FIX-SP01-T third:passed")
		store := &keptStore{}
		run, _, err := RecordProofRun(fixtureRequest(goJSON, pythonJSON), store)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]Result{
			"ST-FIX-INV01-T":      Failed,
			"ST-FIX-SP01-T":       Skipped,
			"ST-FIX-SP02-T":       Failed,
			"ST-FIX-UC01-R01-T01": Passed,
		}
		if len(run.Outcomes) != len(want) {
			t.Fatalf("got %d outcomes, want one per test record: %+v", len(run.Outcomes), run.Outcomes)
		}
		for _, outcome := range run.Outcomes {
			if outcome.Result != want[outcome.TestRecord] {
				t.Fatalf("%s is %s, want %s", outcome.TestRecord, outcome.Result, want[outcome.TestRecord])
			}
		}
	})
}

func writeTestFile(t *testing.T, file, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
