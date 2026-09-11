package storage

import (
	"context"
	"database/sql/driver"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

type connectionProbe struct {
	connect func(context.Context) (driver.Conn, error)
}

func (p connectionProbe) Connect(ctx context.Context) (driver.Conn, error) { return p.connect(ctx) }
func (connectionProbe) Driver() driver.Driver                              { return nil }

type lateConnection struct{ closed bool }

func (c *lateConnection) Close() error                      { c.closed = true; return nil }
func (*lateConnection) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (*lateConnection) Begin() (driver.Tx, error)           { return nil, errors.New("unused") }

func TestConnectionBudgetKeepsCallerContext(t *testing.T) {
	type key struct{}
	parent := context.WithValue(context.Background(), key{}, "caller")
	probe := databaseConnector{Connector: connectionProbe{connect: func(ctx context.Context) (driver.Conn, error) {
		deadline, ok := ctx.Deadline()
		left := time.Until(deadline)
		if !ok || left <= 0 || left > 5*time.Second || ctx.Value(key{}) != "caller" {
			t.Fatal("connection lost its bounded caller context")
		}
		return nil, errors.New("test connection failure")
	}}}
	probe.Connect(parent)
	short, cancel := context.WithTimeout(parent, 30*time.Millisecond)
	defer cancel()
	probe.Connector = connectionProbe{connect: func(ctx context.Context) (driver.Conn, error) {
		deadline, _ := ctx.Deadline()
		want, _ := short.Deadline()
		if !deadline.Equal(want) {
			t.Fatal("connection extended the caller deadline")
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	if _, err := probe.Connect(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("connection lost the caller deadline error", err)
	}
	canceled, stop := context.WithCancel(parent)
	stop()
	late := new(lateConnection)
	probe.Connector = connectionProbe{connect: func(context.Context) (driver.Conn, error) { return late, nil }}
	if conn, err := probe.Connect(canceled); conn != nil || !errors.Is(err, context.Canceled) || !late.closed {
		t.Fatal("connection completed after cancellation without being closed", err)
	}
}

func TestConnectionDeadlineStopsStalledAuthentication(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	defer func() {
		listener.Close()
		<-done
		select {
		case conn := <-accepted:
			conn.Close()
		default:
		}
	}()
	pool, err := OpenDatabase("postgres://test:test@" + listener.Addr().String() + "/test?sslmode=disable&connect_timeout=0")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	started := time.Now()
	err = pool.PingContext(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || time.Since(started) > 7*time.Second {
		t.Fatal("stalled authentication exceeded the common connection budget", err)
	}
}

func TestConnectionBudgetDoesNotExpireTheSession(t *testing.T) {
	pool, err := OpenDatabase(poolTestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	conn, err := pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SELECT pg_sleep(5.1)"); err != nil {
		t.Fatal("connection budget limited an established session", err)
	}
	if err := conn.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
}

var poolVariables = []string{"STEGO_DATABASE_MAX_OPEN_CONNECTIONS", "STEGO_DATABASE_MAX_IDLE_CONNECTIONS", "STEGO_DATABASE_CONNECTION_MAX_LIFETIME", "STEGO_DATABASE_CONNECTION_MAX_IDLE_TIME"}

func clearPoolSettings(t *testing.T) {
	t.Helper()
	for _, name := range poolVariables {
		value, present := os.LookupEnv(name)
		t.Cleanup(func() {
			if present {
				os.Setenv(name, value)
			} else {
				os.Unsetenv(name)
			}
		})
		os.Unsetenv(name)
	}
}
func TestPoolSettingsRejectInvalidValues(t *testing.T) {
	clearPoolSettings(t)
	for _, tc := range []struct {
		name   string
		values []string
	}{
		{poolVariables[0], []string{"", "0", "-1", "1025", "+2", " 2", "2 ", "2.0", "1e2", "99999999999999999999999", "private-value"}},
		{poolVariables[1], []string{"", "-1", "17", "+1", "0.5", "private-value"}},
		{poolVariables[2], []string{"", "0", "0s", "999ms", "-1s", "25h", "private-value", strings.Repeat("1", 64) + "s"}},
		{poolVariables[3], []string{"", "0s", "-1s", "25h", "private-value"}},
	} {
		for _, value := range tc.values {
			t.Run(tc.name+"/"+value, func(t *testing.T) {
				t.Setenv(tc.name, value)
				pool, err := OpenDatabase("postgres://private-user:private-password@localhost/private-db?sslmode=disable")
				if pool != nil {
					pool.Close()
					t.Fatal("invalid settings created a pool")
				}
				if err == nil || err.Error() != "invalid database pool setting: "+tc.name {
					t.Fatal("invalid pool setting lost its safe error", err)
				}
			})
		}
	}
	settings, err := readDatabasePoolSettings()
	if err != nil || settings.open != 16 || settings.idle != 4 || settings.lifetime != 30*time.Minute || settings.idleTime != 5*time.Minute {
		t.Fatal("pool defaults differ", settings, err)
	}
	t.Setenv(poolVariables[0], "1")
	settings, err = readDatabasePoolSettings()
	if err != nil || settings.idle != 1 {
		t.Fatal("default idle limit exceeds a smaller open limit")
	}
	t.Setenv(poolVariables[1], "0")
	t.Setenv(poolVariables[2], "1s")
	t.Setenv(poolVariables[3], "24h")
	settings, err = readDatabasePoolSettings()
	if err != nil || settings.idle != 0 || settings.lifetime != time.Second || settings.idleTime != 24*time.Hour {
		t.Fatal("valid boundary settings rejected", err)
	}
}
func poolTestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("STEGO_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("STEGO_REQUIRE_POSTGRES") == "1" {
			t.Fatal("PostgreSQL is required")
		}
		t.Skip("PostgreSQL is not configured")
	}
	return dsn
}
func TestPoolLimitWaitCancellationAndRelease(t *testing.T) {
	clearPoolSettings(t)
	t.Setenv(poolVariables[0], "2")
	t.Setenv(poolVariables[1], "1")
	pool, err := OpenDatabase(poolTestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first, err := pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	short, done := context.WithTimeout(ctx, 50*time.Millisecond)
	third, err := pool.Conn(short)
	done()
	if third != nil {
		third.Close()
		t.Fatal("pool exceeded its open limit")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("pool wait lost caller deadline", err)
	}
	stats := pool.Stats()
	if stats.MaxOpenConnections != 2 || stats.OpenConnections != 2 || stats.InUse != 2 || stats.WaitCount != 1 || stats.WaitDuration <= 0 {
		t.Fatal("pool wait statistics differ", stats)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	extra, canceledErr := pool.Conn(canceled)
	if extra != nil {
		extra.Close()
		t.Fatal("canceled caller acquired a connection")
	}
	if !errors.Is(canceledErr, context.Canceled) {
		t.Fatal("pool wait lost caller cancellation", canceledErr)
	}
	first.Close()
	if err := pool.PingContext(ctx); err != nil {
		t.Fatal("pool did not recover after release", err)
	}
	second.Close()
	stats = pool.Stats()
	if stats.InUse != 0 || stats.Idle > 1 || stats.MaxIdleClosed == 0 {
		t.Fatal("pool did not enforce idle count", stats)
	}
}
func TestPoolRetiresExpiredConnections(t *testing.T) {
	for _, setting := range []string{poolVariables[2], poolVariables[3]} {
		t.Run(setting, func(t *testing.T) {
			clearPoolSettings(t)
			t.Setenv(poolVariables[0], "1")
			t.Setenv(poolVariables[1], "1")
			t.Setenv(setting, "1s")
			pool, err := OpenDatabase(poolTestDSN(t))
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			var old int
			if err := pool.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&old); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(4 * time.Second)
			for {
				stats := pool.Stats()
				retired := stats.MaxLifetimeClosed
				if setting == poolVariables[3] {
					retired = stats.MaxIdleTimeClosed
				}
				if retired > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("expired connection was not retired", stats)
				}
				time.Sleep(20 * time.Millisecond)
			}
			var next int
			if err := pool.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&next); err != nil || next == old {
				t.Fatal("retired connection was reused", err)
			}
		})
	}
}
