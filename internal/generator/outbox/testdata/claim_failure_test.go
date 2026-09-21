package queue

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// This driver keeps the real PostgreSQL write, then removes its client result.
// It models an unknown result; it does not simulate a network protocol failure.
type claimFaultConnector struct {
	database *sql.DB
	partial  bool
}

func (c claimFaultConnector) Connect(context.Context) (driver.Conn, error) {
	return claimFaultConnection{c}, nil
}
func (claimFaultConnector) Driver() driver.Driver { return claimFaultDriver{} }

type claimFaultDriver struct{}

func (claimFaultDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use the claim fault connector")
}

type claimFaultConnection struct{ claimFaultConnector }

func (claimFaultConnection) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (claimFaultConnection) Begin() (driver.Tx, error) {
	return nil, errors.New("unexpected transaction")
}
func (claimFaultConnection) Close() error { return nil }
func (c claimFaultConnection) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if !strings.Contains(query, "UPDATE stego_outbox.messages AS m") {
		return nil, errors.New("unexpected query")
	}
	values := make([]any, len(args))
	for i, value := range args {
		values[i] = value.Value
	}
	if !c.partial {
		if _, err := c.database.ExecContext(ctx, query, values...); err != nil {
			return nil, err
		}
		return nil, context.Canceled
	}
	rows, err := c.database.QueryContext(ctx, query, values...)
	if err != nil {
		return nil, err
	}
	columns, err := rows.Columns()
	if err != nil {
		rows.Close()
		return nil, err
	}
	return &claimFaultRows{rows: rows, columns: columns}, nil
}

type claimFaultRows struct {
	rows    *sql.Rows
	columns []string
	read    bool
}

func (r *claimFaultRows) Columns() []string { return r.columns }
func (r *claimFaultRows) Close() error      { return r.rows.Close() }
func (r *claimFaultRows) Next(destination []driver.Value) error {
	if r.read {
		if err := r.rows.Close(); err != nil {
			return err
		}
		return context.Canceled
	}
	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			return err
		}
		return io.EOF
	}
	values := make([]any, len(destination))
	for i := range destination {
		values[i] = &destination[i]
	}
	if err := r.rows.Scan(values...); err != nil {
		return err
	}
	r.read = true
	return nil
}
func faultQueue(t *testing.T, database *sql.DB, partial bool) *Queue {
	t.Helper()
	connection := sql.OpenDB(claimFaultConnector{database: database, partial: partial})
	connection.SetMaxOpenConns(1)
	t.Cleanup(func() { connection.Close() })
	queue, err := New(connection)
	if err != nil {
		t.Fatal(err)
	}
	return queue
}

func TestClaimDiscardsPartialResults(t *testing.T) {
	database := testDatabase(t)
	enqueue(t, database, message("first"), message("second"))
	claimed, err := faultQueue(t, database, true).Claim(context.Background(), 2, time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the lost result error: %v", err)
	}
	if claimed != nil {
		t.Fatal("claim returned a partial batch after a row error")
	}
	var retained int
	if err := database.QueryRow("SELECT count(*) FROM stego_outbox.messages WHERE attempts = 1 AND lease_token IS NOT NULL AND lease_until > clock_timestamp()").Scan(&retained); err != nil || retained != 2 {
		t.Fatalf("the unknown claim did not retain both leases: %d, %v", retained, err)
	}
}

func TestUnknownClaimResultRecoversAfterLeaseExpiry(t *testing.T) {
	for _, partial := range []bool{false, true} {
		name := "query_error"
		if partial {
			name = "partial_rows"
		}
		t.Run(name, func(t *testing.T) {
			database := testDatabase(t)
			first, second := message("first"), message("second")
			enqueue(t, database, first, second)
			claimed, err := faultQueue(t, database, partial).Claim(context.Background(), 2, 5*time.Second)
			if !errors.Is(err, context.Canceled) || claimed != nil {
				t.Fatal("failed claim exposed a result", err)
			}
			var token uuid.UUID
			var active int
			if err := database.QueryRow("SELECT lease_token, count(*) FROM stego_outbox.messages WHERE attempts = 1 AND lease_until > clock_timestamp() GROUP BY lease_token").Scan(&token, &active); err != nil || active != 2 || token == uuid.Nil {
				t.Fatalf("claimed leases were not retained: %d, %v", active, err)
			}
			recovered, err := New(database)
			if err != nil {
				t.Fatal(err)
			}
			if rows, err := recovered.Claim(context.Background(), 2, time.Minute); err != nil || len(rows) != 0 {
				t.Fatal("another caller acquired an active lease", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var rows []Delivery
			for {
				rows, err = recovered.Claim(ctx, 2, time.Minute)
				if err != nil {
					t.Fatal("lease recovery failed", err)
				}
				if len(rows) != 0 {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("stored leases did not expire")
				case <-time.After(20 * time.Millisecond):
				}
			}
			if len(rows) != 2 {
				t.Fatal("lease recovery lost a row")
			}
			expected := map[uuid.UUID]Message{first.ID: first, second.ID: second}
			for _, row := range rows {
				original, ok := expected[row.ID]
				if !ok || row.Attempts != 2 || row.Receipt.Token == token || row.ResourceKey != original.ResourceKey || row.Kind != original.Kind {
					t.Fatal("lease recovery changed the message or claim identity")
				}
				// JSONB can change spacing. Compare the fixture's JSON values.
				var expectedPayload, actualPayload any
				if err := json.Unmarshal(original.Payload, &expectedPayload); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(row.Payload, &actualPayload); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(expectedPayload, actualPayload) {
					t.Fatal("lease recovery changed the message payload")
				}
				delete(expected, row.ID)
				if changed, err := recovered.Acknowledge(ctx, Receipt{ID: row.ID, Token: token}); err != nil || changed {
					t.Fatal("stale receipt removed a new claim", err)
				}
				if changed, err := recovered.Acknowledge(ctx, row.Receipt); err != nil || !changed {
					t.Fatal("new receipt did not acknowledge its claim", err)
				}
			}
			if len(expected) != 0 || count(t, database, "stego_outbox.messages") != 0 {
				t.Fatal("recovery left a message incomplete")
			}
		})
	}
}
