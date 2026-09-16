package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func secretSetFixture() ([]string, Owner, []Object) {
	names := []string{"runtime", "files"}
	owner := Owner{"example.test/owner": "widget"}
	secrets := make([]Object, len(names))
	for i, name := range names {
		secrets[i] = Object{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "metadata": Object{"namespace": "service", "name": name, "uid": "uid", "resourceVersion": "1", "labels": Object{"example.test/owner": "widget"}}, "data": Object{"value": "YWJj"}}
	}
	return names, owner, secrets
}
func TestOpaqueSecretSetTracksDataAndIdentity(t *testing.T) {
	names, owner, secrets := secretSetFixture()
	first, err := OpaqueSecretSetDigest("service", names, owner, secrets)
	if err != nil || len(first) != 64 {
		t.Fatal("valid Secrets were rejected", err)
	}
	secrets[0]["metadata"].(Object)["resourceVersion"] = "2"
	same, err := OpaqueSecretSetDigest("service", names, owner, secrets)
	if err != nil || same != first {
		t.Fatal("metadata changed the digest", err)
	}
	secrets[0]["data"].(Object)["value"] = "ZGVm"
	changed, err := OpaqueSecretSetDigest("service", names, owner, secrets)
	if err != nil || changed == first {
		t.Fatal("data did not change the digest", err)
	}
	names[0] = "replacement"
	secrets[0]["metadata"].(Object)["name"] = "replacement"
	renamed, err := OpaqueSecretSetDigest("service", names, owner, secrets)
	if err != nil || renamed == changed {
		t.Fatal("name did not change the digest", err)
	}
	// API decoding and typed Object construction must produce the same hash.
	secrets[0]["data"] = map[string]any{"value": "ZGVm"}
	same, err = OpaqueSecretSetDigest("service", names, owner, secrets)
	if err != nil || same != renamed {
		t.Fatal("map representation changed the digest", err)
	}
}
func TestOpaqueSecretSetRejectsInvalidInput(t *testing.T) {
	cases := map[string]func([]string, Owner, []Object){
		"owner":               func(n []string, o Owner, s []Object) { o["example.test/owner"] = "other" },
		"uid":                 func(n []string, o Owner, s []Object) { delete(s[0]["metadata"].(Object), "uid") },
		"revision":            func(n []string, o Owner, s []Object) { delete(s[0]["metadata"].(Object), "resourceVersion") },
		"namespace":           func(n []string, o Owner, s []Object) { s[0]["metadata"].(Object)["namespace"] = "foreign" },
		"name":                func(n []string, o Owner, s []Object) { s[0]["metadata"].(Object)["name"] = "foreign" },
		"deleted":             func(n []string, o Owner, s []Object) { s[0]["metadata"].(Object)["deletionTimestamp"] = "now" },
		"invalid deletion":    func(n []string, o Owner, s []Object) { s[0]["metadata"].(Object)["deletionTimestamp"] = 1 },
		"kind":                func(n []string, o Owner, s []Object) { s[0]["kind"] = "ConfigMap" },
		"version":             func(n []string, o Owner, s []Object) { s[0]["apiVersion"] = "v2" },
		"type":                func(n []string, o Owner, s []Object) { s[0]["type"] = "kubernetes.io/tls" },
		"empty":               func(n []string, o Owner, s []Object) { s[0]["data"] = Object{} },
		"key":                 func(n []string, o Owner, s []Object) { s[0]["data"] = Object{"../key": "YWJj"} },
		"value type":          func(n []string, o Owner, s []Object) { s[0]["data"] = Object{"key": 1} },
		"invalid base64":      func(n []string, o Owner, s []Object) { s[0]["data"] = Object{"key": "%%%"} },
		"base64 newline":      func(n []string, o Owner, s []Object) { s[0]["data"] = Object{"key": "YWJj\n"} },
		"base64 padding bits": func(n []string, o Owner, s []Object) { s[0]["data"] = Object{"key": "YR=="} },
		"entry bound":         func(n []string, o Owner, s []Object) { s[0]["data"] = Object{"key": strings.Repeat("a", (128<<10)+1)} },
		"total bound": func(n []string, o Owner, s []Object) {
			s[0]["data"] = Object{"a": strings.Repeat("a", 128<<10), "b": strings.Repeat("a", 128<<10), "c": strings.Repeat("a", 128<<10), "d": strings.Repeat("a", 128<<10)}
		},
		"duplicate names": func(n []string, o Owner, s []Object) { n[1] = n[0] },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			n, o, s := secretSetFixture()
			change(n, o, s)
			digest, err := OpaqueSecretSetDigest("service", n, o, s)
			if !errors.Is(err, ErrResourceObservation) || digest != "" {
				t.Fatal("invalid Secret set accepted", err)
			}
		})
	}
	n, o, s := secretSetFixture()
	for _, namespace := range []string{"", "../foreign"} {
		if _, err := OpaqueSecretSetDigest(namespace, n, o, s); err == nil {
			t.Fatal("invalid namespace accepted")
		}
	}
	if _, err := OpaqueSecretSetDigest("service", nil, o, nil); err == nil {
		t.Fatal("empty set accepted")
	}
	if _, err := OpaqueSecretSetDigest("service", n, o, s[:1]); err == nil {
		t.Fatal("missing Secret accepted")
	}
	if _, err := OpaqueSecretSetDigest("service", n, nil, s); err == nil {
		t.Fatal("missing owner accepted")
	}
}

func TestOpaqueSecretSetEnsureChecksExactStoredData(t *testing.T) {
	for _, mode := range []string{"valid", "extra data", "changed data", "replacement", "missing", "foreign owner", "no identity", "deleted", "conflict", "invalid second", "stringData", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			names, owner, desired := secretSetFixture()
			for _, s := range desired {
				m := s["metadata"].(Object)
				delete(m, "uid")
				delete(m, "resourceVersion")
			}
			original, _ := json.Marshal(desired)
			var mu sync.Mutex
			stored := map[string]Object{}
			reads := map[string]int{}
			calls, writes := 0, 0
			client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				calls++
				collection := "/api/v1/namespaces/service/secrets"
				name := strings.TrimPrefix(r.URL.Path, collection+"/")
				if r.Method == http.MethodGet {
					reads[name]++
					current := stored[name]
					if current == nil {
						w.WriteHeader(404)
						return
					}
					if name == names[0] && reads[name] == 2 {
						switch mode {
						case "changed data":
							current["data"].(map[string]any)["value"] = "ZGVm"
						case "replacement":
							current["metadata"].(map[string]any)["uid"] = "replacement"
						case "missing":
							w.WriteHeader(404)
							return
						}
					}
					_ = json.NewEncoder(w).Encode(current)
					return
				}
				if r.URL.Path != collection && r.URL.Path != collection+"/"+names[0] {
					t.Error("write escaped the selected set")
					w.WriteHeader(500)
					return
				}
				writes++
				var body Object
				if json.NewDecoder(r.Body).Decode(&body) != nil {
					t.Error("invalid body")
					w.WriteHeader(500)
					return
				}
				name = String(body, "metadata", "name")
				if mode == "conflict" {
					w.WriteHeader(409)
					return
				}
				metadata := body["metadata"].(map[string]any)
				if r.Method == http.MethodPatch {
					if String(body, "metadata", "uid") != "uid-"+name || String(body, "metadata", "resourceVersion") != "1" {
						t.Error("patch lost its observation")
					}
					metadata["resourceVersion"] = "2"
				} else if r.Method == http.MethodPost {
					metadata["uid"] = "uid-" + name
					metadata["resourceVersion"] = "1"
				} else {
					t.Error("unexpected write")
					w.WriteHeader(500)
					return
				}
				switch mode {
				case "extra data":
					body["data"].(map[string]any)["INJECTED"] = "YWJj"
				case "foreign owner":
					metadata["labels"] = map[string]any{"example.test/owner": "foreign"}
				case "no identity":
					delete(metadata, "uid")
				case "deleted":
					metadata["deletionTimestamp"] = "now"
				}
				stored[name] = body
				_ = json.NewEncoder(w).Encode(body)
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "invalid second":
				desired[1]["data"] = Object{"value": "private-invalid-base64"}
			case "stringData":
				desired[1]["stringData"] = Object{"value": "private-cleartext"}
			case "canceled":
				cancel()
			}
			got, err := client.EnsureOpaqueSecretSet(ctx, "service", names, owner, desired)
			if mode != "valid" {
				if err == nil || got != "" {
					t.Fatal("invalid Secret set accepted")
				}
				if strings.Contains(err.Error(), "private-") {
					t.Fatal("Secret data entered the error")
				}
				mu.Lock()
				defer mu.Unlock()
				if (mode == "invalid second" || mode == "stringData" || mode == "canceled") && calls != 0 {
					t.Fatal("invalid input reached the API")
				}
				return
			}
			if err != nil || len(got) != 64 {
				t.Fatal("valid set rejected", err)
			}
			mu.Lock()
			initialWrites := writes
			mu.Unlock()
			if initialWrites != 2 {
				t.Fatal("initial Secrets were not created")
			}
			repeated, err := client.EnsureOpaqueSecretSet(ctx, "service", names, owner, desired)
			if err != nil || repeated != got {
				t.Fatal("retained set changed", err)
			}
			mu.Lock()
			sameWrites := writes
			mu.Unlock()
			if sameWrites != initialWrites {
				t.Fatal("unchanged Secrets were written again")
			}
			after, _ := json.Marshal(desired)
			if !reflect.DeepEqual(original, after) {
				t.Fatal("caller input changed")
			}
			desired[0]["data"].(Object)["value"] = "ZGVm"
			rotated, err := client.EnsureOpaqueSecretSet(ctx, "service", names, owner, desired)
			if err != nil || rotated == got {
				t.Fatal("rotation did not change the digest", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if writes != initialWrites+1 {
				t.Fatal("rotation wrote an unrelated Secret")
			}
		})
	}
}
