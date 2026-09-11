// Package openapicontract loads captured OpenAPI inputs for SDK generators.
package openapicontract

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/jsell-rh/stego/internal/gen"
	"gopkg.in/yaml.v3"
)

func sourceNames(config map[string]any) (string, []string, error) {
	for key := range config {
		if key != "document" && key != "references" {
			return "", nil, fmt.Errorf("unsupported SDK configuration field %q", key)
		}
	}
	root, ok := config["document"].(string)
	if !ok || gen.ValidatePath(root) != nil {
		return "", nil, fmt.Errorf("SDK requires a project-relative document")
	}
	names := []string{root}
	seen := map[string]bool{root: true}
	if value, present := config["references"]; present {
		refs, ok := value.([]any)
		if !ok || len(refs) > 63 {
			return "", nil, fmt.Errorf("SDK references must contain at most 63 paths")
		}
		for _, value := range refs {
			name, ok := value.(string)
			if !ok || gen.ValidatePath(name) != nil || seen[name] {
				return "", nil, fmt.Errorf("invalid or duplicate SDK reference path")
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return root, names, nil
}
func InputFiles(config map[string]any) ([]string, error) {
	_, names, err := sourceNames(config)
	return names, err
}

// Check syntax before the OpenAPI loader. YAML aliases and duplicate keys must
// not change the meaning of a captured contract.
func checkDocument(data []byte) error {
	if len(data) == 0 || len(data) > 1<<20 {
		return fmt.Errorf("OpenAPI input must contain 1 through 1048576 bytes")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var root yaml.Node
	if err := decoder.Decode(&root); err != nil {
		return fmt.Errorf("invalid OpenAPI YAML")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("OpenAPI input must contain one document")
	}
	count := 0
	var visit func(*yaml.Node, int) error
	visit = func(node *yaml.Node, depth int) error {
		count++
		if count > 100000 || depth > 64 {
			return fmt.Errorf("OpenAPI input exceeds structural limits")
		}
		if node.Kind == yaml.AliasNode || node.Anchor != "" {
			return fmt.Errorf("OpenAPI YAML aliases and anchors are unsupported")
		}
		if node.Kind == yaml.MappingNode {
			seen := map[string]bool{}
			for i := 0; i < len(node.Content); i += 2 {
				key, value := node.Content[i], node.Content[i+1]
				if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || seen[key.Value] {
					return fmt.Errorf("OpenAPI map requires unique string keys")
				}
				seen[key.Value] = true
				if strings.HasPrefix(strings.ToLower(key.Value), "x-") {
					if key.Value != "x-sensitive" || value.Kind != yaml.ScalarNode || value.Tag != "!!bool" {
						return fmt.Errorf("unsupported OpenAPI extension")
					}
					// All body fields are excluded from diagnostics, including sensitive fields.
				}
				if key.Value == "$ref" {
					u, err := url.Parse(value.Value)
					if value.Kind != yaml.ScalarNode || value.Tag != "!!str" || err != nil || u.IsAbs() || u.Host != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.HasPrefix(u.Path, "/") {
						return fmt.Errorf("OpenAPI references must use captured relative files")
					}
				}
			}
		}
		for _, child := range node.Content {
			if err := visit(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	return visit(&root, 0)
}
func Load(ctx gen.Context) (*openapi3.T, error) {
	root, names, err := sourceNames(ctx.ComponentConfig)
	if err != nil {
		return nil, err
	}
	sources := map[string][]byte{}
	for _, name := range names {
		data, present := ctx.Inputs[name]
		if !present {
			return nil, fmt.Errorf("missing captured OpenAPI source %q", name)
		}
		if err := checkDocument(data); err != nil {
			return nil, fmt.Errorf("OpenAPI source %q: %w", name, err)
		}
		sources["/"+name] = data
	}
	deadline, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loader := openapi3.NewLoader()
	loader.Context = deadline
	loader.ReadFromURIFunc = func(_ *openapi3.Loader, u *url.URL) ([]byte, error) {
		if u.Scheme != "stego" || u.Host != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || path.Clean(u.Path) != u.Path {
			return nil, fmt.Errorf("OpenAPI reference is outside captured inputs")
		}
		data, ok := sources[u.Path]
		if !ok {
			return nil, fmt.Errorf("OpenAPI reference is not a declared input")
		}
		return data, nil
	}
	doc, err := loader.LoadFromDataWithPath(sources["/"+root], &url.URL{Scheme: "stego", Path: "/" + root})
	if err != nil {
		return nil, fmt.Errorf("cannot resolve captured OpenAPI contract: %w", err)
	}
	if err := checkSchemaGraph(doc); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.0.") {
		return nil, fmt.Errorf("SDK supports OpenAPI 3.0")
	}
	if err := doc.Validate(deadline); err != nil {
		return nil, fmt.Errorf("invalid OpenAPI contract: %w", err)
	}
	if doc.Paths.Len() == 0 || doc.Paths.Len() > 256 {
		return nil, fmt.Errorf("SDK requires 1 through 256 paths")
	}
	operations := 0
	for _, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			operations++
			switch method {
			case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD":
			default:
				return nil, fmt.Errorf("unsupported SDK HTTP method")
			}
			if op.OperationID == "" || len(op.OperationID) > 128 {
				return nil, fmt.Errorf("SDK operations require bounded operation IDs")
			}
			if len(op.Callbacks) > 0 {
				return nil, fmt.Errorf("SDK callbacks are unsupported")
			}
			if op.RequestBody != nil {
				for content := range op.RequestBody.Value.Content {
					if content != "application/json" {
						return nil, fmt.Errorf("SDK requests require application/json")
					}
				}
			}
			for _, response := range op.Responses.Map() {
				for content := range response.Value.Content {
					if content != "application/json" {
						return nil, fmt.Errorf("SDK responses require application/json")
					}
				}
			}
		}
	}
	if operations > 512 {
		return nil, fmt.Errorf("SDK operation limit exceeded")
	}
	if doc.Components != nil {
		if len(doc.Components.Callbacks) > 0 {
			return nil, fmt.Errorf("SDK callbacks are unsupported")
		}
		for _, scheme := range doc.Components.SecuritySchemes {
			if scheme.Value.Type != "http" || scheme.Value.Scheme != "bearer" {
				return nil, fmt.Errorf("SDK authentication supports HTTP bearer tokens")
			}
		}
	}
	if err := internalize(doc, deadline, root); err != nil {
		return nil, err
	}
	return doc, nil
}

// Bound schema expansion before the backend handles allOf and nested objects.
// Recursive models need a separate generation contract.
func checkSchemaGraph(doc *openapi3.T) error {
	if doc == nil || doc.Paths == nil {
		return fmt.Errorf("SDK contract requires paths")
	}
	count := 0
	active := map[*openapi3.Schema]bool{}
	var walk func(*openapi3.SchemaRef, int) error
	walk = func(ref *openapi3.SchemaRef, depth int) error {
		if ref == nil || ref.Value == nil {
			return nil
		}
		count++
		if count > 32768 || depth > 48 {
			return fmt.Errorf("SDK schema expansion exceeds limits")
		}
		s := ref.Value
		if active[s] {
			return fmt.Errorf("recursive SDK schemas are unsupported")
		}
		active[s] = true
		defer delete(active, s)
		for _, group := range []openapi3.SchemaRefs{s.AllOf, s.AnyOf, s.OneOf} {
			for _, child := range group {
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
		}
		for _, child := range s.Properties {
			if err := walk(child, depth+1); err != nil {
				return err
			}
		}
		for _, child := range []*openapi3.SchemaRef{s.Items, s.Not, s.AdditionalProperties.Schema} {
			if err := walk(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if doc.Components != nil {
		for _, schema := range doc.Components.Schemas {
			if err := walk(schema, 0); err != nil {
				return err
			}
		}
	}
	for _, item := range doc.Paths.Map() {
		if item == nil {
			return fmt.Errorf("invalid SDK path item")
		}
		for _, op := range item.Operations() {
			if op == nil || op.Responses == nil {
				return fmt.Errorf("SDK operation requires responses")
			}
			if op.RequestBody != nil && op.RequestBody.Value != nil {
				for _, media := range op.RequestBody.Value.Content {
					if media == nil {
						return fmt.Errorf("invalid SDK media type")
					}
					if err := walk(media.Schema, 0); err != nil {
						return err
					}
				}
			}
			for _, response := range op.Responses.Map() {
				if response != nil && response.Value != nil {
					for _, media := range response.Value.Content {
						if media == nil {
							return fmt.Errorf("invalid SDK media type")
						}
						if err := walk(media.Schema, 0); err != nil {
							return err
						}
					}
				}
			}
			for _, param := range append(append(openapi3.Parameters{}, item.Parameters...), op.Parameters...) {
				if param != nil && param.Value != nil {
					if err := walk(param.Value.Schema, 0); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// Keep root names. Preserve each file name for other references. Reject names
// that collapse to the same identifier before the backend can merge schemas.
func internalize(doc *openapi3.T, ctx context.Context, root string) error {
	aliases := map[string]string{}
	occupied := map[string]string{}
	add := func(name string, ref openapi3.ComponentRef) {
		key := ref.CollectionName() + "/" + name
		canonical := (&url.URL{Scheme: "stego", Path: "/" + root, Fragment: "/components/" + key}).String()
		aliases[canonical] = name
		occupied[key] = canonical
		if ref.RefString() != "" && ref.RefPath() != nil {
			external := ref.RefPath().String()
			if _, exists := aliases[external]; !exists {
				aliases[external] = name
			}
		}
	}
	if doc.Components != nil {
		// Sorting also gives explicit aliases a stable order.
		for _, name := range sortedKeys(doc.Components.Schemas) {
			add(name, doc.Components.Schemas[name])
		}
		for _, name := range sortedKeys(doc.Components.Parameters) {
			add(name, doc.Components.Parameters[name])
		}
		for _, name := range sortedKeys(doc.Components.RequestBodies) {
			add(name, doc.Components.RequestBodies[name])
		}
		for _, name := range sortedKeys(doc.Components.Responses) {
			add(name, doc.Components.Responses[name])
		}
		for _, name := range sortedKeys(doc.Components.Headers) {
			add(name, doc.Components.Headers[name])
		}
		for _, name := range sortedKeys(doc.Components.SecuritySchemes) {
			add(name, doc.Components.SecuritySchemes[name])
		}
		for _, name := range sortedKeys(doc.Components.Examples) {
			add(name, doc.Components.Examples[name])
		}
		for _, name := range sortedKeys(doc.Components.Links) {
			add(name, doc.Components.Links[name])
		}
		for _, name := range sortedKeys(doc.Components.Callbacks) {
			add(name, doc.Components.Callbacks[name])
		}
	}
	var collision error
	doc.InternalizeRefs(ctx, func(_ *openapi3.T, ref openapi3.ComponentRef) string {
		u := ref.RefPath()
		if u == nil {
			collision = fmt.Errorf("SDK reference has no captured location")
			return "InvalidReference"
		}
		canonical := u.String()
		if name, ok := aliases[canonical]; ok {
			return name
		}
		file := strings.TrimPrefix(u.Path, path.Dir("/"+root)+"/")
		file = strings.TrimSuffix(file, path.Ext(file))
		fragment := strings.TrimPrefix(u.Fragment, "/components/"+ref.CollectionName()+"/")
		name := openapi3.InvalidIdentifierCharRegExp.ReplaceAllString(file+"_"+fragment, "_")
		key := ref.CollectionName() + "/" + name
		if previous, ok := occupied[key]; ok && previous != canonical {
			collision = fmt.Errorf("SDK reference names collide")
		}
		occupied[key] = canonical
		return name
	})
	return collision
}
func sortedKeys[T any](values map[string]T) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
