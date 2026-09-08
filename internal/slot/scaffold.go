package slot

import (
	"bytes"
	"fmt"
	"go/format"
	"go/token"

	"golang.org/x/mod/module"
)

// GenerateFill creates application-owned source that uses the canonical slot
// contracts. Unfinished methods return an error and cannot grant access.
func GenerateFill(packageName, contractsPath string, proto *ProtoFile) ([]byte, error) {
	if !token.IsIdentifier(packageName) || packageName == "_" || packageName == "main" {
		return nil, fmt.Errorf("invalid fill package name %q", packageName)
	}
	if err := module.CheckImportPath(contractsPath); err != nil {
		return nil, fmt.Errorf("invalid slot package path: %w", err)
	}
	if proto == nil || len(proto.Services) == 0 {
		return nil, fmt.Errorf("slot proto has no service definitions")
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "package %s\n\n", packageName)
	fmt.Fprintf(&buf, "import (\n\"context\"\n\"errors\"\nslots %q\n)\n\n", contractsPath)
	buf.WriteString("// ErrNotImplemented prevents use of an unfinished fill.\n")
	buf.WriteString("var ErrNotImplemented = errors.New(\"fill is not implemented\")\n\n")
	buf.WriteString("// Fill implements the application's slot rules.\ntype Fill struct {}\n\n")
	buf.WriteString("// New creates a fill.\nfunc New() *Fill { return &Fill{} }\n\n")
	methods := make(map[string]Method)
	for _, service := range proto.Services {
		if !token.IsIdentifier(service.Name) || len(service.Methods) == 0 {
			return nil, fmt.Errorf("invalid or empty slot service %q", service.Name)
		}
		fmt.Fprintf(&buf, "var _ slots.%sSlot = (*Fill)(nil)\n\n", service.Name)
		for _, method := range service.Methods {
			if previous, exists := methods[method.Name]; exists {
				if previous != method {
					return nil, fmt.Errorf("slot services have conflicting method %q", method.Name)
				}
				continue
			}
			input, output := resolveGoType(method.InputType), resolveGoType(method.OutputType)
			if !token.IsIdentifier(method.Name) || !token.IsIdentifier(input) || !token.IsIdentifier(output) || method.Name == "_" {
				return nil, fmt.Errorf("invalid slot method %q", method.Name)
			}
			methods[method.Name] = method
			fmt.Fprintf(&buf, "// %s must be implemented before this fill is used.\n", method.Name)
			fmt.Fprintf(&buf, "func (f *Fill) %s(ctx context.Context, req *slots.%s) (*slots.%s, error) {\n", method.Name, input, output)
			buf.WriteString("return nil, ErrNotImplemented\n}\n\n")
		}
	}
	return format.Source(buf.Bytes())
}
