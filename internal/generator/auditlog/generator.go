// Package auditlog generates the audit wiring of a service on the audit runtime, github.com/jsell-rh/stego/registry/runtime/audit
// (ST-SUP-RC28). It emits only what changes with the declaration: the audit settings, the sensitive fields of each
// entity, the migration call with the runtime role, the anchor period and the retention, the jobs of controller
// mode at the anchor period, and the database grants. The DDL text, the hash chain, the redaction, the writer, the
// sink destination, the anchor job and the maintenance loop are in the runtime library (ST-STD-RUNTIME-R05).
package auditlog

import (
	"bytes"
	"errors"
	"fmt"
	"go/format"
	"go/token"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

// SinkPort is the port of the audit sink, which the platform binds (ST-DECL-EN10, ST-GEN-INV37). A declaration's
// audit.sink names another port binding in its place.
const SinkPort = "audit-sink"

// The tables of the audit runtime (ST-SUP-RP05-LC10), in the schema of the service.
const (
	EventsTable      = "audit_event"
	HeadsTable       = "audit_chain_heads"
	BoundariesTable  = "audit_anchor_boundaries"
	CheckpointsTable = "audit_retention_checkpoints"
	schema           = "public"
)

// MaxRetentionMonths is the longest retention that generation takes, 100 years, so that a huge value is refused here
// and not at the first maintenance run of audit_maintain (#239 review).
const MaxRetentionMonths = 1200

// retentionText is the grammar of the declaration's audit.retention: a whole number of months.
var retentionText = regexp.MustCompile(`^([1-9][0-9]{0,3}) months?$`)

// Generator is the generator of the audit-log component.
type Generator struct{}

// MinimumGoVersion is the Go version of the runtime module.
func (*Generator) MinimumGoVersion() string { return "1.26.11" }

// plan is what the emitted code holds, read from the context.
type plan struct {
	Package         string
	Level, PII      string
	AnchorPeriod    time.Duration
	RetentionMonths int
	Entities        []entityPlan
}

type entityPlan struct {
	Name   string
	Fields []fieldPlan
	PII    bool
}

type fieldPlan struct{ Name, Sensitivity string }

// ValidateContext refuses a context that the audit wiring cannot serve: audit that is off, an audit.sink that names a
// port the declaration does not bind (ST-SUP-RC28-OC07; the platform binds audit-sink, ST-GEN-INV37), an anchor period or a retention that is not valid, and an
// entity name that is not a Go name.
func (*Generator) ValidateContext(ctx gen.Context) error {
	_, err := read(ctx)
	return err
}

func read(ctx gen.Context) (plan, error) {
	if err := gen.ValidateGoPackageNamespace(ctx.OutputNamespace); err != nil {
		return plan{}, err
	}
	audit := types.Audit{}
	if ctx.Audit != nil {
		audit = *ctx.Audit
	}
	if audit.Enabled != nil && !*audit.Enabled {
		return plan{}, errors.New("audit-log: the declaration turns audit off, so the service must not use the audit-log component")
	}
	sink := audit.Sink
	if sink == "" {
		sink = SinkPort
	}
	// The platform binds audit-sink (ST-GEN-INV37); another port that audit.sink names must be a binding of the
	// declaration.
	if _, bound := ctx.PortBindings[sink]; !bound && sink != SinkPort {
		return plan{}, fmt.Errorf("audit-log: the service does not bind the port %s, which its audit.sink names for the audit sink", sink)
	}
	p := plan{Package: path.Base(ctx.OutputNamespace), Level: "change", PII: "redact"}
	switch audit.Level {
	case "", "change":
	case "metadata":
		p.Level = "metadata"
	default:
		return plan{}, fmt.Errorf("audit-log: the audit level %q is not metadata or change", audit.Level)
	}
	switch audit.PII {
	case "", "redact":
	case "keep":
		p.PII = "keep"
	default:
		return plan{}, fmt.Errorf("audit-log: the audit pii setting %q is not redact or keep", audit.PII)
	}
	period, err := anchorPeriod(ctx.ComponentConfig["anchor_period"])
	if err != nil {
		return plan{}, err
	}
	p.AnchorPeriod = period
	if p.RetentionMonths, err = retention(ctx.ComponentConfig["retention_months"], audit.Retention); err != nil {
		return plan{}, err
	}
	for _, entity := range ctx.Entities {
		if !token.IsIdentifier(entity.Name) || !token.IsExported(entity.Name) {
			return plan{}, fmt.Errorf("audit-log: the entity name %q is not an exported Go name", entity.Name)
		}
		e := entityPlan{Name: entity.Name}
		for _, field := range entity.Fields {
			switch field.Sensitivity {
			case "", "public":
				continue
			case "pii":
				e.PII = true
			case "secret":
			default:
				return plan{}, fmt.Errorf("audit-log: the field %s of the entity %s has the sensitivity %q, which is not public, pii or secret", field.Name, entity.Name, field.Sensitivity)
			}
			e.Fields = append(e.Fields, fieldPlan{Name: field.Name, Sensitivity: field.Sensitivity})
		}
		sort.Slice(e.Fields, func(i, j int) bool { return e.Fields[i].Name < e.Fields[j].Name })
		p.Entities = append(p.Entities, e)
	}
	return p, nil
}

// anchorPeriod reads the anchor_period setting of the component: a Go duration of at least one second, in whole
// seconds, as the DDL text needs it.
func anchorPeriod(value any) (time.Duration, error) {
	text, ok := value.(string)
	if !ok || text == "" {
		return 0, errors.New("audit-log: the component setting anchor_period is not a duration such as 1h")
	}
	period, err := time.ParseDuration(text)
	if err != nil || period < time.Second || period%time.Second != 0 {
		return 0, fmt.Errorf("audit-log: the component setting anchor_period %q is not a whole number of seconds of at least 1s", text)
	}
	return period, nil
}

// retention reads the retention in months: the declaration's audit.retention when it states one, else the
// component setting retention_months.
func retention(setting any, declared string) (int, error) {
	if declared != "" {
		m := retentionText.FindStringSubmatch(declared)
		if m == nil {
			return 0, fmt.Errorf("audit-log: the audit retention %q is not a number of months, such as 13 months", declared)
		}
		months, err := strconv.Atoi(m[1])
		if err != nil || months > MaxRetentionMonths {
			return 0, fmt.Errorf("audit-log: the audit retention %q is longer than %d months", declared, MaxRetentionMonths)
		}
		return months, nil
	}
	var months int
	switch v := setting.(type) {
	case int:
		months = v
	case int64:
		months = int(v)
	case float64:
		months = int(v)
		if float64(months) != v {
			months = 0
		}
	}
	if months < 1 || months > MaxRetentionMonths {
		return 0, fmt.Errorf("audit-log: the component setting retention_months %v is not a whole number of 1 to %d", setting, MaxRetentionMonths)
	}
	return months, nil
}

// Generate emits the audit wiring package.
func (g *Generator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	p, err := read(ctx)
	if err != nil {
		return nil, nil, err
	}
	source, err := format.Source(render(p))
	if err != nil {
		return nil, nil, fmt.Errorf("audit-log: formatting the audit wiring: %w", err)
	}
	files := []gen.File{{Path: path.Join(ctx.OutputNamespace, "audit.go"), Content: source}}
	if err := gen.ValidateNamespace(ctx.OutputNamespace, files); err != nil {
		return nil, nil, err
	}
	wiring := &gen.Wiring{
		// The grants of the runtime role (ST-STD-TXN-R06, ST-SUP-RP05-LC10): the audit table is append-only.
		DatabaseAccess: []gen.DatabaseObject{
			{Schema: schema, Name: EventsTable, Kind: "table", Privileges: []string{"INSERT", "SELECT"}},
			{Schema: schema, Name: HeadsTable, Kind: "table", Privileges: []string{"INSERT", "SELECT", "UPDATE"}},
			{Schema: schema, Name: BoundariesTable, Kind: "table", Privileges: []string{"INSERT", "SELECT", "UPDATE"}},
			{Schema: schema, Name: CheckpointsTable, Kind: "table", Privileges: []string{"SELECT"}},
		},
		BackupObjects: []gen.BackupObject{
			{Schema: schema, Name: EventsTable, Kind: "table"},
			{Schema: schema, Name: HeadsTable, Kind: "table"},
			{Schema: schema, Name: BoundariesTable, Kind: "table"},
			{Schema: schema, Name: CheckpointsTable, Kind: "table"},
			{Schema: schema, Name: "audit_maintain", Kind: "function"},
		},
	}
	return files, wiring, nil
}

// render writes the source of the audit wiring package.
func render(p plan) []byte {
	var b bytes.Buffer
	level := map[string]string{"change": "audit.LevelChange", "metadata": "audit.LevelMetadata"}[p.Level]
	pii := map[string]string{"redact": "audit.PIIRedact", "keep": "audit.PIIKeep"}[p.PII]
	sensitivity := map[string]string{"pii": "audit.PII", "secret": "audit.Secret"}
	fmt.Fprintf(&b, "// Package %s is the audit wiring of the service on the audit runtime (ST-SUP-RC28).\n", p.Package)
	fmt.Fprintf(&b, "package %s\n\nimport (\n\t\"database/sql\"\n\t\"time\"\n\n\t\"github.com/jsell-rh/stego/registry/runtime/audit\"\n)\n\n", p.Package)
	b.WriteString("// Settings are the audit settings of the service declaration (ST-DECL-EN10). Both pii settings redact the\n// audit record: keep binds the source record only.\n")
	fmt.Fprintf(&b, "var Settings = audit.Settings{Level: %s, PII: %s}\n\n", level, pii)
	b.WriteString("// AnchorPeriod is the anchor period of the audit-log component (ST-SUP-RC28-OC08).\n")
	fmt.Fprintf(&b, "const AnchorPeriod = %d * time.Second\n\n", int64(p.AnchorPeriod/time.Second))
	b.WriteString("// RetentionMonths is how many months the local audit partitions are kept.\n")
	fmt.Fprintf(&b, "const RetentionMonths = %d\n\n", p.RetentionMonths)
	b.WriteString("// Fields gives the sensitive fields of each entity by entity name, which its audit rows redact: a pii field is\n// replaced by the redaction marker, and a secret field is never stored (ST-SUP-RC28-OC01).\n")
	b.WriteString("var Fields = map[string]audit.Fields{\n")
	for _, e := range p.Entities {
		fmt.Fprintf(&b, "\t%q: %sFields,\n", e.Name, e.Name)
	}
	b.WriteString("}\n\n")
	for _, e := range p.Entities {
		fmt.Fprintf(&b, "// %sFields are the sensitive fields of the entity %s.\n", e.Name, e.Name)
		var parts []string
		for _, f := range e.Fields {
			parts = append(parts, fmt.Sprintf("%q: %s", f.Name, sensitivity[f.Sensitivity]))
		}
		fmt.Fprintf(&b, "var %sFields = audit.Fields{%s}\n\n", e.Name, strings.Join(parts, ", "))
	}
	b.WriteString("// PII tells, by entity name, whether the entity holds a pii field: each read of such an entity is audited\n// (ST-SUP-RC28-OC04).\n")
	b.WriteString("var PII = map[string]bool{\n")
	for _, e := range p.Entities {
		fmt.Fprintf(&b, "\t%q: %t,\n", e.Name, e.PII)
	}
	b.WriteString("}\n\n")
	b.WriteString(`// Migration returns the statements that create the audit tables of the service, granted to runtimeRole
// (ST-SUP-RC28-OC06). The owner role runs them in the migration job.
func Migration(runtimeRole string) (string, error) {
	return audit.DDL(audit.Config{RuntimeRole: runtimeRole, AnchorPeriod: AnchorPeriod, RetentionMonths: RetentionMonths})
}

// Jobs returns the jobs of controller mode (ST-SUP-RC28-OC08): the anchor job at the anchor period, which publishes
// each tenant's chain head and row count to the audit sink, and the maintenance loop of the partitions. Both run
// only while the controller lease is held. db is the runtime role's pool, and outbox enqueues for the audit sink.
func Jobs(lease audit.LeaseOf, tenants audit.TenantsOf, db *sql.DB, outbox audit.Enqueuer) (*audit.Anchorer, *audit.Maintainer, error) {
	anchorer, err := audit.NewAnchorer(audit.AnchorConfig{Period: AnchorPeriod, Lease: lease, Tenants: tenants, DB: db, Outbox: outbox})
	if err != nil {
		return nil, nil, err
	}
	maintainer, err := audit.NewMaintainer(audit.MaintainConfig{Lease: lease, DB: db, Outbox: outbox})
	if err != nil {
		return nil, nil, err
	}
	return anchorer, maintainer, nil
}
`)
	return b.Bytes()
}
