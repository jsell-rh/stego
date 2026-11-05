package registry

import (
	"bytes"
	"regexp"
)

// fileIsStatic evaluates ST-SUP-SP06 for one emitted file rendered for two
// declarations that differ in every input the component reads: the file is
// static when the two renders give the same bytes once the module path and
// the Go package name of each declaration are normalised. Normalising
// replaces only those two names, and nothing else, such as white space.
func fileIsStatic(first, second []byte, modules, packages [2]string) bool {
	return bytes.Equal(normaliseRender(first, modules[0], packages[0]), normaliseRender(second, modules[1], packages[1]))
}

// normaliseRender replaces the module path, then the package name as a whole
// identifier, with fixed markers.
func normaliseRender(data []byte, module, pkg string) []byte {
	if module != "" {
		data = bytes.ReplaceAll(data, []byte(module), []byte("\x00module\x00"))
	}
	if pkg != "" {
		data = regexp.MustCompile(`\b`+regexp.QuoteMeta(pkg)+`\b`).ReplaceAll(data, []byte("\x00package\x00"))
	}
	return data
}
