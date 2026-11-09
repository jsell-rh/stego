package command

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"stego.dev/runtime/audit"
)

// logRecorder keeps the log records that a logger provider exports.
type logRecorder struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (l *logRecorder) Export(_ context.Context, records []sdklog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range records {
		l.records = append(l.records, r.Clone())
	}
	return nil
}

func (l *logRecorder) Shutdown(context.Context) error   { return nil }
func (l *logRecorder) ForceFlush(context.Context) error { return nil }

func (l *logRecorder) all() []sdklog.Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]sdklog.Record(nil), l.records...)
}

func TestAuditRows(t *testing.T) {
	e := newEnv(t)
	e.exec(`INSERT INTO customers (tenant_id, id, name, email) VALUES ('t1', 'cu1', 'Ann', 'ann@example.com')`)
	cmd := e.command("reserve-and-charge", nil, e.reservationE, e.chargeE, e.customerE)

	t.Run("ST-SUP-RP10-LC06-T01 A committed write has its audit row with the command name", func(t *testing.T) {
		_, err := e.run(cmd, e.request("t1", u7(t), "k-lc06", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			if _, err := repo(t, h, e.reservationE).Create(ctx, reservation{ID: "r-lc06", OrderID: "o1", SKU: "s"}, nil); err != nil {
				return err
			}
			_, err := repo(t, h, e.chargeE).Create(ctx, charge{ID: "c-lc06", Amount: 10, CardLast4: "4242"}, nil)
			return err
		}))
		if err != nil {
			t.Fatal(err)
		}
		if n := e.count(`SELECT count(*) FROM audit_event WHERE tenant_id = 't1' AND action = 'reserve-and-charge'
 AND operation = 'reserve-and-charge' AND hash_version = 3`); n != 2 {
			t.Fatalf("%d audit rows carry the operation reserve-and-charge, want 2", n)
		}
		rows, err := e.admin.QueryContext(e.ctx, `SELECT a.action, a.actor_id, a.resource_type, coalesce(a.patch::text, ''),
 coalesce(a.after::text, ''), a.xmin::text = r.xmin::text
 FROM audit_event a, reservations r WHERE a.tenant_id = 't1' AND r.id = 'r-lc06' AND a.action = 'reserve-and-charge'
 ORDER BY a.seq`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var action, actor, resource, patch, after string
			var sameCommit bool
			if err := rows.Scan(&action, &actor, &resource, &patch, &after, &sameCommit); err != nil {
				t.Fatal(err)
			}
			n++
			if actor != "user:u-7" || patch == "" || !sameCommit {
				t.Errorf("the row of %s has the actor %s, the patch %q, and the same commit %t", resource, actor, patch, sameCommit)
			}
			if resource == "charge" && (strings.Contains(after, "4242") || strings.Contains(patch, "4242") ||
				!strings.Contains(after, audit.RedactionMarker)) {
				t.Errorf("the charge's pii field is not redacted: %s %s", after, patch)
			}
		}
		if n != 2 {
			t.Fatalf("%d audit rows of the command, want 2", n)
		}
	})

	t.Run("ST-SUP-RP10-LC06-T02 A rolled-back write leaves no audit row", func(t *testing.T) {
		before := e.auditRows("t1", "")
		_, err := e.run(cmd, e.request("t1", u7(t), "k-lc06-rb", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			if _, err := repo(t, h, e.reservationE).Create(ctx, reservation{ID: "r-lc06-rb", OrderID: "o1", SKU: "s"}, nil); err != nil {
				return err
			}
			return errors.New("refused")
		}))
		if err == nil {
			t.Fatal("the run committed")
		}
		if e.count(`SELECT count(*) FROM reservations WHERE id = 'r-lc06-rb'`) != 0 || e.auditRows("t1", "") != before {
			t.Fatal("the rolled-back write left a row")
		}
	})

	t.Run("ST-SUP-RP10-LC06-T03 A pii read during the fill does not wait on the command's own audit lock", func(t *testing.T) {
		var readTook time.Duration
		var heldChain bool
		rt := e.runtime(nil)
		rt.beforeCommit = func(ctx context.Context, tx *sql.Tx) error {
			// Run recorded the write's row after the fill and before COMMIT: the transaction holds the chain head.
			var n int
			err := tx.QueryRowContext(ctx, `SELECT count(*) FROM audit_event WHERE tenant_id = 't1' AND action = 'reserve-and-charge' AND resource_id = 'r-lc06-read'`).Scan(&n)
			heldChain = err == nil && n == 1
			return err
		}
		_, err := e.runOn(rt, cmd, e.request("t1", u7(t), "k-lc06-read", `{}`),
			fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
				if _, err := repo(t, h, e.reservationE).Create(ctx, reservation{ID: "r-lc06-read", OrderID: "o1", SKU: "s"}, nil); err != nil {
					return err
				}
				start := time.Now()
				_, err := repo(t, h, e.customerE).Get(ctx, "cu1")
				readTook = time.Since(start)
				return err
			}))
		if err != nil {
			t.Fatal(err)
		}
		if readTook > time.Second || !heldChain {
			t.Fatalf("the pii read took %s, and the write's row was recorded before COMMIT: %t", readTook, heldChain)
		}
		var readSeq, writeSeq int64
		if err := e.admin.QueryRowContext(e.ctx, `SELECT
 (SELECT seq FROM audit_event WHERE tenant_id = 't1' AND action = 'read' AND resource_id = 'cu1' ORDER BY seq DESC LIMIT 1),
 (SELECT seq FROM audit_event WHERE tenant_id = 't1' AND resource_id = 'r-lc06-read')`).Scan(&readSeq, &writeSeq); err != nil {
			t.Fatal(err)
		}
		if writeSeq != readSeq+1 {
			t.Fatalf("the read row has seq %d and the write's row %d, want them next to each other", readSeq, writeSeq)
		}
		if gaps := e.count(`SELECT count(*) FROM (SELECT seq - lag(seq) OVER (ORDER BY seq) AS step FROM audit_event WHERE tenant_id = 't1') s WHERE step <> 1`); gaps != 0 {
			t.Fatalf("t1's chain has %d seq gaps", gaps)
		}
	})

	t.Run("ST-SUP-RP10-LC06-T04 A failed outcome row is logged as metadata and counted", func(t *testing.T) {
		logs := &logRecorder{}
		provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logs)))
		rt := e.runtime(func(c *Config) { c.Logger = provider })
		// Session A holds the chain heads, so the outcome row waits until its lock timeout and fails.
		a := e.session()
		a.exec(`LOCK TABLE ` + audit.HeadsTable + ` IN EXCLUSIVE MODE`)
		req := e.request("t9", u7(t), "k-denied", `{}`)
		err := rt.RecordOutcome(e.ctx, req, "reserve-and-charge", "order", "o-secret-1", audit.OutcomeDenied, "forbidden")
		a.commit()
		if err == nil {
			t.Fatal("the outcome row was written while the chain heads were locked")
		}
		if rt.OutcomeFailures() != 1 {
			t.Fatalf("the failure counter is %d, want 1", rt.OutcomeFailures())
		}
		records := logs.all()
		if len(records) != 1 {
			t.Fatalf("%d log records, want 1", len(records))
		}
		got := map[string]string{}
		records[0].WalkAttributes(func(kv attribute.KeyValue) bool {
			got[string(kv.Key)] = kv.Value.Emit()
			return true
		})
		if got["stego.action"] != "reserve-and-charge" || got["stego.resource_type"] != "order" ||
			got["stego.outcome"] != "denied" || got["stego.error_code"] != "forbidden" || len(got) != 4 {
			t.Fatalf("the log record holds %v", got)
		}
		for key, value := range got {
			if strings.Contains(value, "o-secret-1") || strings.Contains(value, "u-7") {
				t.Fatalf("the log record holds the field value %s=%s", key, value)
			}
		}
	})
}
