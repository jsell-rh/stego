package storage

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	contract "example.com/transaction-test/contracts/storage"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type cursorQueryLog struct {
	logger.Interface
	reads  atomic.Int64
	counts atomic.Int64
}

func (l *cursorQueryLog) Trace(_ context.Context, _ time.Time, query func() (string, int64), _ error) {
	sql, _ := query()
	sql = strings.ToLower(strings.TrimSpace(sql))
	if strings.HasPrefix(sql, "select ") {
		l.reads.Add(1)
	}
	if strings.Contains(sql, "count(") {
		l.counts.Add(1)
	}
}
func cursorLogger(s *Store) *cursorQueryLog {
	log := &cursorQueryLog{Interface: logger.Default.LogMode(logger.Silent)}
	s.db = s.db.Session(&gorm.Session{Logger: log})
	return log
}

func TestCursorPagesUseDatabaseOrderWithoutCounts(t *testing.T) {
	for _, collation := range []string{"C", "und-x-icu"} {
		t.Run(collation, func(t *testing.T) {
			s, db := database(t, false)
			ctx := context.Background()
			ddl := "ALTER TABLE records ALTER COLUMN id TYPE text COLLATE \"C\""
			if collation == "und-x-icu" {
				ddl = "ALTER TABLE records ALTER COLUMN id TYPE text COLLATE \"und-x-icu\""
			}
			if _, err := db.Exec(ddl); err != nil {
				t.Fatal(err)
			}
			for i, id := range []string{"a", "B", "c", "D", "e", "F"} {
				row := record("public-" + id)
				row.ID = id
				if i == 5 {
					row.Value = 2
				}
				if err := s.Create(ctx, "Record", row); err != nil {
					t.Fatal(err)
				}
				if i == 2 || i == 3 {
					if err := s.Delete(ctx, "Record", id); err != nil {
						t.Fatal(err)
					}
				}
			}
			log := cursorLogger(s)
			for _, mode := range []contract.CursorDeletion{contract.CursorLive, contract.CursorAll, contract.CursorDeleted} {
				query := "SELECT id FROM records WHERE value=1"
				if mode == contract.CursorLive {
					query += " AND deleted_at IS NULL"
				}
				if mode == contract.CursorDeleted {
					query += " AND deleted_at IS NOT NULL"
				}
				query += " ORDER BY id"
				rows, err := db.Query(query)
				if err != nil {
					t.Fatal(err)
				}
				var expected []string
				for rows.Next() {
					var id string
					if err := rows.Scan(&id); err != nil {
						t.Fatal(err)
					}
					expected = append(expected, id)
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				rows.Close()
				if collation == "und-x-icu" && mode == contract.CursorAll && slices.IsSorted(expected) {
					t.Fatal("ICU fixture has Go string order")
				}
				var got []string
				after := ""
				for page := 0; page < 10; page++ {
					log.reads.Store(0)
					log.counts.Store(0)
					result, err := s.ReadCursor(ctx, "Record", "", "", contract.CursorOptions{AfterID: after, Limit: 2, Deletion: mode, Fields: []string{"name"}, ImplicitFilters: map[string]string{"value": "1"}, Filter: &contract.RowFilter{Text: &contract.TextMatch{Fields: []string{"name"}, Value: "public-"}}})
					if err != nil {
						t.Fatal(err)
					}
					selected := result.Items.([]Record)
					if len(selected) > 2 || log.counts.Load() != 0 || log.reads.Load() != 1 {
						t.Fatal("cursor query count or row bound", len(selected), log.counts.Load(), log.reads.Load())
					}
					for _, row := range selected {
						if row.Value != 0 || row.Name == "" || row.ResourceVersion < 1 {
							t.Fatal("field or lifecycle projection", row)
						}
						got = append(got, row.ID)
					}
					if len(selected) > 0 && result.NextID != selected[len(selected)-1].ID {
						t.Fatal("wrong continuation")
					}
					if result.More != (len(got) < len(expected)) {
						t.Fatal("lookahead is not exact")
					}
					if !result.More {
						break
					}
					after = result.NextID
				}
				if !slices.Equal(got, expected) {
					t.Fatal("cursor lost database order or visibility", got, expected)
				}
			}
			log.counts.Store(0)
			result, err := s.List(ctx, "Record", "", "", contract.ListOptions{Page: 1, Size: 1})
			if err != nil || result.Total != 4 || log.counts.Load() != 1 {
				t.Fatal("ordinary list lost its count", result, err)
			}
		})
	}
}

func TestCursorRejectsInvalidRequestsBeforeQueries(t *testing.T) {
	s, _ := database(t, false)
	log := cursorLogger(s)
	ctx := context.Background()
	for _, opts := range []contract.CursorOptions{
		{}, {Limit: -1}, {Limit: 1001}, {Limit: 1, Deletion: 3}, {Limit: 1, AfterID: strings.Repeat("a", 257)},
		{Limit: 1, AfterID: string([]byte{0xff})}, {Limit: 1, AfterID: "a\x00b"},
		{Limit: 1, Fields: []string{"missing"}}, {Limit: 1, Fields: []string{"name", "name"}},
		{Limit: 1, ImplicitFilters: map[string]string{"name;DROP TABLE records": ""}},
	} {
		if _, err := s.ReadCursor(ctx, "Record", "", "", opts); !errors.Is(err, contract.ErrCursor) {
			t.Fatal("invalid options", err)
		}
	}
	for _, scope := range [][2]string{{"name", ""}, {"", "value"}, {"missing", "value"}} {
		if _, err := s.ReadCursor(ctx, "Record", scope[0], scope[1], contract.CursorOptions{Limit: 1}); !errors.Is(err, contract.ErrCursor) {
			t.Fatal("invalid scope", err)
		}
	}
	if _, err := s.ReadCursor(nil, "Record", "", "", contract.CursorOptions{Limit: 1}); !errors.Is(err, contract.ErrCursor) {
		t.Fatal(err)
	}
	if _, err := s.ReadCursor(ctx, "Missing", "", "", contract.CursorOptions{Limit: 1}); !errors.Is(err, contract.ErrCursor) {
		t.Fatal(err)
	}
	var empty *Store
	if _, err := empty.ReadCursor(ctx, "Record", "", "", contract.CursorOptions{Limit: 1}); !errors.Is(err, contract.ErrCursor) {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.ReadCursor(canceled, "Record", "", "", contract.CursorOptions{Limit: 1}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := s.ReadCursor(ctx, "Record", "", "", contract.CursorOptions{Limit: 1, Search: "name = 'x'"}); !errors.Is(err, contract.ErrSearch) {
		t.Fatal("missing search provider", err)
	}
	if log.reads.Load() != 0 {
		t.Fatal("invalid cursor performed a query")
	}
}

func TestCursorBindsInputAndKeepsRelatedAccess(t *testing.T) {
	s, _ := database(t, false)
	ctx := context.Background()
	for _, id := range []string{"a-public", "b-hidden", "c-public"} {
		row := record(id)
		row.ID = id
		if id == "b-hidden" {
			row.Value = 2
		}
		if err := s.Create(ctx, "Record", row); err != nil {
			t.Fatal(err)
		}
	}
	opts := contract.CursorOptions{Limit: 10, Related: []contract.RelatedFilter{{Entity: "Record", ForeignField: "id", Values: map[string][]string{"value": {"1"}}}}}
	result, err := s.ReadCursor(ctx, "Record", "", "", opts)
	if err != nil {
		t.Fatal(err)
	}
	rows := result.Items.([]Record)
	if len(rows) != 2 || rows[0].ID != "a-public" || rows[1].ID != "c-public" {
		t.Fatal("cursor escaped related access", rows)
	}
	// The malicious boundary can sort before or after these IDs under the
	// database collation. It must never make a hidden record visible.
	attack := opts
	attack.AfterID = "' OR 1=1 --"
	bounded, err := s.ReadCursor(ctx, "Record", "", "", attack)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range bounded.Items.([]Record) {
		if row.ID != "a-public" && row.ID != "c-public" {
			t.Fatal("cursor input escaped related access", row.ID)
		}
	}
	if err := s.Delete(ctx, "Record", "a-public"); err != nil {
		t.Fatal(err)
	}
	opts.Deletion = contract.CursorAll
	result, err = s.ReadCursor(ctx, "Record", "", "", opts)
	if err != nil || len(result.Items.([]Record)) != 1 {
		t.Fatal("deleted related row granted access", result, err)
	}
}

func TestCursorUsesTransactionAndHonorsCancellation(t *testing.T) {
	s, db := database(t, false)
	ctx := context.Background()
	err := s.WithTransaction(ctx, func(ctx context.Context, tx contract.Transaction) error {
		row := record("uncommitted")
		row.ID = "one"
		if err := tx.Create(ctx, "Record", row); err != nil {
			return err
		}
		result, err := tx.(contract.CursorReader).ReadCursor(ctx, "Record", "", "", contract.CursorOptions{Limit: 1})
		if err != nil {
			return err
		}
		if len(result.Items.([]Record)) != 1 || result.More {
			t.Fatal("cursor left transaction", result)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	locked, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Rollback()
	if _, err := locked.Exec("LOCK TABLE records IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := s.ReadCursor(bounded, "Record", "", "", contract.CursorOptions{Limit: 1}); err == nil {
		t.Fatal("blocked read ignored cancellation")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("cursor did not release a canceled call")
	}
}

func TestCursorRejectsUnusableStoredIdentity(t *testing.T) {
	s, db := database(t, false)
	if _, err := db.Exec("INSERT INTO records(id,name,value,created_time,updated_time) VALUES ($1,'oversized',1,now(),now())", strings.Repeat("z", 257)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadCursor(context.Background(), "Record", "", "", contract.CursorOptions{Limit: 1}); !errors.Is(err, contract.ErrCursorResult) {
		t.Fatal("unusable continuation", err)
	}
}

func BenchmarkRecoveryCursorPage(b *testing.B) {
	s, db := database(b, false)
	if _, err := db.Exec("INSERT INTO records(id,name,value,created_time,updated_time) SELECT lpad(n::text,8,'0'),'record-'||n,1,now(),now() FROM generate_series(1,10000) n"); err != nil {
		b.Fatal(err)
	}
	if _, err := db.Exec("ANALYZE records"); err != nil {
		b.Fatal(err)
	}
	for _, cursor := range []bool{false, true} {
		b.Run(fmt.Sprint("cursor=", cursor), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if cursor {
					result, err := s.ReadCursor(context.Background(), "Record", "", "", contract.CursorOptions{Limit: 100, Fields: []string{"name"}})
					if err != nil || len(result.Items.([]Record)) != 100 {
						b.Fatal(result, err)
					}
				} else {
					result, err := s.List(context.Background(), "Record", "", "", contract.ListOptions{Page: 1, Size: 100, Fields: []string{"name"}, OrderBy: []contract.OrderByField{{Field: "id", Direction: "asc"}}})
					if err != nil || result.Total != 10000 || len(result.Items.([]Record)) != 100 {
						b.Fatal(result, err)
					}
				}
			}
		})
	}
}
