package kubernetes

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
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

func TestWidgetObserveSnapshotReconnectAndExpiredHistory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var mu sync.Mutex
	lists, watches := 0, 0
	object := func(id, version string) Object {
		return Object{"metadata": Object{"uid": id, "resourceVersion": version}}
	}
	c, token := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		q := r.URL.Query()
		if q.Get("labelSelector") != "example.com/widget" {
			t.Error("lost selector")
		}
		if q.Get("watch") != "true" {
			lists++
			if q.Get("limit") != "100" {
				t.Error("unbounded list")
			}
			switch lists {
			case 1:
				json.NewEncoder(w).Encode(Object{"metadata": Object{"resourceVersion": "100", "continue": "page2"}, "items": []Object{object("one", "90")}})
			case 2:
				if q.Get("continue") != "page2" {
					t.Error("lost list cursor")
				}
				json.NewEncoder(w).Encode(Object{"metadata": Object{"resourceVersion": "100"}, "items": []Object{object("two", "95")}})
			case 3:
				json.NewEncoder(w).Encode(Object{"metadata": Object{"resourceVersion": "200"}, "items": []Object{object("two", "180")}})
			default:
				t.Error("unexpected list")
			}
			return
		}
		watches++
		if q.Get("limit") != "" || q.Get("continue") != "" {
			t.Error("watch inherited list paging")
		}
		switch watches {
		case 1:
			if q.Get("resourceVersion") != "100" {
				t.Error("watch lost snapshot version")
			}
			json.NewEncoder(w).Encode(Object{"type": "MODIFIED", "object": object("one", "101")})
			json.NewEncoder(w).Encode(Object{"type": "BOOKMARK", "object": Object{"metadata": Object{"resourceVersion": "102"}}})
		case 2:
			if q.Get("resourceVersion") != "102" || r.Header.Get("Authorization") != "Bearer two" {
				t.Error("watch lost cursor or token rotation")
			}
			json.NewEncoder(w).Encode(Object{"type": "ERROR", "object": Object{"code": 410}})
		case 3:
			if q.Get("resourceVersion") != "200" {
				t.Error("expired history did not relist")
			}
			json.NewEncoder(w).Encode(Object{"type": "DELETED", "object": object("two", "201")})
		default:
			t.Error("unexpected watch")
		}
	})
	kinds := []string{}
	cache := map[string]bool{}
	err := c.Observe(ctx, Collection{Path: "/apis/example.com/v1/widgets", LabelSelector: "example.com/widget"}, func(change Change) error {
		kinds = append(kinds, change.Type)
		switch change.Type {
		case "RESET":
			cache = map[string]bool{}
		case "REPLACE":
			for _, o := range change.Objects {
				cache[String(o, "metadata", "uid")] = true
			}
		case "MODIFIED":
			if err := os.WriteFile(token, []byte("two"), 0600); err != nil {
				return err
			}
		case "DELETED":
			delete(cache, String(change.Object, "metadata", "uid"))
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || len(cache) != 0 || !reflect.DeepEqual(kinds, []string{"RESET", "REPLACE", "MODIFIED", "RESET", "REPLACE", "DELETED"}) {
		t.Fatal(err, kinds, cache)
	}
	mu.Lock()
	defer mu.Unlock()
	if lists != 3 || watches != 3 {
		t.Fatal(lists, watches)
	}
}

func TestWidgetObserveRejectsPartialAndDeniedSnapshots(t *testing.T) {
	for _, mode := range []string{"version", "duplicate", "cursor", "denied", "watch-denied"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			replaced := false
			c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if mode == "denied" || (mode == "watch-denied" && r.URL.Query().Get("watch") == "true") {
					w.WriteHeader(403)
					return
				}
				version, uid, cursor := "10", "one", "next"
				if calls > 1 {
					uid = "two"
					cursor = ""
					if mode == "version" {
						version = "11"
					}
					if mode == "duplicate" {
						uid = "one"
					}
					if mode == "cursor" {
						cursor = "next"
					}
				}
				if mode == "watch-denied" {
					cursor = ""
				}
				json.NewEncoder(w).Encode(Object{"metadata": Object{"resourceVersion": version, "continue": cursor}, "items": []Object{{"metadata": Object{"uid": uid, "resourceVersion": "1"}}}})
			})
			rv := ""
			err := c.observe(context.Background(), Collection{Path: "/api/v1/widgets"}, &rv, func(ch Change) error {
				if ch.Type == "REPLACE" {
					replaced = true
				}
				return nil
			})
			if err == nil || (mode != "watch-denied" && replaced) {
				t.Fatal("accepted incomplete or denied snapshot", err, replaced)
			}
			if (mode == "denied" || mode == "watch-denied") && !errors.Is(err, ErrWatchAccess) {
				t.Fatal(err)
			}
		})
	}
}
func TestWidgetObserveCallbackFailureAndInvalidPaths(t *testing.T) {
	c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid collection reached server") })
	for _, path := range []string{"https://other.test/api/v1/widgets", "/api/../widgets", "/api/%2e%2e/widgets", "/api/v1/widgets?watch=true", "/api//widgets"} {
		if err := c.Observe(context.Background(), Collection{Path: path}, func(Change) error { return nil }); err == nil {
			t.Fatal(path)
		}
	}
	stopped := errors.New("application stopped")
	if err := c.Observe(context.Background(), Collection{Path: "/api/v1/widgets"}, func(Change) error { return stopped }); !errors.Is(err, stopped) {
		t.Fatal(err)
	}
}

func TestRateLimitMetadataAndRetryDelay(t *testing.T) {
	client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(429)
		_, _ = w.Write([]byte("private error"))
	})
	_, code, err := client.Request(context.Background(), "GET", "/api/v1/pods", nil)
	var api *APIError
	if code != 429 || !errors.As(err, &api) || api.RetryAfter != 2*time.Minute || api.StatusCode != 429 || api.Error() != "Kubernetes GET failed with status 429" {
		t.Fatal(code, err)
	}
	for _, test := range []struct {
		input string
		want  time.Duration
	}{{"-1", 0}, {"invalid", 0}, {"999999", time.Hour}, {"1", time.Second}} {
		if got := retryAfter(test.input); got != test.want {
			t.Fatal(test.input, got)
		}
	}
	if got := retryAfter(time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)); got < 28*time.Second || got > 30*time.Second {
		t.Fatal(got)
	}
}

func TestWidgetObserveEmptyPagesAndSnapshotLimits(t *testing.T) {
	for _, mode := range []string{"empty-page", "page-limit", "object-limit"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				items := []Object{}
				cursor := fmt.Sprint(calls)
				if mode == "empty-page" && calls == 2 {
					cursor = ""
					items = append(items, Object{"metadata": Object{"uid": "one", "resourceVersion": "1"}})
				}
				if mode == "object-limit" {
					for n := 0; n < 100; n++ {
						items = append(items, Object{"metadata": Object{"uid": fmt.Sprintf("%d-%d", calls, n), "resourceVersion": "1"}})
					}
				}
				_ = json.NewEncoder(w).Encode(Object{"metadata": Object{"resourceVersion": "10", "continue": cursor}, "items": items})
			})
			rv := ""
			replaced := false
			stop := errors.New("stop after snapshot")
			err := c.observe(context.Background(), Collection{Path: "/api/v1/widgets"}, &rv, func(ch Change) error {
				if ch.Type == "REPLACE" {
					replaced = true
					if len(ch.Objects) != 1 {
						t.Error("lost object after empty page")
					}
					return stop
				}
				return nil
			})
			if mode == "empty-page" {
				if !errors.Is(err, stop) || !replaced || calls != 2 {
					t.Fatal(err, replaced, calls)
				}
			} else if err == nil || replaced || rv != "" {
				t.Fatal("partial or oversized snapshot was accepted", err, replaced, rv)
			}
			if calls > MaxObservedPages {
				t.Fatal("page bound was exceeded", calls)
			}
		})
	}
}

func TestFailureSummaryOmitsPrivateDetails(t *testing.T) {
	for _, err := range []error{errors.New("private-token"), &APIError{Method: "private-token", StatusCode: 403}, &APIError{Method: "DELETE", StatusCode: 999}} {
		if got := FailureSummary(err); got != "Kubernetes operation failed" {
			t.Fatal("unsafe failure summary", got)
		}
	}
	if got := FailureSummary(fmt.Errorf("private-token: %w", &APIError{Method: "DELETE", StatusCode: 403})); got != "Kubernetes DELETE failed with status 403" {
		t.Fatal("safe failure fields were lost", got)
	}
	if FailureSummary(nil) != "" {
		t.Fatal("nil error reported as failure")
	}
}

func TestRequiredAPIsBeforeWidgetEffects(t *testing.T) {
	requirement := APIResource{Name: "widgets", Kind: "Widget", Namespaced: true, Verbs: []string{"get", "create", "patch", "delete"}}
	for _, mode := range []string{"valid", "core", "missing", "partial", "wrong-version", "wrong-kind", "wrong-scope", "missing-scope", "verbs", "duplicate", "malformed", "denied", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			groupVersion := "example.com/v1"
			path := "/apis/" + groupVersion
			if mode == "core" {
				groupVersion = "v1"
				path = "/api/v1"
			}
			calls := 0
			client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "GET" || r.URL.Path != path || r.Header.Get("Authorization") != "Bearer one" {
					t.Error("incorrect discovery request")
				}
				switch mode {
				case "missing":
					w.WriteHeader(404)
					return
				case "denied":
					w.WriteHeader(403)
					return
				case "unavailable":
					w.WriteHeader(503)
					return
				}
				widget := Object{"name": "widgets", "kind": "Widget", "namespaced": true, "verbs": []string{"get", "create", "patch", "delete", "list"}}
				result := Object{"kind": "APIResourceList", "groupVersion": groupVersion, "resources": []Object{widget}}
				switch mode {
				case "partial":
					result["resources"] = []Object{{"name": "gadgets"}}
				case "wrong-version":
					result["groupVersion"] = "example.com/v2"
				case "wrong-kind":
					widget["kind"] = "Gadget"
				case "wrong-scope":
					widget["namespaced"] = false
				case "missing-scope":
					delete(widget, "namespaced")
				case "verbs":
					widget["verbs"] = []string{"get", "create"}
				case "duplicate":
					result["resources"] = []Object{widget, widget}
				case "malformed":
					result["resources"] = []any{true}
				}
				_ = json.NewEncoder(w).Encode(result)
			})
			err := client.RequireResources(context.Background(), groupVersion, requirement)
			if calls != 1 {
				t.Fatal("discovery made more than one request", calls)
			}
			switch mode {
			case "valid", "core":
				if err != nil {
					t.Fatal(err)
				}
			case "denied", "unavailable":
				var api *APIError
				if !errors.As(err, &api) {
					t.Fatal("HTTP failure was lost", err)
				}
			default:
				if !errors.Is(err, ErrDiscovery) {
					t.Fatal("invalid discovery was accepted", err)
				}
			}
		})
	}
}
func TestDiscoveryRejectsInvalidRequirementsBeforeRequests(t *testing.T) {
	client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid requirements reached API")
		w.WriteHeader(500)
	})
	valid := APIResource{Name: "widgets", Kind: "Widget", Namespaced: true, Verbs: []string{"get"}}
	for _, groupVersion := range []string{"", "../v1", "example.com/v1?x=1", "example.com/v1/extra", "https://api/v1", "example..com/v1", "example.com/"} {
		if !errors.Is(client.RequireResources(context.Background(), groupVersion, valid), ErrDiscovery) {
			t.Fatal("invalid group version accepted", groupVersion)
		}
	}
	for _, requirements := range [][]APIResource{nil, {valid, valid}, {{Name: "widgets/status", Kind: "Widget", Verbs: []string{"get"}}}, {{Name: "widgets", Kind: "Widget"}}, {{Name: "widgets", Kind: "Widget", Verbs: []string{"get", "get"}}}, {{Name: "widgets", Kind: "Widget", Verbs: []string{"execute"}}}, make([]APIResource, 33)} {
		if !errors.Is(client.RequireResources(context.Background(), "example.com/v1", requirements...), ErrDiscovery) {
			t.Fatal("invalid requirement accepted")
		}
	}
}
func TestDiscoveryRechecksSupportAndToken(t *testing.T) {
	allowed := true
	expected := "one"
	client, file := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+expected {
			t.Error("old discovery token")
		}
		if !allowed {
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(Object{"kind": "APIResourceList", "groupVersion": "example.com/v1", "resources": []Object{{"name": "widgets", "kind": "Widget", "namespaced": false, "verbs": []string{"get"}}}})
	})
	resource := APIResource{Name: "widgets", Kind: "Widget", Verbs: []string{"get"}}
	if err := client.RequireResources(context.Background(), "example.com/v1", resource); err != nil {
		t.Fatal(err)
	}
	expected = "two"
	if err := os.WriteFile(file, []byte(expected), 0600); err != nil {
		t.Fatal(err)
	}
	allowed = false
	if err := client.RequireResources(context.Background(), "example.com/v1", resource); !errors.Is(err, ErrDiscovery) {
		t.Fatal("removed API support remained cached", err)
	}
}

func TestDiscoveryAllowsResourceNamesWithHyphens(t *testing.T) {
	client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Object{"kind": "APIResourceList", "groupVersion": "example.com/v1", "resources": []Object{{"name": "widget-sets", "kind": "WidgetSet", "namespaced": true, "verbs": []string{"get"}}}})
	})
	if err := client.RequireResources(context.Background(), "example.com/v1", APIResource{Name: "widget-sets", Kind: "WidgetSet", Namespaced: true, Verbs: []string{"get"}}); err != nil {
		t.Fatal(err)
	}
}

func TestPatchOwnedUsesTheExactObservation(t *testing.T) {
	calls := 0
	old := Object{"metadata": Object{"name": "sample", "uid": "uid", "resourceVersion": "4", "labels": Object{"owner": "widget"}}, "spec": Object{"items": []string{"first"}}}
	changes := Object{"spec": Object{"items": []string{"first", "second"}}}
	beforeOld, _ := json.Marshal(old)
	beforeChanges, _ := json.Marshal(changes)
	client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "PATCH" || r.URL.Path != "/apis/example.com/v1/widgets/sample" {
			t.Error("computed patch performed a new read", r.Method, r.URL.Path)
		}
		var patch Object
		_ = json.NewDecoder(r.Body).Decode(&patch)
		if String(patch, "metadata", "uid") != "uid" || String(patch, "metadata", "resourceVersion") != "4" {
			t.Error("patch lost its original preconditions")
		}
		w.WriteHeader(http.StatusConflict)
	})
	_, err := client.PatchOwned(context.Background(), "/apis/example.com/v1/widgets/sample", old, changes, Owner{"owner": "widget"})
	var failure *APIError
	if !errors.As(err, &failure) || failure.StatusCode != 409 || calls != 1 {
		t.Fatal("stale change was retried or accepted", err, calls)
	}
	afterOld, _ := json.Marshal(old)
	afterChanges, _ := json.Marshal(changes)
	if string(beforeOld) != string(afterOld) || string(beforeChanges) != string(afterChanges) {
		t.Fatal("patch changed caller input")
	}
	for _, patch := range []Object{{}, {"metadata": Object{}}, {"status": nil}, {"kind": "Other"}, {"apiVersion": "v1"}} {
		if _, err := client.PatchOwned(context.Background(), "/apis/example.com/v1/widgets/sample", old, patch, Owner{"owner": "widget"}); err == nil {
			t.Fatal("accepted system field or empty patch")
		}
	}
	for _, path := range []string{"/apis/example.com/v1/widgets/other", "/apis/example.com/v1/widgets/sample?watch=true"} {
		if _, err := client.PatchOwned(context.Background(), path, old, changes, Owner{"owner": "widget"}); err == nil {
			t.Fatal("accepted wrong observed path")
		}
	}
	if _, err := client.PatchOwned(context.Background(), "/apis/example.com/v1/widgets/sample", old, changes, Owner{"owner": "other"}); err == nil {
		t.Fatal("accepted foreign ownership")
	}
	if calls != 1 {
		t.Fatal("invalid changes reached the network")
	}
}

func TestPatchOwnedRejectsMissingAndTerminatingResources(t *testing.T) {
	calls := 0
	client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(404) })
	old := Object{"metadata": Object{"name": "sample", "uid": "uid", "resourceVersion": "4", "labels": Object{"owner": "widget"}}}
	if _, err := client.PatchOwned(context.Background(), "/api/v1/widgets/sample", old, Object{"data": Object{"key": "value"}}, Owner{"owner": "widget"}); err == nil {
		t.Fatal("absent patch succeeded")
	}
	old["metadata"].(Object)["deletionTimestamp"] = "2026-09-10T00:00:00Z"
	if _, err := client.PatchOwned(context.Background(), "/api/v1/widgets/sample", old, Object{"data": Object{"key": "value"}}, Owner{"owner": "widget"}); err == nil {
		t.Fatal("terminating patch succeeded")
	}
	if calls != 1 {
		t.Fatal("terminating resource reached the network")
	}
}

func TestWritesRequireStrictFieldValidation(t *testing.T) {
	calls := 0
	client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query()
		if r.URL.Path != "/apis/example.com/v1/widgets" || len(q["fieldValidation"]) != 1 || q.Get("fieldValidation") != "Strict" || q.Get("dryRun") != "All" {
			t.Error("write lost strict validation or caller options", r.URL)
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"private rejected value"}`))
	})
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch} {
		_, code, err := client.Request(context.Background(), method, "/apis/example.com/v1/widgets?dryRun=All&fieldValidation=Ignore&fieldValidation=Warn", Object{"spec": Object{"unknown": true}})
		var failure *APIError
		if code != 400 || !errors.As(err, &failure) || failure.StatusCode != 400 || failure.Method != method || err.Error() != fmt.Sprintf("Kubernetes %s failed with status 400", method) {
			t.Fatal("schema rejection was lost or exposed response data", code, err)
		}
	}
	for _, query := range []string{"bad=%xx", "bad=one;two"} {
		if _, _, err := client.Request(context.Background(), http.MethodPost, "/api/v1/namespaces?"+query, Object{}); err == nil {
			t.Fatal("accepted malformed write query")
		}
	}
	if calls != 3 {
		t.Fatal("invalid query performed I/O or a rejected write was retried", calls)
	}
}
