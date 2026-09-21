package httpapplication

import (
	"fmt"
	"go/format"
	"go/token"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/openapicontract"
	"github.com/jsell-rh/stego/internal/responsemapping"
	"github.com/jsell-rh/stego/internal/types"
)

type responsePlan struct {
	models   string
	mappings []responseMapping
}

type responseMapping struct {
	name, importPath, model, target string
	fields                          []responseField
	inputs                          []gen.GoModelField
}

type responseField struct {
	target     openapicontract.GoProperty
	source     gen.GoModelField
	constant   *string
	prefix     string
	conversion string
	omitEmpty  bool
	jsonLimits *responseJSONLimits
	input      bool
	absent     string
}

type responseJSONLimits struct{ bytes, items, itemBytes int }

func responseContractConfig(config map[string]any) (map[string]any, error) {
	value, present := config["document"]
	references, hasReferences := config["references"]
	_, mappings := config["response_mappings"]
	if !present && !mappings && !hasReferences {
		return nil, nil
	}
	if !present || !mappings {
		return nil, fmt.Errorf("HTTP response mappings require document and response_mappings")
	}
	contract := map[string]any{"document": value}
	if hasReferences {
		contract["references"] = references
	}
	return contract, nil
}

func (*Generator) InputFiles(config map[string]any) ([]gen.InputFile, error) {
	contract, err := responseContractConfig(config)
	if err != nil || contract == nil {
		return nil, err
	}
	names, err := openapicontract.InputFiles(contract)
	return gen.SourceInputs(names), err
}

func prepareResponses(ctx gen.Context) (*responsePlan, error) {
	contract, err := responseContractConfig(ctx.ComponentConfig)
	if err != nil || contract == nil {
		return nil, err
	}
	items, ok := ctx.ComponentConfig["response_mappings"].([]any)
	if !ok || len(items) == 0 || len(items) > 128 {
		return nil, fmt.Errorf("HTTP response_mappings requires 1 through 128 mappings")
	}
	var schemas []string
	seen := map[string]bool{}
	for _, value := range items {
		item, ok := value.(map[string]any)
		if !ok || (len(item) != 5 && len(item) != 6) {
			return nil, fmt.Errorf("HTTP response mapping requires name, provider, model, schema, and fields")
		}
		for key := range item {
			if key != "name" && key != "provider" && key != "model" && key != "schema" && key != "fields" && key != "inputs" {
				return nil, fmt.Errorf("unknown HTTP response mapping option")
			}
		}
		schema, ok := item["schema"].(string)
		if !ok || schema == "" {
			return nil, fmt.Errorf("HTTP response mapping requires a schema")
		}
		if !seen[schema] {
			schemas = append(schemas, schema)
			seen[schema] = true
		}
	}
	input := ctx
	input.ComponentConfig = contract
	doc, err := openapicontract.Load(input)
	if err != nil {
		return nil, err
	}
	models, objects, err := openapicontract.GenerateGoModels(doc, "contract", schemas)
	if err != nil {
		return nil, err
	}
	plan := &responsePlan{models: models}
	names := map[string]bool{"ErrConversion": true}
	for _, value := range items {
		item := value.(map[string]any)
		name, n := item["name"].(string)
		provider, p := item["provider"].(string)
		modelName, m := item["model"].(string)
		fields, f := item["fields"].([]any)
		if !n || !p || !m || !f || len(name) > 128 || !token.IsIdentifier(name) || !token.IsExported(name) || names[name] || len(fields) == 0 || len(fields) > 512 {
			return nil, fmt.Errorf("invalid or duplicate HTTP response mapping")
		}
		names[name] = true
		source, exists := ctx.GoModelSources[provider]
		if !exists || gen.ValidateGoImportNamespace(source.ImportPath) != nil || source.ImportPath == "" || gen.ValidateGoModels(source.Models) != nil {
			return nil, fmt.Errorf("HTTP response mapping has no valid model provider")
		}
		var model *gen.GoModel
		for i := range source.Models {
			if source.Models[i].Name == modelName {
				model = &source.Models[i]
			}
		}
		if model == nil || !token.IsExported(model.GoType) {
			return nil, fmt.Errorf("HTTP response mapping has an unknown or private model")
		}
		object := objects[item["schema"].(string)]
		mapping := responseMapping{name: name, importPath: source.ImportPath, model: model.GoType, target: object.GoType}
		if raw, exists := item["inputs"]; exists {
			mapping.inputs, err = responseInputs(raw)
			if err != nil {
				return nil, err
			}
			if names[name+"Input"] {
				return nil, fmt.Errorf("HTTP response input type conflicts with a mapping name")
			}
			names[name+"Input"] = true
		}
		usedInputs := map[string]bool{}

		rules := map[string]map[string]any{}
		for _, value := range fields {
			rule, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("HTTP response mapping requires field objects")
			}
			target, ok := rule["target"].(string)
			if !ok || target == "" || rules[target] != nil {
				return nil, fmt.Errorf("invalid or duplicate HTTP response target")
			}
			rules[target] = rule
		}
		for _, target := range object.Fields {
			rule, exists := rules[target.JSONName]
			if !exists {
				return nil, fmt.Errorf("HTTP response property %q has no mapping", target.JSONName)
			}
			delete(rules, target.JSONName)
			if omit, exists := rule["omit"]; exists {
				if omit != true || len(rule) != 2 || target.Required || !target.OmitEmpty {
					return nil, fmt.Errorf("HTTP response omission requires an optional omitted field")
				}
				continue
			}
			field, err := bindPreparedResponseField(target, model.Fields, mapping.inputs, rule)
			if err != nil {
				return nil, fmt.Errorf("HTTP response property %q: %w", target.JSONName, err)
			}
			if field.input {
				usedInputs[field.source.Name] = true
			}
			mapping.fields = append(mapping.fields, field)
		}
		if len(usedInputs) != len(mapping.inputs) {
			return nil, fmt.Errorf("HTTP response input has no mapping")
		}
		remainingBytes, remainingItems := 16<<20, 65536
		for _, field := range mapping.fields {
			if limits := field.jsonLimits; limits != nil {
				if limits.bytes > remainingBytes || limits.items > remainingItems {
					return nil, fmt.Errorf("combined JSON limits exceed the response mapping budget")
				}
				remainingBytes -= limits.bytes
				remainingItems -= limits.items
			}
		}
		if len(rules) != 0 {
			return nil, fmt.Errorf("HTTP response mapping has unknown targets")
		}
		plan.mappings = append(plan.mappings, mapping)
	}
	sort.Slice(plan.mappings, func(i, j int) bool { return plan.mappings[i].name < plan.mappings[j].name })
	return plan, nil
}

func bindResponseField(target openapicontract.GoProperty, sources []gen.GoModelField, rule map[string]any) (responseField, error) {
	field := responseField{target: target}
	for option := range rule {
		if option != "target" && option != "source" && option != "constant" && option != "prefix" && option != "conversion" && option != "omit_empty" && option != "max_bytes" && option != "max_items" && option != "max_item_bytes" && option != "on_absent" {
			return field, fmt.Errorf("unknown HTTP response field option")
		}
	}
	base := strings.TrimPrefix(target.GoType, "*")
	if target.Nullable {
		if target.HasEnum {
			return field, fmt.Errorf("nullable response enums are not supported")
		}
		inner, ok := strings.CutPrefix(base, "nullable.Nullable[")
		if !ok || !strings.HasSuffix(inner, "]") {
			return field, fmt.Errorf("nullable response has an unsupported Go type")
		}
		base = strings.TrimSuffix(inner, "]")
	}
	if len(target.StringEnum) != 0 {
		base = "string"
	}
	if value, exists := rule["constant"]; exists {
		constant, ok := value.(string)
		if !ok || len(rule) != 2 || base != "string" || len(constant) > 4096 || !utf8.ValidString(constant) {
			return field, fmt.Errorf("constant requires a bounded string and no other action")
		}
		if len(target.StringEnum) != 0 {
			allowed := false
			for _, value := range target.StringEnum {
				if constant == value {
					allowed = true
					break
				}
			}
			if !allowed {
				return field, fmt.Errorf("constant is outside the response enum")
			}
		}
		field.constant = &constant
		return field, nil
	}
	name, ok := rule["source"].(string)
	if !ok {
		return field, fmt.Errorf("HTTP response source is required")
	}
	for _, source := range sources {
		if source.Name == name {
			field.source = source
		}
	}
	if field.source.Name == "" || !token.IsExported(field.source.Selection) {
		return field, fmt.Errorf("unknown or private HTTP response source")
	}
	if raw, exists := rule["on_absent"]; exists {
		policy, ok := raw.(string)
		if !ok || !target.Nullable || !field.source.Pointer || (policy != "emit_null" && policy != "omit") || (policy == "omit" && (target.Required || !target.OmitEmpty)) {
			return field, fmt.Errorf("on_absent requires a nullable target, a pointer source, and a valid presence policy")
		}
		field.absent = policy
	} else if target.Nullable && field.source.Pointer {
		return field, fmt.Errorf("nullable response pointer requires an explicit on_absent policy")
	}
	if rule["conversion"] == "json_strings" {
		return bindJSONResponse(field, rule)
	}
	for _, option := range []string{"max_bytes", "max_items", "max_item_bytes"} {
		if _, exists := rule[option]; exists {
			return field, fmt.Errorf("JSON limits require json_strings conversion")
		}
	}
	if field.source.Pointer && !target.Nullable && (!strings.HasPrefix(target.GoType, "*") || target.Required) {
		return field, fmt.Errorf("optional source cannot supply a required response value")
	}
	sourceType := map[types.FieldType]string{
		types.FieldTypeString: "string", types.FieldTypeEnum: "string", types.FieldTypeRef: "string",
		types.FieldTypeBool: "bool", types.FieldTypeInt32: "int32", types.FieldTypeInt64: "int64",
		types.FieldTypeFloat: "float32", types.FieldTypeDouble: "float64", types.FieldTypeTimestamp: "time.Time",
	}[field.source.Type]
	if value, exists := rule["conversion"]; exists {
		conversion, ok := value.(string)
		if !ok || conversion != "int32" || sourceType != "int64" || base != "int32" {
			return field, fmt.Errorf("unsupported HTTP response conversion")
		}
		field.conversion = conversion
	} else if sourceType == "" || sourceType != base {
		return field, fmt.Errorf("HTTP response source and target types differ")
	}
	if value, exists := rule["prefix"]; exists {
		prefix, ok := value.(string)
		if !ok || sourceType != "string" || field.source.Pointer || len(prefix) > 4096 || !utf8.ValidString(prefix) {
			return field, fmt.Errorf("prefix requires a non-pointer string source")
		}
		field.prefix = prefix
	}
	if value, exists := rule["omit_empty"]; exists {
		if value != true || field.source.Pointer || sourceType != "string" || (!strings.HasPrefix(target.GoType, "*") || base != "string") || target.Required || !target.OmitEmpty {
			return field, fmt.Errorf("omit_empty requires a non-pointer string and optional response pointer")
		}
		field.omitEmpty = true
	}
	return field, nil
}

func renderResponses(ctx gen.Context, plan *responsePlan) ([]gen.File, error) {
	if plan == nil {
		return nil, nil
	}
	imports := map[string]string{"errors": "errors", "contract": path.Join(ctx.ModuleName, ctx.OutDirName, ctx.OutputNamespace, "contract")}
	providers := map[string]string{}
	var body strings.Builder
	body.WriteString("// ErrConversion contains no supplied value. The caller selects the public status.\nvar ErrConversion = errors.New(\"response conversion failed\")\n")
	for _, mapping := range plan.mappings {
		alias := providers[mapping.importPath]
		if alias == "" {
			alias = fmt.Sprintf("model%d", len(providers))
			providers[mapping.importPath], imports[alias] = alias, mapping.importPath
		}
		inputArgument := ""
		if len(mapping.inputs) != 0 {
			fmt.Fprintf(&body, "// %sInput contains values prepared by the application.\ntype %sInput struct {\n", mapping.name, mapping.name)
			for _, field := range mapping.inputs {
				fieldType := responsemapping.InputGoType(field.Type)
				if field.Type == types.FieldTypeTimestamp {
					imports["time"] = "time"
				}
				if field.Pointer {
					fieldType = "*" + fieldType
				}
				fmt.Fprintf(&body, "%s %s\n", field.Selection, fieldType)
			}
			body.WriteString("}\n")
			inputArgument = ",input " + mapping.name + "Input"
		}
		fmt.Fprintf(&body, "// %s converts a prepared value after the caller checks access.\nfunc %s(value %s.%s%s) (*contract.%s,error) {\nresult := &contract.%s{}\n", mapping.name, mapping.name, alias, mapping.model, inputArgument, mapping.target, mapping.target)
		for _, field := range mapping.fields {
			body.WriteString("{\n")
			receiver := "value"
			if field.input {
				receiver = "input"
			}
			if field.jsonLimits != nil {
				limits := field.jsonLimits
				fmt.Fprintf(&body, "v,err := jsonStrings(%s.%s,%d,%d,%d)\nif err != nil {return nil,ErrConversion}\nif v == nil {v = make([]string,0)}\n", receiver, field.source.Selection, limits.bytes, limits.items, limits.itemBytes)
			} else if field.constant != nil {
				fmt.Fprintf(&body, "v := %s\n", strconv.Quote(*field.constant))
			} else if field.source.Pointer {
				fmt.Fprintf(&body, "if %s.%s != nil {\nv := *%s.%s\n", receiver, field.source.Selection, receiver, field.source.Selection)
			} else {
				fmt.Fprintf(&body, "v := %s.%s\n", receiver, field.source.Selection)
			}
			if field.omitEmpty {
				if field.jsonLimits != nil {
					body.WriteString("if len(v) != 0 {\n")
				} else {
					body.WriteString("if v != \"\" {\n")
				}
			}
			if field.constant == nil {
				switch field.source.Type {
				case types.FieldTypeString, types.FieldTypeEnum, types.FieldTypeRef:
					imports["utf8"] = "unicode/utf8"
					body.WriteString("if !utf8.ValidString(v) {return nil,ErrConversion}\n")
				case types.FieldTypeTimestamp:
					body.WriteString("if _,offset := v.Zone(); offset%60 != 0 {return nil,ErrConversion}\n")
					body.WriteString("if _,err := v.MarshalJSON(); err != nil {return nil,ErrConversion}\n")
				case types.FieldTypeFloat, types.FieldTypeDouble:
					imports["math"] = "math"
					body.WriteString("if math.IsNaN(float64(v)) || math.IsInf(float64(v),0) {return nil,ErrConversion}\n")
				}
			}
			if field.prefix != "" {
				fmt.Fprintf(&body, "v = %s + v\n", strconv.Quote(field.prefix))
			}
			variable := "v"
			if field.conversion == "int32" {
				body.WriteString("if v < -2147483648 || v > 2147483647 {return nil,ErrConversion}\nconverted := int32(v)\n")
				variable = "converted"
			}
			if len(field.target.StringEnum) != 0 {
				body.WriteString("switch v {case ")
				for i, value := range field.target.StringEnum {
					if i > 0 {
						body.WriteString(",")
					}
					body.WriteString(strconv.Quote(value))
				}
				body.WriteString(":\ndefault:return nil,ErrConversion\n}\n")
				cast := field.target.EnumGoType
				if cast != "string" {
					cast = "contract." + cast
				}
				fmt.Fprintf(&body, "enumValue := %s(v)\n", cast)
				variable = "enumValue"
			}
			if field.target.Nullable {
				imports["nullable"] = "github.com/oapi-codegen/nullable"
				variable = "nullable.NewNullableWithValue(" + variable + ")"
			} else if strings.HasPrefix(field.target.GoType, "*") {
				variable = "&" + variable
			}
			fmt.Fprintf(&body, "result.%s = %s\n", field.target.GoName, variable)
			if field.omitEmpty {
				body.WriteString("}\n")
			}
			if field.source.Pointer {
				if field.absent == "emit_null" {
					fmt.Fprintf(&body, "} else {\nresult.%s.SetNull()\n", field.target.GoName)
				}
				body.WriteString("}\n")
			}
			body.WriteString("}\n")
		}
		body.WriteString("return result,nil\n}\n")
	}
	var source strings.Builder
	source.WriteString("// Code generated by STEGO. DO NOT EDIT.\npackage responses\nimport(\n")
	names := make([]string, 0, len(imports))
	for name := range imports {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&source, "%s %q\n", name, imports[name])
	}
	source.WriteString(")\n" + body.String())
	code, err := format.Source([]byte(source.String()))
	if err != nil {
		return nil, fmt.Errorf("cannot format HTTP response mappings: %w", err)
	}
	files := []gen.File{
		{Path: path.Join(ctx.OutputNamespace, "contract/models.go"), Content: []byte(plan.models)},
		{Path: path.Join(ctx.OutputNamespace, "responses/mappings.go"), Content: code},
	}
	for _, mapping := range plan.mappings {
		for _, field := range mapping.fields {
			if field.jsonLimits != nil {
				source, err := responsemapping.JSONStringsSource("responses")
				if err != nil {
					return nil, err
				}
				files = append(files, gen.File{Path: path.Join(ctx.OutputNamespace, "responses/json_strings.go"), Content: source})
				return files, nil
			}
		}
	}
	return files, nil
}

func bindJSONResponse(field responseField, rule map[string]any) (responseField, error) {
	target := field.target
	if !target.StringList || (target.GoType != "[]string" && target.GoType != "*[]string") || field.source.Type != types.FieldTypeJsonb || field.source.Pointer {
		return field, fmt.Errorf("json_strings requires a JSON source and a non-null string list")
	}
	expected := 6
	if value, exists := rule["omit_empty"]; exists {
		if value != true || target.Required || !target.OmitEmpty {
			return field, fmt.Errorf("list omission requires an optional omitted response field")
		}
		field.omitEmpty = true
		expected++
	}
	if len(rule) != expected {
		return field, fmt.Errorf("json_strings requires three limits and no other action")
	}
	bytes, b := rule["max_bytes"].(int)
	items, i := rule["max_items"].(int)
	itemBytes, c := rule["max_item_bytes"].(int)
	if !b || !i || !c || bytes < 1 || bytes > 16<<20 || items < 1 || items > 65536 || itemBytes < 1 || itemBytes > bytes {
		return field, fmt.Errorf("json_strings limits are invalid")
	}
	field.jsonLimits = &responseJSONLimits{bytes, items, itemBytes}
	return field, nil
}
