package outbox

import (
	"bytes"
	"errors"
	"fmt"
	"go/format"
	"path"

	"github.com/jsell-rh/stego/internal/gen"
)

// The outbox on the runtime (ST-SUP-RC18-OC30). When the generated commands and queries carry all data access
// (gen.Context.RuntimeData), the queue table, its policies and its relay functions come from the runtime outbox
// migration, outbox.Migration of the pinned stego.dev/runtime (ST-SUP-RP01-LC19), and this component emits only
// what varies with the declaration (ST-STD-RUNTIME-R05): the destinations of the service with their payload
// limits, the migration call with the roles, and the wiring of the queue and the worker. It emits no queue table,
// no queue code and no worker code of its own, and no grant: the runtime migration grants the runtime role and the
// worker role what they may do, so the database access contract holds no outbox object.

// runtimeFile is the one Go file of the outbox on the runtime.
const runtimeFile = "outbox.go"

// The runtime packages that the emitted wiring imports.
const (
	runtimeOutbox      = "stego.dev/runtime/outbox"
	runtimeAudit       = "stego.dev/runtime/audit"
	runtimeAuditOutbox = "stego.dev/runtime/audit/auditoutbox"
)

// auditComponent is the component whose audit sink is a destination of the outbox (ST-SUP-RC28-OC07).
const auditComponent = "audit-log"

// formerTable is the step that comes before the runtime migration in the emitted Migration. A database that the
// outbox component of an earlier STEGO version served holds its own stego_outbox.messages, with no tenant_id, no
// state and no row-level security. The runtime migration keeps a table that is there (CREATE TABLE IF NOT EXISTS)
// and then fails on its tenant_id index. This step drops that former table when it is empty, so the runtime
// migration creates its own; when it still holds messages, it stops the migration, so no message is lost, and the
// former worker must deliver them first. A table of the runtime migration has tenant_id, so the step leaves it.
const formerTable = `-- The queue table of the former outbox component, with no tenant_id, is replaced by the runtime's queue table.
DO $former$ BEGIN
	IF pg_catalog.to_regclass('stego_outbox.messages') IS NOT NULL AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_attribute
		WHERE attrelid = pg_catalog.to_regclass('stego_outbox.messages') AND attname = 'tenant_id' AND NOT attisdropped) THEN
		IF EXISTS (SELECT 1 FROM stego_outbox.messages) THEN
			RAISE EXCEPTION 'outbox: stego_outbox.messages holds messages of the former outbox component'
				USING HINT = 'deliver them with the former outbox worker until the table is empty, then run this migration again';
		END IF;
		DROP TABLE stego_outbox.messages;
	END IF;
END $former$;

`

// destination is one destination of the service that a peer component owns.
type destination struct {
	// Name is the destination name on the outbox.
	Name string
}

// runtimePlan is what the emitted wiring holds, read from the context.
type runtimePlan struct {
	Package      string
	Destinations []destination
	Audit        bool
}

// validateRuntime refuses a context that the outbox on the runtime cannot serve: peers that call the queue code
// of the former outbox, which this path does not emit.
func validateRuntime(ctx gen.Context) error {
	switch {
	case ctx.StorageContract != "":
		return errors.New("outbox: on the runtime data path the outbox emits no queue code, so it cannot serve the storage contract")
	case ctx.EventsContract != "":
		return errors.New("outbox: on the runtime data path the outbox emits no live event source, so it cannot serve the events contract")
	case ctx.PeerNamespaces["kafka-producer"] != "":
		return errors.New("outbox: on the runtime data path the outbox emits no queue code, so it cannot serve the kafka-producer component")
	}
	return nil
}

// readRuntime gives the plan of the outbox on the runtime: one destination for each peer component that owns one.
func readRuntime(ctx gen.Context) runtimePlan {
	p := runtimePlan{Package: path.Base(ctx.OutputNamespace)}
	if ctx.PeerNamespaces[auditComponent] != "" {
		p.Audit = true
		p.Destinations = append(p.Destinations, destination{Name: "audit-sink"})
	}
	return p
}

// generateRuntime gives the files and the wiring of the outbox on the runtime.
func generateRuntime(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	source, err := format.Source(renderRuntime(readRuntime(ctx)))
	if err != nil {
		return nil, nil, fmt.Errorf("outbox: formatting the outbox wiring: %w", err)
	}
	files := []gen.File{{Path: path.Join(ctx.OutputNamespace, runtimeFile), Content: source}}
	if err := gen.ValidateNamespace(ctx.OutputNamespace, files); err != nil {
		return nil, nil, err
	}
	wiring := &gen.Wiring{BackupObjects: []gen.BackupObject{
		{Schema: "stego_outbox", Name: "messages", Kind: "table"},
		{Schema: "stego_outbox", Name: "messages_sequence_seq", Kind: "sequence"},
	}}
	return files, wiring, nil
}

// renderRuntime writes the source of the outbox wiring package.
func renderRuntime(p runtimePlan) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Package %s is the outbox wiring of the service on the outbox runtime, stego.dev/runtime/outbox\n", p.Package)
	b.WriteString("// (ST-SUP-RC18-OC30). The queue table, its policies and its relay functions come from the runtime migration; this\n// package names the destinations of the service and wires the queue and the worker.\n")
	fmt.Fprintf(&b, "package %s\n\nimport (\n\t\"database/sql\"\n", p.Package)
	if p.Audit {
		b.WriteString("\t\"errors\"\n")
	}
	fmt.Fprintf(&b, "\n")
	if p.Audit {
		fmt.Fprintf(&b, "\t%q\n\t%q\n", runtimeAudit, runtimeAuditOutbox)
	}
	fmt.Fprintf(&b, "\t%q\n)\n\n", runtimeOutbox)

	b.WriteString("// Destinations are the names of the destinations of the service, each registered on the worker by NewWorker.\n")
	b.WriteString("var Destinations = []string{")
	for i, d := range p.Destinations {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q", d.Name)
	}
	b.WriteString("}\n\n")

	b.WriteString("// PayloadLimits gives the payload limit of each destination of the service that sets one of its own; a\n// destination it does not name takes outbox.MaxPayloadBytes (ST-SUP-RC18-OC02).\n")
	b.WriteString("func PayloadLimits() map[string]int {\n\tlimits := map[string]int{}\n")
	if p.Audit {
		b.WriteString("\tfor name, limit := range auditoutbox.PayloadLimits() {\n\t\tlimits[name] = limit\n\t}\n")
	}
	b.WriteString("\treturn limits\n}\n\n")

	b.WriteString("// formerTable replaces the queue table of the former outbox component, which has no tenant_id: it drops that\n")
	b.WriteString("// table when it is empty and stops when it holds messages.\n")
	fmt.Fprintf(&b, "const formerTable = %q\n\n", formerTable)

	b.WriteString(`// Migration returns the statements that create the queue table, its policies and its relay functions, for
// runtimeRole, the request-path role, and workerRole, the role that the worker mode connects as. The owner role runs
// them in the migration job, in one transaction; it needs membership in outbox.RelayRole WITH SET TRUE, INHERIT
// TRUE. The text is the runtime migration (ST-SUP-RP01-LC19) after the step that replaces the former queue table.
func Migration(runtimeRole, workerRole string) (string, error) {
	text, err := outbox.Migration(runtimeRole, workerRole)
	if err != nil {
		return "", err
	}
	return formerTable + text, nil
}

// NewQueue returns the outbox queue of db with the payload limits of the destinations of the service. clock is the
// clock of the queue; nil is the database clock.
func NewQueue(db *sql.DB, clock outbox.Clock) (*outbox.Queue, error) {
	return outbox.New(db, outbox.Options{Clock: clock, PayloadLimits: PayloadLimits()})
}

`)
	if p.Audit {
		b.WriteString(`// AuditQueue returns the audit enqueuer on queue, which the audit writer and the audit jobs take: each audit
// sink message goes in the caller's transaction to the destination audit-sink (ST-SUP-RC28-OC07).
func AuditQueue(queue *outbox.Queue) (*auditoutbox.Queue, error) {
	return auditoutbox.NewQueue(queue)
}

`)
	}
	b.WriteString("// Ports holds the outside system of each destination of the service.\ntype Ports struct {\n")
	if p.Audit {
		b.WriteString("\t// AuditSink is the system that the audit-sink port binds (ST-SUP-RC28-OC07).\n\tAuditSink audit.Sink\n")
	}
	b.WriteString("}\n\n")

	b.WriteString(`// NewWorker returns the delivery worker on queue with config, with each destination of the service registered
// on it, before Run.
func NewWorker(queue *outbox.Queue, config outbox.WorkerConfig, ports Ports) (*outbox.Worker, error) {
	worker, err := outbox.NewWorker(queue, config)
	if err != nil {
		return nil, err
	}
`)
	if p.Audit {
		b.WriteString(`	if ports.AuditSink == nil {
		return nil, errors.New("outbox: the destination audit-sink needs the audit-sink port")
	}
	auditDestination, err := audit.NewDestination(ports.AuditSink)
	if err != nil {
		return nil, err
	}
	auditSink, err := auditoutbox.NewSink(auditDestination)
	if err != nil {
		return nil, err
	}
	if err := worker.Register(auditSink.Destination()); err != nil {
		return nil, err
	}
`)
	} else {
		b.WriteString("\t_ = ports\n")
	}
	b.WriteString("\treturn worker, nil\n}\n")
	return b.Bytes()
}
