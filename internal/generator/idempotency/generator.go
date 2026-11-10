// Package idempotency generates the per-service idempotency wiring of a service (ST-SUP-RC30) on top of the runtime
// library github.com/jsell-rh/stego/registry/runtime/idempotency (ST-SUP-RP07): the key table migration as the runtime's Migration, the
// settings that the declaration's idempotency setting gives (ST-DECL-EN13), one fingerprint function for each
// create and patch operation over the fields that operation declares, and the purge job that the service runs. The
// transaction wrapper, the canonical JSON fingerprint and Purge live in the runtime library; the generated code
// holds only what varies with the declaration (ST-STD-RUNTIME-R05). A collection create or patch is a generated
// command (ST-SUP-RC20-OC37), so the wrapper is the one that command.Run calls (ST-SUP-RP10-LC11).
package idempotency

import (
	"bytes"
	"errors"
	"fmt"
	"go/format"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

// RuntimePackage is the import path of the runtime package that the generated code calls.
const RuntimePackage = "github.com/jsell-rh/stego/registry/runtime/idempotency"

// ProblemSlugs are the problem type slugs of the runtime package (ProblemMissing, ProblemInProgress and
// ProblemReused of github.com/jsell-rh/stego/registry/runtime/idempotency), in that order. The service joins each to its problem base.
var ProblemSlugs = []string{"idempotency-key-missing", "idempotency-key-in-progress", "idempotency-key-reused"}

// The defaults of the declaration's idempotency setting (ST-DECL-UC07-R03, ST-DECL-UC07-R04).
const (
	defaultRetention   = "24h"
	defaultFingerprint = "body"
)

// purgeInterval is the wait between two runs of Purge in the generated purge job.
const purgeInterval = time.Minute

// keyMethods gives the HTTP method of each collection operation that takes the key. GET, PUT and DELETE ignore it.
var keyMethods = map[types.Operation]string{types.OpCreate: "POST", types.OpPatch: "PATCH"}

// Generator is the generator of the idempotency component.
type Generator struct{}

// MinimumGoVersion is the Go version of the runtime module that the generated code imports.
func (*Generator) MinimumGoVersion() string { return "1.26.10" }

// settings is the idempotency setting of one declaration, resolved for the generated code.
type settings struct {
	Package                           string
	RetentionText                     string
	Retention                         time.Duration
	Fingerprint                       string
	ProblemMissing, ProblemInProgress string
	ProblemReused                     string
	Operations                        []operation
}

// operation is one collection operation that takes the key, with the body fields its fingerprint covers.
type operation struct {
	Collection, Operation, Method string
	Required                      bool
	Fields                        []string
}

// function is the name of the operation's fingerprint function, such as FingerprintTodosCreate.
func (op operation) function() string {
	return "Fingerprint" + pascal(op.Collection) + pascal(op.Operation)
}

// ValidateContext checks the output namespace, the idempotency setting and the problem base before any file is
// rendered.
func (*Generator) ValidateContext(ctx gen.Context) error {
	_, err := resolve(ctx)
	return err
}

// Generate emits, under the output namespace, the schema, the settings, the fingerprint functions and the purge
// job, and registers the purge job as a background task of the service.
func (*Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	s, err := resolve(ctx)
	if err != nil {
		return nil, nil, err
	}
	ns := ctx.OutputNamespace
	var files []gen.File
	for _, part := range []struct {
		name   string
		render func(settings) []byte
	}{{"schema.go", renderSchema}, {"settings.go", renderSettings}, {"fingerprint.go", renderFingerprints}, {"purge.go", renderPurge}} {
		source, err := format.Source(part.render(s))
		if err != nil {
			return nil, nil, fmt.Errorf("idempotency: formatting %s: %w", part.name, err)
		}
		files = append(files, gen.File{Path: path.Join(ns, part.name), Content: source})
	}
	if err := gen.ValidateNamespace(ns, files); err != nil {
		return nil, nil, err
	}
	wiring := &gen.Wiring{
		Imports:                 []string{ns},
		Constructors:            []string{s.Package + ".NewPurgeJob()"},
		ConstructorResources:    map[int][]gen.Resource{0: {gen.SQLDatabase}},
		ConstructorReturnsError: map[int]bool{0: true},
		BackgroundTasks:         []int{0},
		NeedsDB:                 true,
	}
	return files, wiring, nil
}

// resolve reads the declaration's idempotency setting into settings: the retention and the fingerprint as declared
// or by their defaults, the problem type URIs under the problem base, and each create and patch operation of each
// collection with its fields, required when Required on names it and accepted otherwise.
func resolve(ctx gen.Context) (settings, error) {
	if err := gen.ValidateGoPackageNamespace(ctx.OutputNamespace); err != nil {
		return settings{}, err
	}
	declared := ctx.Idempotency
	if declared == nil {
		return settings{}, errors.New("idempotency: the declaration has no idempotency setting")
	}
	if ctx.ErrorTypeBase == "" {
		return settings{}, errors.New("idempotency: the declaration has no error_type_base, so the problem types idempotency-key-missing, idempotency-key-in-progress and idempotency-key-reused have no base")
	}
	s := settings{Package: path.Base(ctx.OutputNamespace), RetentionText: declared.Retention, Fingerprint: declared.Fingerprint}
	if s.RetentionText == "" {
		s.RetentionText = defaultRetention
	}
	retention, err := time.ParseDuration(s.RetentionText)
	if err != nil || retention <= 0 {
		return settings{}, fmt.Errorf("idempotency: the retention %q is not a duration greater than zero", s.RetentionText)
	}
	s.Retention = retention
	if s.Fingerprint == "" {
		s.Fingerprint = defaultFingerprint
	}
	if s.Fingerprint != defaultFingerprint {
		return settings{}, fmt.Errorf("idempotency: the fingerprint %q is unknown; the only fingerprint is body", s.Fingerprint)
	}
	for _, operation := range declared.RequiredOn {
		if _, ok := keyMethods[operation]; !ok {
			return settings{}, fmt.Errorf("idempotency: Required on names %q, which is no operation served by POST or PATCH", operation)
		}
	}
	if !slices.Contains(declared.RequiredOn, types.OpCreate) {
		return settings{}, errors.New("idempotency: Required on does not name create")
	}
	uris := ProblemURIs(ctx.ErrorTypeBase)
	s.ProblemMissing, s.ProblemInProgress, s.ProblemReused = uris[0], uris[1], uris[2]
	entities := make(map[string]types.Entity, len(ctx.Entities))
	for _, entity := range ctx.Entities {
		entities[entity.Name] = entity
	}
	names := map[string]string{}
	for _, collection := range ctx.Collections {
		for _, op := range collection.Operations {
			method, ok := keyMethods[op]
			if !ok {
				continue
			}
			o := operation{Collection: collection.Name, Operation: string(op), Method: method,
				Required: slices.Contains(declared.RequiredOn, op), Fields: bodyFields(collection, entities[collection.Entity], op)}
			if other, taken := names[o.function()]; taken {
				return settings{}, fmt.Errorf("idempotency: the collections %s and %s both give the fingerprint function %s", other, collection.Name, o.function())
			}
			names[o.function()] = collection.Name
			s.Operations = append(s.Operations, o)
		}
	}
	return s, nil
}

// ProblemURIs gives the three problem type URIs under the problem base, in the order of ProblemSlugs, joined as the
// rest-api component joins its own problem types: the base and the slug.
func ProblemURIs(base string) []string {
	uris := make([]string, len(ProblemSlugs))
	for i, slug := range ProblemSlugs {
		uris[i] = base + slug
	}
	return uris
}

// bodyFields gives the request body fields that an operation declares: for patch the patchable fields, and for
// create the entity's fields that the client gives, which leaves out the id, the server-managed fields (computed,
// timestamp, created_by, updated_by and generation, as the rest-api component's create request does), the
// collection's scope fields, which are in the path, and its implicit fields.
func bodyFields(collection types.Collection, entity types.Entity, op types.Operation) []string {
	if op == types.OpPatch {
		return append([]string(nil), collection.Patchable...)
	}
	var fields []string
	for _, f := range entity.Fields {
		if _, scoped := collection.Scope[f.Name]; scoped {
			continue
		}
		if _, implicit := collection.Implicit[f.Name]; implicit {
			continue
		}
		if f.Name == "id" || f.Computed || f.Type == types.FieldTypeTimestamp || f.Name == "created_by" ||
			f.Name == "updated_by" || f.Name == "generation" {
			continue
		}
		fields = append(fields, f.Name)
	}
	return fields
}

// pascal turns a declared name such as org-users or patch into a Go identifier part such as OrgUsers or Patch.
func pascal(name string) string {
	var b strings.Builder
	upper := true
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			upper = true
			continue
		}
		if upper {
			r = unicode.ToUpper(r)
			upper = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

const generatedHeader = "// Code generated by stego. DO NOT EDIT.\n\n"

// renderSchema writes the schema file: the key table DDL is the runtime's Migration.
func renderSchema(s settings) []byte {
	var b bytes.Buffer
	b.WriteString(generatedHeader)
	fmt.Fprintf(&b, "package %s\n\nimport stegoidempotency %q\n\n", s.Package, RuntimePackage)
	fmt.Fprintf(&b, "// SchemaSQL creates the key table idempotency_keys. It is the runtime's Migration, and the service applies it\n")
	fmt.Fprintf(&b, "// with its own migrations.\n")
	fmt.Fprintf(&b, "const SchemaSQL = stegoidempotency.Migration\n")
	return b.Bytes()
}

// renderSettings writes the settings file of the generated idempotency package.
func renderSettings(s settings) []byte {
	var b bytes.Buffer
	b.WriteString(generatedHeader)
	fmt.Fprintf(&b, "// Package %s holds the idempotency settings of the service, as its declaration gives them, the\n", s.Package)
	fmt.Fprintf(&b, "// fingerprint function of each operation that takes the key, and the purge job.\n")
	fmt.Fprintf(&b, "package %s\n\nimport \"time\"\n\n", s.Package)
	fmt.Fprintf(&b, "// Retention is how long a completed key and its stored response are kept: %s.\n", s.RetentionText)
	fmt.Fprintf(&b, "const Retention = time.Duration(%d)\n\n", int64(s.Retention))
	fmt.Fprintf(&b, "// Fingerprint is what the fingerprint covers: %s, with the method and the path.\n", s.Fingerprint)
	fmt.Fprintf(&b, "const Fingerprint = %s\n\n", strconv.Quote(s.Fingerprint))
	fmt.Fprintf(&b, "// The problem type URIs of the three idempotency problems, under the service's problem base.\n")
	fmt.Fprintf(&b, "const (\n")
	fmt.Fprintf(&b, "\t// ProblemMissing is the type of a 400 for an absent or malformed key.\n\tProblemMissing = %s\n", strconv.Quote(s.ProblemMissing))
	fmt.Fprintf(&b, "\t// ProblemInProgress is the type of a 409 for a duplicate of a request that is not complete.\n\tProblemInProgress = %s\n", strconv.Quote(s.ProblemInProgress))
	fmt.Fprintf(&b, "\t// ProblemReused is the type of a 422 for a used key with another fingerprint.\n\tProblemReused = %s\n", strconv.Quote(s.ProblemReused))
	fmt.Fprintf(&b, ")\n\n")
	fmt.Fprintf(&b, "// Operation is one collection operation that takes the Idempotency-Key header.\n")
	fmt.Fprintf(&b, "type Operation struct {\n\tCollection string\n\tOperation string\n\tMethod string\n")
	fmt.Fprintf(&b, "\t// Required is true when a request without the key gets 400; otherwise the key is accepted.\n\tRequired bool\n}\n\n")
	fmt.Fprintf(&b, "// Operations are the create and patch operations of the collections, in declaration order. A GET, PUT or\n")
	fmt.Fprintf(&b, "// DELETE operation is not here, because it ignores the key.\n")
	fmt.Fprintf(&b, "var Operations = []Operation{\n")
	for _, op := range s.Operations {
		fmt.Fprintf(&b, "\t{Collection: %s, Operation: %s, Method: %s, Required: %t},\n",
			strconv.Quote(op.Collection), strconv.Quote(op.Operation), strconv.Quote(op.Method), op.Required)
	}
	fmt.Fprintf(&b, "}\n")
	return b.Bytes()
}

// renderFingerprints writes one fingerprint function for each operation. Each decodes the body into a struct of the
// operation's declared fields only, as the service decodes the request, encodes that struct again, and gives the
// runtime's Fingerprint over the method, the path without its query and that body. A field the operation does not
// declare is not in the fingerprint; a declared field that is absent stays absent, and one that is null stays null.
func renderFingerprints(s settings) []byte {
	var b bytes.Buffer
	b.WriteString(generatedHeader)
	fmt.Fprintf(&b, "package %s\n\n", s.Package)
	if len(s.Operations) == 0 {
		return b.Bytes()
	}
	fmt.Fprintf(&b, "import (\n\t\"encoding/json\"\n\t\"net/http\"\n\n\tstegoidempotency %q\n)\n\n", RuntimePackage)
	for _, op := range s.Operations {
		covered := "no body field"
		if len(op.Fields) > 0 {
			covered = "the fields " + strings.Join(op.Fields, ", ")
		}
		fmt.Fprintf(&b, "// %s is the fingerprint of a %s request of the collection %s: the method, the path without\n", op.function(), op.Operation, op.Collection)
		fmt.Fprintf(&b, "// its query and the canonical body over %s.\n", covered)
		fmt.Fprintf(&b, "func %s(r *http.Request, body []byte) (string, error) {\n", op.function())
		fmt.Fprintf(&b, "\tvar fields struct {\n")
		for i, field := range op.Fields {
			fmt.Fprintf(&b, "\t\tF%d json.RawMessage `json:%s`\n", i, strconv.Quote(field+",omitempty"))
		}
		fmt.Fprintf(&b, "\t}\n")
		fmt.Fprintf(&b, "\tif err := json.Unmarshal(body, &fields); err != nil {\n\t\treturn \"\", err\n\t}\n")
		fmt.Fprintf(&b, "\tdeclared, err := json.Marshal(fields)\n\tif err != nil {\n\t\treturn \"\", err\n\t}\n")
		fmt.Fprintf(&b, "\treturn stegoidempotency.Fingerprint(r.Method, r.URL.Path, declared)\n}\n\n")
	}
	return b.Bytes()
}

// renderPurge writes the purge job: a background task of the service that runs the runtime's Purge with its
// default batch and pause, once at start and then once every purge interval, until its context ends.
func renderPurge(s settings) []byte {
	var b bytes.Buffer
	b.WriteString(generatedHeader)
	fmt.Fprintf(&b, "package %s\n\nimport (\n\t\"context\"\n\t\"database/sql\"\n\t\"errors\"\n\t\"time\"\n\n\tstegoidempotency %q\n)\n\n", s.Package, RuntimePackage)
	fmt.Fprintf(&b, "// PurgeInterval is the wait between two runs of Purge.\n")
	fmt.Fprintf(&b, "const PurgeInterval = time.Duration(%d)\n\n", int64(purgeInterval))
	b.WriteString(`// PurgeJob deletes the expired key rows of idempotency_keys with the runtime's Purge.
type PurgeJob struct {
	db *sql.DB
	// Interval is the wait between two runs; zero is PurgeInterval.
	Interval time.Duration
	// Purged, when not nil, gets the count of rows of each run and its error.
	Purged func(rows int64, err error)
}

// NewPurgeJob gives the purge job of the database db.
func NewPurgeJob(db *sql.DB) (*PurgeJob, error) {
	if db == nil {
		return nil, errors.New("the idempotency purge job needs a database")
	}
	return &PurgeJob{db: db}, nil
}

// Run runs Purge at once and then once every interval, until ctx ends. A failed run is tried again at the next
// interval, so a short database outage does not stop the service. Run returns nil when ctx ends.
func (j *PurgeJob) Run(ctx context.Context) error {
	interval := j.Interval
	if interval <= 0 {
		interval = PurgeInterval
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		rows, err := stegoidempotency.Purge(ctx, j.db, stegoidempotency.PurgeOptions{})
		if j.Purged != nil && ctx.Err() == nil {
			j.Purged(rows, err)
		}
		timer.Reset(interval)
	}
}
`)
	return b.Bytes()
}
