package gen

const DefaultInputBytes int64 = 1 << 20
const MaxInputBytes int64 = 4 << 20

// InputFile declares a source and its read limit. Generators select this limit;
// service configuration cannot override it. The compiler checks the path,
// limit, total size, and file identity before generation and apply.
type InputFile struct {
	Path     string
	MaxBytes int64
}

// SourceInputs keeps the 1 MiB limit for protocol and callback source files.
func SourceInputs(names []string) []InputFile {
	result := make([]InputFile, len(names))
	for i, name := range names {
		result[i] = InputFile{Path: name, MaxBytes: DefaultInputBytes}
	}
	return result
}
