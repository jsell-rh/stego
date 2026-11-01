package assure

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// DecisionFile is the name of the file that holds a passed release decision's predicate, the decision.json table
// (ST-ASR-EN03-TB), outside the product's repository. The provider's attestation action signs it with the
// release commit as its subject, at ReleaseDecisionPredicateType.
const DecisionFile = "decision.json"

// decisionDocument is the predicate of a release decision, version 1 of ReleaseDecisionPredicateType: one JSON
// object with the decision's commit, gate kind, coverage, whether it passed, its reasons and code faults, and the
// proof run it rests on. The same decision always gives the same bytes.
type decisionDocument struct {
	Commit     string               `json:"commit"`
	Kind       GateKind             `json:"kind"`
	Coverage   coverageDocument     `json:"coverage"`
	Passed     bool                 `json:"passed"`
	Reasons    []string             `json:"reasons"`
	CodeFaults []string             `json:"codeFaults"`
	ProofRun   *attestationDocument `json:"proofRun"`
}

type coverageDocument struct {
	Total    int `json:"total"`
	Designed int `json:"designed"`
	Built    int `json:"built"`
	Proved   int `json:"proved"`
}

type attestationDocument struct {
	PredicateType string `json:"predicateType"`
	Subject       string `json:"subject"`
	Issuer        string `json:"issuer"`
	Signer        string `json:"signer"`
	LogEntry      string `json:"logEntry"`
}

// encodeDecision writes the decision's predicate, with a newline at its end, and its digest.
func encodeDecision(decision Decision) ([]byte, string, error) {
	document := decisionDocument{
		Commit: decision.Commit,
		Kind:   decision.Kind,
		Coverage: coverageDocument{
			Total:    decision.Coverage.Total,
			Designed: decision.Coverage.Designed,
			Built:    decision.Coverage.Built,
			Proved:   decision.Coverage.Proved,
		},
		Passed:     decision.Passed,
		Reasons:    append([]string{}, decision.Reasons...),
		CodeFaults: append([]string{}, decision.CodeFaults...),
	}
	if proof := decision.ProofRun; proof != nil {
		document.ProofRun = &attestationDocument{
			PredicateType: proof.PredicateType,
			Subject:       proof.Subject,
			Issuer:        proof.Signer.Issuer,
			Signer:        proof.Signer.Subject,
			LogEntry:      proof.LogEntry,
		}
	}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, "", err
	}
	data = append(data, '\n')
	sum := sha256.Sum256(data)
	return data, "sha256:" + hex.EncodeToString(sum[:]), nil
}

// notReason is the reason that names an obligation of the release commit that does not have a share.
func notReason(obligation string, share Share) string {
	return obligation + isNot + string(share)
}

// fullyProved is the predicate of ST-ASR-SP04, Release is fully proved: the designed, built and proved shares at
// the commit are each 100%, and a proof run at the commit passed. It returns a reason for each share each
// obligation does not have, by obligation ID and then in the order designed, built, proved, and then the reasons
// of proofRunPassed: why there is no verified proof run, or each automated test record that did not pass. A proof
// run that is not release grade is no proof run to a release: the reason says so and names the reference it was
// signed at. It returns none when the commit is fully proved.
func fullyProved(measurement Measurement, proof *VerifiedProofRun, spec Spec, why string) []string {
	var reasons []string
	for _, id := range measurement.sortedObligations() {
		standing := measurement.Obligations[id]
		for _, share := range allShares() {
			if !standing.Has(share) {
				reasons = append(reasons, notReason(id, share))
			}
		}
	}
	if proof != nil && proof.Grade != ReleaseGrade {
		_, reference, _ := strings.Cut(proof.Attestation.Signer.Subject, "@")
		why = fmt.Sprintf("%s%s: the proof run at %s is signed at %s, not at %s, and a release takes only release-grade evidence",
			noVerifiedProofRun, proof.Attestation.Subject, proof.Attestation.LogEntry, reference, MainRef)
		proof = nil
	}
	return append(reasons, proofRunPassed(proof, spec, why)...)
}

// releaseAttested is the rule of ST-ASR-INV09, A release decision is attested: a passed release decision is
// handed to the provider's attestation action, and nothing else is. A passed release decision names the
// attestation the action is to make: about the release commit, at ReleaseDecisionPredicateType, by the trusted
// signer at main, the only grade a release decision is signed at. A refused decision, or a change decision, names
// no attestation and is not handed over.
func releaseAttested(decision Decision, trusted TrustedSigner) error {
	refuse := func(format string, args ...any) error {
		return &Refusal{Rule: "releaseAttested", Reason: fmt.Sprintf(format, args...)}
	}
	if decision.Kind != ReleaseGate || !decision.Passed {
		if decision.Attestation != nil {
			return refuse("the %s decision on %s (passed=%t) names an attestation; only a passed release decision is attested",
				decision.Kind, decision.Commit, decision.Passed)
		}
		return nil
	}
	attestation := decision.Attestation
	if attestation == nil {
		return refuse("the passed release decision on %s names no attestation", decision.Commit)
	}
	if attestation.Subject != decision.Commit {
		return refuse("the attestation is about %q, not about the release commit %s", attestation.Subject, decision.Commit)
	}
	if attestation.PredicateType != ReleaseDecisionPredicateType {
		return refuse("the attestation's predicate type is %q, not %s", attestation.PredicateType, ReleaseDecisionPredicateType)
	}
	if trusted.Grade(attestation.Signer, "") != ReleaseGrade {
		return refuse("the signer %s (issuer %s) is not the provider's trusted signer %s at %s (issuer %s)",
			attestation.Signer.Subject, attestation.Signer.Issuer, trusted.Workflow, MainRef, trusted.Issuer)
	}
	return nil
}

// ReleaseRequest is what Gate a release reads: the repository that holds the release commit, the commit, the
// provider's trusted signer, the identity the job's attestation action signs with, and the path of the Lathe CLI.
type ReleaseRequest struct {
	Root    string
	Commit  string
	Trusted TrustedSigner
	Signer  Signer
	Lathe   string
}

// GateRelease is the handler of ST-ASR-CMD03, Gate a release. It measures the coverage at the release commit,
// verifies the proof run there and takes only a release-grade one, signed by the trusted workflow at main, and
// reads the code faults of the commit's tree and spec. It refuses the release unless every share is 100%, naming
// each obligation that is not by its ID and share; unless every automated test record passed; and unless the
// tree has no code-standard fault, naming each with its file or package and the rule. The decision names only
// the commit, lists every reason in the fixed order, is checked by the rules of the Gate decision, and is kept
// by the store. A passed decision names the attestation the provider's action is to make, and once the store has
// kept the decision, it hands it over as the decision.json predicate; when either fails, no predicate is written. It returns an error, and no decision, when it
// cannot read what it decides on, such as when Lathe finds an error in the spec at the commit.
func GateRelease(request ReleaseRequest, store AttestationStore, verifier BundleVerifier, decisions DecisionStore) (Decision, ReleaseDecided, error) {
	if !fullCommit(request.Commit) {
		return Decision{}, ReleaseDecided{}, fmt.Errorf("commit %q is not a full 40-character commit ID", request.Commit)
	}
	measurement, err := MeasureCoverage(request.Root, request.Commit, request.Lathe)
	if err != nil {
		return Decision{}, ReleaseDecided{}, err
	}
	verification, err := VerifyAttestation(request.Commit, store, verifier, Trust{Signer: request.Trusted})
	if err != nil {
		return Decision{}, ReleaseDecided{}, err
	}
	spec, err := ReadCommittedSpec(request.Root, request.Commit)
	if err != nil {
		return Decision{}, ReleaseDecided{}, err
	}
	faults, err := committedFaults(request.Root, request.Commit, spec)
	if err != nil {
		return Decision{}, ReleaseDecided{}, err
	}
	causes := fullyProved(measurement, verification.Proof, spec, noProofRunWhy(request.Commit, verification))
	for _, fault := range faults {
		causes = append(causes, codeFaultReason(fault))
	}
	decision := Decision{
		Commit:     request.Commit,
		Kind:       ReleaseGate,
		Coverage:   measurement.Coverage,
		Reasons:    sortReasons(causes),
		CodeFaults: describeFaults(faults, nil),
	}
	decision.Passed = len(decision.Reasons) == 0
	if verification.Proof != nil {
		attestation := verification.Proof.Attestation
		decision.ProofRun = &attestation
	}
	if decision.Passed {
		attestation, err := NewAttestation(ReleaseDecisionPredicateType, request.Commit, request.Signer, "")
		if err != nil {
			return Decision{}, ReleaseDecided{}, err
		}
		decision.Attestation = &attestation
	}
	for _, rule := range []error{namesBothCommits(decision), namesEveryCause(decision, causes), reasonsInFixedOrder(decision),
		releaseAttested(decision, request.Trusted)} {
		if rule != nil {
			return Decision{}, ReleaseDecided{}, rule
		}
	}
	if err := decisions.Keep(decision); err != nil {
		return Decision{}, ReleaseDecided{}, err
	}
	event := ReleaseDecided{Commit: decision.Commit, Passed: decision.Passed, Reasons: append([]string(nil), decision.Reasons...)}
	if decision.Passed {
		digest, err := decisions.HandOver(decision)
		if err != nil {
			return Decision{}, ReleaseDecided{}, fmt.Errorf("the decision predicate is not written: %w", err)
		}
		event.Attestation, event.PredicateDigest = decision.Attestation, digest
	}
	return decision, event, nil
}
