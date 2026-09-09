package main

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"example.com/shared/fills/rules"
	contract "example.com/shared/out/contracts/storage"
	"example.com/shared/out/internal/api"
	"example.com/shared/out/internal/queue"
	"example.com/shared/out/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	gormschema "gorm.io/gorm/schema"
)

type observedNamer struct {
	gormschema.NamingStrategy
	started *atomic.Bool
	late    *atomic.Int32
}

func (n observedNamer) ColumnName(table, column string) string {
	if n.started.Load() {
		n.late.Add(1)
	}
	return n.NamingStrategy.ColumnName(table, column)
}

func TestStorePreparesRelatedSchemasBeforeConcurrentUse(t *testing.T) {
	for range 20 {
		var started atomic.Bool
		var late atomic.Int32
		orm, err := gorm.Open(postgres.Open("host=127.0.0.1 port=1 user=test dbname=test sslmode=disable"), &gorm.Config{DryRun: true, DisableAutomaticPing: true, NamingStrategy: observedNamer{started: &started, late: &late}, Logger: logger.Default.LogMode(logger.Silent)})
		if err != nil {
			t.Fatal(err)
		}
		db, err := orm.DB()
		if err != nil {
			t.Fatal(err)
		}
		storage, err := store.NewStore(orm)
		if err != nil {
			t.Fatal(err)
		}
		started.Store(true)
		var wait sync.WaitGroup
		for _, entity := range []string{"Record", "Membership"} {
			wait.Go(func() {
				if _, err := storage.List(context.Background(), entity, "", "", contract.ListOptions{Page: 1, Size: 1, CountOnly: true}); err != nil {
					t.Error(err)
				}
			})
		}
		wait.Wait()
		db.Close()
		if late.Load() != 0 {
			t.Fatal("store deferred schema initialization until concurrent queries")
		}
	}
}

//go:embed internal/queue/migrations/000001_outbox.sql
var schema string

func TestSearchPreservesAccessAndExactNumbers(t *testing.T) {
	storage, _ := testStore(t)
	ctx := context.Background()
	for i, name := range []string{"a", "b", "c"} {
		if err := storage.Create(ctx, "Record", map[string]any{"id": name, "name": name, "serial": int64(9007199254740992) + int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := storage.Create(ctx, "Membership", map[string]any{"record_id": "b", "subject": "reader"}); err != nil {
		t.Fatal(err)
	}
	options := contract.ListOptions{Page: 1, Size: 20, Related: []contract.RelatedFilter{{Entity: "Membership", ForeignField: "record_id", Values: map[string][]string{"subject": {"reader"}}}}, Search: "name = 'a' or name = 'b' or name = 'c'"}
	result, err := storage.List(ctx, "Record", "", "", options)
	if err != nil || result.Total != 1 || result.Items.([]store.Record)[0].ID != "b" {
		t.Fatalf("OR search bypassed access: %+v %v", result, err)
	}
	options.Search = "serial = 9007199254740993"
	options.Related = nil
	result, err = storage.List(ctx, "Record", "", "", options)
	if err != nil || result.Total != 1 || result.Items.([]store.Record)[0].ID != "b" {
		t.Fatalf("integer precision changed: %+v %v", result, err)
	}
	options.Search = "serial + 1 in ()"
	result, err = storage.List(ctx, "Record", "", "", options)
	if err != nil || result.Total != 0 {
		t.Fatalf("empty membership search: %+v %v", result, err)
	}
	for _, search := range []string{`"name) OR TRUE --" = 'x'`, "serial like 'x'", "serial = 'not-an-integer'", "created_at = 'not-a-date'", "missing = 'x'", "name = 'x' or missing = 'y'"} {
		options.Search = search
		if _, err := storage.List(ctx, "Record", "", "", options); !errors.Is(err, contract.ErrSearch) {
			t.Fatalf("invalid search %s: %v", search, err)
		}
	}
	options.Search = ""
	options.CountOnly = true
	options.OrderBy = []contract.OrderByField{{Field: "name", Direction: "desc; SELECT 1"}}
	if _, err := storage.List(ctx, "Record", "", "", options); err == nil {
		t.Fatal("count-only accepted invalid ordering")
	}
}

func testStore(t *testing.T) (*store.Store, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("STEGO_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("STEGO_REQUIRE_POSTGRES") == "1" {
			t.Fatal("PostgreSQL is required")
		}
		t.Skip("set STEGO_TEST_POSTGRES_DSN to test shared storage")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*config)
	t.Cleanup(func() { admin.Close() })
	name := "stego_contract_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, `DROP DATABASE "`+name+`" WITH (FORCE)`); err != nil {
			t.Errorf("remove private database: %v", err)
		}
	})
	config.Database = name
	db := stdlib.OpenDB(*config)
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { db.Close() })
	orm, err := gorm.Open(postgres.New(postgres.Config{Conn: db}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(orm.WithContext(ctx)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		t.Fatal(err)
	}
	storage, err := store.NewStore(orm)
	if err != nil {
		t.Fatal(err)
	}
	return storage, db
}

func TestDomainRuleThroughPublicContract(t *testing.T) {
	storage, db := testStore(t)
	ctx := context.Background()
	id := uuid.NewString()
	event := contract.Notification{ID: uuid.New(), Destination: "audit", ResourceKey: id, Kind: "record.created", Payload: []byte(`{"version":1}`)}
	value := map[string]any{"id": id, "name": "shared"}
	if err := rules.Create(ctx, storage, value, event); err != nil {
		t.Fatal(err)
	}
	q, err := queue.New(db)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := q.Claim(ctx, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].ID != event.ID {
		t.Fatalf("domain notification lost: %v", messages)
	}
	var shared contract.Notification = messages[0].Message
	if shared.ResourceKey != id {
		t.Fatal("queue contract changed resource key")
	}

	handler := api.NewRecordsHandler(storage)
	request := httptest.NewRequest(http.MethodGet, "/records/"+id, nil)
	request.SetPathValue("id", id)
	response := httptest.NewRecorder()
	handler.Read(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("HTTP read after domain write: %d %s", response.Code, response.Body.String())
	}
	var resource map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &resource); err != nil {
		t.Fatal(err)
	}
	if resource["id"] != id || resource["name"] != "shared" {
		t.Fatalf("resource mismatch: %v", resource)
	}

	request.SetPathValue("id", uuid.NewString())
	response = httptest.NewRecorder()
	handler.Read(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("shared missing-resource error returned %d", response.Code)
	}
	if api.ErrNotFound != contract.ErrNotFound || store.ErrNotFound != contract.ErrNotFound {
		t.Fatal("storage and HTTP errors have different identities")
	}
}

func TestDomainNotificationFailureRollsBack(t *testing.T) {
	storage, _ := testStore(t)
	id := uuid.NewString()
	event := contract.Notification{ID: uuid.New(), Destination: "audit", ResourceKey: id, Kind: "record.created", Payload: []byte(`bad-json`)}
	if err := rules.Create(context.Background(), storage, map[string]any{"id": id, "name": "invalid"}, event); err == nil {
		t.Fatal("invalid domain notification was accepted")
	}
	if _, err := storage.Get(context.Background(), "Record", id); !errors.Is(err, contract.ErrNotFound) {
		t.Fatalf("domain rule left partial state: %v", err)
	}
}

func TestRelatedFilterCountsAndPagesOnlyVisibleRecords(t *testing.T) {
	storage, db := testStore(t)
	ctx := context.Background()
	for _, name := range []string{"a", "b", "c", "d"} {
		if err := storage.Create(ctx, "Record", map[string]any{"id": name, "name": name}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"b", "d"} {
		// Duplicate grants must not duplicate resources or change the count.
		for range 2 {
			if err := storage.Create(ctx, "Membership", map[string]any{"record_id": id, "subject": "reader"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	opts := contract.ListOptions{Page: 1, Size: 1, OrderBy: []contract.OrderByField{{Field: "name", Direction: "asc"}}, Related: []contract.RelatedFilter{{Entity: "Membership", ForeignField: "record_id", Values: map[string][]string{"subject": {"reader"}}}}}
	for page, want := range []string{"b", "d"} {
		opts.Page = page + 1
		result, err := storage.List(ctx, "Record", "", "", opts)
		if err != nil {
			t.Fatal(err)
		}
		rows := result.Items.([]store.Record)
		if result.Total != 2 || len(rows) != 1 || rows[0].ID != want {
			t.Fatalf("filtered page: %+v", result)
		}
	}
	opts.CountOnly = true
	counted, err := storage.List(ctx, "Record", "", "", opts)
	if err != nil || counted.Total != 2 || len(counted.Items.([]store.Record)) != 0 {
		t.Fatalf("count-only relation filter: %+v, %v", counted, err)
	}
	opts.CountOnly = false
	opts.Related[0].Values["subject"] = nil
	result, err := storage.List(ctx, "Record", "", "", opts)
	if err != nil || result.Total != 0 {
		t.Fatalf("empty filter must deny all rows: %+v, %v", result, err)
	}
	opts.Related[0].Values["subject"] = []string{"' OR true --"}
	result, err = storage.List(ctx, "Record", "", "", opts)
	if err != nil || result.Total != 0 {
		t.Fatalf("filter value became SQL: %+v, %v", result, err)
	}
	opts.Related[0].Values["subject"] = []string{"reader"}
	// Recovery can include a deleted root, but must retain live-grant filters.
	if err := storage.Delete(ctx, "Record", "b"); err != nil {
		t.Fatal(err)
	}
	archived := opts
	archived.IncludeDeleted = true
	archived.Page = 1
	archived.Size = 20
	recovered, err := storage.List(ctx, "Record", "", "", archived)
	if err != nil || recovered.Total != 2 {
		t.Fatalf("deleted root recovery: %+v %v", recovered, err)
	}
	rows := recovered.Items.([]store.Record)
	if len(rows) != 2 || rows[0].ID != "b" || !rows[0].DeletedAt.Valid {
		t.Fatalf("deleted root missing: %+v", rows)
	}
	live, err := storage.List(ctx, "Record", "", "", opts)
	if err != nil || live.Total != 1 {
		t.Fatalf("normal list included deleted root: %+v %v", live, err)
	}
	if _, err := db.Exec("UPDATE memberships SET deleted_at=now()"); err != nil {
		t.Fatal(err)
	}
	result, err = storage.List(ctx, "Record", "", "", opts)
	if err != nil || result.Total != 0 {
		t.Fatalf("deleted grant allowed access: %+v, %v", result, err)
	}
	recovered, err = storage.List(ctx, "Record", "", "", archived)
	if err != nil || recovered.Total != 0 {
		t.Fatalf("recovery included deleted grant: %+v %v", recovered, err)
	}
	for _, filter := range []contract.RelatedFilter{
		{Entity: "Record", ForeignField: "name"},
		{Entity: "Membership", ForeignField: "record_id; SELECT 1"},
		{Entity: "Membership", ForeignField: "record_id", Values: map[string][]string{"subject OR true --": {"reader"}}},
	} {
		opts.Related = []contract.RelatedFilter{filter}
		if _, err := storage.List(ctx, "Record", "", "", opts); err == nil {
			t.Fatal("invalid related filter was accepted")
		}
	}
}

func TestLiveUniqueKeysPreserveHistory(t *testing.T) {
	storage, db := testStore(t)
	ctx := context.Background()
	for _, entity := range []string{"Lease", "Reservation", "Alias"} {
		first := map[string]any{"id": "first", "tenant": "tenant", "name": "shared"}
		second := map[string]any{"id": "second", "tenant": "tenant", "name": "shared"}
		if err := storage.Create(ctx, entity, first); err != nil {
			t.Fatal(entity, err)
		}
		if err := storage.Create(ctx, entity, second); !errors.Is(err, contract.ErrConflict) {
			t.Fatal("live duplicate", entity, err)
		}
		if _, err := storage.Upsert(ctx, entity, second, []string{"name"}, ""); err == nil {
			t.Fatal("upsert accepted a live key", entity)
		}
		if err := storage.Delete(ctx, entity, "first"); err != nil {
			t.Fatal(err)
		}
		if err := storage.Create(ctx, entity, second); err != nil {
			t.Fatal("key remained reserved", entity, err)
		}
		if _, err := storage.Get(ctx, entity, "first"); !errors.Is(err, contract.ErrNotFound) {
			t.Fatal("deleted row is visible", err)
		}
	}
	var old int
	if err := db.QueryRow("SELECT count(*) FROM leases WHERE id='first' AND deleted_at IS NOT NULL").Scan(&old); err != nil || old != 1 {
		t.Fatal("history lost", old, err)
	}
	for _, id := range []string{"null-a", "null-b"} {
		if err := storage.Create(ctx, "Alias", map[string]any{"id": id}); err != nil {
			t.Fatal("NULL semantics changed", err)
		}
	}
	// A normal unique key continues to reserve its value after deletion.
	if err := storage.Create(ctx, "Record", map[string]any{"id": "old", "name": "reserved"}); err != nil {
		t.Fatal(err)
	}
	if err := storage.Delete(ctx, "Record", "old"); err != nil {
		t.Fatal(err)
	}
	if err := storage.Create(ctx, "Record", map[string]any{"id": "new", "name": "reserved"}); !errors.Is(err, contract.ErrConflict) {
		t.Fatal("normal unique key changed", err)
	}
	// Concurrent callers must not create two live rows with the same key.
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, id := range []string{"racer-a", "racer-b"} {
		go func() {
			<-start
			results <- storage.Create(ctx, "Lease", map[string]any{"id": id, "tenant": "other", "name": "race"})
		}()
	}
	close(start)
	successes, conflicts := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			successes++
		} else if errors.Is(err, contract.ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatal("concurrent uniqueness failed", successes, conflicts)
	}
}

func TestRowFilterUnionAndDeclaredReferencePaths(t *testing.T) {
	storage, _ := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		if err := storage.Create(ctx, "Record", map[string]any{"id": id, "name": id}); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []map[string]any{
		{"id": "a-owner", "record_id": "a", "subject": "owner"},
		{"id": "a-other", "record_id": "a", "subject": "other"},
		{"id": "b-viewer", "record_id": "b", "subject": "viewer"},
		{"id": "c-hidden", "record_id": "c", "subject": "other"},
	} {
		if err := storage.Create(ctx, "Membership", row); err != nil {
			t.Fatal(err)
		}
	}
	filter := &contract.RowFilter{All: []contract.RowFilter{
		{Related: &contract.RelatedFilter{Entity: "Record", LocalField: "record_id", ForeignField: "id"}},
		{Any: []contract.RowFilter{
			{Field: "subject", Values: []string{"viewer"}},
			{Related: &contract.RelatedFilter{Entity: "Membership", LocalField: "record_id", ForeignField: "record_id", Values: map[string][]string{"subject": {"owner"}}}},
		}},
	}}
	opts := contract.ListOptions{Page: 1, Size: 1, Filter: filter, OrderBy: []contract.OrderByField{{Field: "id", Direction: "asc"}}}
	for page, want := range []string{"a-other", "a-owner", "b-viewer"} {
		opts.Page = page + 1
		result, err := storage.List(ctx, "Membership", "", "", opts)
		if err != nil {
			t.Fatal(err)
		}
		rows := result.Items.([]store.Membership)
		if result.Total != 3 || len(rows) != 1 || rows[0].ID != want {
			t.Fatalf("union page: %+v", result)
		}
	}
	opts.Page, opts.Size = 1, 20
	// An outer scope and an OR search cannot widen the access filter.
	opts.Search = "subject = 'viewer' or subject = 'other'"
	result, err := storage.List(ctx, "Membership", "record_id", "c", opts)
	if err != nil || result.Total != 0 {
		t.Fatalf("scope bypass: %+v %v", result, err)
	}
	result, err = storage.List(ctx, "Membership", "", "", opts)
	if err != nil || result.Total != 2 {
		t.Fatalf("search bypass: %+v %v", result, err)
	}
	opts.Search = ""
	opts.CountOnly = true
	result, err = storage.List(ctx, "Membership", "", "", opts)
	if err != nil || result.Total != 3 || len(result.Items.([]store.Membership)) != 0 {
		t.Fatalf("count: %+v %v", result, err)
	}
	opts.CountOnly = false
	if err := storage.Delete(ctx, "Membership", "a-owner"); err != nil {
		t.Fatal(err)
	}
	opts.IncludeDeleted = true
	result, err = storage.List(ctx, "Membership", "", "", opts)
	if err != nil || result.Total != 1 {
		t.Fatalf("deleted related row allowed access: %+v %v", result, err)
	}
	if err := storage.Delete(ctx, "Record", "b"); err != nil {
		t.Fatal(err)
	}
	result, err = storage.List(ctx, "Membership", "", "", opts)
	if err != nil || result.Total != 0 {
		t.Fatalf("deleted parent allowed access: %+v %v", result, err)
	}
	// Values remain parameters, including SQL-like input. Empty IN matches none.
	for _, values := range [][]string{nil, {"viewer' OR true --"}} {
		opts.Filter = &contract.RowFilter{Field: "subject", Values: values}
		result, err = storage.List(ctx, "Membership", "", "", opts)
		if err != nil || result.Total != 0 {
			t.Fatalf("literal filter: %+v %v", result, err)
		}
	}
	cyclic := contract.RowFilter{Any: make([]contract.RowFilter, 1)}
	cyclic.Any[0] = cyclic
	wide := contract.RowFilter{Any: make([]contract.RowFilter, 65)}
	for i := range wide.Any {
		wide.Any[i] = contract.RowFilter{Field: "subject", Values: []string{"viewer"}}
	}
	invalid := []contract.RowFilter{
		{}, {All: []contract.RowFilter{}}, {Any: []contract.RowFilter{}},
		{Field: "subject", Any: []contract.RowFilter{{Field: "id"}}},
		{Field: "id OR true --", Values: []string{"x"}},
		{Values: []string{"unused"}, Related: &contract.RelatedFilter{Entity: "Record", LocalField: "record_id", ForeignField: "id"}},
		{Related: &contract.RelatedFilter{Entity: "Record", LocalField: "subject", ForeignField: "id"}},
		{Related: &contract.RelatedFilter{Entity: "Lease", LocalField: "record_id", ForeignField: "id"}},
		{Related: &contract.RelatedFilter{Entity: "Record", LocalField: "record_id", ForeignField: "name"}},
		{Field: "subject", Values: make([]string, 101)}, cyclic, wide,
	}
	for _, filter := range invalid {
		opts.Filter = &filter
		if _, err := storage.List(ctx, "Membership", "", "", opts); err == nil {
			t.Fatal("invalid row filter accepted")
		}
	}
}

func TestLiteralTextMatchPreservesScopeAndPaging(t *testing.T) {
	storage, _ := testStore(t)
	ctx := context.Background()
	for _, row := range []map[string]any{
		{"id": "one", "tenant": "owner", "name": `Build%_!\Night`},
		{"id": "two", "tenant": "owner", "name": "Build plain"},
		{"id": "three", "tenant": "other", "name": `Build%_!\Night`},
		{"id": "four", "tenant": "owner", "name": "' OR true --"},
	} {
		if err := storage.Create(ctx, "Lease", row); err != nil {
			t.Fatal(err)
		}
	}
	opts := contract.ListOptions{Page: 1, Size: 1, OrderBy: []contract.OrderByField{{Field: "id", Direction: "asc"}}, ImplicitFilters: map[string]string{"tenant": "owner"}}
	match := func(value string, page int, total int64, want string) {
		t.Helper()
		opts.Page = page
		opts.Filter = &contract.RowFilter{Text: &contract.TextMatch{Fields: []string{"name", "tenant"}, Value: value}}
		result, err := storage.List(ctx, "Lease", "", "", opts)
		if err != nil {
			t.Fatal(err)
		}
		rows := result.Items.([]store.Lease)
		if result.Total != total {
			t.Fatalf("text total: %+v", result)
		}
		if want == "" {
			if len(rows) != 0 {
				t.Fatalf("unexpected text match: %+v", rows)
			}
		} else if len(rows) != 1 || rows[0].ID != want {
			t.Fatalf("text page: %+v", rows)
		}
	}
	match("BUILD", 1, 2, "one")
	match("BUILD", 2, 2, "two")
	match("BUILD", 3, 2, "")
	match(`%_!\nIgHt`, 1, 1, "one")
	match("' OR true --", 1, 1, "four")
	match("other", 1, 0, "")
	match(strings.Repeat("x", 4096), 1, 0, "")
	opts.CountOnly = true
	match("BUILD", 1, 2, "")
	opts.CountOnly = false
	if err := storage.Delete(ctx, "Lease", "one"); err != nil {
		t.Fatal(err)
	}
	match("BUILD", 1, 1, "two")
	// A nullable string must not match a nonempty value.
	if err := storage.Create(ctx, "Alias", map[string]any{"id": "null"}); err != nil {
		t.Fatal(err)
	}
	result, err := storage.List(ctx, "Alias", "", "", contract.ListOptions{Page: 1, Size: 10, Filter: &contract.RowFilter{Text: &contract.TextMatch{Fields: []string{"name"}, Value: "null"}}})
	if err != nil || result.Total != 0 {
		t.Fatalf("NULL text match: %+v %v", result, err)
	}
	invalid := []contract.RowFilter{
		{Text: &contract.TextMatch{Fields: []string{"name"}, Value: ""}},
		{Text: &contract.TextMatch{Fields: []string{"name"}, Value: string([]byte{255})}},
		{Text: &contract.TextMatch{Fields: []string{"name"}, Value: "\x00"}},
		{Text: &contract.TextMatch{Fields: []string{"name"}, Value: strings.Repeat("x", 4097)}},
		{Text: &contract.TextMatch{Value: "x"}},
		{Text: &contract.TextMatch{Fields: []string{"name", "name"}, Value: "x"}},
		{Text: &contract.TextMatch{Fields: []string{"id"}, Value: "x"}},
		{Text: &contract.TextMatch{Fields: []string{"serial"}, Value: "1"}},
		{Text: &contract.TextMatch{Fields: []string{"name OR true --"}, Value: "x"}},
		{Text: &contract.TextMatch{Fields: make([]string, 9), Value: "x"}},
		{Text: &contract.TextMatch{Fields: []string{"name"}, Value: "x"}, Field: "name"},
		{Text: &contract.TextMatch{Fields: []string{"name"}, Value: "x"}, Values: []string{"unused"}},
	}
	group := contract.RowFilter{All: make([]contract.RowFilter, 17)}
	for i := range group.All {
		group.All[i] = contract.RowFilter{Text: &contract.TextMatch{Fields: []string{"name"}, Value: strings.Repeat("x", 4096)}}
	}
	invalid = append(invalid, group)
	for _, filter := range invalid {
		if _, err := storage.List(ctx, "Record", "", "", contract.ListOptions{Page: 1, Size: 1, Filter: &filter}); err == nil {
			t.Fatal("invalid text match accepted")
		}
	}
}
