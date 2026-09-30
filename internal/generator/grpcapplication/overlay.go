package grpcapplication

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/bufbuild/protocompile"
	"github.com/bufbuild/protocompile/linker"
	"github.com/jsell-rh/stego/internal/gen"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// overlays returns every declared source that pins a frozen reference
// contract. The declared source must conservatively extend that contract:
// reference fields stay unchanged or move into declared retirements, and
// fork-added fields never reuse a reference wire number. This replaces the
// hand-mirrored descriptor tests that fork consumers previously maintained.
func overlays(ctx gen.Context, items []source) ([]source, error) {
	var result []source
	imports := map[string]bool{}
	for _, item := range items {
		imports[item.Import] = true
	}
	for _, item := range items {
		if item.Reference == "" {
			continue
		}
		if _, ok := ctx.Inputs[item.Reference]; !ok {
			return nil, fmt.Errorf("missing reference input %q", item.Reference)
		}
		if imports[item.Reference] || item.Reference == item.File {
			return nil, fmt.Errorf("reference %q must not equal a declared file or import_path", item.Reference)
		}
		result = append(result, item)
	}
	return result, nil
}

// compileReference compiles a frozen reference under the import path of the
// overlay that extends it. Sibling declarations supply shared imports such as
// common.proto, so the resolver map contains every declared text plus the
// reference text under the overlay's own import path.
func compileReference(ctx gen.Context, items []source, overlay source) (protoreflect.FileDescriptor, error) {
	text := map[string]string{}
	for _, item := range items {
		data, ok := ctx.Inputs[item.File]
		if !ok {
			return nil, fmt.Errorf("missing compiler input %q", item.File)
		}
		text[item.Import] = string(data)
	}
	data, ok := ctx.Inputs[overlay.Reference]
	if !ok {
		return nil, fmt.Errorf("missing reference input %q", overlay.Reference)
	}
	text[overlay.Import] = string(data)
	compiler := protocompile.Compiler{Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{Accessor: protocompile.SourceAccessorFromMap(text)}), MaxParallelism: 1, SourceInfoMode: protocompile.SourceInfoNone}
	compileContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	linked, err := compiler.Compile(compileContext, overlay.Import)
	if err != nil {
		return nil, fmt.Errorf("reference %s: %w", overlay.Reference, err)
	}
	return linked[0], nil
}

// checkOverlays validates every declared overlay against its frozen
// reference. prepareProto calls it so plan and apply enforce the same rule.
func checkOverlays(ctx gen.Context, items []source, linked linker.Files) error {
	refs, err := overlays(ctx, items)
	if err != nil {
		return err
	}
	if len(refs) == 0 {
		return nil
	}
	byImport := map[string]protoreflect.FileDescriptor{}
	for _, file := range linked {
		byImport[file.Path()] = file
	}
	for _, overlay := range refs {
		reference, err := compileReference(ctx, items, overlay)
		if err != nil {
			return err
		}
		fork, ok := byImport[overlay.Import]
		if !ok {
			return fmt.Errorf("reference overlay %s: declared file did not compile", overlay.Import)
		}
		if err := validateOverlay(overlay, fork, reference); err != nil {
			return err
		}
	}
	return nil
}

// validateOverlay checks that fork conservatively extends reference. New
// top-level messages, enums, and services are free additions.
func validateOverlay(overlay source, fork, reference protoreflect.FileDescriptor) error {
	prefix := "reference overlay " + overlay.Import + ": "
	messages := reference.Messages()
	for i := 0; i < messages.Len(); i++ {
		refMsg := messages.Get(i)
		forkMsg := fork.Messages().ByName(refMsg.Name())
		if forkMsg == nil {
			return fmt.Errorf("%smessage %s removed from overlay", prefix, refMsg.FullName())
		}
		if err := validateMessage(prefix, forkMsg, refMsg); err != nil {
			return err
		}
	}
	enums := reference.Enums()
	for i := 0; i < enums.Len(); i++ {
		refEnum := enums.Get(i)
		forkEnum := fork.Enums().ByName(refEnum.Name())
		if forkEnum == nil {
			return fmt.Errorf("%senum %s removed from overlay", prefix, refEnum.FullName())
		}
		if err := validateEnum(prefix, forkEnum, refEnum); err != nil {
			return err
		}
	}
	services := reference.Services()
	for i := 0; i < services.Len(); i++ {
		refService := services.Get(i)
		forkService := fork.Services().ByName(refService.Name())
		if forkService == nil {
			return fmt.Errorf("%sservice %s removed from overlay", prefix, refService.FullName())
		}
		if err := validateService(prefix, forkService, refService); err != nil {
			return err
		}
	}
	return nil
}

func validateMessage(prefix string, forkMsg, refMsg protoreflect.MessageDescriptor) error {
	for i := 0; i < refMsg.Fields().Len(); i++ {
		refField := refMsg.Fields().Get(i)
		forkField := forkMsg.Fields().ByName(refField.Name())
		if forkField == nil {
			// A removed reference field must be retired in the fork itself:
			// its number reserved and its name reserved.
			if !forkMsg.ReservedRanges().Has(refField.Number()) || !forkMsg.ReservedNames().Has(refField.Name()) {
				return fmt.Errorf("%sfield %s removed without retirement", prefix, string(refField.FullName()))
			}
			continue
		}
		if forkField.Number() != refField.Number() || forkField.Kind() != refField.Kind() || forkField.Cardinality() != refField.Cardinality() || forkField.IsList() != refField.IsList() || forkField.IsMap() != refField.IsMap() {
			return fmt.Errorf("%sfield %s changed on the wire", prefix, string(refField.FullName()))
		}
		if refField.Kind() == protoreflect.MessageKind && forkField.Message().FullName() != refField.Message().FullName() {
			return fmt.Errorf("%sfield %s changed type on the wire", prefix, string(refField.FullName()))
		}
		if refField.Kind() == protoreflect.EnumKind && forkField.Enum().FullName() != refField.Enum().FullName() {
			return fmt.Errorf("%sfield %s changed type on the wire", prefix, string(refField.FullName()))
		}
	}
	for i := 0; i < forkMsg.Fields().Len(); i++ {
		forkField := forkMsg.Fields().Get(i)
		if refMsg.Fields().ByName(forkField.Name()) != nil {
			continue
		}
		// A fork-added field must not reuse a reference wire number. Numbers
		// the reference reserved for future use are equally off limits.
		if refMsg.Fields().ByNumber(forkField.Number()) != nil || refMsg.ReservedRanges().Has(forkField.Number()) {
			return fmt.Errorf("%sfield %s reuses reference wire number %d", prefix, string(forkField.FullName()), forkField.Number())
		}
	}
	nested := refMsg.Messages()
	for i := 0; i < nested.Len(); i++ {
		refNested := nested.Get(i)
		forkNested := forkMsg.Messages().ByName(refNested.Name())
		if forkNested == nil {
			return fmt.Errorf("%smessage %s removed from overlay", prefix, string(refNested.FullName()))
		}
		if err := validateMessage(prefix, forkNested, refNested); err != nil {
			return err
		}
	}
	refEnums := refMsg.Enums()
	for i := 0; i < refEnums.Len(); i++ {
		refEnum := refEnums.Get(i)
		forkEnum := forkMsg.Enums().ByName(refEnum.Name())
		if forkEnum == nil {
			return fmt.Errorf("%senum %s removed from overlay", prefix, string(refEnum.FullName()))
		}
		if err := validateEnum(prefix, forkEnum, refEnum); err != nil {
			return err
		}
	}
	return nil
}

func validateEnum(prefix string, forkEnum, refEnum protoreflect.EnumDescriptor) error {
	for i := 0; i < refEnum.Values().Len(); i++ {
		refValue := refEnum.Values().Get(i)
		forkValue := forkEnum.Values().ByName(refValue.Name())
		if forkValue == nil {
			if !forkEnum.ReservedRanges().Has(refValue.Number()) || !forkEnum.ReservedNames().Has(refValue.Name()) {
				return fmt.Errorf("%senum value %s removed without retirement", prefix, string(refValue.FullName()))
			}
			continue
		}
		if forkValue.Number() != refValue.Number() {
			return fmt.Errorf("%senum value %s changed on the wire", prefix, string(refValue.FullName()))
		}
	}
	for i := 0; i < forkEnum.Values().Len(); i++ {
		forkValue := forkEnum.Values().Get(i)
		if refEnum.Values().ByName(forkValue.Name()) != nil {
			continue
		}
		if refEnum.Values().ByNumber(forkValue.Number()) != nil || refEnum.ReservedRanges().Has(forkValue.Number()) {
			return fmt.Errorf("%senum value %s reuses reference number %d", prefix, string(forkValue.FullName()), forkValue.Number())
		}
	}
	return nil
}

func validateService(prefix string, forkService, refService protoreflect.ServiceDescriptor) error {
	for j := 0; j < refService.Methods().Len(); j++ {
		refMethod := refService.Methods().Get(j)
		forkMethod := forkService.Methods().ByName(refMethod.Name())
		if forkMethod == nil {
			return fmt.Errorf("%smethod %s removed from overlay", prefix, string(refMethod.FullName()))
		}
		if forkMethod.Input().FullName() != refMethod.Input().FullName() || forkMethod.Output().FullName() != refMethod.Output().FullName() {
			return fmt.Errorf("%smethod %s changed on the wire", prefix, string(refMethod.FullName()))
		}
	}
	return nil
}

// overlayTestFile pins the validated contract in a test emitted next to the
// generated protobuf code. The pinned bytes come from the same
// FileDescriptorProto that protoc-gen-go embeds as the runtime rawDesc, with
// GoPackage and SourceCodeInfo removed: those fields depend on local
// generation settings, not the wire contract.
func overlayTestFile(ctx gen.Context, overlay source, file *protogen.File) (*gen.File, error) {
	descriptor := proto.Clone(file.Proto).(*descriptorpb.FileDescriptorProto)
	if descriptor.Options != nil {
		descriptor.Options.GoPackage = nil
	}
	descriptor.SourceCodeInfo = nil
	data, err := proto.MarshalOptions{AllowPartial: true, Deterministic: true}.Marshal(descriptor)
	if err != nil {
		return nil, err
	}
	descriptorVar := file.GoDescriptorIdent.GoName
	var body bytes.Buffer
	fmt.Fprintf(&body, "package %s\n\n", file.GoPackageName)
	fmt.Fprintf(&body, "import (\n\t\"testing\"\n\n\t\"google.golang.org/protobuf/proto\"\n\t\"google.golang.org/protobuf/reflect/protodesc\"\n\t\"google.golang.org/protobuf/types/descriptorpb\"\n)\n\n")
	fmt.Fprintf(&body, "// %sValidatedDescriptor pins the wire contract that the stego compiler\n", descriptorVar)
	fmt.Fprintf(&body, "// validated against reference %s. It fails when the generated\n", overlay.Reference)
	fmt.Fprintf(&body, "// protobuf code no longer matches that contract.\n")
	fmt.Fprintf(&body, "const %sValidatedDescriptor = %s\n\n", descriptorVar, quoteDescriptor(data))
	fmt.Fprintf(&body, "func Test%sMatchesValidatedContract(t *testing.T) {\n", descriptorVar)
	fmt.Fprintf(&body, "\twant := new(descriptorpb.FileDescriptorProto)\n")
	fmt.Fprintf(&body, "\tif err := proto.Unmarshal([]byte(%sValidatedDescriptor), want); err != nil {\n", descriptorVar)
	fmt.Fprintf(&body, "\t\tt.Fatalf(\"validated descriptor bytes: %%v\", err)\n\t}\n")
	fmt.Fprintf(&body, "\thave := protodesc.ToFileDescriptorProto(%s)\n", descriptorVar)
	fmt.Fprintf(&body, "\tif have.Options == nil {\n\t\thave.Options = new(descriptorpb.FileOptions)\n\t}\n")
	fmt.Fprintf(&body, "\tif want.Options == nil {\n\t\twant.Options = new(descriptorpb.FileOptions)\n\t}\n")
	fmt.Fprintf(&body, "\thave.Options.GoPackage = nil\n\twant.Options.GoPackage = nil\n\thave.SourceCodeInfo = nil\n\twant.SourceCodeInfo = nil\n")
	fmt.Fprintf(&body, "\tif !proto.Equal(want, have) {\n")
	fmt.Fprintf(&body, "\t\tt.Fatal(\"generated descriptor does not match the stego-validated contract\")\n\t}\n}\n")
	name := strings.TrimSuffix(path.Base(overlay.Import), ".proto") + "_overlay_test.go"
	return &gen.File{Path: path.Join(ctx.OutputNamespace, "pb", path.Dir(overlay.Import), name), Content: body.Bytes()}, nil
}

// quoteDescriptor renders marshaled descriptor bytes as a Go string literal
// split into concatenated lines after each 0x0a byte, the same layout that
// protoc-gen-go uses for its embedded raw descriptors.
func quoteDescriptor(data []byte) string {
	var buf bytes.Buffer
	buf.WriteString(`""`)
	for _, line := range bytes.SplitAfter(data, []byte{0x0a}) {
		buf.WriteString(" +\n\t")
		buf.WriteString(strconv.Quote(string(line)))
	}
	return buf.String()
}
