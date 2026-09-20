package appbuild

import (
	"bytes"
	"crypto/sha1" // Git object names use SHA-1. Build records use SHA-256.
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

type sourceEntry struct {
	path, object string
	executable   bool
}

// An archive can apply export attributes from the repository. Check it against
// the raw tree so those attributes cannot remove or change a selected input.
func sourceTree(data []byte) ([]sourceEntry, error) {
	failure := errors.New("source tree must contain bounded regular files")
	if len(data) == 0 || data[len(data)-1] != 0 || len(data) > 8<<20 {
		return nil, failure
	}
	var entries []sourceEntry
	seen := map[string]bool{}
	for _, raw := range bytes.Split(data[:len(data)-1], []byte{0}) {
		header, name, ok := strings.Cut(string(raw), "\t")
		fields := strings.Split(header, " ")
		if !ok || !safePath(name) || len(strings.Split(name, "/")) > 64 || seen[name] || len(entries) >= maxFiles || len(fields) != 3 || fields[1] != "blob" || !revisionPattern.MatchString(fields[2]) || (fields[0] != "100644" && fields[0] != "100755") {
			return nil, failure
		}
		seen[name] = true
		entries = append(entries, sourceEntry{name, fields[2], fields[0] == "100755"})
	}
	return entries, nil
}

func verifySourceTree(root string, entries []sourceEntry, files []File) error {
	failure := errors.New("source archive differs from the selected Git tree")
	if len(entries) != len(files) {
		return failure
	}
	actual := map[string]File{}
	for _, file := range files {
		actual[file.Path] = file
	}
	for _, entry := range entries {
		file, present := actual[entry.path]
		if !present || file.Executable != entry.executable {
			return failure
		}
		object, err := sourceBlob(filepath.Join(root, filepath.FromSlash(entry.path)))
		if err != nil || object != entry.object {
			return failure
		}
	}
	return nil
}

func sourceBlob(name string) (string, error) {
	f, err := openInput(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxFileBytes {
		return "", errors.New("source object must be a bounded regular file")
	}
	hash := sha1.New()
	fmt.Fprintf(hash, "blob %d\x00", info.Size())
	count, err := io.Copy(hash, io.LimitReader(f, maxFileBytes+1))
	if err != nil || count != info.Size() {
		return "", errors.New("source object changed size")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
