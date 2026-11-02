package assure

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	trustedWorkflow = "https://github.com/jsell-rh/stego/.github/workflows/proof-run.yml"
	changeRef       = "refs/pull/7/merge"
)

// memoryStore is an attestation store that keeps bundles in memory by commit, and notes each commit asked for.
type memoryStore struct {
	bundles map[string][][]byte
	asked   []string
	err     error
}

func (s *memoryStore) HandOver(ProofRun) (string, error) {
	return "", errors.New("the memory store takes no proof run")
}

func (s *memoryStore) Fetch(commit string) ([][]byte, error) {
	s.asked = append(s.asked, commit)
	return s.bundles[commit], s.err
}

// statementVerifier stands for the signature check at the adapter's edge: a bundle is a SignedStatement as JSON,
// and one whose signer subject is "forged" fails the check.
type statementVerifier struct{}

func (statementVerifier) VerifyBundle(data []byte, subjectDigest string) (SignedStatement, error) {
	var statement SignedStatement
	if err := json.Unmarshal(data, &statement); err != nil {
		return SignedStatement{}, err
	}
	if statement.Signer.Subject == "forged" {
		return SignedStatement{}, errors.New("the signature does not verify")
	}
	for _, subject := range statement.Subjects {
		if subject.Digest != subjectDigest {
			return SignedStatement{}, errors.New("the statement does not name the subject digest")
		}
	}
	return statement, nil
}

// proofRunStatement is a signed proof run statement about the commit, signed by the trusted workflow at the
// reference, with the outcomes given as "ID result".
func proofRunStatement(t *testing.T, commit, reference string, logIndex int64, outcomes ...string) SignedStatement {
	t.Helper()
	run := ProofRun{Commit: commit, Provider: "github-actions", RunReference: "github:1/1", SpecDigest: "sha256:" + strings.Repeat("a", 64)}
	for _, outcome := range outcomes {
		id, result, _ := strings.Cut(outcome, " ")
		run.Outcomes = append(run.Outcomes, Outcome{TestRecord: id, Result: Result(result), File: "internal/fix/fix_test.go"})
	}
	predicate, err := encodeProofRun(run)
	if err != nil {
		t.Fatal(err)
	}
	return SignedStatement{
		Subjects:         []StatementSubject{{Name: "git+https://github.com/jsell-rh/stego@" + commit, Digest: SubjectDigest(commit)}},
		PredicateType:    ProofRunPredicateType,
		Predicate:        predicate,
		Signer:           Signer{Issuer: GitHubIssuer, Subject: trustedWorkflow + "@" + reference},
		SourceRepository: "https://github.com/jsell-rh/stego",
		SourceRef:        reference,
		SourceDigest:     commit,
		LogIndex:         logIndex,
	}
}

func bundleOf(t *testing.T, statement SignedStatement) []byte {
	t.Helper()
	data, err := json.Marshal(statement)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// sigstoreFixture is the test data set ST-ASR-TD01: a proof run attestation as GitHub keeps it, and the
// public-good Sigstore trust root of the same day.
type sigstoreFixture struct {
	Commit      string          `json:"commit"`
	TrustedRoot json.RawMessage `json:"trustedRoot"`
	Bundle      json.RawMessage `json:"bundle"`
}

func readSigstoreFixture(t *testing.T) sigstoreFixture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "sigstore-proof-run.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture sigstoreFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// edit changes the JSON document at the path of keys and indexes with change, and returns the new document.
func edit(t *testing.T, document []byte, change func(value any) any, path ...any) []byte {
	t.Helper()
	var root any
	if err := json.Unmarshal(document, &root); err != nil {
		t.Fatal(err)
	}
	var walk func(node any, path []any) any
	walk = func(node any, path []any) any {
		if len(path) == 0 {
			return change(node)
		}
		switch key := path[0].(type) {
		case string:
			object := node.(map[string]any)
			object[key] = walk(object[key], path[1:])
			return object
		case int:
			list := node.([]any)
			list[key] = walk(list[key], path[1:])
			return list
		}
		t.Fatalf("bad path element %v", path[0])
		return nil
	}
	data, err := json.Marshal(walk(root, path))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// flipBase64 changes one byte of base64 text in the middle, and keeps it base64.
func flipBase64(t *testing.T) func(any) any {
	return func(value any) any {
		raw, err := base64.StdEncoding.DecodeString(value.(string))
		if err != nil {
			t.Fatal(err)
		}
		raw[len(raw)/2] ^= 0x01
		return base64.StdEncoding.EncodeToString(raw)
	}
}

func TestVerify(t *testing.T) {
	commit := strings.Repeat("ab", 20)
	other := strings.Repeat("cd", 20)
	trusted := TrustedSigner{Issuer: GitHubIssuer, Workflow: trustedWorkflow}

	t.Run("ST-ASR-SP02-T An attestation is verified only for the commit, the proof run type and the trusted signer at a grade the change allows", func(t *testing.T) {
		clean := Trust{Signer: trusted, ChangeRef: changeRef}
		altered := Trust{Signer: trusted, ChangeRef: changeRef, WorkflowChanges: []string{".github/workflows/checks.yml", ".github/workflows/proof-run.yml"}}
		for name, check := range map[string]struct {
			trust Trust
			grade TrustGrade
		}{"an unchanged workflows folder": {clean, ReleaseGrade}, "a changed workflows folder": {altered, ReleaseGrade}} {
			grade, err := attestationVerified(proofRunStatement(t, commit, MainRef, 1), commit, check.trust)
			if err != nil || grade != check.grade {
				t.Fatalf("signed at main with %s: grade %q, %v; want release grade", name, grade, err)
			}
		}
		if grade, err := attestationVerified(proofRunStatement(t, commit, changeRef, 1), commit, clean); err != nil || grade != ChangeGrade {
			t.Fatalf("signed at the change's own reference with an unchanged workflows folder: grade %q, %v; want change grade", grade, err)
		}
		grade, err := attestationVerified(proofRunStatement(t, commit, changeRef, 1), commit, altered)
		var refusal *Refusal
		if !errors.As(err, &refusal) || refusal.Rule != "attestationVerified" || grade != ChangeGrade {
			t.Fatalf("signed at the change's own reference with a changed workflows folder: grade %q, %v; want a refusal of the change grade", grade, err)
		}
		for _, file := range altered.WorkflowChanges {
			if !strings.Contains(refusal.Reason, file) {
				t.Fatalf("the refusal does not name %s: %s", file, refusal.Reason)
			}
		}

		refused := map[string]func(*SignedStatement){
			"a subject digest of another commit": func(s *SignedStatement) { s.Subjects[0].Digest = SubjectDigest(other) },
			"a subject name of another commit":   func(s *SignedStatement) { s.Subjects[0].Name = "git+https://github.com/jsell-rh/stego@" + other },
			"a second subject": func(s *SignedStatement) {
				s.Subjects = append(s.Subjects, StatementSubject{Name: "git+https://github.com/jsell-rh/stego@" + other, Digest: SubjectDigest(other)})
			},
			"no subject":             func(s *SignedStatement) { s.Subjects = nil },
			"another predicate type": func(s *SignedStatement) { s.PredicateType = "https://slsa.dev/provenance/v1" },
			"a proof run about another commit": func(s *SignedStatement) {
				s.Predicate = bytes.Replace(s.Predicate, []byte(commit), []byte(other), 1)
			},
			"a predicate that is not JSON": func(s *SignedStatement) { s.Predicate = []byte("not JSON") },
			"another workflow": func(s *SignedStatement) {
				s.Signer.Subject = "https://github.com/jsell-rh/stego/.github/workflows/checks.yml@" + MainRef
			},
			"another repository's workflow": func(s *SignedStatement) {
				s.Signer.Subject = "https://github.com/someone/stego/.github/workflows/proof-run.yml@" + MainRef
			},
			"another issuer":                        func(s *SignedStatement) { s.Signer.Issuer = "https://accounts.google.com" },
			"another pull request's ref":            func(s *SignedStatement) { s.Signer.Subject = trustedWorkflow + "@refs/pull/8/merge" },
			"a branch that is not main":             func(s *SignedStatement) { s.Signer.Subject = trustedWorkflow + "@refs/heads/feature" },
			"a tag":                                 func(s *SignedStatement) { s.Signer.Subject = trustedWorkflow + "@refs/tags/v1.0.0" },
			"no reference":                          func(s *SignedStatement) { s.Signer.Subject = trustedWorkflow + "@" },
			"another source repository":             func(s *SignedStatement) { s.SourceRepository = "https://github.com/someone/stego" },
			"a source ref that is not the signer's": func(s *SignedStatement) { s.SourceRef = "refs/heads/feature" },
			"a run at main at another commit":       func(s *SignedStatement) { s.SourceDigest = other },
			"a run at main at no commit":            func(s *SignedStatement) { s.SourceDigest = "" },
		}
		for name, change := range refused {
			statement := proofRunStatement(t, commit, MainRef, 1)
			if strings.Contains(name, "pull request") || strings.Contains(name, "branch") || strings.Contains(name, "tag") || name == "no reference" {
				statement = proofRunStatement(t, commit, changeRef, 1)
			}
			change(&statement)
			grade, err := attestationVerified(statement, commit, clean)
			if !errors.As(err, &refusal) || refusal.Rule != "attestationVerified" || grade != NoGrade {
				t.Fatalf("%s: grade %q, %v; want a refusal with no grade", name, grade, err)
			}
		}
		merged := proofRunStatement(t, commit, changeRef, 1)
		merged.SourceDigest = other
		if grade, err := attestationVerified(merged, commit, clean); err != nil || grade != ChangeGrade {
			t.Fatalf("signed at the change's own reference by a run at another commit: grade %q, %v; want change grade", grade, err)
		}
		if grade, err := attestationVerified(proofRunStatement(t, commit, changeRef, 1), commit, Trust{Signer: trusted}); err == nil || grade != NoGrade {
			t.Fatalf("a pull request's signature counts with no change reference to gate: grade %q", grade)
		}
		if grade, err := attestationVerified(proofRunStatement(t, commit, MainRef, 1), commit, Trust{Signer: trusted, ChangeRef: MainRef}); err != nil || grade != ReleaseGrade {
			t.Fatalf("main as the change's reference: grade %q, %v; want release grade", grade, err)
		}
	})

	t.Run("ST-ASR-DS03-T01 A proof run attestation from GitHub's store verifies against the Sigstore trust root, and any altered part yields no attestation", func(t *testing.T) {
		fixture := readSigstoreFixture(t)
		verifier, err := NewSigstoreVerifier(fixture.TrustedRoot)
		if err != nil {
			t.Fatal(err)
		}
		trust := Trust{Signer: trusted, ChangeRef: "refs/pull/24/merge"}
		store := &memoryStore{bundles: map[string][][]byte{fixture.Commit: {fixture.Bundle}}}
		verification, err := VerifyAttestation(fixture.Commit, store, verifier, trust)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(store.asked, []string{fixture.Commit}) {
			t.Fatalf("the store was asked for %v, want the commit", store.asked)
		}
		if verification.Proof == nil {
			t.Fatalf("the attestation does not verify: %v", verification.Refused)
		}
		proof := verification.Proof
		want := Attestation{
			PredicateType: ProofRunPredicateType,
			Subject:       fixture.Commit,
			Signer:        Signer{Issuer: GitHubIssuer, Subject: trustedWorkflow + "@refs/pull/24/merge"},
			LogEntry:      "rekor:3614924958",
		}
		if proof.Attestation != want || proof.Grade != ChangeGrade || proof.Run.Commit != fixture.Commit || len(proof.Run.Outcomes) != 239 {
			t.Fatalf("the verified proof run is %+v (grade %s, %d outcomes), want %+v", proof.Attestation, proof.Grade, len(proof.Run.Outcomes), want)
		}
		// The certificate names the commit the signing run ran at, which the release grade checks.
		if statement, err := verifier.VerifyBundle(fixture.Bundle, SubjectDigest(fixture.Commit)); err != nil || !fullCommit(statement.SourceDigest) {
			t.Fatalf("the certificate's source digest is %q (%v), not a full commit", statement.SourceDigest, err)
		}
		for _, outcome := range proof.Run.Outcomes {
			if outcome.TestRecord == "ST-ASR-UC01-FL-T" && (outcome.Result != Passed || outcome.File != "cmd/assure/flow_test.go") {
				t.Fatalf("the proof run's outcome of ST-ASR-UC01-FL-T is %+v", outcome)
			}
		}

		bundle := []byte(fixture.Bundle)
		payload := func(value any) any {
			raw, err := base64.StdEncoding.DecodeString(value.(string))
			if err != nil {
				t.Fatal(err)
			}
			changed := bytes.Replace(raw, []byte(`"result":"passed"`), []byte(`"result":"failed"`), 1)
			if bytes.Equal(changed, raw) {
				t.Fatal("the payload has no passed outcome to change")
			}
			return base64.StdEncoding.EncodeToString(changed)
		}
		var parsed struct {
			VerificationMaterial struct {
				Certificate any `json:"certificate"`
			} `json:"verificationMaterial"`
		}
		if err := json.Unmarshal(bundle, &parsed); err != nil {
			t.Fatal(err)
		}
		// In the other trust root, each certificate authority's chain is the bundle's own leaf certificate, which
		// is not a certificate authority.
		otherRoot := edit(t, fixture.TrustedRoot, func(value any) any {
			for _, authority := range value.([]any) {
				authority.(map[string]any)["certChain"] = map[string]any{"certificates": []any{parsed.VerificationMaterial.Certificate}}
			}
			return value
		}, "certificateAuthorities")
		otherVerifier, err := NewSigstoreVerifier(otherRoot)
		if err != nil {
			t.Fatal(err)
		}
		for name, check := range map[string]struct {
			bundle   []byte
			verifier BundleVerifier
			commit   string
		}{
			"a payload changed after signing": {edit(t, bundle, payload, "dsseEnvelope", "payload"), verifier, fixture.Commit},
			"a changed signature":             {edit(t, bundle, flipBase64(t), "dsseEnvelope", "signatures", 0, "sig"), verifier, fixture.Commit},
			"a changed certificate":           {edit(t, bundle, flipBase64(t), "verificationMaterial", "certificate", "rawBytes"), verifier, fixture.Commit},
			"a changed inclusion proof":       {edit(t, bundle, flipBase64(t), "verificationMaterial", "tlogEntries", 0, "inclusionProof", "hashes", 0), verifier, fixture.Commit},
			"a changed log entry body":        {edit(t, bundle, flipBase64(t), "verificationMaterial", "tlogEntries", 0, "canonicalizedBody"), verifier, fixture.Commit},
			"no log entry":                    {edit(t, bundle, func(any) any { return []any{} }, "verificationMaterial", "tlogEntries"), verifier, fixture.Commit},
			"another trust root":              {bundle, otherVerifier, fixture.Commit},
			"another commit asked about":      {bundle, verifier, strings.Repeat("ef", 20)},
			"no bundle":                       {nil, verifier, fixture.Commit},
			"a bundle that is not JSON":       {[]byte("{"), verifier, fixture.Commit},
		} {
			if statement, err := check.verifier.VerifyBundle(check.bundle, SubjectDigest(check.commit)); err == nil || !reflect.DeepEqual(statement, SignedStatement{}) {
				t.Fatalf("%s: VerifyBundle gives %+v, %v; want nothing and an error", name, statement, err)
			} else {
				t.Logf("%s: %v", name, err)
			}
			store := &memoryStore{bundles: map[string][][]byte{check.commit: {check.bundle}}}
			verification, err := VerifyAttestation(check.commit, store, check.verifier, trust)
			if err != nil || verification.Proof != nil || len(verification.Refused) != 1 {
				t.Fatalf("%s: VerifyAttestation gives %+v, %v; want no proof run and one refusal", name, verification, err)
			}
		}

		forged := edit(t, bundle, flipBase64(t), "dsseEnvelope", "signatures", 0, "sig")
		store = &memoryStore{bundles: map[string][][]byte{fixture.Commit: {forged, fixture.Bundle, nil}}}
		verification, err = VerifyAttestation(fixture.Commit, store, verifier, trust)
		if err != nil || verification.Proof == nil || verification.Proof.Attestation != want || len(verification.Refused) != 2 {
			t.Fatalf("with a forged, a true and an empty bundle: %+v, %v; want the true one and two refusals", verification, err)
		}
		verification, err = VerifyAttestation(fixture.Commit, &memoryStore{bundles: map[string][][]byte{fixture.Commit: {fixture.Bundle}}}, verifier,
			Trust{Signer: trusted, ChangeRef: "refs/pull/25/merge"})
		if err != nil || verification.Proof != nil || len(verification.Refused) != 1 || !strings.Contains(verification.Refused[0], "refs/pull/24/merge") {
			t.Fatalf("signed at another pull request's ref: %+v, %v; want no proof run", verification, err)
		}
	})

	t.Run("ST-ASR-DS03-T02 Of several verified proof runs the release-grade one and then the newest is taken in any order, and a store that cannot answer is an error", func(t *testing.T) {
		trust := Trust{Signer: trusted, ChangeRef: changeRef}
		oldChange := bundleOf(t, proofRunStatement(t, commit, changeRef, 10, "ST-FIX-INV01-T failed"))
		newChange := bundleOf(t, proofRunStatement(t, commit, changeRef, 30, "ST-FIX-INV01-T passed"))
		release := bundleOf(t, proofRunStatement(t, commit, MainRef, 20, "ST-FIX-INV01-T skipped"))
		forged := proofRunStatement(t, commit, MainRef, 40, "ST-FIX-INV01-T passed")
		forged.Signer.Subject = "forged"
		for _, order := range [][][]byte{{oldChange, newChange, release, bundleOf(t, forged)}, {bundleOf(t, forged), release, newChange, oldChange}} {
			verification, err := VerifyAttestation(commit, &memoryStore{bundles: map[string][][]byte{commit: order}}, statementVerifier{}, trust)
			if err != nil || verification.Proof == nil || verification.Proof.Attestation.LogEntry != "rekor:20" || verification.Proof.Grade != ReleaseGrade {
				t.Fatalf("got %+v, %v; want the release-grade proof run at rekor:20", verification, err)
			}
			if verification.Proof.Run.Outcomes[0].Result != Skipped || len(verification.Refused) != 1 {
				t.Fatalf("got %+v; want its own outcome, skipped, and the forged one refused", verification)
			}
		}
		for _, order := range [][][]byte{{oldChange, newChange}, {newChange, oldChange}} {
			verification, err := VerifyAttestation(commit, &memoryStore{bundles: map[string][][]byte{commit: order}}, statementVerifier{}, trust)
			if err != nil || verification.Proof == nil || verification.Proof.Attestation.LogEntry != "rekor:30" {
				t.Fatalf("got %+v, %v; want the newest change-grade proof run at rekor:30", verification, err)
			}
		}
		broken := proofRunStatement(t, commit, MainRef, 50)
		broken.Predicate = []byte(`{"commit":"` + commit + `","outcomes":[{"testRecord":"ST-FIX-INV01-T","result":"ok"}]}`)
		verification, err := VerifyAttestation(commit, &memoryStore{bundles: map[string][][]byte{commit: {bundleOf(t, broken)}}}, statementVerifier{}, trust)
		if err != nil || verification.Proof != nil || len(verification.Refused) != 1 {
			t.Fatalf("a predicate that is not a whole proof run: %+v, %v; want no proof run", verification, err)
		}
		if _, err := VerifyAttestation(commit, &memoryStore{err: errors.New("GitHub answers 502")}, statementVerifier{}, trust); err == nil {
			t.Fatal("a store that cannot answer gives no error")
		}
		if verification, err := VerifyAttestation(commit, &memoryStore{}, statementVerifier{}, trust); err != nil || verification.Proof != nil || len(verification.Refused) != 0 {
			t.Fatalf("a store with no attestation: %+v, %v; want no proof run and no refusal", verification, err)
		}
		if _, err := VerifyAttestation(commit[:7], &memoryStore{}, statementVerifier{}, trust); err == nil {
			t.Fatal("a short commit ID is verified")
		}
	})
}
