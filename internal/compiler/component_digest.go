package compiler

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"path"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// componentDigestDomain separates the component digest from every other
// SHA-256 digest that STEGO writes.
const componentDigestDomain = "stego-component-content-v1\x00"

// componentLifecycleKeys are the top-level keys of component.yaml that a
// digest leaves out: a promotion moves a component along its stages without
// a change to its content (ST-SUP-Q02, reading a).
var componentLifecycleKeys = map[string]bool{"stage": true, "sunset": true}

// ComponentDigest computes the digest of one component from the registry
// inputs under components/<name>/ alone, in byte order of their paths, with
// the stage and the sunset of component.yaml left out. files holds the
// registry's inputs by their path in the registry, as Registry.InputFiles
// and registry.CaptureInputFiles give them. The digest is 64 lowercase hex
// digits.
func ComponentDigest(files map[string][]byte, name string) (string, error) {
	if name == "" || strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return "", fmt.Errorf("component name %q is not a registry folder name", name)
	}
	prefix := path.Join("components", name) + "/"
	own := map[string][]byte{}
	for file, data := range files {
		if relative, ok := strings.CutPrefix(file, prefix); ok {
			own[relative] = data
		}
	}
	definition, ok := own["component.yaml"]
	if !ok {
		return "", fmt.Errorf("component %q: the registry holds no %scomponent.yaml", name, prefix)
	}
	content, err := withoutLifecycle(definition)
	if err != nil {
		return "", fmt.Errorf("component %q: %scomponent.yaml: %w", name, prefix, err)
	}
	own["component.yaml"] = content

	digest := sha256.New()
	io.WriteString(digest, componentDigestDomain)
	var length [8]byte
	for _, relative := range slices.Sorted(maps.Keys(own)) {
		sum := sha256.Sum256(own[relative])
		binary.BigEndian.PutUint64(length[:], uint64(len(relative)))
		digest.Write(length[:])
		io.WriteString(digest, relative)
		binary.BigEndian.PutUint64(length[:], uint64(len(own[relative])))
		digest.Write(length[:])
		digest.Write(sum[:])
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// withoutLifecycle removes the top-level stage and sunset of a component
// definition, and gives the rest in one canonical encoding, so the bytes
// depend on the content alone.
func withoutLifecycle(definition []byte) ([]byte, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(definition, &document); err != nil {
		return nil, err
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("the definition must be one YAML mapping")
	}
	mapping := document.Content[0]
	kept := mapping.Content[:0:0]
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if componentLifecycleKeys[mapping.Content[i].Value] {
			continue
		}
		kept = append(kept, mapping.Content[i], mapping.Content[i+1])
	}
	mapping.Content = kept
	var encoded bytes.Buffer
	encoder := yaml.NewEncoder(&encoded)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return encoded.Bytes(), nil
}
