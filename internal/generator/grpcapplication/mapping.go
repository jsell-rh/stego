package grpcapplication

import (
	"fmt"
	"go/token"
	"path"
	"sort"
	"strconv"
	"unicode/utf8"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type responseMapping struct {
	name   string
	source protogen.GoIdent
	target *protogen.Message
	fields []mappedField
}

type mappedField struct {
	target     *protogen.Field
	source     gen.GoModelField
	children   []mappedField
	constant   *string
	prefix     string
	conversion string
}

// responseMappings checks the complete public shape before any file is rendered.
// A nested message is present when its fields are mapped. Omitted fields retain
// their protobuf zero value. Optional source values cannot become required values.
func responseMappings(ctx gen.Context, plugin *protogen.Plugin) ([]responseMapping, error) {
	value, exists := ctx.ComponentConfig["response_mappings"]
	if !exists {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok || len(items) == 0 || len(items) > 128 {
		return nil, fmt.Errorf("response_mappings requires 1 to 128 mappings")
	}
	messages := map[string]*protogen.Message{}
	var collect func([]*protogen.Message)
	collect = func(list []*protogen.Message) {
		for _, message := range list {
			messages[string(message.Desc.FullName())] = message
			collect(message.Messages)
		}
	}
	for _, file := range plugin.Files {
		if file.Generate {
			collect(file.Messages)
		}
	}
	result := make([]responseMapping, 0, len(items))
	names := map[string]bool{"ErrConversion": true}
	for _, value := range items {
		item, ok := value.(map[string]any)
		if !ok || len(item) != 5 {
			return nil, fmt.Errorf("response mapping requires name, provider, model, message, and fields")
		}
		name, n := item["name"].(string)
		provider, p := item["provider"].(string)
		modelName, m := item["model"].(string)
		messageName, q := item["message"].(string)
		fields, f := item["fields"].([]any)
		if !n || !p || !m || !q || !f || !token.IsIdentifier(name) || !token.IsExported(name) || names[name] || len(fields) == 0 || len(fields) > 512 {
			return nil, fmt.Errorf("invalid or duplicate response mapping")
		}
		names[name] = true
		source, exists := ctx.GoModelSources[provider]
		if !exists || gen.ValidateGoImportNamespace(source.ImportPath) != nil || gen.ValidateGoModels(source.Models) != nil {
			return nil, fmt.Errorf("response mapping %q has no valid model provider", name)
		}
		var model *gen.GoModel
		for i := range source.Models {
			if source.Models[i].Name == modelName {
				model = &source.Models[i]
			}
		}
		target := messages[messageName]
		if model == nil || !token.IsExported(model.GoType) || target == nil || target.Desc.IsMapEntry() {
			return nil, fmt.Errorf("response mapping %q has an unknown or private model or message", name)
		}
		rules := map[string]map[string]any{}
		for _, value := range fields {
			rule, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("response mapping %q requires field objects", name)
			}
			key, ok := rule["target"].(string)
			if !ok || key == "" || len(key) > 1024 || rules[key] != nil {
				return nil, fmt.Errorf("response mapping %q has an invalid or duplicate target", name)
			}
			for k := range rule {
				if k != "target" && k != "source" && k != "constant" && k != "prefix" && k != "omit" && k != "conversion" {
					return nil, fmt.Errorf("response mapping %q has an unknown field option", name)
				}
			}
			rules[key] = rule
		}
		remaining := 512
		mapped, err := mapMessage(target, "", model.Fields, rules, 0, &remaining)
		if err != nil {
			return nil, fmt.Errorf("response mapping %q: %w", name, err)
		}
		if len(rules) != 0 {
			return nil, fmt.Errorf("response mapping %q has unknown or overlapping targets", name)
		}
		result = append(result, responseMapping{name, protogen.GoIdent{GoName: model.GoType, GoImportPath: protogen.GoImportPath(source.ImportPath)}, target, mapped})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].name < result[j].name })
	return result, nil
}

func mapMessage(message *protogen.Message, prefix string, sources []gen.GoModelField, rules map[string]map[string]any, depth int, remaining *int) ([]mappedField, error) {
	if depth > 8 {
		return nil, fmt.Errorf("nested response mapping exceeds eight levels")
	}
	var result []mappedField
	for _, target := range message.Fields {
		*remaining--
		if *remaining < 0 {
			return nil, fmt.Errorf("response mapping exceeds 512 fields")
		}
		key := prefix + string(target.Desc.Name())
		rule, present := rules[key]
		if present {
			delete(rules, key)
			if omit, exists := rule["omit"]; exists {
				if omit != true || len(rule) != 2 {
					return nil, fmt.Errorf("field %q requires omit: true without another action", key)
				}
				continue
			}
		}
		if target.Desc.IsList() || target.Desc.IsMap() || target.Oneof != nil && !target.Oneof.Desc.IsSynthetic() {
			return nil, fmt.Errorf("field %q requires an explicit omission; this shape is not supported", key)
		}
		if target.Message != nil && target.Message.Desc.FullName() != "google.protobuf.Timestamp" {
			if present {
				return nil, fmt.Errorf("field %q requires nested field mappings", key)
			}
			children, err := mapMessage(target.Message, key+".", sources, rules, depth+1, remaining)
			if err != nil {
				return nil, err
			}
			result = append(result, mappedField{target: target, children: children})
			continue
		}
		if !present {
			return nil, fmt.Errorf("field %q has no mapping or explicit omission", key)
		}
		mapped, err := mapScalar(target, rule, sources)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", key, err)
		}
		result = append(result, mapped)
	}
	return result, nil
}

func mapScalar(target *protogen.Field, rule map[string]any, sources []gen.GoModelField) (mappedField, error) {
	result := mappedField{target: target}
	if value, exists := rule["constant"]; exists {
		text, ok := value.(string)
		if !ok || len(rule) != 2 || len(text) > 4096 || !utf8.ValidString(text) || target.Desc.Kind() != protoreflect.StringKind {
			return result, fmt.Errorf("constant requires a string target and no other action")
		}
		result.constant = &text
		return result, nil
	}
	name, ok := rule["source"].(string)
	if !ok {
		return result, fmt.Errorf("source field is required")
	}
	for _, source := range sources {
		if source.Name == name {
			result.source = source
		}
	}
	if result.source.Name == "" || !token.IsExported(result.source.Selection) {
		return result, fmt.Errorf("unknown or private source field")
	}
	if value, exists := rule["conversion"]; exists {
		conversion, ok := value.(string)
		if !ok || conversion != "int32" || result.source.Type != types.FieldTypeInt64 || target.Desc.Kind() != protoreflect.Int32Kind {
			return result, fmt.Errorf("unsupported explicit conversion")
		}
		result.conversion = conversion
	}
	if value, exists := rule["prefix"]; exists {
		prefix, ok := value.(string)
		if !ok || len(prefix) > 4096 || !utf8.ValidString(prefix) || target.Desc.Kind() != protoreflect.StringKind || result.source.Pointer {
			return result, fmt.Errorf("prefix requires a non-pointer string source")
		}
		result.prefix = prefix
	}
	supported := map[types.FieldType]protoreflect.Kind{
		types.FieldTypeString: protoreflect.StringKind, types.FieldTypeEnum: protoreflect.StringKind,
		types.FieldTypeRef: protoreflect.StringKind, types.FieldTypeInt32: protoreflect.Int32Kind,
		types.FieldTypeInt64: protoreflect.Int64Kind, types.FieldTypeFloat: protoreflect.FloatKind,
		types.FieldTypeDouble: protoreflect.DoubleKind, types.FieldTypeBool: protoreflect.BoolKind,
		types.FieldTypeBytes: protoreflect.BytesKind,
	}
	if result.source.Type == types.FieldTypeTimestamp {
		if target.Message == nil || target.Message.Desc.FullName() != "google.protobuf.Timestamp" {
			return result, fmt.Errorf("timestamp requires google.protobuf.Timestamp")
		}
	} else if result.conversion == "" && (supported[result.source.Type] == 0 || supported[result.source.Type] != target.Desc.Kind()) {
		return result, fmt.Errorf("source and target types differ")
	}
	if result.source.Pointer && !target.Desc.HasPresence() {
		return result, fmt.Errorf("optional source cannot become a required value")
	}
	return result, nil
}

func renderResponseMappings(ctx gen.Context, plugin *protogen.Plugin, mappings []responseMapping) (*gen.File, error) {
	if len(mappings) == 0 {
		return nil, nil
	}
	importPath := path.Join(ctx.ModuleName, ctx.OutDirName, ctx.OutputNamespace, "mapping")
	g := plugin.NewGeneratedFile("response_mappings.go", protogen.GoImportPath(importPath))
	// This file has its own output location, outside the generated protobuf tree.
	g.Skip()
	g.P("// Code generated by STEGO. DO NOT EDIT.")
	g.P("package mapping")
	g.P("// ErrConversion contains no supplied value. The caller selects the public status.")
	g.P("var ErrConversion = ", protogen.GoIdent{GoName: "New", GoImportPath: "errors"}, "(\"response conversion failed\")")
	for _, mapping := range mappings {
		g.P("// ", mapping.name, " converts a prepared value. The caller must first check access.")
		g.P("func ", mapping.name, "(value ", mapping.source, ") (*", mapping.target.GoIdent, ", error) {")
		g.P("result := &", mapping.target.GoIdent, "{}")
		renderMappedFields(g, ctx, mapping.fields, "result")
		g.P("return result, nil")
		g.P("}")
	}
	content, err := g.Content()
	if err != nil {
		return nil, err
	}
	return &gen.File{Path: path.Join(ctx.OutputNamespace, "mapping/mappings.go"), Content: content}, nil
}

func renderMappedFields(g *protogen.GeneratedFile, ctx gen.Context, fields []mappedField, target string) {
	for _, field := range fields {
		destination := target + "." + field.target.GoName
		g.P("{")
		if field.target.Message != nil && field.target.Message.Desc.FullName() != "google.protobuf.Timestamp" {
			g.P(destination, " = &", field.target.Message.GoIdent, "{}")
			renderMappedFields(g, ctx, field.children, destination)
		} else if field.constant != nil {
			g.P("converted := ", strconv.Quote(*field.constant))
			renderAssignment(g, destination, field.target.Desc.HasPresence(), "converted")
		} else if field.source.Type == types.FieldTypeTimestamp {
			function := "Timestamp"
			if field.source.Pointer {
				function = "OptionalTimestamp"
			}
			g.P("converted, err := ", protogen.GoIdent{GoName: function, GoImportPath: protogen.GoImportPath(path.Join(ctx.ModuleName, ctx.OutDirName, ctx.OutputNamespace, "transport"))}, "(value.", field.source.Selection, ")")
			g.P("if err != nil { return nil, ErrConversion }")
			g.P(destination, " = converted")
		} else {
			expression := "value." + field.source.Selection
			if field.source.Pointer {
				g.P("if ", expression, " != nil {")
				expression = "*" + expression
			}
			if field.conversion == "int32" {
				g.P("if ", expression, " < -2147483648 || ", expression, " > 2147483647 { return nil, ErrConversion }")
				expression = "int32(" + expression + ")"
			}
			if field.prefix != "" {
				expression = strconv.Quote(field.prefix) + " + " + expression
			}
			if field.source.Type == types.FieldTypeBytes {
				g.P(destination, " = ", protogen.GoIdent{GoName: "Clone", GoImportPath: "bytes"}, "(", expression, ")")
			} else {
				g.P("converted := ", expression)
				if field.target.Desc.Kind() == protoreflect.StringKind {
					g.P("if !", protogen.GoIdent{GoName: "ValidString", GoImportPath: "unicode/utf8"}, "(converted) { return nil, ErrConversion }")
				}
				renderAssignment(g, destination, field.target.Desc.HasPresence(), "converted")
			}
			if field.source.Pointer {
				g.P("}")
			}
		}
		g.P("}")
	}
}

func renderAssignment(g *protogen.GeneratedFile, destination string, pointer bool, value string) {
	if pointer {
		value = "&" + value
	}
	g.P(destination, " = ", value)
}
