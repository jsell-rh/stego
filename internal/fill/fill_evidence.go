package fill

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jsell-rh/stego/internal/buildidentity"
)

// FillEvidence is the record, in the fill manifest, that the fill's tests
// passed against one exact state of the fill and of its slot contract
// (ST-EXT-EN02). Files holds the digest of each file of the fill as the tests
// found it, so that a report can name the file that changed after the run.
type FillEvidence struct {
	FillDigest     string               `yaml:"fill_digest" json:"fill_digest"`
	ContractDigest string               `yaml:"contract_digest" json:"contract_digest"`
	PassedAt       time.Time            `yaml:"passed_at" json:"passed_at"`
	Compiler       buildidentity.Record `yaml:"compiler" json:"compiler"`
	Files          map[string]string    `yaml:"files" json:"files"`
}

// check refuses evidence with a missing or malformed digest, and names the
// field.
func (e *FillEvidence) check() error {
	if e.FillDigest == "" {
		return errors.New("missing required field: fill_digest")
	}
	if !validDigest(e.FillDigest) {
		return fmt.Errorf("fill_digest %q is not 64 lower-case hexadecimal digits", e.FillDigest)
	}
	if e.ContractDigest == "" {
		return errors.New("missing required field: contract_digest")
	}
	if !validDigest(e.ContractDigest) {
		return fmt.Errorf("contract_digest %q is not 64 lower-case hexadecimal digits", e.ContractDigest)
	}
	if e.PassedAt.IsZero() {
		return errors.New("missing required field: passed_at")
	}
	return nil
}

// requireTests refuses evidence for a fill with no test file: a Go file
// whose name ends in _test.go, as go test reads it (ST-EXT-INV04).
func requireTests(current Current) error {
	if !current.HasTests {
		return errors.New("it has no test file (a file whose name ends in _test.go)")
	}
	return nil
}

// EvidenceDigest is the digest that a sign-off signs: the SHA-256 digest of
// the fill's name, slot and collection and of each field of its evidence, in
// a fixed form. Another fill or other evidence gives another digest.
func EvidenceDigest(f *Fill) (string, error) {
	if f == nil || f.Evidence == nil {
		return "", errors.New("the fill has no evidence")
	}
	statement := struct {
		Kind       string        `json:"kind"`
		Name       string        `json:"name"`
		Implements string        `json:"implements"`
		Collection string        `json:"collection"`
		Evidence   *FillEvidence `json:"evidence"`
	}{"stego-fill-evidence/v1", f.Name, f.Implements, f.Collection, f.Evidence}
	data, err := json.Marshal(statement)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// WriteEvidence writes the evidence into the fill manifest at path, in place
// of the evidence it held, and leaves the rest of the manifest as it was.
func WriteEvidence(path string, evidence FillEvidence) error {
	if err := evidence.check(); err != nil {
		return err
	}
	return writeSection(path, "evidence", evidence)
}

// validDigest tells if a digest is 64 lower-case hexadecimal digits.
func validDigest(digest string) bool {
	if len(digest) != 64 {
		return false
	}
	for _, c := range digest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
