// Package appbuild builds applications from recorded inputs. Records are not signatures.
package appbuild

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const maxFiles = 20000
const maxBytes int64 = 512 << 20
const maxFileBytes int64 = 128 << 20

type File struct {
	Path       string `json:"path"`
	SHA256     string `json:"sha256"`
	Executable bool   `json:"executable"`
}

type Inventory struct {
	SHA256 string `json:"sha256"`
	Files  int    `json:"files"`
	Bytes  int64  `json:"bytes"`
}

func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func safePath(name string) bool {
	if name == "" || name == "." || len(name) > 4096 || !utf8.ValidString(name) || path.Clean(name) != name || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\:") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." || part == ".git" {
			return false
		}
	}
	for _, ch := range name {
		if ch < 32 || ch == 127 {
			return false
		}
	}
	return true
}

func fileDigest(name string) (string, int64, error) {
	info, err := os.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxFileBytes {
		return "", 0, errors.New("build input must be a bounded regular file")
	}
	f, err := openInput(name)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return "", 0, errors.New("build input changed before reading")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxFileBytes+1))
	if err != nil || n != info.Size() || n > maxFileBytes {
		return "", 0, errors.New("build input changed size")
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// inventory uses the same ordered tuple encoding as the compiler SDK check.
func inventory(root string, fileLimit int, byteLimit int64) (Inventory, []File, error) {
	var result Inventory
	var files []File
	entries := 0
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == root {
			return nil
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		entries++
		if entries > 200000 || len(strings.Split(relative, "/")) > 64 || !safePath(relative) {
			return errors.New("invalid build inventory path or count")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("build input contains a link or special file")
		}
		hash, size, err := fileDigest(name)
		if err != nil {
			return err
		}
		result.Bytes += size
		if len(files) >= fileLimit || result.Bytes > byteLimit {
			return errors.New("build inventory exceeds its limits")
		}
		files = append(files, File{relative, hash, info.Mode().Perm()&0111 != 0})
		return nil
	})
	if err != nil {
		return Inventory{}, nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	tuples := make([][]any, 0, len(files))
	for _, f := range files {
		tuples = append(tuples, []any{f.Path, f.SHA256, f.Executable})
	}
	data, err := inventoryJSON(tuples)
	if err != nil {
		return Inventory{}, nil, err
	}
	result.Files, result.SHA256 = len(files), digest(data)
	return result, files, nil
}

// The established SDK inventory uses ASCII JSON escapes. Keep that byte
// format for non-ASCII filenames, including UTF-16 surrogate pairs.
func inventoryJSON(value [][]any) ([]byte, error) {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	data := bytes.TrimSuffix(encoded.Bytes(), []byte{'\n'})
	var result bytes.Buffer
	for _, r := range string(data) {
		if r < 128 {
			result.WriteByte(byte(r))
		} else if r <= 0xffff {
			fmt.Fprintf(&result, `\u%04x`, r)
		} else {
			high, low := utf16.EncodeRune(r)
			fmt.Fprintf(&result, `\u%04x\u%04x`, high, low)
		}
	}
	return result.Bytes(), nil
}

// unpack accepts only directories and regular files in a new private snapshot.
// A Git archive can contain links or submodules; neither is a build input here.
func unpack(input io.Reader, destination string) error {
	root, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer root.Close()
	reader := tar.NewReader(input)
	seen := map[string]bool{}
	var total int64
	entries := 0
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if header.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name := strings.TrimSuffix(header.Name, "/")
		entries++
		if !safePath(name) || seen[name] || entries > 40000 || len(strings.Split(name, "/")) > 64 {
			return errors.New("invalid application archive path")
		}
		seen[name] = true
		switch header.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0700); err != nil {
				return err
			}
		case tar.TypeReg:
			if header.Size < 0 || header.Size > maxFileBytes {
				return errors.New("application archive file exceeds its limit")
			}
			total += header.Size
			if total > maxBytes {
				return errors.New("application archive exceeds its limit")
			}
			if err := root.MkdirAll(path.Dir(name), 0700); err != nil {
				return err
			}
			mode := fs.FileMode(0600)
			if header.Mode&0111 != 0 {
				mode = 0700
			}
			f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return err
			}
			n, copyErr := io.CopyN(f, reader, header.Size)
			closeErr := f.Close()
			if copyErr != nil || closeErr != nil || n != header.Size {
				return errors.New("cannot copy application archive file")
			}
		default:
			return errors.New("application archive contains a link or special file")
		}
	}
}
