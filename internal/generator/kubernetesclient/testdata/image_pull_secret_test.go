package kubernetes

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func pullConfig(password string) []byte {
	encoded, _ := json.Marshal(map[string]any{"auths": map[string]any{"registry.example.test:5000": map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte("pull-user:" + password))}}})
	return encoded
}
func pullTarget() ImagePullSecretTarget {
	return ImagePullSecretTarget{Namespace: "service", NamespaceUID: "namespace-1", Name: "image-pull", Owner: Owner{"example.test/component": "browser"}, NamespaceOwner: Owner{"example.test/owner": "widget"}, Registries: []string{"registry.example.test:5000"}}
}
func pullNamespace() Object {
	return Object{"apiVersion": "v1", "kind": "Namespace", "metadata": Object{"name": "service", "uid": "namespace-1", "resourceVersion": "1", "labels": Object{"example.test/owner": "widget"}}}
}
func pullSecret(password string) Object {
	return Object{"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/dockerconfigjson", "metadata": Object{"name": "image-pull", "namespace": "service", "uid": "secret-1", "resourceVersion": "1", "labels": Object{"example.test/component": "browser"}}, "data": Object{".dockerconfigjson": base64.StdEncoding.EncodeToString(pullConfig(password))}}
}

func TestImagePullConfigRejectsAmbiguousAndUnselectedCredentials(t *testing.T) {
	valid := pullConfig("private-first")
	cases := map[string][]byte{
		"empty": nil, "size": bytes.Repeat([]byte("a"), (16<<10)+1), "null": []byte("null"), "trailing": append(bytes.Clone(valid), []byte(" {}")...),
		"helpers":            []byte(`{"auths":{},"credsStore":"private-helper"}`),
		"unknown":            []byte(`{"auths":{"registry.example.test:5000":{"identitytoken":"private-token"}}}`),
		"wrong registry":     []byte(`{"auths":{"foreign.example.test":{"auth":"dTpw"}}}`),
		"wildcard":           []byte(`{"auths":{"*.example.test":{"auth":"dTpw"}}}`),
		"duplicate root":     []byte(`{"auths":{},"auths":{"registry.example.test:5000":{"auth":"dTpw"}}}`),
		"duplicate registry": []byte(`{"auths":{"registry.example.test:5000":{"auth":"dTpw"},"registry.example.test:5000":{"auth":"dTpw"}}}`),
		"duplicate auth":     []byte(`{"auths":{"registry.example.test:5000":{"auth":"dTpw","\u0061uth":"dTpw"}}}`),
		"empty user":         []byte(`{"auths":{"registry.example.test:5000":{"auth":"OnA="}}}`),
		"empty password":     []byte(`{"auths":{"registry.example.test:5000":{"auth":"dTo="}}}`),
		"control":            []byte(`{"auths":{"registry.example.test:5000":{"auth":"dToK"}}}`),
		"padding":            []byte(`{"auths":{"registry.example.test:5000":{"auth":"dTpwOnB="}}}`),
		"newline":            []byte(`{"auths":{"registry.example.test:5000":{"auth":"dTpw\n"}}}`),
		"null auth":          []byte(`{"auths":{"registry.example.test:5000":{"auth":null}}}`),
		"invalid utf8":       append(bytes.Clone(valid), 0xff),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ValidateImagePullConfig(input, pullTarget().Registries); !errors.Is(err, ErrImagePullSecret) {
				t.Fatal("invalid credentials accepted", err)
			}
		})
	}
	for _, registries := range [][]string{nil, {"registry.example.test:5000", "registry.example.test:5000"}, {"https://registry.example.test:5000"}, {"registry.example.test:5000/path"}, {"registry.example.test:05000"}, {"registry.example.test:0"}, {"registry.example.test:65536"}, {"UPPER.example.test"}, make([]string, 9)} {
		if err := ValidateImagePullConfig(valid, registries); err == nil {
			t.Fatal("invalid selection accepted")
		}
	}
	if err := ValidateImagePullConfig(valid, pullTarget().Registries); err != nil {
		t.Fatal("valid input rejected", err)
	}
	multi := []byte(`{"auths":{"one.example.test":{"auth":"dTpw"},"two.example.test":{"auth":"dTpw"}}}`)
	a, err := imagePullConfig(multi, []string{"one.example.test", "two.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := imagePullConfig(multi, []string{"two.example.test", "one.example.test"})
	if err != nil || !bytes.Equal(a, b) {
		t.Fatal("selection order changed content")
	}
}

func TestImagePullSecretCreateRotateAndRecover(t *testing.T) {
	for _, mode := range []string{"valid", "foreign namespace", "namespace missing", "namespace replacement", "namespace deleting", "namespace no uid", "foreign secret", "secret type", "secret immutable", "secret extra data", "secret deleting", "secret no uid", "secret wrong name", "secret wrong namespace", "create conflict", "patch conflict", "patch replacement", "response extra data", "read replacement", "read version", "read missing", "read changed", "invalid input", "invalid target", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			var stored Object
			namespace := pullNamespace()
			calls, writes, nsReads, secretReads := 0, 0, 0, 0
			target := pullTarget()
			if strings.HasPrefix(mode, "secret ") || mode == "foreign secret" || strings.HasPrefix(mode, "patch ") {
				stored = pullSecret("private-old")
				switch mode {
				case "foreign secret":
					stored["metadata"].(Object)["labels"] = Object{"example.test/component": "foreign"}
				case "secret type":
					stored["type"] = "Opaque"
				case "secret immutable":
					stored["immutable"] = true
				case "secret extra data":
					stored["data"].(Object)["extra"] = "YWJj"
				case "secret deleting":
					stored["metadata"].(Object)["deletionTimestamp"] = "now"
				case "secret no uid":
					delete(stored["metadata"].(Object), "uid")
				case "secret wrong name":
					stored["metadata"].(Object)["name"] = "foreign"
				case "secret wrong namespace":
					stored["metadata"].(Object)["namespace"] = "foreign"
				}
			}
			client, token := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				calls++
				if r.URL.Path == "/api/v1/namespaces/service" && r.Method == http.MethodGet {
					nsReads++
					switch mode {
					case "foreign namespace":
						namespace["metadata"].(Object)["labels"] = Object{"example.test/owner": "foreign"}
					case "namespace missing":
						w.WriteHeader(404)
						return
					case "namespace replacement":
						if nsReads == 2 {
							namespace["metadata"].(Object)["uid"] = "namespace-2"
						}
					case "namespace deleting":
						namespace["metadata"].(Object)["deletionTimestamp"] = "now"
					case "namespace no uid":
						delete(namespace["metadata"].(Object), "uid")
					}
					_ = json.NewEncoder(w).Encode(namespace)
					return
				}
				path := "/api/v1/namespaces/service/secrets/image-pull"
				if r.Method == http.MethodGet && r.URL.Path == path {
					secretReads++
					if stored == nil || (mode == "read missing" && secretReads == 2) {
						w.WriteHeader(404)
						return
					}
					if secretReads == 2 {
						switch mode {
						case "read replacement":
							stored["metadata"].(map[string]any)["uid"] = "other"
						case "read version":
							stored["metadata"].(map[string]any)["resourceVersion"] = "other"
						case "read changed":
							stored["data"] = Object{".dockerconfigjson": base64.StdEncoding.EncodeToString(pullConfig("private-other"))}
						}
					}
					_ = json.NewEncoder(w).Encode(stored)
					return
				}
				if (r.Method != http.MethodPost || r.URL.Path != "/api/v1/namespaces/service/secrets") && (r.Method != http.MethodPatch || r.URL.Path != path) {
					t.Error("unexpected API request")
					w.WriteHeader(500)
					return
				}
				writes++
				if r.URL.Query().Get("fieldValidation") != "Strict" {
					t.Error("write has no strict validation")
				}
				var body Object
				if json.NewDecoder(r.Body).Decode(&body) != nil {
					t.Error("invalid body")
					w.WriteHeader(500)
					return
				}
				if mode == "create conflict" || mode == "patch conflict" {
					w.WriteHeader(409)
					_, _ = w.Write([]byte("private-server-data"))
					return
				}
				if r.Method == http.MethodPost {
					if stored != nil {
						t.Error("create replaced a Secret")
					}
					stored = body
					stored["metadata"].(map[string]any)["uid"] = "secret-1"
					stored["metadata"].(map[string]any)["resourceVersion"] = "1"
				} else {
					if String(body, "metadata", "uid") != String(stored, "metadata", "uid") || String(body, "metadata", "resourceVersion") != String(stored, "metadata", "resourceVersion") {
						t.Error("patch lost identity")
					}
					if len(body) != 2 || len(body["metadata"].(map[string]any)) != 2 {
						t.Error("rotation changed more than data and preconditions")
					}
					stored, _ = snapshot(stored)
					stored["data"] = body["data"]
					stored["metadata"].(map[string]any)["resourceVersion"] = fmt.Sprint(writes)
					if mode == "patch replacement" {
						stored["metadata"].(map[string]any)["uid"] = "replacement"
					}
				}
				if mode == "response extra data" {
					stored["data"].(map[string]any)["injected"] = "YWJj"
				}
				if r.Method == http.MethodPost {
					w.WriteHeader(201)
				}
				_ = json.NewEncoder(w).Encode(stored)
			})
			input := pullConfig("private-first")
			original := bytes.Clone(input)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "invalid input":
				input = []byte("private-invalid")
			case "invalid target":
				target.NamespaceUID = ""
			case "canceled":
				cancel()
			}
			err := client.EnsureImagePullSecret(ctx, target, input)
			if mode != "valid" {
				if err == nil {
					t.Fatal("invalid state accepted")
				}
				if strings.Contains(err.Error(), "private-") {
					t.Fatal("credentials in error")
				}
				mu.Lock()
				defer mu.Unlock()
				if (strings.HasPrefix(mode, "secret ") || mode == "foreign secret" || mode == "foreign namespace" || mode == "namespace missing" || mode == "namespace deleting" || mode == "namespace no uid") && writes != 0 {
					t.Fatal("invalid identity permitted a write")
				}
				if (mode == "invalid input" || mode == "invalid target" || mode == "canceled") && calls != 0 {
					t.Fatal("invalid input reached API")
				}
				if (mode == "patch conflict" || mode == "create conflict") && writes != 1 {
					t.Fatal("conflict retried")
				}
				return
			}
			if err != nil {
				t.Fatal("create failed", err)
			}
			if !bytes.Equal(input, original) {
				t.Fatal("input changed")
			}
			file := filepath.Join(filepath.Dir(token), "pull-config")
			if err = os.WriteFile(file, input, 0600); err != nil {
				t.Fatal(err)
			}
			if err = client.EnsureImagePullSecretFile(ctx, target, file); err != nil {
				t.Fatal("recovery failed", err)
			}
			mu.Lock()
			unchanged := writes
			mu.Unlock()
			if unchanged != 1 {
				t.Fatal("same credentials caused a write")
			}
			if err = os.WriteFile(file, pullConfig("private-rotated"), 0600); err != nil {
				t.Fatal(err)
			}
			if err = client.EnsureImagePullSecretFile(ctx, target, file); err != nil {
				t.Fatal("rotation failed", err)
			}
			mu.Lock()
			rotated := writes
			mu.Unlock()
			if rotated != 2 {
				t.Fatal("rotation did not write exactly once")
			}
			if err = os.Chmod(file, 0644); err != nil {
				t.Fatal(err)
			}
			if err = client.EnsureImagePullSecretFile(ctx, target, file); !errors.Is(err, ErrImagePullSecret) {
				t.Fatal("public file accepted")
			}
			mu.Lock()
			defer mu.Unlock()
			if writes != 2 {
				t.Fatal("public file reached write")
			}
		})
	}
}
