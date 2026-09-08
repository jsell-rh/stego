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
)

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
	return store.NewStore(orm), db
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
	if _, err := db.Exec("UPDATE memberships SET deleted_at=now()"); err != nil {
		t.Fatal(err)
	}
	result, err = storage.List(ctx, "Record", "", "", opts)
	if err != nil || result.Total != 0 {
		t.Fatalf("deleted grant allowed access: %+v, %v", result, err)
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
