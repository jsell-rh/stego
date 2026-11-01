package assure

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// refusedByRule checks that err is a refusal of the rule.
func refusedByRule(t *testing.T, name string, err error, rule string) {
	t.Helper()
	var refusal *Refusal
	if !errors.As(err, &refusal) || refusal.Rule != rule {
		t.Fatalf("%s: got %v, want a refusal by %s", name, err, rule)
	}
}

func TestDecisionInvariants(t *testing.T) {
	commit := strings.Repeat("ab", 20)
	base := strings.Repeat("cd", 20)
	coverage := Coverage{Total: 4, Designed: 4, Built: 2, Proved: 1}

	t.Run("ST-ASR-INV07-T A change decision names the change's commit, the base head and both coverages, and a release decision names only its commit", func(t *testing.T) {
		change := Decision{Commit: commit, Kind: ChangeGate, BaseCommit: base, Coverage: coverage, BaseCoverage: &coverage, Passed: true}
		if err := namesBothCommits(change); err != nil {
			t.Fatalf("a change decision that names both commits is refused: %v", err)
		}
		release := Decision{Commit: commit, Kind: ReleaseGate, Coverage: coverage, Passed: true}
		if err := namesBothCommits(release); err != nil {
			t.Fatalf("a release decision that names its commit is refused: %v", err)
		}
		for name, decision := range map[string]Decision{
			"a change decision with no base head":         {Commit: commit, Kind: ChangeGate, Coverage: coverage, BaseCoverage: &coverage},
			"a change decision with a short base head":    {Commit: commit, Kind: ChangeGate, BaseCommit: base[:7], Coverage: coverage, BaseCoverage: &coverage},
			"a change decision with a branch as its base": {Commit: commit, Kind: ChangeGate, BaseCommit: "main", Coverage: coverage, BaseCoverage: &coverage},
			"a change decision with no base coverage":     {Commit: commit, Kind: ChangeGate, BaseCommit: base, Coverage: coverage},
			"a change decision with a short commit":       {Commit: commit[:12], Kind: ChangeGate, BaseCommit: base, Coverage: coverage, BaseCoverage: &coverage},
			"a release decision with a base head":         {Commit: commit, Kind: ReleaseGate, BaseCommit: base, Coverage: coverage},
			"a release decision with a base coverage":     {Commit: commit, Kind: ReleaseGate, Coverage: coverage, BaseCoverage: &coverage},
			"a decision of no gate kind":                  {Commit: commit, BaseCommit: base, Coverage: coverage, BaseCoverage: &coverage},
		} {
			refusedByRule(t, name, namesBothCommits(decision), "namesBothCommits")
		}
		text := change.Text()
		for _, line := range []string{"change gate on " + commit + ": passed\n", "base head: " + base + "\n",
			"coverage at " + commit + ": " + coverage.String() + "\n", "coverage at " + base + ": " + coverage.String() + "\n"} {
			if !strings.Contains(text, line) {
				t.Fatalf("the decision's text does not hold %q:\n%s", line, text)
			}
		}
	})

	t.Run("ST-ASR-INV08-T A refused decision names every obligation that fell and every test record that did not pass, and a passed decision names no reason", func(t *testing.T) {
		causes := []string{"ST-FIX-INV01 fell from built", "ST-FIX-SP01 fell from proved", "ST-FIX-INV01-T failed", "ST-FIX-SP01-T skipped"}
		refused := Decision{Commit: commit, Kind: ChangeGate, BaseCommit: base, Coverage: coverage, BaseCoverage: &coverage, Reasons: sortReasons(causes)}
		if err := namesEveryCause(refused, causes); err != nil {
			t.Fatalf("a refusal that names every cause is refused: %v", err)
		}
		if err := namesEveryCause(Decision{Commit: commit, Passed: true}, nil); err != nil {
			t.Fatalf("a passed decision with no reason is refused: %v", err)
		}
		for name, decision := range map[string]Decision{
			"a refusal that leaves out an obligation that fell": {Commit: commit, Reasons: sortReasons(causes[1:])},
			"a refusal that leaves out a test that failed":      {Commit: commit, Reasons: sortReasons(append(causes[:2:2], causes[3]))},
			"a refusal with no reason":                          {Commit: commit},
			"a passed decision with reasons":                    {Commit: commit, Passed: true, Reasons: sortReasons(causes)},
		} {
			err := namesEveryCause(decision, causes)
			refusedByRule(t, name, err, "namesEveryCause")
			if strings.HasPrefix(name, "a refusal that leaves out") && !strings.Contains(err.Error(), "ST-FIX-") {
				t.Fatalf("%s: the refusal does not name what is left out: %v", name, err)
			}
		}
		text := refused.Text()
		if !strings.HasSuffix(text, "reasons (4):\nST-FIX-INV01 fell from built\nST-FIX-SP01 fell from proved\nST-FIX-INV01-T failed\nST-FIX-SP01-T skipped\n") {
			t.Fatalf("the refusal does not list each cause on a line of its own:\n%s", text)
		}
	})

	t.Run("ST-ASR-INV10-T The same causes give the same reasons in the same fixed order, and reasons out of that order are refused", func(t *testing.T) {
		want := []string{
			"ST-FIX-INV01 fell from designed",
			"ST-FIX-INV01 fell from built",
			"ST-FIX-INV01 fell from proved",
			"ST-FIX-SP01 fell from built",
			"ST-FIX-SP02 is absent at the change (was designed, built at the base)",
			"ST-FIX-SP03 is not built",
			"ST-FIX-SP03 is not proved",
			noVerifiedProofRun + commit + ": the provider keeps no proof run attestation for it",
			"ST-FIX-INV01-T failed",
			"ST-FIX-INV02-T failed: did not run",
			"ST-FIX-SP01-T has no outcome in the proof run",
			"internal/fix/rules.go (ST-STD-CODE-R02, ST-FIX-INV01-RL): ST-FIX-INV01-RL names fix.ruleOne, which the file does not declare",
			"internal/fix/b.go (ST-STD-CODE-R05, internal/fix/b.go): no technical record names the file, directly or through a folder",
			"internal/fix/c.go (ST-STD-CODE-R05, internal/fix/c.go): no technical record names the file, directly or through a folder",
			".github/workflows/checks.yml differs from the base head",
			".github/workflows/proof-run.yml differs from the base head",
		}
		orders := [][]string{
			append([]string(nil), want...),
			{want[15], want[8], want[13], want[3], want[0], want[11], want[14], want[6], want[2], want[10], want[1], want[12], want[9], want[7], want[5], want[4]},
			{want[7], want[14], want[4], want[12], want[9], want[2], want[15], want[1], want[3], want[10], want[13], want[6], want[8], want[0], want[11], want[5], want[8], want[12]},
		}
		var texts []string
		for _, order := range orders {
			reasons := sortReasons(order)
			if !reflect.DeepEqual(reasons, want) {
				t.Fatalf("the causes %q give the reasons %q, want %q", order, reasons, want)
			}
			decision := Decision{Commit: commit, Kind: ChangeGate, BaseCommit: base, Coverage: coverage, BaseCoverage: &coverage, Reasons: reasons}
			if err := reasonsInFixedOrder(decision); err != nil {
				t.Fatalf("reasons in the fixed order are refused: %v", err)
			}
			texts = append(texts, decision.Text())
		}
		if texts[0] != texts[1] || texts[1] != texts[2] {
			t.Fatalf("the same causes give other decisions:\n%s\n%s\n%s", texts[0], texts[1], texts[2])
		}
		for name, reasons := range map[string][]string{
			"a test record before an obligation":        {want[8], want[0]},
			"a workflow file before a test record":      {want[14], want[8]},
			"built before designed":                     {want[1], want[0]},
			"obligations out of ID order":               {want[3], want[0]},
			"an absent obligation before another":       {want[4], want[3]},
			"proved before built at a release":          {want[6], want[5]},
			"a share not had before an obligation":      {want[5], want[4]},
			"a reason twice":                            {want[8], want[8]},
			"the missing proof run after a test":        {want[8], want[7]},
			"workflow files out of path order":          {want[15], want[14]},
			"test records out of ID order":              {want[9], want[8]},
			"a code fault before a test record":         {want[11], want[10]},
			"a code fault before an obligation":         {want[12], want[5]},
			"a workflow file before a code fault":       {want[14], want[13]},
			"code faults out of rule order":             {want[12], want[11]},
			"code faults of one rule out of key order":  {want[13], want[12]},
			"a code fault twice":                        {want[12], want[12]},
			"a code fault before the missing proof run": {want[11], want[7]},
		} {
			refusedByRule(t, name, reasonsInFixedOrder(Decision{Commit: commit, Reasons: reasons}), "reasonsInFixedOrder")
		}
	})
}
