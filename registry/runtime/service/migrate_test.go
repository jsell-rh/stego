package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	_ "github.com/jackc/pgx/v5/stdlib"
	"google.golang.org/grpc"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/internal/pgtest"
	"github.com/jsell-rh/stego/registry/runtime/lease"
	"github.com/jsell-rh/stego/registry/runtime/manifest"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// steps records the step names of Migrate and the backend of the connection that each runs on.
type steps struct {
	mu       sync.Mutex
	names    []string
	backends map[int]bool
}

func (s *steps) record(t *testing.T) func(string, *sql.Conn) {
	return func(name string, conn *sql.Conn) {
		var pid int
		if err := conn.QueryRowContext(context.Background(), "SELECT pg_backend_pid()").Scan(&pid); err != nil {
			t.Errorf("the step %s has no open connection: %v", name, err)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.names = append(s.names, name)
		s.backends[pid] = true
	}
}

func TestMigrate(t *testing.T) {
	t.Run("ST-SUP-RP14-LC03-T01 Migrate runs the migration job in its fixed order on one connection", func(t *testing.T) {
		endpoint, key := startSpiceDB(t)
		f := newFixture(t)
		settings := MigrationSettings{DatabaseURL: f.database, SpiceDB: SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}}
		want := []string{StepOwnerRole, StepLedgerCheck, StepAuthorizationSchema, StepTables, StepOutboxQueue, StepOutboxSequence, StepAuditTables, StepAuditSink,
			StepIdempotencyKeys, StepAuthorizationTables, StepRuntimeGrants, StepAuditPartitions, StepSchemaAgain, StepBackfill}
		run := func(m Migration) (*steps, error) {
			s := &steps{backends: map[int]bool{}}
			stepped = s.record(t)
			defer func() { stepped = nil }()
			return s, Migrate(context.Background(), settings, m)
		}
		writes := 0
		for _, pass := range []string{"a fresh database", "the same database again"} {
			s, err := run(f.migration(&writes))
			if err != nil {
				t.Fatalf("Migrate on %s: %v", pass, err)
			}
			if !slices.Equal(s.names, want) || len(s.backends) != 1 {
				t.Fatalf("Migrate on %s ran %v on %d connections, want %v on one", pass, s.names, len(s.backends), want)
			}
		}
		if writes != 4 {
			t.Fatalf("the schema was written %d times in two runs, want twice in each", writes)
		}
		// The job made the objects and the grants of the runtime role, and the partitions of the audit table.
		var granted bool
		if err := f.admin.QueryRow(`SELECT has_table_privilege($1, 'public.orders', 'INSERT') AND has_table_privilege($1, 'public.idempotency_keys', 'UPDATE')
 AND has_function_privilege($1, 'public.idempotency_keys_purge(timestamptz, integer)', 'EXECUTE') AND to_regclass('stego_outbox.messages') IS NOT NULL AND to_regclass('public.authz_floor') IS NOT NULL`, f.runtime).Scan(&granted); err != nil || !granted {
			t.Fatalf("the job did not make the objects and the grants of the runtime role: %v", err)
		}
		// The tables step made the tenant list of the tenant runtime, owned by the owner role, which the runtime role
		// reads and writes only through its functions (ST-SUP-RP04-LC07, ST-SUP-Q97).
		var listed bool
		if err := f.admin.QueryRow(`SELECT has_table_privilege($1, 'public.stego_tenants', 'SELECT') AND NOT has_table_privilege($1, 'public.stego_tenants', 'INSERT')
 AND has_function_privilege($1, 'public.stego_provision_tenant(text)', 'EXECUTE')
 AND (SELECT tableowner FROM pg_tables WHERE schemaname = 'public' AND tablename = 'stego_tenants') = $2`, f.runtime, f.owner).Scan(&listed); err != nil || !listed {
			t.Fatalf("the job did not make the tenant list for the runtime role: %v", err)
		}
		// The audit sink step made the default table of the audit sink, owned by the owner role, with SELECT and INSERT
		// and no UPDATE, DELETE or TRUNCATE for the worker role (ST-SUP-RP05-LC15).
		var sinkTable bool
		if err := f.admin.QueryRow(`SELECT has_table_privilege($1, 'stego_audit_sink.entries', 'SELECT') AND has_table_privilege($1, 'stego_audit_sink.entries', 'INSERT')
 AND NOT has_table_privilege($1, 'stego_audit_sink.entries', 'UPDATE') AND NOT has_table_privilege($1, 'stego_audit_sink.entries', 'DELETE')
 AND NOT has_table_privilege($1, 'stego_audit_sink.entries', 'TRUNCATE') AND NOT has_table_privilege($2, 'stego_audit_sink.entries', 'SELECT')
 AND (SELECT tableowner FROM pg_tables WHERE schemaname = 'stego_audit_sink' AND tablename = 'entries') = $3`, f.worker, f.runtime, f.owner).Scan(&sinkTable); err != nil || !sinkTable {
			t.Fatalf("the job did not make the default audit sink for the worker role: %v", err)
		}
		var partitions int
		if err := f.admin.QueryRow(`SELECT count(*) FROM pg_inherits WHERE inhparent = 'public.audit_event'::regclass`).Scan(&partitions); err != nil || partitions < 2 {
			t.Fatalf("the job made %d audit partitions, want this month and the next: %v", partitions, err)
		}

		// A step that fails stops the job there, with a fault that names it, and no later step runs.
		broken := f.migration(nil)
		broken.Authorization = "SELECT 1 / 0"
		s, err := run(broken)
		var step *StepError
		if !errors.As(err, &step) || step.Step != StepAuthorizationTables || Stage(err) != "migrate.authorization_tables" {
			t.Fatalf("the broken job gave %v, want a fault of the step %s", err, StepAuthorizationTables)
		}
		if last := s.names[len(s.names)-1]; last != StepAuthorizationTables || slices.Contains(s.names, StepRuntimeGrants) {
			t.Fatalf("the broken job ran %v, want it to stop at %s", s.names, StepAuthorizationTables)
		}
		// The read-only ledger check refuses a database whose tables this release did not make, before any write.
		if _, err := f.admin.Exec(`CREATE TABLE public.foreign_orders (id text)`); err != nil {
			t.Fatal(err)
		}
		writes = 0
		s, err = run(f.migration(&writes))
		if Stage(err) != "migrate.ledger_check" || writes != 0 || slices.Contains(s.names, StepTables) {
			t.Fatalf("the job on a foreign table gave %v after %v with %d schema writes, want a fault of the ledger check and no write", err, s.names, writes)
		}
		if Stage(errors.New("other")) != "migrate" || !strings.HasPrefix(err.Error(), "migrate: ledger_check: ") {
			t.Fatalf("Stage and the fault text are not the step's: %v", err)
		}
		// A job that lacks a part is refused before it opens the database.
		if err := Migrate(context.Background(), settings, Migration{}); Stage(err) != "migrate.settings" {
			t.Fatalf("a job with no parts gave %v", err)
		}
	})
}

// boardsDDL is the table of a rename at expand of title to heading, owned by tenants and under forced row-level
// security, with the sync trigger of the change, the table of the backfill checkpoints, its functions and the row of
// the change, as the migration of the step makes them as the owner role.
func boardsDDL(t *testing.T, owner string) string {
	t.Helper()
	function, err := manifest.SyncFunction("stego_steps", "boards_heading_sync", 1, manifest.Sync{Change: "board-title-rename", Owner: owner, K: "title", U: "heading",
		Conversion: manifest.Conversion{Name: "identity"}})
	if err != nil {
		t.Fatal(err)
	}
	return `CREATE TABLE public.boards (tenant_id text NOT NULL DEFAULT pg_catalog.current_setting('app.tenant'), id uuid NOT NULL, title text NOT NULL, heading text,
  PRIMARY KEY (tenant_id, id));
ALTER TABLE public.boards ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.boards FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON public.boards USING (tenant_id = pg_catalog.current_setting('app.tenant', true)) WITH CHECK (tenant_id = pg_catalog.current_setting('app.tenant', true));
CREATE SCHEMA stego_steps;
` + function + `;
CREATE TRIGGER boards_heading_sync BEFORE INSERT OR UPDATE ON public.boards FOR EACH ROW EXECUTE FUNCTION stego_steps.boards_heading_sync();
CREATE SCHEMA IF NOT EXISTS stego_schema;
` + lease.BackfillsTable + ";\n" + lease.BackfillFunctions + `
INSERT INTO stego_schema.backfills (change_id) VALUES ('board-title-rename');
`
}

// renameJob gives a fixture with the table boards of a rename board-title-rename at expand, 25 rows in three tenants
// of which 24 have no new value, and the migration of a job that backfills it in batches of 4.
func renameJob(t *testing.T) (*fixture, Migration, []Backfill) {
	t.Helper()
	f := newFixture(t)
	ctx := context.Background()
	owner := func(statement string, args ...any) {
		t.Helper()
		tx, err := f.admin.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, "SET LOCAL ROLE "+f.owner); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, statement, args...); err != nil {
			t.Fatalf("%.60s: %v", statement, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	tenants, err := tenant.Migration("public", f.runtime)
	if err != nil {
		t.Fatal(err)
	}
	owner(tenants)
	owner(lease.LeasesTable)
	owner(boardsDDL(t, f.owner))
	counts := map[string]int{"tenant-a": 10, "tenant-b": 0, "tenant-c": 15}
	for _, id := range []string{"tenant-c", "tenant-a", "tenant-b"} {
		owner(`INSERT INTO public.stego_tenants (tenant_id) VALUES ($1)`, id)
		for i := range counts[id] {
			tx, err := f.admin.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, statement := range []string{"SET LOCAL ROLE " + f.owner, "SELECT pg_catalog.set_config('app.tenant', '" + id + "', true)"} {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO public.boards (id, title) VALUES (gen_random_uuid(), $1)`, fmt.Sprintf("%s-%02d", id, i)); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The sync trigger of the release before copied each insert, so the backfill has rows to move only after the
	// new column is cleared, as for rows that the release before wrote before the step.
	// One row keeps the new value that the trigger gave it, so the backfill leaves it and moves the 24 others.
	if _, err := f.admin.ExecContext(ctx, `ALTER TABLE public.boards DISABLE TRIGGER boards_heading_sync; UPDATE public.boards SET heading = NULL WHERE title <> 'tenant-c-03';
ALTER TABLE public.boards ENABLE TRIGGER boards_heading_sync`); err != nil {
		t.Fatal(err)
	}
	backfills, err := CopyBackfills([]lease.Copy{{Change: "board-title-rename", Table: "boards", TenantColumn: "tenant_id", From: "title", To: "heading"}}, "public")
	if err != nil {
		t.Fatal(err)
	}
	backfills[0].Settings.BatchSize = 4
	m := f.migration(nil)
	m.Service, m.Backfills, m.BackfillLease = "svc", backfills, lease.Settings{TTL: 2 * time.Second, Renew: 500 * time.Millisecond, Retry: 100 * time.Millisecond}
	return f, m, backfills
}

func TestMigrateBackfill(t *testing.T) {
	t.Run("ST-SUP-RP14-LC03-T05 The job backfills each open rename last, as the owner role, under its lease, tenant by tenant, and resumes from its checkpoint", func(t *testing.T) {
		f, m, backfills := renameJob(t)
		ctx := context.Background()
		// Two jobs at once: each takes the lease in turn, and the rows move once.
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				job := m
				job.Holder = fmt.Sprintf("job-%d", i)
				errs[i] = runBackfills(ctx, f.admin, job)
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("job %d: %v", i, err)
			}
		}
		var moved int64
		var completed bool
		var stored string
		if err := f.admin.QueryRowContext(ctx, `SELECT rows_moved, completed_at IS NOT NULL, last_key_type FROM stego_schema.backfills WHERE change_id = 'board-title-rename'`).Scan(&moved, &completed, &stored); err != nil ||
			moved != 24 || !completed || stored != string(lease.KeyComposite) {
			t.Fatalf("the checkpoint has %d moved, complete %v, the key type %q (error %v); want 24, complete and composite", moved, completed, stored, err)
		}
		var unmoved int
		if err := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM public.boards WHERE heading IS DISTINCT FROM title`).Scan(&unmoved); err != nil || unmoved != 0 {
			t.Fatalf("%d rows have no new value equal to the old one (error %v)", unmoved, err)
		}
		var leases int
		if err := f.admin.QueryRowContext(ctx, `SELECT count(*) FROM public.leases WHERE name = 'stego.backfill.board-title-rename'`).Scan(&leases); err != nil || leases != 1 {
			t.Fatalf("there are %d leases stego.backfill.board-title-rename (error %v), want 1", leases, err)
		}
		// A job that stops after its first batches resumes from the checkpoint in the next run, and moves each row once.
		if _, err := f.admin.ExecContext(ctx, `ALTER TABLE public.boards DISABLE TRIGGER boards_heading_sync; UPDATE public.boards SET heading = NULL;
ALTER TABLE public.boards ENABLE TRIGGER boards_heading_sync; UPDATE stego_schema.backfills SET last_key = NULL, last_key_type = NULL, rows_moved = 0, completed_at = NULL`); err != nil {
			t.Fatal(err)
		}
		stopCtx, stop := context.WithCancel(ctx)
		first := m
		first.Backfills = slices.Clone(backfills)
		first.Backfills[0].Settings.Observer = &stopAfter{rows: 8, stop: stop}
		if err := runBackfills(stopCtx, f.admin, first); err == nil {
			t.Fatal("the stopped job ended with no error")
		}
		var after int64
		if err := f.admin.QueryRowContext(ctx, `SELECT rows_moved FROM stego_schema.backfills WHERE change_id = 'board-title-rename'`).Scan(&after); err != nil || after < 8 || after >= 25 {
			t.Fatalf("the stopped job moved %d rows (error %v), want at least 8 and fewer than 25", after, err)
		}
		// The resumed job loses its lease to another holder after its first batch: its next batch is fenced, and it
		// waits for the lease again and completes the change.
		resumed := m
		resumed.Backfills = slices.Clone(backfills)
		var once sync.Once
		took := make(chan int64, 1)
		step := resumed.Backfills[0].Step
		resumed.Backfills[0].Step = func(ctx context.Context, tx *sql.Tx, after *lease.Key, limit int) (lease.Batch, error) {
			once.Do(func() {
				go func() {
					// It waits for the lock of the batch's fence on the lease row, so it takes the lease after the batch.
					if _, err := f.admin.ExecContext(context.Background(), `UPDATE public.leases SET expires_at = now() - interval '1 second' WHERE name = 'stego.backfill.board-title-rename'`); err != nil {
						t.Error(err)
					}
					other, err := lease.New(f.admin, "stego.backfill.board-title-rename", "other", lease.Settings{TTL: time.Second, Renew: 500 * time.Millisecond})
					if err != nil {
						t.Error(err)
						return
					}
					held, err := other.Acquire(context.Background())
					if err != nil {
						t.Error(err)
						took <- 0
						return
					}
					took <- held.Token()
				}()
			})
			return step(ctx, tx, after, limit)
		}
		if err := runBackfills(ctx, f.admin, resumed); err != nil {
			t.Fatal(err)
		}
		if token := <-took; token == 0 {
			t.Fatal("the other holder did not take the lease")
		}
		if err := f.admin.QueryRowContext(ctx, `SELECT rows_moved, completed_at IS NOT NULL FROM stego_schema.backfills WHERE change_id = 'board-title-rename'`).Scan(&moved, &completed); err != nil || moved != 25 || !completed {
			t.Fatalf("after the resumed job the checkpoint has %d moved, complete %v (error %v); want 25 and complete", moved, completed, err)
		}
	})
}

func TestMigrateBackfillStall(t *testing.T) {
	t.Run("ST-SUP-RP09-LC12-T02 The migration job emits a stall, runs on until its backfill time limit, and its next run resumes", func(t *testing.T) {
		f, m, backfills := renameJob(t)
		ctx := context.Background()
		// A session holds a row of tenant-c, so the batches of tenant-a move and the first batch of tenant-c waits.
		lock, err := f.admin.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = lock.Rollback() }()
		if _, err := lock.ExecContext(ctx, `SELECT id FROM public.boards WHERE title = 'tenant-c-07' FOR UPDATE`); err != nil {
			t.Fatal(err)
		}
		ttl, renew := time.Second, 300*time.Millisecond
		job := m
		job.Backfills = slices.Clone(backfills)
		job.BackfillLease = lease.Settings{TTL: ttl, Renew: renew, Retry: 100 * time.Millisecond}
		job.BackfillTimeout = 3*ttl + 3*time.Second
		seen := &stallRecorder{}
		job.Backfills[0].Settings.Observer = seen
		job.Backfills[0].Settings.LockTimeout = 200 * time.Millisecond
		started := time.Now()
		err = runBackfills(ctx, f.admin, job)
		took := time.Since(started)
		if err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the stalled job ends with %v, want the end of its backfill time limit", err)
		}
		if took < job.BackfillTimeout {
			t.Fatalf("the stalled job ended after %v, before its backfill time limit of %v", took, job.BackfillTimeout)
		}
		stalls, lastRows := seen.state()
		if len(stalls) != 1 || stalls[0].Sub(lastRows) > 3*ttl+renew+300*time.Millisecond || stalls[0].Sub(lastRows) < 3*ttl {
			t.Fatalf("schema.backfill.stalled came %d times (%v after the last batch), want once within three TTLs and one renew interval", len(stalls), stalls)
		}
		var moved int64
		var completed bool
		if err := f.admin.QueryRowContext(ctx, `SELECT rows_moved, completed_at IS NOT NULL FROM stego_schema.backfills WHERE change_id = 'board-title-rename'`).Scan(&moved, &completed); err != nil ||
			moved < 10 || moved >= 24 || completed {
			t.Fatalf("after the stalled job the checkpoint has %d moved, complete %v (error %v); want the 10 rows of tenant-a at least, fewer than 24, and not complete", moved, completed, err)
		}
		// The next run, after the lock is released, resumes from the checkpoint and moves each other row once.
		_ = lock.Rollback()
		next := m
		next.Backfills = slices.Clone(backfills)
		resumed := &stallRecorder{}
		next.Backfills[0].Settings.Observer = resumed
		if err := runBackfills(ctx, f.admin, next); err != nil {
			t.Fatal(err)
		}
		if got := resumed.moved(); got != 24-moved {
			t.Fatalf("the next run moved %d rows, want the %d that the stalled job left", got, 24-moved)
		}
		if err := f.admin.QueryRowContext(ctx, `SELECT rows_moved, completed_at IS NOT NULL FROM stego_schema.backfills WHERE change_id = 'board-title-rename'`).Scan(&moved, &completed); err != nil || moved != 24 || !completed {
			t.Fatalf("after the next run the checkpoint has %d moved, complete %v (error %v); want 24 and complete", moved, completed, err)
		}
		if stalls, _ := resumed.state(); len(stalls) != 0 {
			t.Fatalf("the next run emitted %d stalls, want none", len(stalls))
		}
	})
}

// stallRecorder records the stalls of a backfill, the time of its last batch and the rows it moved.
type stallRecorder struct {
	mu     sync.Mutex
	stalls []time.Time
	last   time.Time
	rows   int64
}

func (s *stallRecorder) BackfillRows(_ string, rows int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows += rows
	s.last = time.Now()
}
func (s *stallRecorder) BackfillFailedRows(string, int64)      {}
func (s *stallRecorder) BackfillDuplicateGroups(string, int64) {}
func (s *stallRecorder) BackfillCompleted(string)              {}
func (s *stallRecorder) BackfillThrottled(string, string)      {}
func (s *stallRecorder) BackfillStalled(string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stalls = append(s.stalls, time.Now())
}

func (s *stallRecorder) state() ([]time.Time, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.stalls), s.last
}

func (s *stallRecorder) moved() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows
}

// stopAfter ends the context of a backfill once it has moved the given rows.
type stopAfter struct {
	mu    sync.Mutex
	rows  int64
	moved int64
	stop  context.CancelFunc
}

func (s *stopAfter) BackfillRows(_ string, rows int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.moved += rows; s.moved >= s.rows {
		s.stop()
	}
}
func (s *stopAfter) BackfillFailedRows(string, int64)      {}
func (s *stopAfter) BackfillDuplicateGroups(string, int64) {}
func (s *stopAfter) BackfillCompleted(string)              {}
func (s *stopAfter) BackfillStalled(string)                {}
func (s *stopAfter) BackfillThrottled(string, string)      {}

func TestMigrateLedgerRole(t *testing.T) {
	t.Run("ST-SUP-RP14-LC03-T02 Migrate runs the ledger of the adapter as the owner role, not as the login of the job", func(t *testing.T) {
		endpoint, key := startSpiceDB(t)
		f := newFixture(t)
		settings := MigrationSettings{DatabaseURL: f.database, SpiceDB: SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}}
		m := f.migration(nil)
		var current, session string
		ledger := m.Ledger
		m.Ledger = func(ctx context.Context, conn *sql.Conn) error {
			if err := conn.QueryRowContext(ctx, "SELECT current_user, session_user").Scan(&current, &session); err != nil {
				return err
			}
			return ledger(ctx, conn)
		}
		if err := Migrate(context.Background(), settings, m); err != nil {
			t.Fatal(err)
		}
		login := mustURL(t, f.database).User.Username()
		if current != f.owner || session != login {
			t.Fatalf("the ledger ran as %s in a session of %s, want the owner role %s in the session of the login %s", current, session, f.owner, login)
		}
		var owner string
		if err := f.admin.QueryRow("SELECT tableowner FROM pg_tables WHERE schemaname = 'public' AND tablename = 'orders'").Scan(&owner); err != nil || owner != f.owner {
			t.Fatalf("the table of the ledger is owned by %q (%v), want the owner role %s", owner, err, f.owner)
		}
	})

	t.Run("ST-SUP-RP14-LC03-T04 Migrate refuses an outbox identity sequence that is behind its stored messages", func(t *testing.T) {
		endpoint, key := startSpiceDB(t)
		f := newFixture(t)
		settings := MigrationSettings{DatabaseURL: f.database, SpiceDB: SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}}
		if err := Migrate(context.Background(), settings, f.migration(nil)); err != nil {
			t.Fatal(err)
		}
		// A stored message at sequence 100, as after a failover that carried the rows and not the sequence.
		if _, err := f.admin.Exec(`INSERT INTO stego_outbox.messages (id, sequence, tenant_id, destination, resource_key, kind, payload, outgoing_key,
 created_at, available_at) OVERRIDING SYSTEM VALUE VALUES (gen_random_uuid(), 100, 't1', 'd', 'r', 'k', 'p', 'o', now(), now())`); err != nil {
			t.Fatal(err)
		}
		for _, behind := range []string{"SELECT setval('stego_outbox.messages_sequence_seq', 5)", "SELECT setval('stego_outbox.messages_sequence_seq', 100, false)"} {
			if _, err := f.admin.Exec(behind); err != nil {
				t.Fatal(err)
			}
			err := Migrate(context.Background(), settings, f.migration(nil))
			if Stage(err) != "migrate."+StepOutboxSequence || !errors.Is(err, errSequenceBehind) {
				t.Fatalf("Migrate after %q gave %v at the stage %q, want the refusal of the sequence", behind, err, Stage(err))
			}
		}
		// The sequence past the highest message: the job runs.
		if _, err := f.admin.Exec("SELECT setval('stego_outbox.messages_sequence_seq', 100)"); err != nil {
			t.Fatal(err)
		}
		if err := Migrate(context.Background(), settings, f.migration(nil)); err != nil {
			t.Fatalf("Migrate with the sequence at the highest message: %v", err)
		}
		// An enqueue that commits between the two reads of the check, as under live traffic, does not make the healthy
		// queue look behind (#256 G1).
		betweenSequenceReads = func() {
			if _, err := f.admin.Exec(`INSERT INTO stego_outbox.messages (id, tenant_id, destination, resource_key, kind, payload, outgoing_key,
 created_at, available_at) VALUES (gen_random_uuid(), 't1', 'd', 'r', 'k', 'p', 'o', now(), now())`); err != nil {
				t.Error(err)
			}
		}
		defer func() { betweenSequenceReads = nil }()
		if err := Migrate(context.Background(), settings, f.migration(nil)); err != nil {
			t.Fatalf("Migrate with an enqueue between the reads of the check: %v", err)
		}
	})

	t.Run("ST-SUP-RP14-LC03-T03 Migrate makes the tables of the runtime in public when a schema is named after the owner role", func(t *testing.T) {
		endpoint, key := startSpiceDB(t)
		f := newFixture(t)
		// The default search path "$user", public of the owner puts a schema of its name first (#245 Q2).
		if _, err := f.admin.Exec("CREATE SCHEMA " + f.owner + " AUTHORIZATION " + f.owner); err != nil {
			t.Fatal(err)
		}
		settings := MigrationSettings{DatabaseURL: f.database, SpiceDB: SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}}
		if err := Migrate(context.Background(), settings, f.migration(nil)); err != nil {
			t.Fatal(err)
		}
		var keys string
		if err := f.admin.QueryRow("SELECT pg_catalog.to_regclass('public.idempotency_keys')::text").Scan(&keys); err != nil || keys == "" {
			t.Fatalf("the key table is not in public: %q, %v", keys, err)
		}
		var elsewhere int
		if err := f.admin.QueryRow("SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1", f.owner).Scan(&elsewhere); err != nil || elsewhere != 0 {
			t.Fatalf("the schema of the owner holds %d relations (%v), want none", elsewhere, err)
		}
	})
}

func TestMain(m *testing.M) {
	code := pgtest.Main(m)
	stopSpiceDB()
	os.Exit(code)
}

const rolePassword = "service-test"

// fixture is one database of the local PostgreSQL server of pgtest with the roles of one service, as the tenancy
// component's role migration makes them: the owner role, the runtime role with a login, the worker and relay roles,
// and the owner's membership in the outbox relay role.
type fixture struct {
	t                             *testing.T
	admin                         *sql.DB
	database                      string // the superuser URL of the database
	owner, runtime, worker, relay string
	purge, outboxRelay            string
}

// fixtureOwner is the owner role of the last fixture, which the parts of the test service name; the tests of the
// package do not run in parallel.
var fixtureOwner, fixtureWorker, fixtureOutboxRelay, fixtureRelay, fixturePurge string

func newFixture(t *testing.T) *fixture {
	t.Helper()
	suffix := make([]byte, 5)
	_, _ = rand.Read(suffix)
	tag := hex.EncodeToString(suffix)
	f := &fixture{t: t, owner: "svc_owner_" + tag, runtime: "svc_rt_" + tag, worker: "svc_worker_" + tag, relay: "svc_relay_" + tag,
		purge: "svc_" + tag + "_idempotency_purge", outboxRelay: "svc_" + tag + outbox.RelaySuffix}
	root, err := sql.Open("pgx", pgtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	f.database = pgtest.Database(t, "service")
	if f.admin, err = sql.Open("pgx", f.database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.admin.Close() })
	name := strings.TrimPrefix(mustURL(t, f.database).Path, "/")
	for _, statement := range []string{
		"CREATE ROLE " + f.owner + " NOLOGIN",
		"CREATE ROLE " + f.runtime + " LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '" + rolePassword + "'",
		"CREATE ROLE " + f.worker + " NOLOGIN NOINHERIT",
		"CREATE ROLE " + f.relay + " NOLOGIN NOINHERIT",
		"CREATE ROLE " + f.outboxRelay + " NOLOGIN NOINHERIT",
		"GRANT " + f.outboxRelay + " TO " + f.owner + " WITH SET TRUE, INHERIT FALSE",
		"CREATE ROLE " + f.purge + " NOLOGIN NOINHERIT",
		"GRANT " + f.purge + " TO " + f.owner + " WITH SET TRUE, INHERIT FALSE",
		"GRANT CREATE ON DATABASE " + name + " TO " + f.owner,
	} {
		if _, err := root.ExecContext(context.Background(), statement); err != nil {
			t.Fatalf("%.60s: %v", statement, err)
		}
	}
	// As the tenancy role migration gives them: the owner may grant CREATE on public, which the idempotency and
	// authorization migrations give the purge role and the outbox relay role only while they make their functions.
	for _, statement := range []string{
		"GRANT CREATE ON SCHEMA public TO " + f.owner + " WITH GRANT OPTION",
	} {
		if _, err := f.admin.ExecContext(context.Background(), statement); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, role := range []string{f.runtime, f.worker, f.relay, f.owner, f.purge, f.outboxRelay} {
			_, _ = f.admin.ExecContext(context.Background(), "DROP OWNED BY "+role)
		}
		_, _ = root.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		for _, role := range []string{f.runtime, f.worker, f.relay, f.owner, f.purge, f.outboxRelay} {
			_, _ = root.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+role)
		}
	})
	fixtureOwner, fixtureWorker, fixtureOutboxRelay, fixtureRelay, fixturePurge = f.owner, f.worker, f.outboxRelay, f.relay, f.purge
	return f
}

func mustURL(t *testing.T, text string) *url.URL {
	t.Helper()
	u, err := url.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// runtimeURL is the URL of the runtime role's login on the database.
func (f *fixture) runtimeURL() string {
	u := mustURL(f.t, f.database)
	u.User = url.UserPassword(f.runtime, rolePassword)
	return u.String()
}

// tenantConfig is the connection configuration of the adapter in the tests: the local server, without TLS on the
// loopback address, as the adapter's loopback test mode allows.
func tenantConfig(calls *int) func(string) (tenant.Config, error) {
	return func(dsn string) (tenant.Config, error) {
		if calls != nil {
			*calls++
		}
		return tenant.Config{DSN: dsn, InsecureLoopback: true}, nil
	}
}

// ordersDDL is the ledger of the test service: one tenant-owned table of orders with row-level security.
const ordersDDL = `CREATE TABLE IF NOT EXISTS public.orders (tenant_id text NOT NULL, id text NOT NULL, status text NOT NULL,
  version bigint NOT NULL DEFAULT 1, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id));
ALTER TABLE public.orders ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.orders FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS orders_tenant ON public.orders;
CREATE POLICY orders_tenant ON public.orders USING (tenant_id = current_setting('app.tenant', true))
  WITH CHECK (tenant_id = current_setting('app.tenant', true));`

// migration is the migration job of the test service, with a schema writer that writes into calls.
func (f *fixture) migration(calls *int) Migration {
	t := f.t
	queue, err := outbox.Migration(f.outboxRelay, f.runtime, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	auditText, err := audit.DDL(audit.Config{RuntimeRole: f.runtime, AnchorPeriod: time.Hour, RetentionMonths: 12})
	if err != nil {
		t.Fatal(err)
	}
	authzText, err := authz.Migration("public", f.runtime, f.relay, f.outboxRelay, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	return Migration{
		OwnerRole: f.owner, RuntimeRole: f.runtime, WorkerRole: f.worker, PurgeRole: f.purge, OutboxRelayRole: f.outboxRelay, TenantConfig: tenantConfig(nil),
		LedgerCheck: func(ctx context.Context, conn *sql.Conn) error {
			var foreign bool
			if err := conn.QueryRowContext(ctx, `SELECT to_regclass('public.foreign_orders') IS NOT NULL`).Scan(&foreign); err != nil {
				return err
			}
			if foreign {
				return fmt.Errorf("the database holds a table that this release did not make")
			}
			return nil
		},
		Ledger: func(ctx context.Context, conn *sql.Conn) error {
			_, err := conn.ExecContext(ctx, ordersDDL)
			return err
		},
		Outbox: queue, Audit: auditText, Authorization: authzText,
		WriteSchema: func(ctx context.Context, conn grpc.ClientConnInterface) error {
			if calls != nil {
				*calls++
			}
			if spiceServer.endpoint == "" {
				return nil
			}
			_, err := v1.NewSchemaServiceClient(conn).WriteSchema(ctx, &v1.WriteSchemaRequest{Schema: spiceSchema})
			return err
		},
		Grants: []string{`GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE "public"."orders"`,
			`GRANT SELECT, INSERT, UPDATE ON TABLE "public"."idempotency_keys"`},
	}
}

// writeFile writes data to a new file of the test, which only its owner may read, and gives its path.
func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// spiceDBImage is the pinned SpiceDB image of the local stand-in (ST-STD-LOCAL-R04), as the other runtime tests pin
// it.
const spiceDBImage = "authzed/spicedb:v1.57.0@sha256:a170d372c9f6b921bb6849064df6ca3566dd5b174882c1a2c4300c2ae1d60efd"

// spiceSchema is the schema of the test service.
const spiceSchema = `definition user {}
definition service {
	relation operator: user
	permission create_order = operator
}
definition order {
	relation owner: user
	permission view = owner
	permission create_order = owner
}
definition stego_floor {
	relation marker: stego_floor
}`

// spiceServer is the one SpiceDB container of the test binary, with its preshared key.
var spiceServer struct {
	once                sync.Once
	name, endpoint, key string
	err                 error
}

// startSpiceDB starts a local SpiceDB container without privilege, with an in-memory datastore and limits, once for
// the test binary, and gives its endpoint and preshared key.
func startSpiceDB(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		if os.Getenv(pgtest.NoContainersVariable) == "1" {
			t.Skip("docker is not on the PATH, and STEGO_TEST_NO_CONTAINERS=1: this test needs a local SpiceDB container")
		}
		t.Fatal("docker is not on the PATH: this test needs a local SpiceDB container")
	}
	spiceServer.once.Do(func() {
		suffix := make([]byte, 6)
		_, _ = rand.Read(suffix)
		spiceServer.name, spiceServer.key = "stego-service-spicedb-"+hex.EncodeToString(suffix), "service-"+hex.EncodeToString(suffix)
		run := exec.Command("docker", "run", "-d", "--rm", "--name", spiceServer.name, "--memory", "256m", "--cpus", "1",
			"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "-p", "127.0.0.1::50051", spiceDBImage,
			"serve", "--grpc-preshared-key", spiceServer.key, "--datastore-engine", "memory")
		var stderr bytes.Buffer
		run.Stderr = &stderr
		if err := run.Run(); err != nil {
			spiceServer.err = fmt.Errorf("start the SpiceDB container: %v: %s", err, stderr.String())
			return
		}
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			out, err := exec.Command("docker", "port", spiceServer.name, "50051/tcp").Output()
			if err == nil && len(out) > 0 {
				spiceServer.endpoint = strings.TrimSpace(strings.Split(string(out), "\n")[0])
				break
			}
		}
		if spiceServer.endpoint == "" {
			spiceServer.err = fmt.Errorf("the SpiceDB container publishes no port")
			return
		}
		// The container answers once its health check serves.
		conn, err := DialSpiceDB(SpiceDB{Endpoint: spiceServer.endpoint, Key: spiceServer.key, InsecureLoopback: true})
		if err != nil {
			spiceServer.err = err
			return
		}
		defer conn.Close()
		for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
			if spiceServer.err = reach(context.Background(), conn); spiceServer.err == nil {
				return
			}
		}
	})
	if spiceServer.err != nil {
		t.Fatal(spiceServer.err)
	}
	return spiceServer.endpoint, spiceServer.key
}

func stopSpiceDB() {
	if spiceServer.name != "" {
		_ = exec.Command("docker", "rm", "-f", spiceServer.name).Run()
	}
}
