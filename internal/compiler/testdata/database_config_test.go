package main

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMountedDatabaseSecret(t *testing.T) {
	name := os.Getenv("STEGO_TEST_PROJECTED_DATABASE_URL_FILE")
	if name == "" {
		t.Skip("set STEGO_TEST_PROJECTED_DATABASE_URL_FILE for a mounted Secret")
	}
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DATABASE_URL_FILE", name)
	value, err := stegoDatabaseURL()
	if err != nil {
		t.Fatal("mounted database Secret was rejected")
	}
	address, err := url.Parse(value)
	if err != nil || address.Scheme != "postgres" || address.Query().Get("sslmode") != "verify-full" {
		t.Fatal("mounted database Secret changed")
	}
}

func TestDatabaseSources(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DATABASE_URL_FILE", "")
	reject := func() {
		t.Helper()
		value, err := stegoDatabaseURL()
		if value != "" || err == nil || err.Error() != "invalid database configuration source" {
			t.Fatal("invalid source did not return a fixed error")
		}
	}
	reject()
	t.Setenv("DATABASE_URL", "private-environment-value")
	if value, err := stegoDatabaseURL(); err != nil || value != "private-environment-value" {
		t.Fatal("environment source changed", err)
	}
	t.Setenv("DATABASE_URL_FILE", "/private-missing-file")
	reject()
	t.Setenv("DATABASE_URL", strings.Repeat("a", 65537))
	t.Setenv("DATABASE_URL_FILE", "")
	reject()
	t.Setenv("DATABASE_URL", "")
	for _, name := range []string{"relative-file", "/private-missing-file", "/" + strings.Repeat("a", 4096), t.TempDir(), "/dev/zero"} {
		t.Setenv("DATABASE_URL_FILE", name)
		reject()
	}
	directory := t.TempDir()
	fifo := filepath.Join(directory, "private-fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_URL_FILE", fifo)
	start := time.Now()
	reject()
	if time.Since(start) > time.Second {
		t.Fatal("FIFO source blocked startup")
	}
	loop := filepath.Join(directory, "private-loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_URL_FILE", loop)
	reject()
	name := filepath.Join(directory, "private-database-url")
	t.Setenv("DATABASE_URL_FILE", name)
	for _, mode := range []os.FileMode{0600, 0400, 0440, 0640, 0644, 0660, 0700, 0610, 0601} {
		if err := os.WriteFile(name, []byte("private-value"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(name, mode); err != nil {
			t.Fatal(err)
		}
		valid := mode == 0600 || mode == 0400 || mode == 0440 || mode == 0640
		value, err := stegoDatabaseURL()
		if valid && (err != nil || value != "private-value") {
			t.Fatal("private file rejected", mode, err)
		}
		if !valid {
			reject()
		}
		if err := os.Chmod(name, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, data := range []string{"", "\n", "\r\n", "private\x00value", "private\nvalue", "private\r", "private\n\r\n", strings.Repeat("a", 65537)} {
		if err := os.WriteFile(name, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		reject()
	}
	for _, ending := range []string{"", "\n", "\r\n"} {
		const value = "host=db password=' private value ' "
		if err := os.WriteFile(name, []byte(value+ending), 0600); err != nil {
			t.Fatal(err)
		}
		if got, err := stegoDatabaseURL(); err != nil || got != value {
			t.Fatal("file source changed credential whitespace", err)
		}
	}
	if err := os.WriteFile(name, []byte(strings.Repeat("a", 65536)), 0600); err != nil {
		t.Fatal(err)
	}
	if value, err := stegoDatabaseURL(); err != nil || len(value) != 65536 {
		t.Fatal("bounded file rejected", err)
	}
}

func TestProjectedDatabaseSecret(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DATABASE_URL_FILE", filepath.Join(directory, "url"))
	if err := os.Symlink("..data/url", filepath.Join(directory, "url")); err != nil {
		t.Fatal(err)
	}
	for _, revision := range []string{"one", "two"} {
		version := filepath.Join(directory, revision)
		if err := os.Mkdir(version, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(version, "url"), []byte(revision), 0440); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(revision, filepath.Join(directory, "..next")); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(directory, "..next"), filepath.Join(directory, "..data")); err != nil {
			t.Fatal(err)
		}
		if got, err := stegoDatabaseURL(); err != nil || got != revision {
			t.Fatal("projected secret revision was not read", err)
		}
	}
}
