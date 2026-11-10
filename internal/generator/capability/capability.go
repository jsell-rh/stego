// Package capability is the generator code that the capability generators of commands and queries share
// (ST-SUP-RC32, ST-SUP-RC33): the Go names of a declared name, the read-only value type of an entity on the runtime
// data path with its stored columns, the Go models that describe it, and the import of a fill package. It renders no
// file itself; each generator emits the code in its own namespace.
package capability

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

// MixedCase is the Go mixed case of a name in lower snake or kebab case, with dots as separators: id is ID. It is
// the rule of the domain-probe generator, so a probe type of an emitter has the name that the capability
// generators give it (ST-SUP-RC34-OC03).
func MixedCase(name string) string {
	var b strings.Builder
	for _, part := range strings.FieldsFunc(name, func(r rune) bool { return r == '_' || r == '-' || r == '.' }) {
		if part == "id" {
			b.WriteString("ID")
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	return b.String()
}

// LowerFirst is a Go name with a lower-case first letter, for an unexported name.
func LowerFirst(name string) string {
	if strings.HasPrefix(name, "ID") {
		return "id" + name[2:]
	}
	if name == "" {
		return name
	}
	return strings.ToLower(name[:1]) + name[1:]
}

// SnakeCase is the lower snake case of a declared entity name, such as order_line of OrderLine.
func SnakeCase(name string) string {
	runes := []rune(name)
	var out []rune
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) {
			prev := runes[i-1]
			if unicode.IsLower(prev) || (unicode.IsUpper(prev) && i+1 < len(runes) && unicode.IsLower(runes[i+1])) {
				out = append(out, '_')
			}
		}
		out = append(out, unicode.ToLower(r))
	}
	return string(out)
}

// TableName is the table of an entity, as the PostgreSQL adapter creates it on the runtime data path
// (ST-SUP-RC01-OC41): the plural of the snake case of the entity name.
func TableName(entity string) string {
	s := SnakeCase(entity)
	switch {
	case s == "":
		return s
	case strings.HasSuffix(s, "s"), strings.HasSuffix(s, "x"), strings.HasSuffix(s, "z"), strings.HasSuffix(s, "sh"),
		strings.HasSuffix(s, "ch"):
		return s + "es"
	case strings.HasSuffix(s, "y") && len(s) > 1 && !strings.ContainsRune("aeiou", rune(s[len(s)-2])):
		return s[:len(s)-1] + "ies"
	}
	return s + "s"
}

// Read kinds: how the generated load function reads a column from a stored row.
const (
	ReadText   = "text"
	ReadInt    = "int"
	ReadInt32  = "int32"
	ReadBool   = "bool"
	ReadTime   = "time"
	ReadFloat  = "float"
	ReadBytes  = "bytes"
	ReadJSON   = "json"
	columnName = `^[a-z_][a-z0-9_]{0,62}$`
)

var plainColumn = regexp.MustCompile(columnName)

// Field is one stored column of an entity's read-only value: its column, its Go field and type, how a load reads it,
// the declared field type and its sensitivity (public, pii or secret).
type Field struct {
	Column      string
	GoName      string
	GoType      string
	Read        string
	Type        types.FieldType
	Sensitivity string
}

// Value is the read-only value type of one entity on the runtime data path: its Go type, its table and its stored
// columns, the key id first, then each declared field and the two time columns.
type Value struct {
	Entity types.Entity
	GoType string
	Table  string
	Fields []Field
}

// ValueOf gives the read-only value type of the entity. It refuses a field name that is not a plain lower-case
// column name, or that is a column the runtime keeps for itself, and a field type that has no read.
func ValueOf(entity types.Entity) (Value, error) {
	goType := MixedCase(SnakeCase(entity.Name))
	if !plainColumn.MatchString(SnakeCase(entity.Name)) || goType == "" {
		return Value{}, fmt.Errorf("the entity %s has a name that gives no Go type", entity.Name)
	}
	v := Value{Entity: entity, GoType: goType, Table: TableName(entity.Name)}
	v.Fields = append(v.Fields, Field{Column: "id", GoName: "ID", GoType: "string", Read: ReadText, Type: types.FieldTypeString})
	reserved := map[string]bool{"id": true, "tenant_id": true, "authz_token": true, "version": true, "created_time": true,
		"updated_time": true}
	goNames := map[string]bool{"ID": true, "CreatedTime": true, "UpdatedTime": true}
	for _, f := range entity.Fields {
		if !plainColumn.MatchString(f.Name) || reserved[f.Name] {
			return Value{}, fmt.Errorf("the entity %s has the field %q, which is not a plain lower-case column name or is a column that the runtime keeps", entity.Name, f.Name)
		}
		goType, read, err := goTypeOf(f)
		if err != nil {
			return Value{}, fmt.Errorf("the entity %s: %w", entity.Name, err)
		}
		name := MixedCase(f.Name)
		if goNames[name] {
			return Value{}, fmt.Errorf("the entity %s has two fields with the Go name %s", entity.Name, name)
		}
		goNames[name] = true
		v.Fields = append(v.Fields, Field{Column: f.Name, GoName: name, GoType: goType, Read: read, Type: f.Type,
			Sensitivity: f.Sensitivity})
	}
	v.Fields = append(v.Fields,
		Field{Column: "created_time", GoName: "CreatedTime", GoType: "time.Time", Read: ReadTime, Type: types.FieldTypeTimestamp},
		Field{Column: "updated_time", GoName: "UpdatedTime", GoType: "time.Time", Read: ReadTime, Type: types.FieldTypeTimestamp})
	return v, nil
}

// goTypeOf gives the Go type of a declared field and its read kind. A NULL column reads as the zero value, so no
// field is a pointer.
func goTypeOf(f types.Field) (string, string, error) {
	switch f.Type {
	case types.FieldTypeString, types.FieldTypeEnum, types.FieldTypeRef:
		return "string", ReadText, nil
	case types.FieldTypeInt64:
		return "int64", ReadInt, nil
	case types.FieldTypeInt32:
		return "int32", ReadInt32, nil
	case types.FieldTypeBool:
		return "bool", ReadBool, nil
	case types.FieldTypeTimestamp:
		return "time.Time", ReadTime, nil
	case types.FieldTypeFloat, types.FieldTypeDouble:
		return "float64", ReadFloat, nil
	case types.FieldTypeBytes:
		return "[]byte", ReadBytes, nil
	case types.FieldTypeJsonb:
		return "json.RawMessage", ReadJSON, nil
	}
	return "", "", fmt.Errorf("the field %s has the type %q, which has no Go type", f.Name, f.Type)
}

// GoModel is the Go model of the value type, for the domain-probe generator's loaded types (ST-SUP-RC34-OC01).
func (v Value) GoModel() gen.GoModel {
	model := gen.GoModel{Name: v.Entity.Name, GoType: v.GoType}
	for _, f := range v.Fields {
		model.Fields = append(model.Fields, gen.GoModelField{Name: f.Column, Selection: f.GoName, Type: f.Type})
	}
	return model
}

// fillName is the form of a fill name: a folder name under fills that a Go import path can hold.
var fillName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// FillImport gives the import path of the fill package fills/<fill> of the service's module and the Go name that
// generated code imports it as. It refuses a fill name that is not a folder name a Go import path can hold.
func FillImport(module, fill string) (string, string, error) {
	if !fillName.MatchString(fill) {
		return "", "", fmt.Errorf("the fill name %q is not a lower-case folder name", fill)
	}
	return strings.TrimSuffix(module, "/") + "/fills/" + fill, "fill" + MixedCase(fill), nil
}
