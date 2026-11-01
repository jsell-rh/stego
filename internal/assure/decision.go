package assure

import (
	"fmt"
	"sort"
	"strings"
)

// Decision is the decision of one gate on one commit (ST-ASR-EN03), with every reason for a refusal. A change
// decision names the base head it was gated against and the coverage there; a release decision names neither.
// Reasons holds one reason per line, in the fixed order of reasonOrder, and is empty when the commit passed.
//
// CodeFaults holds each code-standard fault of the commit's tree (ST-ASR-UC02-R05). The code standard's
// predicates are not wired into the gate yet, so the change gate leaves it empty.
type Decision struct {
	Commit       string
	Kind         GateKind
	BaseCommit   string
	Coverage     Coverage
	BaseCoverage *Coverage
	ProofRun     *Attestation
	Passed       bool
	Reasons      []string
	Attestation  *Attestation
	CodeFaults   []string
}

// Text is the decision as assure prints it and keeps it in the job's output. The same decision always gives the
// same bytes.
func (d Decision) Text() string {
	var text strings.Builder
	verdict := "refused"
	if d.Passed {
		verdict = "passed"
	}
	fmt.Fprintf(&text, "%s gate on %s: %s\n", d.Kind, d.Commit, verdict)
	if d.Kind == ChangeGate {
		fmt.Fprintf(&text, "base head: %s\n", d.BaseCommit)
	}
	fmt.Fprintf(&text, "coverage at %s: %s\n", d.Commit, d.Coverage)
	if d.BaseCoverage != nil {
		fmt.Fprintf(&text, "coverage at %s: %s\n", d.BaseCommit, *d.BaseCoverage)
	}
	if d.ProofRun != nil {
		fmt.Fprintf(&text, "proof run: %s about %s, signed by %s (issuer %s), %s\n",
			d.ProofRun.PredicateType, d.ProofRun.Subject, d.ProofRun.Signer.Subject, d.ProofRun.Signer.Issuer, d.ProofRun.LogEntry)
	} else {
		text.WriteString("proof run: none verified\n")
	}
	if len(d.Reasons) == 0 {
		text.WriteString("reasons: none\n")
		return text.String()
	}
	fmt.Fprintf(&text, "reasons (%d):\n", len(d.Reasons))
	for _, reason := range d.Reasons {
		text.WriteString(reason + "\n")
	}
	return text.String()
}

// The three sections of the reasons, in their fixed order.
const (
	obligationSection = iota
	proofSection
	workflowSection
)

// The reasons, as the gate writes them.
const (
	fellFrom           = " fell from "
	differsFromBase    = " differs from the base head"
	noVerifiedProofRun = "no verified proof run at "
)

// fellReason is the reason that names an obligation that fell from a share.
func fellReason(obligation string, share Share) string {
	return obligation + fellFrom + string(share)
}

// workflowReason is the reason that names a file under the workflows folder that differs from the base head's.
func workflowReason(file string) string {
	return file + differsFromBase
}

// reasonOrder is the place of a reason in the fixed order: the obligations that fell, by ID, each with its shares
// in the order designed, built, proved; then the proof run (why there is none first) and the test records that
// did not pass, by ID; then the workflow files that differ, by path.
func reasonOrder(reason string) (int, string) {
	if file, ok := strings.CutSuffix(reason, differsFromBase); ok {
		return workflowSection, file
	}
	if obligation, share, ok := strings.Cut(reason, fellFrom); ok && !strings.Contains(obligation, " ") {
		for i, s := range allShares() {
			if Share(share) == s {
				return obligationSection, obligation + "\x00" + string(rune('0'+i))
			}
		}
	}
	if strings.HasPrefix(reason, noVerifiedProofRun) {
		return proofSection, ""
	}
	id, _, _ := strings.Cut(reason, " ")
	return proofSection, id
}

// sortReasons puts the reasons in the fixed order of reasonOrder, with each reason once.
func sortReasons(reasons []string) []string {
	sorted := append([]string(nil), reasons...)
	sort.SliceStable(sorted, func(i, j int) bool { return reasonBefore(sorted[i], sorted[j]) })
	unique := sorted[:0]
	for i, reason := range sorted {
		if i == 0 || reason != sorted[i-1] {
			unique = append(unique, reason)
		}
	}
	return unique
}

// reasonBefore tells if reason a comes before reason b in the fixed order.
func reasonBefore(a, b string) bool {
	sectionA, keyA := reasonOrder(a)
	sectionB, keyB := reasonOrder(b)
	if sectionA != sectionB {
		return sectionA < sectionB
	}
	if keyA != keyB {
		return keyA < keyB
	}
	return a < b
}

// namesBothCommits is the rule of ST-ASR-INV07: a change decision names the change's commit and the base head it
// was gated against, with the coverage of both; a release decision names only its commit.
func namesBothCommits(decision Decision) error {
	refuse := func(format string, args ...any) error {
		return &Refusal{Rule: "namesBothCommits", Reason: fmt.Sprintf(format, args...)}
	}
	if !fullCommit(decision.Commit) {
		return refuse("the decision's commit %q is not a full 40-character commit ID", decision.Commit)
	}
	switch decision.Kind {
	case ChangeGate:
		if !fullCommit(decision.BaseCommit) {
			return refuse("the change decision on %s names the base head %q, not a full commit", decision.Commit, decision.BaseCommit)
		}
		if decision.BaseCoverage == nil {
			return refuse("the change decision on %s does not name the coverage at the base head %s", decision.Commit, decision.BaseCommit)
		}
	case ReleaseGate:
		if decision.BaseCommit != "" || decision.BaseCoverage != nil {
			return refuse("the release decision on %s names a base head, %q", decision.Commit, decision.BaseCommit)
		}
	default:
		return refuse("the decision's gate kind %q is not change or release", decision.Kind)
	}
	return nil
}

// namesEveryCause is the rule of ST-ASR-INV08: a refused decision names each cause, every obligation that fell
// from a share and every test record that did not pass, each given as its reason; a passed decision names no
// reason, and a decision with a reason is not passed.
func namesEveryCause(decision Decision, causes []string) error {
	refuse := func(format string, args ...any) error {
		return &Refusal{Rule: "namesEveryCause", Reason: fmt.Sprintf(format, args...)}
	}
	if decision.Passed != (len(decision.Reasons) == 0) {
		return refuse("the decision on %s is passed=%t with %d reasons", decision.Commit, decision.Passed, len(decision.Reasons))
	}
	named := make(map[string]bool, len(decision.Reasons))
	for _, reason := range decision.Reasons {
		named[reason] = true
	}
	var missing []string
	for _, cause := range causes {
		if !named[cause] {
			missing = append(missing, cause)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return refuse("the decision on %s does not name: %s", decision.Commit, strings.Join(missing, "; "))
	}
	return nil
}

// reasonsInFixedOrder is the rule of ST-ASR-INV10: the reasons are in the fixed order of reasonOrder, each once,
// so that the same commits and proof run give the same decision, with the same reasons in the same order.
func reasonsInFixedOrder(decision Decision) error {
	for i := 1; i < len(decision.Reasons); i++ {
		if !reasonBefore(decision.Reasons[i-1], decision.Reasons[i]) {
			return &Refusal{Rule: "reasonsInFixedOrder", Reason: fmt.Sprintf(
				"the reason %q comes after %q, against the fixed order, or twice", decision.Reasons[i], decision.Reasons[i-1])}
		}
	}
	return nil
}
