package compiler

import (
	"bytes"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/jsell-rh/stego/internal/parser"
	"github.com/jsell-rh/stego/internal/types"
	"gopkg.in/yaml.v3"
)

// MissingAddSteps gives the schema changes of kind add that apply writes before it plans (ST-GEN-UC12-R06): one
// for each new entity, and for each new field that is optional or has a default on an entity of the base, that no
// step declares, in the order of the declaration. A new field with the shape of a field that the head removes is a
// rename, which apply does not write. Each change ID is add-<entity>-<field> or add-<entity> in lower kebab case,
// with the lowest number suffix from 2 that makes it unique, so that the same declaration always gets the same
// steps. With no record at the base, no release has run and no step is missing.
func MissingAddSteps(base BaseSource, head *types.ServiceDeclaration) ([]types.SchemaChange, error) {
	if base.Record == nil || base.Declaration == nil {
		return nil, nil
	}
	state, err := decodeState(base.Record, ".stego/state.yaml at "+base.Ref)
	if err != nil {
		return nil, err
	}
	if state.LastApplied == nil || HashBytes(base.Declaration) != state.LastApplied.ServiceHash {
		return nil, nil
	}
	declaration, err := parser.ParseServiceDeclarationFromBytes(base.Declaration, "service.yaml at "+base.Ref)
	if err != nil {
		return nil, err
	}
	old, now := shapeAt(declaration, len(declaration.SchemaHistory)+1), shapeAt(head, len(declaration.SchemaHistory))
	used := map[string]bool{}
	for _, change := range head.SchemaChanges {
		used[change.ID] = true
	}
	unique := func(id string) string {
		name := id
		for n := 2; used[name]; n++ {
			name = id + "-" + strconv.Itoa(n)
		}
		used[name] = true
		return name
	}
	var steps []types.SchemaChange
	for _, entity := range now {
		at := slices.IndexFunc(old, func(e *storedEntity) bool { return e.Name == entity.Name })
		if at < 0 {
			steps = append(steps, types.SchemaChange{ID: unique("add-" + kebab(entity.Name)), Kind: "add", Entity: entity.Name})
			continue
		}
		before := old[at]
		if before.Loose || entity.Loose {
			continue
		}
		var removed []storedField
		for _, field := range before.Fields {
			if entity.field(field.Name) < 0 {
				removed = append(removed, field)
			}
		}
		for _, field := range entity.Fields {
			if before.field(field.Name) >= 0 || field.Loose || (!field.Shape.Optional && field.Shape.Default == nil) {
				continue
			}
			if slices.ContainsFunc(removed, func(r storedField) bool { return !r.Loose && reflect.DeepEqual(r.Shape, field.Shape) }) {
				continue
			}
			steps = append(steps, types.SchemaChange{ID: unique("add-" + kebab(entity.Name) + "-" + kebab(field.Name)), Kind: "add", Entity: entity.Name, Field: field.Name})
		}
	}
	return steps, nil
}

// generatedSchemaKinds are the kinds of schema change whose steps generation carries out. The kinds rename,
// retype, require, drop and unique have no generated step migration yet, so generation refuses them rather than
// write code that does not move the rows; each kind joins the list in the change that generates its steps. An add
// generates the table or the column as before (ST-GEN-UC12-R06).
var generatedSchemaKinds = map[string]bool{"add": true}

// unbuiltSchemaChanges refuses a declaration with a schema change of a kind that generation does not carry out
// yet, naming the change and its kind. Checking still accepts it (stego validate), so the author can declare it.
func unbuiltSchemaChanges(service *types.ServiceDeclaration) error {
	var refused []string
	for _, change := range service.SchemaChanges {
		if !generatedSchemaKinds[change.Kind] {
			refused = append(refused, fmt.Sprintf("%s (%s)", change.ID, change.Kind))
		}
	}
	if len(refused) == 0 {
		return nil
	}
	return fmt.Errorf("generation does not carry out the schema change %s yet: STEGO generates no migration, trigger or backfill for that kind, so it writes no code for the declaration; plan and apply wait until the kind is built", strings.Join(refused, ", "))
}

// WithSchemaSteps gives a copy of the declaration with each change and its expand step at the end of the schema
// history.
func WithSchemaSteps(service *types.ServiceDeclaration, changes []types.SchemaChange) *types.ServiceDeclaration {
	copied := *service
	copied.SchemaChanges = append(slices.Clone(service.SchemaChanges), changes...)
	copied.SchemaHistory = slices.Clone(service.SchemaHistory)
	for _, change := range changes {
		copied.SchemaHistory = append(copied.SchemaHistory, types.SchemaStep{Change: change.ID, Phase: "expand"})
	}
	return &copied
}

// AppendSchemaSteps writes each change and its expand step at the end of the lists schema_changes and
// schema_history of the declaration's text, and keeps every other byte. A list that is missing is added at the end
// of the document. It refuses a list that is not a block sequence at the top level, because only that form keeps
// the text around it.
func AppendSchemaSteps(data []byte, changes []types.SchemaChange) ([]byte, error) {
	if len(changes) == 0 {
		return data, nil
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("service.yaml is not one mapping, so apply cannot write its schema steps")
	}
	var changeLines, stepLines []string
	for _, change := range changes {
		entry := "{id: " + change.ID + ", kind: " + change.Kind + ", entity: " + change.Entity
		if change.Field != "" {
			entry += ", field: " + change.Field
		}
		changeLines = append(changeLines, entry+"}")
		stepLines = append(stepLines, "{change: "+change.ID+", phase: expand}")
	}
	lines := strings.SplitAfter(string(data), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > 0 && !strings.HasSuffix(lines[len(lines)-1], "\n") {
		lines[len(lines)-1] += "\n"
	}
	insertions := []struct {
		key     string
		entries []string
	}{{"schema_changes", changeLines}, {"schema_history", stepLines}}
	type insertion struct {
		at   int
		text []string
	}
	var planned []insertion
	var appended []string
	root := document.Content[0]
	for _, list := range insertions {
		value := (*yaml.Node)(nil)
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value == list.key {
				value = root.Content[i+1]
			}
		}
		if value == nil {
			appended = append(appended, list.key+":\n")
			for _, entry := range list.entries {
				appended = append(appended, "  - "+entry+"\n")
			}
			continue
		}
		if value.Kind != yaml.SequenceNode || value.Style&yaml.FlowStyle != 0 || len(value.Content) == 0 {
			return nil, fmt.Errorf("service.yaml gives %s as a flow or empty list; write it as a block list, one item on each line, so that apply can add its schema steps", list.key)
		}
		indent := strings.Repeat(" ", value.Content[0].Column-3)
		end := blockEnd(lines, value.Line-1)
		var text []string
		for _, entry := range list.entries {
			text = append(text, indent+"- "+entry+"\n")
		}
		planned = append(planned, insertion{at: end, text: text})
	}
	// Insert at the later line first, so that the line numbers of the earlier insertion stay right.
	slices.SortFunc(planned, func(a, b insertion) int { return b.at - a.at })
	for _, p := range planned {
		lines = slices.Insert(lines, p.at, p.text...)
	}
	lines = append(lines, appended...)
	var out bytes.Buffer
	for _, line := range lines {
		out.WriteString(line)
	}
	return out.Bytes(), nil
}

// blockEnd gives the index of the first line after the block that starts at the line first: the next line that
// starts with a character that is not a space, a dash or a comment mark, or the end of the text. Blank lines and
// comments at the end of the block stay after it.
func blockEnd(lines []string, first int) int {
	end := first + 1
	for i := first + 1; i < len(lines); i++ {
		trimmed := strings.TrimRight(lines[i], "\r\n")
		if trimmed == "" || strings.HasPrefix(strings.TrimLeft(trimmed, " "), "#") {
			continue
		}
		if trimmed[0] != ' ' && trimmed[0] != '-' {
			break
		}
		end = i + 1
	}
	return end
}
