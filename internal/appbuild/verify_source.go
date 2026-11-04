package appbuild

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// VerifySource compares a complete source snapshot with an authenticated record.
// The caller must authenticate the expected digest before this call. The source
// must stay unchanged while the caller uses the result. No source is executed.
func VerifySource(recordPath, expectedSHA256, source string) (*Record, error) {
	record, err := readBuildRecord(recordPath, expectedSHA256)
	if err != nil {
		return nil, err
	}
	if source == "" {
		return nil, errors.New("a source snapshot directory is required")
	}
	source = filepath.Clean(source)
	info, err := os.Lstat(source)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("cannot read the source snapshot %s as a real folder; the check failed", source)
	}
	_, files, err := inventory(source, maxFiles, maxBytes)
	if err != nil {
		return nil, fmt.Errorf("source snapshot %s holds an input that cannot be checked: %v", source, err)
	}
	reader := newVerifyReader(expectedSHA256, sourceMismatches(record.Inputs, files))
	if err := reader.failure("the source snapshot " + source); err != nil {
		return nil, err
	}
	return record, nil
}

// sourceMismatches names each path that the record lists and the snapshot
// lacks, each path the snapshot adds, and each path whose content or
// executable bit differs, in path order.
func sourceMismatches(recorded, found []File) []string {
	have := make(map[string]File, len(found))
	for _, file := range found {
		have[file.Path] = file
	}
	var mismatches []string
	listed := make(map[string]bool, len(recorded))
	for _, want := range recorded {
		listed[want.Path] = true
		got, present := have[want.Path]
		switch {
		case !present:
			mismatches = append(mismatches, want.Path+": missing")
		case got.SHA256 != want.SHA256:
			mismatches = append(mismatches, fmt.Sprintf("%s: changed, recorded %s, found %s", want.Path, want.SHA256, got.SHA256))
		case got.Executable != want.Executable:
			mismatches = append(mismatches, fmt.Sprintf("%s: executable bit recorded %t, found %t", want.Path, want.Executable, got.Executable))
		}
	}
	for _, got := range found {
		if !listed[got.Path] {
			mismatches = append(mismatches, got.Path+": added")
		}
	}
	sort.Strings(mismatches)
	return mismatches
}
