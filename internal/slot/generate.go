package slot

import (
	"bytes"
	"fmt"
	"go/format"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
)

// GenerateInterface generates a gen.File containing Go interfaces and
// supporting types derived from parsed proto files. The generated code
// represents the slot contract that fills must implement.
//
// filePath is the output file path relative to the project output root.
// pkgName is the Go package name for the generated file.
// slot is the parsed slot proto file containing service and message definitions.
// imports are parsed proto files for imported packages (e.g. stego.common).
//
// For each service in the slot proto, a Go interface is generated with methods
// corresponding to the service's RPCs. For each message (both local and
// imported), a Go struct is generated with fields matching the message fields.
// Fully-qualified proto types are resolved to their Go type names.
//
// The returned gen.File stores the formatted source in Content (without the
// generated-file header). Use File.Bytes() to obtain the complete output
// including the mandatory header.
func GenerateInterface(filePath, pkgName string, slot *ProtoFile, imports []*ProtoFile) (gen.File, error) {
	return GenerateInterfaceExcluding(filePath, pkgName, slot, imports, nil)
}

// GenerateInterfaceExcluding is like GenerateInterface but skips emitting
// types listed in excludeTypes. This is used when multiple slot files are
// generated into the same package to avoid duplicate type declarations for
// shared imported types (e.g. SlotResult, Identity).
func GenerateInterfaceExcluding(filePath, pkgName string, slot *ProtoFile, imports []*ProtoFile, excludeTypes map[string]bool) (gen.File, error) {
	if pkgName == "" {
		return gen.File{}, fmt.Errorf("pkgName must not be empty")
	}
	if slot == nil {
		return gen.File{}, fmt.Errorf("slot proto must not be nil")
	}
	if len(slot.Services) == 0 {
		return gen.File{}, fmt.Errorf("slot proto has no service definitions")
	}

	// Build a combined message map: qualified name -> Message.
	allMessages := make(map[string]Message)
	for _, imp := range imports {
		for _, msg := range imp.Messages {
			qualified := imp.Package + "." + msg.Name
			allMessages[qualified] = msg
			// Also index by simple name for local resolution.
			allMessages[msg.Name] = msg
		}
	}
	for _, msg := range slot.Messages {
		qualified := slot.Package + "." + msg.Name
		allMessages[qualified] = msg
		allMessages[msg.Name] = msg
	}

	// Collect all types referenced in service RPCs.
	referenced := CollectAllReferencedTypes(slot, allMessages)

	var buf bytes.Buffer

	fmt.Fprintf(&buf, "package %s\n\n", pkgName)

	buf.WriteString("import \"context\"\n\n")

	// Generate structs for all referenced messages (imports first, then local).
	// Skip types already emitted in another file within the same package.
	emitted := make(map[string]bool)
	for _, imp := range imports {
		for _, msg := range imp.Messages {
			goName := msg.Name
			if !referenced[goName] || emitted[goName] {
				continue
			}
			emitted[goName] = true
			if excludeTypes != nil && excludeTypes[goName] {
				continue
			}
			writeMessageStruct(&buf, msg)
		}
	}
	for _, msg := range slot.Messages {
		goName := msg.Name
		if emitted[goName] {
			continue
		}
		emitted[goName] = true
		if excludeTypes != nil && excludeTypes[goName] {
			continue
		}
		writeMessageStruct(&buf, msg)
	}

	// Generate interfaces from services.
	for _, svc := range slot.Services {
		fmt.Fprintf(&buf, "// %sSlot is the interface that fills implement for the %s slot.\n", svc.Name, svc.Name)
		fmt.Fprintf(&buf, "type %sSlot interface {\n", svc.Name)
		for _, m := range svc.Methods {
			inputType := resolveGoType(m.InputType)
			outputType := resolveGoType(m.OutputType)
			fmt.Fprintf(&buf, "\t%s(ctx context.Context, req *%s) (*%s, error)\n",
				m.Name, inputType, outputType)
		}
		buf.WriteString("}\n\n")
	}

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return gen.File{}, fmt.Errorf("formatting generated code: %w (raw:\n%s)", err, buf.String())
	}

	return gen.File{
		Path:    filePath,
		Content: formatted,
	}, nil
}

// writeMessageStruct writes a Go struct definition for a proto message.
func writeMessageStruct(buf *bytes.Buffer, msg Message) {
	fmt.Fprintf(buf, "// %s is generated from proto message %s.\n", msg.Name, msg.Name)
	fmt.Fprintf(buf, "type %s struct {\n", msg.Name)
	for _, f := range msg.Fields {
		goType := protoTypeToGo(f.Type)
		goName := protoFieldToGoName(f.Name)
		fmt.Fprintf(buf, "\t%s %s\n", goName, goType)
	}
	buf.WriteString("}\n\n")
}

// CollectAllReferencedTypes walks service RPC signatures and message fields
// recursively to find all types that need Go struct definitions.
func CollectAllReferencedTypes(proto *ProtoFile, allMessages map[string]Message) map[string]bool {
	result := make(map[string]bool)

	// Seed from service RPC signatures.
	var queue []string
	for _, svc := range proto.Services {
		for _, m := range svc.Methods {
			queue = append(queue, m.InputType, m.OutputType)
		}
	}

	// Walk transitively through message fields.
	for len(queue) > 0 {
		typeName := queue[0]
		queue = queue[1:]

		goName := resolveGoType(typeName)
		if result[goName] {
			continue
		}
		if isPrimitiveProtoType(typeName) {
			continue
		}

		result[goName] = true

		// Look up this message and enqueue its field types.
		if msg, ok := allMessages[typeName]; ok {
			for _, f := range msg.Fields {
				ft := f.Type
				// Strip map wrapper.
				if strings.HasPrefix(ft, "map<") {
					inner := ft[4 : len(ft)-1]
					parts := strings.SplitN(inner, ",", 2)
					for _, p := range parts {
						queue = append(queue, strings.TrimSpace(p))
					}
					continue
				}
				queue = append(queue, ft)
			}
		} else if msg, ok := allMessages[goName]; ok {
			for _, f := range msg.Fields {
				queue = append(queue, f.Type)
			}
		}
	}

	return result
}

// isPrimitiveProtoType returns true for protobuf scalar types.
func isPrimitiveProtoType(t string) bool {
	switch t {
	case "string", "int32", "int64", "float", "double", "bool", "bytes":
		return true
	}
	return false
}

// protoTypeToGo converts a protobuf field type to a Go type.
func protoTypeToGo(protoType string) string {
	if strings.HasPrefix(protoType, "map<") {
		inner := protoType[4 : len(protoType)-1]
		parts := strings.SplitN(inner, ",", 2)
		if len(parts) == 2 {
			k := protoTypeToGo(strings.TrimSpace(parts[0]))
			v := protoTypeToGo(strings.TrimSpace(parts[1]))
			return fmt.Sprintf("map[%s]%s", k, v)
		}
	}

	switch protoType {
	case "string":
		return "string"
	case "int32":
		return "int32"
	case "int64":
		return "int64"
	case "float":
		return "float32"
	case "double":
		return "float64"
	case "bool":
		return "bool"
	case "bytes":
		return "[]byte"
	default:
		return "*" + resolveGoType(protoType)
	}
}

// resolveGoType converts a possibly-qualified proto type name to a Go type name.
func resolveGoType(protoType string) string {
	if idx := strings.LastIndex(protoType, "."); idx >= 0 {
		return protoType[idx+1:]
	}
	return protoType
}

// protoFieldToGoName converts a snake_case proto field name to PascalCase Go name.
func protoFieldToGoName(name string) string {
	parts := strings.Split(name, "_")
	var result strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		upper := strings.ToUpper(p)
		switch upper {
		case "ID", "URL", "HTTP", "API", "RPC", "IP":
			result.WriteString(upper)
		default:
			result.WriteString(strings.ToUpper(p[:1]) + p[1:])
		}
	}
	return result.String()
}
