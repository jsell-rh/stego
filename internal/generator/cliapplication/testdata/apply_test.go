package command

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func applyDefinition() Application {
	app := definition()
	fields := []Field{{Flag: "name", Key: "name", Type: "string", Required: true}, {Flag: "count", Key: "count", Type: "integer"}, {Flag: "tags", Key: "tags", Type: "string-list", Nullable: true}}
	patch := append([]Field(nil), fields...)
	patch[0].Required = false
	for _, kind := range []string{"Record", "Widget"} {
		app.Resources = append(app.Resources, ApplyResource{Kind: kind, APIVersion: "example/v1", Path: "/" + strings.ToLower(kind) + "s", CreateFields: fields, PatchFields: patch})
	}
	return app
}

type applyFixture struct {
	mu            sync.Mutex
	calls, writes int
	mode          string
	rows          map[string]map[string]any
	order         []string
	directory     string
}

func applySetup(t *testing.T) *applyFixture {
	t.Helper()
	f := &applyFixture{directory: t.TempDir(), rows: map[string]map[string]any{}}
	t.Setenv("TEST_CLI_CONFIG", filepath.Join(f.directory, "config.json"))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		if r.Header.Get("Authorization") != "Bearer test-apply-token" {
			t.Error("apply token changed")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			if f.mode == "lookup-failure" && strings.Contains(r.URL.RawQuery, "second") {
				w.WriteHeader(403)
				fmt.Fprint(w, `{"private":"do-not-print"}`)
				return
			}
			if f.mode == "invalid-list" {
				fmt.Fprint(w, `{"items":null,"total":0}`)
				return
			}
			if r.URL.RawQuery == "" {
				if row, ok := f.rows[r.URL.Path]; ok {
					json.NewEncoder(w).Encode(row)
				} else {
					w.WriteHeader(404)
				}
				return
			}
			var items []map[string]any
			items = make([]map[string]any, 0)
			for path, row := range f.rows {
				if strings.HasPrefix(path, r.URL.Path+"/") && r.URL.Query().Get("search") == "name = '"+strings.ReplaceAll(row["name"].(string), "'", "''")+"'" {
					items = append(items, row)
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"items": items, "total": len(items)})
			return
		}
		f.writes++
		var body map[string]any
		decoder := json.NewDecoder(r.Body)
		decoder.UseNumber()
		if decoder.Decode(&body) != nil {
			t.Error("invalid apply request")
		}
		if body["name"] == "second" && f.mode == "write-failure" {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"private":"do-not-print"}`)
			return
		}
		path, id := r.URL.Path, ""
		if r.Method == "POST" {
			id = fmt.Sprintf("id-%d", f.writes)
			path += "/" + id
		} else {
			id = path[strings.LastIndex(path, "/")+1:]
		}
		body["id"] = id
		f.rows[path] = body
		f.order = append(f.order, body["name"].(string))
		if body["name"] == "second" && f.mode == "unknown-write" {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"private":"do-not-print"}`)
			return
		}
		if f.mode == "wrong-write-id" {
			body = map[string]any{"id": "wrong", "name": "wrong"}
		}
		if r.Method == "POST" {
			w.WriteHeader(201)
		}
		json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(server.Close)
	ca, token := filepath.Join(f.directory, "ca.pem"), filepath.Join(f.directory, "token")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token, []byte("test-apply-token"), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := Run(context.Background(), applyDefinition(), []string{"login", "--url", server.URL, "--token-file", token, "--ca-file", ca}, &output); err != nil {
		t.Fatal(err)
	}
	return f
}
func applyFile(t *testing.T, path, data string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func runApplyTest(t *testing.T, app Application, args ...string) ([]ApplyResult, string, error) {
	t.Helper()
	var output bytes.Buffer
	err := Run(context.Background(), app, args, &output)
	var results []ApplyResult
	if output.Len() > 0 && json.Unmarshal(output.Bytes(), &results) != nil {
		t.Fatalf("invalid apply result: %s", output.Bytes())
	}
	return results, output.String(), err
}
func manifest(kind, name, spec string) string {
	return fmt.Sprintf("apiVersion: example/v1\nkind: %s\nmetadata:\n  name: %q\nspec: %s\n", kind, name, spec)
}

func TestApplyCreatePatchAndDryRun(t *testing.T) {
	f := applySetup(t)
	app := applyDefinition()
	path := applyFile(t, filepath.Join(f.directory, "input.yaml"), manifest("Record", "a' OR name = 'b", `{"count":9007199254740993,"tags":["a","b"]}`)+"---\n"+manifest("Widget", "other", `{"count":0}`))
	// Dry run needs neither credentials nor a network connection.
	t.Setenv("TEST_CLI_CONFIG", filepath.Join(f.directory, "absent.json"))
	result, _, err := runApplyTest(t, app, "apply", "-f", path, "--dry-run", "-o", "json")
	if err != nil || len(result) != 2 || result[0].Status != "validated" || f.calls != 0 {
		t.Fatal("dry run contacted API or credentials", err)
	}
	t.Setenv("TEST_CLI_CONFIG", filepath.Join(f.directory, "config.json"))
	first, _, err := runApplyTest(t, app, "apply", "-f", path, "-o", "json")
	if err != nil || len(first) != 2 || first[0].Status != "created" || f.writes != 2 {
		t.Fatal("apply creation", first, err)
	}
	f.mu.Lock()
	number := f.rows["/records/"+first[0].ID]["count"].(json.Number).String()
	f.mu.Unlock()
	if number != "9007199254740993" {
		t.Fatal("apply lost integer precision")
	}
	second, _, err := runApplyTest(t, app, "apply", "--filename="+path, "--output=json")
	if err != nil || second[0].ID != first[0].ID || second[0].Status != "configured" || f.writes != 4 || len(f.rows) != 2 {
		t.Fatal("repeat apply did not patch", second, err)
	}
}
func TestApplyRejectsAllInputBeforeRequests(t *testing.T) {
	f := applySetup(t)
	app := applyDefinition()
	good := manifest("Record", "first", "{}")
	invalid := []string{
		"kind: Unknown\nmetadata: {name: x}", "kind: Record\nmetadata: {}", "kind: Record\nmetadata: {name: x}\nspec: {count: '1'}",
		"kind: Record\nmetadata: {name: x}\nspec: {count: 9223372036854775808}", "kind: Record\nmetadata: {name: x}\nspec: {unknown: 1}",
		"kind: Record\nmetadata: {name: x}\nspec: {name: x}", "kind: Record\nmetadata: {name: x, id: '../escape'}", "kind: Record\nmetadata: {name: x}\nextra: true",
		"kind: Record\nkind: Widget\nmetadata: {name: x}", "kind: Record\nmetadata: &a {name: x}\nspec: *a", "kind: Record\nmetadata: {name: !private secret}",
		"kind: Record\napiVersion: wrong/v1\nmetadata: {name: x}", "kind: Record\nmetadata: {name: x}\nspec: {count: 0x12}", "kind: Record\nmetadata: {name: x}\nspec: {count: .nan}",
		"kind: Record\nmetadata: {name: x}\nspec: {tags: {nested: value}}", `{"kind":"Record","metadata":{"name":"x"},"spec":{"tags":["\ud800"]}}`,
		"kind: Record\nmetadata: {name: x}\nspec: {tags: [a], tags: [b]}", good + "---\n" + good,
	}
	for i, bad := range invalid {
		path := applyFile(t, filepath.Join(f.directory, "invalid.yaml"), good+"---\n"+bad)
		if _, _, err := runApplyTest(t, app, "apply", "-f", path, "-o", "json"); err == nil {
			t.Fatalf("accepted invalid document %d", i)
		}
	}
	if f.calls != 0 || f.writes != 0 {
		t.Fatal("invalid document reached API")
	}
}
func TestApplyPreflightAndPartialResults(t *testing.T) {
	for _, mode := range []string{"lookup-failure", "invalid-list", "write-failure", "unknown-write"} {
		t.Run(mode, func(t *testing.T) {
			f := applySetup(t)
			f.mode = mode
			path := applyFile(t, filepath.Join(f.directory, "input.yaml"), manifest("Record", "first", "{}")+"---\n"+manifest("Record", "second", "{}")+"---\n"+manifest("Record", "third", "{}"))
			results, output, err := runApplyTest(t, applyDefinition(), "apply", "-f", path, "-o", "json")
			if err == nil || strings.Contains(output, "do-not-print") || strings.Contains(err.Error(), "do-not-print") {
				t.Fatal("apply failure or private response handling", err)
			}
			if mode == "lookup-failure" || mode == "invalid-list" {
				if f.writes != 0 {
					t.Fatal("lookup failure allowed writes")
				}
				return
			}
			want := "failed"
			if mode == "unknown-write" {
				want = "unknown"
			}
			if f.writes != 2 || len(results) != 3 || results[0].Status != "created" || results[1].Status != want || results[2].Status != "not_attempted" {
				t.Fatal("partial results are wrong", results, f.writes)
			}
		})
	}
	t.Run("required-create-field", func(t *testing.T) {
		f := applySetup(t)
		app := applyDefinition()
		app.Resources[0].CreateFields[1].Required = true
		path := applyFile(t, filepath.Join(f.directory, "input.yaml"), manifest("Record", "first", "{count: 1}")+"---\n"+manifest("Record", "second", "{}"))
		if _, _, err := runApplyTest(t, app, "apply", "-f", path, "-o", "json"); err == nil || f.writes != 0 {
			t.Fatal("late required field allowed writes", err)
		}
	})
}
func TestApplyAmbiguityAndExplicitIDs(t *testing.T) {
	f := applySetup(t)
	for _, id := range []string{"one", "two"} {
		f.rows["/records/"+id] = map[string]any{"id": id, "name": "same"}
	}
	path := applyFile(t, filepath.Join(f.directory, "input.yaml"), manifest("Record", "same", "{}"))
	if _, _, err := runApplyTest(t, applyDefinition(), "apply", "-f", path, "-o", "json"); err == nil || f.writes != 0 {
		t.Fatal("ambiguous name was applied")
	}
	applyFile(t, path, "kind: Record\nmetadata: {name: same, id: one}\nspec: {count: 2}\n---\nkind: Record\nmetadata: {name: same, id: two}\nspec: {count: 3}\n")
	result, _, err := runApplyTest(t, applyDefinition(), "apply", "-f", path, "-o", "json")
	if err != nil || len(result) != 2 || result[0].ID != "one" || result[1].ID != "two" {
		t.Fatal("explicit IDs did not resolve ambiguity", result, err)
	}
	applyFile(t, path, "kind: Record\nmetadata: {name: same, id: one}\n---\nkind: Record\nmetadata: {name: changed, id: one}\n")
	if _, _, err := runApplyTest(t, applyDefinition(), "apply", "-f", path, "-o", "json"); err == nil {
		t.Fatal("duplicate explicit target accepted")
	}
}
func TestApplyDirectoriesAndInputBounds(t *testing.T) {
	f := applySetup(t)
	directory := filepath.Join(f.directory, "documents")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(directory, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	applyFile(t, filepath.Join(directory, "a.yaml"), manifest("Record", "first", "{}"))
	applyFile(t, filepath.Join(directory, "nested", "b.json"), `{"kind":"Record","metadata":{"name":"second"}}`)
	applyFile(t, filepath.Join(directory, "z.yml"), manifest("Record", "third", "{}"))
	if _, _, err := runApplyTest(t, applyDefinition(), "apply", "-f", directory, "-o", "json"); err != nil || !slices.Equal(f.order, []string{"first", "second", "third"}) {
		t.Fatal("directory order", f.order, err)
	}
	before := f.calls
	if err := os.Symlink(filepath.Join(directory, "a.yaml"), filepath.Join(directory, "link.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runApplyTest(t, applyDefinition(), "apply", "-f", directory, "-o", "json"); err == nil || f.calls != before {
		t.Fatal("directory link reached API")
	}
	if err := os.Remove(filepath.Join(directory, "link.yaml")); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(directory, "fifo.yaml")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runApplyTest(t, applyDefinition(), "apply", "-f", fifo, "-o", "json"); err == nil {
		t.Fatal("FIFO accepted as a regular input")
	}
	if _, err := decodeApply([]byte(strings.Repeat("---\nkind: Record\nmetadata: {name: x}\n", 129))); err == nil {
		t.Fatal("document bound ignored")
	}
	if _, err := decodeApply([]byte("kind: Record\nmetadata: {name: x}\nspec: " + strings.Repeat("[", 40) + "0" + strings.Repeat("]", 40))); err == nil {
		t.Fatal("depth bound ignored")
	}
}
func TestApplyStdinAndCancellation(t *testing.T) {
	original := os.Stdin
	defer func() { os.Stdin = original }()
	file, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(manifest("Record", "stdin", "{}")); err != nil {
		t.Fatal(err)
	}
	file.Seek(0, 0)
	os.Stdin = file
	result, _, err := runApplyTest(t, applyDefinition(), "apply", "-f", "-", "--dry-run", "-o", "json")
	if err != nil || len(result) != 1 || result[0].Name != "stdin" {
		t.Fatal("stdin apply", err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	defer reader.Close()
	os.Stdin = reader
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		var out bytes.Buffer
		done <- Run(ctx, applyDefinition(), []string{"apply", "-f", "-", "--dry-run"}, &out)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled stdin succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled stdin remained blocked")
	}
}

func TestApplyDefinitionAndOutputFailures(t *testing.T) {
	for _, change := range []func(*Application){
		func(a *Application) { a.Resources[0].Path = "//outside.invalid/records" },
		func(a *Application) { a.Resources[0].Path = "/records/{id}" },
		func(a *Application) { a.Resources[1].Kind = a.Resources[0].Kind },
		func(a *Application) { a.Resources[0].APIVersion = "bad\xff" },
		func(a *Application) {
			a.Resources[0].CreateFields = []Field{{Flag: "value", Key: "value", Type: "string"}}
		},
		func(a *Application) {
			a.Resources[0].CreateFields = append(a.Resources[0].CreateFields, Field{Flag: "id", Key: "id", Type: "string"})
		},
		func(a *Application) {
			a.Resources[0].PatchFields = append([]Field(nil), a.Resources[0].PatchFields...)
			a.Resources[0].PatchFields[1].Type = "string"
		},
	} {
		a := applyDefinition()
		change(&a)
		if validate(a) == nil {
			t.Fatal("invalid apply definition accepted")
		}
	}
	f := applySetup(t)
	path := applyFile(t, filepath.Join(f.directory, "input.yaml"), manifest("Record", "first", "{}"))
	for _, args := range [][]string{{"-f", path, "--filename", path}, {"-f", path, "--dry-run=maybe"}, {"-f", path, "--output", "xml"}, {"-f", path, "-k", "other"}, {"-k", "other"}} {
		if _, _, err := runApplyTest(t, applyDefinition(), append([]string{"apply"}, args...)...); err == nil || f.calls != 0 {
			t.Fatal("invalid apply options reached API")
		}
	}
	if _, _, err := runApplyTest(t, applyDefinition(), "apply", "-f", path, "--output-file", path, "-o", "json"); err == nil || f.calls != 0 {
		t.Fatal("output overwrote apply input")
	}
	f.mode = "wrong-write-id"
	results, _, err := runApplyTest(t, applyDefinition(), "apply", "-f", path, "-o", "json")
	if err == nil || len(results) != 1 || results[0].Status != "unknown" || f.writes != 1 {
		t.Fatal("invalid write response allowed success or retry", results, err)
	}
}
