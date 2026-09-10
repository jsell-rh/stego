package registry

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/parser"
)

const maxRegistryBytes = 64 << 20
const maxRegistryFiles = 4096
const maxRegistryEntries = 16384
const maxRegistryDepth = 16

type registryFile struct {
	data []byte
	hash [sha256.Size]byte
	size int64
}
type registrySnapshot struct {
	dir         string
	files       map[string]registryFile
	directories map[string]bool
	hash        string
}

// Capture only YAML and protobuf input files. Other regular files are not
// compiler inputs. Git metadata is excluded. All reads remain inside the root.
func captureRegistry(dir string, keepData bool) (*registrySnapshot, error) {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("registry root must be a directory, without a symbolic link")
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	snapshot := &registrySnapshot{dir: absolute, files: map[string]registryFile{}, directories: map[string]bool{}}
	entries, total := 0, int64(0)
	buffer := make([]byte, 32<<10)
	var walk func(string, int) error
	walk = func(name string, depth int) error {
		if depth > maxRegistryDepth {
			return fmt.Errorf("registry exceeds directory depth %d", maxRegistryDepth)
		}
		directory, err := root.Open(name)
		if err != nil {
			return err
		}
		defer directory.Close()
		for {
			batch, readErr := directory.ReadDir(128)
			for _, entry := range batch {
				entries++
				if entries > maxRegistryEntries {
					return fmt.Errorf("registry exceeds %d directory entries", maxRegistryEntries)
				}
				if name == "." && entry.Name() == ".git" {
					continue
				}
				relative := path.Join(name, entry.Name())
				if len(relative) > 1024 {
					return fmt.Errorf("registry path exceeds 1024 bytes")
				}
				if err := gen.ValidatePath(relative); err != nil {
					return err
				}
				info, err := entry.Info()
				if err != nil {
					return err
				}
				if info.Mode()&os.ModeSymlink != 0 {
					return fmt.Errorf("registry symbolic link is not allowed: %s", relative)
				}
				if name == "." && (relative == "components" || relative == "archetypes" || relative == "mixins") && !info.IsDir() {
					return fmt.Errorf("registry category %s must be a directory", relative)
				}
				if info.IsDir() {
					snapshot.directories[relative] = true
					if err := walk(relative, depth+1); err != nil {
						return err
					}
					continue
				}
				if !info.Mode().IsRegular() {
					return fmt.Errorf("registry input must be a regular file: %s", relative)
				}
				if extension := path.Ext(relative); extension != ".yaml" && extension != ".proto" {
					continue
				}
				if len(snapshot.files) >= maxRegistryFiles {
					return fmt.Errorf("registry exceeds %d input files", maxRegistryFiles)
				}
				file, err := root.Open(relative)
				if err != nil {
					return err
				}
				record, err := readRegistryFile(file, keepData, min(int64(parser.MaxDocumentBytes), maxRegistryBytes-total), buffer)
				file.Close()
				if err != nil {
					return fmt.Errorf("registry input %s: %w", relative, err)
				}
				total += record.size
				snapshot.files[relative] = record
			}
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			if readErr != nil {
				return readErr
			}
		}
	}
	if err := walk(".", 0); err != nil {
		return nil, err
	}
	digest := sha256.New()
	io.WriteString(digest, "stego-registry-content-v1\x00")
	names := slices.Sorted(maps.Keys(snapshot.files))
	var length [8]byte
	for _, name := range names {
		record := snapshot.files[name]
		binary.BigEndian.PutUint64(length[:], uint64(len(name)))
		digest.Write(length[:])
		io.WriteString(digest, name)
		binary.BigEndian.PutUint64(length[:], uint64(record.size))
		digest.Write(length[:])
		digest.Write(record.hash[:])
	}
	snapshot.hash = hex.EncodeToString(digest.Sum(nil))
	return snapshot, nil
}

func readRegistryFile(file *os.File, keepData bool, limit int64, buffer []byte) (registryFile, error) {
	info, err := file.Stat()
	if err != nil {
		return registryFile{}, err
	}
	if !info.Mode().IsRegular() {
		return registryFile{}, errors.New("input must be a regular file")
	}
	if info.Size() > limit {
		return registryFile{}, errors.New("input exceeds the 4 MiB file or 64 MiB registry limit")
	}
	digest := sha256.New()
	var data bytes.Buffer
	var output io.Writer = digest
	if keepData {
		data.Grow(int(info.Size()))
		output = io.MultiWriter(digest, &data)
	}
	count, err := io.CopyBuffer(output, io.LimitReader(file, limit+1), buffer)
	if err != nil {
		return registryFile{}, err
	}
	if count > limit {
		return registryFile{}, errors.New("input exceeds the 4 MiB file or 64 MiB registry limit")
	}
	var sum [sha256.Size]byte
	copy(sum[:], digest.Sum(sum[:0]))
	return registryFile{data: data.Bytes(), hash: sum, size: count}, nil
}

// ContentHash identifies the captured YAML and protobuf inputs, including names.
// It does not identify compiler code, fills, or application dependencies.
func (r *Registry) ContentHash() string { return r.source.hash }

// ReadFile returns a copy of a captured registry input. It does not read disk.
func (r *Registry) ReadFile(name string) ([]byte, error) {
	if err := gen.ValidatePath(name); err != nil {
		return nil, err
	}
	record, ok := r.source.files[name]
	if !ok {
		return nil, fmt.Errorf("registry input %s: %w", name, os.ErrNotExist)
	}
	return bytes.Clone(record.data), nil
}

// ReadProtoImport resolves the optional stego/ prefix inside the captured tree.
func (r *Registry) ReadProtoImport(name string) ([]byte, error) {
	if err := gen.ValidatePath(name); err != nil {
		return nil, err
	}
	if path.Ext(name) != ".proto" {
		return nil, fmt.Errorf("registry import must be a protobuf file: %s", name)
	}
	trimmed := strings.TrimPrefix(name, "stego/")
	if _, ok := r.source.files[trimmed]; ok {
		return r.ReadFile(trimmed)
	}
	return r.ReadFile(name)
}

// Verify checks the current tree against the captured inputs. It also detects
// directory changes, including an added artifact directory with no YAML file.
func (r *Registry) Verify() error {
	current, err := captureRegistry(r.source.dir, false)
	if err != nil {
		return fmt.Errorf("registry changed after planning; run plan again: %w", err)
	}
	if current.hash != r.source.hash || !maps.Equal(current.directories, r.source.directories) {
		return errors.New("registry changed after planning; run plan again")
	}
	return nil
}

func (r *Registry) childDirectories(category string) []string {
	var names []string
	for name := range r.source.directories {
		if path.Dir(name) == category {
			names = append(names, path.Base(name))
		}
	}
	slices.Sort(names)
	return names
}
