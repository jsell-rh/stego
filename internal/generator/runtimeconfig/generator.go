// Package runtimeconfig generates typed, bounded environment configuration.
package runtimeconfig

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"go/format"
	"path"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jsell-rh/stego/internal/gen"
)

//go:embed configuration.go.tmpl
var source string

type Generator struct{}

type declaration struct {
	Groups []group `json:"groups"`
}

type group struct {
	Name   string  `json:"name"`
	Fields []field `json:"fields"`
}

type field struct {
	Name      string  `json:"name"`
	Env       string  `json:"env"`
	Type      string  `json:"type"`
	Default   *string `json:"default,omitempty"`
	Min       *string `json:"min,omitempty"`
	Max       *string `json:"max,omitempty"`
	MinLength *int    `json:"min_length,omitempty"`
	MaxLength *int    `json:"max_length,omitempty"`
}

var exportedName = regexp.MustCompile(`^[A-Z][A-Za-z0-9]{0,63}$`)
var environmentName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)
var errDeclaration = errors.New("invalid runtime-configuration declaration")

// No input values enter compiler errors. Defaults can contain private values.
func prepare(ctx gen.Context) (declaration, error) {
	var result declaration
	if err := gen.ValidateGoPackageNamespace(ctx.OutputNamespace); err != nil {
		return result, err
	}
	raw, err := json.Marshal(ctx.ComponentConfig)
	if err != nil || len(raw) > 128*1024 {
		return result, errDeclaration
	}
	var normalized any
	if json.Unmarshal(raw, &normalized) != nil || containsNull(normalized) {
		return result, errDeclaration
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || len(result.Groups) < 1 || len(result.Groups) > 32 {
		return declaration{}, errDeclaration
	}
	// Check all emitted package names, including loaders, before generation.
	symbols := map[string]bool{"Error": true, "ErrConfiguration": true}
	total := 0
	for _, group := range result.Groups {
		if !exportedName.MatchString(group.Name) || len(group.Fields) < 1 || len(group.Fields) > 64 {
			return declaration{}, errDeclaration
		}
		for _, name := range []string{group.Name, "Load" + group.Name, "Read" + group.Name} {
			if symbols[name] {
				return declaration{}, errDeclaration
			}
			symbols[name] = true
		}
		names := map[string]bool{"String": true, "GoString": true, "Format": true, "MarshalJSON": true}
		envs := map[string]bool{}
		for _, field := range group.Fields {
			total++
			if total > 256 || !exportedName.MatchString(field.Name) || names[field.Name] || !environmentName.MatchString(field.Env) || envs[field.Env] || !validField(field) {
				return declaration{}, errDeclaration
			}
			names[field.Name], envs[field.Env] = true, true
		}
	}
	return result, nil
}

func containsNull(value any) bool {
	switch value := value.(type) {
	case nil:
		return true
	case map[string]any:
		for _, child := range value {
			if containsNull(child) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if containsNull(child) {
				return true
			}
		}
	}
	return false
}

func validText(value string) bool {
	return len(value) <= 4096 && utf8.ValidString(value) && strings.IndexFunc(value, unicode.IsControl) == -1
}

func integer(value string) (int64, bool) {
	n, err := strconv.ParseInt(value, 10, 64)
	return n, err == nil && strconv.FormatInt(n, 10) == value
}

func validField(f field) bool {
	if f.Default != nil && !validText(*f.Default) {
		return false
	}
	switch f.Type {
	case "string":
		if f.Min != nil || f.Max != nil || f.MinLength == nil || f.MaxLength == nil || *f.MinLength < 0 || *f.MaxLength < 1 || *f.MaxLength > 4096 || *f.MinLength > *f.MaxLength {
			return false
		}
		return f.Default == nil || len(*f.Default) >= *f.MinLength && len(*f.Default) <= *f.MaxLength
	case "integer", "duration":
		if f.Min == nil || f.Max == nil || f.MinLength != nil || f.MaxLength != nil || len(*f.Min) > 64 || len(*f.Max) > 64 {
			return false
		}
		parse := integer
		if f.Type == "duration" {
			parse = func(value string) (int64, bool) { n, err := time.ParseDuration(value); return int64(n), err == nil }
		}
		if f.Type == "duration" && f.Default != nil && len(*f.Default) > 64 {
			return false
		}
		minimum, okMin := parse(*f.Min)
		maximum, okMax := parse(*f.Max)
		if !okMin || !okMax || minimum > maximum {
			return false
		}
		if f.Default != nil {
			n, ok := parse(*f.Default)
			return ok && n >= minimum && n <= maximum
		}
		return true
	case "boolean":
		return f.Min == nil && f.Max == nil && f.MinLength == nil && f.MaxLength == nil && (f.Default == nil || *f.Default == "true" || *f.Default == "false")
	}
	return false
}

func (*Generator) ValidateContext(ctx gen.Context) error {
	_, err := prepare(ctx)
	return err
}

func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	config, err := prepare(ctx)
	if err != nil {
		return nil, nil, err
	}
	functions := template.FuncMap{
		"quote": strconv.Quote,
		"value": func(value *string) string { return *value },
		"kind": func(value string) string {
			return map[string]string{"string": "string", "integer": "int64", "duration": "time.Duration", "boolean": "bool"}[value]
		},
		"reader": func(f field) string {
			switch f.Type {
			case "string":
				return fmt.Sprintf("readString(raw, %d, %d)", *f.MinLength, *f.MaxLength)
			case "integer":
				return fmt.Sprintf("readInteger(raw, %s, %s)", *f.Min, *f.Max)
			case "duration":
				minimum, _ := time.ParseDuration(*f.Min)
				maximum, _ := time.ParseDuration(*f.Max)
				return fmt.Sprintf("readDuration(raw, %d, %d)", minimum, maximum)
			default:
				return "readBoolean(raw)"
			}
		},
	}
	t, err := template.New("configuration").Funcs(functions).Parse(source)
	if err != nil {
		return nil, nil, err
	}
	var output bytes.Buffer
	if err := t.Execute(&output, struct {
		Package string
		Groups  []group
	}{path.Base(ctx.OutputNamespace), config.Groups}); err != nil {
		return nil, nil, err
	}
	code, err := format.Source(output.Bytes())
	if err != nil {
		return nil, nil, errors.New("cannot format runtime configuration")
	}
	files := []gen.File{{Path: path.Join(ctx.OutputNamespace, "configuration.go"), Content: code}}
	if err := gen.ValidateNamespace(ctx.OutputNamespace, files); err != nil {
		return nil, nil, err
	}
	return files, &gen.Wiring{}, nil
}
