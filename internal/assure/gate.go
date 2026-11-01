package assure

import (
	"fmt"
	"sort"
	"strings"
)

// GateKind is which bar a gate holds a commit to (ST-VO57): change or release.
type GateKind string

// The two gates.
const (
	ChangeGate  GateKind = "change"
	ReleaseGate GateKind = "release"
)

// ParseGateKind reads a gate kind. It refuses any text that is not exactly change or release (ST-VO57-C01).
func ParseGateKind(text string) (GateKind, error) {
	switch kind := GateKind(text); kind {
	case ChangeGate, ReleaseGate:
		return kind, nil
	}
	return "", fmt.Errorf("gate kind %q is not one of change or release", text)
}

// WorkflowsFolder is the folder of the provider's workflow files, from the repository's root.
const WorkflowsFolder = ".github/workflows"

// proofRunPassed is the predicate of ST-ASR-SP01, Proof run passed: the proof run's attestation is verified,
// and every test record whose state is automated in the spec at its commit has an outcome of passed. It returns
// a reason for each automated test record that did not pass, in ID order, and none when the proof run passed.
// With no verified proof run, the one reason says why.
func proofRunPassed(proof *VerifiedProofRun, spec Spec, why string) []string {
	if proof == nil {
		return []string{why}
	}
	outcomes := make(map[string]Outcome, len(proof.Run.Outcomes))
	for _, outcome := range proof.Run.Outcomes {
		outcomes[outcome.TestRecord] = outcome
	}
	var reasons []string
	for _, id := range spec.automated() {
		outcome, ok := outcomes[id]
		switch {
		case !ok:
			reasons = append(reasons, id+" has no outcome in the proof run")
		case outcome.Result.Passes():
		case outcome.Result == Failed && outcome.Detail == DidNotRun:
			reasons = append(reasons, id+" failed: "+DidNotRun)
		default:
			reasons = append(reasons, id+" "+string(outcome.Result))
		}
	}
	return reasons
}

// holdsRatchet is the predicate of ST-ASR-SP03, No obligation falls: every obligation that Lathe counts as
// designed, built or proved at the base head is still counted so at the change's commit. An obligation that is
// new at the change may arrive with no share. It returns a reason for each share an obligation fell from, by
// obligation ID and then in the order designed, built, proved, and none when the change holds the ratchet. An
// obligation that the change does not have at all, because it removes or renames the record or because the
// change's branch is behind the base, is no longer counted either: it gives one reason that says it is absent and
// names each share it had at the base.
func holdsRatchet(base, change Measurement) []string {
	var reasons []string
	for _, id := range base.sortedObligations() {
		before := base.Obligations[id]
		after, present := change.Obligations[id]
		if !present {
			var had []string
			for _, share := range allShares() {
				if before.Has(share) {
					had = append(had, string(share))
				}
			}
			if len(had) > 0 {
				reasons = append(reasons, absentReason(id, had))
			}
			continue
		}
		for _, share := range allShares() {
			if before.Has(share) && !after.Has(share) {
				reasons = append(reasons, fellReason(id, share))
			}
		}
	}
	return reasons
}

// workflowChanges lists each file under the workflows folder that differs between the base's tree and the
// change's tree: added, removed, or with other bytes or another mode, by path. It first compares the two tree
// objects of the folder, and lists nothing when they are the same object.
func workflowChanges(root, base, change string) ([]string, error) {
	baseTree, err := folderTree(root, base, WorkflowsFolder)
	if err != nil {
		return nil, err
	}
	changeTree, err := folderTree(root, change, WorkflowsFolder)
	if err != nil {
		return nil, err
	}
	if baseTree == changeTree {
		return nil, nil
	}
	out, err := git(root, "diff-tree", "-r", "-z", "--no-renames", "--name-only", base, change, "--", WorkflowsFolder+"/")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, file := range strings.Split(string(out), "\x00") {
		if file != "" {
			files = append(files, file)
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("the trees of %s at %s and %s differ, and git names no file that differs", WorkflowsFolder, base, change)
	}
	sort.Strings(files)
	return files, nil
}

// folderTree is the object ID of the folder in the commit's tree, or none when the tree has no such folder.
func folderTree(root, commit, folder string) (string, error) {
	out, err := git(root, "ls-tree", "-z", "--full-tree", commit, "--", folder)
	if err != nil {
		return "", err
	}
	entry := strings.TrimSuffix(string(out), "\x00")
	if entry == "" {
		return "", nil
	}
	info, _, _ := strings.Cut(entry, "\t")
	fields := strings.Fields(info)
	if len(fields) != 3 {
		return "", fmt.Errorf("git ls-tree gives %q for %s at %s", entry, folder, commit)
	}
	return fields[1] + " " + fields[2], nil
}

// ChangeRequest is what Gate a change reads: the repository that holds both commits, the change's commit and the
// base head, the provider's trusted signer and the change's own reference, and the path of the Lathe CLI.
type ChangeRequest struct {
	Root      string
	Commit    string
	Base      string
	Trusted   TrustedSigner
	ChangeRef string
	Lathe     string
}

// GateChange is the handler of ST-ASR-CMD02, Gate a change. It measures the coverage at the change's commit and
// at the base head, verifies the proof run at the change's commit, and refuses the change when an obligation
// present at the base lost its designed, built or proved standing, when no proof run at the commit is verified,
// when an automated test record did not pass there, or when the only proof run is change grade and the change
// alters the workflows folder. The decision names both commits and both coverages, and lists every reason in the
// fixed order; the store keeps it. It returns an error, and no decision, when it cannot read what it decides on.
func GateChange(request ChangeRequest, store AttestationStore, verifier BundleVerifier, decisions DecisionStore) (Decision, ChangeDecided, error) {
	for _, commit := range []string{request.Commit, request.Base} {
		if !fullCommit(commit) {
			return Decision{}, ChangeDecided{}, fmt.Errorf("commit %q is not a full 40-character commit ID", commit)
		}
	}
	change, err := MeasureCoverage(request.Root, request.Commit, request.Lathe)
	if err != nil {
		return Decision{}, ChangeDecided{}, err
	}
	base, err := MeasureCoverage(request.Root, request.Base, request.Lathe)
	if err != nil {
		return Decision{}, ChangeDecided{}, err
	}
	changed, err := workflowChanges(request.Root, request.Base, request.Commit)
	if err != nil {
		return Decision{}, ChangeDecided{}, err
	}
	verification, err := VerifyAttestation(request.Commit, store, verifier,
		Trust{Signer: request.Trusted, ChangeRef: request.ChangeRef, WorkflowChanges: changed})
	if err != nil {
		return Decision{}, ChangeDecided{}, err
	}
	spec, err := ReadCommittedSpec(request.Root, request.Commit)
	if err != nil {
		return Decision{}, ChangeDecided{}, err
	}
	why := noVerifiedProofRun + request.Commit
	if len(verification.Refused) == 0 {
		why += ": the provider keeps no proof run attestation for it"
	} else {
		why += ": " + strings.Join(verification.Refused, "; ")
	}
	why = strings.Join(strings.Fields(why), " ")
	causes := holdsRatchet(base, change)
	causes = append(causes, proofRunPassed(verification.Proof, spec, why)...)
	if verification.Proof == nil && verification.WorkflowsChanged {
		for _, file := range changed {
			causes = append(causes, workflowReason(file))
		}
	}
	baseCoverage := base.Coverage
	decision := Decision{
		Commit:       request.Commit,
		Kind:         ChangeGate,
		BaseCommit:   request.Base,
		Coverage:     change.Coverage,
		BaseCoverage: &baseCoverage,
		Reasons:      sortReasons(causes),
	}
	decision.Passed = len(decision.Reasons) == 0
	if verification.Proof != nil {
		attestation := verification.Proof.Attestation
		decision.ProofRun = &attestation
	}
	for _, rule := range []error{namesBothCommits(decision), namesEveryCause(decision, causes), reasonsInFixedOrder(decision)} {
		if rule != nil {
			return Decision{}, ChangeDecided{}, rule
		}
	}
	if err := decisions.Keep(decision); err != nil {
		return Decision{}, ChangeDecided{}, err
	}
	return decision, ChangeDecided{
		Commit:     decision.Commit,
		BaseCommit: decision.BaseCommit,
		Passed:     decision.Passed,
		Reasons:    append([]string(nil), decision.Reasons...),
	}, nil
}
