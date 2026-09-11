package storage

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"example.com/dbprobe/tracing"
	"github.com/jackc/pgx/v5"
)

func TestDatabaseConfigurationDoesNotReadSettingsFromCredentials(t *testing.T) {
	t.Setenv("PGTZ", "")
	t.Setenv("PGSERVICE", "")
	for _, dsn := range []string{
		"postgres://user:TimeZone%3DEurope%2FParis@localhost/db?sslmode=disable",
		"host=localhost user=user password='TimeZone=Europe/Paris' dbname=db sslmode=disable",
	} {
		config, err := databaseConfiguration(dsn)
		if err != nil || config.Password != "TimeZone=Europe/Paris" {
			t.Fatal("credential parsing changed", err)
		}
		for key := range config.RuntimeParams {
			if strings.EqualFold(key, "timezone") {
				t.Fatal("credential text changed the database timezone")
			}
		}
	}
	config, err := databaseConfiguration("postgres://user:secret@localhost/db?sslmode=disable&TimeZone=UTC")
	if err != nil || config.RuntimeParams["TimeZone"] != "UTC" {
		t.Fatal("explicit database setting was lost", err)
	}
	if _, err := databaseConfiguration("host='private-unclosed"); err == nil || err.Error() != "invalid database configuration" {
		t.Fatal("invalid connection settings exposed parser details")
	}
}

func TestDriverQueryLifetimeAndTransactions(t *testing.T) {
	dsn := os.Getenv("STEGO_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("STEGO_REQUIRE_POSTGRES") == "1" {
			t.Fatal("PostgreSQL is required")
		}
		t.Skip("PostgreSQL is not configured")
	}
	db, err := OpenDatabase(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), tracing.Key{}, "owner"), 15*time.Second)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	require := func(call, outcome string) {
		t.Helper()
		select {
		case event := <-tracing.Events:
			if event.Call != call || event.Outcome != outcome || event.Value != "owner" {
				t.Fatalf("wrong driver event: %+v", event)
			}
		case <-ctx.Done():
			t.Fatal("driver completion missing")
		}
	}
	require("connect", "success")
	rows, err := conn.QueryContext(ctx, "SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	require("prepare", "success")
	select {
	case event := <-tracing.Events:
		t.Fatalf("query completed before rows closed: %+v", event)
	default:
	}
	rows.Close()
	require("query", "success")
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	require("query", "success")
	if _, err = tx.ExecContext(ctx, "CREATE TEMP TABLE private_probe (value text UNIQUE)"); err != nil {
		t.Fatal(err)
	}
	require("query", "success")
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	require("query", "success")
	if _, err = conn.ExecContext(ctx, "INSERT INTO private_probe VALUES ('private-value')"); err != nil {
		t.Fatal(err)
	}
	require("query", "success")
	if _, err = conn.ExecContext(ctx, "INSERT INTO private_probe VALUES ('private-value')"); err == nil {
		t.Fatal("constraint failure missing")
	}
	require("query", "failure")
	tx, err = conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	require("query", "success")
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	require("query", "success")
	short, stop := context.WithTimeout(ctx, 40*time.Millisecond)
	_, err = conn.ExecContext(short, "SELECT pg_sleep(2)")
	stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline changed: %v", err)
	}
	require("query", "deadline")
}

func TestDriverNativeOperations(t *testing.T) {
	dsn := os.Getenv("STEGO_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("STEGO_REQUIRE_POSTGRES") == "1" {
			t.Fatal("PostgreSQL is required")
		}
		t.Skip("PostgreSQL is not configured")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.Tracer = databaseTracer{}
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), tracing.Key{}, "owner"), 10*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err = conn.Prepare(ctx, "private-statement", "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, "CREATE TEMP TABLE private_copy (value text)"); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.CopyFrom(ctx, pgx.Identifier{"private_copy"}, []string{"value"}, pgx.CopyFromRows([][]any{{"private-copy-value"}})); err != nil {
		t.Fatal(err)
	}
	batch := &pgx.Batch{}
	batch.Queue("SELECT 1")
	if err = conn.SendBatch(ctx, batch).Close(); err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for len(tracing.Events) > 0 {
		event := <-tracing.Events
		if event.Outcome != "success" || event.Value != "owner" {
			t.Fatal("native call lost its context or result")
		}
		found[event.Call] = true
	}
	for _, name := range []string{"connect", "prepare", "query", "copy", "batch"} {
		if !found[name] {
			t.Fatal("native call missing", name)
		}
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, err := conn.Exec(canceled, "SELECT 1"); !errors.Is(err, context.Canceled) {
		t.Fatal("caller cancellation changed", err)
	}
	select {
	case event := <-tracing.Events:
		if event.Call != "query" || event.Outcome != "canceled" || event.Value != "owner" {
			t.Fatal("canceled query lost its result or context")
		}
	case <-ctx.Done():
		t.Fatal("canceled query completion missing")
	}
}
