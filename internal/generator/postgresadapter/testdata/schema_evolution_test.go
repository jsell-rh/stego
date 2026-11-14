package ledger_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"example.com/schema-gate/ledger"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// The row-level security of a pooled service, which the ledger of a release on main holds after 001_initial.
func init() { ledger.RegisterSQL("100_tenancy_row_security", "SELECT 1") }

// ledgerServer opens the PostgreSQL server of the test, or skips with no server.
func ledgerServer(t *testing.T) *pgx.ConnConfig {
	t.Helper()
	dsn := os.Getenv("STEGO_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("STEGO_REQUIRE_POSTGRES") == "1" {
			t.Fatal("PostgreSQL fixture is required")
		}
		t.Skip("PostgreSQL fixture is not configured")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// ledgerDB makes a new database on the server and opens it.
func ledgerDB(t *testing.T, cfg *pgx.ConnConfig) *sql.DB {
	t.Helper()
	admin := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { admin.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	name := "ledger_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})
	copied := cfg.Copy()
	copied.Database = name
	db := stdlib.OpenDB(*copied)
	t.Cleanup(func() { db.Close() })
	return db
}

type ledgerRow struct {
	version string
	set     sql.NullString
}

func ledgerState(t *testing.T, db *sql.DB) (columns int, rows []ledgerRow) {
	t.Helper()
	if err := db.QueryRow(`SELECT count(*) FROM pg_catalog.pg_attribute WHERE attrelid = 'stego_schema.migrations'::pg_catalog.regclass AND attname = 'compatible_with' AND NOT attisdropped`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	query := "SELECT version, NULL::text FROM stego_schema.migrations ORDER BY version"
	if columns == 1 {
		query = "SELECT version, compatible_with::text FROM stego_schema.migrations ORDER BY version"
	}
	result, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Close()
	for result.Next() {
		var row ledgerRow
		if err := result.Scan(&row.version, &row.set); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	return columns, rows
}

func TestLedgerColumn(t *testing.T) {
	t.Run("ST-SUP-RC01-OC29-T01 The migration job adds the column, leaves the old rows with no set and writes no ledger row for it", func(t *testing.T) {
		cfg := ledgerServer(t)
		ctx := context.Background()
		db := ledgerDB(t, cfg)
		// The release before: its ledger holds 001_initial and 100_tenancy_row_security and has no compatible_with.
		if err := ledger.Migrate(ctx, db); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("ALTER TABLE stego_schema.migrations DROP COLUMN compatible_with"); err != nil {
			t.Fatal(err)
		}
		if columns, rows := ledgerState(t, db); columns != 0 || len(rows) != 2 || rows[0].version != "001_initial" || rows[1].version != "100_tenancy_row_security" {
			t.Fatalf("the ledger of the release before is %d column(s) and %v", columns, rows)
		}
		if err := ledger.CheckLedger(ctx, db); err != nil {
			t.Fatalf("the read-only check refuses a ledger with no column: %v", err)
		}
		if err := ledger.VerifySchema(ctx, db); err != nil {
			t.Fatalf("the schema check refuses the ledger of the release before: %v", err)
		}
		for run := 1; run <= 2; run++ {
			if err := ledger.Migrate(ctx, db); err != nil {
				t.Fatalf("migration job run %d: %v", run, err)
			}
			columns, rows := ledgerState(t, db)
			if columns != 1 || len(rows) != 2 {
				t.Fatalf("after run %d the ledger has %d compatible_with column(s) and the rows %v, want one column and the two rows", run, columns, rows)
			}
			for _, row := range rows {
				if row.set.Valid {
					t.Errorf("after run %d the row %s of the release before has the set %s, want none", run, row.version, row.set.String)
				}
			}
		}
		if err := ledger.VerifySchema(ctx, db); err != nil {
			t.Fatalf("the schema check refuses the ledger with its new column: %v", err)
		}
		if err := ledger.CheckLedger(ctx, db); err != nil {
			t.Fatalf("the read-only check refuses the ledger with its new column: %v", err)
		}
		fresh := ledgerDB(t, cfg)
		if err := ledger.Migrate(ctx, fresh); err != nil {
			t.Fatal(err)
		}
		if columns, rows := ledgerState(t, fresh); columns != 1 || len(rows) != 2 {
			t.Fatalf("a new database has %d compatible_with column(s) and the rows %v", columns, rows)
		}
		// Any other change to the ledger table is refused.
		if _, err := db.Exec("ALTER TABLE stego_schema.migrations ADD COLUMN extra text"); err != nil {
			t.Fatal(err)
		}
		if err := ledger.VerifySchema(ctx, db); !errors.Is(err, ledger.ErrSchemaGeneration) {
			t.Fatalf("the schema check accepts a ledger with another column: %v", err)
		}
		if _, err := db.Exec("ALTER TABLE stego_schema.migrations DROP COLUMN extra; ALTER TABLE stego_schema.migrations ALTER COLUMN compatible_with SET NOT NULL"); err == nil {
			if err := ledger.VerifySchema(ctx, db); !errors.Is(err, ledger.ErrSchemaGeneration) {
				t.Fatalf("the schema check accepts a NOT NULL compatible_with: %v", err)
			}
		}
	})
}
