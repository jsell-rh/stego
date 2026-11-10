// Package domainprobe generates the typed domain probes of a service
// (ST-SUP-RC34): from the declared observations, the enum types, one probe
// type for each emitter with one typed method for each observation it emits,
// the constructors that the handles, the slot components and the fill tests
// bind a probe with, and the catalog observations.json. The emit function,
// the sinks and Recorder live in the runtime library github.com/jsell-rh/stego/registry/runtime/probe
// (ST-SUP-RP12); the generated code holds only what varies with the
// declaration (ST-STD-RUNTIME-R05).
package domainprobe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/format"
	"go/token"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

// Generator is the generator of the domain-probe component.
type Generator struct{}

// MinimumGoVersion is the Go version of the runtime module that the probes import.
func (*Generator) MinimumGoVersion() string { return "1.26.10" }

// ValidateContext checks the declared observations and emitters before any code is rendered.
func (*Generator) ValidateContext(ctx gen.Context) error {
	_, err := plan(ctx)
	return err
}

// Generate emits the probe package and observations.json in the component's output namespace.
func (*Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	m, err := plan(ctx)
	if err != nil {
		return nil, nil, err
	}
	source, err := format.Source(render(m))
	if err != nil {
		return nil, nil, fmt.Errorf("domain-probe: formatting the probe package: %w", err)
	}
	catalog, err := renderCatalog(m)
	if err != nil {
		return nil, nil, err
	}
	files := []gen.File{
		{Path: path.Join(ctx.OutputNamespace, "probes.go"), Content: source},
		{Path: path.Join(ctx.OutputNamespace, "observations.json"), Content: catalog},
	}
	if err := gen.ValidateNamespace(ctx.OutputNamespace, files); err != nil {
		return nil, nil, err
	}
	return files, &gen.Wiring{}, nil
}

// The import paths of the runtime library that the probe package uses.
const (
	probeImport   = "github.com/jsell-rh/stego/registry/runtime/probe"
	commandImport = "github.com/jsell-rh/stego/registry/runtime/command"
)

// fieldKind is the kind of one declared field.
type fieldKind uint8

const (
	entityField fieldKind = iota + 1
	enumField
	boolField
	measureField
)

// field is one checked field of an observation.
type field struct {
	name   string
	kind   fieldKind
	param  string // the method parameter, for a field that is not an entity field
	entity *loadedType
	// selection and value are the Go field of the entity model and its probe value kind: Text, Integer or Bool.
	selection, value string
	pointer          bool
	enumType         string
	values           []string
	unit             string
	min, max         int64
}

// loadedType is one entity that a field reads, with its generated loaded type.
type loadedType struct {
	entity, goType, alias, helper string
}

// observation is one checked observation.
type observation struct {
	decl     types.Observation
	method   string
	variable string
	kind     string
	severity string
	counts   string
	fields   []field
	// entities are the loaded values that the method takes first, in the order the fields first name them.
	entities []*loadedType
	params   []string // the method parameter names of entities
	value    string   // the histogram value name and its parameter
	valueArg string
	unit     string
}

// emitter is one checked emitter with the observations that name it.
type emitter struct {
	name, goName string
	kind         gen.EmitterKind
	methods      []*observation
}

// model is what the generator renders.
type model struct {
	pkg          string
	entityImport string
	loaded       []*loadedType
	observations []*observation
	emitters     []*emitter
}

var (
	snakeName   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	emitterText = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)
	enumType    = regexp.MustCompile(`^enum\((.*)\)$`)
	measureType = regexp.MustCompile(`^measure\(\s*([A-Za-z0-9]+)\s*,\s*(-?[0-9]+)\s*\.\.\s*(-?[0-9]+)\s*\)$`)
	entityType  = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\.([a-z][a-z0-9_]*)$`)
)

var (
	kinds      = map[string]string{"event": "Event", "counter": "Counter", "histogram": "Histogram"}
	severities = map[string]string{"debug": "Debug", "info": "Info", "warn": "Warn", "error": "Error"}
	units      = map[string]string{"s": "Seconds", "ms": "Milliseconds", "By": "Bytes", "1": "One"}
	reserved   = map[string]bool{"value": true, "trace_id": true, "span_id": true, "tenant": true, "principal": true, "operation": true}
)

// plan checks the context and builds the model. It refuses, naming the observation, each declaration that the
// probe runtime's Declare would refuse, so the emitted descriptors never fail at start (ST-DECL-INV48 to
// ST-DECL-INV55).
func plan(ctx gen.Context) (*model, error) {
	if err := gen.ValidateGoPackageNamespace(ctx.OutputNamespace); err != nil {
		return nil, err
	}
	if len(ctx.ComponentConfig) != 0 {
		return nil, fmt.Errorf("domain-probe has no component settings")
	}
	m := &model{pkg: path.Base(ctx.OutputNamespace)}
	names := newNames()
	byName := map[string]*emitter{}
	for _, e := range ctx.Emitters {
		switch e.Kind {
		case gen.EmitterCommand, gen.EmitterQuery, gen.EmitterWorker, gen.EmitterProcessMethod, gen.EmitterSlotBinding:
		default:
			return nil, fmt.Errorf("domain-probe: the emitter %q has the unknown kind %q", e.Name, e.Kind)
		}
		if !emitterText.MatchString(e.Name) || byName[e.Name] != nil {
			return nil, fmt.Errorf("domain-probe: the emitter %q is not well formed or is repeated", e.Name)
		}
		em := &emitter{name: e.Name, goName: mixedCase(e.Name), kind: e.Kind}
		exported := []string{em.goName + "Probe", "New" + em.goName + "Probe", "Record" + em.goName}
		if e.Kind == gen.EmitterSlotBinding {
			exported = append(exported, "Bind"+em.goName, em.goName+"ProbeFrom")
		}
		if err := names.take("the emitter "+e.Name, exported...); err != nil {
			return nil, err
		}
		byName[e.Name] = em
		m.emitters = append(m.emitters, em)
	}
	entities := map[string]types.Entity{}
	for _, e := range ctx.Entities {
		entities[e.Name] = e
	}
	loaded := map[string]*loadedType{}
	seen := map[string]bool{}
	for _, decl := range ctx.Observations {
		o, err := checkObservation(ctx, m, decl, entities, loaded, names)
		if err != nil {
			return nil, err
		}
		if seen[decl.Name] {
			return nil, fmt.Errorf("domain-probe: the observation name %s is used twice", decl.Name)
		}
		seen[decl.Name] = true
		for _, name := range decl.EmittedBy {
			byName[name].methods = append(byName[name].methods, o)
		}
		m.observations = append(m.observations, o)
	}
	return m, nil
}

// checkObservation checks one observation and makes its model.
func checkObservation(ctx gen.Context, m *model, decl types.Observation, entities map[string]types.Entity,
	loaded map[string]*loadedType, names *nameSet) (*observation, error) {
	refuse := func(format string, args ...any) (*observation, error) {
		return nil, fmt.Errorf("domain-probe: observation "+strconv.Quote(decl.Name)+": "+format, args...)
	}
	if !snakeName.MatchString(decl.Name) {
		return refuse("the name is not lower snake case of at most 63 characters")
	}
	o := &observation{decl: decl, method: mixedCase(decl.Name), variable: "obs" + mixedCase(decl.Name)}
	if o.kind = kinds[decl.Kind]; o.kind == "" {
		return refuse("the kind %q is not event, counter or histogram", decl.Kind)
	}
	if o.severity = severities[decl.Severity]; o.severity == "" {
		return refuse("the severity %q is not debug, info, warn or error", decl.Severity)
	}
	switch decl.Counts {
	case "", "attempt":
		o.counts = "CountsAttempt"
	case "committed":
		o.counts = "CountsCommitted"
	default:
		return refuse("counts %q is not attempt or committed", decl.Counts)
	}
	if decl.OutcomeMeasured != "" && decl.Counts == "" {
		return refuse("it names an outcome measured and does not state counts")
	}
	if decl.Kind == "histogram" {
		name, unit, ok := strings.Cut(decl.Value, ":")
		name, unit = strings.TrimSpace(name), strings.TrimSpace(unit)
		if !ok || !snakeName.MatchString(name) || units[unit] == "" || reserved[name] {
			return refuse("a histogram names one value as name: unit with the unit s, ms, By or 1")
		}
		o.value, o.unit = name, unit
	} else if decl.Value != "" {
		return refuse("only a histogram names a value")
	}
	if len(decl.Fields) > 8 {
		return refuse("it has more than eight fields")
	}
	if len(decl.EmittedBy) == 0 || len(decl.EmittedBy) > 127 {
		return refuse("it names no emitter or more than 127")
	}
	emitted := map[string]bool{}
	for _, name := range decl.EmittedBy {
		if emitted[name] {
			return refuse("the emitter %q is named twice", name)
		}
		emitted[name] = true
		if !hasEmitter(ctx.Emitters, name) {
			return refuse("the emitter %q is not a command, query, worker, process method or slot binding of the declaration", name)
		}
	}
	params := map[string]bool{"stegoProbe": true}
	param := func(name string) (string, error) {
		p := lowerMixedCase(name)
		if token.IsKeyword(p) || isPredeclared(p) {
			p += "Value"
		}
		if params[p] {
			return "", fmt.Errorf("domain-probe: observation %q: the parameter %s is used twice", decl.Name, p)
		}
		params[p] = true
		return p, nil
	}
	series := 1
	fieldNames := map[string]bool{}
	for _, f := range decl.Fields {
		if !snakeName.MatchString(f.Name) || reserved[f.Name] || fieldNames[f.Name] || f.Name == o.value {
			return refuse("the field %q is not well formed, is reserved or is repeated", f.Name)
		}
		fieldNames[f.Name] = true
		c, err := checkField(ctx, m, decl, f, entities, loaded, names)
		if err != nil {
			return nil, err
		}
		if c.kind != entityField {
			if c.param, err = param(f.Name); err != nil {
				return nil, err
			}
		} else if !containsLoaded(o.entities, c.entity) {
			o.entities = append(o.entities, c.entity)
		}
		if decl.Kind != "event" {
			switch c.kind {
			case enumField:
				series *= len(c.values)
			case boolField:
				series *= 2
			default:
				return refuse("the field %s of a counter or a histogram is not an enum or a bool", f.Name)
			}
			if series > 127 {
				return refuse("more than 127 series")
			}
		}
		o.fields = append(o.fields, c)
	}
	if decl.Kind != "event" {
		if o.counts == "CountsAttempt" {
			series *= 2
		}
		if series *= len(decl.EmittedBy); series > 127 {
			return refuse("%d series is more than 127", series)
		}
	}
	for _, l := range o.entities {
		p, err := param(lowerMixedCase(l.entity))
		if err != nil {
			return nil, err
		}
		o.params = append(o.params, p)
	}
	if o.value != "" {
		var err error
		if o.valueArg, err = param(o.value); err != nil {
			return nil, err
		}
	}
	return o, nil
}

// checkField checks the type of one field: Entity.field, enum(a | b), bool or measure(unit, min..max).
func checkField(ctx gen.Context, m *model, decl types.Observation, f types.ObservationField, entities map[string]types.Entity,
	loaded map[string]*loadedType, names *nameSet) (field, error) {
	refuse := func(format string, args ...any) (field, error) {
		return field{}, fmt.Errorf("domain-probe: observation %q, field %q: "+format, append([]any{decl.Name, f.Name}, args...)...)
	}
	text := strings.TrimSpace(f.Type)
	c := field{name: f.Name}
	switch {
	case text == "bool":
		c.kind = boolField
	case enumType.MatchString(text):
		c.kind = enumField
		c.enumType = mixedCase(decl.Name) + mixedCase(f.Name)
		set := map[string]bool{}
		exported := []string{c.enumType}
		for _, v := range strings.Split(enumType.FindStringSubmatch(text)[1], "|") {
			v = strings.TrimSpace(v)
			if !snakeName.MatchString(v) || set[v] {
				return refuse("the value %q is not lower snake case or is repeated", v)
			}
			set[v] = true
			c.values = append(c.values, v)
			exported = append(exported, c.enumType+mixedCase(v))
		}
		if len(c.values) > 32 {
			return refuse("an enum has one to 32 values")
		}
		if err := names.take("the field "+f.Name+" of "+decl.Name, exported...); err != nil {
			return field{}, err
		}
	case measureType.MatchString(text):
		parts := measureType.FindStringSubmatch(text)
		c.kind, c.unit = measureField, parts[1]
		var err1, err2 error
		c.min, err1 = strconv.ParseInt(parts[2], 10, 64)
		c.max, err2 = strconv.ParseInt(parts[3], 10, 64)
		if units[c.unit] == "" || err1 != nil || err2 != nil || c.min > c.max {
			return refuse("a measure has the unit s, ms, By or 1 and a range min..max of integers")
		}
	case entityType.MatchString(text):
		parts := entityType.FindStringSubmatch(text)
		entity, ok := entities[parts[1]]
		if !ok {
			return refuse("the entity %s is not declared", parts[1])
		}
		declared := parts[2] == "id"
		ftype := types.FieldTypeString
		for _, ef := range entity.Fields {
			if ef.Name == parts[2] {
				declared, ftype = true, ef.Type
			}
		}
		if !declared {
			return refuse("the entity %s has no field %s", entity.Name, parts[2])
		}
		switch ftype {
		case types.FieldTypeString, types.FieldTypeEnum, types.FieldTypeRef:
			c.value = "text"
		case types.FieldTypeInt32, types.FieldTypeInt64:
			c.value = "integer"
		case types.FieldTypeBool:
			c.value = "flag"
		default:
			return refuse("the entity field %s.%s has the type %s, which is not text, an integer or bool", entity.Name, parts[2], ftype)
		}
		l, err := loadedOf(ctx, m, entity.Name, loaded, names)
		if err != nil {
			return refuse("%v", err)
		}
		selection, pointer, err := selectionOf(ctx, m.entityImport, entity.Name, parts[2])
		if err != nil {
			return refuse("%v", err)
		}
		c.kind, c.entity, c.selection, c.pointer = entityField, l, selection, pointer
	default:
		return refuse("the type %q is not Entity.field, enum(a | b), bool or measure(unit, min..max)", f.Type)
	}
	return c, nil
}

// loadedOf gives the loaded type of the entity, and finds the Go model provider of the entities the first time.
func loadedOf(ctx gen.Context, m *model, entity string, loaded map[string]*loadedType, names *nameSet) (*loadedType, error) {
	if l := loaded[entity]; l != nil {
		return l, nil
	}
	var found []string
	var goType, importPath string
	providers := make([]string, 0, len(ctx.GoModelSources))
	for name := range ctx.GoModelSources {
		providers = append(providers, name)
	}
	sort.Strings(providers)
	for _, name := range providers {
		source := ctx.GoModelSources[name]
		for _, model := range source.Models {
			if model.Name == entity && token.IsExported(model.GoType) {
				found = append(found, name)
				goType, importPath = model.GoType, source.ImportPath
			}
		}
	}
	if len(found) != 1 {
		return nil, fmt.Errorf("the entity %s needs exactly one Go model provider, and has %d (%s)", entity, len(found), strings.Join(found, ", "))
	}
	if gen.ValidateGoImportNamespace(importPath) != nil {
		return nil, fmt.Errorf("the Go model provider of %s has no valid import path", entity)
	}
	if m.entityImport != "" && m.entityImport != importPath {
		return nil, fmt.Errorf("the entities come from two packages, %s and %s", m.entityImport, importPath)
	}
	m.entityImport = importPath
	l := &loadedType{entity: entity, goType: goType, alias: "Loaded" + goType, helper: "stegoLoaded" + goType}
	if err := names.take("the loaded type of "+entity, l.alias); err != nil {
		return nil, err
	}
	loaded[entity] = l
	m.loaded = append(m.loaded, l)
	return l, nil
}

// selectionOf gives the Go field of the entity field in its model, and whether it is a pointer.
func selectionOf(ctx gen.Context, importPath, entity, name string) (string, bool, error) {
	for _, source := range ctx.GoModelSources {
		if source.ImportPath != importPath {
			continue
		}
		for _, model := range source.Models {
			if model.Name != entity {
				continue
			}
			for _, f := range model.Fields {
				if f.Name == name && token.IsExported(f.Selection) {
					return f.Selection, f.Pointer, nil
				}
			}
		}
	}
	return "", false, fmt.Errorf("the Go model of %s has no exported field %s", entity, name)
}

func hasEmitter(emitters []gen.Emitter, name string) bool {
	for _, e := range emitters {
		if e.Name == name {
			return true
		}
	}
	return false
}

func containsLoaded(list []*loadedType, l *loadedType) bool {
	for _, x := range list {
		if x == l {
			return true
		}
	}
	return false
}

// nameSet holds the exported names of the probe package, so two declarations never give one name.
type nameSet struct{ owner map[string]string }

func newNames() *nameSet {
	return &nameSet{owner: map[string]string{"RecorderTenant": "the package", "RecorderPrincipal": "the package"}}
}

func (s *nameSet) take(owner string, names ...string) error {
	for _, n := range names {
		if prev, ok := s.owner[n]; ok {
			return fmt.Errorf("domain-probe: %s and %s both give the Go name %s", prev, owner, n)
		}
		s.owner[n] = owner
	}
	return nil
}

// mixedCase is the Go mixed case of a name in lower snake or kebab case, with dots as separators: id is ID.
func mixedCase(name string) string {
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

// lowerMixedCase is mixedCase with a lower first letter, for a parameter.
func lowerMixedCase(name string) string {
	s := mixedCase(name)
	if strings.HasPrefix(s, "ID") {
		return "id" + s[2:]
	}
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func isPredeclared(name string) bool {
	switch name {
	case "bool", "byte", "error", "float64", "int64", "string", "true", "false", "nil", "len", "new", "make", "any",
		"probe", "context", "command", "entities":
		return true
	}
	return false
}

// render gives the source of the probe package.
func render(m *model) []byte {
	var b bytes.Buffer
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	w("// Package %s holds the typed domain probes of the service (ST-SUP-RC34). A fill emits a declared\n", m.pkg)
	w("// observation only through the probe of its own command, query, worker, process method or slot binding.\n")
	w("package %s\n\nimport (\n\t\"context\"\n\n", m.pkg)
	if len(m.loaded) > 0 {
		w("\tentities %q\n", m.entityImport)
		w("\t%q\n", commandImport)
	}
	w("\t%q\n)\n\n", probeImport)
	w("// RecorderTenant and RecorderPrincipal are the fixed tenant and principal of a probe that a Record\n")
	w("// constructor binds for a fill test.\nconst (\n\tRecorderTenant = \"tenant-test\"\n\tRecorderPrincipal = \"user:test\"\n)\n\n")
	for _, l := range m.loaded {
		w("// %s is a loaded value of %s: only a read or a write through a handle makes one.\n", l.alias, l.entity)
		w("type %s = command.Loaded[entities.%s]\n\n", l.alias, l.goType)
		w("func %s(l %s) (entities.%s, bool) {\n\tif l == nil {\n\t\treturn entities.%s{}, false\n\t}\n\treturn l.Value(), true\n}\n\n",
			l.helper, l.alias, l.goType, l.goType)
	}
	for _, o := range m.observations {
		for _, f := range o.fields {
			if f.kind != enumField {
				continue
			}
			w("// %s is the closed set of values of the field %s of %s.\ntype %s string\n\n", f.enumType, f.name, o.decl.Name, f.enumType)
			w("// The values of %s.\nconst (\n", f.enumType)
			for _, v := range f.values {
				w("\t%s%s %s = %q\n", f.enumType, mixedCase(v), f.enumType, v)
			}
			w(")\n\n")
		}
	}
	for _, o := range m.observations {
		w("var %s = declare(probe.Spec{Name: %q, Kind: probe.%s, Severity: probe.%s, Counts: probe.%s,\n", o.variable, o.decl.Name, o.kind, o.severity, o.counts)
		if o.value != "" {
			w("\tValue: %q, Unit: probe.%s,\n", o.value, units[o.unit])
		}
		if len(o.fields) > 0 {
			w("\tFields: []probe.Field{\n")
			for _, f := range o.fields {
				switch f.kind {
				case entityField:
					w("\t\t{Name: %q, Type: probe.%s},\n", f.name, map[string]string{"text": "FieldText", "integer": "FieldInteger", "flag": "FieldBool"}[f.value])
				case enumField:
					w("\t\t{Name: %q, Type: probe.FieldEnum, Values: []string{%s}},\n", f.name, quoteList(f.values))
				case boolField:
					w("\t\t{Name: %q, Type: probe.FieldBool},\n", f.name)
				case measureField:
					w("\t\t{Name: %q, Type: probe.FieldMeasure, Unit: probe.%s, Min: %d, Max: %d},\n", f.name, units[f.unit], f.min, f.max)
				}
			}
			w("\t},\n")
		}
		w("\tEmittedBy: []string{%s},\n})\n\n", quoteList(o.decl.EmittedBy))
	}
	if len(m.observations) > 0 {
		w("// declare makes the descriptor of one declared observation. The generator checked the declaration as Declare\n")
		w("// does, so it does not fail.\nfunc declare(spec probe.Spec) *probe.Observation {\n\to, err := probe.Declare(spec)\n\tif err != nil {\n\t\tpanic(err)\n\t}\n\treturn o\n}\n\n")
	}
	helpers := map[string]bool{}
	for _, o := range m.observations {
		for _, f := range o.fields {
			if f.kind == entityField {
				helpers[f.value+map[bool]string{true: "Pointer", false: ""}[f.pointer]] = true
			}
		}
	}
	for _, h := range []string{"text", "textPointer", "integer", "integerPointer", "flag", "flagPointer"} {
		if helpers[h] {
			w("%s\n", helperSource[h])
		}
	}
	slots := false
	for _, e := range m.emitters {
		renderEmitter(w, e)
		slots = slots || e.kind == gen.EmitterSlotBinding
	}
	if slots {
		w("// slotKey is the context key of the probe of a slot binding.\ntype slotKey struct{}\n\n")
		w("// slotProbe is the probe of one slot binding in the context of its fill.\ntype slotProbe struct {\n\temitter string\n\tp *probe.Probe\n}\n")
	}
	return b.Bytes()
}

// helperSource are the value helpers of entity fields: a value of a missing loaded value matches no field, so the
// runtime drops and counts the emission.
var helperSource = map[string]string{
	"text":           "func stegoText(ok bool, v string) probe.Value {\n\tif !ok {\n\t\treturn probe.Value{}\n\t}\n\treturn probe.Text(v)\n}\n",
	"textPointer":    "func stegoTextPointer(ok bool, v *string) probe.Value {\n\tif !ok {\n\t\treturn probe.Value{}\n\t}\n\tif v == nil {\n\t\treturn probe.Text(\"\")\n\t}\n\treturn probe.Text(*v)\n}\n",
	"integer":        "func stegoInteger[T ~int32 | ~int64](ok bool, v T) probe.Value {\n\tif !ok {\n\t\treturn probe.Value{}\n\t}\n\treturn probe.Integer(int64(v))\n}\n",
	"integerPointer": "func stegoIntegerPointer[T ~int32 | ~int64](ok bool, v *T) probe.Value {\n\tif !ok {\n\t\treturn probe.Value{}\n\t}\n\tif v == nil {\n\t\treturn probe.Integer(0)\n\t}\n\treturn probe.Integer(int64(*v))\n}\n",
	"flag":           "func stegoFlag(ok bool, v bool) probe.Value {\n\tif !ok {\n\t\treturn probe.Value{}\n\t}\n\treturn probe.Bool(v)\n}\n",
	"flagPointer":    "func stegoFlagPointer(ok bool, v *bool) probe.Value {\n\tif !ok {\n\t\treturn probe.Value{}\n\t}\n\treturn probe.Bool(v != nil && *v)\n}\n",
}

// renderEmitter gives the probe type of one emitter, its methods and its constructors.
func renderEmitter(w func(string, ...any), e *emitter) {
	t := e.goName + "Probe"
	w("// %s is the probe of the %s %s: it has a method for each observation that names it, and no other.\n", t, e.kind, e.name)
	w("type %s struct{ p *probe.Probe }\n\n", t)
	w("// New%s wraps the runtime probe of one call of %s, for the runtime that binds the call.\n", t, e.name)
	w("func New%s(p *probe.Probe) %s { return %s{p: p} }\n\n", t, t, t)
	w("// Record%s binds a probe of %s to the recorder r for a fill test, with RecorderTenant and RecorderPrincipal.\n", e.goName, e.name)
	w("// The test ends the call with Commit or Rollback before it reads the emissions.\n")
	w("func Record%s(r *probe.Recorder) (%s, *probe.Call) {\n\tcall := r.Bind(context.Background(), RecorderTenant, RecorderPrincipal, %q)\n\treturn %s{p: call.Probe()}, call\n}\n\n", e.goName, t, e.name, t)
	if e.kind == gen.EmitterSlotBinding {
		w("// Bind%s binds a probe call of %s to the call's span context, tenant and principal, and gives the\n", e.goName, e.name)
		w("// context that the slot's component passes the fill. The component ends the call with Commit or Rollback.\n")
		w("func Bind%s(ctx context.Context, probes interface {\n\tBind(ctx context.Context, tenant, principal, operation string) *probe.Call\n}, tenant, principal string) (context.Context, *probe.Call) {\n", e.goName)
		w("\tcall := probes.Bind(ctx, tenant, principal, %q)\n\treturn context.WithValue(ctx, slotKey{}, slotProbe{emitter: %q, p: call.Probe()}), call\n}\n\n", e.name, e.name)
		w("// %sFrom gives the fill of %s its probe from the context its slot's component passed it. In the context\n", t, e.name)
		w("// of another emitter it gives a probe that emits nothing and counts each attempt.\n")
		w("func %sFrom(ctx context.Context) %s {\n\theld, _ := ctx.Value(slotKey{}).(slotProbe)\n\tif held.emitter != %q {\n\t\treturn %s{p: held.p.Muted()}\n\t}\n\treturn %s{p: held.p}\n}\n\n", t, t, e.name, t, t)
	}
	for _, o := range e.methods {
		var params, args []string
		for i, l := range o.entities {
			params = append(params, o.params[i]+" "+l.alias)
		}
		for _, f := range o.fields {
			switch f.kind {
			case enumField:
				params = append(params, f.param+" "+f.enumType)
			case boolField:
				params = append(params, f.param+" bool")
			case measureField:
				params = append(params, f.param+" int64")
			}
		}
		if o.valueArg != "" {
			params = append(params, o.valueArg+" float64")
		}
		w("// %s emits %s, a %s of severity %s.\n", o.method, o.decl.Name, o.decl.Kind, o.decl.Severity)
		w("func (stegoProbe %s) %s(%s) {\n", t, o.method, strings.Join(params, ", "))
		for i, l := range o.entities {
			w("\tstegoV%d, stegoOK%d := %s(%s)\n", i, i, l.helper, o.params[i])
		}
		for _, f := range o.fields {
			switch f.kind {
			case entityField:
				i := indexOf(o.entities, f.entity)
				helper := "stego" + strings.ToUpper(f.value[:1]) + f.value[1:]
				if f.pointer {
					helper += "Pointer"
				}
				args = append(args, fmt.Sprintf("%s(stegoOK%d, stegoV%d.%s)", helper, i, i, f.selection))
			case enumField:
				args = append(args, "probe.Enum(string("+f.param+"))")
			case boolField:
				args = append(args, "probe.Bool("+f.param+")")
			case measureField:
				args = append(args, "probe.Measure("+f.param+")")
			}
		}
		if o.valueArg != "" {
			args = append(args, "probe.Number("+o.valueArg+")")
		}
		w("\tstegoProbe.p.Emit(%s)\n}\n\n", strings.Join(append([]string{o.variable}, args...), ", "))
	}
}

func indexOf(list []*loadedType, l *loadedType) int {
	for i, x := range list {
		if x == l {
			return i
		}
	}
	return -1
}

func quoteList(values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = strconv.Quote(v)
	}
	return strings.Join(quoted, ", ")
}

// catalogEntry is one observation in observations.json.
type catalogEntry struct {
	Name            string         `json:"name"`
	Signal          string         `json:"signal"`
	Kind            string         `json:"kind"`
	Severity        string         `json:"severity"`
	Counts          string         `json:"counts"`
	Fields          []catalogField `json:"fields"`
	Value           *catalogValue  `json:"value"`
	EmittedBy       []string       `json:"emitted_by"`
	OutcomeMeasured string         `json:"outcome_measured"`
}

type catalogField struct {
	Name   string   `json:"name"`
	Type   string   `json:"type"`
	Entity string   `json:"entity,omitempty"`
	Field  string   `json:"field,omitempty"`
	Values []string `json:"values,omitempty"`
	Unit   string   `json:"unit,omitempty"`
	Min    *int64   `json:"min,omitempty"`
	Max    *int64   `json:"max,omitempty"`
}

type catalogValue struct {
	Name string `json:"name"`
	Unit string `json:"unit"`
}

// renderCatalog gives observations.json: each observation in name order, the same bytes for the same declaration.
func renderCatalog(m *model) ([]byte, error) {
	entries := make([]catalogEntry, 0, len(m.observations))
	for _, o := range m.observations {
		e := catalogEntry{Name: o.decl.Name, Signal: "domain." + o.decl.Name, Kind: o.decl.Kind, Severity: o.decl.Severity,
			Counts: "attempt", Fields: []catalogField{}, EmittedBy: append([]string{}, o.decl.EmittedBy...), OutcomeMeasured: o.decl.OutcomeMeasured}
		if o.counts == "CountsCommitted" {
			e.Counts = "committed"
		}
		if o.value != "" {
			e.Value = &catalogValue{Name: o.value, Unit: o.unit}
		}
		for _, f := range o.fields {
			c := catalogField{Name: f.name}
			switch f.kind {
			case entityField:
				c.Type, c.Entity = "entity", f.entity.entity
				c.Field = strings.TrimPrefix(strings.TrimSpace(fieldType(o.decl, f.name)), f.entity.entity+".")
			case enumField:
				c.Type, c.Values = "enum", f.values
			case boolField:
				c.Type = "bool"
			case measureField:
				minimum, maximum := f.min, f.max
				c.Type, c.Unit, c.Min, c.Max = "measure", f.unit, &minimum, &maximum
			}
			e.Fields = append(e.Fields, c)
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	out, err := json.MarshalIndent(struct {
		Observations []catalogEntry `json:"observations"`
	}{entries}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

func fieldType(decl types.Observation, name string) string {
	for _, f := range decl.Fields {
		if f.Name == name {
			return f.Type
		}
	}
	return ""
}
