// Package spicedbauthz generates the per-service authorization wiring of a service that binds authorization to
// SpiceDB (ST-SUP-RC29) on top of the runtime library github.com/jsell-rh/stego/registry/runtime/authz (ST-SUP-RP06). It emits only what
// varies with the declaration (ST-STD-RUNTIME-R05): the service's schema with the floor marker definition, the
// permission of each operation, the tenant-qualified resource of each protected entity, the tuples and the tuple
// writes of each entity's relationships, the migration call with the protected tables and their grants, and the
// relay with the owning table of each protected resource type, its start-up check, its registration on the outbox
// worker and its late-revocation watch at the declared settings. The ports, the SpiceDB provider, the consistency
// floor, the relay, the revocation records and the handle are in the runtime library.
package spicedbauthz

import (
	"bytes"
	"errors"
	"fmt"
	"go/format"
	"go/token"
	"path"
	"sort"
	"strconv"
	"strings"
	"testing/fstest"
	"time"
	"unicode"

	"github.com/jsell-rh/stego/internal/declaration"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
	"github.com/jsell-rh/stego/internal/types"
)

// RuntimePackage is the import path of the runtime package that the generated code calls.
const RuntimePackage = "github.com/jsell-rh/stego/registry/runtime/authz"

// MarkerDefinition is the floor marker definition that the generator adds to the schema it emits, so the author
// never writes it (ST-DECL-INV22): the relay TOUCHes stego_floor:floor#marker@stego_floor:floor to raise the
// revocation floor (ST-SUP-RP06-LC13).
const MarkerDefinition = "definition " + types.FloorMarkerType + " {\n\trelation marker: " + types.FloorMarkerType + "\n}\n"

// TenantColumn is the tenant column on which the relay stores a token on its owning row (ST-SUP-RP06-LC22).
const TenantColumn = "tenant_id"

// TokenColumn is the column of a protected table that holds the token of its row's last tuple write
// (ST-SUP-RC29-OC06).
const TokenColumn = "authz_token"

// RevocationRoute is the route of the revocation status, which the operation-to-permission table holds with its own
// decision and from which alone the router registers it (ST-SUP-RC29-OC18). Its path is the RevocationPath of the
// authorization runtime with the identity, as the Authz-Revocation-Location of a write gives it (ST-SUP-RP10-LC35).
const RevocationRoute = "GET /authz/revocations/{id}"

// RevocationOperation is the name of the revocation status operation in the operation-to-permission table.
const RevocationOperation = "revocation-status"

// databaseSchema is the SQL schema of the protected tables and of the authorization tables.
const databaseSchema = "public"

// Generator is the generator of the spicedb-authz component.
type Generator struct{}

// MinimumGoVersion is the Go version of the runtime module that the generated code imports.
func (*Generator) MinimumGoVersion() string { return "1.26.11" }

// plan is what the emitted code holds, read from the context.
type plan struct {
	Package       string
	SchemaPath    string
	Schema        string
	RelayName     string
	AlertBound    time.Duration
	WatchInterval time.Duration
	// WaitCap, Waiters and TenantWaiters are the bounds of a wait for a revocation (ST-SUP-RC29-OC19).
	WaitCap       time.Duration
	Waiters       int
	TenantWaiters int
	Operations    []operationPlan
	Entities      []entityPlan
	Bindings      []bindingPlan
}

// operationPlan is the decision of one operation of one collection.
type operationPlan struct {
	Collection, Operation, ResourceType, Permission, Reason string
	Public                                                  bool
}

// entityPlan is one protected entity: its resource type and its table.
type entityPlan struct {
	Name, ResourceType, Table string
}

// bindingPlan is one slot binding that names the relations its fills may write.
type bindingPlan struct {
	Collection, Slot, GoName string
	Relations                []relationPlan
}

type relationPlan struct{ ResourceType, Name string }

// ValidateContext refuses a context that the authorization wiring cannot serve, before any file is written: no
// authorization setting, no pooled tenancy with the tenant column tenant_id, a schema that the compiler did not
// capture, a declaration that the authorization checks refuse (ST-SUP-RC29-OC01, ST-SUP-RC29-OC11,
// ST-SUP-RC29-OC13), two protected entities of one resource type, a setting that is not a positive duration, and
// a name that gives no Go name.
func (*Generator) ValidateContext(ctx gen.Context) error {
	_, err := read(ctx)
	return err
}

// Generate emits the authorization wiring package and the schema with the floor marker definition.
func (*Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	p, err := read(ctx)
	if err != nil {
		return nil, nil, err
	}
	source, err := format.Source(render(p))
	if err != nil {
		return nil, nil, fmt.Errorf("spicedb-authz: formatting the authorization wiring: %w", err)
	}
	files := []gen.File{
		{Path: path.Join(ctx.OutputNamespace, "authz.go"), Content: source},
		{Path: path.Join(ctx.OutputNamespace, "schema.zed"), Content: []byte(p.Schema)},
	}
	if err := gen.ValidateNamespace(ctx.OutputNamespace, files); err != nil {
		return nil, nil, err
	}
	return files, &gen.Wiring{}, nil
}

// read reads the context into the plan of the emitted code.
func read(ctx gen.Context) (plan, error) {
	if err := gen.ValidateGoPackageNamespace(ctx.OutputNamespace); err != nil {
		return plan{}, err
	}
	setting := ctx.Authorization
	if setting == nil {
		return plan{}, errors.New("spicedb-authz: the declaration binds no authorization, so the service must not use the spicedb-authz component")
	}
	if ctx.Tenancy == nil {
		return plan{}, errors.New("spicedb-authz: the service has no pooled tenancy; every resource ID at the provider is tenant-qualified and the relay stores each token on its owning row by tenant_id (ST-SUP-RP06-LC22), so declare tenancy with kind pooled")
	}
	if ctx.Tenancy.Key != TenantColumn {
		return plan{}, fmt.Errorf("spicedb-authz: the tenant column is %q; the relay stores each token on its owning row by the tenant column %s, so declare tenancy with the key %s", ctx.Tenancy.Key, TenantColumn, TenantColumn)
	}
	source, ok := ctx.Inputs[setting.SchemaPath]
	// The schema path is written into a Go comment, so it must be a clean path with no control character.
	if err := gen.ValidatePath(setting.SchemaPath); setting.SchemaPath == "" || !ok || err != nil {
		return plan{}, fmt.Errorf("spicedb-authz: the schema %q that the authorization setting names is not among the inputs of the generator", setting.SchemaPath)
	}
	if err := checkDeclaration(ctx, source); err != nil {
		return plan{}, err
	}
	p := plan{Package: path.Base(ctx.OutputNamespace), SchemaPath: setting.SchemaPath, Schema: withMarker(string(source)),
		RelayName: ctx.ServiceName + "-authz-relay"}
	var err error
	if p.AlertBound, err = duration(ctx.ComponentConfig, "alert_bound"); err != nil {
		return plan{}, err
	}
	if p.WatchInterval, err = duration(ctx.ComponentConfig, "watch_interval"); err != nil {
		return plan{}, err
	}
	if p.WaitCap, err = duration(ctx.ComponentConfig, "wait_cap"); err != nil {
		return plan{}, err
	}
	if p.Waiters, err = count(ctx.ComponentConfig, "waiter_limit"); err != nil {
		return plan{}, err
	}
	if p.TenantWaiters, err = count(ctx.ComponentConfig, "tenant_waiter_limit"); err != nil {
		return plan{}, err
	}
	if p.TenantWaiters > p.Waiters {
		return plan{}, fmt.Errorf("spicedb-authz: the component setting tenant_waiter_limit %d is more than waiter_limit %d; the limit of a tenant is inside the limit of all", p.TenantWaiters, p.Waiters)
	}
	entities := map[string]types.Entity{}
	owners := map[string]string{}
	for _, entity := range ctx.Entities {
		entities[entity.Name] = entity
		if entity.ResourceType == "" {
			continue
		}
		if !token.IsIdentifier(entity.Name) || !token.IsExported(entity.Name) {
			return plan{}, fmt.Errorf("spicedb-authz: the entity name %q is not an exported Go name", entity.Name)
		}
		if other, taken := owners[entity.ResourceType]; taken {
			return plan{}, fmt.Errorf("spicedb-authz: the entities %s and %s both have the resource type %s; the relay stores the token of a resource type on one table, so give each entity a resource type of its own", other, entity.Name, entity.ResourceType)
		}
		owners[entity.ResourceType] = entity.Name
		e := entityPlan{Name: entity.Name, ResourceType: entity.ResourceType, Table: postgresadapter.TableName(entity.Name)}
		p.Entities = append(p.Entities, e)
	}
	for _, collection := range ctx.Collections {
		entity := entities[collection.Entity]
		if entity.ResourceType == "" {
			continue
		}
		for _, op := range collection.Operations {
			permission := entity.Permissions[op]
			o := operationPlan{Collection: collection.Name, Operation: string(op), ResourceType: entity.ResourceType, Permission: permission}
			if permission == types.PermissionPublic {
				o.Public, o.Permission, o.Reason = true, "", entity.PublicReasons[op]
			}
			p.Operations = append(p.Operations, o)
		}
	}
	names := map[string]string{}
	for _, e := range p.Entities {
		for _, name := range []string{e.Name + "Resource"} {
			names[name] = "the entity " + e.Name
		}
	}
	for _, binding := range ctx.SlotBindings {
		if len(binding.RelationshipsWritten) == 0 {
			continue
		}
		b := bindingPlan{Collection: binding.Collection, Slot: binding.Slot, GoName: pascal(binding.Collection) + pascal(binding.Slot) + "Relations"}
		if other, taken := names[b.GoName]; taken || !token.IsIdentifier(b.GoName) {
			return plan{}, fmt.Errorf("spicedb-authz: the slot binding %s.%s gives the Go name %s, which %s also gives", binding.Collection, binding.Slot, b.GoName, other)
		}
		names[b.GoName] = "the slot binding " + binding.Collection + "." + binding.Slot
		for _, written := range binding.RelationshipsWritten {
			resourceType, relation, _ := strings.Cut(written, "#")
			b.Relations = append(b.Relations, relationPlan{ResourceType: resourceType, Name: relation})
		}
		p.Bindings = append(p.Bindings, b)
	}
	return p, nil
}

// checkDeclaration runs the authorization checks of Declaring on the declaration that the context holds, with the
// captured schema: the schema must compile and hold every resource type, permission, relation and caveat parameter
// that an entity, collection or slot binding names, and must not define the floor marker (ST-DECL-INV22,
// ST-SUP-RC29-OC13). The generator refuses what checking refuses, naming each fault, before any file is written.
func checkDeclaration(ctx gen.Context, source []byte) error {
	service := &types.ServiceDeclaration{Name: ctx.ServiceName, Authorization: ctx.Authorization, Entities: ctx.Entities,
		Collections: ctx.Collections, Slots: ctx.SlotBindings, Tenancy: &types.Tenancy{Kind: "pooled", Key: ctx.Tenancy.Key}}
	faults := declaration.CheckAuthorization(service, fstest.MapFS{ctx.Authorization.SchemaPath: &fstest.MapFile{Data: source}})
	if len(faults) == 0 {
		return nil
	}
	var parts []string
	for _, fault := range faults {
		parts = append(parts, fault.Path+": "+fault.Message)
	}
	sort.Strings(parts)
	return fmt.Errorf("spicedb-authz: generation refused: %s", strings.Join(parts, "; "))
}

// withMarker gives the schema with the floor marker definition added at its end.
func withMarker(schema string) string {
	if !strings.HasSuffix(schema, "\n") {
		schema += "\n"
	}
	return schema + "\n" + MarkerDefinition
}

// duration reads a component setting that is a positive Go duration, such as 5s.
func duration(config map[string]any, name string) (time.Duration, error) {
	text, _ := config[name].(string)
	d, err := time.ParseDuration(text)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("spicedb-authz: the component setting %s %q is not a positive duration such as 5s", name, text)
	}
	return d, nil
}

// maxCount is the largest count setting: a waiter limit above it holds more waits than a process serves.
const maxCount = 100000

// count reads a component setting that is a whole number of 1 to maxCount.
func count(config map[string]any, name string) (int, error) {
	n := 0
	switch v := config[name].(type) {
	case int:
		n = v
	case int64:
		n = int(v)
	case float64:
		if v == float64(int(v)) {
			n = int(v)
		}
	}
	if n < 1 || n > maxCount {
		return 0, fmt.Errorf("spicedb-authz: the component setting %s %v is not a whole number of 1 to %d", name, config[name], maxCount)
	}
	return n, nil
}

// pascal turns a declared name such as org-users, before_update or owner into a Go name part such as OrgUsers,
// BeforeUpdate or Owner.
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

// goText gives a Go string literal of text: a raw literal when text holds no backquote and no carriage return,
// so the emitted schema reads as the author wrote it.
func goText(text string) string {
	if !strings.ContainsAny(text, "`\r") {
		return "`" + text + "`"
	}
	return strconv.Quote(text)
}

// render writes the source of the authorization wiring package.
func render(p plan) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Package %s is the authorization wiring of the service on the authorization runtime\n// github.com/jsell-rh/stego/registry/runtime/authz (ST-SUP-RC29).\n", p.Package)
	fmt.Fprintf(&b, "package %s\n\n", p.Package)
	b.WriteString("import (\n\t\"context\"\n\t\"database/sql\"\n\t\"fmt\"\n\t\"strings\"\n\t\"time\"\n\n")
	b.WriteString("\tv1 \"github.com/authzed/authzed-go/proto/authzed/api/v1\"\n\totellog \"go.opentelemetry.io/otel/log\"\n\t\"go.opentelemetry.io/otel/metric\"\n\t\"google.golang.org/grpc\"\n\n")
	fmt.Fprintf(&b, "\tstegoauthz %q\n\t\"github.com/jsell-rh/stego/registry/runtime/outbox\"\n", RuntimePackage)
	if len(p.Entities) > 0 {
		b.WriteString("\t\"github.com/jsell-rh/stego/registry/runtime/tenant\"\n")
	}
	b.WriteString(")\n\n")

	fmt.Fprintf(&b, "// Schema is the service's schema %s with the floor marker definition %s added, which the relay\n", p.SchemaPath, types.FloorMarkerType)
	b.WriteString("// touches to raise the revocation floor (ST-SUP-RP06-LC13); schema.zed holds the same text. The migration\n// job writes it to the provider with WriteSchema (ST-SUP-RC29-OC23); generation never does (ST-SUP-RC29-OC13).\n")
	fmt.Fprintf(&b, "const Schema = %s\n\n", goText(p.Schema))
	b.WriteString("// DatabaseSchema is the SQL schema of the protected tables and of the authorization tables.\n")
	fmt.Fprintf(&b, "const DatabaseSchema = %q\n\n", databaseSchema)
	b.WriteString("// RelayName is the name of the relay's service account, service_account:<RelayName> (ST-SUP-RP06-LC07).\n")
	fmt.Fprintf(&b, "const RelayName = %q\n\n", p.RelayName)
	fmt.Fprintf(&b, "// AlertBound is the delivery alert bound of a revocation, the component setting alert_bound: %s\n// (ST-SUP-RP06-LC16).\n", p.AlertBound)
	fmt.Fprintf(&b, "const AlertBound = time.Duration(%d)\n\n", int64(p.AlertBound))
	fmt.Fprintf(&b, "// WatchInterval is the time between two reads of the oldest age of the authz outbox messages, the component\n// setting watch_interval: %s (ST-SUP-RP06-LC14).\n", p.WatchInterval)
	fmt.Fprintf(&b, "const WatchInterval = time.Duration(%d)\n\n", int64(p.WatchInterval))
	fmt.Fprintf(&b, "// WaitCap is the longest wait for a revocation, the component setting wait_cap: %s (ST-SUP-RC29-OC19).\n", p.WaitCap)
	fmt.Fprintf(&b, "const WaitCap = time.Duration(%d)\n\n", int64(p.WaitCap))
	fmt.Fprintf(&b, "// Waiters is the most waits for a revocation at one time, the component setting waiter_limit: %d; TenantWaiters\n// is the most of one tenant, inside it, the component setting tenant_waiter_limit: %d (ST-SUP-RC29-OC19).\n", p.Waiters, p.TenantWaiters)
	fmt.Fprintf(&b, "const (\n\tWaiters       = %d\n\tTenantWaiters = %d\n)\n\n", p.Waiters, p.TenantWaiters)

	b.WriteString("// Operation is the decision of one operation: of an operation of a collection, the permission of the resource type\n// that it checks, or, for a public operation, the reason that the declaration gives (ST-SUP-RC29-OC01); of the\n// revocation status route, its route and the decision Writer, which allows only the principal and tenant that made\n// the write and calls no provider (ST-SUP-RC29-OC18).\n")
	b.WriteString("type Operation struct {\n\tCollection   string\n\tOperation    string\n\tResourceType string\n\tPermission   string\n\tPublic       bool\n\tReason       string\n\tRoute        string\n\tWriter       bool\n}\n\n")
	b.WriteString("// Operations are the decisions of the operations of the collections, in declaration order, and then of the\n// revocation status route, from which alone the router registers that route.\n")
	b.WriteString("var Operations = []Operation{\n")
	for _, o := range p.Operations {
		if o.Public {
			fmt.Fprintf(&b, "\t{Collection: %q, Operation: %q, ResourceType: %q, Public: true, Reason: %q},\n", o.Collection, o.Operation, o.ResourceType, o.Reason)
			continue
		}
		fmt.Fprintf(&b, "\t{Collection: %q, Operation: %q, ResourceType: %q, Permission: %q},\n", o.Collection, o.Operation, o.ResourceType, o.Permission)
	}
	fmt.Fprintf(&b, "\t{Operation: %q, Route: %q, Writer: true},\n", RevocationOperation, RevocationRoute)
	b.WriteString("}\n\n")

	b.WriteString("// Owners maps each protected resource type to the table of its rows, whose authz_token holds the token of the\n// row's last tuple write; the relay stores it there (ST-SUP-RC29-OC06).\n")
	b.WriteString("var Owners = map[string]stegoauthz.Owner{\n")
	for _, e := range p.Entities {
		fmt.Fprintf(&b, "\t%q: {Schema: DatabaseSchema, Table: %q, Key: \"id\"},\n", e.ResourceType, e.Table)
	}
	b.WriteString("}\n\n")

	for _, e := range p.Entities {
		renderEntity(&b, e)
	}
	for _, binding := range p.Bindings {
		fmt.Fprintf(&b, "// %s are the relations that the fills of the slot binding %s.%s may write (ST-SUP-RC29-OC11).\n", binding.GoName, binding.Collection, binding.Slot)
		fmt.Fprintf(&b, "var %s = []stegoauthz.Relation{", binding.GoName)
		for i, r := range binding.Relations {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "{ResourceType: %q, Name: %q}", r.ResourceType, r.Name)
		}
		b.WriteString("}\n\n")
	}

	b.WriteString(`// Migration returns the statements of the authorization tables of the service, for the migration job to run as
// the owner role after the migration of the outbox runtime and after the protected tables. It adds authz_token to
// each protected table, creates the revocation floor authz_floor and the revocation records with the runtime's
// Migration (ST-SUP-RP06-LC21), and grants workerRole, which stores each token on its owning row, UPDATE on
// authz_token only. runtimeRole is the role the service runs as; relayRole is the login role of the relay, which may
// update the floor and nothing else; outboxRelayRole is the outbox relay role of this service alone, which owns the
// state hook functions.
func Migration(runtimeRole, relayRole, outboxRelayRole, workerRole string) (string, error) {
	text, err := stegoauthz.Migration(DatabaseSchema, runtimeRole, relayRole, outboxRelayRole, workerRole)
	if err != nil {
		return "", err
	}
	worker := "\"" + strings.ReplaceAll(workerRole, "\"", "\"\"") + "\""
	var statements strings.Builder
`)
	for _, e := range p.Entities {
		table := databaseSchema + `.\"` + e.Table + `\"`
		fmt.Fprintf(&b, "\tstatements.WriteString(\"ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s text NOT NULL DEFAULT '' CHECK (octet_length(%s) <= 4096);\\n\")\n", table, TokenColumn, TokenColumn)
		fmt.Fprintf(&b, "\tstatements.WriteString(\"GRANT SELECT (%s, id), UPDATE (%s) ON %s TO \" + worker + \";\\n\")\n", TenantColumn, TokenColumn, table)
	}
	b.WriteString(`	return text + statements.String(), nil
}

// WriteSchema writes Schema to SpiceDB through WriteSchema, for the migration job to run before the service starts,
// so the schema is versioned with the release (ST-SUP-RC29-OC23). SpiceDB refuses, and writes nothing, when the
// schema it holds has a definition or a relation that Schema removes and that still has relationships; WriteSchema
// then gives that refusal, and the release does not start. conn is a connection to SpiceDB with a key that may
// write the schema.
func WriteSchema(ctx context.Context, conn grpc.ClientConnInterface) error {
	if _, err := v1.NewSchemaServiceClient(conn).WriteSchema(ctx, &v1.WriteSchemaRequest{Schema: Schema}); err != nil {
		return fmt.Errorf("authz: SpiceDB refuses the schema of the release, which would remove a definition or a relation that has relationships, or cannot take it: %w", err)
	}
	return nil
}

// NewRuntime returns the authorization runtime of the service, which binds the handles of its transactions and
// reads the revocation statuses. db is the runtime role's pool, provider the decision provider, and queue the
// outbox queue in which a handle enqueues the relay's tuple writes. A wait for a revocation is bounded by WaitCap,
// Waiters and TenantWaiters (ST-SUP-RC29-OC19).
func NewRuntime(db *sql.DB, provider stegoauthz.Decision, queue *outbox.Queue, meter metric.MeterProvider) (*stegoauthz.Runtime, error) {
	return stegoauthz.New(stegoauthz.Config{Provider: provider, DB: db, Schema: DatabaseSchema, Outbox: queue, Meter: meter,
		WaitCap: WaitCap, Waiters: Waiters, TenantWaiters: TenantWaiters})
}

// NewRelay builds the relay of the service, the outbox destination authz, with the owning table of each protected
// resource type and the delivery alert bound. It runs the relay's start-up check, a TOUCH of the floor marker, so
// the service does not start when the provider's schema lacks stego_floor (ST-SUP-RP06-LC13), and then registers
// the destination on the outbox worker: idempotent, must land, with the relay's state hook (ST-SUP-RP06-LC06,
// ST-SUP-RP01-LC25). db is the relay role's pool, and relationships the provider's relationships port, which only
// the relay holds (ST-SUP-RC29-OC08).
func NewRelay(ctx context.Context, db *sql.DB, relationships stegoauthz.Relationships, worker *outbox.Worker, meter metric.MeterProvider, logger otellog.LoggerProvider) (*stegoauthz.Relay, error) {
	relay, err := stegoauthz.NewRelay(stegoauthz.RelayConfig{Name: RelayName, Relationships: relationships, DB: db,
		Schema: DatabaseSchema, Owners: Owners, Meter: meter, Logger: logger}, stegoauthz.WithAlertBound(AlertBound))
	if err != nil {
		return nil, err
	}
	if err := relay.Check(ctx); err != nil {
		return nil, err
	}
	if err := worker.Register(relay.Destination()); err != nil {
		return nil, err
	}
	return relay, nil
}

// Watch runs the late-revocation alert of relay until ctx ends: every WatchInterval it reads the oldest age of the
// authz outbox messages through oldest, and raises one alert on each upward crossing of AlertBound; a read that
// fails alerts too (ST-SUP-RP06-LC14).
func Watch(ctx context.Context, relay *stegoauthz.Relay, oldest stegoauthz.OldestAge) error {
	return relay.Watch(ctx, WatchInterval, oldest)
}
`)
	return b.Bytes()
}

// renderEntity writes the resource function of a protected entity. The runtime writes the declared relationships
// of a row as it writes its references (ST-SUP-RP10-LC05, ST-SUP-Q118), from the Relationships list that the command
// generator gives the entity, so no relationship mapper is emitted here.
func renderEntity(b *bytes.Buffer, e entityPlan) {
	fmt.Fprintf(b, "// %sResource gives the resource %s of the %s row of key in tenant t, with the tenant-qualified ID of the\n", e.Name, e.ResourceType, e.Name)
	b.WriteString("// runtime (ST-SUP-RC29-OC22, ST-SUP-RP06-LC22).\n")
	fmt.Fprintf(b, "func %sResource(t tenant.Tenant, key string) (stegoauthz.Resource, error) {\n\treturn stegoauthz.NewResource(t, %q, key)\n}\n\n", e.Name, e.ResourceType)
}
