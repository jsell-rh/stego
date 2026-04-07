package slot

import (
	"bytes"
	"fmt"
	"go/format"

	"github.com/jsell-rh/stego/internal/gen"
)

// GenerateOperators generates gate, chain, and fan-out operator wrapper types
// for each service defined in the slot proto file. The generated types
// implement the corresponding slot interface (e.g. BeforeCreateSlot) and can
// be used to compose multiple fills into a single slot implementation.
//
// Gate: all fills must return Ok for the operation to proceed.
// Chain: fills are called sequentially; stops on first non-Ok result.
//
//	When ShortCircuit is true, a Halt result also stops the chain
//	and returns the halted result with its status code.
//
// FanOut: all fills are called concurrently. Any failure fails the whole.
//
// The generated code belongs in the same package as the generated interface
// (from GenerateInterface), since it references the interface type, request
// types, and SlotResult.
func GenerateOperators(filePath, pkgName string, proto *ProtoFile) (gen.File, error) {
	if pkgName == "" {
		return gen.File{}, fmt.Errorf("pkgName must not be empty")
	}
	if proto == nil {
		return gen.File{}, fmt.Errorf("proto must not be nil")
	}
	if len(proto.Services) == 0 {
		return gen.File{}, fmt.Errorf("proto has no service definitions")
	}

	var buf bytes.Buffer

	fmt.Fprintf(&buf, "package %s\n\n", pkgName)
	buf.WriteString("import \"context\"\n\n")

	for _, svc := range proto.Services {
		ifaceName := svc.Name + "Slot"

		writeGateOperator(&buf, svc, ifaceName)
		writeChainOperator(&buf, svc, ifaceName)
		writeFanOutOperator(&buf, svc, ifaceName)
	}

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return gen.File{}, fmt.Errorf("formatting operators: %w (raw:\n%s)", err, buf.String())
	}

	return gen.File{
		Path:    filePath,
		Content: formatted,
	}, nil
}

func writeGateOperator(buf *bytes.Buffer, svc Service, ifaceName string) {
	structName := svc.Name + "Gate"

	fmt.Fprintf(buf, "// %s wraps multiple %s fills as a gate.\n", structName, ifaceName)
	fmt.Fprintf(buf, "// All fills must return Ok for the operation to proceed.\n")
	fmt.Fprintf(buf, "type %s struct {\n\tfills []%s\n}\n\n", structName, ifaceName)

	fmt.Fprintf(buf, "// New%s creates a gate operator that requires all fills to return Ok.\n", structName)
	fmt.Fprintf(buf, "func New%s(fills ...%s) *%s {\n", structName, ifaceName, structName)
	fmt.Fprintf(buf, "\treturn &%s{fills: fills}\n", structName)
	fmt.Fprintf(buf, "}\n\n")

	for _, m := range svc.Methods {
		inputType := resolveGoType(m.InputType)
		outputType := resolveGoType(m.OutputType)

		fmt.Fprintf(buf, "func (g *%s) %s(ctx context.Context, req *%s) (*%s, error) {\n",
			structName, m.Name, inputType, outputType)
		fmt.Fprintf(buf, "\tfor _, f := range g.fills {\n")
		fmt.Fprintf(buf, "\t\tresult, err := f.%s(ctx, req)\n", m.Name)
		fmt.Fprintf(buf, "\t\tif err != nil {\n\t\t\treturn nil, err\n\t\t}\n")
		fmt.Fprintf(buf, "\t\tif !result.Ok {\n\t\t\treturn result, nil\n\t\t}\n")
		fmt.Fprintf(buf, "\t}\n")
		fmt.Fprintf(buf, "\treturn &%s{Ok: true}, nil\n", outputType)
		fmt.Fprintf(buf, "}\n\n")
	}
}

func writeChainOperator(buf *bytes.Buffer, svc Service, ifaceName string) {
	structName := svc.Name + "Chain"

	fmt.Fprintf(buf, "// %s wraps multiple %s fills as a sequential chain.\n", structName, ifaceName)
	fmt.Fprintf(buf, "// Fills are called in order. Stops on first non-Ok result.\n")
	fmt.Fprintf(buf, "// When ShortCircuit is true, a Halt result also stops the chain\n")
	fmt.Fprintf(buf, "// and returns the result with its status code.\n")
	fmt.Fprintf(buf, "type %s struct {\n\tfills []%s\n\tShortCircuit bool\n}\n\n", structName, ifaceName)

	fmt.Fprintf(buf, "// New%s creates a chain operator with optional short-circuit halt.\n", structName)
	fmt.Fprintf(buf, "func New%s(shortCircuit bool, fills ...%s) *%s {\n", structName, ifaceName, structName)
	fmt.Fprintf(buf, "\treturn &%s{fills: fills, ShortCircuit: shortCircuit}\n", structName)
	fmt.Fprintf(buf, "}\n\n")

	for _, m := range svc.Methods {
		inputType := resolveGoType(m.InputType)
		outputType := resolveGoType(m.OutputType)

		fmt.Fprintf(buf, "func (c *%s) %s(ctx context.Context, req *%s) (*%s, error) {\n",
			structName, m.Name, inputType, outputType)
		fmt.Fprintf(buf, "\tvar lastResult *%s\n", outputType)
		fmt.Fprintf(buf, "\tfor _, f := range c.fills {\n")
		fmt.Fprintf(buf, "\t\tresult, err := f.%s(ctx, req)\n", m.Name)
		fmt.Fprintf(buf, "\t\tif err != nil {\n\t\t\treturn nil, err\n\t\t}\n")
		fmt.Fprintf(buf, "\t\tlastResult = result\n")
		fmt.Fprintf(buf, "\t\tif !result.Ok {\n\t\t\treturn result, nil\n\t\t}\n")
		fmt.Fprintf(buf, "\t\tif c.ShortCircuit && result.Halt {\n\t\t\treturn result, nil\n\t\t}\n")
		fmt.Fprintf(buf, "\t}\n")
		fmt.Fprintf(buf, "\tif lastResult == nil {\n\t\treturn &%s{Ok: true}, nil\n\t}\n", outputType)
		fmt.Fprintf(buf, "\treturn lastResult, nil\n")
		fmt.Fprintf(buf, "}\n\n")
	}
}

func writeFanOutOperator(buf *bytes.Buffer, svc Service, ifaceName string) {
	structName := svc.Name + "FanOut"

	fmt.Fprintf(buf, "// %s wraps multiple %s fills for concurrent execution.\n", structName, ifaceName)
	fmt.Fprintf(buf, "// All fills are called concurrently. Any failure fails the whole operation.\n")
	fmt.Fprintf(buf, "type %s struct {\n\tfills []%s\n}\n\n", structName, ifaceName)

	fmt.Fprintf(buf, "// New%s creates a fan-out operator for concurrent fill execution.\n", structName)
	fmt.Fprintf(buf, "func New%s(fills ...%s) *%s {\n", structName, ifaceName, structName)
	fmt.Fprintf(buf, "\treturn &%s{fills: fills}\n", structName)
	fmt.Fprintf(buf, "}\n\n")

	for _, m := range svc.Methods {
		inputType := resolveGoType(m.InputType)
		outputType := resolveGoType(m.OutputType)

		fmt.Fprintf(buf, "func (fo *%s) %s(ctx context.Context, req *%s) (*%s, error) {\n",
			structName, m.Name, inputType, outputType)
		fmt.Fprintf(buf, "\tif len(fo.fills) == 0 {\n")
		fmt.Fprintf(buf, "\t\treturn &%s{Ok: true}, nil\n", outputType)
		fmt.Fprintf(buf, "\t}\n")
		fmt.Fprintf(buf, "\ttype fanOutResult struct {\n\t\tres *%s\n\t\terr error\n\t}\n", outputType)
		fmt.Fprintf(buf, "\tch := make(chan fanOutResult, len(fo.fills))\n")
		fmt.Fprintf(buf, "\tfor _, fill := range fo.fills {\n")
		fmt.Fprintf(buf, "\t\tgo func(f %s) {\n", ifaceName)
		fmt.Fprintf(buf, "\t\t\tr, err := f.%s(ctx, req)\n", m.Name)
		fmt.Fprintf(buf, "\t\t\tch <- fanOutResult{r, err}\n")
		fmt.Fprintf(buf, "\t\t}(fill)\n")
		fmt.Fprintf(buf, "\t}\n")
		fmt.Fprintf(buf, "\tvar firstErr error\n")
		fmt.Fprintf(buf, "\tvar firstFailure *%s\n", outputType)
		fmt.Fprintf(buf, "\tfor range fo.fills {\n")
		fmt.Fprintf(buf, "\t\tr := <-ch\n")
		fmt.Fprintf(buf, "\t\tif r.err != nil && firstErr == nil {\n")
		fmt.Fprintf(buf, "\t\t\tfirstErr = r.err\n")
		fmt.Fprintf(buf, "\t\t}\n")
		fmt.Fprintf(buf, "\t\tif r.res != nil && !r.res.Ok && firstFailure == nil {\n")
		fmt.Fprintf(buf, "\t\t\tfirstFailure = r.res\n")
		fmt.Fprintf(buf, "\t\t}\n")
		fmt.Fprintf(buf, "\t}\n")
		fmt.Fprintf(buf, "\tif firstErr != nil {\n\t\treturn nil, firstErr\n\t}\n")
		fmt.Fprintf(buf, "\tif firstFailure != nil {\n\t\treturn firstFailure, nil\n\t}\n")
		fmt.Fprintf(buf, "\treturn &%s{Ok: true}, nil\n", outputType)
		fmt.Fprintf(buf, "}\n\n")
	}
}
