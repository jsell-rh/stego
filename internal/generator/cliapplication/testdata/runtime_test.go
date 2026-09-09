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
	"syscall"
	"testing"
)

func definition() Application {
	return Application{ConfigEnv: "TEST_CLI_CONFIG", ConfigName: "sample", Commands: []Command{
		{Name: []string{"create", "record"}, Method: "POST", Path: "/records", Fields: []Field{{Flag: "name", Key: "name", Type: "string", Required: true}, {Flag: "tags", Key: "tags", Type: "string-list"}, {Flag: "count", Key: "count", Type: "integer"}}, Success: []int{201}},
		{Name: []string{"get", "record"}, Method: "GET", Path: "/records/{id}", ID: true, Success: []int{200}},
		{Name: []string{"list", "records"}, Method: "GET", Path: "/records", Query: true, Fields: []Field{{Flag: "search", Key: "search", Type: "string"}}, Success: []int{200}},
		{Name: []string{"delete", "record"}, Method: "DELETE", Path: "/records/{id}", ID: true, Confirm: true, Success: []int{204}},
	}}
}
func TestCommandWorkflowAndBoundaries(t *testing.T) {
	directory := t.TempDir()
	config := filepath.Join(directory, "config.json")
	t.Setenv("TEST_CLI_CONFIG", config)
	token := filepath.Join(directory, "token")
	if err := os.WriteFile(token, []byte("first-token"), 0600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var mode atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		want := "first-token"
		if mode.Load() == 1 {
			want = "second-token"
		}
		if r.Header.Get("Authorization") != "Bearer "+want {
			t.Error("incorrect token")
		}
		w.Header().Set("Content-Type", "application/json")
		switch mode.Load() {
		case 2:
			w.WriteHeader(500)
			w.Write([]byte("private response first-token"))
			return
		case 3:
			w.Header().Set("Location", "https://elsewhere.invalid")
			w.WriteHeader(302)
			return
		case 4:
			w.Write([]byte(`{"id":"a","id":"b"}`))
			return
		}
		switch r.Method {
		case "POST":
			var body struct {
				Name  string
				Tags  []string
				Count int64
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Name != "sample" {
				t.Error("invalid body")
			}
			w.WriteHeader(201)
			w.Write([]byte(`{"id":"one","name":"sample","sequence":9007199254740993}`))
		case "DELETE":
			if r.URL.Path != "/records/one" {
				t.Error("incorrect delete path")
			}
			w.WriteHeader(204)
		default:
			if r.URL.RawQuery != "" && r.URL.Query().Get("search") != "literal%_!" {
				t.Error("query encoding")
			}
			w.Write([]byte(`{"id":"one","name":"sample","sequence":9007199254740993}`))
		}
	}))
	defer server.Close()
	ca := filepath.Join(directory, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, error) {
		var output bytes.Buffer
		err := Run(context.Background(), definition(), args, &output)
		return output.String(), err
	}
	login := []string{"login", "--url", server.URL, "--token-file", token, "--ca-file", ca}
	if _, err := run(login...); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(config)
	if err != nil || bytes.Contains(saved, []byte("first-token")) {
		t.Fatal("token copied to configuration")
	}
	info, err := os.Stat(config)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("configuration permissions")
	}
	for _, args := range [][]string{
		{"create", "record", "--name", "sample", "--tags", `["a","b"]`, "--count", "9007199254740993"},
		{"get", "record", "one"}, {"list", "records", "--search", "literal%_!"}, {"delete", "record", "--yes", "one"},
	} {
		if _, err := run(args...); err != nil {
			t.Fatal(err)
		}
	}
	if output, err := run("get", "record", "one"); err != nil || !strings.Contains(output, "9007199254740993") {
		t.Fatal("exact response number changed")
	}
	one := definition()
	one.Commands = append(one.Commands, Command{Name: []string{"whoami"}, Method: "GET", Path: "/records/one", Success: []int{200}})
	var identity bytes.Buffer
	if err := Run(context.Background(), one, []string{"whoami"}, &identity); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(config, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := run("get", "record", "one"); err == nil {
		t.Fatal("public configuration accepted")
	}
	os.Chmod(config, 0600)
	if err := os.Chmod(directory, 0777); err != nil {
		t.Fatal(err)
	}
	if _, err := run("get", "record", "one"); err == nil {
		t.Fatal("writable configuration directory accepted")
	}
	os.Chmod(directory, 0700)
	// A rotation is read by the next command. No token enters the config file.
	mode.Store(1)
	if err := os.WriteFile(token, []byte("second-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := run("get", "record", "one"); err != nil {
		t.Fatal(err)
	}
	mode.Store(0)
	os.WriteFile(token, []byte("first-token"), 0600)
	before := calls.Load()
	for _, args := range [][]string{
		{"create", "record", "--name", "sample", "--name", "duplicate"}, {"create", "record", "--unknown", "x"}, {"create", "record"},
		{"create", "record", "--name", "sample", "--count", "1.5"}, {"create", "record", "--name", "sample", "--tags", `"wrong"`},
		{"delete", "record", "one"}, {"get", "record", "../escape"}, {"get", "record", "one/else"}, {"get", "record", "one", "extra"},
		{"login", "--url", server.URL, "--token", "first-token"}, {"get", "record", "one", "--body", "file"},
		{"create", "record", "--name", string([]byte{255})}, {"create", "record", "--name", strings.Repeat("x", 4097)},
	} {
		if _, err := run(args...); err == nil {
			t.Fatalf("invalid args accepted: %q", args)
		}
	}
	body := filepath.Join(directory, "body.json")
	for _, data := range []string{`{"name":"a","name":"b"}`, `{"name":"\ud800"}`, `{"name":null}`, `{"name":"sample","admin":true}`, `[]`, `{"name":"sample"} {}`, `{"name":"sample","tags":["\ud800"]}`} {
		os.WriteFile(body, []byte(data), 0600)
		if _, err := run("create", "record", "--body", body); err == nil {
			t.Fatal("invalid body accepted")
		}
	}
	if calls.Load() != before {
		t.Fatal("invalid request reached server")
	}
	os.WriteFile(body, []byte(`{"name":"sample"}`), 0600)
	if _, err := run("create", "record", "--body", body); err != nil {
		t.Fatal(err)
	}
	for _, value := range []int32{2, 3, 4} {
		mode.Store(value)
		before = calls.Load()
		out, err := run("get", "record", "one")
		if err == nil || out != "" || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "first-token") {
			t.Fatal("response was not rejected safely")
		}
		if calls.Load() != before+1 {
			t.Fatal("request replayed")
		}
	}
	mode.Store(0)
	if err := os.Chmod(token, 0644); err != nil {
		t.Fatal(err)
	}
	before = calls.Load()
	if _, err := run("get", "record", "one"); err == nil {
		t.Fatal("public token file accepted")
	}
	if calls.Load() != before {
		t.Fatal("public credential reached server")
	}
	os.Chmod(token, 0600)
	if _, err := run("logout"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(config); !os.IsNotExist(err) {
		t.Fatal("logout retained config")
	}
	if _, err := os.Stat(token); err != nil {
		t.Fatal("logout removed externally owned token")
	}
	if err := os.Symlink(body, config); err != nil {
		t.Fatal(err)
	}
	if _, err := run(login...); err == nil {
		t.Fatal("configuration symlink accepted")
	}
	os.Remove(config)
	fifo := filepath.Join(directory, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := run("create", "record", "--body", fifo); err == nil {
		t.Fatal("FIFO body accepted")
	}
	if _, err := run("login", "--url", server.URL, "--token-file", fifo, "--ca-file", ca); err == nil {
		t.Fatal("FIFO credential accepted")
	}
	if _, err := run("login", "--url", strings.Replace(server.URL, "https:", "http:", 1), "--token-file", token, "--ca-file", ca); err == nil {
		t.Fatal("plaintext destination accepted")
	}
	if _, err := run("--help"); err != nil {
		t.Fatal(err)
	}
}
func TestDefinitionAndJSONLimits(t *testing.T) {
	app := definition()
	app.Commands = append(app.Commands, Command{Name: []string{"whoami"}, Method: "GET", Path: "/records/one", Success: []int{200}})
	if err := validate(app); err != nil {
		t.Fatal(err)
	}
	app.Commands = append(app.Commands, Command{Name: []string{"get"}, Method: "GET", Path: "/records/one", Success: []int{200}})
	if validate(app) == nil {
		t.Fatal("ambiguous command prefix accepted")
	}
	app = definition()
	app.Commands = append(app.Commands, app.Commands[0])
	if validate(app) == nil {
		t.Fatal("duplicate command accepted")
	}
	for _, path := range []string{"//other/records", "https://other/records", "/records/../escape", "/records/%2fescape", "/records?query=true"} {
		app = definition()
		app.Commands[0].Path = path
		if validate(app) == nil {
			t.Fatal("invalid route accepted")
		}
	}
	app = definition()
	app.Commands[0].Fields[0].Flag = "body"
	if validate(app) == nil {
		t.Fatal("reserved flag accepted")
	}
	for _, data := range []string{strings.Repeat("[", 33) + "0" + strings.Repeat("]", 33), `{"nested":{"a":1,"a":2}}`, string([]byte{255}), `"\ud800"`, "true false"} {
		if validJSON([]byte(data)) {
			t.Fatal("invalid JSON accepted")
		}
	}
	if !validJSON([]byte(`{"value":9007199254740993,"unicode":"\ud83d\ude00"}`)) {
		t.Fatal("valid JSON rejected")
	}
}
