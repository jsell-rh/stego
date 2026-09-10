package command

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRelativeTimestamp(t *testing.T) {
	now := time.Date(2028, 2, 28, 23, 30, 0, 123, time.FixedZone("offset", 3600))
	for value, duration := range map[string]time.Duration{"30d": 30 * 24 * time.Hour, "24h": 24 * time.Hour, "1h30m": 90 * time.Minute, "1ns": time.Nanosecond, "+2h": 2 * time.Hour} {
		got, err := relativeTimestamp(value, now)
		want := now.Add(duration).UTC().Format(time.RFC3339Nano)
		if err != nil || got != want {
			t.Fatal(value, got, err, want)
		}
	}
	for _, value := range []string{"", "0", "0s", "-1h", "0d", "-1d", "1.5d", "1d2h", "d", "1 d", "18446744073709551616d", "106752d", "9223372036854775808ns", strings.Repeat("1", 65)} {
		if _, err := relativeTimestamp(value, now); err == nil {
			t.Fatal("invalid duration accepted", value)
		}
	}
	if _, err := relativeTimestamp("1h", time.Date(9999, 12, 31, 23, 30, 0, 0, time.UTC)); err == nil {
		t.Fatal("calendar overflow accepted")
	}
}

func relativeDefinition() Application {
	app := definition()
	app.Commands = []Command{{Name: []string{"create", "record"}, Method: "POST", Path: "/records", Fields: []Field{{Flag: "end-at", Key: "end_at", Type: "string", RelativeFlag: "end-in", Required: true}}, Success: []int{201}}}
	return app
}
func TestRelativeFlagDefinition(t *testing.T) {
	for _, change := range []func(*Command){
		func(c *Command) { c.Query = true }, func(c *Command) { c.Method = "GET" },
		func(c *Command) { c.Fields[0].Type = "integer" }, func(c *Command) { c.Fields[0].RelativeFlag = "body" },
		func(c *Command) { c.Fields[0].RelativeFlag = "end-at" }, func(c *Command) { c.Fields[0].RelativeFlag = "not valid" },
		func(c *Command) { c.Fields = append(c.Fields, Field{Flag: "end-in", Key: "other", Type: "string"}) },
		func(c *Command) {
			c.Fields = append([]Field{{Flag: "end-in", Key: "other", Type: "string"}}, c.Fields...)
		},
		func(c *Command) {
			c.Path = "/records/{record}"
			c.PathParameters = []PathParameter{{Flag: "end-in", Key: "record"}}
		},
	} {
		app := relativeDefinition()
		if err := validate(app); err != nil {
			t.Fatal(err)
		}
		change(&app.Commands[0])
		if err := validate(app); err == nil {
			t.Fatal("ambiguous relative definition accepted")
		}
	}
}
func TestRelativeFlagThroughTLS(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("TEST_CLI_CONFIG", filepath.Join(directory, "config.json"))
	token := filepath.Join(directory, "token")
	if err := os.WriteFile(token, []byte("private-token"), 0600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	received := make(chan string, 8)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]string
		if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 1 || body["end_at"] == "" {
			t.Error("incorrect request fields")
		}
		received <- body["end_at"]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		w.Write([]byte(`{"id":"one"}`))
	}))
	defer server.Close()
	ca := filepath.Join(directory, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	app := relativeDefinition()
	run := func(args ...string) error { return Run(context.Background(), app, args, &bytes.Buffer{}) }
	if err := run("login", "--url", server.URL, "--token-file", token, "--ca-file", ca); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct {
		value string
		delta time.Duration
	}{{"30d", 30 * 24 * time.Hour}, {"2h30m", 150 * time.Minute}} {
		before := time.Now()
		if err := run("create", "record", "--end-in", entry.value); err != nil {
			t.Fatal(err)
		}
		after := time.Now()
		got, err := time.Parse(time.RFC3339Nano, <-received)
		if err != nil || got.Before(before.Add(entry.delta)) || got.After(after.Add(entry.delta)) {
			t.Fatal("relative deadline is outside command interval", got, err)
		}
	}
	absolute := "2030-01-01T00:00:00Z"
	if err := run("create", "record", "--end-at", absolute); err != nil {
		t.Fatal(err)
	}
	if <-received != absolute {
		t.Fatal("absolute timestamp changed")
	}
	body := filepath.Join(directory, "body.json")
	if err := os.WriteFile(body, []byte(`{"end_at":"2030-01-01T00:00:00Z"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run("create", "record", "--body", body); err != nil {
		t.Fatal(err)
	}
	if <-received != absolute {
		t.Fatal("body timestamp changed")
	}
	for _, args := range [][]string{{"--end-in", "0d"}, {"--end-in", "-1h"}, {"--end-in", "99999999999999d"}, {"--end-in", "1h", "--end-at", absolute}, {"--end-at", absolute, "--end-in", "1h"}, {"--body", body, "--end-in", "1h"}, {"--end-in", "1h", "--end-in", "2h"}} {
		output := filepath.Join(directory, "invalid-output")
		before := calls.Load()
		command := append([]string{"create", "record", "--output-file", output}, args...)
		if err := run(command...); err == nil || calls.Load() != before {
			t.Fatal("invalid expiry reached server", err)
		}
		if _, err := os.Stat(output); !os.IsNotExist(err) {
			t.Fatal("invalid expiry reserved output", err)
		}
	}
	var help bytes.Buffer
	if err := Run(context.Background(), app, []string{"create", "record", "--help"}, &help); err != nil || !strings.Contains(help.String(), "--end-in DURATION") {
		t.Fatal("relative flag missing from help", err)
	}
}
