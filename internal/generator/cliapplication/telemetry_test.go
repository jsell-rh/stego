package cliapplication_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func testTelemetryProcess(t *testing.T, project string) {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "cli")
	build := exec.Command("go", "build", "-race", "-o", binary, "./out/cli/cmd")
	build.Dir = project
	if data, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build telemetry CLI: %v %s", err, data)
	}
	env := []string{}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "OTEL_") {
			env = append(env, entry)
		}
	}
	cfg := filepath.Join(t.TempDir(), "config.json")
	env = append(env, "TEST_CLI_CONFIG="+cfg, "GORACE=atexit_sleep_ms=0")
	run := func(args ...string) ([]byte, string, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = env
		var output, diagnostics bytes.Buffer
		cmd.Stdout = &output
		cmd.Stderr = &diagnostics
		err := cmd.Run()
		return output.Bytes(), diagnostics.String(), err
	}
	check := func(data, outcome string) {
		t.Helper()
		count := 0
		for _, line := range strings.Split(data, "\n") {
			var record map[string]any
			if json.Unmarshal([]byte(line), &record) == nil && record["event.name"] == "cli.command.completed" {
				count++
				if record["outcome"] != outcome {
					t.Fatal("wrong command outcome", record["outcome"])
				}
			}
		}
		if count != 1 || strings.Contains(data, "private-") {
			t.Fatal("missing or unsafe command completion", count)
		}
	}
	output, diagnostics, err := run("version")
	if err != nil || !json.Valid(output) {
		t.Fatal("telemetry changed version stdout", err)
	}
	check(diagnostics, "success")
	output, diagnostics, err = run("private-unknown-command")
	if err == nil || len(output) != 0 {
		t.Fatal("command failure changed")
	}
	check(diagnostics, "failure")
	entered := make(chan struct{}, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { entered <- struct{}{}; <-r.Context().Done() }))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	server.StartTLS()
	defer server.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("private-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err = run("login", "--url", server.URL, "--token-file", tokenFile, "--ca-file", ca); err != nil {
		t.Fatal("CLI login failed", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "get", "record", "private-id")
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-entered:
	case <-ctx.Done():
		<-done
		t.Fatal("CLI request did not start")
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled command exited successfully")
		}
	case <-time.After(4 * time.Second):
		cancel()
		<-done
		t.Fatal("CLI did not stop after cancellation")
	}
	if stdout.Len() != 0 {
		t.Fatal("cancellation wrote command output")
	}
	check(stderr.String(), "canceled")
	if !strings.Contains(stderr.String(), `"event.name":"http.client.request.completed"`) {
		t.Fatal("CLI HTTP call has no local completion")
	}
}
