package storage

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

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
