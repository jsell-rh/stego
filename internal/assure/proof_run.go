package assure

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ProofRun is what one CI run of the test suites showed at one exact commit (ST-ASR-EN01): an outcome for every
// automated test record in the spec at the commit, and the attestation the provider's trusted signer makes of it.
type ProofRun struct {
	Commit       string
	Provider     string
	RunReference string
	StartedAt    time.Time
	FinishedAt   time.Time
	SpecDigest   string
	Attestation  Attestation
	Outcomes     []Outcome
}

// Outcome is what the test of one test record did in the run (ST-ASR-EN02). Duration is in seconds, and only
// when Timed is true. Detail tells why the test failed or was skipped.
type Outcome struct {
	TestRecord string
	Result     Result
	File       string
	Duration   float64
	Timed      bool
	Detail     string
}

// DidNotRun is the detail of the failed outcome of an automated test record whose test did not run.
const DidNotRun = "did not run"

// Checkout is the state of the tree the suites ran against: the commit checked out, and each change that is not
// committed, as git status names it.
type Checkout struct {
	Head    string
	Changes []string
}

// Refusal is why a rule of the proof run refused it. Rule is the rule's name in code, as its record names it.
type Refusal struct {
	Rule   string
	Reason string
}

func (r *Refusal) Error() string {
	return r.Reason
}

// atOneCleanCommit is the rule of ST-ASR-INV01: a proof run names exactly one full commit, and the suites ran
// against that commit's tree with no uncommitted change.
func atOneCleanCommit(commit string, tree Checkout) error {
	refuse := func(format string, args ...any) error {
		return &Refusal{Rule: "atOneCleanCommit", Reason: fmt.Sprintf(format, args...)}
	}
	if !fullCommit(commit) {
		return refuse("commit %q is not a full 40-character commit ID", commit)
	}
	if tree.Head != commit {
		return refuse("the tree is at commit %q, not at commit %s", tree.Head, commit)
	}
	if len(tree.Changes) > 0 {
		changes := append([]string(nil), tree.Changes...)
		sort.Strings(changes)
		return refuse("the tree at commit %s has uncommitted changes: %s", commit, strings.Join(changes, ", "))
	}
	return nil
}

// outcomeForEveryAutomatedTest is the rule of ST-ASR-INV02: the proof run holds an outcome for every test record
// whose state is automated in the spec. It adds a failed outcome with the detail "did not run" for each automated
// test record that has none, and returns the outcomes in test record ID order.
func outcomeForEveryAutomatedTest(outcomes []Outcome, spec Spec) []Outcome {
	have := make(map[string]bool, len(outcomes))
	all := append([]Outcome(nil), outcomes...)
	for _, outcome := range outcomes {
		have[outcome.TestRecord] = true
	}
	for _, id := range spec.automated() {
		if !have[id] {
			all = append(all, Outcome{TestRecord: id, Result: Failed, Detail: DidNotRun})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].TestRecord < all[j].TestRecord })
	return all
}

// outcomesNameKnownRecords is the rule of ST-ASR-INV03: every outcome names a test record that exists in the
// spec at the run's commit. It refuses the proof run and names each unknown ID, in order.
func outcomesNameKnownRecords(outcomes []Outcome, spec Spec) error {
	var unknown []string
	for _, outcome := range outcomes {
		if _, ok := spec.Tests[outcome.TestRecord]; !ok {
			unknown = append(unknown, outcome.TestRecord)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return &Refusal{
		Rule:   "outcomesNameKnownRecords",
		Reason: "a test names an ID that is not a test record in the spec: " + strings.Join(unknown, ", "),
	}
}

// attestedByTrustedSigner is the rule of ST-ASR-INV04: the proof run is attested by the trusted signer of the CI
// provider that ran it, with the run's commit as the attestation's subject and the proof run's predicate type.
func attestedByTrustedSigner(run ProofRun, attestation Attestation, trusted TrustedSigner) error {
	refuse := func(format string, args ...any) error {
		return &Refusal{Rule: "attestedByTrustedSigner", Reason: fmt.Sprintf(format, args...)}
	}
	if attestation.Subject != run.Commit {
		return refuse("the attestation is about commit %q, not about the run's commit %s", attestation.Subject, run.Commit)
	}
	if attestation.PredicateType != ProofRunPredicateType {
		return refuse("the attestation's predicate type is %q, not %s", attestation.PredicateType, ProofRunPredicateType)
	}
	if !trusted.Trusts(attestation.Signer) {
		return refuse("the signer %s (issuer %s) is not the provider's trusted signer %s@ (issuer %s)",
			attestation.Signer.Subject, attestation.Signer.Issuer, trusted.Workflow, trusted.Issuer)
	}
	return nil
}

// testRun is what one test whose name carries a test record's ID did in the suites' output: one testRun for each
// ID the name carries, in the order the output gives them.
type testRun struct {
	id       string
	result   Result
	duration float64
	timed    bool
	detail   string
}

// oneOutcomePerTestRecord is the rule of ST-ASR-INV06: the proof run holds at most one outcome per test record.
// When a test record's ID runs more than once, the outcome is failed if any run failed, else skipped if any was
// skipped, else passed. The duration and detail are those of the first run with that result. The outcomes are in
// test record ID order.
func oneOutcomePerTestRecord(runs []testRun) []Outcome {
	index := make(map[string]int)
	var outcomes []Outcome
	for _, run := range runs {
		i, seen := index[run.id]
		if !seen {
			index[run.id] = len(outcomes)
			outcomes = append(outcomes, outcomeOf(run))
			continue
		}
		if weight(run.result) > weight(outcomes[i].Result) {
			outcomes[i] = outcomeOf(run)
		}
	}
	sort.Slice(outcomes, func(i, j int) bool { return outcomes[i].TestRecord < outcomes[j].TestRecord })
	return outcomes
}

func outcomeOf(run testRun) Outcome {
	return Outcome{TestRecord: run.id, Result: run.result, Duration: run.duration, Timed: run.timed, Detail: run.detail}
}

// weight orders the results for oneOutcomePerTestRecord: failed over skipped over passed.
func weight(result Result) int {
	switch result {
	case Failed:
		return 2
	case Skipped:
		return 1
	}
	return 0
}
