package appbuild

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
		return nil, errors.New("source snapshot must be a real directory")
	}
	actual, files, err := inventory(source, maxFiles, maxBytes)
	if err != nil {
		return nil, errors.New("source snapshot contains invalid or excessive inputs")
	}
	if actual != record.Source || !reflect.DeepEqual(files, record.Inputs) {
		return nil, errors.New("source snapshot differs from the build record")
	}
	return record, nil
}
