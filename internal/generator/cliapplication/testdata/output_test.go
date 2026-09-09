package command

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
)

func TestScopedCredentialOutput(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("TEST_CLI_CONFIG", filepath.Join(directory, "config.json"))
	token := filepath.Join(directory, "token")
	if err := os.WriteFile(token, []byte("private-token"), 0600); err != nil {
		t.Fatal(err)
	}
	app := definition()
	app.Commands = append(app.Commands, Command{Name: []string{"issue", "key"}, Method: "POST", Path: "/projects/{project}/keys", PathParameters: []PathParameter{{Flag: "project-id", Key: "project"}}, Sensitive: true, Fields: []Field{{Flag: "name", Key: "name", Type: "string", Required: true}}, Success: []int{201}})
	var calls atomic.Int32
	var mode atomic.Int32
	var target atomic.Value
	target.Store("")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/projects/project-one/keys" {
			t.Error("incorrect scoped route")
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 1 || body["name"] != "build" {
			t.Error("path or output flags entered request body")
		}
		if name := target.Load().(string); name != "" {
			info, err := os.Stat(name)
			if err != nil || info.Size() != 0 || info.Mode().Perm() != 0600 {
				t.Error("private file was not reserved before the request")
			}
		}
		if mode.Load() == 1 {
			w.WriteHeader(503)
			w.Write([]byte("secret-in-error"))
			return
		}
		w.WriteHeader(201)
		if mode.Load() == 2 {
			w.Write([]byte(`{"secret":"one","secret":"two"}`))
			return
		}
		w.Write([]byte(`{"secret":"one-time-secret","number":9007199254740993}`))
	}))
	defer server.Close()
	ca := filepath.Join(directory, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0644); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	run := func(args ...string) error { output.Reset(); return Run(context.Background(), app, args, &output) }
	if err := run("login", "--url", server.URL, "--token-file", token, "--ca-file", ca); err != nil {
		t.Fatal(err)
	}
	args := []string{"issue", "key", "--project-id", "project-one", "--name", "build"}
	existing := filepath.Join(directory, "existing")
	os.WriteFile(existing, []byte("keep"), 0600)
	link := filepath.Join(directory, "link")
	os.Symlink(existing, link)
	fifo := filepath.Join(directory, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	unsafe := filepath.Join(directory, "unsafe")
	os.Mkdir(unsafe, 0700)
	os.Chmod(unsafe, 0777)
	for _, extra := range [][]string{nil, {"--output-file", ""}, {"--output-file", existing}, {"--output-file", link}, {"--output-file", fifo}, {"--output-file", directory}, {"--output-file", filepath.Join(unsafe, "new")}, {"--output-file", filepath.Join(directory, "missing", "new")}} {
		if err := run(append(append([]string{}, args...), extra...)...); err == nil {
			t.Fatal("unsafe output accepted")
		}
		if output.Len() != 0 {
			t.Fatal("failed command wrote stdout")
		}
	}
	for _, id := range []string{"../escape", "bad/value", "%2f", ""} {
		if err := run("issue", "key", "--project-id", id, "--name", "build", "--output-file", "-"); err == nil {
			t.Fatal("invalid path value accepted")
		}
	}
	if err := run("issue", "key", "--name", "build", "--output-file", "-"); err == nil {
		t.Fatal("missing path value accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("invalid input reached the server")
	}
	name := filepath.Join(directory, "credential.json")
	target.Store(name)
	if err := run(append(args, "--output-file", name)...); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(name)
	if err != nil || !bytes.Contains(data, []byte("one-time-secret")) || !bytes.Contains(data, []byte("9007199254740993")) || output.Len() != 0 {
		t.Fatal("credential output failed")
	}
	if err := run(append(args, "--output-file", name)...); err == nil || calls.Load() != 1 {
		t.Fatal("existing credential was replaced")
	}
	for _, state := range []int32{1, 2} {
		mode.Store(state)
		failed := filepath.Join(directory, "failed.json")
		target.Store(failed)
		err := run(append(args, "--output-file", failed)...)
		if err == nil || strings.Contains(err.Error(), "secret") || output.Len() != 0 {
			t.Fatal("failed API output exposed response data")
		}
		if _, err := os.Stat(failed); !os.IsNotExist(err) {
			t.Fatal("empty failed reservation remains")
		}
	}
	mode.Store(0)
	target.Store("")
	if err := run(append(args, "--output-file", "-")...); err != nil || !strings.Contains(output.String(), "one-time-secret") {
		t.Fatal("explicit stdout output failed")
	}
	body := filepath.Join(directory, "body.json")
	os.WriteFile(body, []byte(`{"name":"build"}`), 0600)
	if err := run("issue", "key", "--project-id", "project-one", "--body", body, "--output-file", "-"); err != nil {
		t.Fatal("body file did not work with scoped output")
	}
}

type privateErrorWriter struct{}

func (privateErrorWriter) Write([]byte) (int, error) { return 0, errors.New("private-value") }

type shortWriter struct{}

func (shortWriter) Write(data []byte) (int, error) { return len(data) - 1, nil }

func TestCredentialOutputWriteFailures(t *testing.T) {
	for _, writer := range []interface{ Write([]byte) (int, error) }{privateErrorWriter{}, shortWriter{}} {
		reserved, err := reserveOutput("-", writer)
		if err != nil {
			t.Fatal(err)
		}
		err = reserved.write([]byte("private-value"))
		reserved.close()
		if err == nil || strings.Contains(err.Error(), "private-value") {
			t.Fatal("output failure leaked data or passed")
		}
	}
	name := filepath.Join(t.TempDir(), "output")
	var stdout bytes.Buffer
	reserved, err := reserveOutput(name, &stdout)
	if err != nil {
		t.Fatal(err)
	}
	reserved.file.Close()
	if err := reserved.write([]byte("private-value")); err == nil {
		t.Fatal("closed output accepted")
	}
	reserved.close()
	if _, err := os.Stat(name); err != nil || stdout.Len() != 0 {
		t.Fatal("failed file output was removed or sent to stdout")
	}
}

func TestInvalidScopedDefinitions(t *testing.T) {
	for _, command := range []Command{
		{Path: "/projects/{project}/keys", PathParameters: []PathParameter{{Flag: "project-id", Key: "missing"}}},
		{Path: "/projects/{project}/keys", PathParameters: []PathParameter{{Flag: "output-file", Key: "project"}}},
		{Path: "/projects/{project}/keys", PathParameters: []PathParameter{{Flag: "project-id", Key: "project"}, {Flag: "second", Key: "project"}}},
		{Path: "/projects/prefix{project}/keys", PathParameters: []PathParameter{{Flag: "project-id", Key: "project"}}},
		{Path: "/projects/{project}/{project}", PathParameters: []PathParameter{{Flag: "project-id", Key: "project"}}},
		{Path: "/projects/{project}/keys", PathParameters: []PathParameter{{Flag: "name", Key: "project"}}, Fields: []Field{{Flag: "name", Key: "name", Type: "string"}}},
		{Path: "/keys", Fields: []Field{{Flag: "output-file", Key: "value", Type: "string"}}},
	} {
		command.Name = []string{"issue", "key"}
		command.Method = "POST"
		command.Success = []int{201}
		app := definition()
		app.Commands = []Command{command}
		if err := validate(app); err == nil {
			t.Fatal("invalid definition accepted")
		}
	}
}
