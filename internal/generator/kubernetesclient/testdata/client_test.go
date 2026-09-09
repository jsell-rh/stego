package kubernetes

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func fixture(t *testing.T, handler http.HandlerFunc) (*Client, string) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	dir := t.TempDir()
	ca, file := filepath.Join(dir, "ca"), filepath.Join(dir, "token")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := New(Options{ServerURL: server.URL, CAFile: ca, TokenFile: file})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client, file
}
func TestWidgetCreateConvergeAndDelete(t *testing.T) {
	var mu sync.Mutex
	var current Object
	requests, writes := 0, 0
	owner := Owner{"example.com/widget-id": "widget-1"}
	want := Object{"apiVersion": "example.com/v1", "kind": "Widget", "metadata": Object{"name": "widget", "labels": Object{"example.com/widget-id": "widget-1"}}, "spec": Object{"replicas": 1}}
	original, _ := snapshot(want)
	client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests++
		if r.Header.Get("Authorization") != "Bearer one" {
			t.Error("missing token")
		}
		if r.Method == "GET" {
			if current == nil {
				w.WriteHeader(404)
			} else {
				_ = json.NewEncoder(w).Encode(current)
			}
			return
		}
		writes++
		var body Object
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid write")
			w.WriteHeader(500)
			return
		}
		switch r.Method {
		case "POST":
			if r.URL.Path != "/apis/example.com/v1/widgets" || current != nil {
				t.Error("invalid create")
			}
			current = body
			meta := current["metadata"].(map[string]any)
			meta["uid"] = "uid-1"
			meta["resourceVersion"] = "v1"
			current["status"] = Object{"ready": true}
			current["spec"].(map[string]any)["defaulted"] = "kept"
		case "PATCH":
			if String(body, "metadata", "uid") != "uid-1" || String(body, "metadata", "resourceVersion") != "v1" {
				t.Error("missing patch identity")
			}
			if r.Header.Get("Content-Type") != "application/merge-patch+json" {
				t.Error("incorrect patch type")
			}
			current["spec"] = body["spec"]
		case "DELETE":
			if String(body, "preconditions", "uid") != "uid-1" || String(body, "preconditions", "resourceVersion") != "v1" {
				t.Error("missing delete identity")
			}
			current = nil
			_ = json.NewEncoder(w).Encode(Object{"kind": "Status"})
			return
		default:
			t.Error("unexpected mutation")
		}
		_ = json.NewEncoder(w).Encode(current)
	})
	ctx := context.Background()
	collection := "/apis/example.com/v1/widgets"
	if _, err := client.Ensure(ctx, collection, want, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Ensure(ctx, collection, want, owner); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if writes != 1 {
		t.Error("unchanged object caused a write")
	}
	mu.Unlock()
	normalized, _ := snapshot(want)
	if !reflect.DeepEqual(original, normalized) {
		t.Fatal("caller desired object changed")
	}
	want["spec"].(Object)["replicas"] = 2
	if _, err := client.Ensure(ctx, collection, want, owner); err != nil {
		t.Fatal(err)
	}
	if gone, err := client.DeleteOwned(ctx, collection+"/widget", owner); gone || err != nil {
		t.Fatal("submitted delete counted as absence", gone, err)
	}
	if gone, err := client.DeleteOwned(ctx, collection+"/widget", owner); !gone || err != nil {
		t.Fatal("absent widget", gone, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if writes != 3 || requests != 8 {
		t.Fatal("unexpected request count", requests, writes)
	}
}
func TestOwnershipConflictAndInvalidDesiredState(t *testing.T) {
	cases := []struct {
		name     string
		metadata Object
		code     int
	}{
		{"foreign", Object{"uid": "u", "resourceVersion": "1", "labels": Object{"example.com/widget-id": "other"}}, 200},
		{"missing identity", Object{"labels": Object{"example.com/widget-id": "widget-1"}}, 200},
		{"conflict", Object{"uid": "u", "resourceVersion": "1", "labels": Object{"example.com/widget-id": "widget-1"}}, 409},
		{"denied", Object{"uid": "u", "resourceVersion": "1", "labels": Object{"example.com/widget-id": "widget-1"}}, 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			writes := 0
			client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					_ = json.NewEncoder(w).Encode(Object{"metadata": tc.metadata})
					return
				}
				mu.Lock()
				writes++
				mu.Unlock()
				w.WriteHeader(tc.code)
			})
			owner := Owner{"example.com/widget-id": "widget-1"}
			want := Object{"metadata": Object{"name": "widget", "labels": Object{"example.com/widget-id": "widget-1"}}, "spec": Object{"value": "new"}}
			if _, err := client.Ensure(context.Background(), "/widgets", want, owner); err == nil {
				t.Fatal("unsafe update accepted")
			}
			if gone, err := client.DeleteOwned(context.Background(), "/widgets/widget", owner); gone || err == nil {
				t.Fatal("unsafe deletion accepted", gone, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if tc.code == 200 && writes != 0 {
				t.Fatal("foreign or unidentified resource was mutated")
			}
		})
	}
	client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid desired object reached server")
		w.WriteHeader(500)
	})
	for _, want := range []Object{nil, {"metadata": Object{"name": "widget"}}, {"metadata": Object{"name": "../other", "labels": Object{"id": "one"}}}, {"metadata": Object{"name": "widget", "uid": "forged", "labels": Object{"id": "one"}}}, {"metadata": Object{"name": "widget", "labels": Object{"id": "one"}}, "unsupported": func() {}}} {
		if _, err := client.Ensure(context.Background(), "/widgets", want, Owner{"id": "one"}); err == nil {
			t.Fatal("invalid desired object accepted")
		}
	}
	if _, err := client.DeleteOwned(context.Background(), "/widgets/widget", Owner{}); err == nil {
		t.Fatal("unowned delete accepted")
	}
}
func TestCredentialRotationAndResponseErrors(t *testing.T) {
	var mu sync.Mutex
	expected := "one"
	client, file := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+expected {
			t.Error("old token used")
		}
		w.WriteHeader(404)
	})
	if _, code, err := client.Request(context.Background(), "GET", "/widgets/one", nil); code != 404 || err != nil {
		t.Fatal(code, err)
	}
	mu.Lock()
	expected = "two"
	mu.Unlock()
	if err := os.WriteFile(file, []byte("two"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.Request(context.Background(), "PATCH", "/widgets/one", Object{}); err == nil {
		t.Fatal("missing mutation succeeded")
	}
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.Request(context.Background(), "GET", "/widgets/one", nil); err == nil {
		t.Fatal("public credential file accepted")
	}
}

func TestIntegerPrecisionAndResponseValidation(t *testing.T) {
	const exact = int64(9007199254740993)
	client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/valid":
			_, _ = io.WriteString(w, `{"spec":{"counter":9007199254740993}}`)
		case "/null":
			_, _ = io.WriteString(w, `null`)
		case "/array":
			_, _ = io.WriteString(w, `[]`)
		case "/trailing":
			_, _ = io.WriteString(w, `{} {}`)
		}
	})
	actual, _, err := client.Request(context.Background(), "GET", "/valid", nil)
	if err != nil {
		t.Fatal(err)
	}
	number, ok := Nested(actual, "spec", "counter").(json.Number)
	if !ok || number.String() != "9007199254740993" {
		t.Fatal("response lost integer precision", number)
	}
	if !Contains(actual, Object{"spec": Object{"counter": exact}}) {
		t.Fatal("exact integer does not match")
	}
	if Contains(actual, Object{"spec": Object{"counter": exact - 1}}) {
		t.Fatal("different integers became equal")
	}
	for _, path := range []string{"/null", "/array", "/trailing"} {
		if _, _, err := client.Request(context.Background(), "GET", path, nil); err == nil {
			t.Fatalf("accepted invalid object at %s", path)
		}
	}
}
