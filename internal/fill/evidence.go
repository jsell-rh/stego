package fill

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/jsell-rh/stego/internal/buildidentity"
	"github.com/jsell-rh/stego/internal/compiler"
)

// TestRun runs the tests of the fill in the folder and gives an error when
// a test fails or the tests do not run.
type TestRun func(fillDir string) error

// EvidenceResult is what recording test evidence did for one fill: the
// evidence it wrote, or why it wrote none.
type EvidenceResult struct {
	Fill   string
	Passed *FillTestsPassed
	// Kept tells that the fill's evidence already named its current state,
	// so the passing run kept it, and its sign-off with it.
	Kept   bool
	Failed bool
	Reason string
}

// RecordEvidence runs the tests of each fill of the project and, for each
// fill whose tests pass, writes its evidence into its fill manifest, with the
// fill digest and the contract digest taken before the run (ST-EXT-CMD02).
// It writes nothing for a fill whose manifest it cannot read against the
// declaration, for a fill with no test file or with an import it may not
// have (ST-EXT-INV05), and for a fill whose tests fail
// or whose files change during the run, and names why. Evidence that names
// the fill's current state stays as it is, with its sign-off, when the tests
// pass again. The results are in the order of the fill names.
func RecordEvidence(check compiler.FillCheck, run TestRun, build buildidentity.Record, now func() time.Time) ([]EvidenceResult, error) {
	names, err := fillFolders(check.ProjectDir)
	if err != nil {
		return nil, err
	}
	var results []EvidenceResult
	for _, name := range names {
		results = append(results, recordOne(check, name, run, build, now))
	}
	return results, nil
}

// recordOne records the evidence of one fill.
func recordOne(check compiler.FillCheck, name string, run TestRun, build buildidentity.Record, now func() time.Time) EvidenceResult {
	result := EvidenceResult{Fill: name}
	dir := filepath.Join(check.ProjectDir, "fills", name)
	manifest := filepath.Join(dir, ManifestFile)
	f, err := ReadManifest(manifest, check)
	if err != nil {
		result.Reason = err.Error()
		return result
	}
	before, err := CurrentState(dir, f, check)
	if err != nil {
		result.Reason = err.Error()
		return result
	}
	if err := requireTests(before); err != nil {
		result.Reason = err.Error()
		return result
	}
	if err := checkImports(dir, check); err != nil {
		result.Reason = err.Error()
		return result
	}
	if err := run(dir); err != nil {
		result.Failed = true
		result.Reason = fmt.Sprintf("its tests failed: %v", err)
		return result
	}
	after, err := CurrentState(dir, f, check)
	if err != nil {
		result.Reason = err.Error()
		return result
	}
	if after.FillDigest != before.FillDigest || after.ContractDigest != before.ContractDigest {
		result.Reason = "its files or its slot contract changed while its tests ran"
		return result
	}
	if f.Evidence != nil && f.Evidence.FillDigest == before.FillDigest && f.Evidence.ContractDigest == before.ContractDigest {
		result.Kept = true
		result.Passed = &FillTestsPassed{FillName: name, FillDigest: before.FillDigest, ContractDigest: before.ContractDigest}
		return result
	}
	evidence := FillEvidence{
		FillDigest:     before.FillDigest,
		ContractDigest: before.ContractDigest,
		PassedAt:       now().UTC().Truncate(time.Second),
		Compiler:       build,
		Files:          before.Files,
	}
	if err := WriteEvidence(manifest, evidence); err != nil {
		result.Reason = fmt.Sprintf("writing its evidence: %v", err)
		return result
	}
	result.Passed = &FillTestsPassed{FillName: name, FillDigest: evidence.FillDigest, ContractDigest: evidence.ContractDigest}
	return result
}
