package assure

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// ProofRunPredicateType is the predicate type of the proof run statement. STEGO owns it: it is a URI under the
// STEGO repository, and version 1 is the document that encodeProofRun writes.
const ProofRunPredicateType = "https://github.com/jsell-rh/stego/assure/proof-run/v1"

// Attestation is a reference to a signed statement about one commit (ST-VO56): its predicate type, the commit
// it is about, who signed it, and the transparency log entry that holds it when the signer is keyless.
//
// The predicate type of a proof run is ProofRunPredicateType,
// https://github.com/jsell-rh/stego/assure/proof-run/v1. The subject is always a full 40-character commit ID
// (ST-VO56-C01). An attestation store indexes the statement by SubjectDigest of that commit.
type Attestation struct {
	PredicateType string
	Subject       string
	Signer        Signer
	LogEntry      string
}

// Signer is who signed a statement (ST-VO50): for a keyless signer, the OIDC issuer that vouched for it and the
// workflow identity with the exact reference it ran at.
type Signer struct {
	Issuer  string
	Subject string
}

// TrustedSigner is the one identity a CI provider may sign a proof run with: the issuer, and the workflow
// identity up to the "@" that starts its reference. A run of that workflow at any reference signs a proof run
// (ST-ASR-UC01); a run of any other workflow does not. What the signature is worth to a gate depends on the
// reference: see Grade.
type TrustedSigner struct {
	Issuer   string
	Workflow string
}

// TrustGrade is what a proof run's signature is worth to a gate (ST-ASR-TM09), read from the reference in the
// signer's identity.
type TrustGrade string

// The trust grades. NoGrade is worth nothing to a gate.
const (
	NoGrade      TrustGrade = ""
	ChangeGrade  TrustGrade = "change"
	ReleaseGrade TrustGrade = "release"
)

// MainRef is the reference of the base branch main. The trusted workflow signs at main only from main's own
// tree, which no change can alter before it merges.
const MainRef = "refs/heads/main"

// Trusts tells if the signer is the trusted signer: the issuer, and the workflow at any reference.
func (t TrustedSigner) Trusts(signer Signer) bool {
	_, ok := t.reference(signer)
	return ok
}

// Grade tells what the signer's signature is worth to a gate: release grade when the trusted workflow signed at
// main; change grade when it signed at the change's own reference, changeRef, such as refs/pull/7/merge; and no
// grade for any other reference or identity, or for a changeRef that is empty or main.
func (t TrustedSigner) Grade(signer Signer, changeRef string) TrustGrade {
	reference, ok := t.reference(signer)
	switch {
	case !ok:
		return NoGrade
	case reference == MainRef:
		return ReleaseGrade
	case changeRef != "" && changeRef != MainRef && reference == changeRef:
		return ChangeGrade
	}
	return NoGrade
}

// reference is the reference the signer's workflow ran at, when the signer is the trusted signer.
func (t TrustedSigner) reference(signer Signer) (string, bool) {
	if t.Issuer == "" || t.Workflow == "" || signer.Issuer != t.Issuer {
		return "", false
	}
	reference, ok := strings.CutPrefix(signer.Subject, t.Workflow+"@")
	if !ok || reference == "" {
		return "", false
	}
	return reference, true
}

// NewAttestation makes an attestation reference. It refuses a subject that is not a full 40-character commit
// ID, such as a tag, a branch or a short ID (ST-VO56-C01), and a reference with no predicate type or signer.
func NewAttestation(predicateType, subject string, signer Signer, logEntry string) (Attestation, error) {
	if !fullCommit(subject) {
		return Attestation{}, fmt.Errorf("attestation subject %q is not a full 40-character commit ID", subject)
	}
	if predicateType == "" {
		return Attestation{}, errors.New("attestation has no predicate type")
	}
	if signer.Issuer == "" || signer.Subject == "" {
		return Attestation{}, errors.New("attestation has no signer")
	}
	return Attestation{PredicateType: predicateType, Subject: subject, Signer: signer, LogEntry: logEntry}, nil
}

// SubjectDigest is the digest that an attestation store indexes a statement about the commit by: "sha256:" and
// the SHA-256 of the full commit ID as text. A store takes only SHA-2 digests, and a commit ID is a SHA-1, so
// the statement names the commit in its subject name and in its predicate, and anyone who knows the commit can
// find the statement.
func SubjectDigest(commit string) string {
	sum := sha256.Sum256([]byte(commit))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// fullCommit tells if the text is a full commit ID: 40 lower-case hexadecimal digits.
func fullCommit(text string) bool {
	if len(text) != 40 {
		return false
	}
	for _, r := range text {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
