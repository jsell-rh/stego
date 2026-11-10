package command

import (
	"fmt"
	"go/token"
	"strings"

	"github.com/jsell-rh/stego/internal/types"
)

// entity is one entity that a command writes, with its repository declaration (ST-SUP-RP10-LC02).
type entity struct {
	decl         types.Entity
	goName       string
	table        string
	fields       []*entityField
	resourceType string
	read, list   string
	pii          bool
	deep         bool         // a field is a pointer or a slice, so Clone copies it
	parent       *entityField // the reference to the parent resource, or nil
	parentEntity string
	keys         []*entityField // the fields of a unique key besides the ID (ST-SUP-RP10-LC24)
}

// entityField is one declared field of an entity.
type entityField struct {
	decl        types.Field
	goName      string
	goType      string
	pointer     bool
	slice       bool
	sensitivity string
}

// goTypes are the Go types of the declared field types, as the PostgreSQL columns of ST-SUP-RC01-OC41 scan.
var goTypes = map[types.FieldType]string{
	types.FieldTypeString: "string", types.FieldTypeEnum: "string", types.FieldTypeRef: "string",
	types.FieldTypeInt32: "int32", types.FieldTypeInt64: "int64", types.FieldTypeFloat: "float32",
	types.FieldTypeDouble: "float64", types.FieldTypeBool: "bool", types.FieldTypeTimestamp: "time.Time",
	types.FieldTypeBytes: "[]byte", types.FieldTypeJsonb: "[]byte",
}

// planEntity checks one entity that a command writes and builds its repository declaration.
func planEntity(e types.Entity, byName map[string]types.Entity, tenantKey string) (*entity, error) {
	if e.Tenant != "" && e.Tenant != "owned" {
		return nil, fmt.Errorf("the entity is %s; a command writes only entities that a tenant owns", e.Tenant)
	}
	if e.Kind != "" && e.Kind != "data" {
		return nil, fmt.Errorf("the entity is of kind %s; a command writes only data entities", e.Kind)
	}
	p := &entity{decl: e, goName: e.Name, table: tableName(e.Name), resourceType: e.ResourceType}
	if !sqlName.MatchString(p.table) || !token.IsIdentifier(e.Name) || !token.IsExported(e.Name) {
		return nil, fmt.Errorf("the entity name %s does not give a plain table name and an exported Go name", e.Name)
	}
	if e.ResourceType != "" {
		p.read, p.list = e.Permissions[types.OpRead], e.Permissions[types.OpList]
		if p.read == "" || p.read == "public" {
			return nil, fmt.Errorf("the entity has the resource type %s and no read permission", e.ResourceType)
		}
		if p.list == "public" {
			p.list = ""
		}
	}
	var refs []*entityField
	for _, f := range e.Fields {
		if !sqlName.MatchString(f.Name) || f.Name == "id" || f.Name == tenantKey || f.Name == "version" || f.Name == "authz_token" || f.Name == "created_time" || f.Name == "updated_time" {
			return nil, fmt.Errorf("the field name %s is not a plain column name of its own", f.Name)
		}
		goType, ok := goTypes[f.Type]
		if !ok {
			return nil, fmt.Errorf("the field %s has the type %s, which a command cannot write", f.Name, f.Type)
		}
		if f.Computed {
			return nil, fmt.Errorf("the field %s is computed, which a command cannot write", f.Name)
		}
		ef := &entityField{decl: f, goName: mixedCase(f.Name), goType: goType, sensitivity: f.Sensitivity}
		ef.slice = strings.HasPrefix(goType, "[]")
		if f.Optional && !ef.slice {
			ef.pointer = true
			ef.goType = "*" + goType
		}
		if ef.pointer || ef.slice {
			p.deep = true
		}
		switch f.Sensitivity {
		case "", "none":
		case "pii":
			p.pii = true
		case "secret":
		default:
			return nil, fmt.Errorf("the field %s has the sensitivity %s, which is not pii or secret", f.Name, f.Sensitivity)
		}
		if f.Unique || len(f.UniqueComposite) > 0 {
			p.keys = append(p.keys, ef)
		}
		if f.Type == types.FieldTypeRef && byName[f.To].ResourceType != "" {
			refs = append(refs, ef)
		}
		p.fields = append(p.fields, ef)
	}
	// An entity whose one reference names an entity with a resource type goes under that parent: its create, and a
	// move of a row to another parent, decide the command's permission on the parent that the row names, whether or
	// not the entity has a resource type of its own (ST-SUP-RP10-LC05). Two such references, or an optional one,
	// would leave a parent that nothing decides on, so they are refused.
	switch {
	case len(refs) > 1:
		names := make([]string, len(refs))
		for i, r := range refs {
			names[i] = r.decl.Name
		}
		return nil, fmt.Errorf("the fields %s refer to entities with a resource type; a command decides on one parent of a row, so the entity may refer to one such entity only (ST-SUP-Q79)", strings.Join(names, ", "))
	case len(refs) == 1 && refs[0].pointer:
		return nil, fmt.Errorf("the optional field %s refers to %s, which has a resource type; a command decides on the parent that a row names, so the reference must be required (ST-SUP-Q79)", refs[0].decl.Name, refs[0].decl.To)
	case len(refs) == 1:
		p.parent, p.parentEntity = refs[0], refs[0].decl.To
	}
	return p, nil
}

// columns is the column list that each statement of the entity returns, in the order that Scan reads: the ID as
// text, the fields, the resource version and the authorization token, which an entity with no resource type does
// not have.
func (e *entity) columns() string {
	cols := []string{`"id"::text`}
	for _, f := range e.fields {
		cols = append(cols, quote(f.decl.Name))
	}
	cols = append(cols, `"version"`)
	if e.resourceType != "" {
		cols = append(cols, `"authz_token"`)
	} else {
		cols = append(cols, `''::text`)
	}
	return strings.Join(cols, ", ")
}

// statements gives the statements of the entity's repository (ST-SUP-RP10-LC23, ST-SUP-RP10-LC24): each takes the
// tenant as $1.
func (e *entity) statements(tenantKey string) (selectRow, forUpdate, forDelete, insert, update, remove string) {
	table := "public." + quote(e.table)
	where := fmt.Sprintf("%s = $1 AND \"id\" = $2::uuid", quote(tenantKey))
	selectRow = fmt.Sprintf("SELECT %s FROM %s WHERE %s", e.columns(), table, where)
	forUpdate = selectRow + " FOR NO KEY UPDATE"
	forDelete = selectRow + " FOR UPDATE"
	names := []string{quote(tenantKey), `"id"`}
	params := []string{"$1", "$2::uuid"}
	sets := make([]string, 0, len(e.fields)+2)
	for i, f := range e.fields {
		names = append(names, quote(f.decl.Name))
		params = append(params, fmt.Sprintf("$%d", i+3))
		sets = append(sets, fmt.Sprintf("%s = $%d", quote(f.decl.Name), i+5))
	}
	sets = append(sets, `"version" = "version" + 1`, `"updated_time" = now()`)
	insert = fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) RETURNING %s", table, strings.Join(names, ", "), strings.Join(params, ", "), e.columns())
	// $4 is the ID of the values, which an update does not change.
	update = fmt.Sprintf("UPDATE %s SET %s WHERE %s AND \"version\" = $3 AND \"id\" = $4::uuid RETURNING %s", table, strings.Join(sets, ", "), where, e.columns())
	remove = fmt.Sprintf("DELETE FROM %s WHERE %s AND \"version\" = $3", table, where)
	return
}

func quote(name string) string { return `"` + name + `"` }

// renderEntity writes the Go type, the repository declaration and the repository accessor of one entity.
func renderEntity(w *writer, e *entity, tenantKey string) {
	w.f("// %s is a row of the entity %s, as its command handle reads and writes it.\ntype %s struct {\n\tID string\n", e.goName, e.decl.Name, e.goName)
	for _, f := range e.fields {
		w.f("\t%s %s\n", f.goName, f.goType)
	}
	w.f("}\n\n")
	w.f("// Loaded%s is a loaded value of %s: only a read or a write through a command handle makes one.\ntype Loaded%s = command.Loaded[%s]\n\n", e.goName, e.decl.Name, e.goName, e.goName)
	selectRow, forUpdate, forDelete, insert, update, remove := e.statements(tenantKey)
	w.f("// %sEntity is the repository declaration of %s (ST-SUP-RP10-LC02).\n", e.goName, e.decl.Name)
	w.f("var %sEntity = mustEntity(command.NewEntity(command.EntitySpec[%s]{\n", e.goName, e.goName)
	w.f("\tName: %q,\n", e.decl.Name)
	if e.resourceType != "" {
		w.f("\tResourceType: %q,\n\tReadPermission: %q,\n", e.resourceType, e.read)
		if e.list != "" {
			w.f("\tListPermission: %q,\n", e.list)
		}
	}
	var sensitive []string
	for _, f := range e.fields {
		switch f.sensitivity {
		case "pii":
			sensitive = append(sensitive, fmt.Sprintf("%q: audit.PII", f.decl.Name))
		case "secret":
			sensitive = append(sensitive, fmt.Sprintf("%q: audit.Secret", f.decl.Name))
		}
	}
	if len(sensitive) > 0 {
		w.f("\tFields: audit.Fields{%s},\n", strings.Join(sensitive, ", "))
	}
	if e.pii {
		w.f("\tPII: true,\n")
	}
	w.f("\tSelect: %q,\n\tSelectForUpdate: %q,\n\tSelectForDelete: %q,\n", selectRow, forUpdate, forDelete)
	w.f("\tInsert: %q,\n\tUpdate: %q,\n\tDelete: %q,\n", insert, update, remove)
	// Scan reads the columns of the statements.
	w.f("\tScan: func(row command.Scanner) (%s, command.Meta, error) {\n\t\tvar v %s\n\t\tvar m command.Meta\n", e.goName, e.goName)
	targets := []string{"&v.ID"}
	for _, f := range e.fields {
		targets = append(targets, "&v."+f.goName)
	}
	targets = append(targets, "&m.Version", "&m.Token")
	w.f("\t\terr := row.Scan(%s)\n\t\tm.ID = v.ID\n\t\treturn v, m, err\n\t},\n", strings.Join(targets, ", "))
	// Values gives the ID and the field values; a new row with no ID gets a new one.
	values := []string{"id"}
	for _, f := range e.fields {
		values = append(values, "v."+f.goName)
	}
	w.f("\tValues: func(v %s) []any {\n\t\tid := v.ID\n\t\tif id == \"\" {\n\t\t\tid = NewID()\n\t\t}\n\t\treturn []any{%s}\n\t},\n", e.goName, strings.Join(values, ", "))
	state := []string{`"id": v.ID`}
	for _, f := range e.fields {
		state = append(state, fmt.Sprintf("%q: v.%s", f.decl.Name, f.goName))
	}
	w.f("\tState: func(v %s) map[string]any {\n\t\treturn map[string]any{%s}\n\t},\n", e.goName, strings.Join(state, ", "))
	changed := []string{"before.ID != after.ID"}
	for _, f := range e.keys {
		if f.slice {
			changed = append(changed, fmt.Sprintf("!bytes.Equal(before.%s, after.%s)", f.goName, f.goName))
			continue
		}
		if f.pointer {
			changed = append(changed, fmt.Sprintf("!equalPointer(before.%s, after.%s)", f.goName, f.goName))
			continue
		}
		changed = append(changed, fmt.Sprintf("before.%s != after.%s", f.goName, f.goName))
	}
	w.f("\tKeyChanged: func(before, after %s) bool {\n\t\treturn %s\n\t},\n", e.goName, strings.Join(changed, " || "))
	if !e.deep {
		w.f("\tClone: func(v %s) %s { return v },\n", e.goName, e.goName)
	} else {
		w.f("\tClone: func(v %s) %s {\n", e.goName, e.goName)
		for _, f := range e.fields {
			switch {
			case f.slice:
				w.f("\t\tv.%s = bytes.Clone(v.%s)\n", f.goName, f.goName)
			case f.pointer:
				w.f("\t\tv.%s = clonePointer(v.%s)\n", f.goName, f.goName)
			}
		}
		w.f("\t\treturn v\n\t},\n")
	}
	w.f("\tValidID: ValidUUID,\n")
	if e.parent != nil {
		w.f("\tParent: func(v %s) (string, string) { return %q, v.%s },\n", e.goName, e.parentEntity, e.parent.goName)
	}
	w.f("}))\n\n")
	w.f("// %sRepository gives the repository of %s of a command handle; the handle refuses an entity that its command does\n// not declare it writes.\n", e.goName, e.decl.Name)
	w.f("func %sRepository[P any](h *command.Handle[P]) (*command.Repository[%s], error) {\n\treturn command.Repo(h, %sEntity)\n}\n\n", e.goName, e.goName, e.goName)
}
