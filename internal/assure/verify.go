package assure

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// SignedStatement is what a signature check gives for one bundle: the in-toto statement that the signature
// covers, the signer that the signing certificate names, the source repository, ref and digest (the commit the
// signing workflow ran at) that the certificate names, and the index of the transparency log entry that holds it. Only a BundleVerifier makes one, and only
// when every check of the bundle passed.
type SignedStatement struct {
	Subjects         []StatementSubject
	PredicateType    string
	Predicate        json.RawMessage
	Signer           Signer
	SourceRepository string
	SourceRef        string
	SourceDigest     string
	LogIndex         int64
}

// StatementSubject is one subject of a statement: its name, and its digest as "sha256:" and hexadecimal.
type StatementSubject struct {
	Name   string
	Digest string
}

// BundleVerifier checks one signed bundle as the provider keeps it: the signature against the certificate, the
// certificate against the trust root, the transparency log entry, and that the statement names the subject
// digest. It returns the statement the signature covers, with the signer from the certificate, and nothing at
// all when any check fails.
type BundleVerifier interface {
	VerifyBundle(bundle []byte, subjectDigest string) (SignedStatement, error)
}

// SigstoreVerifier checks Sigstore bundles in process with sigstore-go, against one trust root, as gh
// attestation verify does: the certificate chains to the root's certificate authority and has a signed
// certificate timestamp, the transparency log holds the entry, and the log's time falls in the certificate's
// validity.
type SigstoreVerifier struct {
	verifier *verify.Verifier
}

// NewSigstoreVerifier makes a verifier that trusts the Sigstore trust root given as its JSON document.
func NewSigstoreVerifier(trustedRoot []byte) (SigstoreVerifier, error) {
	material, err := root.NewTrustedRootFromJSON(trustedRoot)
	if err != nil {
		return SigstoreVerifier{}, fmt.Errorf("the Sigstore trust root: %w", err)
	}
	verifier, err := verify.NewVerifier(material,
		verify.WithSignedCertificateTimestamps(1), verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))
	if err != nil {
		return SigstoreVerifier{}, err
	}
	return SigstoreVerifier{verifier: verifier}, nil
}

// PublicGoodTrustedRoot fetches the trust root of the public-good Sigstore instance, which signs the
// attestations of a public GitHub repository, through Sigstore's TUF repository. TUF checks the root against the
// keys sigstore-go carries. It keeps its cache in the folder cache, which must be outside the product's
// repository.
func PublicGoodTrustedRoot(cache string) ([]byte, error) {
	options := tuf.DefaultOptions().WithCachePath(cache)
	client, err := tuf.New(options)
	if err != nil {
		return nil, fmt.Errorf("Sigstore's TUF repository: %w", err)
	}
	return client.GetTarget("trusted_root.json")
}

// VerifyBundle checks the bundle and returns the statement it signs. It refuses an empty bundle.
func (s SigstoreVerifier) VerifyBundle(data []byte, subjectDigest string) (SignedStatement, error) {
	if s.verifier == nil {
		return SignedStatement{}, errors.New("the verifier has no trust root")
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return SignedStatement{}, errors.New("the provider gives no bundle")
	}
	algorithm, digest, ok := strings.Cut(subjectDigest, ":")
	sum, err := hex.DecodeString(digest)
	if !ok || algorithm != "sha256" || err != nil || len(sum) != 32 {
		return SignedStatement{}, fmt.Errorf("the subject digest %q is not sha256 and 64 hexadecimal digits", subjectDigest)
	}
	var signed bundle.Bundle
	if err := signed.UnmarshalJSON(data); err != nil {
		return SignedStatement{}, fmt.Errorf("the bundle does not load: %w", err)
	}
	result, err := s.verifier.Verify(&signed, verify.NewPolicy(verify.WithArtifactDigest("sha256", sum), verify.WithoutIdentitiesUnsafe()))
	if err != nil {
		return SignedStatement{}, fmt.Errorf("the bundle does not verify: %w", err)
	}
	if result.Signature == nil || result.Signature.Certificate == nil {
		return SignedStatement{}, errors.New("the bundle is not signed by a certificate")
	}
	envelope, err := signed.Envelope()
	if err != nil {
		return SignedStatement{}, fmt.Errorf("the bundle holds no signed statement: %w", err)
	}
	payload, err := envelope.RawEnvelope().DecodeB64Payload()
	if err != nil {
		return SignedStatement{}, err
	}
	entries, err := signed.TlogEntries()
	if err != nil || len(entries) == 0 {
		return SignedStatement{}, errors.New("the bundle holds no transparency log entry")
	}
	statement, err := readStatement(payload)
	if err != nil {
		return SignedStatement{}, err
	}
	certificate := result.Signature.Certificate
	statement.Signer = Signer{Issuer: certificate.Issuer, Subject: certificate.SubjectAlternativeName}
	statement.SourceRepository, statement.SourceRef = certificate.SourceRepositoryURI, certificate.SourceRepositoryRef
	statement.SourceDigest = certificate.SourceRepositoryDigest
	statement.LogIndex = entries[0].LogIndex()
	return statement, nil
}

// readStatement reads an in-toto statement, version 1, from the payload the signature covers.
func readStatement(payload []byte) (SignedStatement, error) {
	var document struct {
		Type    string `json:"_type"`
		Subject []struct {
			Name   string            `json:"name"`
			Digest map[string]string `json:"digest"`
		} `json:"subject"`
		PredicateType string          `json:"predicateType"`
		Predicate     json.RawMessage `json:"predicate"`
	}
	if err := json.Unmarshal(payload, &document); err != nil {
		return SignedStatement{}, fmt.Errorf("the signed payload is not an in-toto statement: %w", err)
	}
	if document.Type != "https://in-toto.io/Statement/v1" {
		return SignedStatement{}, fmt.Errorf("the signed payload is of type %q, not an in-toto statement, version 1", document.Type)
	}
	statement := SignedStatement{PredicateType: document.PredicateType, Predicate: document.Predicate}
	for _, subject := range document.Subject {
		statement.Subjects = append(statement.Subjects, StatementSubject{Name: subject.Name, Digest: "sha256:" + subject.Digest["sha256"]})
	}
	return statement, nil
}

// Trust is what a gate trusts a proof run's signature at: the provider's trusted signer, the change's own
// reference, and each file under the workflows folder that differs between the base head's tree and the
// change's tree.
type Trust struct {
	Signer          TrustedSigner
	ChangeRef       string
	WorkflowChanges []string
}

// attestationVerified is the predicate of ST-ASR-SP02, Attestation is verified. The statement, which a
// BundleVerifier gave, must be about exactly the commit: one subject, with SubjectDigest of the commit and a
// name that ends in the commit, and a proof run predicate about the commit. Its predicate type must be
// ProofRunPredicateType. The certificate's source repository and source ref must be the trusted workflow's
// repository and the ref the signer ran at. Its signer, from the certificate, must be the trusted signer at a
// grade: release grade at main, for any tree, when the certificate's source digest, the commit the signing run ran
// at, is the commit; change grade at the change's own reference, only while no file under the workflows
// folder differs from the base head's. It returns the grade, and a refusal that says why when the statement
// fails. A change-grade statement that fails only on the workflows folder returns ChangeGrade with the refusal,
// so the gate can name each file.
func attestationVerified(statement SignedStatement, commit string, trust Trust) (TrustGrade, error) {
	refuse := func(format string, args ...any) error {
		return &Refusal{Rule: "attestationVerified", Reason: fmt.Sprintf(format, args...)}
	}
	if !fullCommit(commit) {
		return NoGrade, refuse("commit %q is not a full 40-character commit ID", commit)
	}
	if len(statement.Subjects) != 1 {
		return NoGrade, refuse("the statement names %d subjects, not exactly the commit %s", len(statement.Subjects), commit)
	}
	subject := statement.Subjects[0]
	if subject.Digest != SubjectDigest(commit) || !strings.HasSuffix(subject.Name, "@"+commit) {
		return NoGrade, refuse("the statement is about %s (%s), not about the commit %s", subject.Name, subject.Digest, commit)
	}
	if statement.PredicateType != ProofRunPredicateType {
		return NoGrade, refuse("the predicate type is %q, not %s", statement.PredicateType, ProofRunPredicateType)
	}
	var predicate struct {
		Commit string `json:"commit"`
	}
	if err := json.Unmarshal(statement.Predicate, &predicate); err != nil || predicate.Commit != commit {
		return NoGrade, refuse("the proof run in the statement is about commit %q, not about %s", predicate.Commit, commit)
	}
	grade := trust.Signer.Grade(statement.Signer, trust.ChangeRef)
	if grade != NoGrade {
		repository, _, _ := strings.Cut(trust.Signer.Workflow, "/.github/workflows/")
		_, reference, _ := strings.Cut(statement.Signer.Subject, "@")
		if statement.SourceRepository != repository || statement.SourceRef != reference {
			return NoGrade, refuse("the certificate names the source %s at %s, not the trusted workflow's repository %s at %s",
				statement.SourceRepository, statement.SourceRef, repository, reference)
		}
	}
	switch grade {
	case ReleaseGrade:
		// The run at main signs the commit it ran at, so a release-grade statement about another commit is not
		// that run's. A pull request's run runs at its merge commit, so change grade has no such check.
		if statement.SourceDigest != commit {
			return NoGrade, refuse("it is signed at %s by a run at commit %q, not at the commit %s it is about",
				MainRef, statement.SourceDigest, commit)
		}
		return grade, nil
	case ChangeGrade:
		if len(trust.WorkflowChanges) > 0 {
			return grade, refuse("it is signed at the change's own reference %s, and the change alters the workflows folder: %s",
				trust.ChangeRef, strings.Join(trust.WorkflowChanges, ", "))
		}
		return grade, nil
	}
	changeRef := trust.ChangeRef
	if changeRef == "" {
		if reference, ok := trust.Signer.reference(statement.Signer); ok {
			refusal := refuse("it is signed by %s at %s, not at %s, and only release-grade evidence counts here",
				trust.Signer.Workflow, reference, MainRef).(*Refusal)
			refusal.SignedAt = reference
			return NoGrade, refusal
		}
		changeRef = "none"
	}
	return NoGrade, refuse("it is signed by %s (issuer %s), not by %s at %s or at the change's own reference (%s)",
		statement.Signer.Subject, statement.Signer.Issuer, trust.Signer.Workflow, MainRef, changeRef)
}

// VerifiedProofRun is a proof run whose attestation passed every check, with the grade of its signature.
type VerifiedProofRun struct {
	Run         ProofRun
	Attestation Attestation
	Grade       TrustGrade
}

// Verification is what VerifyAttestation found for a commit: the proof run it accepted, or none; why it refused
// each attestation, in order; whether it refused a change-grade proof run only because the change alters the
// workflows folder; and, with no change reference gated, each attestation it refused only because the trusted
// workflow signed it at a pull request's reference, which counts only in that change's own gate.
type Verification struct {
	Proof            *VerifiedProofRun
	Refused          []string
	WorkflowsChanged bool
	ChangeGrade      []ChangeGradeAttestation
}

// ChangeGradeAttestation is an attestation that the trusted workflow signed at a pull request's reference, which
// VerifyAttestation refused because no change reference is gated: the reference, the log entry, and the reason,
// as Refused gives it.
type ChangeGradeAttestation struct {
	Ref      string
	LogEntry string
	Reason   string
}

// VerifyAttestation is the service of ST-ASR-DS03, Verify an attestation. It fetches every proof run
// attestation the store keeps for the commit, by the commit, and accepts one only when the verifier checks its
// bundle and attestationVerified accepts its statement, and its predicate is a whole proof run about the commit.
// Any failed check refuses that attestation whole; nothing of it is kept. When it accepts more than one, it takes
// a release-grade one over a change-grade one, then the newest by its transparency log entry, so the same
// attestations always give the same proof run. A store that cannot answer is an error, not a refusal.
func VerifyAttestation(commit string, store AttestationStore, verifier BundleVerifier, trust Trust) (Verification, error) {
	if !fullCommit(commit) {
		return Verification{}, fmt.Errorf("commit %q is not a full 40-character commit ID", commit)
	}
	bundles, err := store.Fetch(commit)
	if err != nil {
		return Verification{}, fmt.Errorf("the attestations of %s: %w", commit, err)
	}
	var verification Verification
	var accepted []VerifiedProofRun
	var logIndexes []int64
	for _, data := range bundles {
		statement, err := verifier.VerifyBundle(data, SubjectDigest(commit))
		if err != nil {
			verification.Refused = append(verification.Refused, "an attestation fails its signature check: "+err.Error())
			continue
		}
		where := "the attestation at rekor:" + strconv.FormatInt(statement.LogIndex, 10)
		grade, err := attestationVerified(statement, commit, trust)
		if err != nil {
			reason := where + " is refused: " + err.Error()
			verification.Refused = append(verification.Refused, reason)
			verification.WorkflowsChanged = verification.WorkflowsChanged || grade == ChangeGrade
			var refusal *Refusal
			if errors.As(err, &refusal) && strings.HasPrefix(refusal.SignedAt, "refs/pull/") {
				verification.ChangeGrade = append(verification.ChangeGrade, ChangeGradeAttestation{
					Ref: refusal.SignedAt, LogEntry: "rekor:" + strconv.FormatInt(statement.LogIndex, 10), Reason: reason,
				})
			}
			continue
		}
		run, err := decodeProofRun(statement.Predicate)
		if err != nil {
			verification.Refused = append(verification.Refused, where+" holds no proof run: "+err.Error())
			continue
		}
		attestation, err := NewAttestation(statement.PredicateType, commit, statement.Signer, "rekor:"+strconv.FormatInt(statement.LogIndex, 10))
		if err != nil {
			verification.Refused = append(verification.Refused, where+" is refused: "+err.Error())
			continue
		}
		run.Attestation = attestation
		accepted = append(accepted, VerifiedProofRun{Run: run, Attestation: attestation, Grade: grade})
		logIndexes = append(logIndexes, statement.LogIndex)
	}
	sort.Strings(verification.Refused)
	sort.Slice(verification.ChangeGrade, func(i, j int) bool { return verification.ChangeGrade[i].Reason < verification.ChangeGrade[j].Reason })
	best := -1
	for i := range accepted {
		if best < 0 || better(accepted[i].Grade, logIndexes[i], accepted[best].Grade, logIndexes[best]) {
			best = i
		}
	}
	if best >= 0 {
		proof := accepted[best]
		verification.Proof = &proof
		verification.WorkflowsChanged = false
	}
	return verification, nil
}

// better tells if a proof run of grade a at log index i is to be taken over one of grade b at log index j.
func better(a TrustGrade, i int64, b TrustGrade, j int64) bool {
	if a != b {
		return a == ReleaseGrade
	}
	return i > j
}

// decodeProofRun reads a proof run predicate, version 1 of ProofRunPredicateType, as encodeProofRun writes it.
// It refuses a field it does not know, a result that is not passed, failed or skipped, a test record that has
// two outcomes, and a time that is not RFC 3339.
func decodeProofRun(predicate []byte) (ProofRun, error) {
	var document proofRunDocument
	decoder := json.NewDecoder(bytes.NewReader(predicate))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return ProofRun{}, fmt.Errorf("the predicate is not a proof run: %w", err)
	}
	if decoder.More() {
		return ProofRun{}, errors.New("the predicate holds more than one document")
	}
	started, err := time.Parse(time.RFC3339Nano, document.StartedAt)
	if err != nil {
		return ProofRun{}, fmt.Errorf("the proof run's start: %w", err)
	}
	finished, err := time.Parse(time.RFC3339Nano, document.FinishedAt)
	if err != nil {
		return ProofRun{}, fmt.Errorf("the proof run's finish: %w", err)
	}
	run := ProofRun{
		Commit:       document.Commit,
		Provider:     document.Provider,
		RunReference: document.RunReference,
		StartedAt:    started,
		FinishedAt:   finished,
		SpecDigest:   document.SpecDigest,
	}
	seen := make(map[string]bool, len(document.Outcomes))
	for _, entry := range document.Outcomes {
		result, err := ParseResult(string(entry.Result))
		if err != nil {
			return ProofRun{}, err
		}
		if entry.TestRecord == "" || seen[entry.TestRecord] {
			return ProofRun{}, fmt.Errorf("the proof run has no ID or two outcomes for test record %q", entry.TestRecord)
		}
		seen[entry.TestRecord] = true
		outcome := Outcome{TestRecord: entry.TestRecord, Result: result, File: entry.File, Detail: entry.Detail}
		if entry.Duration != nil {
			outcome.Duration, outcome.Timed = *entry.Duration, true
		}
		run.Outcomes = append(run.Outcomes, outcome)
	}
	sort.Slice(run.Outcomes, func(i, j int) bool { return run.Outcomes[i].TestRecord < run.Outcomes[j].TestRecord })
	return run, nil
}
