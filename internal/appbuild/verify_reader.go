package appbuild

import (
	"errors"
	"strings"
)

// Verification is the read model of a check of an executable or a source
// snapshot against a trusted build record (ST-BLD-RM01): the record's digest,
// whether every digest matched, and each input or output that differs.
type Verification struct {
	RecordDigest string   `json:"record_digest"`
	Matches      bool     `json:"matches"`
	Mismatches   []string `json:"mismatches,omitempty"`
}

// VerifyReader holds the verification that one run built in memory and hands
// it to the query handler. The handler reads nothing else, and nothing is kept
// between runs.
type VerifyReader struct {
	verification Verification
}

// newVerifyReader keeps the result of one check: the trusted record's digest
// and each difference the check found, in the order it found them.
func newVerifyReader(recordDigest string, mismatches []string) *VerifyReader {
	return &VerifyReader{Verification{RecordDigest: recordDigest, Matches: len(mismatches) == 0, Mismatches: append([]string(nil), mismatches...)}}
}

// Verification gives the verification the reader holds.
func (reader *VerifyReader) Verification() Verification {
	verification := reader.verification
	verification.Mismatches = append([]string(nil), verification.Mismatches...)
	return verification
}

// failure is nil when the check matched, and otherwise an error that names
// every difference.
func (reader *VerifyReader) failure(subject string) error {
	if reader.verification.Matches {
		return nil
	}
	return errors.New(subject + " differs from the build record " + reader.verification.RecordDigest + ":\n  " + strings.Join(reader.verification.Mismatches, "\n  "))
}
