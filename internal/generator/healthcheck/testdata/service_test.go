package main

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestAssembledHealthBypassesOnlyProbeAuthentication(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "service")
	build := exec.Command("go", "build", "-race", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, output)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary)
	command.Env = append(os.Environ(), "PORT=0", "GORACE=atexit_sleep_ms=0")
	output, err := command.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		command.Process.Signal(syscall.SIGTERM)
		if err := command.Wait(); err != nil {
			t.Error("service did not stop cleanly", err)
		}
	}()
	scanner := bufio.NewScanner(output)
	address := ""
	for scanner.Scan() {
		line := scanner.Text()
		if _, port, ok := strings.Cut(line, "starting server on [::]:"); ok {
			address = "http://127.0.0.1:" + port
			break
		}
	}
	if address == "" {
		t.Fatal("service did not start")
	}
	client := &http.Client{Timeout: time.Second}
	for path, want := range map[string]int{"/livez": 200, "/readyz": 200, "/private": 401} {
		deadline := time.Now().Add(time.Second)
		for {
			r, err := client.Get(address + path)
			if err != nil {
				t.Fatal(err)
			}
			r.Body.Close()
			if r.StatusCode == want {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("unexpected probe or auth status", path, r.StatusCode)
			}
			time.Sleep(time.Millisecond)
		}
	}
	request, _ := http.NewRequest("GET", address+"/private", nil)
	request.Header.Set("X-Test-Authorized", "yes")
	result, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	result.Body.Close()
	if result.StatusCode != 204 {
		t.Fatal("protected handler was not reached", result.StatusCode)
	}

}
