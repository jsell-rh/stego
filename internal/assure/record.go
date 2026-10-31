package assure

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// Spec is what Record a proof run reads from the spec at the commit: the digest of its data files and its test
// records.
type Spec struct {
	Digest string
	Tests  map[string]TestRecord
}

// TestRecord is one test record of the spec: its ID, the file that runs it, and whether its state is automated.
type TestRecord struct {
	ID        string
	File      string
	Automated bool
}

// automated lists the IDs of the automated test records, in order.
func (s Spec) automated() []string {
	var ids []string
	for id, record := range s.Tests {
		if record.Automated {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// ReadSpec reads the spec's data files, the *.jsonl files in the folder dir of the tree fsys. Every *_test.jsonl
// file is a table of test records; a test record is automated when its state is automated. The digest is
// "sha256:" and the SHA-256 of the lines "<SHA-256 of the file>  <dir>/<name>", one per data file in name order,
// which is what sha256sum data/*.jsonl | sha256sum gives in the C locale.
func ReadSpec(fsys fs.FS, dir string) (Spec, error) {
	files, err := fs.Glob(fsys, path.Join(dir, "*.jsonl"))
	if err != nil {
		return Spec{}, err
	}
	if len(files) == 0 {
		return Spec{}, fmt.Errorf("the spec has no data files in %s", dir)
	}
	sort.Strings(files)
	spec := Spec{Tests: make(map[string]TestRecord)}
	var sums strings.Builder
	for _, file := range files {
		data, err := fs.ReadFile(fsys, file)
		if err != nil {
			return Spec{}, err
		}
		sum := sha256.Sum256(data)
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(sum[:]), file)
		if strings.HasSuffix(file, "_test.jsonl") {
			if err := readTestRecords(file, data, spec.Tests); err != nil {
				return Spec{}, err
			}
		}
	}
	digest := sha256.Sum256([]byte(sums.String()))
	spec.Digest = "sha256:" + hex.EncodeToString(digest[:])
	return spec, nil
}

// readTestRecords reads one table of test records, one JSON object per line, into tests.
func readTestRecords(file string, data []byte, tests map[string]TestRecord) error {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		var row struct {
			ID    string `json:"id"`
			File  string `json:"file"`
			State string `json:"state"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return fmt.Errorf("%s line %d: %w", file, line, err)
		}
		if row.ID == "" {
			return fmt.Errorf("%s line %d: a test record has no ID", file, line)
		}
		if _, ok := tests[row.ID]; ok {
			return fmt.Errorf("%s line %d: test record %s is in the spec twice", file, line, row.ID)
		}
		tests[row.ID] = TestRecord{ID: row.ID, File: row.File, Automated: row.State == "automated"}
	}
	return scanner.Err()
}

// RecordRequest is what Record a proof run reads: the commit CI checked out and the state of its tree, the spec
// at that commit, the suites' output, and the CI run with the identity its attestation action signs with.
type RecordRequest struct {
	Commit       string
	Tree         Checkout
	Spec         Spec
	GoTestJSON   io.Reader
	PythonJSON   io.Reader
	Provider     string
	RunReference string
	Signer       Signer
	Trusted      TrustedSigner
}

// RecordProofRun is the handler of ST-ASR-CMD01, Record a proof run. It builds the proof run from the suites'
// output at the commit CI checked out, fills in a failed outcome for every automated test record that did not
// run, refuses an outcome for an unknown ID, and hands the statement to the store for the provider to attest.
// When it refuses, the store gets nothing.
func RecordProofRun(request RecordRequest, store AttestationStore) (ProofRun, ProofRunAttested, error) {
	if err := atOneCleanCommit(request.Commit, request.Tree); err != nil {
		return ProofRun{}, ProofRunAttested{}, err
	}
	if !knownProvider(request.Provider) {
		return ProofRun{}, ProofRunAttested{}, fmt.Errorf("CI provider %q is not one of github-actions or konflux", request.Provider)
	}
	if request.RunReference == "" {
		return ProofRun{}, ProofRunAttested{}, errors.New("the proof run has no run reference")
	}
	suites, err := ReadOutcomes(request.GoTestJSON, request.PythonJSON)
	if err != nil {
		return ProofRun{}, ProofRunAttested{}, err
	}
	if err := outcomesNameKnownRecords(suites.Outcomes, request.Spec); err != nil {
		return ProofRun{}, ProofRunAttested{}, err
	}
	outcomes := outcomeForEveryAutomatedTest(suites.Outcomes, request.Spec)
	for i := range outcomes {
		outcomes[i].File = request.Spec.Tests[outcomes[i].TestRecord].File
	}
	run := ProofRun{
		Commit:       request.Commit,
		Provider:     request.Provider,
		RunReference: request.RunReference,
		StartedAt:    suites.StartedAt,
		FinishedAt:   suites.FinishedAt,
		SpecDigest:   request.Spec.Digest,
		Outcomes:     outcomes,
	}
	attestation, err := NewAttestation(ProofRunPredicateType, request.Commit, request.Signer, "")
	if err != nil {
		return ProofRun{}, ProofRunAttested{}, err
	}
	if err := attestedByTrustedSigner(run, attestation, request.Trusted); err != nil {
		return ProofRun{}, ProofRunAttested{}, err
	}
	run.Attestation = attestation
	digest, err := store.HandOver(run)
	if err != nil {
		return ProofRun{}, ProofRunAttested{}, err
	}
	return run, ProofRunAttested{
		Commit:          run.Commit,
		SpecDigest:      run.SpecDigest,
		Attestation:     attestation,
		PredicateDigest: digest,
	}, nil
}

// knownProvider tells if the provider is a CI provider (ST-VO51): github-actions or konflux.
func knownProvider(provider string) bool {
	switch provider {
	case "github-actions", "konflux":
		return true
	}
	return false
}
