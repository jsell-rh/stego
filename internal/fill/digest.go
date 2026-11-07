package fill

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Digest is the fill digest (ST-EXT-DS01): the SHA-256 digest of every file
// in the fill's folder, by its path in the folder and its content, in the
// order of the paths. The fill manifest counts without its evidence and its
// sign-off, so recording either never changes the digest. It refuses a
// folder that holds a symbolic link or another file that is not regular.
func Digest(dir string) (string, error) {
	files, err := FileDigests(dir)
	if err != nil {
		return "", err
	}
	return combine(files), nil
}

// FileDigests gives the digest of each file in the fill's folder, by its
// slash-separated path in the folder, as Digest counts it.
func FileDigests(dir string) (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(dir, name)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if !entry.Type().IsRegular() {
			return fmt.Errorf("fill file %s is not a regular file", relative)
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		if relative == ManifestFile {
			data = manifestContent(data)
		}
		sum := sha256.Sum256(data)
		files[relative] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// manifestContent is the fill manifest as the digest counts it: the YAML
// document with its evidence and its sign-off left out, written again in one
// form. A manifest that is not a YAML mapping counts as its bytes.
func manifestContent(data []byte) []byte {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return data
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return data
	}
	removeKey(document.Content[0], "evidence")
	removeKey(document.Content[0], "sign_off")
	out, err := encodeNode(&document)
	if err != nil {
		return data
	}
	return out
}

// combine is the digest of a set of named contents: each name and the digest
// of its content, with their lengths, in the order of the names.
func combine(files map[string]string) string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	hash := sha256.New()
	for _, name := range names {
		fmt.Fprintf(hash, "%s:%s\x00%s\n", strconv.Itoa(len(name)), name, files[name])
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// ContractFiles reads a slot contract and the files it imports from the
// active registry. The registry gives both methods.
type ContractFiles interface {
	ReadFile(name string) ([]byte, error)
	ReadProtoImport(name string) ([]byte, error)
}

// protoImport matches one import statement of a .proto file.
var protoImport = regexp.MustCompile(`(?m)^\s*import\s+(?:public\s+|weak\s+)?"([^"]+)"\s*;`)

// ContractDigest is the slot contract digest (ST-EXT-DS01): the SHA-256
// digest of the slot's .proto file at contract, a path in the registry, and of
// each .proto file that it imports, directly or through another import, in
// the order of their names. No other file of the registry counts.
func ContractDigest(files ContractFiles, contract string) (string, error) {
	data, err := files.ReadFile(contract)
	if err != nil {
		return "", fmt.Errorf("slot contract %s: %w", contract, err)
	}
	contents := map[string]string{}
	add := func(name string, data []byte) {
		sum := sha256.Sum256(data)
		contents[name] = hex.EncodeToString(sum[:])
	}
	add(contract, data)
	seen := map[string]bool{}
	pending := importsOf(data)
	for len(pending) > 0 {
		name := pending[0]
		pending = pending[1:]
		if seen[name] {
			continue
		}
		seen[name] = true
		imported, err := files.ReadProtoImport(name)
		if err != nil {
			return "", fmt.Errorf("slot contract %s imports %s: %w", contract, name, err)
		}
		add("import:"+path.Clean(name), imported)
		pending = append(pending, importsOf(imported)...)
	}
	return combine(contents), nil
}

// importsOf gives the import paths of a .proto file, with its comments left
// out.
func importsOf(data []byte) []string {
	var imports []string
	for _, match := range protoImport.FindAllSubmatch(stripProtoComments(data), -1) {
		imports = append(imports, string(match[1]))
	}
	return imports
}

// stripProtoComments removes the line and block comments of a .proto file,
// outside its string literals.
func stripProtoComments(data []byte) []byte {
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); i++ {
		switch {
		case data[i] == '"' || data[i] == '\'':
			quote := data[i]
			out = append(out, data[i])
			for i++; i < len(data); i++ {
				out = append(out, data[i])
				if data[i] == '\\' && i+1 < len(data) {
					i++
					out = append(out, data[i])
					continue
				}
				if data[i] == quote {
					break
				}
			}
		case data[i] == '/' && i+1 < len(data) && data[i+1] == '/':
			for i < len(data) && data[i] != '\n' {
				i++
			}
			if i < len(data) {
				out = append(out, '\n')
			}
		case data[i] == '/' && i+1 < len(data) && data[i+1] == '*':
			for i += 2; i+1 < len(data) && !(data[i] == '*' && data[i+1] == '/'); i++ {
				if data[i] == '\n' {
					out = append(out, '\n')
				}
			}
			i++
		default:
			out = append(out, data[i])
		}
	}
	return out
}
