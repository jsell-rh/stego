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
