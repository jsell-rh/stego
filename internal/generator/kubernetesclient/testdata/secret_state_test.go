package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestSecretStateRecoversAndRefusesLostIdentity(t *testing.T) {
	for _, mode := range []string{"recover", "read-only", "missing-secret", "missing-marker", "changed-data", "changed-pin", "foreign-secret", "closed", "namespace-replaced", "invalid-prepared"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			owner := Owner{"example.test/owner": "widget"}
			o := SecretStateOptions{Namespace: "state", Name: "keys", Marker: "identity", Annotation: "example.test/digest", Owner: owner}
			objects := map[string]Object{}
			ns := Object{"apiVersion": "v1", "kind": "Namespace", "metadata": Object{"name": "state", "uid": "ns-1", "resourceVersion": "1", "labels": Object{"example.test/owner": "widget"}}}
			objects["/api/v1/namespaces/state"] = ns
			writes, initializations, registrations, namespaceReads := 0, 0, 0, 0
			replace := false
			client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Method == http.MethodGet {
					if r.URL.Path == "/api/v1/namespaces/state" {
						namespaceReads++
						if replace && namespaceReads%2 == 0 {
							ns["metadata"].(Object)["uid"] = "replacement"
						}
					}
					value := objects[r.URL.Path]
					if value == nil {
						w.WriteHeader(404)
						return
					}
					_ = json.NewEncoder(w).Encode(value)
					return
				}
				if r.Method != http.MethodPost {
					t.Error("state used a non-create write")
					w.WriteHeader(500)
					return
				}
				writes++
				var body Object
				if json.NewDecoder(r.Body).Decode(&body) != nil {
					t.Error("invalid write")
					w.WriteHeader(500)
					return
				}
				name := String(body, "metadata", "name")
				path := r.URL.Path + "/" + name
				if objects[path] != nil {
					w.WriteHeader(409)
					return
				}
				meta := body["metadata"].(map[string]any)
				meta["uid"] = name + "-1"
				meta["resourceVersion"] = "1"
				objects[path] = body
				w.WriteHeader(201)
				_ = json.NewEncoder(w).Encode(body)
			})
			binding := SecretStateBinding{}
			cb := SecretStateCallbacks{
				Load: func(context.Context) (SecretStateBinding, error) { return binding, nil },
				Bind: func(_ context.Context, digest string) (SecretStateBinding, error) {
					registrations++
					binding = SecretStateBinding{Present: true, Digest: digest}
					return binding, nil
				},
				Initialize: func(context.Context) (map[string]string, error) {
					initializations++
					if mode == "invalid-prepared" {
						return map[string]string{"key": "%%%"}, nil
					}
					return map[string]string{"key": "YWJj"}, nil
				},
				Validate: func(secret Object) error { return nil },
			}
			result, err := client.LoadSecretState(context.Background(), o, cb, true)
			if mode == "invalid-prepared" {
				if err == nil || result != nil || writes != 0 || registrations != 0 {
					t.Fatal("invalid state was written", err)
				}
				return
			}
			if !errors.Is(err, ErrSecretStatePending) || result != nil || writes != 2 || initializations != 1 || registrations != 0 {
				t.Fatal("initial state did not wait for allocator pin", err, writes, initializations, registrations)
			}
			mu.Lock()
			marker := objects["/api/v1/namespaces/state/configmaps/identity"]
			digest := String(marker, "data", "sha256")
			ns["metadata"].(Object)["annotations"] = Object{o.Annotation: digest}
			mu.Unlock()
			result, err = client.LoadSecretState(context.Background(), o, cb, true)
			if err != nil || result == nil || registrations != 1 || writes != 2 {
				t.Fatal("sealed state was not registered", err)
			}
			mu.Lock()
			switch mode {
			case "missing-secret":
				delete(objects, "/api/v1/namespaces/state/secrets/keys")
			case "missing-marker":
				delete(objects, "/api/v1/namespaces/state/configmaps/identity")
			case "changed-data":
				objects["/api/v1/namespaces/state/secrets/keys"]["data"] = Object{"key": "ZGVm"}
			case "changed-pin":
				ns["metadata"].(Object)["annotations"] = Object{o.Annotation: strings.Repeat("b", 64)}
			case "foreign-secret":
				objects["/api/v1/namespaces/state/secrets/keys"]["metadata"].(map[string]any)["labels"] = Object{}
			case "closed":
				binding.Closed = true
			case "namespace-replaced":
				replace = true
				namespaceReads = 0
			}
			mu.Unlock()
			readonly := mode == "read-only" || mode == "missing-marker"
			result, err = client.LoadSecretState(context.Background(), o, cb, !readonly)
			if mode == "recover" || mode == "read-only" {
				if err != nil || result == nil {
					t.Fatal("state did not recover", err)
				}
			} else if err == nil || result != nil {
				t.Fatal("invalid state was used", mode)
			}
			if initializations != 1 || writes != 2 {
				t.Fatal("recovery replaced state", writes, initializations)
			}
		})
	}
}

func TestSecretStateRequiresValidRegistration(t *testing.T) {
	client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid binding reached Kubernetes")
		w.WriteHeader(500)
	})
	o := SecretStateOptions{Namespace: "state", Name: "keys", Marker: "identity", Annotation: "example.test/digest", Owner: Owner{"example.test/owner": "widget"}}
	for _, binding := range []SecretStateBinding{{Closed: true}, {Digest: strings.Repeat("a", 64)}, {Present: true}, {Present: true, Digest: "invalid"}, {Present: true, Closed: true}} {
		cb := SecretStateCallbacks{Load: func(context.Context) (SecretStateBinding, error) { return binding, nil }, Bind: func(context.Context, string) (SecretStateBinding, error) { return binding, nil }, Initialize: func(context.Context) (map[string]string, error) {
			t.Fatal("invalid binding initialized state")
			return nil, nil
		}, Validate: func(Object) error { return nil }}
		if value, err := client.LoadSecretState(context.Background(), o, cb, true); err == nil || value != nil {
			t.Fatal("invalid or closed binding was accepted")
		}
	}
}
