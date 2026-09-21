package openapicontract

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// GoObject binds an OpenAPI object to the actual generated Go type.
type GoObject struct {
	GoType string
	Fields []GoProperty
}

// GoProperty records the backend field and the declared JSON presence rules.
// GoType is a parsed backend expression, never an application expression.
type GoProperty struct {
	JSONName   string
	GoName     string
	GoType     string
	Required   bool
	Nullable   bool
	OmitEmpty  bool
	StringList bool
	StringEnum []string
	EnumGoType string
}

// GenerateGoModels renders model types and binds selected response objects.
// A document must come from Load. The caller owns it exclusively during this
// call. On error, neither source nor partial bindings are returned.
func GenerateGoModels(doc *openapi3.T, packageName string, schemas []string) (string, map[string]GoObject, error) {
	if len(schemas) == 0 || len(schemas) > 128 || doc == nil || doc.Components == nil {
		return "", nil, fmt.Errorf("Go response models require 1 through 128 schemas")
	}
	selected := map[string]map[string]objectProperty{}
	for _, name := range schemas {
		if _, exists := selected[name]; exists {
			return "", nil, fmt.Errorf("duplicate Go response schema")
		}
		properties, err := objectProperties(doc.Components.Schemas[name])
		if err != nil {
			return "", nil, fmt.Errorf("response schema %q: %w", name, err)
		}
		selected[name] = properties
	}
	source, names, err := generateGo(doc, packageName, GoModels, true)
	if err != nil {
		return "", nil, err
	}
	declarations, err := modelDeclarations(source)
	if err != nil {
		return "", nil, err
	}
	result := make(map[string]GoObject, len(selected))
	for _, name := range schemas {
		goName := names[name]
		structure, ok := objectDeclaration(declarations, goName)
		if !ok || !token.IsExported(goName) {
			return "", nil, fmt.Errorf("response schema %q has no generated public structure", name)
		}
		object := GoObject{GoType: goName, Fields: make([]GoProperty, 0, len(structure.Fields.List))}
		seen := map[string]bool{}
		for _, field := range structure.Fields.List {
			if len(field.Names) != 1 || field.Tag == nil || !token.IsExported(field.Names[0].Name) {
				return "", nil, fmt.Errorf("response schema %q has an unsupported Go field", name)
			}
			raw, err := strconv.Unquote(field.Tag.Value)
			if err != nil {
				return "", nil, fmt.Errorf("invalid Go response field tag")
			}
			tag, present := reflect.StructTag(raw).Lookup("json")
			parts := strings.Split(tag, ",")
			if !present || parts[0] == "" || parts[0] == "-" || len(parts) > 2 || (len(parts) == 2 && parts[1] != "omitempty") {
				return "", nil, fmt.Errorf("unsupported Go response JSON tag")
			}
			property, exists := selected[name][parts[0]]
			if !exists || seen[parts[0]] {
				return "", nil, fmt.Errorf("Go response fields do not match the schema properties")
			}
			seen[parts[0]] = true
			var expression bytes.Buffer
			if err := format.Node(&expression, token.NewFileSet(), field.Type); err != nil {
				return "", nil, fmt.Errorf("invalid Go response field type")
			}
			enum, enumType, err := stringEnumProperty(property.schema, field.Type, declarations)
			if err != nil {
				return "", nil, err
			}
			object.Fields = append(object.Fields, GoProperty{
				JSONName: parts[0], GoName: field.Names[0].Name, GoType: expression.String(),
				Required: property.required, Nullable: property.schema.Nullable, OmitEmpty: len(parts) == 2,
				StringList: stringListProperty(property.schema),
				StringEnum: enum, EnumGoType: enumType,
			})
		}
		if len(seen) != len(selected[name]) {
			return "", nil, fmt.Errorf("incomplete Go response property bindings")
		}
		sort.Slice(object.Fields, func(i, j int) bool { return object.Fields[i].JSONName < object.Fields[j].JSONName })
		result[name] = object
	}
	return source, result, nil
}

type objectProperty struct {
	schema   *openapi3.Schema
	required bool
}

func objectDeclaration(declarations map[string]ast.Expr, name string) (*ast.StructType, bool) {
	for range 49 {
		switch expression := declarations[name].(type) {
		case *ast.StructType:
			return expression, true
		case *ast.Ident:
			name = expression.Name
		default:
			return nil, false
		}
	}
	return nil, false
}

// Reject intersections that would let the backend merge two property rules.
// Required names can refer to a property declared in another allOf branch.
func objectProperties(root *openapi3.SchemaRef) (map[string]objectProperty, error) {
	properties := map[string]objectProperty{}
	required := map[string]bool{}
	remaining := 4096
	var walk func(*openapi3.SchemaRef, int) error
	walk = func(ref *openapi3.SchemaRef, depth int) error {
		remaining--
		if ref == nil || ref.Value == nil || depth > 48 || remaining < 0 {
			return fmt.Errorf("missing or excessive response object")
		}
		schema := ref.Value
		if (schema.Type != nil && !schema.Type.Is("object")) || schema.Nullable || len(schema.AnyOf) != 0 || len(schema.OneOf) != 0 || schema.Not != nil || schema.AdditionalProperties.Schema != nil || (schema.AdditionalProperties.Has != nil && *schema.AdditionalProperties.Has) {
			return fmt.Errorf("response mapping requires a fixed non-null object")
		}
		if schema.Type == nil && len(schema.AllOf) == 0 && len(schema.Properties) == 0 && len(schema.Required) == 0 {
			return fmt.Errorf("response mapping requires an object schema")
		}
		for _, name := range schema.Required {
			required[name] = true
		}
		for name, child := range schema.Properties {
			if _, exists := properties[name]; exists {
				return fmt.Errorf("composed response properties are ambiguous")
			}
			if child == nil || child.Value == nil || len(properties) >= 512 {
				return fmt.Errorf("missing or excessive response properties")
			}
			properties[name] = objectProperty{schema: child.Value}
		}
		for _, child := range schema.AllOf {
			if err := walk(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root, 0); err != nil {
		return nil, err
	}
	for name := range required {
		property, exists := properties[name]
		if !exists {
			return nil, fmt.Errorf("required response property has no declaration")
		}
		property.required = true
		properties[name] = property
	}
	return properties, nil
}

// The parser accepts duplicate identifiers. Reject them before returning code,
// including collisions in unselected models that share the generated package.
func modelDeclarations(source string) (map[string]ast.Expr, error) {
	file, err := parser.ParseFile(token.NewFileSet(), "models.go", source, 0)
	if err != nil {
		return nil, fmt.Errorf("invalid generated Go models")
	}
	seen := map[string]bool{}
	declarations := map[string]ast.Expr{}
	claim := func(name string) error {
		if name == "_" || seen[name] {
			return fmt.Errorf("duplicate or blank generated Go identifier")
		}
		seen[name] = true
		return nil
	}
	for _, declaration := range file.Decls {
		switch declaration := declaration.(type) {
		case *ast.GenDecl:
			for _, spec := range declaration.Specs {
				switch spec := spec.(type) {
				case *ast.TypeSpec:
					if err := claim(spec.Name.Name); err != nil {
						return nil, err
					}
					declarations[spec.Name.Name] = spec.Type
				case *ast.ValueSpec:
					for _, name := range spec.Names {
						if err := claim(name.Name); err != nil {
							return nil, err
						}
					}
				}
			}
		case *ast.FuncDecl:
			if declaration.Recv == nil {
				if err := claim(declaration.Name.Name); err != nil {
					return nil, err
				}
			}
		}
	}
	var fieldError error
	ast.Inspect(file, func(node ast.Node) bool {
		structure, ok := node.(*ast.StructType)
		if !ok {
			return true
		}
		names := map[string]bool{}
		for _, field := range structure.Fields.List {
			for _, name := range field.Names {
				if names[name.Name] || name.Name == "_" {
					fieldError = fmt.Errorf("duplicate or blank generated Go field")
				}
				names[name.Name] = true
			}
		}
		return true
	})
	if fieldError != nil {
		return nil, fieldError
	}
	return declarations, nil
}

// Reject nullable items and composition that would lose a declared list shape.
func stringListProperty(schema *openapi3.Schema) bool {
	if schema == nil || schema.Type == nil || !schema.Type.Is("array") || schema.Items == nil || schema.Items.Value == nil || len(schema.AllOf)+len(schema.AnyOf)+len(schema.OneOf) != 0 || schema.Not != nil {
		return false
	}
	item := schema.Items.Value
	return item.Type != nil && item.Type.Is("string") && !item.Nullable && len(item.AllOf)+len(item.AnyOf)+len(item.OneOf) == 0 && item.Not == nil
}
