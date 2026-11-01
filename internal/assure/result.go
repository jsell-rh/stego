// Package assure holds the Assuring domain (ST-ASR): the proof run that shows which tests ran and passed at an
// exact commit, kept only as an attestation at the CI provider.
package assure

import "fmt"

// Result is what one test did in one run (ST-VO54): passed, failed or skipped. Only passed counts as a pass.
type Result string

// The three test results.
const (
	Passed  Result = "passed"
	Failed  Result = "failed"
	Skipped Result = "skipped"
)

// ParseResult reads a test result. It refuses any text that is not exactly passed, failed or skipped
// (ST-VO54-C01).
func ParseResult(text string) (Result, error) {
	switch result := Result(text); result {
	case Passed, Failed, Skipped:
		return result, nil
	}
	return "", fmt.Errorf("test result %q is not one of passed, failed or skipped", text)
}

// Passes tells if the result counts as a pass. Only passed does (ST-VO54-C01).
func (r Result) Passes() bool {
	return r == Passed
}
