package auditlog

import (
	"database/sql"
	"regexp"
	"strings"
	"testing"
	"time"
)

// rejected runs the statement q in a transaction of the tenant t1 on db, and returns its error text.
func (f *fixture) rejected(db *sql.DB, q string) string {
	f.t.Helper()
	err := f.inTx(db, "t1", func(tx *sql.Tx) error {
		_, err := tx.ExecContext(f.ctx, q)
		return err
	})
	if err == nil {
		f.t.Fatalf("%s succeeded", q)
	}
	return err.Error()
}

func (f *fixture) rows(id string) int64 {
	f.t.Helper()
	var n int64
	if err := f.inTx(f.owner, id, func(tx *sql.Tx) error {
		return tx.QueryRowContext(f.ctx, "SELECT count(*) FROM audit_event").Scan(&n)
	}); err != nil {
		f.t.Fatal(err)
	}
	return n
}

var monthBound = regexp.MustCompile(`^FOR VALUES FROM \('(\d{4})-(\d{2})-01 00:00:00\+00'\) TO \('(\d{4})-(\d{2})-01 00:00:00\+00'\)$`)

func TestMigration(t *testing.T) {
	t.Run("ST-SUP-RC28-OC06-T01 The migration creates one audit_event table partitioned by month", func(t *testing.T) {
		f := newFixture(t)
		// The service declares three entities; there is still one audit table.
		var tables int
		if err := f.owner.QueryRowContext(f.ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') AND NOT c.relispartition
   AND c.relname NOT IN ('audit_chain_heads', 'audit_anchor_boundaries', 'audit_retention_checkpoints', 'leases')`).Scan(&tables); err != nil {
			t.Fatal(err)
		}
		if tables != 1 {
			t.Fatalf("%d audit tables that are not partitions, want exactly one", tables)
		}
		var strategy, column string
		if err := f.owner.QueryRowContext(f.ctx, `SELECT p.partstrat::text, a.attname FROM pg_partitioned_table p
 JOIN pg_attribute a ON a.attrelid = p.partrelid AND a.attnum = p.partattrs[0]
 WHERE p.partrelid = 'public.audit_event'::regclass`).Scan(&strategy, &column); err != nil {
			t.Fatalf("audit_event is not a partitioned table: %v", err)
		}
		if strategy != "r" || column != "recorded_at" {
			t.Fatalf("audit_event is partitioned by %s on %s, want RANGE on recorded_at", strategy, column)
		}
		rows, err := f.owner.QueryContext(f.ctx, `SELECT pg_get_expr(c.relpartbound, c.oid) FROM pg_inherits i
 JOIN pg_class c ON c.oid = i.inhrelid WHERE i.inhparent = 'public.audit_event'::regclass`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		partitions := 0
		for rows.Next() {
			var bound string
			if err := rows.Scan(&bound); err != nil {
				t.Fatal(err)
			}
			m := monthBound.FindStringSubmatch(bound)
			if m == nil {
				t.Fatalf("the partition bound %s is not one month", bound)
			}
			from, _ := time.Parse("2006-01", m[1]+"-"+m[2])
			to, _ := time.Parse("2006-01", m[3]+"-"+m[4])
			if !from.AddDate(0, 1, 0).Equal(to) {
				t.Fatalf("the partition bound %s is not one month", bound)
			}
			partitions++
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if partitions < 4 {
			t.Fatalf("%d partitions, want the current month and the next three", partitions)
		}
	})

	t.Run("ST-SUP-RC28-OC06-T02 The runtime role can select from and insert into audit_event, and cannot update, delete or truncate", func(t *testing.T) {
		f := newFixture(t)
		f.change(f.writer(), "t1", "c1", nil, map[string]any{"plan": "free"})
		err := f.inTx(f.runtime, "t1", func(tx *sql.Tx) error {
			var n int
			if err := tx.QueryRowContext(f.ctx, "SELECT count(*) FROM audit_event").Scan(&n); err != nil {
				return err
			}
			_, err := tx.ExecContext(f.ctx, `INSERT INTO audit_event (tenant_id, seq, id, occurred_at, recorded_at, actor_id,
 actor_kind, actor_ip, user_agent, action, resource_type, resource_id, outcome, error_code, request_id, trace_id,
 span_id, idempotency_key, service, service_version, declaration_version, prev_hash, hash)
 VALUES ('t1', 99, gen_random_uuid(), now(), now(), 'u1', 'user', '', '', 'create', 'Customer', 'c9', 'ok', '', '',
 '', '', '', 'customers', '', '', '\x00', '\x00')`)
			return err
		})
		if err != nil {
			t.Fatalf("SELECT and INSERT as the runtime role: %v", err)
		}
		for _, q := range []string{"UPDATE audit_event SET action = 'x'", "DELETE FROM audit_event", "TRUNCATE audit_event"} {
			if text := f.rejected(f.runtime, q); !strings.Contains(text, "permission denied") {
				t.Errorf("%s as the runtime role: %s, want a permission error", q, text)
			}
		}
	})

	t.Run("ST-SUP-RC28-OC06-T03 The reject trigger refuses UPDATE, DELETE and TRUNCATE also for the owner role", func(t *testing.T) {
		f := newFixture(t)
		f.change(f.writer(), "t1", "c1", nil, map[string]any{"plan": "free"})
		before := f.rows("t1")
		for _, q := range []string{"UPDATE audit_event SET action = 'x'", "DELETE FROM audit_event", "TRUNCATE audit_event"} {
			if text := f.rejected(f.owner, q); !strings.Contains(text, "the audit table rejects") {
				t.Errorf("%s as the owner role: %s, want the error of the trigger", q, text)
			}
		}
		var action string
		if err := f.inTx(f.owner, "t1", func(tx *sql.Tx) error {
			return tx.QueryRowContext(f.ctx, "SELECT action FROM audit_event WHERE resource_id = 'c1'").Scan(&action)
		}); err != nil {
			t.Fatal(err)
		}
		if after := f.rows("t1"); after != before || action != "update" {
			t.Fatalf("rows %d then %d, action %q: the rows changed", before, after, action)
		}
	})

	t.Run("ST-SUP-RC28-OC06-T04 The emitted audit tables keep each tenant's rows apart", func(t *testing.T) {
		f := newFixture(t)
		w := f.writer()
		f.change(w, "t1", "c1", nil, map[string]any{"plan": "free"})
		f.change(w, "t2", "c2", nil, map[string]any{"plan": "gold"})
		f.change(w, "t1", "c3", nil, map[string]any{"plan": "gold"})
		var tenants []string
		err := f.inTx(f.runtime, "t1", func(tx *sql.Tx) error {
			for _, table := range []string{"audit_event", "audit_chain_heads"} {
				rows, err := tx.QueryContext(f.ctx, "SELECT tenant_id FROM "+table)
				if err != nil {
					return err
				}
				for rows.Next() {
					var id string
					if err := rows.Scan(&id); err != nil {
						rows.Close()
						return err
					}
					tenants = append(tenants, table+":"+id)
				}
				rows.Close()
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		want := "audit_event:t1 audit_event:t1 audit_chain_heads:t1"
		if strings.Join(tenants, " ") != want {
			t.Fatalf("the rows of t1's transaction are %v, want %s", tenants, want)
		}
	})
}
